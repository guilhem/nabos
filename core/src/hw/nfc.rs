//! ST25R391x hardware adapter; the crate owns RF, I2C and programming holds.

use super::{decode_st25tb, rfid, st25tb_compatible, TagEvent, Tech, WriteReq};
use i2cdev::linux::LinuxI2CDevice;
use st25r391x::{
    Device, ErrorKind, FrameOptions, Outcome, Settings, St25r391x, Tag, TagId, Technology,
    I2C_ADDRESS,
};
use std::io;
use std::time::{Duration, Instant};

type Io<T> = io::Result<T>;
const POLL_TIMEOUT: Duration = Duration::from_secs(2);
const WRITE_TIMEOUT: Duration = Duration::from_secs(5);
const CLEANUP_TIMEOUT: Duration = Duration::from_millis(100);
const NABAZTAG_NDEF_TYPE: &[u8] = b"tagtagtag.fr:z";

pub struct Reader<D: Device = LinuxI2CDevice> {
    dev: St25r391x<D>,
    settings: Settings,
}

// TagTagTag NFC board profile used by the previous board driver. Keep the
// crate's tuning controls; regulator adjustment remains library-owned.
fn board_settings() -> Settings {
    Settings {
        supply_3v: false,
        receiver_a: [0x08, 0x2d, 0, 0],
        receiver_b: [0x04, 0x3d, 0, 0],
        correlator_b: [0x1b, 0],
        ..Settings::default()
    }
}

fn io_err(message: impl Into<String>) -> io::Error {
    io::Error::other(message.into())
}

fn remaining(deadline: Instant) -> Io<Duration> {
    let left = deadline.saturating_duration_since(Instant::now());
    if left.is_zero() {
        Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "operation deadline reached",
        ))
    } else {
        Ok(left)
    }
}

fn pending(req: &WriteReq) -> Io<()> {
    match req.stopped() {
        Some(reason) => Err(io_err(reason)),
        None => Ok(()),
    }
}

fn event(tag: &Tag) -> TagEvent {
    let (tech, uid) = match tag {
        Tag::St25tb(t) => ("st25tb", t.uid.into_iter().rev().collect()),
        Tag::NfcB(t) => ("iso14443b", t.pupi.to_vec()),
        Tag::NfcA(t) => {
            let tech = match (t.iso_dep(), t.nfc_dep()) {
                (true, true) => "iso14443a_t4t_nfcdep",
                (true, false) => "iso14443a_t4t",
                (false, true) => "iso14443a_nfcdep",
                _ if t.classic_hint() => "iso14443a_mifare_classic",
                _ if t.type2_hint() => "iso14443a_t2t",
                _ => "iso14443a",
            };
            (tech, t.uid.clone())
        }
    };
    TagEvent {
        tech,
        uid,
        support: "unknown",
        ..Default::default()
    }
}

fn tag_id(ev: &TagEvent) -> Option<TagId> {
    if ev.tech == "st25tb" {
        let mut uid: [u8; 8] = ev.uid.as_slice().try_into().ok()?;
        uid.reverse();
        Some(TagId::St25tb(uid))
    } else if ev.tech == "iso14443b" {
        Some(TagId::NfcB(ev.uid.as_slice().try_into().ok()?))
    } else if ev.tech.starts_with("iso14443a") && [4, 7, 10].contains(&ev.uid.len()) {
        Some(TagId::NfcA(ev.uid.clone()))
    } else {
        None
    }
}

impl Reader {
    pub fn open() -> Io<Option<Self>> {
        let device = LinuxI2CDevice::new("/dev/i2c-1", I2C_ADDRESS).map_err(io::Error::other)?;
        Self::from_device(device, board_settings())
    }
}

impl<D: Device> Reader<D> {
    fn from_device(device: D, settings: Settings) -> Io<Option<Self>> {
        let mut dev = St25r391x::new(device);
        match dev.probe(POLL_TIMEOUT) {
            Ok(_) => {}
            Err(e)
                if matches!(&e.kind, ErrorKind::UnexpectedIdentity(_))
                    || matches!(e.raw_os_error(), Some(libc::ENXIO | libc::EREMOTEIO)) =>
            {
                return Ok(None)
            }
            Err(e) => return Err(io::Error::other(e)),
        }
        dev.initialize(settings.clone(), POLL_TIMEOUT)
            .map_err(io::Error::other)?;
        Ok(Some(Self { dev, settings }))
    }

    fn ready(&mut self, deadline: Instant) -> Io<()> {
        if self.dev.is_poisoned() {
            self.dev
                .initialize(self.settings.clone(), remaining(deadline)?)
                .map_err(io::Error::other)?;
        }
        Ok(())
    }

    pub fn shutdown(&mut self) -> Io<()> {
        self.dev.shutdown(CLEANUP_TIMEOUT).map_err(io::Error::other)
    }

    pub fn poll(&mut self, known: &[TagEvent]) -> Io<Vec<TagEvent>> {
        let deadline = Instant::now() + POLL_TIMEOUT;
        let mut found = Vec::new();
        let result = self.poll_inner(known, deadline, &mut found);
        let cleanup = self.shutdown();
        match (result, cleanup) {
            (Ok(()), Ok(())) => Ok(found),
            (Err(operation), Ok(())) if !found.is_empty() => {
                warn!("NFC partial poll: {operation}");
                Ok(found)
            }
            (Err(operation), Ok(())) => Err(operation),
            (Ok(_), Err(e)) => Err(e),
            (Err(operation), Err(cleanup)) => {
                Err(io_err(format!("{operation}; shutdown: {cleanup}")))
            }
        }
    }

    fn poll_inner(
        &mut self,
        known: &[TagEvent],
        deadline: Instant,
        found: &mut Vec<TagEvent>,
    ) -> Io<()> {
        // Select every current identity, rather than letting general discovery
        // repeatedly choose another tag and expire a still-present one.
        for ev in known {
            let Some(id) = tag_id(ev) else { continue };
            self.ready(deadline)?;
            match self.dev.select(&id, remaining(deadline)?) {
                Ok(tag) => found.push(event(&tag)),
                Err(e) if matches!(&e.kind, ErrorKind::NoResponse | ErrorKind::TagMismatch) => {}
                Err(e) => return Err(io::Error::other(e)),
            }
            self.shutdown()?;
        }
        for technology in [Technology::NfcA, Technology::NfcB, Technology::St25tb] {
            self.ready(deadline)?;
            if let Some(tag) = self
                .dev
                .discover(technology, remaining(deadline)?)
                .map_err(io::Error::other)?
            {
                let mut ev = event(&tag);
                if !rfid::known(found, &ev) {
                    if !rfid::known(known, &ev) {
                        if let Err(e) = self.read_selected(&tag, &mut ev, deadline) {
                            warn!("NFC {} tag {:?} payload: {e}", ev.tech, ev.uid);
                            ev = event(&tag);
                        }
                    }
                    found.push(ev);
                }
            }
            self.shutdown()?;
            if Instant::now() >= deadline {
                break;
            }
        }
        Ok(())
    }

    fn read_selected(&mut self, tag: &Tag, ev: &mut TagEvent, deadline: Instant) -> Io<()> {
        match tag {
            Tag::St25tb(t) if st25tb_compatible(&t.uid) => {
                let data = self.st25tb_read((7..=15).chain([255]), deadline)?;
                decode_st25tb(&data, ev);
            }
            Tag::NfcA(t) if t.type2_hint() => {
                if let Some((cc, area)) = self.t2t_area(deadline, None)? {
                    decode_t2t(cc, &area, ev);
                }
            }
            _ => {}
        }
        Ok(())
    }

    fn st25tb_read(
        &mut self,
        blocks: impl IntoIterator<Item = u8>,
        deadline: Instant,
    ) -> Io<Vec<u8>> {
        let mut data = Vec::new();
        for block in blocks {
            let mut rx = [0; 6];
            let result = self
                .dev
                .exchange(
                    &[0x08, block],
                    &mut rx,
                    FrameOptions::default(),
                    remaining(deadline)?,
                )
                .map_err(io::Error::other)?;
            if result.outcome != Outcome::Received || result.bits != 48 || result.bytes != 6 {
                return Err(io_err("short ST25TB read"));
            }
            data.extend_from_slice(&rx[..4]);
        }
        Ok(data)
    }

    fn t2t_read(
        &mut self,
        start: usize,
        count: usize,
        deadline: Instant,
        req: Option<&WriteReq>,
    ) -> Io<Vec<u8>> {
        if start + count > 256 {
            // ponytail: no sector select; support pages 0..255 until larger tags are qualified.
            return Err(io_err("T2T sector select unsupported"));
        }
        let mut out = Vec::with_capacity(count * 4);
        let mut page = start;
        while page < start + count {
            if let Some(req) = req {
                pending(req)?;
            }
            let mut rx = [0; 18];
            let options = FrameOptions {
                response_timeout: Duration::from_millis(25),
                ..FrameOptions::default()
            };
            let result = self
                .dev
                .exchange(&[0x30, page as u8], &mut rx, options, remaining(deadline)?)
                .map_err(io::Error::other)?;
            if result.outcome != Outcome::Received || result.bits != 144 || result.bytes != 18 {
                return Err(io_err("short T2T read"));
            }
            let keep = (start + count - page).min(4);
            out.extend_from_slice(&rx[..keep * 4]);
            page += keep;
        }
        Ok(out)
    }

    fn t2t_area(
        &mut self,
        deadline: Instant,
        req: Option<&WriteReq>,
    ) -> Io<Option<([u8; 4], Vec<u8>)>> {
        let cc: [u8; 4] = self.t2t_read(3, 1, deadline, req)?.try_into().unwrap();
        let size = cc[2] as usize * 8;
        if cc[0] != 0xe1 || cc[1] != 0x10 || ![0, 0x0f].contains(&cc[3]) || size == 0 || size > 1008
        {
            return Ok(None);
        }
        let area = self.t2t_read(4, size / 4, deadline, req)?;
        Ok(Some((cc, area)))
    }

    pub fn write(&mut self, req: &WriteReq) -> Result<(), String> {
        self.write_inner(req).map_err(|e| e.to_string())
    }

    fn write_inner(&mut self, req: &WriteReq) -> Io<()> {
        pending(req)?;
        if req.payload.is_empty() || req.payload.len() > 36 || !req.payload.len().is_multiple_of(4)
        {
            return Err(io_err("invalid block payload"));
        }
        let id = match req.tech {
            Tech::St25tb => {
                let mut uid: [u8; 8] = req
                    .uid
                    .as_slice()
                    .try_into()
                    .map_err(|_| io_err("invalid ST25TB UID"))?;
                uid.reverse();
                if !st25tb_compatible(&uid) {
                    return Err(io_err("unsupported tag model"));
                }
                TagId::St25tb(uid)
            }
            Tech::T2t if [4, 7, 10].contains(&req.uid.len()) => TagId::NfcA(req.uid.clone()),
            _ => return Err(io_err("invalid T2T UID")),
        };
        self.ready(req.deadline)?;
        pending(req)?;
        let tag = self
            .dev
            .select(&id, remaining(req.deadline)?)
            .map_err(io::Error::other)?;
        let prepared = if req.tech == Tech::T2t {
            if !matches!(tag, Tag::NfcA(ref t) if t.type2_hint()) {
                return Err(io_err("not a T2T tag"));
            }
            let (cc, area) = self
                .t2t_area(req.deadline, Some(req))?
                .ok_or_else(|| io_err("unsupported T2T capability container"))?;
            if cc[3] != 0 {
                return Err(io_err("tag is read-only"));
            }
            let new = ndef::rewrite_area(&area, &ndef::record(NABAZTAG_NDEF_TYPE, &req.payload))
                .ok_or_else(|| io_err("no space on tag"))?;
            Some((area, new))
        } else {
            None
        };
        req.admit().map_err(io_err)?;
        let deadline = Instant::now() + WRITE_TIMEOUT;
        if let Some((area, new)) = prepared {
            for (i, (old, data)) in area
                .as_chunks::<4>()
                .0
                .iter()
                .zip(new.as_chunks::<4>().0)
                .enumerate()
            {
                if old == data {
                    continue;
                }
                let tx = [0xa2, 4 + i as u8, data[0], data[1], data[2], data[3]];
                let mut rx = [0; 1];
                let options = FrameOptions {
                    rx_crc: false,
                    rx_parity: false,
                    response_timeout: Duration::from_millis(30),
                    ..FrameOptions::default()
                };
                let result = self
                    .dev
                    .exchange(&tx, &mut rx, options, remaining(deadline)?)
                    .map_err(io::Error::other)?;
                if result.outcome != Outcome::Received
                    || result.bytes != 1
                    || result.bits != 4
                    || rx[0] & 0x0f != 0x0a
                {
                    return Err(io_err("T2T write not acknowledged"));
                }
            }
            if self.t2t_read(4, new.len() / 4, deadline, None)? != new {
                return Err(io_err("written data mismatch"));
            }
        } else {
            for (i, data) in req.payload.as_chunks::<4>().0.iter().enumerate() {
                let tx = [0x09, 7 + i as u8, data[0], data[1], data[2], data[3]];
                let options = FrameOptions {
                    tx_only: true,
                    ..FrameOptions::default()
                };
                let result = self
                    .dev
                    .exchange(&tx, &mut [], options, remaining(deadline)?)
                    .map_err(io::Error::other)?;
                if result.outcome != Outcome::Transmitted {
                    return Err(io_err("unexpected ST25TB write answer"));
                }
            }
            if self.st25tb_read(7..7 + (req.payload.len() / 4) as u8, deadline)? != req.payload {
                return Err(io_err("written data mismatch"));
            }
        }
        Ok(())
    }
}

pub mod ndef {
    //! Minimal NFC Forum TLV/NDEF handling for Type 2 tags.

    /// NDEF message values found in a TLV area (None: no NDEF TLV at all).
    pub fn messages(area: &[u8]) -> Vec<Vec<u8>> {
        let mut out = Vec::new();
        let mut i = 0;
        while i < area.len() {
            match area[i] {
                0x00 => i += 1,
                0xFE => break,
                t => {
                    let Some(&l) = area.get(i + 1) else { break };
                    let (len, start) = if l == 0xFF {
                        match area.get(i + 2..i + 4) {
                            Some(b) => (u16::from_be_bytes([b[0], b[1]]) as usize, i + 4),
                            None => break,
                        }
                    } else {
                        (l as usize, i + 2)
                    };
                    let Some(v) = area.get(start..start + len) else {
                        break;
                    };
                    if t == 0x03 {
                        out.push(v.to_vec());
                    }
                    i = start + len;
                }
            }
        }
        out
    }

    /// (tnf, type, payload) records of an NDEF message.
    pub fn records(msg: &[u8]) -> Vec<(u8, Vec<u8>, Vec<u8>)> {
        let mut out = Vec::new();
        let mut i = 0;
        while i + 3 <= msg.len() {
            let h = msg[i];
            let type_len = msg[i + 1] as usize;
            let mut j = i + 2;
            let payload_len = if h & 0x10 != 0 {
                j += 1;
                msg[i + 2] as usize
            } else {
                let Some(b) = msg.get(j..j + 4) else { break };
                j += 4;
                u32::from_be_bytes([b[0], b[1], b[2], b[3]]) as usize
            };
            let id_len = if h & 0x08 != 0 {
                let Some(&l) = msg.get(j) else { break };
                j += 1;
                l as usize
            } else {
                0
            };
            let Some(t) = msg.get(j..j + type_len) else {
                break;
            };
            let p0 = j + type_len + id_len;
            let Some(p) = msg.get(p0..p0 + payload_len) else {
                break;
            };
            out.push((h & 0x07, t.to_vec(), p.to_vec()));
            i = p0 + payload_len;
            if h & 0x40 != 0 {
                break;
            }
        }
        out
    }

    /// Single short external record (MB|ME|SR, TNF 4).
    pub fn record(typ: &[u8], payload: &[u8]) -> Vec<u8> {
        let mut r = vec![0xD4, typ.len() as u8, payload.len() as u8];
        r.extend_from_slice(typ);
        r.extend_from_slice(payload);
        r
    }

    /// Replace NDEF TLVs of the area by one message, keep other TLVs, pad with zeros.
    pub fn rewrite_area(area: &[u8], message: &[u8]) -> Option<Vec<u8>> {
        let mut tlv = vec![0x03];
        if message.len() > 254 {
            tlv.push(0xFF);
            tlv.extend_from_slice(&(message.len() as u16).to_be_bytes());
        } else {
            tlv.push(message.len() as u8);
        }
        tlv.extend_from_slice(message);
        let mut out = Vec::new();
        let mut pending = Some(tlv);
        let mut i = 0;
        while i < area.len() {
            match area[i] {
                0x00 => {
                    out.push(0);
                    i += 1;
                }
                0xFE => break,
                t => {
                    let l = *area.get(i + 1)?;
                    let (len, start) = if l == 0xFF {
                        (
                            u16::from_be_bytes([*area.get(i + 2)?, *area.get(i + 3)?]) as usize,
                            i + 4,
                        )
                    } else {
                        (l as usize, i + 2)
                    };
                    let end = start + len;
                    area.get(start..end)?;
                    if t == 0x03 {
                        if let Some(p) = pending.take() {
                            out.extend(p);
                        }
                    } else {
                        out.extend_from_slice(&area[i..end]);
                    }
                    i = end;
                }
            }
        }
        if let Some(p) = pending.take() {
            out.extend(p);
        }
        out.push(0xFE);
        if out.len() > area.len() {
            return None;
        }
        out.resize(area.len(), 0);
        Some(out)
    }
}

/// Decode a T2T TLV area into an event.
fn decode_t2t(cc: [u8; 4], area: &[u8], ev: &mut TagEvent) {
    ev.locked = cc[3] == 0x0F;
    let msgs = ndef::messages(area);
    let records: Vec<_> = msgs.iter().flat_map(|m| ndef::records(m)).collect();
    if let Some((_, _, p)) = records
        .iter()
        .find(|(tnf, t, p)| *tnf == 4 && t == NABAZTAG_NDEF_TYPE && p.len() >= 4)
    {
        ev.app = Some(p[0]);
        ev.picture = Some(p[1]);
        ev.data = Some(p[4..p.len().min(36)].to_vec());
        ev.support = "formatted";
    } else if !records.is_empty() {
        ev.support = "foreign-data";
    } else if ev.locked {
        ev.support = "locked";
    } else {
        ev.support = "empty";
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn t2t_ndef_roundtrip() {
        // Blank formatted NTAG: empty NDEF TLV then terminator.
        let mut area = vec![0x03, 0x00, 0xFE];
        area.resize(48, 0);
        let mut ev = TagEvent::default();
        decode_t2t([0xE1, 0x10, 6, 0], &area, &mut ev);
        assert_eq!(ev.support, "empty");
        let payload = crate::hw::encode_tag_data(7, 9, Some(&[2]));
        let new = ndef::rewrite_area(&area, &ndef::record(NABAZTAG_NDEF_TYPE, &payload)).unwrap();
        let mut ev = TagEvent::default();
        decode_t2t([0xE1, 0x10, 6, 0x0F], &new, &mut ev);
        assert_eq!(
            (ev.support, ev.app, ev.picture, ev.locked),
            ("formatted", Some(9), Some(7), true)
        );
        assert_eq!(ev.data.as_deref(), Some(&[2, 0xFF, 0xFF, 0xFF][..]));
        // Foreign NDEF (URI record) is kept as foreign data; too small area refused.
        let uri = [
            0x03, 0x08, 0xD1, 0x01, 0x04, b'U', 0x04, b'a', b'.', b'b', 0xFE, 0, 0, 0, 0, 0,
        ];
        let mut ev = TagEvent::default();
        decode_t2t([0xE1, 0x10, 2, 0], &uri, &mut ev);
        assert_eq!(ev.support, "foreign-data");
        assert!(ndef::rewrite_area(&uri, &ndef::record(NABAZTAG_NDEF_TYPE, &payload)).is_none());
    }
}

#[cfg(test)]
mod adapter_tests {
    use super::*;
    use crate::hw::{Cancel, WriteControl};
    use i2cdev::linux::LinuxI2CError;
    use std::sync::{Arc, Mutex};

    const UID: [u8; 8] = [1, 2, 3, 4, 5, 0x18, 2, 0xd0];

    struct Chip {
        registers: [u8; 64],
        irqs: [u8; 4],
        tx: Vec<u8>,
        rx: Vec<u8>,
        partial: u8,
        accesses: Vec<Vec<u8>>,
        frames: Vec<Vec<u8>>,
        blocks: [[u8; 4]; 256],
        pages: [u8; 1024],
        probe_errno: Option<i32>,
        cancel_select: Option<Cancel>,
        cancel_program: Option<Cancel>,
        expire_program: Option<Instant>,
        fail_program: bool,
        fail_payload: bool,
        fail_discovery: bool,
        fail_shutdown: bool,
        ack: u8,
    }

    impl Default for Chip {
        fn default() -> Self {
            let mut registers = [0; 64];
            registers[0x3f] = 0x2a;
            let mut pages = [0; 1024];
            pages[12..16].copy_from_slice(&[0xe1, 0x10, 6, 0]);
            pages[16..19].copy_from_slice(&[3, 0, 0xfe]);
            Self {
                registers,
                irqs: [0; 4],
                tx: Vec::new(),
                rx: Vec::new(),
                partial: 0,
                accesses: Vec::new(),
                frames: Vec::new(),
                blocks: [[0xff; 4]; 256],
                pages,
                probe_errno: None,
                cancel_select: None,
                cancel_program: None,
                expire_program: None,
                fail_program: false,
                fail_payload: false,
                fail_discovery: false,
                fail_shutdown: false,
                ack: 0x0a,
            }
        }
    }

    #[derive(Clone, Default)]
    struct Bus(Arc<Mutex<Chip>>);

    impl Device for Bus {
        fn transfer(
            &mut self,
            write: &[u8],
            read: Option<&mut [u8]>,
        ) -> Result<u32, LinuxI2CError> {
            let mut c = self.0.lock().unwrap();
            c.accesses.push(write.to_vec());
            if let Some(out) = read {
                match write {
                    [0x7f] if c.probe_errno.is_some() => {
                        return Err(LinuxI2CError::Errno(c.probe_errno.unwrap()))
                    }
                    [0x5a] => {
                        out.copy_from_slice(&c.irqs);
                        c.irqs = [0; 4];
                    }
                    [0x5e] => out.copy_from_slice(&[c.rx.len() as u8, c.partial << 1]),
                    [0x9f] => {
                        assert_eq!(out.len(), c.rx.len());
                        out.copy_from_slice(&c.rx);
                    }
                    [reg] => {
                        let reg = (reg & 0x3f) as usize;
                        out.copy_from_slice(&c.registers[reg..reg + out.len()]);
                    }
                    _ => panic!("unexpected read: {write:x?}"),
                }
                return Ok(2);
            }
            match write {
                [0xc2]
                    if c.fail_shutdown
                        && c.frames
                            .iter()
                            .any(|f| matches!(f.first(), Some(9 | 0xa2 | 0x05))) =>
                {
                    return Err(LinuxI2CError::Errno(libc::EIO))
                }
                [0xd6] => c.irqs[1] = 0x80,
                [0xc8] => c.irqs[1] = 2,
                [0xdb] => {
                    c.tx.clear();
                    c.rx.clear();
                    c.partial = 0;
                }
                [0xc6 | 0xc7] => {
                    c.rx = vec![0x44, 0];
                    c.irqs = [0x18, 0, 0, 0];
                }
                [0xc4 | 0xc5] => {
                    let frame = c.tx.clone();
                    c.frames.push(frame.clone());
                    if c.fail_discovery && frame.first() == Some(&0x05) {
                        return Err(LinuxI2CError::Errno(libc::EIO));
                    }
                    c.partial = 0;
                    c.rx = match frame.as_slice() {
                        [0x0c] => Vec::new(),
                        [0x06, 0] | [0x0e, 0x42] => vec![0x42, 0, 0],
                        [0x0b] => {
                            if let Some(cancel) = c.cancel_select.take() {
                                cancel.cancel();
                            }
                            [UID.as_slice(), &[0, 0]].concat()
                        }
                        [0x93, 0x20] => vec![1, 2, 3, 4, 4],
                        [0x93, 0x70, 1, 2, 3, 4, 4] => {
                            if let Some(cancel) = c.cancel_select.take() {
                                cancel.cancel();
                            }
                            vec![0, 0, 0]
                        }
                        [0x05, 0, 0 | 8] => Vec::new(),
                        [0x08, block] => [c.blocks[*block as usize].as_slice(), &[0, 0]].concat(),
                        [0x30, page] => {
                            let start = *page as usize * 4;
                            [&c.pages[start..start + 16], &[0, 0]].concat()
                        }
                        [0x09, block, data @ ..] => {
                            c.blocks[*block as usize].copy_from_slice(data);
                            Vec::new()
                        }
                        [0xa2, page, data @ ..] => {
                            assert_eq!(write, &[0xc4], "WRITE includes TX CRC");
                            assert_eq!(
                                c.registers[5] & 0x40,
                                0x40,
                                "four-bit ACK has no RX parity"
                            );
                            assert_eq!(c.registers[0x0a], 0x80, "four-bit ACK has no RX CRC");
                            let start = *page as usize * 4;
                            c.pages[start..start + 4].copy_from_slice(data);
                            c.partial = 4;
                            vec![c.ack]
                        }
                        _ => panic!("unexpected RF frame: {frame:x?}"),
                    };
                    c.irqs = [
                        if c.rx.is_empty() { 8 } else { 0x18 },
                        if c.rx.is_empty() { 0x40 } else { 0 },
                        0,
                        0,
                    ];
                    if c.fail_payload && matches!(frame.first(), Some(8 | 0x30)) {
                        c.fail_payload = false;
                        return Err(LinuxI2CError::Errno(libc::EIO));
                    }
                    if matches!(frame.first(), Some(9 | 0xa2)) {
                        if let Some(cancel) = c.cancel_program.take() {
                            cancel.cancel();
                        }
                        if let Some(deadline) = c.expire_program.take() {
                            std::thread::sleep(
                                deadline.saturating_duration_since(Instant::now())
                                    + Duration::from_millis(2),
                            );
                        }
                        if c.fail_program {
                            return Err(LinuxI2CError::Errno(libc::EIO));
                        }
                    }
                }
                [0xfb, ..] | [0x80, ..] => {
                    if write[0] == 0x80 {
                        c.tx = write[1..].to_vec();
                    }
                }
                [command] if *command >= 0xc0 => {}
                [reg, data @ ..] => {
                    let start = *reg as usize;
                    c.registers[start..start + data.len()].copy_from_slice(data);
                    if *reg == 2 {
                        c.registers[0x31] = if data[0] == 0 {
                            0
                        } else {
                            0x10 | (data[0] & 0x40) >> 1
                        };
                        if data[0] == 0x81 {
                            c.irqs[0] = 0x80;
                        }
                    }
                }
                _ => panic!("unexpected write: {write:x?}"),
            }
            Ok(1)
        }
    }

    fn reader() -> (Reader<Bus>, Bus) {
        let bus = Bus::default();
        let reader = Reader::from_device(bus.clone(), board_settings())
            .unwrap()
            .unwrap();
        (reader, bus)
    }

    fn request(tech: Tech) -> (WriteReq, tokio::sync::oneshot::Receiver<Result<(), String>>) {
        let (reply, rx) = tokio::sync::oneshot::channel();
        (
            WriteReq {
                tech,
                uid: if tech == Tech::St25tb {
                    UID.into_iter().rev().collect()
                } else {
                    vec![1, 2, 3, 4]
                },
                payload: crate::hw::encode_tag_data(7, 9, Some(&[2])),
                deadline: Instant::now() + Duration::from_secs(1),
                cancel: Cancel::default(),
                control: WriteControl::default(),
                reply,
            },
            rx,
        )
    }

    #[test]
    fn probe_rejections_and_drop_never_initialize_or_shutdown() {
        for errno in [
            None,
            Some(libc::ENXIO),
            Some(libc::EREMOTEIO),
            Some(libc::EIO),
        ] {
            let bus = Bus::default();
            {
                let mut c = bus.0.lock().unwrap();
                c.registers[0x3f] = 0;
                c.probe_errno = errno;
            }
            let result = Reader::from_device(bus.clone(), board_settings());
            if errno == Some(libc::EIO) {
                assert!(result.is_err());
            } else {
                assert!(result.unwrap().is_none());
            }
            assert_eq!(bus.0.lock().unwrap().accesses, vec![vec![0x7f]]);
        }
    }

    #[test]
    fn canceled_pending_and_selection_requests_never_program() {
        let (mut reader, bus) = reader();
        let (req, _rx) = request(Tech::St25tb);
        req.cancel.cancel();
        let before = bus.0.lock().unwrap().accesses.len();
        assert_eq!(reader.write(&req), Err("canceled".into()));
        assert_eq!(bus.0.lock().unwrap().accesses.len(), before);
        let (req, _rx) = request(Tech::St25tb);
        bus.0.lock().unwrap().cancel_select = Some(req.cancel.clone());
        assert_eq!(reader.write(&req), Err("canceled".into()));
        assert!(!req.control.uncertain());
        assert!(!bus.0.lock().unwrap().frames.iter().any(|f| f[0] == 9));
        reader.shutdown().unwrap();
    }

    #[test]
    fn admitted_write_finishes_after_cancel_and_pending_deadline() {
        let (mut reader, bus) = reader();
        let (mut req, _rx) = request(Tech::St25tb);
        req.deadline = Instant::now() + Duration::from_millis(200);
        {
            let mut c = bus.0.lock().unwrap();
            c.cancel_program = Some(req.cancel.clone());
            c.expire_program = Some(req.deadline);
        }
        reader.write(&req).unwrap();
        assert!(Instant::now() > req.deadline);
        let c = bus.0.lock().unwrap();
        assert_eq!(
            c.frames.iter().filter(|f| f[0] == 9).count(),
            req.payload.len() / 4
        );
        assert_eq!(c.blocks[7..7 + req.payload.len() / 4].concat(), req.payload);
        drop(c);
        reader.shutdown().unwrap();
        assert!(
            req.control.uncertain(),
            "only the shared RFID loop finishes the barrier"
        );
    }

    #[test]
    fn type2_uses_four_bit_ack_and_reads_back_written_ndef() {
        let (mut reader, bus) = reader();
        let (req, _rx) = request(Tech::T2t);
        reader.write(&req).unwrap();
        let mut ev = TagEvent::default();
        decode_t2t(
            [0xe1, 0x10, 6, 0],
            &bus.0.lock().unwrap().pages[16..64],
            &mut ev,
        );
        assert_eq!((ev.app, ev.picture), (Some(9), Some(7)));
        reader.shutdown().unwrap();
        assert!(req.control.uncertain());
        let (mut req, _rx) = request(Tech::T2t);
        req.payload[0] ^= 1;
        bus.0.lock().unwrap().ack = 0;
        assert!(reader.write(&req).unwrap_err().contains("not acknowledged"));
        reader.shutdown().unwrap();
    }

    #[test]
    fn known_tag_polling_verifies_presence_without_payload_reads() {
        let (mut reader, bus) = reader();
        let known = [
            TagEvent {
                tech: "st25tb",
                uid: UID.into_iter().rev().collect(),
                ..Default::default()
            },
            TagEvent {
                tech: "iso14443a_t2t",
                uid: vec![1, 2, 3, 4],
                ..Default::default()
            },
        ];
        let found = reader.poll(&known).unwrap();
        assert_eq!(found.len(), 2);
        assert!(known.iter().all(|t| rfid::known(&found, t)));
        assert!(!bus
            .0
            .lock()
            .unwrap()
            .frames
            .iter()
            .any(|f| matches!(f[0], 8 | 0x30)));
        assert_eq!(reader.dev.field_state(), st25r391x::FieldState::Off);
        bus.0.lock().unwrap().fail_payload = true;
        let found = reader.poll(&[]).unwrap();
        let unreadable = found.iter().find(|t| t.tech == "iso14443a_t2t").unwrap();
        assert_eq!(unreadable.support, "unknown");
        assert!(unreadable.data.is_none());
        assert!(!reader.dev.is_poisoned());
        assert_eq!(reader.dev.field_state(), st25r391x::FieldState::Off);
    }

    #[test]
    fn partial_poll_preserves_observations_only_after_verified_shutdown() {
        let known = [
            TagEvent {
                tech: "st25tb",
                uid: UID.into_iter().rev().collect(),
                ..Default::default()
            },
            TagEvent {
                tech: "iso14443a_t2t",
                uid: vec![1, 2, 3, 4],
                ..Default::default()
            },
        ];
        for known in [known.as_slice(), &[]] {
            for fail_cleanup in [false, true] {
                let (mut reader, bus) = reader();
                {
                    let mut c = bus.0.lock().unwrap();
                    c.fail_discovery = true;
                    c.fail_shutdown = fail_cleanup;
                }
                let result = reader.poll(known);
                if fail_cleanup {
                    assert!(result.unwrap_err().to_string().contains("shutdown"));
                } else {
                    let found = result.unwrap();
                    if known.is_empty() {
                        assert_eq!(found.len(), 1);
                        assert_eq!(found[0].tech, "iso14443a_t2t");
                    } else {
                        assert!(known.iter().all(|t| rfid::known(&found, t)));
                    }
                    assert_eq!(reader.dev.field_state(), st25r391x::FieldState::Off);
                }
                assert!(reader.dev.is_poisoned());
                {
                    let mut c = bus.0.lock().unwrap();
                    assert_eq!(c.frames.last().unwrap().first(), Some(&0x05));
                    c.fail_discovery = false;
                    c.fail_shutdown = false;
                }
                assert!(!reader.poll(known).unwrap().is_empty());
                assert!(!reader.dev.is_poisoned());
                assert_eq!(reader.dev.field_state(), st25r391x::FieldState::Off);
            }
        }
    }

    #[test]
    fn ambiguous_write_and_failed_cleanup_keep_barrier_and_never_replay() {
        let (mut reader, bus) = reader();
        let (req, _rx) = request(Tech::St25tb);
        {
            let mut c = bus.0.lock().unwrap();
            c.fail_program = true;
            c.fail_shutdown = true;
        }
        let error = reader.write(&req).unwrap_err();
        assert!(error.contains("PossiblyStarted") && error.contains("cleanup"));
        assert!(reader.shutdown().is_err());
        assert!(req.control.uncertain());
        assert_eq!(
            bus.0
                .lock()
                .unwrap()
                .frames
                .iter()
                .filter(|f| f[0] == 9)
                .count(),
            1
        );
        {
            let mut c = bus.0.lock().unwrap();
            c.fail_program = false;
            c.fail_shutdown = false;
        }
        let (next, _rx) = request(Tech::St25tb);
        reader.write(&next).unwrap();
        assert!(!reader.dev.is_poisoned());
        reader.shutdown().unwrap();
    }
}
