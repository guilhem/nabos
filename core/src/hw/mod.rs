//! Hardware access. Each device is served by its own thread and reports
//! typed asynchronous events to the hardware service.

pub mod button;
pub mod cr14;
pub mod ears;
pub mod leds;
pub mod nfc;

use crate::Config;
use std::os::fd::AsRawFd;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::{mpsc::UnboundedSender, oneshot};

pub type Tx = UnboundedSender<HwEvent>;

#[derive(Debug)]
pub enum HwEvent {
    Button(&'static str, Option<u64>),
    /// Manual ear movement: 0 left, 1 right.
    EarMoved(usize),
    Tag(TagEvent),
}

#[derive(Debug, Clone, Default)]
pub struct TagEvent {
    pub removed: bool,
    pub tech: &'static str,
    pub uid: Vec<u8>,
    pub support: &'static str,
    pub locked: bool,
    pub picture: Option<u8>,
    pub app: Option<u8>,
    pub data: Option<Vec<u8>>,
}

pub fn send(tx: &Tx, ev: HwEvent) {
    let _ = tx.send(ev);
}

/// Admission is fenced against cancellation under one lock. An admitted driver
/// command is indivisible; cancellation only removes commands still waiting.
#[derive(Clone, Default)]
pub struct Cancel(Arc<std::sync::Mutex<bool>>);

impl Cancel {
    pub fn cancel(&self) {
        *self.0.lock().unwrap() = true;
    }
    pub fn same(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }
    pub fn is_cancelled(&self) -> bool {
        *self.0.lock().unwrap()
    }
    pub fn admit(&self) -> bool {
        !*self.0.lock().unwrap()
    }
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Tech {
    St25tb,
    T2t,
}

#[derive(Default)]
struct WritePhase {
    admitted: bool,
    canceled: bool,
    finished: bool,
}

#[derive(Clone, Default)]
pub struct WriteControl(Arc<std::sync::Mutex<WritePhase>>);

impl WriteControl {
    pub fn cancel(&self) {
        self.0.lock().unwrap().canceled = true;
    }
    pub fn uncertain(&self) -> bool {
        let phase = self.0.lock().unwrap();
        phase.admitted && !phase.finished
    }
    pub fn finish(&self) {
        self.0.lock().unwrap().finished = true;
    }
}

/// Nabaztag tag payload (block 7 onwards): "Nb" + picture + app, little endian,
/// then application data terminated by 0xFF and padded to 4 bytes.
pub fn encode_tag_data(picture: u8, app: u8, data: Option<&[u8]>) -> Vec<u8> {
    let mut out = vec![app, picture, b'b', b'N'];
    match data {
        Some(d) if !d.is_empty() => {
            out.extend_from_slice(d);
            if d.len() < 32 {
                out.push(0xFF);
            }
            while out.len() % 4 != 0 {
                out.push(0xFF);
            }
        }
        _ => out.extend_from_slice(&[0xFF; 4]),
    }
    out
}

/// Decode ST25TB blocks 7..=15 then system block 255 (40 bytes).
pub fn decode_st25tb(data: &[u8], ev: &mut TagEvent) {
    let system = u32::from_le_bytes([data[36], data[37], data[38], data[39]]);
    ev.locked = system & 0xFF80_0000 != 0xFF80_0000;
    if data[3] == b'N' && data[2] == b'b' {
        ev.picture = Some(data[1]);
        ev.app = Some(data[0]);
        ev.data = Some(data[4..36].to_vec());
        ev.support = "formatted";
    } else if data[..36].iter().any(|b| *b != 0xFF) {
        ev.support = "foreign-data";
    } else if ev.locked {
        ev.support = "locked";
    } else {
        ev.support = "empty";
    }
}

/// Supported ST25TB models (UID in protocol/little-endian order).
pub fn st25tb_compatible(uid_le: &[u8]) -> bool {
    let n = uid_le.len();
    n == 8
        && uid_le[n - 1] == 0xD0
        && uid_le[n - 2] == 0x02
        && [0x18, 0x30, 0x1C, 0x0C, 0x3C].contains(&(uid_le[n - 3] & 0xFC))
}

/// Wait until fd is readable, false on timeout.
pub fn poll_readable(fd: &impl AsRawFd, timeout: Duration) -> bool {
    let mut p = libc::pollfd {
        fd: fd.as_raw_fd(),
        events: libc::POLLIN,
        revents: 0,
    };
    let ms = timeout.as_millis().min(i32::MAX as u128) as i32;
    unsafe { libc::poll(&mut p, 1, ms) > 0 }
}

pub struct WriteReq {
    pub tech: Tech,
    pub uid: Vec<u8>,
    pub payload: Vec<u8>,
    pub deadline: Instant,
    pub cancel: Cancel,
    pub control: WriteControl,
    pub reply: oneshot::Sender<Result<(), String>>,
}

pub struct Rfid {
    pub kind: &'static str,
    tx: std::sync::mpsc::Sender<WriteReq>,
}

impl WriteReq {
    pub fn stopped(&self) -> Option<&'static str> {
        if self.cancel.is_cancelled()
            || self.control.0.lock().unwrap().canceled
            || self.reply.is_closed()
        {
            Some("canceled")
        } else if Instant::now() >= self.deadline {
            Some("timeout")
        } else {
            None
        }
    }
    pub fn admit(&self) -> Result<(), String> {
        let canceled = self.cancel.0.lock().unwrap();
        let mut phase = self.control.0.lock().unwrap();
        if *canceled || phase.canceled || self.reply.is_closed() {
            return Err("canceled".into());
        }
        if Instant::now() >= self.deadline {
            return Err("timeout".into());
        }
        phase.admitted = true;
        Ok(())
    }
}

impl Rfid {
    pub fn start(&self, req: WriteReq) -> Result<(), String> {
        self.tx.send(req).map_err(|_| "reader stopped".to_string())
    }
}

pub type Status = (String, bool, String, String, bool, bool, String, i16, i16);

pub struct HwInfo {
    pub model: &'static str,
    pub simulated: bool,
}

pub struct Hw {
    pub leds: leds::Leds,
    pub ears: ears::Ears,
    pub rfid: Option<Rfid>,
    pub button: bool,
    pub info: HwInfo,
}

impl Hw {
    pub fn open(cfg: &Config, tx: Tx, presence: Option<crate::network::Presence>) -> Hw {
        let sim = cfg.simulate;
        // Physical setup confirmation remains available while LED initialization
        // waits for its hardware thread.
        let button = sim || button::spawn(&cfg.gpio_chip, cfg.button_gpio, tx.clone(), presence);
        let leds = leds::Leds::open(cfg);
        let ears = ears::Ears::open(sim, tx.clone());
        let spawn_reader =
            |kind: &'static str,
             run: fn(std::sync::mpsc::Receiver<WriteReq>, Tx) -> std::io::Result<()>| {
                let (wtx, wrx) = std::sync::mpsc::channel();
                let t = tx.clone();
                std::thread::Builder::new()
                    .name(kind.into())
                    .spawn(move || {
                        if let Err(e) = run(wrx, t) {
                            error!("{kind} reader stopped: {e}");
                        }
                    })
                    .ok()?;
                Some(Rfid { kind, tx: wtx })
            };
        let (rfid, model) = if sim {
            (None, "simulated")
        } else if std::path::Path::new(nfc::DEVICE).exists() {
            (spawn_reader("st25r391x", nfc::run), "2022_NFC")
        } else if std::path::Path::new(cr14::DEVICE).exists() {
            (spawn_reader("cr14", cr14::run), "2019_TAGTAG")
        } else {
            (None, "2019_TAG")
        };
        Hw {
            leds,
            ears,
            rfid,
            button,
            info: HwInfo {
                model,
                simulated: sim,
            },
        }
    }

    pub fn status(&self) -> Status {
        let (left, right) = self.ears.snapshot();
        (
            self.info.model.into(),
            self.info.simulated,
            self.ears.status(0).into(),
            self.ears.status(1).into(),
            self.leds.available(),
            self.button,
            self.rfid.as_ref().map_or("none", |r| r.kind).into(),
            left,
            right,
        )
    }
    pub fn ready(&self) -> bool {
        self.button && self.leds.available() && !self.ears.broken(0) && !self.ears.broken(1)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tag_data_roundtrip() {
        let enc = encode_tag_data(42, 5, Some(&[1]));
        assert_eq!(enc, vec![5, 42, b'b', b'N', 1, 0xFF, 0xFF, 0xFF]);
        let mut blocks = vec![0xFF; 40];
        blocks[..8].copy_from_slice(&enc);
        let mut ev = TagEvent::default();
        decode_st25tb(&blocks, &mut ev);
        assert_eq!(
            (ev.support, ev.picture, ev.app, ev.locked),
            ("formatted", Some(42), Some(5), false)
        );
        let mut empty = TagEvent::default();
        decode_st25tb(&[0xFF; 40], &mut empty);
        assert_eq!(empty.support, "empty");
        assert!(st25tb_compatible(&[1, 2, 3, 4, 5, 0x18, 0x02, 0xD0]));
        assert!(!st25tb_compatible(&[1, 2, 3, 4, 5, 0x40, 0x02, 0xD0]));
    }
}
