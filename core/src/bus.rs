//! Connection-bound hardware authority and typed D-Bus transport.
#![allow(clippy::too_many_arguments)] // Frozen wire signals have nine tag fields.
use crate::device::{bounded, Device};
use crate::hw::{self, Cancel, Hw, HwEvent, Status, Tech, WriteControl, WriteReq};
use crate::maintenance;
use futures_util::StreamExt;
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use tokio::sync::{watch, Mutex as AsyncMutex, Notify, OwnedMutexGuard};
use zbus::{fdo, message::Header, object_server::SignalEmitter, Connection};

pub const SERVICE: &str = "io.github.guilhem.NabHardware1";
pub const PATH: &str = "/io/github/guilhem/NabHardware1";
const WORK_TIMEOUT: Duration = Duration::from_secs(65);
const MAX_WRITES: usize = 64;
const RESULT_TTL: Duration = Duration::from_secs(120);

fn failed(reason: impl ToString) -> fdo::Error {
    fdo::Error::Failed(reason.to_string())
}
fn denied(reason: &str) -> fdo::Error {
    fdo::Error::AccessDenied(reason.into())
}
fn sender(header: &Header<'_>) -> fdo::Result<String> {
    header
        .sender()
        .map(ToString::to_string)
        .ok_or_else(|| denied("missing-sender"))
}

/// Authenticate only the Unix account supplied by the bus for a unique sender.
pub async fn authorize(bus: &Connection, sender: &str) -> fdo::Result<()> {
    authorize_user(bus, sender, "NABOS_APP_USER", "nab-app").await
}

pub async fn authorize_user(
    bus: &Connection,
    sender: &str,
    environment: &str,
    default_user: &str,
) -> fdo::Result<()> {
    let name =
        zbus::names::UniqueName::try_from(sender).map_err(|_| denied("unique-sender-required"))?;
    let expected = match std::env::var(environment) {
        Ok(user) => user,
        Err(std::env::VarError::NotPresent) => default_user.into(),
        Err(_) => return Err(denied("invalid-service-user")),
    };
    let uid = user_id(&expected)?;
    let dbus = fdo::DBusProxy::new(bus).await?;
    let actual = bounded(dbus.get_connection_unix_user(name.clone().into()))
        .await
        .map_err(denied_owned)?;
    if actual != uid {
        return Err(denied("untrusted-service-user"));
    }
    if !bounded(dbus.name_has_owner(name.into()))
        .await
        .map_err(denied_owned)?
    {
        return Err(denied("caller-disconnected"));
    }
    Ok(())
}

fn user_id(name: &str) -> fdo::Result<u32> {
    let name = std::ffi::CString::new(name).map_err(|_| denied("invalid-service-user"))?;
    let mut buffer = vec![0u8; 1024];
    loop {
        let mut entry = std::mem::MaybeUninit::<libc::passwd>::uninit();
        let mut result = std::ptr::null_mut();
        // The reentrant lookup writes only to our entry and buffer; neither is
        // read until success, and only the numeric UID escapes their lifetime.
        let error = unsafe {
            libc::getpwnam_r(
                name.as_ptr(),
                entry.as_mut_ptr(),
                buffer.as_mut_ptr().cast(),
                buffer.len(),
                &mut result,
            )
        };
        if error == libc::ERANGE && buffer.len() < 1024 * 1024 {
            buffer.resize(buffer.len() * 2, 0);
            continue;
        }
        if error != 0 || result.is_null() {
            return Err(denied("service-user-unavailable"));
        }
        return Ok(unsafe { entry.assume_init().pw_uid });
    }
}
fn denied_owned(reason: String) -> fdo::Error {
    denied(&reason)
}

struct Claim {
    owner: String,
    cancel: Cancel,
}
struct Write {
    owner: String,
    control: WriteControl,
    result: watch::Sender<Option<&'static str>>,
    active: bool,
    deadline: Instant,
    completed: Option<Instant>,
}
struct State {
    claim: Option<Claim>,
    maintenance: maintenance::State,
    draining: bool,
    drain_running: bool,
    next_write: u64,
    writes: BTreeMap<u64, Write>,
}
impl Default for State {
    fn default() -> Self {
        Self {
            claim: None,
            maintenance: maintenance::State::default(),
            draining: false,
            drain_running: false,
            next_write: 1,
            writes: BTreeMap::new(),
        }
    }
}

pub struct Hardware {
    pub hw: Arc<Hw>,
    state: Mutex<State>,
    serial: Arc<AsyncMutex<()>>,
    changed: Notify,
}
impl Hardware {
    pub fn new(hw: Arc<Hw>) -> Arc<Self> {
        Arc::new(Self {
            hw,
            state: Mutex::new(State::default()),
            serial: Arc::new(AsyncMutex::new(())),
            changed: Notify::new(),
        })
    }
    fn token(&self, owner: &str) -> fdo::Result<Cancel> {
        let s = self.state.lock().unwrap();
        let claim = s
            .claim
            .as_ref()
            .filter(|c| c.owner == owner)
            .ok_or_else(|| denied("claim-required"))?;
        if s.draining || s.maintenance.blocked() || claim.cancel.is_cancelled() {
            return Err(failed("hardware-quiescing-or-maintenance"));
        }
        Ok(claim.cancel.clone())
    }
    async fn admission(
        &self,
        bus: &Connection,
        owner: &str,
    ) -> fdo::Result<(OwnedMutexGuard<()>, Cancel)> {
        let token = self.token(owner)?;
        let guard = tokio::time::timeout(WORK_TIMEOUT, self.serial.clone().lock_owned())
            .await
            .map_err(|_| failed("hardware-busy"))?;
        authorize(bus, owner).await?;
        let current = self.token(owner)?;
        if !token.same(&current) {
            return Err(denied("stale-claim"));
        }
        Ok((guard, token))
    }
    async fn ear_admission(
        &self,
        bus: &Connection,
        owner: &str,
        index: Option<usize>,
    ) -> fdo::Result<(OwnedMutexGuard<()>, Cancel)> {
        let (guard, token) = self.admission(bus, owner).await?;
        work(async {
            match index {
                Some(i) => self.hw.ears.wait_one_idle(i).await,
                None => self.hw.ears.wait_idle().await,
            }
        })
        .await?;
        authorize(bus, owner).await?;
        if !token.same(&self.token(owner)?) {
            return Err(denied("stale-claim"));
        }
        Ok((guard, token))
    }
    pub fn fence(&self, remove_claim: bool) {
        {
            let mut s = self.state.lock().unwrap();
            if let Some(c) = &s.claim {
                c.cancel.cancel();
            }
            for w in s.writes.values() {
                if w.active {
                    w.control.cancel();
                }
            }
            if remove_claim {
                s.claim = None;
            }
            s.draining = true;
        }
    }
    pub fn invalidate(self: &Arc<Self>, remove_claim: bool) {
        self.fence(remove_claim);
        self.drain();
    }
    fn drain(self: &Arc<Self>) {
        {
            let mut s = self.state.lock().unwrap();
            if !s.draining || s.drain_running {
                return;
            }
            s.drain_running = true;
        }
        let hardware = self.clone();
        tokio::spawn(async move {
            let quiet = tokio::time::timeout(WORK_TIMEOUT, async {
                let _serial = hardware.serial.lock().await;
                // Attempt both waits even if one device fails. This is also
                // used by graceful shutdown after urgent motor stop.
                let (leds, ears) =
                    tokio::join!(hardware.hw.leds.clear(), hardware.hw.ears.wait_idle());
                leds?;
                ears?;
                loop {
                    let busy = hardware
                        .state
                        .lock()
                        .unwrap()
                        .writes
                        .values()
                        .any(|w| w.active || w.control.uncertain());
                    if !busy {
                        return Ok::<_, String>(());
                    }
                    tokio::time::sleep(Duration::from_millis(50)).await;
                }
            })
            .await;
            let mut s = hardware.state.lock().unwrap();
            s.drain_running = false;
            if matches!(quiet, Ok(Ok(()))) {
                s.draining = false;
                if !s.maintenance.blocked() {
                    if let Some(c) = &mut s.claim {
                        c.cancel = Cancel::default();
                    }
                }
            } else {
                warn!("hardware quiescence unproved; admission remains blocked");
            }
            hardware.changed.notify_waiters();
        });
    }
    async fn wait_quiet(&self) -> fdo::Result<()> {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let changed = self.changed.notified();
                if !self.state.lock().unwrap().draining {
                    return;
                }
                changed.await;
            }
        })
        .await
        .map_err(|_| failed("hardware-busy; quiescence not proven"))
    }
    pub async fn maintenance_request(
        self: &Arc<Self>,
        bus: &Connection,
        owner: String,
        operation: maintenance::Operation,
    ) -> fdo::Result<String> {
        maintenance::authorize(bus, &owner).await?;
        match operation {
            maintenance::Operation::Acquire(operation) => {
                let token = self
                    .state
                    .lock()
                    .unwrap()
                    .maintenance
                    .acquire(owner.clone(), operation)?;
                self.invalidate(false);
                self.wait_quiet().await?;
                maintenance::authorize(bus, &owner).await?;
                Ok(token)
            }
            maintenance::Operation::Abort(operation) => self
                .state
                .lock()
                .unwrap()
                .maintenance
                .abort(&owner, &operation),
            maintenance::Operation::Release(token) => self
                .state
                .lock()
                .unwrap()
                .maintenance
                .release(&owner, &token),
        }
    }
    pub async fn observe(self: &Arc<Self>, mut observation: maintenance::Observation) {
        if let Some(bus) = &observation.connection {
            if maintenance::authorize(bus, &observation.owner)
                .await
                .is_err()
            {
                observation.safe = false;
            }
        } else {
            observation.safe = false;
        }
        let (was_blocked, blocked) = {
            let mut s = self.state.lock().unwrap();
            let before = s.maintenance.blocked();
            s.maintenance.observe(observation);
            let after = s.maintenance.blocked();
            if before && !after && !s.draining {
                if let Some(c) = &mut s.claim {
                    c.cancel = Cancel::default();
                }
            }
            (before, after)
        };
        if blocked && !was_blocked {
            self.invalidate(false);
        }
    }
    fn write(
        &self,
        owner: &str,
        id: u64,
    ) -> fdo::Result<(WriteControl, watch::Receiver<Option<&'static str>>, Instant)> {
        let s = self.state.lock().unwrap();
        let w = s
            .writes
            .get(&id)
            .filter(|w| w.owner == owner)
            .ok_or_else(|| denied("unknown-write"))?;
        Ok((w.control.clone(), w.result.subscribe(), w.deadline))
    }
    fn cleanup(self: &Arc<Self>) {
        let mut s = self.state.lock().unwrap();
        s.writes.retain(|_, w| {
            w.active
                || w.control.uncertain()
                || w.completed.is_none_or(|t| t.elapsed() < RESULT_TTL)
        });
        drop(s);
        self.drain();
    }
}

struct Api(Arc<Hardware>);
#[zbus::interface(name = "io.github.guilhem.NabHardware1")]
impl Api {
    #[zbus(property)]
    fn ready(&self) -> bool {
        self.0.hw.ready()
    }
    #[zbus(property)]
    fn status(&self) -> Status {
        self.0.hw.status()
    }

    async fn claim(
        &self,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        let owner = sender(&header)?;
        authorize(bus, &owner).await?;
        {
            let mut s = self.0.state.lock().unwrap();
            if s.draining {
                return Err(failed("hardware-quiescing"));
            }
            if let Some(c) = &s.claim {
                if c.owner != owner {
                    return Err(denied("hardware-claimed"));
                }
            } else {
                s.claim = Some(Claim {
                    owner: owner.clone(),
                    cancel: Cancel::default(),
                });
            }
        }
        // Publish the claim before the last await: the disconnect monitor must
        // see it, including a caller that vanished during authentication.
        if let Err(e) = authorize(bus, &owner).await {
            if self
                .0
                .state
                .lock()
                .unwrap()
                .claim
                .as_ref()
                .is_some_and(|c| c.owner == owner)
            {
                self.0.invalidate(true);
            }
            return Err(e);
        }
        Ok(())
    }

    async fn release(
        &self,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        let owner = sender(&header)?;
        authorize(bus, &owner).await?;
        {
            let s = self.0.state.lock().unwrap();
            if !s.claim.as_ref().is_some_and(|c| c.owner == owner) {
                return Err(denied("claim-required"));
            }
        }
        self.0.invalidate(true);
        self.0.wait_quiet().await
    }
    async fn set_leds(
        &self,
        colors: Vec<(u8, u8, u8, u8)>,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        let mut seen = [false; 5];
        if colors.len() > 5
            || colors.iter().any(|(i, _, _, _)| {
                if *i >= 5 || seen[*i as usize] {
                    true
                } else {
                    seen[*i as usize] = true;
                    false
                }
            })
        {
            return Err(fdo::Error::InvalidArgs(
                "distinct LED indices 0..4 required".into(),
            ));
        }
        let (_guard, cancel) = self.0.admission(bus, &sender(&header)?).await?;
        work(
            self.0.hw.leds.set(
                colors
                    .into_iter()
                    .map(|(i, r, g, b)| (i as usize, [r, g, b]))
                    .collect(),
                cancel,
            ),
        )
        .await
    }
    async fn pulse_led(
        &self,
        index: u8,
        red: u8,
        green: u8,
        blue: u8,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        led(index)?;
        let (_guard, cancel) = self.0.admission(bus, &sender(&header)?).await?;
        work(
            self.0
                .hw
                .leds
                .pulse(index as usize, [red, green, blue], cancel),
        )
        .await
    }
    async fn move_ear(
        &self,
        index: u8,
        position: u8,
        backward: bool,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        ear(index)?;
        let (_guard, cancel) = self
            .0
            .ear_admission(bus, &sender(&header)?, Some(index as usize))
            .await?;
        work(
            self.0
                .hw
                .ears
                .go(index as usize, position, backward, cancel),
        )
        .await
    }
    async fn step_ear(
        &self,
        index: u8,
        steps: u8,
        backward: bool,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        ear(index)?;
        let (_guard, cancel) = self
            .0
            .ear_admission(bus, &sender(&header)?, Some(index as usize))
            .await?;
        work(self.0.hw.ears.step(index as usize, steps, backward, cancel)).await
    }
    #[zbus(out_args("left", "right"))]
    async fn read_ears(
        &self,
        detect: bool,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<(i16, i16)> {
        let (_guard, cancel) = if detect {
            self.0.ear_admission(bus, &sender(&header)?, None).await?
        } else {
            (self.0.serial.clone().lock_owned().await, Cancel::default())
        };
        let (left, right) =
            tokio::time::timeout(WORK_TIMEOUT, self.0.hw.ears.positions(detect, cancel))
                .await
                .map_err(|_| failed("ears-timeout"))?;
        Ok((left.map_or(-1, i16::from), right.map_or(-1, i16::from)))
    }
    async fn wait_ears_idle(&self) -> fdo::Result<()> {
        work(async {
            let _guard = self.0.serial.lock().await;
            self.0.hw.ears.wait_idle().await
        })
        .await
    }
    #[allow(clippy::too_many_arguments)]
    async fn start_write(
        &self,
        tech: String,
        uid: Vec<u8>,
        picture: u8,
        app: u8,
        data: Vec<u8>,
        timeout: u32,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<u64> {
        let tech = match tech.as_str() {
            "st25tb" if uid.len() == 8 => Tech::St25tb,
            "iso14443a_t2t" if [4, 7, 10].contains(&uid.len()) => Tech::T2t,
            _ => {
                return Err(fdo::Error::InvalidArgs(
                    "unsupported technology or UID length".into(),
                ))
            }
        };
        if data.len() > 32 || !(1..=60).contains(&timeout) {
            return Err(fdo::Error::InvalidArgs(
                "data <=32 bytes; timeout 1..60 seconds".into(),
            ));
        }
        let owner = sender(&header)?;
        let (_guard, cancel) = self.0.admission(bus, &owner).await?;
        let control = WriteControl::default();
        let (result, _) = watch::channel(None);
        let deadline = Instant::now() + Duration::from_secs(timeout as u64);
        let id = {
            let mut s = self.0.state.lock().unwrap();
            if s.writes.len() >= MAX_WRITES
                || s.writes.values().any(|w| w.active || w.control.uncertain())
            {
                return Err(failed("write-busy-or-limit"));
            }
            let id = s.next_write;
            s.next_write = id
                .checked_add(1)
                .ok_or_else(|| failed("write-ID-exhausted"))?;
            s.writes.insert(
                id,
                Write {
                    owner,
                    control: control.clone(),
                    result,
                    active: true,
                    deadline,
                    completed: None,
                },
            );
            id
        };
        let (reply, rx) = tokio::sync::oneshot::channel();
        let hardware = self.0.clone();
        let req = WriteReq {
            tech,
            uid,
            payload: hw::encode_tag_data(picture, app, Some(&data)),
            deadline,
            cancel,
            control: control.clone(),
            reply,
        };
        let submitted = self.0.hw.rfid.as_ref().map(|r| r.start(req));
        tokio::spawn(async move {
            let outcome = match submitted {
                None => "no-reader",
                Some(Err(_)) => "write-failed",
                Some(Ok(())) => match tokio::time::timeout(
                    Duration::from_secs(timeout as u64) + WORK_TIMEOUT,
                    rx,
                )
                .await
                {
                    Ok(Ok(Ok(()))) => "completed",
                    Ok(Ok(Err(e))) if e == "canceled" => "canceled",
                    Ok(Ok(Err(e))) if e == "timeout" => "timeout",
                    Err(_) => {
                        control.cancel();
                        "timeout"
                    }
                    _ => "write-failed",
                },
            };
            let mut s = hardware.state.lock().unwrap();
            if let Some(w) = s.writes.get_mut(&id) {
                w.active = false;
                w.completed = Some(Instant::now());
                w.result.send_replace(Some(outcome));
            }
            hardware.changed.notify_waiters();
        });
        Ok(id)
    }
    async fn wait_write(&self, id: u64, #[zbus(header)] header: Header<'_>) -> fdo::Result<String> {
        let (_, mut result, deadline) = self.0.write(&sender(&header)?, id)?;
        let end = tokio::time::Instant::from_std(deadline + WORK_TIMEOUT);
        loop {
            if let Some(value) = *result.borrow() {
                return Ok(value.into());
            }
            if tokio::time::timeout_at(end, result.changed())
                .await
                .is_err()
            {
                return Ok("timeout".into());
            }
        }
    }
    async fn cancel_write(
        &self,
        id: u64,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        let owner = sender(&header)?;
        let (_guard, _) = self.0.admission(bus, &owner).await?;
        self.0.write(&owner, id)?.0.cancel();
        Ok(())
    }
    #[zbus(signal)]
    async fn changed(emitter: &SignalEmitter<'_>, status: &Status) -> zbus::Result<()>;
    #[zbus(signal)]
    async fn button(emitter: &SignalEmitter<'_>, gesture: &str, edge: u64) -> zbus::Result<()>;
    #[zbus(signal)]
    async fn ear_moved(emitter: &SignalEmitter<'_>, index: u8) -> zbus::Result<()>;
    #[zbus(signal)]
    #[allow(clippy::too_many_arguments)]
    async fn tag(
        emitter: &SignalEmitter<'_>,
        removed: bool,
        tech: &str,
        uid: &[u8],
        support: &str,
        locked: bool,
        formatted: bool,
        picture: u8,
        app: u8,
        data: &[u8],
    ) -> zbus::Result<()>;
}
fn led(i: u8) -> fdo::Result<()> {
    if i < 5 {
        Ok(())
    } else {
        Err(fdo::Error::InvalidArgs("LED index 0..4".into()))
    }
}
fn ear(i: u8) -> fdo::Result<()> {
    if i < 2 {
        Ok(())
    } else {
        Err(fdo::Error::InvalidArgs("ear index 0..1".into()))
    }
}
async fn work(call: impl std::future::Future<Output = Result<(), String>>) -> fdo::Result<()> {
    tokio::time::timeout(WORK_TIMEOUT, call)
        .await
        .map_err(|_| failed("hardware-timeout; quiescence not proven"))?
        .map_err(failed)
}

struct Simulation {
    presence: crate::network::Presence,
}
#[zbus::interface(name = "io.github.guilhem.NabHardware1.Simulation")]
impl Simulation {
    async fn button(
        &self,
        gesture: String,
        edge: u64,
        #[zbus(signal_emitter)] emitter: SignalEmitter<'_>,
    ) -> fdo::Result<()> {
        if ![
            "down",
            "up",
            "click",
            "double_click",
            "triple_click",
            "hold",
            "click_and_hold",
            "double_click_and_hold",
        ]
        .contains(&gesture.as_str())
            || (gesture != "down" && edge != 0)
        {
            return Err(fdo::Error::InvalidArgs("invalid gesture or edge".into()));
        }
        if gesture == "down" {
            self.presence.press(Duration::from_nanos(edge));
        }
        Api::button(&emitter, &gesture, edge).await.map_err(failed)
    }
    async fn ear_moved(
        &self,
        index: u8,
        #[zbus(signal_emitter)] emitter: SignalEmitter<'_>,
    ) -> fdo::Result<()> {
        ear(index)?;
        Api::ear_moved(&emitter, index).await.map_err(failed)
    }
    #[allow(clippy::too_many_arguments)]
    async fn tag(
        &self,
        removed: bool,
        tech: String,
        uid: Vec<u8>,
        support: String,
        locked: bool,
        formatted: bool,
        picture: u8,
        app: u8,
        data: Vec<u8>,
        #[zbus(signal_emitter)] emitter: SignalEmitter<'_>,
    ) -> fdo::Result<()> {
        if tech.len() > 64 || support.len() > 64 || uid.len() > 10 || data.len() > 32 {
            return Err(fdo::Error::InvalidArgs(
                "invalid simulated tag bounds".into(),
            ));
        }
        Api::tag(
            &emitter, removed, &tech, &uid, &support, locked, formatted, picture, app, &data,
        )
        .await
        .map_err(failed)
    }
}

pub async fn export(
    bus: &Connection,
    hardware: Arc<Hardware>,
    presence: crate::network::Presence,
) -> zbus::Result<()> {
    bus.object_server().at(PATH, Api(hardware.clone())).await?;
    if hardware.hw.info.simulated {
        bus.object_server()
            .at(PATH, Simulation { presence })
            .await?;
    }
    bus.object_server()
        .at(maintenance::PATH, maintenance::Agent(hardware))
        .await?;
    Ok(())
}

pub async fn run(
    device: Device,
    hardware: Arc<Hardware>,
    mut events: tokio::sync::mpsc::UnboundedReceiver<HwEvent>,
) -> Result<(), String> {
    let bus = device.connection().await?;
    let dbus = fdo::DBusProxy::new(&bus).await.map_err(|e| e.to_string())?;
    // Subscribe before publishing the service so an owner cannot disappear in a gap.
    let mut owners = dbus
        .receive_name_owner_changed()
        .await
        .map_err(|e| e.to_string())?;
    export(
        &bus,
        hardware.clone(),
        crate::network::start(device.clone()),
    )
    .await
    .map_err(|e| e.to_string())?;
    bus.request_name_with_flags(SERVICE, zbus::fdo::RequestNameFlags::DoNotQueue.into())
        .await
        .map_err(|e| e.to_string())?;
    let watchdog = crate::supervision::Watchdog::from_env(hardware.hw.info.simulated)?;
    if let Some(watchdog) = &watchdog {
        watchdog.ready()?;
    }
    hardware.hw.ears.start()?;
    maintenance::start(device, hardware.clone());
    let emitter = SignalEmitter::new(&bus, PATH).map_err(|e| e.to_string())?;
    let mut tick = tokio::time::interval(Duration::from_secs(1));
    let mut watchdog_tick = tokio::time::interval(
        watchdog
            .as_ref()
            .map_or(Duration::from_secs(1), |w| w.interval),
    );
    let mut status = hardware.hw.status();
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .map_err(|e| e.to_string())?;
    loop {
        tokio::select! {
            _=term.recv()=>break,
            _=tokio::signal::ctrl_c()=>break,
            _=watchdog_tick.tick(), if watchdog.is_some()=> {
                watchdog.as_ref().unwrap().ping(hardware.hw.ears.healthy())?;
            }
            owner=owners.next() => {
                let Some(owner)=owner else {hardware.invalidate(true);return Err("bus disconnected".into());};
                if let Ok(args)=owner.args() {
                    if args.new_owner().is_none() {
                        let lost=hardware.state.lock().unwrap().claim.as_ref().is_some_and(|c| c.owner==args.name().as_str());
                        if lost {hardware.invalidate(true);}
                    }
                    if args.name().as_str()==crate::device::SERVICE {
                        hardware.observe(maintenance::Observation {connection:None,owner:String::new(),safe:false}).await;
                    }
                }
            }
            Some(event)=events.recv()=> {
                let result=match event {
                    HwEvent::Button(gesture,edge)=>Api::button(&emitter,gesture,edge.unwrap_or(0)).await,
                    HwEvent::EarMoved(index)=>Api::ear_moved(&emitter,index as u8).await,
                    HwEvent::Tag(tag)=>Api::tag(&emitter,tag.removed,tag.tech,&tag.uid,tag.support,tag.locked,tag.picture.is_some() && tag.app.is_some(),tag.picture.unwrap_or(0),tag.app.unwrap_or(0),tag.data.as_deref().unwrap_or(&[])).await,
                };
                if let Err(e)=result {warn!("hardware event: {e}");}
            }
            _=tick.tick()=> {
                hardware.cleanup();
                let current=hardware.hw.status();
                if current!=status {
                    status=current;
                    let interface=bus.object_server().interface::<_,Api>(PATH).await.map_err(|e|e.to_string())?;
                    let api=interface.get().await;
                    api.status_changed(&emitter).await.map_err(|e|e.to_string())?;
                    api.ready_changed(&emitter).await.map_err(|e|e.to_string())?;
                    Api::changed(&emitter,&status).await.map_err(|e|e.to_string())?;
                }
            }
        }
    }
    // This flag bypasses serial admission and the motor command queues, so a
    // slow LED clear or indivisible NFC write cannot delay the motor stop.
    hardware.hw.ears.request_stop();
    hardware.invalidate(true);
    let quiet = hardware.wait_quiet();
    tokio::pin!(quiet);
    loop {
        tokio::select! {
            result=&mut quiet=>return result.map_err(|e| e.to_string()),
            _=watchdog_tick.tick(), if watchdog.is_some()=> {
                watchdog.as_ref().unwrap().ping(hardware.hw.ears.healthy())?;
            }
        }
    }
}

#[cfg(test)]
#[path = "client_tests.rs"]
mod tests;
