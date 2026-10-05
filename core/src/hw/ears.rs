//! Userspace TagTagTag ears, using falling edges from the GPIO character device.
//! Geometry follows tagtagtag-ears.c at 5ad1f1398ec03b28f0abf4df1f6ba60a5c0cec35:
//! 17 holes, one long interval, and a zero offset of three holes. No debounce.
//! Only start() enables real motors; each worker owns its motor pair and encoder.

use super::{button::monotonic, send, Cancel, HwEvent, Tx};
use gpiocdev::line::{EdgeDetection, EdgeKind, EventClock, Value, Values};
use gpiocdev::Request;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;

pub const STEPS: u8 = 17;
const OFFZERO: i16 = 3;
const PINS: [(u32, [u32; 2]); 2] = [(24, [12, 11]), (23, [10, 9])];
const NO_EDGE: Duration = Duration::from_secs(4);
// Hardware tuning point: maximum acceptable delay between a kernel edge and
// stopping/counting it. Raising this permits more mechanical overshoot.
const MAX_EVENT_LATENESS: Duration = Duration::from_millis(50);
const POLL: Duration = Duration::from_millis(10);
const HEALTH_TIMEOUT: Duration = Duration::from_millis(500);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(1);
const SIM_INTERVAL: Duration = Duration::from_millis(2);
const SIM_GAP: Duration = Duration::from_millis(8);

#[derive(Clone, Copy, Debug)]
enum Command {
    Go(u8, bool),
    Step(u8, bool),
}

enum Req {
    Move(Command, Cancel, oneshot::Sender<Result<(), String>>),
    Query(bool, Cancel, oneshot::Sender<Option<u8>>),
    Idle(oneshot::Sender<Result<(), String>>),
}

#[derive(Debug)]
enum Phase {
    Off,
    Calibration {
        last: Option<Duration>,
        intervals: Box<[Duration; 17]>,
        count: usize,
    },
    Validate {
        forward: u8,
        last: Duration,
    },
    Idle,
    Detect {
        direction: i16,
        last: Option<Duration>,
        fronts: u8,
        holes: u8,
        target: Option<u8>,
    },
    Moving {
        direction: i16,
        remaining: u16,
    },
    Settling {
        direction: i16,
    },
    Correcting {
        direction: i16,
    },
    Broken,
}

struct Fsm {
    phase: Phase,
    pos: Option<u8>,
    boundary: Duration,
    seqno: u32,
    last_event: Option<Duration>,
    progress: Duration,
    deadline: Option<Duration>,
    corrected: bool,
}

fn wrap(pos: i16) -> u8 {
    pos.rem_euclid(i16::from(STEPS)) as u8
}

/// The remainder is travelled in the requested direction, then complete turns.
fn distance(pos: u8, target: u8, backward: bool) -> u16 {
    let delta = if backward {
        i16::from(pos) - i16::from(target % STEPS)
    } else {
        i16::from(target % STEPS) - i16::from(pos)
    };
    u16::from(wrap(delta)) + u16::from(target / STEPS) * u16::from(STEPS)
}

fn calibration(intervals: &[Duration; 17]) -> Result<(u8, Duration), String> {
    let gap_ix = intervals
        .iter()
        .enumerate()
        .max_by_key(|(_, d)| **d)
        .unwrap()
        .0;
    let gap = intervals[gap_ix];
    let normal = *intervals
        .iter()
        .enumerate()
        .filter(|(i, _)| *i != gap_ix)
        .map(|(_, d)| d)
        .max()
        .unwrap();
    if normal.is_zero() || gap < normal + normal / 2 {
        return Err("ear-gap-not-distinct".into());
    }
    Ok((
        wrap(i16::from(STEPS) - 1 - gap_ix as i16 - OFFZERO),
        (normal + gap) / 2,
    ))
}

impl Fsm {
    fn new(sim: bool) -> Self {
        Self {
            phase: if sim { Phase::Idle } else { Phase::Off },
            pos: sim.then_some(0),
            boundary: (SIM_INTERVAL + SIM_GAP) / 2,
            seqno: 0,
            last_event: None,
            progress: Duration::ZERO,
            deadline: None,
            corrected: false,
        }
    }

    fn busy(&self) -> bool {
        !matches!(self.phase, Phase::Idle | Phase::Off | Phase::Broken)
    }

    fn direction(&self) -> i16 {
        match self.phase {
            Phase::Calibration { .. } => 1,
            Phase::Validate { .. } => -1,
            Phase::Detect { direction, .. }
            | Phase::Moving { direction, .. }
            | Phase::Correcting { direction } => direction,
            _ => 0,
        }
    }

    fn status(&self) -> &'static str {
        match self.phase {
            Phase::Off => "off",
            Phase::Calibration { .. } | Phase::Validate { .. } => "initializing",
            Phase::Broken => "broken",
            _ => "ok",
        }
    }

    fn budget(&mut self, now: Duration, fronts: u32) {
        self.progress = now;
        self.deadline = Some(now + NO_EDGE * fronts);
        self.corrected = false;
    }

    fn start(&mut self, now: Duration) {
        if matches!(self.phase, Phase::Off) {
            // One sync front, 17 full intervals, one backward validation, one correction.
            self.budget(now, 20);
            self.phase = Phase::Calibration {
                last: None,
                intervals: Box::new([Duration::ZERO; 17]),
                count: 0,
            };
        }
    }

    fn fail(&mut self) {
        self.pos = None;
        self.phase = Phase::Broken;
        self.deadline = None;
    }

    fn stop(&mut self) {
        if self.busy() {
            self.pos = None;
        }
        self.phase = Phase::Off;
        self.deadline = None;
    }

    fn check_time(&self, now: Duration) -> Result<(), String> {
        if self.busy()
            && (now.saturating_sub(self.progress) >= NO_EDGE
                || self.deadline.is_some_and(|d| now >= d))
        {
            Err("ear-timeout".into())
        } else {
            Ok(())
        }
    }

    fn run(&mut self, direction: i16, remaining: u16) {
        self.phase = if remaining == 0 {
            Phase::Idle
        } else {
            Phase::Moving {
                direction,
                remaining,
            }
        };
        if remaining == 0 {
            self.deadline = None;
        }
    }

    fn command(&mut self, command: Command, now: Duration, high: bool) {
        match command {
            Command::Step(n, back) => {
                self.budget(now, u32::from(n) + 1);
                self.run(if back { -1 } else { 1 }, u16::from(n));
            }
            Command::Go(target, back) => {
                let direction = if back { -1 } else { 1 };
                if let Some(pos) = self.pos {
                    let n = distance(pos, target, back);
                    self.budget(now, u32::from(n) + 1);
                    self.run(direction, n);
                } else {
                    self.budget(now, 18 + 271 + 1);
                    self.phase = Phase::Detect {
                        direction,
                        last: (!high).then_some(now),
                        fronts: 0,
                        holes: 0,
                        target: Some(target),
                    };
                }
            }
        }
    }

    fn detect(&mut self, now: Duration, high: bool) {
        self.budget(now, 18 + 16 + 1);
        self.phase = Phase::Detect {
            direction: 1,
            last: (!high).then_some(now),
            fronts: 0,
            holes: 0,
            target: None,
        };
    }

    /// Returns true only when an idle ear first loses its known position.
    fn manual(&mut self, high: bool) -> bool {
        high && matches!(self.phase, Phase::Idle) && self.pos.take().is_some()
    }

    fn edge(&mut self, at: Duration, seqno: u32, now: Duration) -> Result<bool, String> {
        if seqno != self.seqno.wrapping_add(1)
            || at > now
            || now - at > MAX_EVENT_LATENESS
            || self.last_event.is_some_and(|last| at <= last)
        {
            return Err("ear-lost-or-stale-edge".into());
        }
        self.seqno = seqno;
        self.last_event = Some(at);
        self.check_time(now)?;
        if self.busy() {
            // Never let a late edge after four seconds reset the no-edge timer.
            if at < self.progress || at - self.progress >= NO_EDGE {
                return Err("ear-timeout".into());
            }
            self.progress = at;
        }
        match &mut self.phase {
            Phase::Calibration {
                last,
                intervals,
                count,
            } => {
                if let Some(previous) = *last {
                    intervals[*count] = at - previous;
                    *count += 1;
                    if *count == 17 {
                        let (forward, boundary) = calibration(intervals)?;
                        self.boundary = boundary;
                        self.phase = Phase::Validate { forward, last: at };
                        return Ok(false);
                    }
                }
                *last = Some(at);
            }
            Phase::Validate { forward, last } => {
                let gap = at - *last > self.boundary;
                if gap != (*forward == wrap(-OFFZERO)) {
                    return Err("ear-backward-validation".into());
                }
                self.pos = Some(wrap(i16::from(*forward) - 1));
                self.phase = Phase::Settling { direction: -1 };
            }
            Phase::Detect {
                direction,
                last,
                fronts,
                holes,
                target,
            } => {
                *fronts += 1;
                if let Some(previous) = *last {
                    *holes += 1;
                    if at - previous > self.boundary {
                        // In reverse the far side of the gap is one hole earlier.
                        let found = wrap(-OFFZERO - i16::from(*direction < 0));
                        self.pos = Some(found);
                        let (direction, remaining) = if let Some(target) = *target {
                            (*direction, distance(found, target, *direction < 0))
                        } else {
                            let original = wrap(i16::from(found) - i16::from(*holes));
                            // Read(true) returns to the physical position before detection.
                            let forward = distance(found, original, false);
                            if forward > 8 {
                                (-1, 17 - forward)
                            } else {
                                (1, forward)
                            }
                        };
                        if remaining == 0 {
                            self.phase = Phase::Settling { direction };
                        } else {
                            self.run(direction, remaining);
                        }
                        return Ok(false);
                    }
                } else if *direction < 0 {
                    *holes += 1;
                }
                *last = Some(at);
                if *fronts >= 18 {
                    return Err("ear-gap-not-found".into());
                }
            }
            Phase::Moving {
                direction,
                remaining,
            } => {
                self.pos = self.pos.map(|p| wrap(i16::from(p) + *direction));
                *remaining -= 1;
                if *remaining == 0 {
                    self.phase = Phase::Settling {
                        direction: *direction,
                    };
                }
            }
            Phase::Correcting { direction } => {
                // The first stop overshot the target hole; reversing finds that
                // same hole, so keep its position instead of counting it twice.
                self.phase = Phase::Settling {
                    direction: *direction,
                };
            }
            Phase::Idle => return Ok(self.pos.take().is_some()),
            _ => {}
        }
        Ok(false)
    }

    /// Called only after driving both outputs low and sampling the encoder.
    fn settle(&mut self, high: bool) -> Result<(), String> {
        if let Phase::Settling { direction } = self.phase {
            if high {
                if self.corrected {
                    return Err("ear-overshoot".into());
                }
                self.corrected = true;
                self.phase = Phase::Correcting {
                    direction: -direction,
                };
            } else {
                self.phase = Phase::Idle;
                self.deadline = None;
            }
        }
        Ok(())
    }
}

struct Gpio {
    motors: Request,
    encoder: Request,
    lines: [u32; 2],
    input: u32,
    direction: i16,
}

impl Drop for Gpio {
    fn drop(&mut self) {
        // Also stop before releasing the requests if the worker unwinds.
        let low = motor_values(&self.lines, 0);
        let result = self
            .motors
            .set_values(&low)
            .map_err(|e| e.to_string())
            .and_then(|_| verify(&self.motors, &low));
        if let Err(e) = result {
            error!("ear GPIO release: off unconfirmed: {e}");
        }
    }
}

fn outputs(chip: &str, lines: &[u32]) -> Result<Request, String> {
    Request::builder()
        .on_chip(chip)
        .with_consumer("nab-hardware-ears")
        .with_lines(lines)
        .as_output(Value::Inactive)
        .request()
        .map_err(|e| e.to_string())
}

fn motor_values(lines: &[u32; 2], direction: i16) -> Values {
    [
        (lines[0], Value::from(direction > 0)),
        (lines[1], Value::from(direction < 0)),
    ]
    .into_iter()
    .collect()
}

fn verify(req: &Request, expected: &Values) -> Result<(), String> {
    let mut actual: Values = expected.iter().map(|v| (v.offset, v.value)).collect();
    req.values(&mut actual).map_err(|e| e.to_string())?;
    if &actual != expected {
        Err("ear-motor-readback".into())
    } else {
        Ok(())
    }
}

/// Direct recovery helper: no service, D-Bus, NFC or LED initialization.
pub fn stop_all(chip: &str) -> Result<(), String> {
    let lines = [12, 11, 10, 9];
    let req = outputs(chip, &lines)?;
    let low = Values::from_offsets(&lines);
    req.set_values(&low).map_err(|e| e.to_string())?;
    verify(&req, &low)
}

enum Backend {
    Gpio(Gpio),
    Sim {
        physical: u8,
        direction: i16,
        next: Option<Duration>,
        seqno: u32,
    },
}

impl Backend {
    fn open(index: usize, sim: bool, chip: &str) -> Result<Self, String> {
        if sim {
            return Ok(Self::Sim {
                physical: 0,
                direction: 0,
                next: None,
                seqno: 0,
            });
        }
        let (input, lines) = PINS[index];
        let motors = outputs(chip, &lines)?;
        verify(&motors, &motor_values(&lines, 0))?;
        let encoder = Request::builder()
            .on_chip(chip)
            .with_consumer("nab-hardware-ears")
            .with_line(input)
            .as_input()
            .with_edge_detection(EdgeDetection::FallingEdge)
            .with_event_clock(EventClock::Monotonic)
            .request()
            .map_err(|e| e.to_string())?;
        Ok(Self::Gpio(Gpio {
            motors,
            encoder,
            lines,
            input,
            direction: 0,
        }))
    }

    fn drive(&mut self, direction: i16) -> Result<(), String> {
        match self {
            Self::Gpio(g) => {
                if direction != g.direction {
                    // A single ioctl updates the pair. Every change of direction
                    // passes through (0,0); never brake with (1,1).
                    g.motors
                        .set_values(&motor_values(&g.lines, 0))
                        .map_err(|e| e.to_string())?;
                    g.direction = 0;
                    if direction != 0 {
                        g.motors
                            .set_values(&motor_values(&g.lines, direction))
                            .map_err(|e| e.to_string())?;
                        g.direction = direction;
                    }
                }
            }
            Self::Sim {
                physical,
                direction: old,
                next,
                ..
            } => {
                if *old != direction {
                    *old = direction;
                    *next = (direction != 0).then(|| monotonic() + sim_delta(*physical, direction));
                }
            }
        }
        Ok(())
    }

    fn check(&self) -> Result<bool, String> {
        match self {
            Self::Gpio(g) => {
                verify(&g.motors, &motor_values(&g.lines, g.direction))?;
                g.encoder
                    .value(g.input)
                    .map(|v| v == Value::Active)
                    .map_err(|e| e.to_string())
            }
            Self::Sim { .. } => Ok(false),
        }
    }

    /// Unconditionally write low even after an earlier GPIO write failed.
    fn off(&mut self) -> Result<(), String> {
        match self {
            Self::Gpio(g) => {
                g.motors
                    .set_values(&motor_values(&g.lines, 0))
                    .map_err(|e| e.to_string())?;
                g.direction = 0;
                verify(&g.motors, &motor_values(&g.lines, 0))
            }
            Self::Sim {
                direction, next, ..
            } => {
                *direction = 0;
                *next = None;
                Ok(())
            }
        }
    }

    /// At most one event per turn: stop and deadlines cannot starve in a flood.
    fn poll(&mut self) -> Result<Option<(Duration, u32)>, String> {
        match self {
            Self::Gpio(g) => {
                if !g.encoder.wait_edge_event(POLL).map_err(|e| e.to_string())? {
                    return Ok(None);
                }
                // gpiocdev 0.7's single-event convenience method can consume
                // several events. Capacity one leaves every later edge queued.
                let e = g
                    .encoder
                    .new_edge_event_buffer(1)
                    .read_event()
                    .map_err(|e| e.to_string())?;
                if e.kind != EdgeKind::Falling || e.offset != g.input || e.line_seqno != e.seqno {
                    return Err("ear-invalid-edge".into());
                }
                Ok(Some((Duration::from_nanos(e.timestamp_ns), e.seqno)))
            }
            Self::Sim {
                physical,
                direction,
                next,
                seqno,
            } => {
                let now = monotonic();
                if let Some(at) = *next {
                    if now >= at {
                        *physical = wrap(i16::from(*physical) + *direction);
                        *seqno = seqno.wrapping_add(1);
                        *next = Some(at + sim_delta(*physical, *direction));
                        return Ok(Some((at, *seqno)));
                    }
                    std::thread::park_timeout(POLL.min(at - now));
                } else {
                    std::thread::park_timeout(POLL);
                }
                Ok(None)
            }
        }
    }
}

fn sim_delta(physical: u8, direction: i16) -> Duration {
    if (direction > 0 && physical == wrap(-OFFZERO - 1))
        || (direction < 0 && physical == wrap(-OFFZERO))
    {
        SIM_GAP
    } else {
        SIM_INTERVAL
    }
}

struct Shared {
    pos: Option<u8>,
    status: &'static str,
    checked: Instant,
    stopped: bool,
    off_error: Option<String>,
}

struct Control {
    start: AtomicBool,
    stop: AtomicBool,
}

pub struct Ear {
    tx: Option<mpsc::Sender<Req>>,
    shared: Arc<Mutex<Shared>>,
    control: Arc<Control>,
    thread: Option<std::thread::Thread>,
}

impl Ear {
    fn open(index: usize, sim: bool, chip: &str, events: Tx) -> Self {
        let shared = Arc::new(Mutex::new(Shared {
            pos: sim.then_some(0),
            status: if sim { "ok" } else { "off" },
            checked: Instant::now(),
            stopped: false,
            off_error: None,
        }));
        let control = Arc::new(Control {
            start: AtomicBool::new(false),
            stop: AtomicBool::new(false),
        });
        let mut ear = Self {
            tx: None,
            shared,
            control,
            thread: None,
        };
        let backend = match Backend::open(index, sim, chip) {
            Ok(backend) => backend,
            Err(e) => {
                warn!("ear {index} GPIO: {e}");
                ear.shared.lock().unwrap().status = "missing";
                return ear;
            }
        };
        let (tx, rx) = mpsc::channel();
        let sh = ear.shared.clone();
        let ctl = ear.control.clone();
        match std::thread::Builder::new()
            .name(format!("ear-{index}"))
            .spawn(move || worker(index, backend, Fsm::new(sim), rx, sh, ctl, events))
        {
            Ok(handle) => {
                ear.thread = Some(handle.thread().clone());
                ear.tx = Some(tx);
            }
            Err(e) => {
                warn!("ear {index} worker: {e}");
                ear.shared.lock().unwrap().status = "broken";
            }
        }
        ear
    }

    fn submit(&self, req: Req) -> Result<(), String> {
        if self.control.stop.load(Ordering::Acquire) {
            return Err("ear-stopped".into());
        }
        self.tx
            .as_ref()
            .ok_or("ear-unavailable")?
            .send(req)
            .map_err(|_| "ear-stopped".to_string())?;
        if let Some(thread) = &self.thread {
            thread.unpark();
        }
        Ok(())
    }

    async fn write(&self, command: Command, cancel: Cancel) -> Result<(), String> {
        let (reply, rx) = oneshot::channel();
        self.submit(Req::Move(command, cancel, reply))?;
        rx.await.map_err(|_| "ear-stopped")?
    }

    async fn idle(&self) -> Result<(), String> {
        let result = if self.control.stop.load(Ordering::Acquire) {
            Err("ear-stopped".into())
        } else {
            let (reply, rx) = oneshot::channel();
            match self.submit(Req::Idle(reply)) {
                Ok(()) => rx.await.unwrap_or_else(|_| Err("ear-stopped".into())),
                Err(e) => Err(e),
            }
        };
        if self.control.stop.load(Ordering::Acquire) {
            let deadline = Instant::now() + SHUTDOWN_TIMEOUT;
            loop {
                {
                    let s = self.shared.lock().unwrap();
                    if s.stopped {
                        return s.off_error.clone().map_or(Ok(()), Err);
                    }
                }
                if self.tx.is_none() {
                    return Err("ear-off-unconfirmed".into());
                }
                if Instant::now() >= deadline {
                    return Err("ear-shutdown-timeout".into());
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        }
        result
    }

    async fn query(&self, detect: bool, cancel: Cancel) -> Option<u8> {
        let (reply, rx) = oneshot::channel();
        self.submit(Req::Query(detect, cancel, reply)).ok()?;
        rx.await.ok().flatten()
    }

    fn stop(&self) {
        self.control.stop.store(true, Ordering::Release);
        if let Some(thread) = &self.thread {
            thread.unpark();
        }
    }
}

fn publish(shared: &Mutex<Shared>, fsm: &Fsm, checked: bool) {
    let mut s = shared.lock().unwrap();
    s.pos = fsm.pos;
    s.status = fsm.status();
    // Updated by the actual motor/encoder control loop, never another thread.
    if checked {
        s.checked = Instant::now();
    }
}

fn fault(
    index: usize,
    backend: &mut Backend,
    fsm: &mut Fsm,
    shared: &Mutex<Shared>,
    error: String,
) {
    let first_fault = !matches!(fsm.phase, Phase::Broken);
    fsm.fail();
    // Stop before logging: a blocked journal/stderr must not keep motors on.
    let off_error = backend.off().err();
    {
        let mut s = shared.lock().unwrap();
        s.pos = None;
        s.status = "broken";
        s.off_error = off_error.clone();
    }
    if first_fault {
        error!("ear {index}: {error}");
    }
    if let Some(e) = &off_error {
        error!("ear {index} emergency off: {e}");
    }
}

fn worker(
    index: usize,
    mut backend: Backend,
    mut fsm: Fsm,
    rx: mpsc::Receiver<Req>,
    shared: Arc<Mutex<Shared>>,
    control: Arc<Control>,
    events: Tx,
) {
    let mut query: Option<oneshot::Sender<Option<u8>>> = None;
    loop {
        if control.stop.load(Ordering::Acquire) {
            fsm.stop();
            let off_error = backend.off().err();
            let mut s = shared.lock().unwrap();
            s.pos = fsm.pos;
            s.status = if off_error.is_some() { "broken" } else { "off" };
            s.off_error = off_error;
            s.stopped = true;
            return;
        }
        if let Err(e) = fsm.check_time(monotonic()) {
            fault(index, &mut backend, &mut fsm, &shared, e);
        }
        // A fault may have failed its first emergency off write. Retry low,
        // never accept readback of the old running direction as healthy.
        if matches!(fsm.phase, Phase::Broken) {
            if let Some(reply) = query.take() {
                let _ = reply.send(None);
            }
            if let Err(e) = backend.off() {
                publish(&shared, &fsm, false);
                reject_one(&rx);
                shared.lock().unwrap().off_error = Some(e);
                std::thread::park_timeout(POLL);
                continue;
            }
            shared.lock().unwrap().off_error = None;
        }
        let high = match backend.check() {
            Ok(high) => high,
            Err(e) => {
                fault(index, &mut backend, &mut fsm, &shared, e);
                // Failed controls must not refresh worker health.
                publish(&shared, &fsm, false);
                if let Some(reply) = query.take() {
                    let _ = reply.send(None);
                }
                reject_one(&rx);
                std::thread::park_timeout(POLL);
                continue;
            }
        };
        if fsm.manual(high) {
            send(&events, HwEvent::EarMoved(index));
        }
        if control.start.load(Ordering::Acquire) && matches!(fsm.phase, Phase::Off) {
            fsm.start(monotonic());
            if control.stop.load(Ordering::Acquire) {
                continue;
            }
            if let Err(e) = backend.drive(fsm.direction()) {
                fault(index, &mut backend, &mut fsm, &shared, e);
            }
        }
        if !fsm.busy() {
            if let Some(reply) = query.take() {
                let _ = reply.send(fsm.pos);
            }
            // Do not admit a new command until pending idle/manual edges have
            // been processed; they cannot be replayed as the new movement.
            let pending = match &backend {
                Backend::Gpio(g) => g.encoder.has_edge_event().map_err(|e| e.to_string()),
                Backend::Sim { .. } => Ok(false),
            };
            match pending {
                Err(e) => fault(index, &mut backend, &mut fsm, &shared, e),
                Ok(false) => match rx.try_recv() {
                    Ok(req) => {
                        let available = matches!(fsm.phase, Phase::Idle);
                        match req {
                            Req::Move(command, cancel, reply) => {
                                let result = if reply.is_closed() || !cancel.admit() {
                                    Err("canceled".into())
                                } else if !available {
                                    Err("ear-unavailable".into())
                                } else {
                                    fsm.command(command, monotonic(), high);
                                    if control.stop.load(Ordering::Acquire) {
                                        fsm.stop();
                                        Err("ear-stopped".into())
                                    } else {
                                        backend.drive(fsm.direction()).inspect_err(|e| {
                                            fault(
                                                index,
                                                &mut backend,
                                                &mut fsm,
                                                &shared,
                                                e.clone(),
                                            );
                                        })
                                    }
                                };
                                let _ = reply.send(result);
                            }
                            Req::Query(detect, cancel, reply) => {
                                if reply.is_closed() || !cancel.admit() || !available {
                                    let _ = reply.send(None);
                                } else if detect && fsm.pos.is_none() {
                                    fsm.detect(monotonic(), high);
                                    if control.stop.load(Ordering::Acquire) {
                                        fsm.stop();
                                        let _ = reply.send(None);
                                    } else {
                                        if let Err(e) = backend.drive(fsm.direction()) {
                                            fault(index, &mut backend, &mut fsm, &shared, e);
                                        }
                                        query = Some(reply);
                                    }
                                } else {
                                    let _ = reply.send(fsm.pos);
                                }
                            }
                            Req::Idle(reply) => {
                                let _ = reply.send(if available {
                                    Ok(())
                                } else {
                                    Err("ear-unavailable".into())
                                });
                            }
                        }
                    }
                    Err(mpsc::TryRecvError::Disconnected) => {
                        control.stop.store(true, Ordering::Release);
                        continue;
                    }
                    Err(mpsc::TryRecvError::Empty) => {}
                },
                Ok(true) => {}
            }
        }
        publish(&shared, &fsm, true);
        match backend.poll() {
            Ok(Some((at, seqno))) => {
                // Recheck urgent stop even if poll never sleeps under a flood.
                if control.stop.load(Ordering::Acquire) {
                    continue;
                }
                match fsm.edge(at, seqno, monotonic()) {
                    Ok(moved) => {
                        if moved {
                            send(&events, HwEvent::EarMoved(index));
                        }
                        if control.stop.load(Ordering::Acquire) {
                            continue;
                        }
                        let result = backend.drive(fsm.direction()).and_then(|_| {
                            if matches!(fsm.phase, Phase::Settling { .. }) {
                                let high = backend.check()?;
                                fsm.settle(high)?;
                                backend.drive(fsm.direction())?;
                            }
                            Ok(())
                        });
                        if let Err(e) = result {
                            fault(index, &mut backend, &mut fsm, &shared, e);
                        }
                    }
                    Err(e) => fault(index, &mut backend, &mut fsm, &shared, e),
                }
            }
            Ok(None) => {}
            Err(e) => {
                fault(index, &mut backend, &mut fsm, &shared, e);
                // Avoid spinning on a permanently failed encoder request.
                std::thread::park_timeout(POLL);
            }
        }
        publish(&shared, &fsm, false);
    }
}

fn reject_one(rx: &mpsc::Receiver<Req>) {
    match rx.try_recv() {
        Ok(Req::Move(_, _, reply)) | Ok(Req::Idle(reply)) => {
            let _ = reply.send(Err("ear-broken".into()));
        }
        Ok(Req::Query(_, _, reply)) => {
            let _ = reply.send(None);
        }
        _ => {}
    }
}

pub struct Ears {
    ears: [Ear; 2],
}

impl Ears {
    pub fn open(sim: bool, chip: &str, events: Tx) -> Self {
        Self {
            ears: [
                Ear::open(0, sim, chip, events.clone()),
                Ear::open(1, sim, chip, events),
            ],
        }
    }

    /// Nonblocking and idempotent. Real calibration is performed by the workers.
    pub fn start(&self) -> Result<(), String> {
        for e in &self.ears {
            if e.tx.is_none()
                || e.control.stop.load(Ordering::Acquire)
                || matches!(e.shared.lock().unwrap().status, "missing" | "broken")
            {
                return Err("ear-unavailable".into());
            }
        }
        for e in &self.ears {
            e.control.start.store(true, Ordering::Release);
            if let Some(thread) = &e.thread {
                thread.unpark();
            }
        }
        Ok(())
    }

    pub fn healthy(&self) -> bool {
        self.ears.iter().all(|e| {
            let s = e.shared.lock().unwrap();
            s.off_error.is_none()
                && (s.stopped || (e.tx.is_some() && s.checked.elapsed() <= HEALTH_TIMEOUT))
        })
    }

    /// Urgent stop bypasses all queued requests and never waits for a worker.
    pub fn request_stop(&self) {
        for e in &self.ears {
            e.stop();
        }
    }

    pub async fn shutdown(&self) -> Result<(), String> {
        self.request_stop();
        let deadline = Instant::now() + SHUTDOWN_TIMEOUT;
        loop {
            let mut stopped = true;
            for e in &self.ears {
                if e.tx.is_none() {
                    return Err("ear-off-unconfirmed".into());
                }
                let s = e.shared.lock().unwrap();
                if s.stopped {
                    if let Some(error) = &s.off_error {
                        return Err(error.clone());
                    }
                }
                stopped &= s.stopped;
            }
            if stopped {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err("ear-shutdown-timeout".into());
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    pub async fn go(
        &self,
        ear: usize,
        pos: u8,
        backward: bool,
        cancel: Cancel,
    ) -> Result<(), String> {
        self.ears
            .get(ear)
            .ok_or("invalid-ear")?
            .write(Command::Go(pos, backward), cancel)
            .await
    }

    pub async fn step(
        &self,
        ear: usize,
        delta: u8,
        backward: bool,
        cancel: Cancel,
    ) -> Result<(), String> {
        self.ears
            .get(ear)
            .ok_or("invalid-ear")?
            .write(Command::Step(delta, backward), cancel)
            .await
    }

    pub async fn wait_one_idle(&self, ear: usize) -> Result<(), String> {
        self.ears.get(ear).ok_or("invalid-ear")?.idle().await
    }

    pub async fn wait_idle(&self) -> Result<(), String> {
        for e in &self.ears {
            e.idle().await?;
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
        let pos = |e: &Ear| e.shared.lock().unwrap().pos.map_or(-1, i16::from);
        (pos(&self.ears[0]), pos(&self.ears[1]))
    }

    pub fn broken(&self, ear: usize) -> bool {
        matches!(
            self.status(ear),
            "off" | "initializing" | "broken" | "missing"
        )
    }

    pub fn status(&self, ear: usize) -> &'static str {
        self.ears[ear].shared.lock().unwrap().status
    }
}

impl Drop for Ears {
    fn drop(&mut self) {
        self.request_stop();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn edge(fsm: &mut Fsm, at: Duration) {
        fsm.edge(at, fsm.seqno.wrapping_add(1), at).unwrap();
        fsm.settle(false).unwrap();
    }

    /// Deterministic mechanical model: one falling edge per hole, long gap
    /// between 13 and 14 in either direction. Uses the actual FSM's direction.
    fn finish(fsm: &mut Fsm, physical: &mut u8, now: &mut Duration) -> usize {
        let mut fronts = 0;
        while fsm.busy() {
            assert!(fronts < 320, "bounded operation did not finish");
            let direction = fsm.direction();
            assert_ne!(direction, 0);
            *now += sim_delta(*physical, direction);
            *physical = wrap(i16::from(*physical) + direction);
            edge(fsm, *now);
            fronts += 1;
        }
        fronts
    }

    #[test]
    fn calibration_all_gap_indices_and_backward_validation() {
        for gap in 0..17 {
            let mut fsm = Fsm::new(false);
            let mut now = Duration::from_secs(1);
            fsm.start(now);
            now += SIM_INTERVAL;
            edge(&mut fsm, now); // Sync edge, not one of the 17 intervals.
            for i in 0..17 {
                now += if i == gap { SIM_GAP } else { SIM_INTERVAL };
                edge(&mut fsm, now);
            }
            let forward = wrap(13 - gap as i16);
            assert!(matches!(fsm.phase, Phase::Validate { .. }));
            assert_eq!(fsm.direction(), -1);
            assert_eq!(fsm.boundary, Duration::from_millis(5));
            now += if forward == 14 { SIM_GAP } else { SIM_INTERVAL };
            edge(&mut fsm, now);
            assert_eq!(
                fsm.pos,
                Some(wrap(i16::from(forward) - 1)),
                "gap index {gap}"
            );
            assert!(matches!(fsm.phase, Phase::Idle));
        }
        let mut intervals = [Duration::from_millis(100); 17];
        intervals[1] = Duration::from_millis(150);
        assert!(calibration(&intervals).is_ok()); // Exactly 1.5*second maximum.
        intervals[1] = Duration::from_millis(149);
        assert!(calibration(&intervals).is_err());
        for (forward, delta) in [(14, SIM_INTERVAL), (13, SIM_GAP)] {
            let mut fsm = Fsm::new(false);
            fsm.phase = Phase::Validate {
                forward,
                last: Duration::ZERO,
            };
            fsm.boundary = Duration::from_millis(5);
            fsm.budget(Duration::ZERO, 2);
            assert!(fsm.edge(delta, 1, delta).is_err());
        }
    }

    #[test]
    fn manual_read_detects_and_restores_every_initial_position() {
        for original in 0..STEPS {
            let mut fsm = Fsm::new(true);
            assert!(fsm.manual(true));
            assert_eq!(fsm.pos, None);
            assert!(!fsm.manual(true)); // One manual-move notification.
            let mut now = Duration::from_secs(1);
            fsm.detect(now, false);
            let mut physical = original;
            finish(&mut fsm, &mut physical, &mut now);
            assert_eq!(physical, original);
            assert_eq!(fsm.pos, Some(original));
        }
        // A start between holes synchronizes on the next hole in forward motion.
        let mut fsm = Fsm::new(true);
        fsm.pos = None;
        let mut now = Duration::from_secs(1);
        fsm.detect(now, true);
        let mut physical = 7;
        finish(&mut fsm, &mut physical, &mut now);
        assert_eq!(physical, 8);
        assert_eq!(fsm.pos, Some(8));
    }

    #[test]
    fn absolute_extra_turns_and_steps_in_both_directions() {
        for backward in [false, true] {
            for initial in 0..STEPS {
                for target in [0, 16, 17, 20, 34, 254, 255] {
                    let mut fsm = Fsm::new(true);
                    fsm.pos = Some(initial);
                    let mut now = Duration::from_secs(1);
                    let mut physical = initial;
                    fsm.command(Command::Go(target, backward), now, false);
                    assert_eq!(
                        finish(&mut fsm, &mut physical, &mut now),
                        usize::from(distance(initial, target, backward))
                    );
                    assert_eq!(fsm.pos, Some(target % STEPS));
                    assert_eq!(physical, target % STEPS);
                    // Unknown position must detect in the chosen direction and
                    // still execute every requested additional rotation.
                    let mut fsm = Fsm::new(true);
                    fsm.pos = None;
                    let mut now = Duration::from_secs(1);
                    let mut physical = initial;
                    fsm.command(Command::Go(target, backward), now, false);
                    let fronts = finish(&mut fsm, &mut physical, &mut now);
                    assert!(fronts >= usize::from(target / STEPS * STEPS));
                    assert_eq!(fsm.pos, Some(target % STEPS));
                    assert_eq!(physical, target % STEPS);
                }
            }
            for n in [0, 1, 17, 255] {
                let mut fsm = Fsm::new(true);
                let mut now = Duration::from_secs(1);
                let mut physical = 0;
                fsm.command(Command::Step(n, backward), now, false);
                assert_eq!(finish(&mut fsm, &mut physical, &mut now), usize::from(n));
                assert_eq!(
                    physical,
                    wrap(if backward {
                        -i16::from(n)
                    } else {
                        i16::from(n)
                    })
                );
                assert_eq!(fsm.pos, Some(physical));
            }
        }
    }

    #[test]
    fn bounded_detection_no_edge_total_deadline_lost_and_stale_events() {
        let mut fsm = Fsm::new(true);
        fsm.pos = None;
        let start = Duration::from_secs(1);
        fsm.detect(start, true);
        for n in 1..18 {
            edge(&mut fsm, start + SIM_INTERVAL * n);
        }
        let at = start + SIM_INTERVAL * 18;
        assert!(fsm.edge(at, 18, at).is_err());

        let mut fsm = Fsm::new(true);
        fsm.command(Command::Step(255, false), start, false);
        assert!(fsm.check_time(start + NO_EDGE).is_err());
        let at = start + NO_EDGE;
        assert!(fsm.edge(at, 1, at).is_err());
        let mut fsm = Fsm::new(true);
        fsm.command(Command::Step(255, false), start, false);
        let at = start + Duration::from_secs(3);
        edge(&mut fsm, at);
        assert!(fsm.check_time(at + NO_EDGE).is_err()); // Last kernel edge, not processing time.
        fsm.deadline = Some(at + SIM_INTERVAL);
        assert!(fsm.check_time(at + SIM_INTERVAL).is_err());

        for (seqno, delay) in [
            (2, Duration::ZERO),
            (1, MAX_EVENT_LATENESS + Duration::from_nanos(1)),
        ] {
            let mut fsm = Fsm::new(true);
            fsm.command(Command::Step(2, false), start, false);
            assert!(fsm
                .edge(start + SIM_INTERVAL, seqno, start + SIM_INTERVAL + delay)
                .is_err());
            let mut backend = Backend::Sim {
                physical: 0,
                direction: 1,
                next: Some(start),
                seqno: 0,
            };
            let shared = Mutex::new(Shared {
                pos: Some(0),
                status: "ok",
                checked: Instant::now(),
                stopped: false,
                off_error: None,
            });
            fault(
                0,
                &mut backend,
                &mut fsm,
                &shared,
                "injected GPIO/edge failure".into(),
            );
            assert_eq!(shared.lock().unwrap().pos, None);
            assert_eq!(shared.lock().unwrap().status, "broken");
            assert!(matches!(fsm.phase, Phase::Broken));
            assert_eq!(fsm.pos, None);
            assert_eq!(fsm.direction(), 0);
            assert!(matches!(
                backend,
                Backend::Sim {
                    direction: 0,
                    next: None,
                    ..
                }
            ));
            fsm.start(start);
            assert!(matches!(fsm.phase, Phase::Broken)); // No replay/recalibration.
        }
        let mut fsm = Fsm::new(true);
        edge(&mut fsm, start);
        assert!(fsm.edge(start, 2, start).is_err()); // Non-increasing timestamp.
        assert!(Fsm::new(true).edge(start, 1, start - SIM_INTERVAL).is_err());
    }

    #[test]
    fn overshoot_corrects_once_in_opposite_direction_and_keeps_target() {
        for backward in [false, true] {
            let mut fsm = Fsm::new(true);
            let start = Duration::from_secs(1);
            fsm.command(Command::Step(1, backward), start, false);
            let at = start + SIM_INTERVAL;
            fsm.edge(at, 1, at).unwrap();
            assert_eq!(fsm.direction(), 0); // Stop before inversion.
            fsm.settle(true).unwrap();
            assert_eq!(fsm.direction(), if backward { 1 } else { -1 });
            let target = fsm.pos;
            let at = at + SIM_INTERVAL;
            fsm.edge(at, 2, at).unwrap();
            assert_eq!(fsm.pos, target);
            assert_eq!(fsm.direction(), 0);
            assert!(fsm.settle(true).is_err()); // Cannot correct indefinitely.
        }
    }

    #[test]
    fn no_motor_before_start_and_health_tracks_worker_checks() {
        let mut fsm = Fsm::new(false);
        assert_eq!(fsm.direction(), 0);
        assert_eq!(fsm.status(), "off");
        let at = Duration::from_secs(1);
        edge(&mut fsm, at);
        assert_eq!(fsm.direction(), 0);
        fsm.start(at + SIM_INTERVAL);
        assert_eq!(fsm.direction(), 1);
        assert_eq!(fsm.status(), "initializing");
        fsm.stop();
        assert_eq!(fsm.direction(), 0);
        assert_eq!(fsm.pos, None);

        let (tx, _rx) = mpsc::channel();
        let make = || Ear {
            tx: Some(tx.clone()),
            shared: Arc::new(Mutex::new(Shared {
                pos: Some(0),
                status: "ok",
                checked: Instant::now(),
                stopped: false,
                off_error: None,
            })),
            control: Arc::new(Control {
                start: AtomicBool::new(false),
                stop: AtomicBool::new(false),
            }),
            thread: None,
        };
        let ears = Ears {
            ears: [make(), make()],
        };
        assert!(ears.healthy());
        ears.ears[0].shared.lock().unwrap().checked = Instant::now() - HEALTH_TIMEOUT - POLL;
        assert!(!ears.healthy());
        ears.ears[0].shared.lock().unwrap().stopped = true;
        assert!(ears.healthy()); // Explicitly confirmed low is safe after exit.
        ears.ears[0].shared.lock().unwrap().off_error = Some("uncertain GPIO off".into());
        assert!(!ears.healthy());
    }

    #[tokio::test]
    async fn simulation_queue_cancellation_and_stop_while_moving() {
        let (tx, _) = tokio::sync::mpsc::unbounded_channel();
        let ears = Ears::open(true, "unused", tx);
        assert_eq!(ears.snapshot(), (0, 0)); // D-Bus tests do not call start().
        assert_eq!(
            ears.positions(false, Cancel::default()).await,
            (Some(0), Some(0))
        );
        ears.start().unwrap();
        ears.start().unwrap();
        assert!(ears.healthy());
        for dropped in [false, true] {
            ears.step(0, 50, false, Cancel::default()).await.unwrap();
            let cancel = Cancel::default();
            let (reply, rx) = oneshot::channel();
            ears.ears[0]
                .submit(Req::Move(Command::Go(2, false), cancel.clone(), reply))
                .unwrap();
            if dropped {
                drop(rx);
            } else {
                cancel.cancel();
                assert_eq!(rx.await.unwrap(), Err("canceled".into()));
            }
            ears.wait_idle().await.unwrap();
        }
        assert_eq!(ears.positions(false, Cancel::default()).await.0, Some(15));
        let cancel = Cancel::default();
        ears.step(0, 2, false, cancel.clone()).await.unwrap();
        cancel.cancel(); // Already admitted movement still finishes.
        ears.wait_idle().await.unwrap();
        assert_eq!(ears.positions(false, Cancel::default()).await.0, Some(0));
        ears.step(0, 255, false, Cancel::default()).await.unwrap();
        let (reply, rx) = oneshot::channel();
        ears.ears[0]
            .submit(Req::Move(Command::Go(7, false), Cancel::default(), reply))
            .unwrap();
        let (idle_reply, idle_rx) = oneshot::channel();
        ears.ears[0].submit(Req::Idle(idle_reply)).unwrap();
        ears.request_stop();
        let start = Instant::now();
        ears.wait_one_idle(0).await.unwrap();
        ears.wait_idle().await.unwrap();
        ears.shutdown().await.unwrap();
        assert!(start.elapsed() < SHUTDOWN_TIMEOUT);
        assert!(rx.await.is_err()); // Queued movement is never started.
        assert!(idle_rx.await.is_err()); // Worker exits instead of draining FIFO.
        assert_eq!(ears.status(0), "off");
        assert_eq!(ears.snapshot().0, -1);
        assert!(ears.healthy()); // Confirmed low is explicitly safe after exit.
        assert!(ears.start().is_err());
    }
}

#[cfg(test)]
mod worker_tests {
    use super::*;

    #[test]
    fn worker_requires_start_then_calibrates_and_stops_before_exit() {
        let shared = Arc::new(Mutex::new(Shared {
            pos: None,
            status: "off",
            checked: Instant::now(),
            stopped: false,
            off_error: None,
        }));
        let control = Arc::new(Control {
            start: AtomicBool::new(false),
            stop: AtomicBool::new(false),
        });
        let (tx, rx) = mpsc::channel();
        let (events, _) = tokio::sync::mpsc::unbounded_channel();
        let sh = shared.clone();
        let ctl = control.clone();
        let backend = Backend::Sim {
            physical: 0,
            direction: 0,
            next: None,
            seqno: 0,
        };
        let worker =
            std::thread::spawn(move || worker(0, backend, Fsm::new(false), rx, sh, ctl, events));
        let (reply, result) = oneshot::channel();
        tx.send(Req::Move(
            Command::Step(255, false),
            Cancel::default(),
            reply,
        ))
        .unwrap();
        assert_eq!(
            result.blocking_recv().unwrap(),
            Err("ear-unavailable".into())
        );
        assert_eq!(shared.lock().unwrap().pos, None);
        assert_eq!(shared.lock().unwrap().status, "off");
        control.start.store(true, Ordering::Release);
        worker.thread().unpark();
        let (reply, result) = oneshot::channel();
        tx.send(Req::Idle(reply)).unwrap();
        result.blocking_recv().unwrap().unwrap();
        assert_eq!(shared.lock().unwrap().pos, Some(0));
        control.stop.store(true, Ordering::Release);
        worker.thread().unpark();
        worker.join().unwrap();
        let s = shared.lock().unwrap();
        assert!(s.stopped);
        assert_eq!(s.off_error, None);
        assert_eq!(s.status, "off");
    }

    #[test]
    fn queued_detect_rechecks_cancel_after_unknown_position_movement() {
        let shared = Arc::new(Mutex::new(Shared {
            pos: None,
            status: "ok",
            checked: Instant::now(),
            stopped: false,
            off_error: None,
        }));
        let control = Arc::new(Control {
            start: AtomicBool::new(false),
            stop: AtomicBool::new(false),
        });
        let (tx, rx) = mpsc::channel();
        let (events, _) = tokio::sync::mpsc::unbounded_channel();
        let sh = shared.clone();
        let ctl = control.clone();
        let backend = Backend::Sim {
            physical: 5,
            direction: 0,
            next: None,
            seqno: 0,
        };
        let mut fsm = Fsm::new(true);
        fsm.pos = None;
        let worker = std::thread::spawn(move || worker(0, backend, fsm, rx, sh, ctl, events));
        let (reply, result) = oneshot::channel();
        tx.send(Req::Move(
            Command::Step(50, false),
            Cancel::default(),
            reply,
        ))
        .unwrap();
        result.blocking_recv().unwrap().unwrap();
        let cancel = Cancel::default();
        let (reply, result) = oneshot::channel();
        tx.send(Req::Query(true, cancel.clone(), reply)).unwrap();
        cancel.cancel();
        assert_eq!(result.blocking_recv().unwrap(), None);
        // Detection would resolve the unknown position: it was never admitted.
        assert_eq!(shared.lock().unwrap().pos, None);
        control.stop.store(true, Ordering::Release);
        worker.thread().unpark();
        worker.join().unwrap();
        assert!(shared.lock().unwrap().stopped);
    }

    #[test]
    fn motor_pair_values_are_forward_backward_or_low() {
        for (_, lines) in PINS {
            for direction in [-1, 0, 1] {
                let values = motor_values(&lines, direction);
                assert_eq!(values.get(lines[0]), Some(Value::from(direction > 0)));
                assert_eq!(values.get(lines[1]), Some(Value::from(direction < 0)));
                assert_eq!(values.len(), 2);
            }
        }
    }
}
