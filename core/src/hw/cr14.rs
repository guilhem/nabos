//! CR14 reader (/dev/rfid0, pguyot/cr14 driver) for ST25TB/SRI tags.
//! Messages: 'p' poll once, 'P' poll repeat, 'i' idle, 'u'+uid(8, LE),
//! 'R'/'W' + uid + count + addresses (+ data), answered by 'R'/'W' + count + data.

use super::Tech;
use super::{
    decode_st25tb, poll_readable, send, st25tb_compatible, HwEvent, TagEvent, Tx, WriteReq,
};
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::sync::mpsc::Receiver;
use std::time::{Duration, Instant};

pub const DEVICE: &str = "/dev/rfid0";
const REMOVED_TIMEOUT: Duration = Duration::from_secs(1);

#[derive(PartialEq, Clone, Copy)]
enum State {
    PollingOnce,
    PollingRepeat,
    Reading,
    Writing,
}

struct Reader {
    f: File,
    tx: Tx,
    state: State,
    current: Option<Vec<u8>>, // uid, little endian as sent by the driver
    deadline: Option<Instant>,
    pending: Option<(WriteReq, Vec<u8>)>,
}

fn event(uid_le: &[u8]) -> TagEvent {
    let mut uid = uid_le.to_vec();
    uid.reverse();
    TagEvent {
        tech: "st25tb",
        uid,
        support: "unknown",
        ..Default::default()
    }
}

impl Reader {
    fn cmd(&mut self, bytes: &[u8]) -> std::io::Result<()> {
        self.f.write_all(bytes)
    }

    fn read_n(&mut self, n: usize) -> std::io::Result<Vec<u8>> {
        let mut b = vec![0; n];
        self.f.read_exact(&mut b)?;
        Ok(b)
    }

    fn poll_once(&mut self) -> std::io::Result<()> {
        self.state = State::PollingOnce;
        self.cmd(b"p")
    }

    fn repeat(&mut self) -> std::io::Result<()> {
        self.state = State::PollingRepeat;
        self.deadline = Some(Instant::now() + REMOVED_TIMEOUT);
        self.cmd(b"P")
    }

    fn removed(&mut self) {
        if let Some(uid) = self.current.take() {
            send(
                &self.tx,
                HwEvent::Tag(TagEvent {
                    removed: true,
                    ..event(&uid)
                }),
            );
        }
    }

    fn start_write(&mut self, req: WriteReq) -> std::io::Result<()> {
        if req.tech != Tech::St25tb || req.uid.len() != 8 {
            let _ = req
                .reply
                .send(Err("unsupported tag technology for this reader".into()));
            return Ok(());
        }
        if let Err(e) = req.admit() {
            let _ = req.reply.send(Err(e));
            return Ok(());
        }
        let mut uid_le = req.uid.clone();
        uid_le.reverse();
        let count = (req.payload.len() / 4) as u8;
        let mut msg = vec![b'W'];
        msg.extend_from_slice(&uid_le);
        msg.push(count);
        msg.extend(7..7 + count);
        msg.extend_from_slice(&req.payload);
        let expected = req.payload.clone();
        self.state = State::Writing;
        self.deadline = None;
        self.pending = Some((req, expected));
        self.cmd(&msg)
    }

    fn packet(&mut self) -> std::io::Result<()> {
        let mut h = [0u8; 1];
        self.f.read_exact(&mut h)?;
        match h[0] {
            b'u' => {
                let uid_le = self.read_n(8)?;
                if !matches!(self.state, State::PollingOnce | State::PollingRepeat) {
                    return Ok(());
                }
                if self.current.as_deref() == Some(&uid_le[..]) {
                    self.deadline = Some(Instant::now() + REMOVED_TIMEOUT);
                    if self.state != State::PollingRepeat {
                        self.repeat()?;
                    }
                    return Ok(());
                }
                self.removed();
                self.current = Some(uid_le.clone());
                if st25tb_compatible(&uid_le) {
                    let mut msg = vec![b'R'];
                    msg.extend_from_slice(&uid_le);
                    msg.extend_from_slice(&[10, 7, 8, 9, 10, 11, 12, 13, 14, 15, 255]);
                    self.state = State::Reading;
                    self.deadline = Some(Instant::now() + REMOVED_TIMEOUT);
                    self.cmd(&msg)?;
                } else {
                    send(&self.tx, HwEvent::Tag(event(&uid_le)));
                    self.repeat()?;
                }
            }
            b'R' => {
                let n = self.read_n(1)?[0] as usize;
                let data = self.read_n(n * 4)?;
                if self.state != State::Reading || n != 10 {
                    return Ok(());
                }
                if let Some(uid) = self.current.clone() {
                    let mut ev = event(&uid);
                    decode_st25tb(&data, &mut ev);
                    send(&self.tx, HwEvent::Tag(ev));
                }
                self.repeat()?;
            }
            b'W' => {
                let n = self.read_n(1)?[0] as usize;
                let data = self.read_n(n * 4)?;
                if self.state != State::Writing {
                    return Ok(());
                }
                if let Some((req, expected)) = self.pending.take() {
                    let r = if data == expected {
                        Ok(())
                    } else {
                        Err("written data mismatch".into())
                    };
                    req.control.finish();
                    let _ = req.reply.send(r);
                }
                // Tag is still there: keep it as current, avoid a new event.
                self.repeat()?;
            }
            other => warn!("unexpected CR14 packet header {other:#x}"),
        }
        Ok(())
    }

    fn tick(&mut self) -> std::io::Result<()> {
        let now = Instant::now();
        // Once W is transmitted, only its answer proves completion. A deadline
        // cannot return the reader to polling or authorize another write.
        if self.deadline.is_some_and(|d| now >= d) {
            self.deadline = None;
            self.removed();
            self.poll_once()?;
        }
        Ok(())
    }
}

pub fn run(requests: Receiver<WriteReq>, tx: Tx) -> std::io::Result<()> {
    let f = OpenOptions::new().read(true).write(true).open(DEVICE)?;
    let mut r = Reader {
        f,
        tx,
        state: State::PollingOnce,
        current: None,
        deadline: None,
        pending: None,
    };
    r.poll_once()?;
    info!("CR14 reader ready");
    loop {
        if r.pending.is_none() {
            if let Ok(req) = requests.try_recv() {
                r.start_write(req)?;
            }
        }
        if poll_readable(&r.f, Duration::from_millis(100)) {
            r.packet()?;
        }
        r.tick()?;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hw::{Cancel, WriteControl};
    use std::os::fd::OwnedFd;
    use std::os::unix::net::UnixStream;

    fn request() -> (WriteReq, tokio::sync::oneshot::Receiver<Result<(), String>>) {
        let (reply, rx) = tokio::sync::oneshot::channel();
        (
            WriteReq {
                tech: Tech::St25tb,
                uid: vec![0; 8],
                payload: vec![1; 8],
                deadline: Instant::now() + Duration::from_secs(1),
                cancel: Cancel::default(),
                control: WriteControl::default(),
                reply,
            },
            rx,
        )
    }
    #[test]
    fn canceled_expired_or_dropped_requests_never_write_and_admitted_write_finishes() {
        let (driver, mut chip) = UnixStream::pair().unwrap();
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let mut reader = Reader {
            f: File::from(OwnedFd::from(driver)),
            tx,
            state: State::PollingOnce,
            current: None,
            deadline: None,
            pending: None,
        };
        let (req, _rx) = request();
        req.cancel.cancel();
        reader.start_write(req).unwrap();
        let (mut req, _rx) = request();
        req.deadline = Instant::now();
        reader.start_write(req).unwrap();
        let (req, rx) = request();
        drop(rx);
        reader.start_write(req).unwrap();
        assert!(!poll_readable(&chip, Duration::from_millis(5)));
        let (req, mut rx) = request();
        let control = req.control.clone();
        reader.start_write(req).unwrap();
        let mut command = [0u8; 20];
        chip.read_exact(&mut command).unwrap();
        assert_eq!(command[0], b'W');
        control.cancel();
        reader.pending.as_mut().unwrap().0.deadline = Instant::now();
        reader.tick().unwrap();
        assert!(
            reader.pending.is_some(),
            "timeout does not abort an admitted driver W"
        );
        assert!(control.uncertain());
        chip.write_all(&[b'W', 2, 1, 1, 1, 1, 1, 1, 1, 1]).unwrap();
        reader.packet().unwrap();
        assert_eq!(rx.try_recv().unwrap(), Ok(()));
        assert!(!control.uncertain());
    }
}
