//! Private-bus client tests, without linking the daemon library.
//! Real-daemon checks: DEVICE_CORE_BIN=/absolute/path/device-core cargo test -- --ignored.

use crate::device::Device;
use crate::hw::player::{Player, Source};
use crate::hw::{Cancel, CancelSource, Hw};
use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::time::Duration;
use zbus::{Connection, Proxy};

struct Daemon(Child);

impl Drop for Daemon {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

async fn manager(device: &Device) -> Proxy<'static> {
    let connection = device.connection().await.unwrap();
    zbus::proxy::Builder::new(&connection)
        .destination(crate::device::SERVICE)
        .unwrap()
        .path(crate::device::ROOT)
        .unwrap()
        .interface("io.github.guilhem.DeviceCore1.Manager")
        .unwrap()
        .cache_properties(zbus::proxy::CacheProperties::No)
        .build()
        .await
        .unwrap()
}

async fn daemon(bus: &PrivateBus, milliseconds: u64, agents: bool) -> (Daemon, Proxy<'static>) {
    let binary = std::env::var_os("DEVICE_CORE_BIN")
        .expect("real-daemon tests require an explicit DEVICE_CORE_BIN executable path");
    let log = std::fs::File::create(bus.directory.join("device-core.log")).unwrap();
    let child = Command::new(binary)
        .arg("--simulate")
        .env_clear()
        .envs(
            std::env::vars_os()
                .filter(|(key, _)| !key.to_string_lossy().starts_with("DEVICE_CORE_")),
        )
        .env("DEVICE_CORE_BUS_ADDRESS", &bus.address)
        .env("DEVICE_CORE_DATA_DIR", bus.directory.join("data"))
        .env(
            "DEVICE_CORE_NETWORK_GUARD",
            bus.directory.join("network.guard"),
        )
        .env("DEVICE_CORE_AUDIO_ROOTS", &bus.directory)
        .env("DEVICE_CORE_SIM_AUDIO_MS", milliseconds.to_string())
        .env("DEVICE_CORE_DEFAULT_VOLUME", "100")
        .env("DEVICE_CORE_HTTP_ADDR", "")
        .env("DEVICE_CORE_PRESENCE_UNIT", "nab-core.service")
        .env(
            "DEVICE_CORE_MAINTENANCE_UNITS",
            if agents { "nab-core.service" } else { "" },
        )
        .env("DEVICE_CORE_LVA_UNIT", "")
        .env("DEVICE_CORE_UPDATE_REPO", "")
        .env("DEVICE_CORE_UPDATE_ASSET", "")
        .env(
            "DEVICE_CORE_TIMESYNC_FILE",
            bus.directory.join("synchronized"),
        )
        .env("DEVICE_CORE_TIMESYNC_CLOCK", bus.directory.join("clock"))
        .stdout(log.try_clone().unwrap())
        .stderr(log)
        .spawn()
        .expect("cannot launch DEVICE_CORE_BIN");
    let mut daemon = Daemon(child);
    let device = Device::on_bus(bus.address.clone());
    let manager = manager(&device).await;
    let ready = tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            assert!(
                daemon.0.try_wait().unwrap().is_none(),
                "device-core exited before readiness"
            );
            if manager.get_property::<bool>("Ready").await.unwrap_or(false)
                && !manager
                    .get_property::<bool>("Maintenance")
                    .await
                    .unwrap_or(true)
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    assert!(
        ready.is_ok(),
        "device-core startup log: {}",
        std::fs::read_to_string(bus.directory.join("device-core.log")).unwrap()
    );
    (daemon, device.proxy("Audio").await.unwrap())
}

async fn wait(audio: &Proxy<'_>, id: &str) -> String {
    tokio::time::timeout(Duration::from_secs(3), audio.call("Wait", &(id,)))
        .await
        .unwrap()
        .unwrap()
}

async fn status(audio: &Proxy<'_>) -> crate::hw::player::Status {
    audio.get_property("Status").await.unwrap()
}

fn configuration(bus: &PrivateBus) -> crate::Config {
    crate::Config {
        simulate: true,
        mqtt_host: "127.0.0.1".into(),
        mqtt_port: 0,
        sounds_dirs: vec![bus.directory.clone()],
        chor_dirs: vec![bus.directory.clone()],
        gpio_chip: "/unused-in-simulation".into(),
        button_gpio: 17,
        ws2811_lib: "/unused-in-simulation".into(),
        led_brightness: 200,
        led_strip: "grb".into(),
    }
}

fn hardware(bus: &PrivateBus, device: Device) -> Arc<Hw> {
    let cfg = configuration(bus);
    let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
    Arc::new(Hw::open(&cfg, tx, None, Arc::new(Player::new(device))))
}

struct PrivateBus {
    child: Child,
    directory: PathBuf,
    address: String,
}

impl PrivateBus {
    fn new() -> Self {
        let directory = std::env::temp_dir().join(format!(
            "nab-core-clients-{}-{}",
            std::process::id(),
            fastrand::u64(..)
        ));
        std::fs::create_dir(&directory).unwrap();
        let address = format!("unix:path={}", directory.join("bus").display());
        let mut child = Self::spawn(&address);
        let mut ready = String::new();
        BufReader::new(child.stdout.take().unwrap())
            .read_line(&mut ready)
            .unwrap();
        assert!(ready.starts_with(&address));
        Self {
            child,
            directory,
            address,
        }
    }

    fn spawn(address: &str) -> Child {
        Command::new("dbus-daemon")
            .args([
                "--session",
                "--nofork",
                "--print-address=1",
                "--address",
                address,
            ])
            .stdout(Stdio::piped())
            .spawn()
            .expect("dbus-daemon is required for private-bus integration checks")
    }

    fn restart(&mut self) {
        self.child.kill().unwrap();
        self.child.wait().unwrap();
        let _ = std::fs::remove_file(self.directory.join("bus"));
        self.child = Self::spawn(&self.address);
        let mut ready = String::new();
        BufReader::new(self.child.stdout.take().unwrap())
            .read_line(&mut ready)
            .unwrap();
        assert!(ready.starts_with(&self.address));
    }
}

impl Drop for PrivateBus {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}

#[tokio::test]
async fn shared_connection_recovers_after_private_bus_restart() {
    let mut bus = PrivateBus::new();
    let device = Device::on_bus(bus.address.clone());
    let first = device.connection().await.unwrap();
    let same = device.clone().connection().await.unwrap();
    assert_eq!(first.unique_name(), same.unique_name());
    bus.restart();
    tokio::time::timeout(Duration::from_secs(2), async {
        while !first.is_closed() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    assert!(!device.connection().await.unwrap().is_closed());
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn real_audio_ids_cancellation_and_completion() {
    let bus = PrivateBus::new();
    let (_server, audio) = daemon(&bus, 200, false).await;
    let device = Device::on_bus(bus.address.clone());
    let player = Arc::new(Player::new(device.clone()));
    let source = bus.directory.join("sound.mp3");
    std::fs::write(&source, b"simulation media").unwrap();
    let first = player.start(Source::File(source.clone())).await.unwrap();
    let second = player.start(Source::File(source.clone())).await.unwrap();
    assert_ne!(first, second);
    assert_eq!(wait(&audio, &first).await, "preempted");
    player.stop(&first).await.unwrap();
    let status = player.status().await.unwrap();
    assert_eq!(
        (status.id.as_str(), status.state.as_str(), status.volume),
        (second.as_str(), "playing", 100)
    );
    assert!(player.wait(&second, &Cancel::never()).await.unwrap());
    assert_eq!(wait(&audio, &second).await, "completed");
    assert!(player
        .start(Source::File(bus.directory.join("missing.mp3")))
        .await
        .is_err());
    assert!(player.wait("unknown", &Cancel::never()).await.is_err());

    let id = player
        .start(Source::Stream(
            "http://127.0.0.1:23456/radio/0123456789abcdef".into(),
        ))
        .await
        .unwrap();
    let (cancel, token) = CancelSource::new();
    cancel.cancel();
    assert!(!player.wait(&id, &token).await.unwrap());
    assert_eq!(wait(&audio, &id).await, "stopped");
    assert_eq!(player.status().await.unwrap().state, "idle");
    assert_eq!(
        device.connection().await.unwrap().unique_name(),
        device.clone().connection().await.unwrap().unique_name()
    );
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn real_audio_owner_disconnect_and_daemon_restart_are_errors_then_recover() {
    let bus = PrivateBus::new();
    let (server, audio) = daemon(&bus, 10000, false).await;
    let device = Device::on_bus(bus.address.clone());
    let player = Arc::new(Player::new(device.clone()));
    let source = bus.directory.join("sound.mp3");
    std::fs::write(&source, b"simulation media").unwrap();
    let id = player.start(Source::File(source.clone())).await.unwrap();
    device.connection().await.unwrap().close().await.unwrap();
    assert_eq!(wait(&audio, &id).await, "owner-lost");
    assert!(player
        .wait(&id, &Cancel::never())
        .await
        .unwrap_err()
        .contains("owner-lost"));
    let active = player.start(Source::File(source.clone())).await.unwrap();
    let waiter = {
        let player = player.clone();
        let id = active.clone();
        tokio::spawn(async move { player.wait(&id, &Cancel::never()).await })
    };
    tokio::time::sleep(Duration::from_millis(20)).await;
    drop(server);
    assert!(tokio::time::timeout(Duration::from_secs(2), waiter)
        .await
        .unwrap()
        .unwrap()
        .is_err());
    assert!(player.start(Source::File(source.clone())).await.is_err());
    let (_replacement, replacement) = daemon(&bus, 100, false).await;
    let new = player.start(Source::File(source)).await.unwrap();
    assert_ne!(active, new);
    assert!(player.stop(&active).await.is_err());
    assert_eq!(status(&replacement).await.id, new);
    assert!(player.wait(&new, &Cancel::never()).await.unwrap());
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn abandoned_audio_start_is_reaped_without_stopping_a_newer_id() {
    use std::future::Future;
    use std::task::Poll;
    let bus = PrivateBus::new();
    let (_server, audio) = daemon(&bus, 200, false).await;
    let player = Arc::new(Player::new(Device::on_bus(bus.address.clone())));
    let source = bus.directory.join("sound.mp3");
    std::fs::write(&source, b"simulation media").unwrap();
    for replace in [false, true] {
        let mut start = Box::pin(player.start(Source::File(source.clone())));
        // Dispatch Start, then leave its reply unpolled to model task abortion.
        std::future::poll_fn(|cx| {
            assert!(start.as_mut().poll(cx).is_pending());
            Poll::Ready(())
        })
        .await;
        tokio::time::timeout(Duration::from_secs(2), async {
            while status(&audio).await.id.is_empty() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let abandoned = status(&audio).await.id;
        let replacement = if replace {
            Some(player.start(Source::File(source.clone())).await.unwrap())
        } else {
            None
        };
        drop(start);
        assert_eq!(
            wait(&audio, &abandoned).await,
            if replace { "preempted" } else { "stopped" }
        );
        if let Some(id) = replacement {
            assert_eq!(wait(&audio, &id).await, "completed");
        }
    }
}

struct FailingAudio;

#[zbus::interface(name = "io.github.guilhem.DeviceCore1.Audio")]
impl FailingAudio {
    fn start(&self, _kind: &str, _source: &str) -> zbus::fdo::Result<String> {
        Err(zbus::fdo::Error::Failed("test-audio-failure".into()))
    }
}

#[tokio::test]
async fn dbus_failures_reach_command_outcomes_including_choreography() {
    use crate::engine::{run_job, Outcome};
    use crate::protocol::{Action, Item};
    let bus = PrivateBus::new();
    let server = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .name(crate::device::SERVICE)
        .unwrap()
        .serve_at(format!("{}/Audio", crate::device::ROOT), FailingAudio)
        .unwrap()
        .build()
        .await
        .unwrap();
    let device = Device::on_bus(bus.address.clone());
    let hw = hardware(&bus, device);
    let play = || Action::Play {
        sequence: vec![Item {
            stream: Some("http://127.0.0.1:23456/radio/0123456789abcdef".into()),
            ..Item::default()
        }],
        cancelable: true,
    };
    assert!(matches!(
        run_job(hw.clone(), play(), Cancel::never()).await,
        Outcome::Error("audio_failed")
    ));
    std::fs::create_dir(bus.directory.join("choreographies")).unwrap();
    // The MIDI opcode picks any of its named files. Every pick must resolve.
    for name in crate::chor::MIDI_LIST {
        std::fs::write(bus.directory.join(name), b"simulation media").unwrap();
    }
    std::fs::write(bus.directory.join("midi.chor"), [0, 16, 0, 19]).unwrap();
    let choreography = Action::Play {
        sequence: vec![Item {
            choreography: Some("midi.chor".into()),
            ..Item::default()
        }],
        cancelable: true,
    };
    assert!(matches!(
        run_job(hw.clone(), choreography, Cancel::never()).await,
        Outcome::Error("audio_failed")
    ));
    server.close().await.unwrap();
    assert!(matches!(
        run_job(hw, play(), Cancel::never()).await,
        Outcome::Error("audio_failed")
    ));
}

struct SystemdManager;

#[zbus::interface(name = "org.freedesktop.systemd1.Manager")]
impl SystemdManager {
    #[zbus(name = "GetUnitByPIDFD")]
    fn get_unit_by_pidfd(
        &self,
        fd: zbus::zvariant::OwnedFd,
    ) -> (zbus::zvariant::OwnedObjectPath, String, Vec<u8>) {
        use std::os::fd::AsRawFd;
        let info =
            std::fs::read_to_string(format!("/proc/self/fdinfo/{}", fd.as_raw_fd())).unwrap();
        let pid: u32 = info
            .lines()
            .find_map(|line| line.strip_prefix("Pid:\t"))
            .unwrap()
            .parse()
            .unwrap();
        assert_eq!(pid, std::process::id());
        (
            "/org/freedesktop/systemd1/unit/nab_2dcore_2eservice"
                .try_into()
                .unwrap(),
            "nab-core.service".into(),
            vec![0; 16],
        )
    }
}

struct SystemdUnit;

#[zbus::interface(name = "org.freedesktop.systemd1.Unit")]
impl SystemdUnit {
    #[zbus(property)]
    fn id(&self) -> &str {
        "nab-core.service"
    }
}

async fn systemd(bus: &PrivateBus) -> Connection {
    zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .name("org.freedesktop.systemd1")
        .unwrap()
        .serve_at("/org/freedesktop/systemd1", SystemdManager)
        .unwrap()
        .serve_at(
            "/org/freedesktop/systemd1/unit/nab_2dcore_2eservice",
            SystemdUnit,
        )
        .unwrap()
        .build()
        .await
        .unwrap()
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn real_network_authorizes_only_fresh_original_gpio_presence() {
    let bus = PrivateBus::new();
    let _systemd = systemd(&bus).await;
    let (_server, _audio) = daemon(&bus, 20, false).await;
    let network = Device::on_bus(bus.address.clone())
        .proxy("Network")
        .await
        .unwrap();
    type NetworkStatus = (
        String,
        String,
        bool,
        String,
        Vec<u8>,
        String,
        u64,
        String,
        String,
    );
    tokio::time::timeout(Duration::from_secs(2), async {
        while network
            .get_property::<NetworkStatus>("Status")
            .await
            .unwrap()
            .0
            != "hotspot"
        {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    let stale = crate::hw::button::monotonic();
    let token: String = network.call("Reserve", &("",)).await.unwrap();
    let presence = crate::network::start(Device::on_bus(bus.address.clone()));
    presence.press(stale);
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(!network
        .call::<_, _, bool>("Authorized", &(token.as_str(),))
        .await
        .unwrap());
    presence.press(crate::hw::button::monotonic());
    tokio::time::timeout(Duration::from_secs(2), async {
        while !network
            .call::<_, _, bool>("Authorized", &(token.as_str(),))
            .await
            .unwrap()
        {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn real_manager_without_agents_opens_the_core_barrier() {
    use crate::{engine::Input, maintenance};
    let bus = PrivateBus::new();
    let (_server, _audio) = daemon(&bus, 20, false).await;
    let device = Device::on_bus(bus.address.clone());
    let capabilities: Vec<String> = manager(&device)
        .await
        .get_property("Capabilities")
        .await
        .unwrap();
    assert!(!capabilities.iter().any(|c| c == "maintenance-agents"));
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    maintenance::start(device, tx);
    let Input::MaintenanceObserved(observation) =
        tokio::time::timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap()
    else {
        panic!("unexpected maintenance input")
    };
    assert!(observation.connection.is_some());
    assert!(observation.safe);
    let mut state = maintenance::State::default();
    state.observe(observation);
    assert!(!state.blocked());
}

#[tokio::test]
#[ignore = "requires explicit DEVICE_CORE_BIN; run with --ignored"]
async fn real_coordinator_acquires_and_releases_core_on_simulated_power() {
    use crate::{engine::Input, maintenance};
    let bus = PrivateBus::new();
    let _systemd = systemd(&bus).await;
    let (_server, _audio) = daemon(&bus, 20, true).await;
    let device = Device::on_bus(bus.address.clone());
    let manager = manager(&device).await;
    let capabilities: Vec<String> = manager.get_property("Capabilities").await.unwrap();
    assert!(capabilities.iter().any(|c| c == "maintenance-agents"));
    let system = device.proxy("System").await.unwrap();
    assert!(
        system.call::<_, _, ()>("Reboot", &()).await.is_err(),
        "required core agent is not registered yet"
    );
    let hw = hardware(&bus, device.clone());
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let mqtt = crate::bus::Bus::start(&configuration(&bus), tx.clone());
    let engine = tokio::spawn(crate::engine::Engine::new(hw, mqtt, tx.clone()).run(rx));
    maintenance::start(device, tx.clone());
    tokio::time::timeout(Duration::from_secs(5), async {
        while system.call::<_, _, ()>("Reboot", &()).await.is_err() {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .unwrap();
    assert!(!manager.get_property::<bool>("Maintenance").await.unwrap());
    // A second operation succeeds only after the first reservation was released.
    crate::device::bounded(system.call::<_, _, ()>("PowerOff", &()))
        .await
        .unwrap();
    assert!(!manager.get_property::<bool>("Maintenance").await.unwrap());
    tx.send(Input::Shutdown)
        .unwrap_or_else(|_| panic!("engine stopped"));
    engine.await.unwrap();
}

#[derive(Clone)]
struct MockManager {
    agents: bool,
    ready: Arc<std::sync::atomic::AtomicBool>,
    maintenance: Arc<std::sync::atomic::AtomicBool>,
    registered: Arc<std::sync::atomic::AtomicBool>,
}

impl MockManager {
    fn new(agents: bool, ready: bool) -> Self {
        Self {
            agents,
            ready: Arc::new(std::sync::atomic::AtomicBool::new(ready)),
            maintenance: Arc::default(),
            registered: Arc::default(),
        }
    }
}

#[zbus::interface(name = "io.github.guilhem.DeviceCore1.Manager")]
impl MockManager {
    #[zbus(property)]
    fn capabilities(&self) -> Vec<String> {
        if self.agents {
            vec!["maintenance-agents".into()]
        } else {
            Vec::new()
        }
    }
    #[zbus(property)]
    fn ready(&self) -> bool {
        self.ready.load(std::sync::atomic::Ordering::SeqCst)
    }
    #[zbus(property)]
    fn maintenance(&self) -> bool {
        self.maintenance.load(std::sync::atomic::Ordering::SeqCst)
    }
    fn register_agent(&self, path: zbus::zvariant::OwnedObjectPath) -> zbus::fdo::Result<()> {
        assert_eq!(path.as_str(), crate::maintenance::PATH);
        if !self.agents {
            return Err(zbus::fdo::Error::AccessDenied("agents-disabled".into()));
        }
        self.registered
            .store(true, std::sync::atomic::Ordering::SeqCst);
        Ok(())
    }
}

#[tokio::test]
async fn manager_without_agents_still_fences_readiness_and_maintenance() {
    use crate::engine::Input;
    use crate::maintenance;
    use std::sync::atomic::Ordering;

    async fn next(
        rx: &mut tokio::sync::mpsc::UnboundedReceiver<Input>,
    ) -> maintenance::Observation {
        match tokio::time::timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap()
        {
            Input::MaintenanceObserved(observation) => observation,
            _ => panic!("unexpected maintenance input"),
        }
    }

    let bus = PrivateBus::new();
    let controller = MockManager::new(false, false);
    let _server = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .name(crate::device::SERVICE)
        .unwrap()
        .serve_at(crate::device::ROOT, controller.clone())
        .unwrap()
        .build()
        .await
        .unwrap();
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
    maintenance::start(Device::on_bus(bus.address.clone()), tx);
    let mut state = maintenance::State::default();
    let observation = next(&mut rx).await;
    assert!(observation.connection.is_some());
    state.observe(observation);
    assert!(state.blocked(), "Ready=false still blocks commands");
    controller.ready.store(true, Ordering::SeqCst);
    state.observe(next(&mut rx).await);
    assert!(!state.blocked());
    controller.maintenance.store(true, Ordering::SeqCst);
    state.observe(next(&mut rx).await);
    assert!(
        state.blocked(),
        "maintenance remains enforced without agents"
    );
    controller.maintenance.store(false, Ordering::SeqCst);
    state.observe(next(&mut rx).await);
    assert!(!state.blocked());
    assert!(!controller.registered.load(Ordering::SeqCst));
}

#[tokio::test]
async fn manager_owner_fences_lost_acquire_and_repeated_release() {
    use crate::maintenance;
    use std::sync::atomic::Ordering;
    let bus = PrivateBus::new();
    let controller = MockManager::new(true, true);
    let server = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .name(crate::device::SERVICE)
        .unwrap()
        .serve_at(crate::device::ROOT, controller.clone())
        .unwrap()
        .build()
        .await
        .unwrap();
    let device = Device::on_bus(bus.address.clone());
    let hw = hardware(&bus, device.clone());
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let mqtt = crate::bus::Bus::start(&configuration(&bus), tx.clone());
    let engine = tokio::spawn(crate::engine::Engine::new(hw, mqtt, tx.clone()).run(rx));
    maintenance::start(device.clone(), tx.clone());
    tokio::time::timeout(Duration::from_secs(3), async {
        while !controller.registered.load(Ordering::SeqCst) {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    let client = device.connection().await.unwrap();
    let destination = client.unique_name().unwrap().to_owned();
    let agent = zbus::Proxy::new(
        &server,
        destination.clone(),
        maintenance::PATH,
        "io.github.guilhem.DeviceCore1.Agent",
    )
    .await
    .unwrap();
    let token: String = agent.call("Acquire", &("update-a",)).await.unwrap();
    assert_eq!(
        agent
            .call::<_, _, String>("Acquire", &("update-a",))
            .await
            .unwrap(),
        token
    );
    assert!(agent
        .call::<_, _, ()>("Abort", &("wrong-operation",))
        .await
        .is_err());
    let attacker = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .build()
        .await
        .unwrap();
    let unauthorized = zbus::Proxy::new(
        &attacker,
        destination,
        maintenance::PATH,
        "io.github.guilhem.DeviceCore1.Agent",
    )
    .await
    .unwrap();
    assert!(unauthorized
        .call::<_, _, ()>("Abort", &("update-a",))
        .await
        .is_err());
    // Ignore the acquired token: rollback must work even when that reply was lost.
    agent
        .call::<_, _, ()>("Abort", &("update-a",))
        .await
        .unwrap();
    agent
        .call::<_, _, ()>("Abort", &("update-a",))
        .await
        .unwrap();
    for _ in 0..2 {
        agent
            .call::<_, _, ()>("Release", &(token.as_str(),))
            .await
            .unwrap();
    }
    let second: String = agent.call("Acquire", &("update-b",)).await.unwrap();
    assert_ne!(token, second);
    assert!(agent
        .call::<_, _, ()>("Release", &(token.as_str(),))
        .await
        .is_err());
    assert!(agent
        .call::<_, _, ()>("Abort", &("update-a",))
        .await
        .is_err());
    for _ in 0..2 {
        agent
            .call::<_, _, ()>("Release", &(second.as_str(),))
            .await
            .unwrap();
    }
    tx.send(crate::engine::Input::Shutdown)
        .unwrap_or_else(|_| panic!("engine stopped"));
    engine.await.unwrap();
}
