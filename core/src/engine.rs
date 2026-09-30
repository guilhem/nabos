//! Core state machine (idle / asleep / playing), command queue, infos,
//! deduplication and expiration over MQTT.

use crate::bus::{now, Bus};
use crate::hw::leds::{Rgb, BOTTOM, NOSE};
use crate::hw::{encode_tag_data, Cancel, CancelSource, Hw, HwEvent, TagEvent};
use crate::playback::{self, FUCHSIA, INFO_LOOP};
use crate::protocol::{app_name, Action, Animation, Command, Rejection};
use serde_json::{json, Value};
use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender};
use tokio::task::JoinHandle;

const MAX_SEEN: usize = 1024;
const MAX_INFOS: usize = 16;

pub enum Input {
    Cmd(Result<Command, Rejection>, bool),
    Settings(Value),
    Hw(HwEvent),
    JobDone(String, Outcome),
    EarsDetected(Option<u8>, Option<u8>),
    Shutdown,
}

pub enum Outcome {
    Ok(Option<Value>),
    Canceled,
    Error(&'static str),
}

#[derive(Clone, Copy, PartialEq, Debug)]
enum State {
    Idle,
    Asleep,
    Playing,
}

struct Queued {
    id: String,
    expires_at: u64,
    action: Action,
}

struct Running {
    id: String,
    cancelable: bool,
    cancel: CancelSource,
    feedback: Arc<AtomicBool>,
    handle: JoinHandle<()>,
}

pub struct Engine {
    hw: Arc<Hw>,
    bus: Bus,
    tx: UnboundedSender<Input>,
    state: State,
    queue: VecDeque<Queued>,
    job: Option<Running>,
    bg: Option<JoinHandle<()>>,
    belly: Option<Rgb>,
    infos: Vec<(String, Animation)>,
    indicator: Option<Animation>,
    ears: (u8, u8),
    network: String,
    seen: HashMap<String, u64>,
    ear_task: Option<JoinHandle<()>>,
}

fn hex(b: &[u8]) -> String {
    b.iter()
        .map(|x| format!("{x:02x}"))
        .collect::<Vec<_>>()
        .join(":")
}

fn tag_json(t: &TagEvent) -> Value {
    let mut v = json!({"event": if t.removed { "removed" } else { "detected" }, "tech": t.tech, "uid": hex(&t.uid)});
    if !t.removed {
        v["support"] = json!(t.support);
        v["locked"] = json!(t.locked);
        if let Some(p) = t.picture {
            v["picture"] = json!(p);
        }
        if let Some(app) = t.app.filter(|a| *a != 255) {
            v["app"] = json!(app_name(app));
            if let Some(d) = &t.data {
                let end = d.iter().position(|b| *b == 0xFF).unwrap_or(d.len());
                v["data"] = json!(String::from_utf8_lossy(&d[..end]));
            }
        }
    }
    v
}

async fn run_job(hw: Arc<Hw>, action: Action, cancel: Cancel) -> Outcome {
    match action {
        Action::Play { sequence, .. } => playback::play_sequence(hw, &sequence, &cancel).await,
        Action::Message {
            signature, body, ..
        } => playback::play_message(hw, signature.as_ref(), &body, &cancel).await,
        Action::Test(kind) => {
            return if playback::test(&hw, kind).await {
                Outcome::Ok(None)
            } else {
                Outcome::Error("test_failed")
            };
        }
        Action::RfidWrite {
            tech,
            uid,
            picture,
            app,
            data,
            timeout,
        } => {
            let Some(reader) = &hw.rfid else {
                return Outcome::Error("no_reader");
            };
            hw.leds.set(NOSE, [255, 0, 0]);
            let r = reader
                .write(
                    tech,
                    uid.clone(),
                    encode_tag_data(picture, app, data.as_deref()),
                    Duration::from_secs(timeout),
                )
                .await;
            hw.leds.set_all([0, 0, 0]);
            return match r {
                Ok(()) => Outcome::Ok(Some(json!({"uid": hex(&uid)}))),
                Err(e) if e == "timeout" => Outcome::Error("timeout"),
                Err(e) => {
                    warn!("RFID write failed: {e}");
                    Outcome::Error("write_failed")
                }
            };
        }
        _ => {}
    }
    if cancel.is_cancelled() {
        Outcome::Canceled
    } else {
        Outcome::Ok(None)
    }
}

impl Engine {
    pub fn new(hw: Arc<Hw>, bus: Bus, tx: UnboundedSender<Input>) -> Engine {
        Engine {
            hw,
            bus,
            tx,
            state: State::Idle,
            queue: VecDeque::new(),
            job: None,
            bg: None,
            belly: None,
            infos: Vec::new(),
            indicator: None,
            ears: (0, 0),
            network: "ok".into(),
            seen: HashMap::new(),
            ear_task: None,
        }
    }

    pub async fn run(mut self, mut rx: UnboundedReceiver<Input>) {
        playback::boot_leds(&self.hw);
        self.publish_state();
        self.start_idle(true, false);
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        loop {
            let input = tokio::select! {
                i = rx.recv() => i,
                _ = tick.tick() => {
                    self.sweep();
                    continue;
                }
            };
            match input {
                None | Some(Input::Shutdown) => break,
                Some(Input::Cmd(c, retained)) => self.command(c, retained),
                Some(Input::Settings(v)) => self.settings(v),
                Some(Input::Hw(ev)) => self.hardware(ev),
                Some(Input::JobDone(id, out)) => self.job_done(id, out),
                Some(Input::EarsDetected(l, r)) => {
                    self.ears = (l.unwrap_or(self.ears.0), r.unwrap_or(self.ears.1));
                    self.bus.event("ears", json!({"left": l, "right": r}));
                    self.publish_state();
                }
            }
        }
        info!("shutting down");
        self.stop_bg();
        if let Some(j) = self.job.take() {
            j.handle.abort();
        }
        self.hw.player.stop().await;
        self.hw.leds.set_all([0, 0, 0]);
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    fn publish_state(&self) {
        let state = match self.state {
            State::Idle => "idle",
            State::Asleep => "asleep",
            State::Playing => "playing",
        };
        self.bus.state(json!({
            "v": 1,
            "state": state,
            "playing": self.job.as_ref().map(|j| j.id.clone()),
            "ears": {"left": self.ears.0, "right": self.ears.1},
            "hardware": self.hw.describe(),
            "version": env!("CARGO_PKG_VERSION"),
        }));
    }

    fn ok(&self, id: &str, data: Option<Value>) {
        self.bus.result(Some(id), "ok", None, data);
    }

    fn fail(&self, id: &str, status: &str, error: &str) {
        self.bus.result(Some(id), status, Some(error), None);
    }

    fn sweep(&mut self) {
        let now = now();
        self.seen.retain(|_, exp| *exp >= now);
        let (expired, kept): (Vec<_>, Vec<_>) =
            self.queue.drain(..).partition(|q| q.expires_at < now);
        self.queue = kept.into();
        for q in expired {
            self.bus.result(Some(&q.id), "expired", None, None);
        }
    }

    fn remember(&mut self, id: &str, expires_at: u64) {
        if self.seen.len() >= MAX_SEEN {
            if let Some(oldest) = self
                .seen
                .iter()
                .min_by_key(|(_, e)| **e)
                .map(|(k, _)| k.clone())
            {
                self.seen.remove(&oldest);
            }
        }
        self.seen.insert(id.to_string(), expires_at);
    }

    fn command(&mut self, c: Result<Command, Rejection>, retained: bool) {
        if retained {
            self.bus.clear_retained_cmd();
        }
        let cmd = match c {
            Err((id, why)) => {
                self.bus.result(id.as_deref(), "rejected", Some(&why), None);
                return;
            }
            Ok(c) => c,
        };
        if retained {
            return self.fail(&cmd.id, "rejected", "retained");
        }
        let now = now();
        if self.seen.contains_key(&cmd.id) {
            return self.fail(&cmd.id, "duplicate", "already received");
        }
        if cmd.expires_at < now {
            return self.bus.result(Some(&cmd.id), "expired", None, None);
        }
        self.remember(&cmd.id, cmd.expires_at);
        let id = cmd.id.clone();
        match cmd.action {
            Action::Sleep if self.state == State::Asleep => self.ok(&id, None),
            Action::RfidWrite { .. } | Action::Test(_)
                if self.state == State::Asleep && self.job.is_none() =>
            {
                self.start_job(Queued {
                    id,
                    expires_at: cmd.expires_at,
                    action: cmd.action,
                });
            }
            Action::Play { .. }
            | Action::Message { .. }
            | Action::Sleep
            | Action::RfidWrite { .. }
            | Action::Test(_) => {
                self.queue.push_back(Queued {
                    id,
                    expires_at: cmd.expires_at,
                    action: cmd.action,
                });
                self.pump();
            }
            Action::Cancel { target } => match &self.job {
                Some(j) if target.as_ref().is_none_or(|t| *t == j.id) => {
                    // The owner naming the running id may stop a non-cancelable job.
                    if j.cancelable || target.is_some() {
                        j.cancel.cancel();
                        self.ok(&id, None);
                    } else {
                        self.fail(&id, "error", "not_cancelable");
                    }
                }
                _ => match target.and_then(|t| self.queue.iter().position(|q| q.id == t)) {
                    Some(pos) => {
                        let q = self.queue.remove(pos).unwrap();
                        self.bus.result(Some(&q.id), "canceled", None, None);
                        self.ok(&id, None);
                    }
                    None => self.fail(&id, "error", "not_playing"),
                },
            },
            Action::Info { info_id, animation } => {
                match (
                    animation,
                    self.infos.iter().position(|(k, _)| *k == info_id),
                ) {
                    (None, Some(pos)) => {
                        self.infos.remove(pos);
                    }
                    (None, None) => {}
                    (Some(a), Some(pos)) => self.infos[pos].1 = a,
                    (Some(_), None) if self.infos.len() >= MAX_INFOS => {
                        return self.fail(&id, "rejected", "too many infos")
                    }
                    (Some(a), None) => self.infos.push((info_id, a)),
                }
                self.ok(&id, None);
                self.refresh_bg();
            }
            Action::Indicator { animation } => {
                self.indicator = animation;
                self.ok(&id, None);
                self.refresh_bg();
            }
            Action::Ears { left, right } => {
                self.ears = (left.unwrap_or(self.ears.0), right.unwrap_or(self.ears.1));
                self.ok(&id, None);
                self.publish_state();
                if self.state == State::Idle && self.job.is_none() {
                    let (hw, (l, r)) = (self.hw.clone(), self.ears);
                    tokio::spawn(async move { hw.ears.move_to(l, r).await });
                }
            }
            Action::Wakeup => {
                self.ok(&id, None);
                if self.state == State::Asleep {
                    self.state = State::Idle;
                    self.publish_state();
                    if self.job.is_none() {
                        self.start_idle(true, false);
                    }
                    self.pump();
                }
            }
            Action::Gestalt => {
                let data = json!({"hardware": self.hw.describe(), "queue": self.queue.len(), "infos": self.infos.len()});
                self.ok(&id, Some(data));
            }
        }
    }

    fn settings(&mut self, v: Value) {
        if let Some(l) = v.get("locale").and_then(Value::as_str) {
            let b = l.as_bytes();
            if b.len() == 5
                && b[2] == b'_'
                && b[..2].iter().all(u8::is_ascii_lowercase)
                && b[3..].iter().all(u8::is_ascii_uppercase)
            {
                self.hw.res.set_locale(l);
            }
        }
        if let Some(n) = v.get("network").and_then(Value::as_str) {
            if ["ok", "lan", "offline"].contains(&n) && n != self.network {
                self.network = n.to_string();
                if self.state == State::Idle && self.job.is_none() {
                    self.start_idle(false, false);
                }
            }
        }
    }

    fn hardware(&mut self, ev: HwEvent) {
        match ev {
            HwEvent::Button(e, edge_monotonic_ns) => {
                if let Some(j) = self.job.as_ref().filter(|j| e == "click" && j.cancelable) {
                    j.feedback.store(true, Ordering::Relaxed);
                    j.cancel.cancel();
                    return;
                }
                let mut payload = json!({"event": e});
                if let Some(edge) = edge_monotonic_ns.filter(|_| e == "down") {
                    payload["edge_monotonic_ns"] = json!(edge.to_string());
                }
                self.bus.event("button", payload);
            }
            HwEvent::EarMoved(ear) => {
                let ear = if ear == 0 { "left" } else { "right" };
                self.bus.event("ear_moved", json!({"ear": ear}));
                if let Some(t) = self.ear_task.take() {
                    t.abort();
                }
                let (hw, tx) = (self.hw.clone(), self.tx.clone());
                self.ear_task = Some(tokio::spawn(async move {
                    // Let the user finish moving the ears.
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    let (l, r) = hw.ears.detect().await;
                    let _ = tx.send(Input::EarsDetected(l, r));
                }));
            }
            HwEvent::Tag(t) => {
                self.bus.event("rfid", tag_json(&t));
                if !t.removed && self.state == State::Idle && self.job.is_none() {
                    self.start_idle(false, true);
                }
            }
        }
    }

    fn job_done(&mut self, id: String, out: Outcome) {
        if self.job.as_ref().is_none_or(|j| j.id != id) {
            return;
        }
        self.job = None;
        match out {
            Outcome::Ok(data) => self.ok(&id, data),
            Outcome::Canceled => self.bus.result(Some(&id), "canceled", None, None),
            Outcome::Error(e) => self.fail(&id, "error", e),
        }
        if self.state == State::Asleep {
            self.publish_state();
            self.start_asleep(true);
        } else {
            self.pump();
        }
    }

    /// Start the next runnable queued item, or go back to idle.
    fn pump(&mut self) {
        if self.job.is_some() || self.state == State::Asleep {
            return;
        }
        let now = now();
        while let Some(q) = self.queue.pop_front() {
            if q.expires_at < now {
                self.bus.result(Some(&q.id), "expired", None, None);
                continue;
            }
            if matches!(q.action, Action::Sleep) {
                if self
                    .queue
                    .iter()
                    .any(|o| !matches!(o.action, Action::Sleep))
                {
                    self.queue.push_back(q);
                    continue;
                }
                self.ok(&q.id, None);
                for other in self.queue.drain(..) {
                    self.bus.result(Some(&other.id), "ok", None, None);
                }
                self.state = State::Asleep;
                self.publish_state();
                self.start_asleep(true);
                return;
            }
            self.start_job(q);
            return;
        }
        if self.state == State::Playing {
            self.state = State::Idle;
            self.publish_state();
            self.start_idle(true, false);
        }
    }

    fn start_job(&mut self, q: Queued) {
        self.stop_bg();
        self.belly = None;
        let cancelable = match &q.action {
            Action::Play { cancelable, .. } | Action::Message { cancelable, .. } => *cancelable,
            _ => false,
        };
        let (source, cancel) = CancelSource::new();
        let feedback = Arc::new(AtomicBool::new(false));
        let (hw, tx, fb, id) = (
            self.hw.clone(),
            self.tx.clone(),
            feedback.clone(),
            q.id.clone(),
        );
        let handle = tokio::spawn(async move {
            let out = run_job(hw.clone(), q.action, cancel).await;
            if fb.load(Ordering::Relaxed) {
                playback::abort_feedback(&hw).await;
            }
            let _ = tx.send(Input::JobDone(id, out));
        });
        self.job = Some(Running {
            id: q.id,
            cancelable,
            cancel: source,
            feedback,
            handle,
        });
        if self.state != State::Asleep {
            self.state = State::Playing;
        }
        self.publish_state();
    }

    fn stop_bg(&mut self) {
        if let Some(t) = self.bg.take() {
            t.abort();
            playback::clear_info(&self.hw);
        }
    }

    fn refresh_bg(&mut self) {
        if self.job.is_none() {
            match self.state {
                State::Idle => self.start_idle(false, false),
                State::Asleep => self.start_asleep(false),
                State::Playing => {}
            }
        }
    }

    fn start_idle(&mut self, transition: bool, rfid_feedback: bool) {
        self.stop_bg();
        let belly = match self.network.as_str() {
            "offline" => [255, 0, 0],
            "lan" => [255, 165, 0],
            _ => FUCHSIA,
        };
        let pulse = transition || rfid_feedback || self.belly != Some(belly);
        self.belly = Some(belly);
        let (hw, (l, r), infos, indicator) = (
            self.hw.clone(),
            self.ears,
            self.infos.clone(),
            self.indicator.clone(),
        );
        self.bg = Some(tokio::spawn(async move {
            if rfid_feedback {
                playback::rfid_feedback(hw.clone()).await;
            }
            if transition {
                playback::move_ears_with_leds(&hw, FUCHSIA, l, r).await;
            }
            if pulse {
                hw.leds.pulse(BOTTOM, belly);
            }
            animations(&hw, indicator, infos).await;
        }));
    }

    fn start_asleep(&mut self, setup: bool) {
        self.stop_bg();
        self.belly = None;
        let (hw, indicator) = (self.hw.clone(), self.indicator.clone());
        self.bg = Some(tokio::spawn(async move {
            if setup {
                playback::sleep_setup(&hw).await;
            }
            animations(&hw, indicator, Vec::new()).await;
        }));
    }
}

async fn animations(hw: &Hw, indicator: Option<Animation>, infos: Vec<(String, Animation)>) {
    if let Some(a) = indicator {
        playback::play_animation(hw, &a, None).await;
    }
    if infos.is_empty() {
        return std::future::pending().await;
    }
    loop {
        for (_, a) in &infos {
            playback::play_animation(hw, a, Some(INFO_LOOP)).await;
        }
    }
}
