//! Head button on GPIO 17 (TagTagTag 2019+ boards), GPIO character device.
//! Click, multiple-click and hold detection.

use super::{send, HwEvent, Tx};
use std::time::{Duration, Instant};

const HOLD: Duration = Duration::from_millis(2000);
const CLICK_AND_HOLD: Duration = Duration::from_millis(2000);
const DOUBLE_CLICK: Duration = Duration::from_millis(150);
const TRIPLE_CLICK: Duration = Duration::from_millis(150);

#[derive(Default)]
pub struct Fsm {
    seq: u8,
    down: bool,
    timer: Option<(Instant, &'static str)>,
}

impl Fsm {
    pub fn edge(&mut self, down: bool, now: Instant) -> Vec<&'static str> {
        self.timer = None;
        let mut out = Vec::new();
        if !down && self.down {
            self.down = false;
            out.push("up");
            match self.seq {
                5 => {
                    self.seq = 0;
                    out.push("triple_click");
                }
                3 => {
                    self.seq = 4;
                    self.timer = Some((now + TRIPLE_CLICK, "double_click"));
                }
                1 => {
                    self.seq = 2;
                    self.timer = Some((now + DOUBLE_CLICK, "click"));
                }
                _ => {}
            }
        } else if down && !self.down {
            self.down = true;
            out.push("down");
            match self.seq {
                0 => {
                    self.seq = 1;
                    self.timer = Some((now + HOLD, "hold"));
                }
                2 => {
                    self.seq = 3;
                    self.timer = Some((now + CLICK_AND_HOLD, "click_and_hold"));
                }
                4 => {
                    self.seq = 5;
                    self.timer = Some((now + TRIPLE_CLICK, "click_and_hold"));
                }
                _ => {}
            }
        }
        out
    }

    pub fn deadline(&self) -> Option<Instant> {
        self.timer.map(|(t, _)| t)
    }

    pub fn timeout(&mut self, now: Instant) -> Option<&'static str> {
        match self.timer {
            Some((t, ev)) if now >= t => {
                self.timer = None;
                self.seq = 0;
                Some(ev)
            }
            _ => None,
        }
    }
}

pub fn spawn(chip: &str, line: u32, tx: Tx) -> bool {
    let req = gpiocdev::Request::builder()
        .on_chip(chip)
        .with_consumer("nab-core")
        .with_line(line)
        .as_input()
        .with_edge_detection(gpiocdev::line::EdgeDetection::BothEdges)
        .with_debounce_period(Duration::from_millis(10))
        .request();
    let req = match req {
        Ok(r) => r,
        Err(e) => {
            error!("button {chip}:{line}: {e}");
            return false;
        }
    };
    std::thread::spawn(move || {
        let mut fsm = Fsm::default();
        loop {
            let wait = fsm.deadline().map_or(Duration::from_secs(3600), |d| {
                d.saturating_duration_since(Instant::now())
            });
            match req.wait_edge_event(wait) {
                Ok(true) => match req.read_edge_event() {
                    // Button pulls the line low when pressed.
                    Ok(e) => {
                        let down = e.kind == gpiocdev::line::EdgeKind::Falling;
                        for ev in fsm.edge(down, Instant::now()) {
                            send(&tx, HwEvent::Button(ev));
                        }
                    }
                    Err(e) => error!("button read: {e}"),
                },
                Ok(false) => {
                    if let Some(ev) = fsm.timeout(Instant::now()) {
                        send(&tx, HwEvent::Button(ev));
                    }
                }
                Err(e) => {
                    error!("button wait: {e}");
                    std::thread::sleep(Duration::from_secs(1));
                }
            }
        }
    });
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(fsm: &mut Fsm, steps: &[(u64, Option<bool>)]) -> Vec<&'static str> {
        let t0 = Instant::now();
        let mut out = Vec::new();
        for (ms, edge) in steps {
            let now = t0 + Duration::from_millis(*ms);
            if let Some(ev) = fsm.timeout(now) {
                out.push(ev);
            }
            if let Some(d) = edge {
                out.extend(fsm.edge(*d, now));
            }
        }
        out.retain(|e| *e != "up" && *e != "down");
        out
    }

    #[test]
    fn gestures() {
        assert_eq!(
            run(
                &mut Fsm::default(),
                &[(0, Some(true)), (80, Some(false)), (300, None)]
            ),
            vec!["click"]
        );
        assert_eq!(
            run(
                &mut Fsm::default(),
                &[(0, Some(true)), (2100, None), (2200, Some(false))]
            ),
            vec!["hold"]
        );
        assert_eq!(
            run(
                &mut Fsm::default(),
                &[
                    (0, Some(true)),
                    (50, Some(false)),
                    (100, Some(true)),
                    (150, Some(false)),
                    (400, None)
                ]
            ),
            vec!["double_click"]
        );
        assert_eq!(
            run(
                &mut Fsm::default(),
                &[
                    (0, Some(true)),
                    (50, Some(false)),
                    (100, Some(true)),
                    (150, Some(false)),
                    (200, Some(true)),
                    (250, Some(false))
                ]
            ),
            vec!["triple_click"]
        );
        assert_eq!(
            run(
                &mut Fsm::default(),
                &[
                    (0, Some(true)),
                    (50, Some(false)),
                    (100, Some(true)),
                    (2200, None)
                ]
            ),
            vec!["click_and_hold"]
        );
    }
}
