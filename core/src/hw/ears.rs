//! Ears through the tagtagtag-ears driver (/dev/ear0 left, /dev/ear1 right).
//! Byte protocol: '.' wait idle, '+'/'-' n steps, '>'/'<' position (17 steps
//! per turn), '?' position, '!' position with detection; reads return a
//! position (0xFF unknown) or 'm' when the user moved the ear.
//! In simulate mode the same byte protocol is applied to an in-memory ear.

use super::{send, HwEvent, Tx};
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::sync::{mpsc, Arc, Condvar, Mutex};
use std::time::Duration;
use tokio::sync::oneshot;

pub const STEPS: u8 = 17;
const QUERY_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Default)]
struct Shared {
    pos: Option<u8>,
    seq: u64,
    broken: bool,
}

type SharedRef = Arc<(Mutex<Shared>, Condvar)>;

enum Req {
    Write(Vec<u8>, oneshot::Sender<()>),
    Query(bool, oneshot::Sender<Option<u8>>),
}

enum Backend {
    Dev(File),
    Sim(Option<u8>),
}

fn report(shared: &SharedRef, pos: Option<u8>) {
    let (m, cv) = &**shared;
    let mut s = m.lock().unwrap();
    s.pos = pos;
    s.seq += 1;
    cv.notify_all();
}

fn mark_broken(shared: &SharedRef) {
    let (m, cv) = &**shared;
    let mut s = m.lock().unwrap();
    s.broken = true;
    s.pos = None;
    cv.notify_all();
}

/// Apply driver commands to a simulated ear.
fn sim_apply(pos: &mut Option<u8>, bytes: &[u8], shared: &SharedRef) {
    let mut i = 0;
    while i < bytes.len() {
        let arg = bytes.get(i + 1).copied().unwrap_or(0);
        match bytes[i] {
            b'+' => *pos = pos.map(|p| ((p as u32 + arg as u32) % STEPS as u32) as u8),
            b'-' => *pos = pos.map(|p| ((p as i32 - arg as i32).rem_euclid(STEPS as i32)) as u8),
            b'>' | b'<' => *pos = Some(arg % STEPS),
            b'?' => report(shared, *pos),
            b'!' => report(shared, Some(*pos.get_or_insert(0))),
            _ => {}
        }
        i += if matches!(bytes[i], b'+' | b'-' | b'>' | b'<') {
            2
        } else {
            1
        };
    }
}

pub struct Ear {
    tx: Option<mpsc::Sender<Req>>,
    shared: SharedRef,
    missing: bool,
}

impl Ear {
    fn open(index: usize, sim: bool, events: Tx) -> Ear {
        let shared: SharedRef = Arc::new((Mutex::new(Shared::default()), Condvar::new()));
        let backend = if sim {
            Backend::Sim(Some(0))
        } else {
            let path = format!("/dev/ear{index}");
            match OpenOptions::new().read(true).write(true).open(&path) {
                Ok(mut f) => {
                    if f.write_all(b"?").is_err() {
                        error!("ear {index} is apparently broken");
                        mark_broken(&shared);
                        return Ear {
                            tx: None,
                            shared,
                            missing: false,
                        };
                    }
                    let mut reader = f.try_clone().expect("dup ear fd");
                    let sh = shared.clone();
                    std::thread::spawn(move || {
                        let mut b = [0u8; 1];
                        loop {
                            match reader.read(&mut b) {
                                Ok(1) if b[0] == b'm' => send(&events, HwEvent::EarMoved),
                                Ok(1) => report(&sh, (b[0] != 0xFF).then_some(b[0])),
                                Ok(_) | Err(_) => {
                                    error!("ear {index} has been declared broken");
                                    mark_broken(&sh);
                                    return;
                                }
                            }
                        }
                    });
                    Backend::Dev(f)
                }
                Err(e) => {
                    warn!("{path}: {e}");
                    return Ear {
                        tx: None,
                        shared,
                        missing: true,
                    };
                }
            }
        };
        let (tx, rx) = mpsc::channel::<Req>();
        let sh = shared.clone();
        std::thread::spawn(move || worker(backend, rx, sh));
        Ear {
            tx: Some(tx),
            shared,
            missing: false,
        }
    }

    fn usable(&self) -> Option<&mpsc::Sender<Req>> {
        if self.shared.0.lock().unwrap().broken {
            return None;
        }
        self.tx.as_ref()
    }

    async fn write(&self, bytes: Vec<u8>) {
        if let Some(tx) = self.usable() {
            let (r, rx) = oneshot::channel();
            if tx.send(Req::Write(bytes, r)).is_ok() {
                let _ = rx.await;
            }
        }
    }

    async fn query(&self, detect: bool) -> Option<u8> {
        let tx = self.usable()?;
        let (r, rx) = oneshot::channel();
        tx.send(Req::Query(detect, r)).ok()?;
        rx.await.ok().flatten()
    }
}

fn worker(mut backend: Backend, rx: mpsc::Receiver<Req>, shared: SharedRef) {
    let write = |backend: &mut Backend, bytes: &[u8]| -> bool {
        match backend {
            Backend::Dev(f) => {
                // Blocks while the ear is running (driver semantics).
                if f.write_all(bytes).is_err() {
                    mark_broken(&shared);
                    return false;
                }
                true
            }
            Backend::Sim(pos) => {
                sim_apply(pos, bytes, &shared);
                true
            }
        }
    };
    for req in rx {
        match req {
            Req::Write(bytes, reply) => {
                write(&mut backend, &bytes);
                let _ = reply.send(());
            }
            Req::Query(detect, reply) => {
                let start = shared.0.lock().unwrap().seq;
                let cmd: &[u8] = if detect { b"!." } else { b"?." };
                let mut result = None;
                if write(&mut backend, cmd) {
                    let (m, cv) = &*shared;
                    let guard = m.lock().unwrap();
                    let (s, _) = cv
                        .wait_timeout_while(guard, QUERY_TIMEOUT, |s| s.seq == start && !s.broken)
                        .unwrap();
                    result = s.pos;
                }
                let _ = reply.send(result);
            }
        }
    }
}

pub struct Ears {
    ears: [Ear; 2],
}

impl Ears {
    pub fn open(sim: bool, events: Tx) -> Ears {
        Ears {
            ears: [Ear::open(0, sim, events.clone()), Ear::open(1, sim, events)],
        }
    }

    /// Go to position (additional turns when >= 17), backward if requested.
    pub async fn go(&self, ear: usize, pos: u8, backward: bool) {
        if let Some(e) = self.ears.get(ear) {
            e.write(vec![if backward { b'<' } else { b'>' }, pos]).await;
        }
    }

    pub async fn step(&self, ear: usize, delta: u8, backward: bool) {
        if let Some(e) = self.ears.get(ear) {
            e.write(vec![if backward { b'-' } else { b'+' }, delta])
                .await;
        }
    }

    pub async fn wait_idle(&self) {
        for e in &self.ears {
            e.write(vec![b'.']).await;
        }
    }

    pub async fn positions(&self) -> (Option<u8>, Option<u8>) {
        (
            self.ears[0].query(false).await,
            self.ears[1].query(false).await,
        )
    }

    pub async fn detect(&self) -> (Option<u8>, Option<u8>) {
        (
            self.ears[0].query(true).await,
            self.ears[1].query(true).await,
        )
    }

    pub async fn move_to(&self, left: u8, right: u8) {
        self.go(0, left, false).await;
        self.go(1, right, false).await;
        self.wait_idle().await;
    }

    pub fn broken(&self, ear: usize) -> bool {
        let e = &self.ears[ear];
        e.missing || e.shared.0.lock().unwrap().broken
    }

    pub fn status(&self, ear: usize) -> &'static str {
        let e = &self.ears[ear];
        if e.missing {
            "missing"
        } else if e.shared.0.lock().unwrap().broken {
            "broken"
        } else {
            "ok"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn simulated_driver_protocol() {
        let shared: SharedRef = Arc::new((Mutex::new(Shared::default()), Condvar::new()));
        let mut pos = Some(0);
        sim_apply(&mut pos, b"+\x05-\x07?", &shared);
        assert_eq!(shared.0.lock().unwrap().pos, Some(15));
        sim_apply(&mut pos, b">\x14.", &shared);
        assert_eq!(pos, Some(3));
        let mut unknown = None;
        sim_apply(&mut unknown, b"?", &shared);
        assert_eq!(shared.0.lock().unwrap().pos, None);
        sim_apply(&mut unknown, b"!", &shared);
        assert_eq!(shared.0.lock().unwrap().pos, Some(0));
    }
}
