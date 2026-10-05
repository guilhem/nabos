//! CR14 hardware adapter; RF framing and programming holds belong to the crate.

use super::{decode_st25tb, rfid, st25tb_compatible, TagEvent, Tech, WriteReq};
use ::cr14::{Cr14, State, Uid};
use i2cdev::linux::LinuxI2CDevice;
use std::io;
use std::time::{Duration, Instant};

const POLL_TIMEOUT: Duration = Duration::from_secs(1);
const WRITE_TIMEOUT: Duration = Duration::from_secs(5);

pub struct Reader(Cr14<LinuxI2CDevice>);

fn event(uid: Uid) -> TagEvent {
    TagEvent {
        tech: "st25tb",
        uid: uid.0.into_iter().rev().collect(),
        support: "unknown",
        ..Default::default()
    }
}

impl Reader {
    pub fn open() -> io::Result<Self> {
        let device = LinuxI2CDevice::new("/dev/i2c-1", 0x50).map_err(io::Error::other)?;
        let mut reader = Self(Cr14::new(device));
        reader.ready(Instant::now() + POLL_TIMEOUT)?;
        Ok(reader)
    }

    fn ready(&mut self, deadline: Instant) -> io::Result<()> {
        if self.0.state() != State::Ready {
            self.0.reinitialize(deadline).map_err(io::Error::other)?;
        }
        Ok(())
    }

    pub fn poll(&mut self, known: &[TagEvent]) -> io::Result<Vec<TagEvent>> {
        let deadline = Instant::now() + POLL_TIMEOUT;
        self.ready(deadline)?;
        let mut found = Vec::new();
        for uid in self.0.inventory(deadline).map_err(io::Error::other)? {
            let mut ev = event(uid);
            if !rfid::known(known, &ev) && st25tb_compatible(&uid.0) && Instant::now() < deadline {
                match self
                    .0
                    .read_blocks(uid, &[7, 8, 9, 10, 11, 12, 13, 14, 15, 255], deadline)
                {
                    Ok(blocks) => decode_st25tb(&blocks.concat(), &mut ev),
                    Err(e) => {
                        warn!("CR14 tag {} payload: {e}", uid);
                        self.ready(Instant::now() + POLL_TIMEOUT)?;
                    }
                }
            }
            found.push(ev);
        }
        Ok(found)
    }

    pub fn write(&mut self, req: &WriteReq) -> Result<(), String> {
        if let Some(reason) = req.stopped() {
            return Err(reason.into());
        }
        if req.tech != Tech::St25tb
            || req.uid.len() != 8
            || req.payload.is_empty()
            || req.payload.len() > 36
            || !req.payload.len().is_multiple_of(4)
        {
            return Err("unsupported tag or invalid block payload".into());
        }
        let mut bytes: [u8; 8] = req.uid.as_slice().try_into().unwrap();
        bytes.reverse();
        if !st25tb_compatible(&bytes) {
            return Err("unsupported tag model".into());
        }
        let writes: Vec<_> = req
            .payload
            .as_chunks::<4>()
            .0
            .iter()
            .enumerate()
            .map(|(i, data)| (7 + i as u8, *data))
            .collect();
        self.ready(req.deadline).map_err(|e| e.to_string())?;
        let mut denied = None;
        let progress = self
            .0
            .write_blocks_with_admission(Uid(bytes), &writes, req.deadline, || match req.admit() {
                Ok(()) => Some(Instant::now() + WRITE_TIMEOUT),
                Err(reason) => {
                    denied = Some(reason);
                    None
                }
            })
            .map_err(|e| match denied {
                Some(reason) => {
                    warn!("CR14 write refused after selection: {e}");
                    reason
                }
                None => e.to_string(),
            })?;
        if progress.readback != writes {
            return Err("written data mismatch".into());
        }
        Ok(())
    }

    pub fn shutdown(&mut self) -> io::Result<()> {
        self.0.shutdown().map_err(io::Error::other)
    }
}
