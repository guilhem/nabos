//! ST25R391x reader (/dev/nfc0, pguyot/st25r391x driver, nfc.h protocol v1).
//! Reader state machine:
//! discover+select, read ST25TB blocks or T2T NDEF, removal detection, writes.

use super::Tech;
use super::{
    decode_st25tb, poll_readable, send, st25tb_compatible, HwEvent, TagEvent, Tx, WriteReq,
};
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::sync::mpsc::Receiver;
use std::time::{Duration, Instant};

pub const DEVICE: &str = "/dev/nfc0";
const PROTOCOL_VERSION_1: u64 = 0x004E_4643_0000_0001;
const GET_PROTOCOL_VERSION: libc::c_ulong = 0x8008_4E00; // _IOR('N', 0, uint64_t)
const REMOVED_TIMEOUT: Duration = Duration::from_millis(1500);

const IDLE_REQUEST: u8 = 2;
const IDLE_ACK: u8 = 3;
const DISCOVER: u8 = 4;
const DETECTED: u8 = 5;
const SELECT: u8 = 6;
const SELECTED: u8 = 7;
const TRANSCEIVE: u8 = 8;
const TRANSCEIVE_RESPONSE: u8 = 9;

const TYPE_T2T: u8 = 2;
const TYPE_ST25TB: u8 = 17;
// Protocol mask pynfcdev NFCTagProtocol.ALL.
const ALL_PROTOCOLS: u64 = (1 << 1)
    | (1 << 2)
    | (1 << 3)
    | (1 << 4)
    | (1 << 5)
    | (1 << 6)
    | (1 << 7)
    | (1 << 16)
    | (1 << 17)
    | (1 << 24);

const F_BITS: u8 = 1 << 2;
const F_TX_ONLY: u8 = 1 << 3;
const F_NOCRC_RX: u8 = 1 << 5;
const F_NOPAR_RX: u8 = 1 << 6;
const F_ERROR: u8 = 1 << 7;

const NABAZTAG_NDEF_TYPE: &[u8] = b"tagtagtag.fr:z";

fn tech_name(t: u8) -> &'static str {
    match t {
        1 => "iso14443a",
        2 => "iso14443a_t2t",
        3 => "iso14443a_mifare_classic",
        4 => "iso14443a_nfcdep",
        6 => "iso14443a_t4t",
        7 => "iso14443a_t4t_nfcdep",
        8 => "iso14443a_t1t",
        16 => "iso14443b",
        17 => "st25tb",
        _ => "unknown",
    }
}

/// Tag id as used by the driver (protocol order) from a tag info payload.
fn tag_id(tag_type: u8, info: &[u8]) -> Option<Vec<u8>> {
    match tag_type {
        1..=4 | 6..=8 => {
            let n = *info.get(3)? as usize;
            info.get(4..4 + n).map(<[u8]>::to_vec)
        }
        16 => info.get(0..4).map(<[u8]>::to_vec),
        17 => info.get(0..8).map(<[u8]>::to_vec),
        _ => None,
    }
}

fn uid_for_event(tag_type: u8, id: &[u8]) -> Vec<u8> {
    let mut uid = id.to_vec();
    if tag_type == TYPE_ST25TB {
        uid.reverse();
    }
    uid
}

struct Dev {
    f: File,
    idle: bool,
}

type Io<T> = std::io::Result<T>;

fn io_err(msg: &str) -> std::io::Error {
    std::io::Error::other(msg.to_string())
}

impl Dev {
    fn send(&mut self, typ: u8, payload: &[u8]) -> Io<()> {
        let mut msg = vec![typ];
        msg.extend_from_slice(&(payload.len() as u16).to_le_bytes());
        msg.extend_from_slice(payload);
        if matches!(typ, DISCOVER | SELECT | TRANSCEIVE) {
            self.idle = false;
        }
        self.f.write_all(&msg)
    }

    fn recv(&mut self, timeout: Duration) -> Io<Option<(u8, Vec<u8>)>> {
        if !poll_readable(&self.f, timeout) {
            return Ok(None);
        }
        let mut h = [0u8; 3];
        self.f.read_exact(&mut h)?;
        let mut payload = vec![0; u16::from_le_bytes([h[1], h[2]]) as usize];
        self.f.read_exact(&mut payload)?;
        if h[0] == IDLE_ACK {
            self.idle = true;
        }
        Ok(Some((h[0], payload)))
    }

    fn go_idle(&mut self) -> Io<bool> {
        if self.idle {
            return Ok(true);
        }
        self.send(IDLE_REQUEST, &[])?;
        let end = Instant::now() + Duration::from_secs(1);
        while !self.idle && Instant::now() < end {
            self.recv(Duration::from_millis(100))?;
        }
        // Discovery may resume without an ACK when the driver was already idle;
        // that assumption cannot prove completion of an admitted physical write.
        let confirmed = self.idle;
        self.idle = true;
        Ok(confirmed)
    }

    fn discover(&mut self, protocols: u64, select: bool) -> Io<()> {
        self.go_idle()?;
        let mut p = protocols.to_le_bytes().to_vec();
        p.extend_from_slice(&0u32.to_le_bytes()); // polling period
        p.extend_from_slice(&[0, 0, select as u8]); // device count, max bitrate, flags
        self.send(DISCOVER, &p)
    }

    /// Returns (rx_count, rx data).
    fn transceive(&mut self, tx: &[u8], rx_timeout_us: u16, flags: u8) -> Io<(u16, Vec<u8>)> {
        let count = if flags & F_BITS != 0 {
            tx.len() * 8
        } else {
            tx.len()
        } as u16;
        let mut p = count.to_le_bytes().to_vec();
        p.extend_from_slice(&rx_timeout_us.to_le_bytes());
        p.push(flags);
        p.extend_from_slice(tx);
        self.send(TRANSCEIVE, &p)?;
        let end = Instant::now() + Duration::from_secs(2);
        while Instant::now() < end {
            if let Some((typ, r)) = self.recv(Duration::from_millis(200))? {
                if typ == TRANSCEIVE_RESPONSE && r.len() >= 3 {
                    if r[2] & F_ERROR != 0 {
                        self.idle = true; // chip unselected, field off
                        return Err(io_err("transceive error"));
                    }
                    return Ok((u16::from_le_bytes([r[0], r[1]]), r[3..].to_vec()));
                }
            }
        }
        Err(io_err("transceive timeout"))
    }

    fn st25tb_read(&mut self, blocks: impl IntoIterator<Item = u8>) -> Io<Vec<u8>> {
        let mut out = Vec::new();
        for b in blocks {
            let (n, d) = self.transceive(&[0x08, b], 302, 0)?;
            if n != 6 || d.len() < 4 {
                return Err(io_err("short ST25TB read"));
            }
            out.extend_from_slice(&d[..4]);
        }
        Ok(out)
    }

    fn st25tb_write(&mut self, payload: &[u8], control: &super::WriteControl) -> Io<()> {
        for (i, chunk) in payload.chunks(4).enumerate() {
            let mut tx = vec![0x09, 7 + i as u8];
            tx.extend_from_slice(chunk);
            let (n, _) = self.transceive(&tx, 7000, F_TX_ONLY)?;
            if n != 0 {
                return Err(io_err("unexpected ST25TB write answer"));
            }
        }
        let back = self.st25tb_read(7..7 + (payload.len() / 4) as u8)?;
        // Every block and the verification read have answered, even when the
        // bytes differ. Physical completion is separate from write success.
        control.finish();
        if back != payload {
            return Err(io_err("written data mismatch"));
        }
        Ok(())
    }

    /// Read count blocks from start (4 blocks per READ).
    fn t2t_read(&mut self, start: usize, count: usize, pending: Option<&WriteReq>) -> Io<Vec<u8>> {
        let (mut b, mut left, mut out) = (start, count, Vec::new());
        while left > 0 {
            if let Some(reason) = pending.and_then(WriteReq::stopped) {
                return Err(io_err(reason));
            }
            if b > 255 {
                // ponytail: no T2T sector select, tags above 1 KiB are reported as unknown
                return Err(io_err("T2T sector select unsupported"));
            }
            let (n, d) = self.transceive(&[0x30, b as u8], 25000, 0)?;
            if n != 18 || d.len() < 16 {
                return Err(io_err("short T2T read"));
            }
            let keep = left.min(4);
            out.extend_from_slice(&d[..keep * 4]);
            b += keep;
            left -= keep;
        }
        Ok(out)
    }

    /// Capability container and TLV area (starting at block 4).
    fn t2t_area(&mut self, pending: Option<&WriteReq>) -> Io<([u8; 4], Vec<u8>)> {
        let first = self.t2t_read(3, 4, pending)?;
        let cc = [first[0], first[1], first[2], first[3]];
        if cc[0] != 0xE1 || cc[1] != 0x10 || !(cc[3] == 0x00 || cc[3] == 0x0F) {
            return Err(io_err("not an NDEF formatted T2T"));
        }
        let size = cc[2] as usize * 8;
        let mut area = first[4..].to_vec();
        area.extend(self.t2t_read(7, size.saturating_sub(12) / 4, pending)?);
        Ok((cc, area))
    }

    fn t2t_write(&mut self, req: &WriteReq) -> Io<()> {
        let (cc, area) = self.t2t_area(Some(req))?;
        if cc[3] != 0 {
            return Err(io_err("tag is read-only"));
        }
        let new = ndef::rewrite_area(&area, &ndef::record(NABAZTAG_NDEF_TYPE, &req.payload))
            .ok_or_else(|| io_err("no space on tag"))?;
        req.admit().map_err(|e| io_err(&e))?;
        // All pages form one indivisible write after this admission point.
        for (i, (old, new)) in area.chunks(4).zip(new.chunks(4)).enumerate() {
            if old == new {
                continue;
            }
            let mut tx = vec![0xA2, 4 + i as u8];
            tx.extend_from_slice(new);
            let (n, d) = self.transceive(&tx, 30000, F_BITS | F_NOCRC_RX | F_NOPAR_RX)?;
            if n != 4 || d.first().is_none_or(|a| a & 0x0F != 0x0A) {
                return Err(io_err("T2T write not acknowledged"));
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

enum Next {
    Discover,
    Removal(u8, Vec<u8>),
}

struct Reader {
    dev: Dev,
    tx: Tx,
    requests: Receiver<WriteReq>,
}

impl Reader {
    fn emit(&self, ev: TagEvent) {
        send(&self.tx, HwEvent::Tag(ev));
    }

    fn base_event(tag_type: u8, id: &[u8]) -> TagEvent {
        TagEvent {
            tech: tech_name(tag_type),
            uid: uid_for_event(tag_type, id),
            support: "unknown",
            ..Default::default()
        }
    }

    /// A tag was selected by discovery: read it and report it.
    fn read_selected(&mut self, tag_type: u8, id: Vec<u8>) -> Io<Next> {
        let mut ev = Self::base_event(tag_type, &id);
        let read = match tag_type {
            TYPE_ST25TB if st25tb_compatible(&id) => self
                .dev
                .st25tb_read((7..=15).chain([255]))
                .map(|d| decode_st25tb(&d, &mut ev)),
            TYPE_T2T => self
                .dev
                .t2t_area(None)
                .map(|(cc, area)| decode_t2t(cc, &area, &mut ev)),
            _ => Ok(()),
        };
        if let Err(e) = read {
            debug!("NFC read {}: {e}", ev.tech);
            ev = Self::base_event(tag_type, &id);
        }
        self.emit(ev);
        self.dev.go_idle()?;
        Ok(Next::Removal(tag_type, id))
    }

    fn write(&mut self, req: WriteReq) -> Io<Next> {
        let (tag_type, id) = match req.tech {
            Tech::St25tb => (TYPE_ST25TB, uid_for_event(TYPE_ST25TB, &req.uid)),
            Tech::T2t => (TYPE_T2T, req.uid.clone()),
        };
        if let Some(reason) = req.stopped() {
            let _ = req.reply.send(Err(reason.into()));
            return Ok(Next::Discover);
        }
        self.dev.go_idle()?;
        if let Some(reason) = req.stopped() {
            let _ = req.reply.send(Err(reason.into()));
            return Ok(Next::Discover);
        }
        let mut sel = vec![tag_type];
        if tag_type == TYPE_T2T {
            sel.push(id.len() as u8);
        }
        sel.extend_from_slice(&id);
        self.dev.send(SELECT, &sel)?;
        let mut selected = false;
        while req.stopped().is_none() {
            if let Some((typ, _)) = self.dev.recv(Duration::from_millis(100))? {
                if typ == SELECTED {
                    selected = true;
                    break;
                }
            }
        }
        let result = if let Some(reason) = req.stopped() {
            Err(reason.to_string())
        } else if !selected {
            Err("timeout".to_string())
        } else if tag_type == TYPE_ST25TB {
            req.admit().and_then(|_| {
                self.dev
                    .st25tb_write(&req.payload, &req.control)
                    .map_err(|e| e.to_string())
            })
        } else {
            self.dev.t2t_write(&req).map_err(|e| e.to_string())
        };
        if result.is_ok() {
            req.control.finish();
        }
        if self.dev.go_idle()? {
            // A failed readback is still completed work when physical idle is
            // acknowledged. Preserve write-failed, but release the barrier.
            req.control.finish();
        }
        let _ = req.reply.send(result);
        Ok(Next::Removal(tag_type, id))
    }

    fn discover(&mut self) -> Io<Next> {
        self.dev.discover(ALL_PROTOCOLS, true)?;
        loop {
            if let Ok(req) = self.requests.try_recv() {
                return self.write(req);
            }
            match self.dev.recv(Duration::from_millis(100))? {
                Some((SELECTED, p)) if !p.is_empty() => {
                    return match tag_id(p[0], &p[1..]) {
                        Some(id) => self.read_selected(p[0], id),
                        None => {
                            self.dev.go_idle()?;
                            Ok(Next::Discover)
                        }
                    };
                }
                Some((IDLE_ACK, _)) => self.dev.discover(ALL_PROTOCOLS, true)?,
                _ => {}
            }
        }
    }

    fn removal(&mut self, tag_type: u8, id: Vec<u8>) -> Io<Next> {
        self.dev.discover(1u64 << tag_type, false)?;
        let mut deadline = Instant::now() + REMOVED_TIMEOUT;
        loop {
            if let Ok(req) = self.requests.try_recv() {
                return self.write(req);
            }
            if Instant::now() >= deadline {
                break;
            }
            match self.dev.recv(Duration::from_millis(100))? {
                Some((DETECTED, p)) if !p.is_empty() => {
                    if tag_id(p[0], &p[1..]).as_deref() == Some(&id[..]) {
                        deadline = Instant::now() + REMOVED_TIMEOUT;
                    } else {
                        break;
                    }
                }
                Some((IDLE_ACK, _)) => self.dev.discover(1u64 << tag_type, false)?,
                _ => {}
            }
        }
        self.emit(TagEvent {
            removed: true,
            ..Self::base_event(tag_type, &id)
        });
        self.dev.go_idle()?;
        Ok(Next::Discover)
    }
}

pub fn run(requests: Receiver<WriteReq>, tx: Tx) -> std::io::Result<()> {
    let f = OpenOptions::new().read(true).write(true).open(DEVICE)?;
    let mut version: u64 = 0;
    if unsafe { libc::ioctl(f.as_raw_fd(), GET_PROTOCOL_VERSION as _, &mut version) } < 0
        || version != PROTOCOL_VERSION_1
    {
        return Err(io_err("incompatible nfc device protocol version"));
    }
    let mut r = Reader {
        dev: Dev { f, idle: true },
        tx,
        requests,
    };
    info!("ST25R391x reader ready");
    let mut next = Next::Discover;
    loop {
        next = match next {
            Next::Discover => r.discover()?,
            Next::Removal(t, id) => r.removal(t, id)?,
        };
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
mod cancellation_tests {
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
                payload: vec![1; 36],
                deadline: Instant::now() + Duration::from_secs(1),
                cancel: Cancel::default(),
                control: WriteControl::default(),
                reply,
            },
            rx,
        )
    }
    fn reader(driver: UnixStream) -> Reader {
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let (_requests, requests) = std::sync::mpsc::channel();
        Reader {
            dev: Dev {
                f: File::from(OwnedFd::from(driver)),
                idle: true,
            },
            tx,
            requests,
        }
    }
    fn packet(chip: &mut UnixStream) -> (u8, Vec<u8>) {
        let mut h = [0u8; 3];
        chip.read_exact(&mut h).unwrap();
        let mut payload = vec![0; u16::from_le_bytes([h[1], h[2]]) as usize];
        chip.read_exact(&mut payload).unwrap();
        (h[0], payload)
    }
    fn answer(chip: &mut UnixStream, typ: u8, payload: &[u8]) {
        let mut message = vec![typ];
        message.extend_from_slice(&(payload.len() as u16).to_le_bytes());
        message.extend_from_slice(payload);
        chip.write_all(&message).unwrap();
    }
    #[test]
    fn pending_selection_and_preparation_cancel_before_write_admission() {
        let (driver, mut chip) = UnixStream::pair().unwrap();
        let mut reader = reader(driver);
        let (req, mut rx) = request();
        req.cancel.cancel();
        reader.write(req).unwrap();
        assert_eq!(rx.try_recv().unwrap(), Err("canceled".into()));
        assert!(!poll_readable(&chip, Duration::from_millis(5)));
        let (req, mut rx) = request();
        let control = req.control.clone();
        let server = std::thread::spawn(move || {
            assert_eq!(packet(&mut chip).0, SELECT);
            control.cancel();
            answer(&mut chip, SELECTED, &[]);
            assert_eq!(packet(&mut chip).0, IDLE_REQUEST);
            answer(&mut chip, IDLE_ACK, &[]);
        });
        reader.write(req).unwrap();
        server.join().unwrap();
        assert_eq!(rx.try_recv().unwrap(), Err("canceled".into()));
    }
    #[test]
    fn cancellation_after_first_admission_finishes_all_tag_blocks() {
        let (driver, mut chip) = UnixStream::pair().unwrap();
        let mut reader = reader(driver);
        let (req, mut rx) = request();
        let control = req.control.clone();
        let completed = control.clone();
        let server = std::thread::spawn(move || {
            assert_eq!(packet(&mut chip).0, SELECT);
            answer(&mut chip, SELECTED, &[]);
            let mut written = 0;
            loop {
                let (typ, payload) = packet(&mut chip);
                if typ == IDLE_REQUEST {
                    answer(&mut chip, IDLE_ACK, &[]);
                    break;
                }
                assert_eq!(typ, TRANSCEIVE);
                if payload[5] == 0x09 {
                    written += 1;
                    control.cancel();
                    answer(&mut chip, TRANSCEIVE_RESPONSE, &[0, 0, 0]);
                } else {
                    assert_eq!(payload[5], 0x08);
                    answer(&mut chip, TRANSCEIVE_RESPONSE, &[6, 0, 0, 1, 1, 1, 1, 0, 0]);
                }
            }
            assert_eq!(
                written, 9,
                "cancel cannot interrupt an admitted complete tag write"
            );
        });
        reader.write(req).unwrap();
        server.join().unwrap();
        assert_eq!(rx.try_recv().unwrap(), Ok(()));
        assert!(!completed.uncertain());
    }
    #[test]
    fn acknowledged_readback_mismatch_is_failed_but_physically_completed() {
        let (driver, mut chip) = UnixStream::pair().unwrap();
        let mut reader = reader(driver);
        let (req, mut rx) = request();
        let control = req.control.clone();
        let server = std::thread::spawn(move || {
            assert_eq!(packet(&mut chip).0, SELECT);
            answer(&mut chip, SELECTED, &[]);
            loop {
                let (typ, payload) = packet(&mut chip);
                if typ == IDLE_REQUEST {
                    answer(&mut chip, IDLE_ACK, &[]);
                    break;
                }
                assert_eq!(typ, TRANSCEIVE);
                if payload[5] == 0x09 {
                    answer(&mut chip, TRANSCEIVE_RESPONSE, &[0, 0, 0]);
                } else {
                    assert_eq!(payload[5], 0x08);
                    answer(&mut chip, TRANSCEIVE_RESPONSE, &[6, 0, 0, 2, 2, 2, 2, 0, 0]);
                }
            }
        });
        reader.write(req).unwrap();
        server.join().unwrap();
        assert_eq!(rx.try_recv().unwrap(), Err("written data mismatch".into()));
        assert!(
            !control.uncertain(),
            "a verified failure must not hold the hardware forever"
        );
    }
}
