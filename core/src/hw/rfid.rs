//! Product tag events and write lifecycle; the reader crates own RF and I2C.

use super::{cr14, nfc, send, HwEvent, TagEvent, Tx, WriteReq};
use std::io;
use std::sync::mpsc::{Receiver, RecvTimeoutError};
use std::time::{Duration, Instant};

const POLL_PERIOD: Duration = Duration::from_millis(100);
const REMOVED_TIMEOUT: Duration = Duration::from_millis(1500);

pub enum Reader {
    Cr14(cr14::Reader),
    Nfc(nfc::Reader),
}

impl Reader {
    pub fn open() -> io::Result<Self> {
        if let Some(reader) = nfc::Reader::open()? {
            Ok(Self::Nfc(reader))
        } else {
            cr14::Reader::open().map(Self::Cr14)
        }
    }

    pub fn kind(&self) -> &'static str {
        match self {
            Self::Cr14(_) => "cr14",
            Self::Nfc(_) => "st25r391x",
        }
    }

    pub fn model(&self) -> &'static str {
        match self {
            Self::Cr14(_) => "2019_TAGTAG",
            Self::Nfc(_) => "2022_NFC",
        }
    }

    fn poll(&mut self, known: &[TagEvent]) -> io::Result<Vec<TagEvent>> {
        match self {
            Self::Cr14(r) => r.poll(known),
            Self::Nfc(r) => r.poll(known),
        }
    }

    fn write(&mut self, request: &WriteReq) -> Result<(), String> {
        match self {
            Self::Cr14(r) => r.write(request),
            Self::Nfc(r) => r.write(request),
        }
    }

    fn shutdown(&mut self) -> io::Result<()> {
        match self {
            Self::Cr14(r) => r.shutdown(),
            Self::Nfc(r) => r.shutdown(),
        }
    }

    pub fn run(mut self, requests: Receiver<WriteReq>, tx: Tx) -> io::Result<()> {
        let mut tags = Tags::default();
        info!("{} userspace reader ready", self.kind());
        loop {
            match requests.recv_timeout(POLL_PERIOD) {
                Ok(request) => {
                    let result = self.write(&request);
                    if result.is_ok() {
                        for (tag, seen) in &mut tags.0 {
                            if tag.uid == request.uid {
                                *seen = Instant::now();
                            }
                        }
                    }
                    finish_write(request, result, self.shutdown())?;
                }
                Err(RecvTimeoutError::Disconnected) => return self.shutdown(),
                Err(RecvTimeoutError::Timeout) => {
                    let known: Vec<_> = tags.0.iter().map(|(tag, _)| tag.clone()).collect();
                    match self.poll(&known) {
                        Ok(found) => {
                            for event in tags.observe(found, Instant::now()) {
                                send(&tx, HwEvent::Tag(event));
                            }
                        }
                        Err(e) => warn!("{} polling: {e}", self.kind()),
                    }
                }
            }
            for event in tags.expire(Instant::now()) {
                send(&tx, HwEvent::Tag(event));
            }
        }
    }
}

fn finish_write(
    request: WriteReq,
    result: Result<(), String>,
    cleanup: io::Result<()>,
) -> io::Result<()> {
    // Verified RF off includes the library's programming hold, even after an
    // ambiguous write. Failed cleanup retains an admitted write's maintenance
    // barrier and stops the reader without replaying memory commands.
    if cleanup.is_ok() {
        request.control.finish();
    }
    let _ = request.reply.send(match &cleanup {
        Ok(()) => result,
        Err(e) => Err(format!("{result:?}; reader shutdown: {e}")),
    });
    cleanup
}

#[derive(Default)]
struct Tags(Vec<(TagEvent, Instant)>);

pub fn known(known: &[TagEvent], event: &TagEvent) -> bool {
    known
        .iter()
        .any(|t| t.tech == event.tech && t.uid == event.uid)
}

impl Tags {
    fn observe(&mut self, found: Vec<TagEvent>, now: Instant) -> Vec<TagEvent> {
        let mut events = Vec::new();
        for tag in found {
            if let Some((_, seen)) = self
                .0
                .iter_mut()
                .find(|(t, _)| t.tech == tag.tech && t.uid == tag.uid)
            {
                *seen = now;
            } else {
                events.push(tag.clone());
                self.0.push((tag, now));
            }
        }
        events
    }

    fn expire(&mut self, now: Instant) -> Vec<TagEvent> {
        let mut events = Vec::new();
        self.0.retain(|(tag, seen)| {
            if now.duration_since(*seen) < REMOVED_TIMEOUT {
                return true;
            }
            events.push(TagEvent {
                removed: true,
                ..tag.clone()
            });
            false
        });
        events
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hw::{Cancel, Tech, WriteControl};

    #[test]
    fn write_result_and_verified_physical_completion_are_independent() {
        for (result, cleanup, uncertain) in [
            (Err("readback mismatch".into()), Ok(()), false),
            (Ok(()), Err(io::Error::other("RF off unconfirmed")), true),
        ] {
            let (reply, mut rx) = tokio::sync::oneshot::channel();
            let control = WriteControl::default();
            let request = WriteReq {
                tech: Tech::St25tb,
                uid: vec![0; 8],
                payload: vec![1; 8],
                deadline: Instant::now() + Duration::from_secs(1),
                cancel: Cancel::default(),
                control: control.clone(),
                reply,
            };
            request.admit().unwrap();
            let _ = finish_write(request, result, cleanup);
            assert!(rx.try_recv().unwrap().is_err());
            assert_eq!(control.uncertain(), uncertain);
        }
    }

    #[test]
    fn present_tags_emit_once_and_removal_waits_for_the_last_observation() {
        let mut tags = Tags::default();
        let now = Instant::now();
        let tag = TagEvent {
            tech: "st25tb",
            uid: vec![1; 8],
            ..Default::default()
        };
        assert_eq!(tags.observe(vec![tag.clone()], now).len(), 1);
        assert!(tags.observe(vec![tag], now + POLL_PERIOD).is_empty());
        assert!(tags.expire(now + REMOVED_TIMEOUT).is_empty());
        let removed = tags.expire(now + REMOVED_TIMEOUT + POLL_PERIOD);
        assert_eq!(removed.len(), 1);
        assert!(removed[0].removed);
        assert!(tags.expire(now + REMOVED_TIMEOUT * 2).is_empty());
    }
}
