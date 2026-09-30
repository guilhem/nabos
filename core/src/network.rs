//! Autonomous Wi-Fi control; no MQTT, HTTP, settings file or secret output.
//! NetworkManager owns all persistent state. The controller serializes radio
//! operations while D-Bus getters and physical presence stay responsive.

mod nm;
#[cfg(test)]
mod tests;

use nm::{bounded, Nm};
use serde::Serialize;
use std::io::Read;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::{mpsc, oneshot};
use tokio::time::Instant;
use zbus::object_server::SignalEmitter;
use zbus::{fdo, Connection};

const SERVICE: &str = "org.nabaztag.Core";
const PATH: &str = "/org/nabaztag/Core/Network";
const INTERFACE: &str = "org.nabaztag.Core.Network1";
const HOTSPOT_UUID: &str = "4e61624f-5300-4000-8000-000000000001";
const HOTSPOT_ADDRESS: &str = "10.41.0.1";
const GRACE: Duration = Duration::from_secs(90);
const RETRY: Duration = Duration::from_secs(300);
const LEASE: Duration = Duration::from_secs(300);

#[derive(Clone, Serialize, PartialEq)]
struct Status {
    mode: &'static str,
    address: String,
    ssid: Vec<u8>,
    profile_uuid: String,
    attempt_id: u64,
    phase: &'static str,
    error: &'static str,
}

impl Status {
    fn reconnecting() -> Self {
        Self {
            mode: "reconnecting",
            address: String::new(),
            ssid: Vec::new(),
            profile_uuid: String::new(),
            attempt_id: 0,
            phase: "idle",
            error: "",
        }
    }
    fn unavailable() -> Self {
        Self {
            mode: "unavailable",
            ..Self::reconnecting()
        }
    }
}

#[derive(Clone, Serialize)]
struct Network {
    ssid: Vec<u8>,
    strength: u8,
    security: &'static str,
}
#[derive(Clone, Serialize)]
struct Profile {
    uuid: String,
    ssid: Vec<u8>,
}

struct Reservation {
    token: String,
    created: Duration,
    last_press: Duration,
    expires: Duration,
    authorized_until: Option<Duration>,
}

struct Shared {
    status: Status,
    networks: Vec<Network>,
    profiles: Vec<Profile>,
    reservation: Option<Reservation>,
    active: Option<(u64, Arc<AtomicBool>)>,
    committing: bool,
    next_attempt: u64,
}

impl Default for Shared {
    fn default() -> Self {
        Self {
            status: Status::unavailable(),
            networks: Vec::new(),
            profiles: Vec::new(),
            reservation: None,
            active: None,
            committing: false,
            next_attempt: 0,
        }
    }
}

impl Shared {
    fn reserved(&self, now: Duration) -> bool {
        self.reservation.as_ref().is_some_and(|r| now < r.expires)
    }

    fn authorized(&self, token: &str, now: Duration) -> bool {
        self.status.mode == "hotspot"
            && !token.is_empty()
            && self.reservation.as_ref().is_some_and(|r| {
                r.token == token
                    && now < r.expires
                    && r.authorized_until.is_some_and(|expires| now < expires)
            })
    }

    fn press(&mut self, edge: Duration, now: Duration) {
        if self.status.mode != "hotspot"
            || self.active.is_some()
            || edge > now
            || now.saturating_sub(edge) >= LEASE
        {
            return;
        }
        if let Some(r) = self.reservation.as_mut() {
            if edge > r.created
                && edge > r.last_press
                && edge < r.expires
                && now < r.expires
                && !r.authorized_until.is_some_and(|expires| now < expires)
            {
                r.authorized_until = Some(edge + LEASE);
                r.last_press = edge;
            }
        }
    }

    fn observe(&mut self, mut status: Status) {
        status.attempt_id = self.status.attempt_id;
        status.phase = self.status.phase;
        status.error = self.status.error;
        // Presence and reservation cannot survive a completed radio transition.
        if self.status.mode == "hotspot" && status.mode == "client" {
            self.reservation = None;
        }
        self.status = status;
    }

    fn finish_attempt(&mut self, id: u64, result: nm::Result<()>, now: Duration) {
        if !self
            .active
            .as_ref()
            .is_some_and(|(active, _)| *active == id)
        {
            return;
        }
        // Discard GPIO edges generated during the attempt, even if the reader
        // delivers them after failure or cancellation released the radio.
        if let Some(r) = self.reservation.as_mut() {
            r.last_press = now;
            r.authorized_until = None;
        }
        self.active = None;
        self.committing = false;
        match result {
            Ok(()) => {
                self.status.phase = "succeeded";
                self.status.error = "";
            }
            Err("cancelled") => {
                self.status.phase = "cancelled";
                self.status.error = "";
            }
            Err(error) => {
                self.status.phase = "failed";
                self.status.error = error;
            }
        }
    }
}

#[derive(Clone)]
pub struct Presence(Arc<Mutex<Shared>>);

impl Presence {
    /// Called directly by the GPIO reader, using the kernel CLOCK_MONOTONIC
    /// timestamp, before publishing the unrelated MQTT button event.
    pub fn press(&self, edge: Duration) {
        self.0
            .lock()
            .unwrap()
            .press(edge, crate::hw::button::monotonic());
    }
}

struct Request {
    ssid: Vec<u8>,
    security: String,
    password: String,
    uuid: String,
    setup: bool,
}

impl Request {
    fn validate(&self) -> fdo::Result<()> {
        if self.ssid.len() > 32 || self.password.len() > 64 || self.password.contains('\0') {
            return Err(fdo::Error::InvalidArgs("invalid-network".into()));
        }
        if !self.uuid.is_empty() {
            if valid_uuid(&self.uuid) && self.uuid != HOTSPOT_UUID {
                return Ok(());
            }
            return Err(fdo::Error::InvalidArgs("invalid-profile".into()));
        }
        if self.ssid.is_empty() {
            return Err(fdo::Error::InvalidArgs("invalid-network".into()));
        }
        let size = self.password.len();
        let valid = match self.security.as_str() {
            "open" => size == 0,
            "wpa-psk" => {
                (8..=63).contains(&size)
                    || (size == 64 && self.password.bytes().all(|b| b.is_ascii_hexdigit()))
            }
            "sae" => (1..=63).contains(&size),
            _ => false,
        };
        if valid {
            Ok(())
        } else {
            Err(fdo::Error::InvalidArgs("invalid-security".into()))
        }
    }
}

enum Command {
    Scan,
    Connect {
        id: u64,
        request: Request,
        cancelled: Arc<AtomicBool>,
    },
    Forget {
        uuid: String,
        reply: oneshot::Sender<nm::Result<()>>,
    },
}

struct Api {
    shared: Arc<Mutex<Shared>>,
    commands: mpsc::Sender<Command>,
}

#[zbus::interface(name = "org.nabaztag.Core.Network1")]
impl Api {
    #[zbus(property)]
    fn status(&self) -> String {
        json(&self.shared.lock().unwrap().status)
    }
    #[zbus(property)]
    fn networks(&self) -> String {
        json(&self.shared.lock().unwrap().networks)
    }
    #[zbus(property)]
    fn profiles(&self) -> String {
        json(&self.shared.lock().unwrap().profiles)
    }

    fn scan(&self) -> fdo::Result<()> {
        self.commands
            .try_send(Command::Scan)
            .map_err(|_| fdo::Error::Failed("network-busy".into()))
    }

    fn reserve(&self, token: &str) -> fdo::Result<String> {
        if token.len() > 64 {
            return Err(fdo::Error::InvalidArgs("invalid-reservation".into()));
        }
        let now = crate::hw::button::monotonic();
        let mut s = self.shared.lock().unwrap();
        if s.status.mode != "hotspot" || s.active.is_some() {
            return Err(fdo::Error::Failed("not-hotspot".into()));
        }
        if token.is_empty() {
            if s.reserved(now) {
                return Err(fdo::Error::Failed("network-busy".into()));
            }
            let token =
                random_token().map_err(|_| fdo::Error::Failed("random-unavailable".into()))?;
            s.reservation = Some(Reservation {
                token: token.clone(),
                created: now,
                last_press: now,
                expires: now + LEASE,
                authorized_until: None,
            });
            Ok(token)
        } else if let Some(r) = s
            .reservation
            .as_mut()
            .filter(|r| r.token == token && now < r.expires)
        {
            r.expires = now + LEASE;
            Ok(r.token.clone())
        } else {
            Err(fdo::Error::AccessDenied("invalid-reservation".into()))
        }
    }

    fn authorized(&self, token: &str) -> bool {
        self.shared
            .lock()
            .unwrap()
            .authorized(token, crate::hw::button::monotonic())
    }

    fn release(&self, token: &str) -> fdo::Result<()> {
        let mut s = self.shared.lock().unwrap();
        if s.reservation
            .as_ref()
            .is_some_and(|r| !token.is_empty() && r.token == token)
        {
            s.reservation = None;
            Ok(())
        } else {
            Err(fdo::Error::AccessDenied("invalid-reservation".into()))
        }
    }

    fn connect(
        &self,
        ssid: Vec<u8>,
        security: &str,
        password: &str,
        uuid: &str,
        token: &str,
    ) -> fdo::Result<u64> {
        let request = Request {
            ssid,
            security: security.into(),
            password: password.into(),
            uuid: uuid.into(),
            setup: !token.is_empty(),
        };
        request.validate()?;
        let mut s = self.shared.lock().unwrap();
        if s.active.is_some() {
            return Err(fdo::Error::Failed("network-busy".into()));
        }
        if request.setup && !s.authorized(token, crate::hw::button::monotonic()) {
            return Err(fdo::Error::AccessDenied(
                "physical-confirmation-required".into(),
            ));
        }
        if s.status.mode == "unavailable" {
            return Err(fdo::Error::Failed("nm-unavailable".into()));
        }
        s.next_attempt = s
            .next_attempt
            .checked_add(1)
            .ok_or_else(|| fdo::Error::Failed("attempt-overflow".into()))?;
        let id = s.next_attempt;
        let cancelled = Arc::new(AtomicBool::new(false));
        self.commands
            .try_send(Command::Connect {
                id,
                request,
                cancelled: cancelled.clone(),
            })
            .map_err(|_| fdo::Error::Failed("network-busy".into()))?;
        if !token.is_empty() {
            if let Some(r) = s.reservation.as_mut() {
                r.authorized_until = None;
            }
        }
        s.active = Some((id, cancelled));
        s.committing = false;
        s.status.attempt_id = id;
        s.status.phase = "connecting";
        s.status.error = "";
        Ok(id)
    }

    fn cancel(&self, attempt_id: u64) -> fdo::Result<()> {
        let s = self.shared.lock().unwrap();
        if let Some((id, cancelled)) = &s.active {
            if *id == attempt_id {
                if s.committing {
                    return Err(fdo::Error::Failed("attempt-committing".into()));
                }
                cancelled.store(true, Ordering::SeqCst);
                return Ok(());
            }
        }
        Err(fdo::Error::InvalidArgs("unknown-attempt".into()))
    }

    async fn forget(&self, uuid: &str) -> fdo::Result<()> {
        if !valid_uuid(uuid) || uuid == HOTSPOT_UUID {
            return Err(fdo::Error::InvalidArgs("invalid-profile".into()));
        }
        if self.shared.lock().unwrap().active.is_some() {
            return Err(fdo::Error::Failed("network-busy".into()));
        }
        let (reply, response) = oneshot::channel();
        self.commands
            .try_send(Command::Forget {
                uuid: uuid.into(),
                reply,
            })
            .map_err(|_| fdo::Error::Failed("network-busy".into()))?;
        tokio::time::timeout(Duration::from_secs(15), response)
            .await
            .map_err(|_| fdo::Error::Failed("nm-timeout".into()))?
            .map_err(|_| fdo::Error::Failed("nm-unavailable".into()))?
            .map_err(|error| fdo::Error::Failed(error.into()))
    }

    #[zbus(signal)]
    async fn changed(emitter: &SignalEmitter<'_>, status: &str) -> zbus::Result<()>;
}

fn json(value: &impl Serialize) -> String {
    serde_json::to_string(value).expect("network JSON")
}

fn random_bytes() -> std::io::Result<[u8; 32]> {
    let mut bytes = [0u8; 32];
    std::fs::File::open("/dev/urandom")?.read_exact(&mut bytes)?;
    Ok(bytes)
}

fn random_token() -> std::io::Result<String> {
    Ok(random_bytes()?.iter().map(|b| format!("{b:02x}")).collect())
}

fn new_uuid() -> nm::Result<String> {
    let mut b = random_bytes().map_err(|_| "random-unavailable")?;
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    Ok(format!("{:02x}{:02x}{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}-{:02x}{:02x}{:02x}{:02x}{:02x}{:02x}",
        b[0],b[1],b[2],b[3],b[4],b[5],b[6],b[7],b[8],b[9],b[10],b[11],b[12],b[13],b[14],b[15]))
}

fn valid_uuid(uuid: &str) -> bool {
    uuid.len() == 36
        && uuid.bytes().enumerate().all(|(i, b)| {
            if [8, 13, 18, 23].contains(&i) {
                b == b'-'
            } else {
                b.is_ascii_hexdigit()
            }
        })
}

/// Simulation exits here, before constructing a bus connection or claiming a
/// name. The network task retries bus errors without stopping hardware.
pub fn start(simulate: bool) -> Option<Presence> {
    if simulate {
        return None;
    }
    let shared = Arc::new(Mutex::new(Shared::default()));
    let presence = Presence(shared.clone());
    tokio::spawn(async move {
        loop {
            let result = serve_system(shared.clone()).await;
            {
                let mut s = shared.lock().unwrap();
                s.observe(Status::unavailable());
                if let Some((id, _)) = s.active.as_ref() {
                    let id = *id;
                    s.finish_attempt(id, Err("bus-unavailable"), crate::hw::button::monotonic());
                }
            }
            if result.is_err() {
                warn!("network bus unavailable; retrying");
            }
            tokio::time::sleep(Duration::from_secs(5)).await;
        }
    });
    Some(presence)
}

async fn serve_system(shared: Arc<Mutex<Shared>>) -> nm::Result<()> {
    let (commands, receiver) = mpsc::channel(8);
    let api = Api {
        shared: shared.clone(),
        commands,
    };
    let bus = bounded(
        zbus::connection::Builder::system()
            .map_err(|_| "bus-unavailable")?
            .serve_at(PATH, api)
            .map_err(|_| "bus-unavailable")?
            .name(SERVICE)
            .map_err(|_| "bus-unavailable")?
            .build(),
    )
    .await?;
    Controller::new(bus, shared, receiver).run().await
}

struct Controller {
    bus: Connection,
    shared: Arc<Mutex<Shared>>,
    commands: mpsc::Receiver<Command>,
    owner: String,
    grace: Option<Instant>,
    retry: Instant,
    last_emitted: String,
}

impl Controller {
    fn new(bus: Connection, shared: Arc<Mutex<Shared>>, commands: mpsc::Receiver<Command>) -> Self {
        Self {
            bus,
            shared,
            commands,
            owner: String::new(),
            grace: Some(Instant::now() + GRACE),
            retry: Instant::now() + RETRY,
            last_emitted: String::new(),
        }
    }

    async fn run(mut self) -> nm::Result<()> {
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                _ = tick.tick() => { self.reconcile().await; }
                command = self.commands.recv() => match command {
                    Some(Command::Connect { id, request, cancelled }) => {
                        self.emit().await?;
                        // Reply/progress reaches Go before the single radio switches.
                        tokio::time::sleep(Duration::from_millis(500)).await;
                        let result = self.attempt(&request, &cancelled).await;
                        let mut s = self.shared.lock().unwrap();
                        s.finish_attempt(id, result, crate::hw::button::monotonic());
                    }
                    Some(Command::Scan) => {
                        if let Ok(nm) = Nm::discover(&self.bus).await {
                            let _ = nm.scan().await;
                            self.refresh_networks(&nm).await;
                        }
                    }
                    Some(Command::Forget { uuid, reply }) => {
                        let result = self.forget(&uuid).await;
                        let _ = reply.send(result);
                    }
                    None => return Err("bus-unavailable"),
                }
            }
            self.emit().await?;
        }
    }

    async fn emit(&mut self) -> nm::Result<()> {
        let (status, networks, profiles) = {
            let s = self.shared.lock().unwrap();
            (json(&s.status), json(&s.networks), json(&s.profiles))
        };
        let all = format!("{status}{networks}{profiles}");
        if all == self.last_emitted {
            return Ok(());
        }
        let emitter = SignalEmitter::new(&self.bus, PATH).map_err(|_| "bus-unavailable")?;
        bounded(Api::changed(&emitter, &status)).await?;
        // State is controller-owned, so publish the standard property signal
        // explicitly as well as the versioned progress signal.
        let properties = nm::Dict::from([
            ("Status".into(), nm::string(&status)),
            ("Networks".into(), nm::string(&networks)),
            ("Profiles".into(), nm::string(&profiles)),
        ]);
        bounded(self.bus.emit_signal(
            None::<&str>,
            PATH,
            "org.freedesktop.DBus.Properties",
            "PropertiesChanged",
            &(INTERFACE, properties, Vec::<String>::new()),
        ))
        .await?;
        self.last_emitted = all;
        Ok(())
    }

    async fn refresh_networks(&self, nm: &Nm) {
        if let Ok(Ok(networks)) = tokio::time::timeout(Duration::from_secs(10), nm.networks()).await
        {
            let mut s = self.shared.lock().unwrap();
            if !networks.is_empty() || s.networks.is_empty() {
                s.networks = networks;
            }
        }
    }

    async fn reconcile(&mut self) {
        let result = tokio::time::timeout(Duration::from_secs(15), self.reconcile_inner()).await;
        if !matches!(result, Ok(Ok(()))) {
            self.shared.lock().unwrap().observe(Status::unavailable());
        }
    }

    async fn reconcile_inner(&mut self) -> nm::Result<()> {
        let nm = Nm::discover(&self.bus).await?;
        let restarted = nm.owner != self.owner;
        if restarted {
            self.owner = nm.owner.clone();
            self.grace = Some(Instant::now() + GRACE);
            self.retry = Instant::now() + RETRY;
            self.shared.lock().unwrap().reservation = None;
        }
        let checkpoints = nm.checkpoints().await?;
        let profiles = nm.profiles().await?;
        // An interrupted transaction remains protected by NM's own timer. Only
        // orphaned marked profiles created by this core are cleaned up, even
        // when preparation persisted them before NM lost its checkpoint.
        if checkpoints.is_empty() {
            for candidate in profiles.iter().filter(|p| p.candidate) {
                nm.discard_candidate(&candidate.path, &candidate.public.uuid)
                    .await?;
            }
        }
        self.shared.lock().unwrap().profiles = profiles
            .iter()
            .filter(|p| !p.candidate)
            .map(|p| p.public.clone())
            .collect();
        let snapshot = nm.snapshot().await?;
        let mode = snapshot.status.mode;
        let previous = self.shared.lock().unwrap().status.mode;
        self.shared.lock().unwrap().observe(snapshot.status);
        self.refresh_networks(&nm).await;
        if !checkpoints.is_empty() {
            return Ok(());
        }
        let now = Instant::now();
        if mode == "client" {
            self.grace = None;
            self.retry = now + RETRY;
        } else if mode == "hotspot" {
            self.grace = None;
            let reserved = self
                .shared
                .lock()
                .unwrap()
                .reserved(crate::hw::button::monotonic());
            if now >= self.retry && !reserved && self.shared.lock().unwrap().active.is_none() {
                self.retry = now + RETRY;
                let networks = self.shared.lock().unwrap().networks.clone();
                // Prefer a visible native profile; otherwise let the first known
                // one try (hidden networks do not appear in scan results).
                let known = networks
                    .iter()
                    .find_map(|n| {
                        profiles
                            .iter()
                            .find(|p| !p.candidate && p.public.ssid == n.ssid)
                    })
                    .or_else(|| profiles.iter().find(|p| !p.candidate));
                if let Some(known) = known {
                    // Make the transition visible before awaiting NM. A new
                    // reservation cannot be granted after the retry has begun.
                    self.shared.lock().unwrap().status.mode = "reconnecting";
                    let _ = nm.activate(&known.path).await;
                    self.grace = Some(now + GRACE);
                }
            }
        } else {
            if self.grace.is_none() || previous == "client" {
                self.grace = Some(now + GRACE);
            }
            if self.grace.is_some_and(|deadline| now >= deadline) {
                nm.hotspot().await?;
                self.grace = Some(now + GRACE);
                self.retry = now + RETRY;
            }
        }
        Ok(())
    }

    async fn forget(&mut self, uuid: &str) -> nm::Result<()> {
        if self.shared.lock().unwrap().active.is_some() {
            return Err("network-busy");
        }
        let nm = Nm::discover(&self.bus).await?;
        if !nm.checkpoints().await?.is_empty() {
            return Err("network-busy");
        }
        let profiles = nm.profiles().await?;
        let profile = profiles
            .iter()
            .find(|p| !p.candidate && p.public.uuid == uuid)
            .ok_or("unknown-profile")?;
        nm.delete(&profile.path).await?;
        self.shared
            .lock()
            .unwrap()
            .profiles
            .retain(|p| p.uuid != uuid);
        Ok(())
    }

    async fn attempt(&mut self, request: &Request, cancelled: &AtomicBool) -> nm::Result<()> {
        if cancelled.load(Ordering::SeqCst) {
            return Err("cancelled");
        }
        let nm = Nm::discover(&self.bus).await?;
        let before = nm.snapshot().await?;
        if request.setup && before.status.mode != "hotspot" {
            return Err("not-hotspot");
        }
        if !nm.checkpoints().await?.is_empty() {
            return Err("network-busy");
        }
        let existing = if request.uuid.is_empty() {
            None
        } else {
            let profiles = nm.profiles().await?;
            Some(
                profiles
                    .into_iter()
                    .find(|p| !p.candidate && p.public.uuid == request.uuid)
                    .ok_or("unknown-profile")?,
            )
        };
        if cancelled.load(Ordering::SeqCst) {
            return Err("cancelled");
        }
        let uuid = match &existing {
            Some(p) => p.public.uuid.clone(),
            None => new_uuid()?,
        };
        let mut settings = nm::wifi_settings(
            &uuid,
            &format!("{}{}", nm::CANDIDATE_PREFIX, uuid),
            &request.ssid,
            &request.security,
            &request.password,
        );
        let started = Instant::now();
        let checkpoint = nm.checkpoint().await?;
        let mut candidate = None;
        let mut promoting = false;
        let operation = async {
            let path = match existing {
                Some(ref saved) => saved.path.clone(),
                None => {
                    let path = nm.add(&settings, false).await?;
                    candidate = Some(path.clone());
                    path
                }
            };
            if cancelled.load(Ordering::SeqCst) {
                return Err("cancelled");
            }
            nm.activate(&path).await?;
            loop {
                if cancelled.load(Ordering::SeqCst) {
                    return Err("cancelled");
                }
                // Leave time for the bounded save and destroy calls before NM's
                // non-extendable 90-second rollback timer.
                if Instant::now() >= started + GRACE - Duration::from_secs(15) {
                    return Err("connection-timeout");
                }
                let snapshot = nm.snapshot().await?;
                let success =
                    snapshot.status.mode == "client" && snapshot.status.profile_uuid == uuid;
                self.shared.lock().unwrap().observe(snapshot.status);
                self.emit().await?;
                if snapshot.failed {
                    return Err(match snapshot.reason {
                        7..=11 => "wifi-authentication-failed",
                        5 | 6 | 15 | 16 | 17 => "no-address",
                        _ => "activation-failed",
                    });
                }
                if success {
                    if cancelled.load(Ordering::SeqCst) {
                        return Err("cancelled");
                    }
                    let version = if candidate.is_some() {
                        // Prepare durably, still marked and with autoconnect off.
                        // Native profiles are activated without any settings write.
                        nm.save(&path, &settings, 0).await?;
                        nm.version(&path).await?
                    } else {
                        0
                    };
                    if !nm.checkpoints().await?.contains(&checkpoint) {
                        return Err("checkpoint-expired");
                    }
                    // Re-read after persistence; a concurrent loss/foreign
                    // activation must not commit a previously good snapshot.
                    let current = nm.snapshot().await?;
                    if current.status.mode != "client" || current.status.profile_uuid != uuid {
                        return Err("connection-lost");
                    }
                    {
                        // Cancel uses the same mutex: every accepted cancellation
                        // precedes commit, including one during the last snapshot.
                        let mut s = self.shared.lock().unwrap();
                        if cancelled.load(Ordering::SeqCst) {
                            return Err("cancelled");
                        }
                        s.committing = true;
                    }
                    nm.destroy(&checkpoint).await?;
                    if candidate.is_some() {
                        let connection = settings.get_mut("connection").unwrap();
                        connection.insert("id".into(), nm::string(nm::COMMITTED_ID));
                        connection.insert("autoconnect".into(), true.into());
                        promoting = true;
                        nm.save(&path, &settings, version).await?;
                    }
                    self.shared.lock().unwrap().observe(current.status);
                    self.grace = None;
                    self.retry = Instant::now() + RETRY;
                    return Ok(());
                }
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
        };
        let mut result =
            tokio::time::timeout_at(started + GRACE - Duration::from_secs(2), operation)
                .await
                .unwrap_or(Err("connection-timeout"));
        if result.is_err() && promoting {
            // A lost promotion reply may have committed. Resolve by UUID with a
            // fresh owner; never send an old object's path to a replacement NM.
            let resolution = async {
                let current_nm = Nm::discover(&self.bus).await?;
                let profiles = current_nm.profiles().await?;
                if let Some(profile) = profiles.iter().find(|p| p.public.uuid == uuid) {
                    if !profile.candidate {
                        let saved: nm::Settings = current_nm
                            .call(profile.path.as_str(), nm::CONNECTION, "GetSettings", &())
                            .await?;
                        let autoconnect = saved
                            .get("connection")
                            .and_then(|s| s.get("autoconnect"))
                            .and_then(|v| bool::try_from(v).ok());
                        if nm::text(&saved, "connection", "id") != nm::COMMITTED_ID
                            || autoconnect != Some(true)
                            || current_nm
                                .property::<bool>(profile.path.as_str(), nm::CONNECTION, "Unsaved")
                                .await?
                        {
                            return Err("commit-unconfirmed");
                        }
                        let status = current_nm
                            .snapshot()
                            .await
                            .map(|s| s.status)
                            .unwrap_or_else(|_| Status::unavailable());
                        self.shared.lock().unwrap().observe(status);
                        return Ok(true);
                    }
                }
                if !current_nm.checkpoints().await?.is_empty() {
                    return Err("commit-unconfirmed");
                }
                if let Some(profile) = profiles.iter().find(|p| p.public.uuid == uuid) {
                    current_nm.discard_candidate(&profile.path, &uuid).await?;
                }
                // Destroy succeeded, so native rollback is no longer available.
                // Recover the prior radio mode without rewriting its profile.
                if before.status.mode == "hotspot" {
                    current_nm.hotspot().await?;
                } else if let Some(previous) = profiles
                    .iter()
                    .find(|p| !p.candidate && p.public.uuid == before.status.profile_uuid)
                {
                    current_nm.activate(&previous.path).await?;
                }
                let status = current_nm.snapshot().await?.status;
                self.shared.lock().unwrap().observe(status);
                Ok(false)
            }
            .await;
            match resolution {
                Ok(true) => result = Ok(()),
                Ok(false) => {}
                Err(_) => {
                    result = Err("commit-unconfirmed");
                    self.shared.lock().unwrap().observe(Status::unavailable());
                }
            }
            self.grace = Some(Instant::now() + GRACE);
            self.retry = Instant::now() + RETRY;
        } else if result.is_err() {
            // A failed rollback keeps the native timer armed. Never destroy a
            // checkpoint whose restoration was not confirmed.
            let restored = nm.rollback(&checkpoint).await.is_ok();
            if restored {
                let _ = nm.destroy(&checkpoint).await;
            }
            if let Some(path) = candidate {
                let _ = nm.discard_candidate(&path, &uuid).await;
            }
            // Client rollback starts asynchronous reactivation/DHCP. Let the
            // existing grace below finish that recovery before opening an AP.
            if restored
                && before.status.mode == "hotspot"
                && nm.checkpoints().await.is_ok_and(|c| c.is_empty())
            {
                if let Ok(snapshot) = nm.snapshot().await {
                    if snapshot.status.mode != "client" && snapshot.status.mode != "hotspot" {
                        let _ = nm.hotspot().await;
                    }
                }
            }
            if let Ok(snapshot) = nm.snapshot().await {
                self.shared.lock().unwrap().observe(snapshot.status);
            } else {
                self.shared.lock().unwrap().observe(Status::unavailable());
            }
            self.grace = Some(Instant::now() + GRACE);
        }
        if result.is_err() && cancelled.load(Ordering::SeqCst) {
            Err("cancelled")
        } else {
            result
        }
    }
}
