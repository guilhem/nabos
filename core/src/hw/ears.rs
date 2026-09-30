//! Ears through the tagtagtag-ears driver (/dev/ear0 left, /dev/ear1 right).
//! Byte protocol: '.' wait idle, '+'/'-' n steps, '>'/'<' position (17 steps
//! per turn), '?' position, '!' position with detection; reads return a
//! position (0xFF unknown) or 'm' when the user moved the ear.
//! In simulate mode the same byte protocol is applied to an in-memory ear.

use super::{send, Cancel, HwEvent, Tx};
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
    Write(Vec<u8>, Cancel, oneshot::Sender<Result<(), String>>),
    Query(bool, Cancel, oneshot::Sender<Option<u8>>),
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
            {
                report(&shared, Some(0));
                Backend::Sim(Some(0))
            }
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
                                Ok(1) if b[0] == b'm' => send(&events, HwEvent::EarMoved(index)),
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

    async fn write(&self, bytes: Vec<u8>, cancel: Cancel) -> Result<(), String> {
        let tx = self.usable().ok_or("ear-unavailable")?;
        let (r, rx) = oneshot::channel();
        tx.send(Req::Write(bytes, cancel, r))
            .map_err(|_| "ear-stopped")?;
        rx.await.map_err(|_| "ear-stopped")?
    }

    async fn query(&self, detect: bool, cancel: Cancel) -> Option<u8> {
        let tx = self.usable()?;
        let (r, rx) = oneshot::channel();
        tx.send(Req::Query(detect, cancel, r)).ok()?;
        rx.await.ok().flatten()
    }
}

fn worker(mut backend: Backend, rx: mpsc::Receiver<Req>, shared: SharedRef) {
    let status = shared.clone();
    serve(rx, shared, move |bytes| match &mut backend {
        Backend::Dev(f) => {
            if f.write_all(bytes).is_err() {
                mark_broken(&status);
                return false;
            }
            true
        }
        Backend::Sim(pos) => {
            sim_apply(pos, bytes, &status);
            true
        }
    });
}

fn serve(rx: mpsc::Receiver<Req>, shared: SharedRef, mut write: impl FnMut(&[u8]) -> bool) {
    for req in rx {
        match req {
            Req::Write(bytes, cancel, reply) => {
                let result = if reply.is_closed() || !cancel.admit() {
                    Err("canceled".into())
                } else if !write(b".") {
                    Err("ear-broken".into())
                } else if reply.is_closed() || !cancel.admit() {
                    // The driver's write blocks before sending a new command.
                    // Wait using '.' first, then fence that transmission again.
                    Err("canceled".into())
                } else if bytes == b"." || write(&bytes) {
                    Ok(())
                } else {
                    Err("ear-broken".into())
                };
                let _ = reply.send(result);
            }
            Req::Query(detect, cancel, reply) => {
                if reply.is_closed() || !cancel.admit() {
                    let _ = reply.send(None);
                    continue;
                }
                if !write(b".") || reply.is_closed() || !cancel.admit() {
                    let _ = reply.send(None);
                    continue;
                }
                let start = shared.0.lock().unwrap().seq;
                let cmd: &[u8] = if detect { b"!." } else { b"?." };
                let mut result = None;
                if write(cmd) {
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

    /// Driver position includes extra turns, without normalization on real ears.
    pub async fn go(
        &self,
        ear: usize,
        pos: u8,
        backward: bool,
        cancel: Cancel,
    ) -> Result<(), String> {
        self.ears[ear]
            .write(vec![if backward { b'<' } else { b'>' }, pos], cancel)
            .await
    }

    pub async fn step(
        &self,
        ear: usize,
        delta: u8,
        backward: bool,
        cancel: Cancel,
    ) -> Result<(), String> {
        self.ears[ear]
            .write(vec![if backward { b'-' } else { b'+' }, delta], cancel)
            .await
    }

    pub async fn wait_one_idle(&self, ear: usize) -> Result<(), String> {
        self.ears[ear].write(vec![b'.'], Cancel::default()).await
    }

    pub async fn wait_idle(&self) -> Result<(), String> {
        for e in &self.ears {
            e.write(vec![b'.'], Cancel::default()).await?;
        }
        Ok(())
    }

    pub async fn positions(&self, detect: bool, cancel: Cancel) -> (Option<u8>, Option<u8>) {
        (
            self.ears[0].query(detect, cancel.clone()).await,
            self.ears[1].query(detect, cancel).await,
        )
    }

    pub fn snapshot(&self) -> (i16, i16) {
        let pos = |e: &Ear| e.shared.0.lock().unwrap().pos.map_or(-1, i16::from);
        (pos(&self.ears[0]), pos(&self.ears[1]))
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

#[cfg(test)]
mod cancellation_tests {
    use super::*;

    #[test]
    fn cancel_or_drop_during_real_idle_wait_prevents_next_motor_command() {
        let shared: SharedRef = Arc::new((Mutex::new(Shared::default()), Condvar::new()));
        let (tx, rx) = mpsc::channel();
        let (blocked, idle_started) = mpsc::channel();
        let (resume, resume_rx) = mpsc::channel();
        let sent = Arc::new(Mutex::new(Vec::new()));
        let commands = sent.clone();
        let worker = std::thread::spawn(move || {
            let mut moving = false;
            serve(rx, shared, |bytes| {
                if bytes == b"." {
                    if moving {
                        blocked.send(()).unwrap();
                        resume_rx.recv().unwrap();
                        moving = false;
                    }
                } else {
                    commands.lock().unwrap().push(bytes.to_vec());
                    moving = true;
                }
                true
            });
        });
        for dropped in [false, true] {
            let (reply, rx) = oneshot::channel();
            tx.send(Req::Write(vec![b'>', 1], Cancel::default(), reply))
                .unwrap();
            rx.blocking_recv().unwrap().unwrap();
            let cancel = Cancel::default();
            let (reply, rx) = oneshot::channel();
            tx.send(Req::Write(vec![b'>', 2], cancel.clone(), reply))
                .unwrap();
            idle_started.recv_timeout(Duration::from_secs(2)).unwrap();
            if dropped {
                drop(rx);
            } else {
                cancel.cancel();
                resume.send(()).unwrap();
                assert_eq!(rx.blocking_recv().unwrap(), Err("canceled".into()));
            }
            if dropped {
                resume.send(()).unwrap();
            }
            let (reply, rx) = oneshot::channel();
            tx.send(Req::Write(vec![b'.'], Cancel::default(), reply))
                .unwrap();
            rx.blocking_recv().unwrap().unwrap();
        }
        let (reply, rx) = oneshot::channel();
        tx.send(Req::Write(vec![b'>', 1], Cancel::default(), reply))
            .unwrap();
        rx.blocking_recv().unwrap().unwrap();
        let cancel = Cancel::default();
        let (reply, rx) = oneshot::channel();
        tx.send(Req::Query(true, cancel.clone(), reply)).unwrap();
        idle_started.recv_timeout(Duration::from_secs(2)).unwrap();
        cancel.cancel();
        resume.send(()).unwrap();
        assert_eq!(rx.blocking_recv().unwrap(), None);
        drop(tx);
        worker.join().unwrap();
        assert_eq!(
            *sent.lock().unwrap(),
            vec![vec![b'>', 1]; 3],
            "no second movement or detection is transmitted after caller loss"
        );
    }
}
