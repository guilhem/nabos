//! Head button on GPIO 17 (TagTagTag 2019+ boards), GPIO character device.
//! Click, multiple-click and hold detection. Two clicks then a third press held
//! 10 s is `double_click_and_hold`; releasing that third press after the
//! triple-click threshold but before 10 s emits nothing (no accidental power off).
//! Times are CLOCK_MONOTONIC since boot: edges carry their kernel timestamp and a
//! timer fires only after a wait proved that no edge happened before it, so a
//! late thread cannot turn a release before 10 s into a reset.

use super::{send, HwEvent, Tx};
use std::time::Duration;

const HOLD: Duration = Duration::from_millis(2000);
const CLICK_AND_HOLD: Duration = Duration::from_millis(2000);
const DOUBLE_CLICK: Duration = Duration::from_millis(150);
const TRIPLE_CLICK: Duration = Duration::from_millis(150);
const DOUBLE_CLICK_AND_HOLD: Duration = Duration::from_secs(10);

#[derive(Default)]
pub struct Fsm {
    /// 0 idle, 1 first press, 2 released, 3 second press, 4 released,
    /// 5 third press (triple click on release), 6 third press held (release ignored).
    seq: u8,
    down: bool,
    timer: Option<(Duration, &'static str)>,
}

/// CLOCK_MONOTONIC, the clock of the GPIO edge timestamps.
pub(crate) fn monotonic() -> Duration {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // SAFETY: valid out pointer; CLOCK_MONOTONIC always exists on Linux.
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    Duration::new(ts.tv_sec as u64, ts.tv_nsec as u32)
}

impl Fsm {
    /// `now`: the edge's kernel timestamp.
    pub fn edge(&mut self, down: bool, now: Duration) -> Vec<&'static str> {
        let mut out = Vec::new();
        // Timers due before this edge happened fire first (5 -> 6, then 10 s),
        // or a long third press read late would still end as triple_click.
        while self.deadline().is_some_and(|d| now >= d) {
            out.extend(self.timeout(now));
        }
        self.timer = None;
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
                6 => self.seq = 0,
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
                    self.timer = Some((now + TRIPLE_CLICK, "double_click_and_hold"));
                }
                _ => {}
            }
        }
        out
    }

    pub fn deadline(&self) -> Option<Duration> {
        self.timer.map(|(t, _)| t)
    }

    /// `now`: a time up to which no edge happened.
    pub fn timeout(&mut self, now: Duration) -> Option<&'static str> {
        match self.timer {
            Some((t, ev)) if now >= t && self.seq == 5 => {
                // Past the triple-click threshold: wait for the full hold from the press.
                self.seq = 6;
                self.timer = Some((t - TRIPLE_CLICK + DOUBLE_CLICK_AND_HOLD, ev));
                None
            }
            Some((t, ev)) if now >= t => {
                self.timer = None;
                self.seq = 0;
                Some(ev)
            }
            _ => None,
        }
    }
}

pub fn spawn(chip: &str, line: u32, tx: Tx, presence: Option<crate::network::Presence>) -> bool {
    let req = gpiocdev::Request::builder()
        .on_chip(chip)
        .with_consumer("nab-hardware")
        .with_line(line)
        .as_input()
        .with_edge_detection(gpiocdev::line::EdgeDetection::BothEdges)
        .with_debounce_period(Duration::from_millis(10))
        .with_event_clock(gpiocdev::line::EventClock::Monotonic)
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
            let start = monotonic();
            let wait = fsm
                .deadline()
                .map_or(Duration::from_secs(3600), |d| d.saturating_sub(start));
            match req.wait_edge_event(wait) {
                Ok(true) => match req.read_edge_event() {
                    // Button pulls the line low when pressed.
                    Ok(e) => {
                        let down = e.kind == gpiocdev::line::EdgeKind::Falling;
                        if down && !fsm.down {
                            if let Some(presence) = &presence {
                                presence.press(Duration::from_nanos(e.timestamp_ns));
                            }
                        }
                        for ev in fsm.edge(down, Duration::from_nanos(e.timestamp_ns)) {
                            send(
                                &tx,
                                HwEvent::Button(ev, (ev == "down").then_some(e.timestamp_ns)),
                            );
                        }
                    }
                    Err(e) => error!("button read: {e}"),
                },
                // No edge until start + wait (ppoll, nanosecond timeout): a pending
                // edge is always read before a timer that it precedes.
                Ok(false) => {
                    if let Some(ev) = fsm.timeout(start + wait) {
                        send(&tx, HwEvent::Button(ev, None));
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
        let t0 = Duration::ZERO;
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
        let two_clicks = [
            (0, Some(true)),
            (50, Some(false)),
            (100, Some(true)),
            (150, Some(false)),
            (200, Some(true)),
        ];
        let with = |tail: &[(u64, Option<bool>)]| {
            let mut s = two_clicks.to_vec();
            s.extend_from_slice(tail);
            run(&mut Fsm::default(), &s)
        };
        // third press held 10 s
        assert_eq!(
            with(&[
                (400, None),
                (10150, None),
                (10200, None),
                (10300, Some(false))
            ]),
            vec!["double_click_and_hold"]
        );
        // released after the threshold but before 10 s: nothing, not triple_click,
        // and the next click is a plain click
        assert_eq!(
            with(&[
                (400, None),
                (1200, Some(false)),
                (2000, Some(true)),
                (2050, Some(false)),
                (2300, None)
            ]),
            vec!["click"]
        );
        assert!(with(&[(400, None), (10150, Some(false)), (20000, None)]).is_empty());
        // released within 150 ms: still a triple click
        assert_eq!(with(&[(340, Some(false))]), vec!["triple_click"]);
    }

    /// Edges delivered late, with no timeout() call in between.
    #[test]
    fn late_edges() {
        let edges = |steps: &[(u64, bool)]| {
            let t0 = Duration::ZERO;
            let mut fsm = Fsm::default();
            let mut out = Vec::new();
            for (ms, d) in steps {
                out.extend(fsm.edge(*d, t0 + Duration::from_millis(*ms)));
            }
            out.retain(|e| *e != "up" && *e != "down");
            (out, fsm.seq, fsm.deadline())
        };
        let two_clicks = [
            (0, true),
            (50, false),
            (100, true),
            (150, false),
            (200, true),
        ];
        let with = |release: u64| {
            let mut s = two_clicks.to_vec();
            s.push((release, false));
            edges(&s)
        };
        // third press released after 2 s: nothing, back to idle
        assert_eq!(with(2200), (vec![], 0, None));
        // third press released after 10 s: the reset gesture, before up
        assert_eq!(with(10300), (vec!["double_click_and_hold"], 0, None));
        // quick third release still counts
        assert_eq!(with(300), (vec!["triple_click"], 0, None));
        // a click whose timer expired before the next press stays a click
        assert_eq!(
            edges(&[(0, true), (50, false), (1000, true), (1050, false)]).0,
            vec!["click"]
        );
    }

    /// Replays what the thread sees when it stalls: third press at 0.2 s, release
    /// timestamped 9.2 s but read at 12 s.
    #[test]
    fn stalled_thread_uses_edge_timestamps() {
        let ms = Duration::from_millis;
        let presses = |fsm: &mut Fsm| {
            let mut out = Vec::new();
            for (t, d) in [
                (0, true),
                (50, false),
                (100, true),
                (150, false),
                (200, true),
            ] {
                out.extend(fsm.edge(d, ms(t)));
            }
            out
        };
        // Thread on time until 0.35 s (wait proved empty), then stalled to 12 s:
        // the next ppoll (wait 0) finds the queued release first.
        let mut fsm = Fsm::default();
        let mut out = presses(&mut fsm);
        out.extend(fsm.timeout(ms(350)));
        assert_eq!(fsm.deadline(), Some(ms(10200)));
        let wait = fsm.deadline().unwrap().saturating_sub(ms(12000));
        assert_eq!(wait, Duration::ZERO);
        out.extend(fsm.edge(false, ms(9200)));
        out.extend(fsm.timeout(ms(12000) + wait));
        out.retain(|e| *e != "up" && *e != "down");
        assert!(out.is_empty(), "{out:?}");
        assert_eq!((fsm.seq, fsm.deadline()), (0, None));

        // Thread stalled before even the 150 ms step.
        let mut fsm = Fsm::default();
        let mut out = presses(&mut fsm);
        out.extend(fsm.edge(false, ms(9200)));
        out.extend(fsm.timeout(ms(12000)));
        out.retain(|e| *e != "up" && *e != "down");
        assert!(out.is_empty(), "{out:?}");

        // A release really after 10 s, read at 12 s: one reset, before up.
        let mut fsm = Fsm::default();
        presses(&mut fsm);
        assert_eq!(
            fsm.edge(false, ms(10300)),
            vec!["double_click_and_hold", "up"]
        );
    }
}
