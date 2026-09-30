//! Hardware access. Each device is served by its own thread and reports
//! asynchronous events to the engine through the input channel.

pub mod button;
pub mod cr14;
pub mod ears;
pub mod leds;
pub mod nfc;
pub mod player;

use crate::engine::Input;
use crate::protocol::Tech;
use crate::resources::Resources;
use crate::Config;
use std::os::fd::AsRawFd;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::{mpsc::UnboundedSender, oneshot, watch};

pub type Tx = UnboundedSender<Input>;

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
    let _ = tx.send(Input::Hw(ev));
}

/// Cooperative cancellation shared by a job and the engine.
#[derive(Clone)]
pub struct Cancel(watch::Receiver<bool>);
pub struct CancelSource(watch::Sender<bool>);

impl CancelSource {
    pub fn new() -> (CancelSource, Cancel) {
        let (tx, rx) = watch::channel(false);
        (CancelSource(tx), Cancel(rx))
    }
    pub fn cancel(&self) {
        let _ = self.0.send(true);
    }
}

impl Cancel {
    /// A token that is never cancelled (shared, no allocation per call).
    pub fn never() -> Cancel {
        static NEVER: std::sync::OnceLock<(watch::Sender<bool>, watch::Receiver<bool>)> =
            std::sync::OnceLock::new();
        Cancel(NEVER.get_or_init(|| watch::channel(false)).1.clone())
    }
    pub fn is_cancelled(&self) -> bool {
        *self.0.borrow()
    }
    pub async fn wait(&self) {
        let mut rx = self.0.clone();
        if rx.wait_for(|v| *v).await.is_err() {
            std::future::pending::<()>().await;
        }
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
    pub reply: oneshot::Sender<Result<(), String>>,
}

pub struct Rfid {
    pub kind: &'static str,
    tx: std::sync::mpsc::Sender<WriteReq>,
}

impl Rfid {
    pub async fn write(
        &self,
        tech: Tech,
        uid: Vec<u8>,
        payload: Vec<u8>,
        timeout: Duration,
    ) -> Result<(), String> {
        let (reply, rx) = oneshot::channel();
        let req = WriteReq {
            tech,
            uid,
            payload,
            deadline: Instant::now() + timeout,
            reply,
        };
        self.tx
            .send(req)
            .map_err(|_| "reader stopped".to_string())?;
        match tokio::time::timeout(timeout + Duration::from_secs(2), rx).await {
            Ok(Ok(r)) => r,
            Ok(Err(_)) => Err("reader stopped".into()),
            Err(_) => Err("timeout".into()),
        }
    }
}

pub struct HwInfo {
    pub model: &'static str,
    pub simulated: bool,
}

pub struct Hw {
    pub leds: leds::Leds,
    pub ears: ears::Ears,
    pub player: Arc<player::Player>,
    pub res: Resources,
    pub rfid: Option<Rfid>,
    pub button: bool,
    pub info: HwInfo,
}

impl Hw {
    pub fn open(
        cfg: &Config,
        tx: Tx,
        presence: Option<crate::network::Presence>,
        player: Arc<player::Player>,
    ) -> Hw {
        let sim = cfg.simulate;
        // Physical setup confirmation remains available while LED initialization
        // waits for its hardware thread.
        let button = !sim && button::spawn(&cfg.gpio_chip, cfg.button_gpio, tx.clone(), presence);
        let leds = leds::Leds::open(cfg);
        let ears = ears::Ears::open(sim, tx.clone());
        let res = Resources::new(cfg.sounds_dirs.clone(), cfg.chor_dirs.clone());
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
            player,
            res,
            rfid,
            button,
            info: HwInfo {
                model,
                simulated: sim,
            },
        }
    }

    pub fn describe(&self) -> serde_json::Value {
        serde_json::json!({
            "model": self.info.model,
            "rfid": self.rfid.as_ref().map(|r| r.kind).unwrap_or("none"),
            "left_ear": self.ears.status(0),
            "right_ear": self.ears.status(1),
            "leds": self.leds.ok,
            "button": self.button,
            "simulated": self.info.simulated,
        })
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
