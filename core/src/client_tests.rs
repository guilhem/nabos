//! Private-bus tests use the real bus ProcessFD; systemd alone is a fixture.
use super::*;
use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use zbus::zvariant::{OwnedFd, OwnedObjectPath};

struct PrivateBus {
    child: Child,
    directory: PathBuf,
    address: String,
}
impl PrivateBus {
    fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let directory = std::env::temp_dir().join(format!(
            "nab-hardware-test-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&directory).unwrap();
        let address = format!("unix:path={}", directory.join("bus").display());
        let mut child = Command::new("dbus-daemon")
            .args([
                "--session",
                "--nofork",
                "--print-address=1",
                "--address",
                &address,
            ])
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut ready = String::new();
        BufReader::new(child.stdout.take().unwrap())
            .read_line(&mut ready)
            .unwrap();
        assert!(
            ready.starts_with(&address),
            "private bus must actually start"
        );
        Self {
            child,
            directory,
            address,
        }
    }
    async fn connect(&self) -> Connection {
        zbus::connection::Builder::address(self.address.as_str())
            .unwrap()
            .build()
            .await
            .unwrap()
    }
}
impl Drop for PrivateBus {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}

struct Systemd(Arc<Mutex<String>>);
#[zbus::interface(name = "org.freedesktop.systemd1.Manager")]
impl Systemd {
    #[zbus(name = "GetUnitByPIDFD")]
    fn get_unit_by_pidfd(&self, fd: OwnedFd) -> (OwnedObjectPath, String, Vec<u8>) {
        use std::os::fd::AsRawFd;
        let info =
            std::fs::read_to_string(format!("/proc/self/fdinfo/{}", fd.as_raw_fd())).unwrap();
        let pid: u32 = info
            .lines()
            .find_map(|l| l.strip_prefix("Pid:\t"))
            .unwrap()
            .parse()
            .unwrap();
        assert_eq!(
            pid,
            std::process::id(),
            "the bus must supply a real pinned caller FD"
        );
        (
            "/org/freedesktop/systemd1/unit/nabos_2eservice"
                .try_into()
                .unwrap(),
            self.0.lock().unwrap().clone(),
            vec![0; 16],
        )
    }
}
struct Manager;
#[zbus::interface(name = "io.github.guilhem.DeviceCore1.Manager")]
impl Manager {
    #[zbus(property)]
    fn ready(&self) -> bool {
        true
    }
    #[zbus(property)]
    fn maintenance(&self) -> bool {
        false
    }
    #[zbus(property)]
    fn capabilities(&self) -> Vec<String> {
        vec!["maintenance-agents".into()]
    }
    fn register_agent(&self, _path: OwnedObjectPath) {}
}
struct Network(Arc<Mutex<Vec<u64>>>);
#[zbus::interface(name = "io.github.guilhem.DeviceCore1.Network")]
impl Network {
    fn report_presence(&self, edge: u64) {
        self.0.lock().unwrap().push(edge);
    }
}
struct Harness {
    bus: PrivateBus,
    unit: Arc<Mutex<String>>,
    _systemd: Connection,
    daemon: Connection,
    hardware: Arc<Hardware>,
    connection: Connection,
    task: tokio::task::JoinHandle<Result<(), String>>,
    presence: Arc<Mutex<Vec<u64>>>,
}
impl Harness {
    async fn new() -> Self {
        let bus = PrivateBus::new();
        let unit = Arc::new(Mutex::new("nabos.service".into()));
        let systemd = zbus::connection::Builder::address(bus.address.as_str())
            .unwrap()
            .name("org.freedesktop.systemd1")
            .unwrap()
            .serve_at("/org/freedesktop/systemd1", Systemd(unit.clone()))
            .unwrap()
            .build()
            .await
            .unwrap();
        let presence = Arc::new(Mutex::new(Vec::new()));
        let daemon = zbus::connection::Builder::address(bus.address.as_str())
            .unwrap()
            .name(crate::device::SERVICE)
            .unwrap()
            .serve_at(crate::device::ROOT, Manager)
            .unwrap()
            .serve_at(
                format!("{}/Network", crate::device::ROOT),
                Network(presence.clone()),
            )
            .unwrap()
            .build()
            .await
            .unwrap();
        let device = Device::on_bus(bus.address.clone());
        let connection = device.connection().await.unwrap();
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        let cfg = crate::Config {
            simulate: true,
            gpio_chip: "/unused".into(),
            button_gpio: 17,
            ws2811_lib: "/unused".into(),
            led_brightness: 200,
            led_strip: "grb".into(),
        };
        let hw = Arc::new(Hw::open(&cfg, tx, None));
        let hardware = Hardware::new(hw);
        let h = hardware.clone();
        let task = tokio::spawn(async move { run(device, h, rx).await });
        let dbus = fdo::DBusProxy::new(&connection).await.unwrap();
        eventually(|| async {
            dbus.name_has_owner(SERVICE.try_into().unwrap())
                .await
                .unwrap_or(false)
        })
        .await;
        eventually(|| async { !hardware.state.lock().unwrap().maintenance.blocked() }).await;
        Self {
            bus,
            unit,
            _systemd: systemd,
            daemon,
            hardware,
            connection,
            task,
            presence,
        }
    }
    async fn api<'a>(&self, bus: &'a Connection) -> Proxy<'a> {
        Proxy::new(bus, SERVICE, PATH, SERVICE).await.unwrap()
    }
    async fn agent<'a>(&'a self, bus: &'a Connection) -> Proxy<'a> {
        Proxy::new(
            bus,
            self.connection.unique_name().unwrap().as_str(),
            maintenance::PATH,
            "io.github.guilhem.DeviceCore1.Agent",
        )
        .await
        .unwrap()
    }
}
impl Drop for Harness {
    fn drop(&mut self) {
        self.task.abort();
    }
}
async fn eventually<F, Fut>(mut check: F)
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = bool>,
{
    tokio::time::timeout(Duration::from_secs(5), async {
        while !check().await {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore = "requires a recent private D-Bus with ProcessFD (Ubuntu 26.04)"]
async fn private_bus_auth_bounds_wire_and_owner_disconnect() {
    let h = Harness::new().await;
    let caller = h.bus.connect().await;
    let other = h.bus.connect().await;
    let api = h.api(&caller).await;
    let outsider = h.api(&other).await;
    assert!(
        api.get_property::<bool>("Ready").await.unwrap(),
        "simulation has ears, LEDs and a button without RFID"
    );
    let status: Status = api.get_property("Status").await.unwrap();
    assert_eq!(
        (&status.0, status.1, status.6.as_str(), status.7, status.8),
        (&"simulated".into(), true, "none", 0, 0)
    );
    assert!(api
        .call::<_, _, ()>("SetLeds", &(vec![(0u8, 1u8, 2u8, 3u8)],))
        .await
        .is_err());
    *h.unit.lock().unwrap() = "untrusted.service".into();
    assert!(api.call::<_, _, ()>("Claim", &()).await.is_err());
    *h.unit.lock().unwrap() = "nabos.service".into();
    api.call::<_, _, ()>("Claim", &()).await.unwrap();
    assert!(outsider.call::<_, _, ()>("Claim", &()).await.is_err());
    api.call::<_, _, ()>("Claim", &()).await.unwrap();
    api.call::<_, _, ()>("SetLeds", &(vec![(0u8, 1u8, 2u8, 3u8), (4, 4, 5, 6)],))
        .await
        .unwrap();
    assert!(api
        .call::<_, _, ()>("SetLeds", &(vec![(0u8, 1u8, 2u8, 3u8), (0, 4, 5, 6)],))
        .await
        .is_err());
    assert!(api
        .call::<_, _, ()>("SetLeds", &(vec![(5u8, 1u8, 2u8, 3u8)],))
        .await
        .is_err());
    assert!(api
        .call::<_, _, ()>("PulseLed", &(5u8, 0u8, 0u8, 0u8))
        .await
        .is_err());
    assert!(api
        .call::<_, _, ()>("MoveEar", &(2u8, 0u8, false))
        .await
        .is_err());
    assert!(outsider
        .call::<_, _, ()>("MoveEar", &(0u8, 3u8, false))
        .await
        .is_err());
    api.call::<_, _, ()>("MoveEar", &(0u8, 255u8, true))
        .await
        .unwrap();
    api.call::<_, _, ()>("StepEar", &(1u8, 255u8, false))
        .await
        .unwrap();
    api.call::<_, _, ()>("WaitEarsIdle", &()).await.unwrap();
    let positions: (i16, i16) = api.call("ReadEars", &(true,)).await.unwrap();
    assert_eq!(positions, (0, 0));
    for (uid, data, timeout) in [
        (vec![0u8; 7], vec![], 1u32),
        (vec![0; 8], vec![0; 33], 1),
        (vec![0; 8], vec![], 0),
        (vec![0; 8], vec![], 61),
    ] {
        assert!(api
            .call::<_, _, u64>("StartWrite", &("st25tb", uid, 1u8, 2u8, data, timeout))
            .await
            .is_err());
    }
    let id: u64 = api
        .call(
            "StartWrite",
            &("st25tb", vec![0u8; 8], 1u8, 2u8, vec![0u8; 32], 1u32),
        )
        .await
        .unwrap();
    assert_eq!(
        api.call::<_, _, String>("WaitWrite", &(id,)).await.unwrap(),
        "no-reader"
    );
    assert!(outsider
        .call::<_, _, String>("WaitWrite", &(id,))
        .await
        .is_err());
    api.call::<_, _, ()>("CancelWrite", &(id,)).await.unwrap();
    for expected in 2..=MAX_WRITES as u64 {
        let next: u64 = api
            .call(
                "StartWrite",
                &("st25tb", vec![0u8; 8], 1u8, 2u8, Vec::<u8>::new(), 1u32),
            )
            .await
            .unwrap();
        assert_eq!(next, expected);
        assert_eq!(
            api.call::<_, _, String>("WaitWrite", &(next,))
                .await
                .unwrap(),
            "no-reader"
        );
    }
    assert!(api
        .call::<_, _, u64>(
            "StartWrite",
            &("st25tb", vec![0u8; 8], 1u8, 2u8, Vec::<u8>::new(), 1u32)
        )
        .await
        .is_err());
    {
        let mut state = h.hardware.state.lock().unwrap();
        for write in state.writes.values_mut() {
            write.completed = Some(Instant::now() - RESULT_TTL);
        }
    }
    h.hardware.cleanup();
    assert!(h.hardware.state.lock().unwrap().writes.is_empty());
    h.hardware.state.lock().unwrap().next_write = u64::MAX;
    assert!(api
        .call::<_, _, u64>(
            "StartWrite",
            &("st25tb", vec![0u8; 8], 1u8, 2u8, Vec::<u8>::new(), 1u32)
        )
        .await
        .is_err());
    assert!(
        h.hardware.state.lock().unwrap().writes.is_empty(),
        "ID exhaustion never wraps or leaves orphan records"
    );
    api.call::<_, _, ()>("PulseLed", &(4u8, 255u8, 0u8, 0u8))
        .await
        .unwrap();
    let token = h
        .hardware
        .token(caller.unique_name().unwrap().as_str())
        .unwrap();
    caller.clone().close().await.unwrap();
    eventually(|| async { token.is_cancelled() }).await;
    eventually(|| async { !h.hardware.state.lock().unwrap().draining }).await;
    outsider.call::<_, _, ()>("Claim", &()).await.unwrap();
    outsider.call::<_, _, ()>("Release", &()).await.unwrap();
}

#[tokio::test]
#[ignore = "requires a recent private D-Bus with ProcessFD (Ubuntu 26.04)"]
async fn private_bus_maintenance_fences_deferred_mutations_and_authenticates_current_daemon() {
    let h = Harness::new().await;
    let caller = h.bus.connect().await;
    let outsider = h.bus.connect().await;
    let api = h.api(&caller).await;
    api.call::<_, _, ()>("Claim", &()).await.unwrap();
    assert!(h
        .agent(&outsider)
        .await
        .call::<_, _, String>("Acquire", &("update-a",))
        .await
        .is_err());
    let serial = h.hardware.serial.lock().await;
    let clone = caller.clone();
    let deferred = tokio::spawn(async move {
        Proxy::new(&clone, SERVICE, PATH, SERVICE)
            .await
            .unwrap()
            .call::<_, _, ()>("MoveEar", &(0u8, 5u8, false))
            .await
    });
    tokio::time::sleep(Duration::from_millis(30)).await;
    let daemon = h.daemon.clone();
    let destination = h.connection.unique_name().unwrap().to_string();
    let acquired = tokio::spawn(async move {
        Proxy::new(
            &daemon,
            destination,
            maintenance::PATH,
            "io.github.guilhem.DeviceCore1.Agent",
        )
        .await
        .unwrap()
        .call::<_, _, String>("Acquire", &("update-a",))
        .await
    });
    eventually(|| async { h.hardware.state.lock().unwrap().maintenance.blocked() }).await;
    drop(serial);
    assert!(deferred.await.unwrap().is_err());
    let token = acquired.await.unwrap().unwrap();
    let agent = h.agent(&h.daemon).await;
    assert_eq!(
        agent
            .call::<_, _, String>("Acquire", &("update-a",))
            .await
            .unwrap(),
        token
    );
    assert!(agent
        .call::<_, _, String>("Acquire", &("update-b",))
        .await
        .is_err());
    assert!(api
        .call::<_, _, ()>("MoveEar", &(0u8, 5u8, false))
        .await
        .is_err());
    assert!(agent
        .call::<_, _, ()>("Release", &("wrong-token",))
        .await
        .is_err());
    agent.call::<_, _, ()>("Release", &(&token,)).await.unwrap();
    agent.call::<_, _, ()>("Release", &(&token,)).await.unwrap();
    eventually(|| async { !h.hardware.state.lock().unwrap().maintenance.blocked() }).await;
    api.call::<_, _, ()>("MoveEar", &(0u8, 2u8, false))
        .await
        .unwrap();
    let positions: (i16, i16) = api.call("ReadEars", &(false,)).await.unwrap();
    assert_eq!(positions, (2, 0));
    let old_owner = h.daemon.unique_name().unwrap().to_string();
    h.daemon.release_name(crate::device::SERVICE).await.unwrap();
    eventually(|| async { h.hardware.state.lock().unwrap().maintenance.blocked() }).await;
    assert!(maintenance::authorize(&h.connection, &old_owner)
        .await
        .is_err());
    assert!(agent
        .call::<_, _, String>("Acquire", &("stale-daemon",))
        .await
        .is_err());
    h.daemon.request_name(crate::device::SERVICE).await.unwrap();
    eventually(|| async { !h.hardware.state.lock().unwrap().maintenance.blocked() }).await;
}

#[tokio::test]
#[ignore = "requires a recent private D-Bus with ProcessFD (Ubuntu 26.04)"]
async fn private_bus_simulation_emits_hardware_interface_and_preserves_presence() {
    let h = Harness::new().await;
    let caller = h.bus.connect().await;
    let api = h.api(&caller).await;
    let mut button = api.receive_signal("Button").await.unwrap();
    let mut ears = api.receive_signal("EarMoved").await.unwrap();
    let mut tags = api.receive_signal("Tag").await.unwrap();
    let sim = Proxy::new(
        &caller,
        SERVICE,
        PATH,
        "io.github.guilhem.NabHardware1.Simulation",
    )
    .await
    .unwrap();
    sim.call::<_, _, ()>("Button", &("down", 123456789u64))
        .await
        .unwrap();
    let message = tokio::time::timeout(Duration::from_secs(2), button.next())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        message.body().deserialize::<(String, u64)>().unwrap(),
        ("down".into(), 123456789)
    );
    eventually(|| async { h.presence.lock().unwrap().as_slice() == [123456789] }).await;
    assert!(sim
        .call::<_, _, ()>("Button", &("click", 123u64))
        .await
        .is_err());
    sim.call::<_, _, ()>("EarMoved", &(1u8,)).await.unwrap();
    let message = tokio::time::timeout(Duration::from_secs(2), ears.next())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(message.body().deserialize::<u8>().unwrap(), 1);
    let tag = (
        false,
        "st25tb",
        vec![0u8; 8],
        "formatted",
        false,
        true,
        42u8,
        255u8,
        vec![0u8, 255, 128],
    );
    sim.call::<_, _, ()>("Tag", &tag).await.unwrap();
    let message = tokio::time::timeout(Duration::from_secs(2), tags.next())
        .await
        .unwrap()
        .unwrap();
    let received: (bool, String, Vec<u8>, String, bool, bool, u8, u8, Vec<u8>) =
        message.body().deserialize().unwrap();
    assert_eq!(received.7, 255);
    assert_eq!(received.8, vec![0, 255, 128]);
}

#[tokio::test]
#[ignore = "requires a recent private D-Bus with ProcessFD (Ubuntu 26.04)"]
async fn private_bus_explicit_address_survives_redirected_system_environment() {
    const TEST: &str =
        "bus::tests::private_bus_explicit_address_survives_redirected_system_environment";
    if let Ok(expected) = std::env::var("NABOS_TEST_PRIVATE_BUS_ID") {
        let address = std::env::var("NABOS_DEVICE_BUS_ADDRESS").unwrap();
        assert_eq!(std::env::var("DBUS_SYSTEM_BUS_ADDRESS").unwrap(), address);
        let device = Device::open(true).await.unwrap();
        let connection = device.connection().await.unwrap();
        assert_eq!(
            fdo::DBusProxy::new(&connection)
                .await
                .unwrap()
                .get_id()
                .await
                .unwrap(),
            expected.as_str()
        );
        return;
    }
    let bus = PrivateBus::new();
    let connection = bus.connect().await;
    let id = fdo::DBusProxy::new(&connection)
        .await
        .unwrap()
        .get_id()
        .await
        .unwrap();
    // A child isolates the redirected environment from concurrently running tests.
    let output = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", TEST, "--ignored"])
        .env("DBUS_SYSTEM_BUS_ADDRESS", &bus.address)
        .env("NABOS_DEVICE_BUS_ADDRESS", &bus.address)
        .env("NABOS_TEST_PRIVATE_BUS_ID", id.to_string())
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "redirected simulation failed: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
}
