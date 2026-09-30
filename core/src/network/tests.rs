use super::*;
use nm::{Dict, Settings, NM, ROOT, SETTINGS};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::io::{BufRead, BufReader};
use std::pin::Pin;
use std::process::{Child, Stdio};
use zbus::export::futures_core::Stream;
use zbus::zvariant::OwnedObjectPath;
use zbus::{ObjectServer, Proxy};

const DEV: &str = "/org/freedesktop/NetworkManager/Devices/1";
const ACT: &str = "/org/freedesktop/NetworkManager/ActiveConnection/1";
const IP: &str = "/org/freedesktop/NetworkManager/IP4Config/1";
const AP: &str = "/org/freedesktop/NetworkManager/AccessPoint/1";
const CP: &str = "/org/freedesktop/NetworkManager/Checkpoint/1";
const OLD: &str = "/org/freedesktop/NetworkManager/Settings/1";
const OLD_UUID: &str = "11111111-1111-4111-8111-111111111111";
const RECOVERY: &str = "/org/freedesktop/NetworkManager/Settings/2";

fn path(s: &str) -> OwnedObjectPath {
    s.try_into().unwrap()
}
fn copy_settings(settings: &Settings) -> Settings {
    settings
        .iter()
        .map(|(section, values)| {
            (
                section.clone(),
                values
                    .iter()
                    .map(|(name, value)| (name.clone(), value.try_clone().unwrap()))
                    .collect(),
            )
        })
        .collect()
}

struct PrivateBus {
    child: Child,
    address: String,
}
impl PrivateBus {
    fn start() -> Self {
        let mut child = std::process::Command::new("dbus-daemon")
            .args(["--session", "--nofork", "--nopidfile", "--print-address=1"])
            .stdout(Stdio::piped())
            .spawn()
            .expect("private dbus-daemon");
        let mut address = String::new();
        BufReader::new(child.stdout.take().unwrap())
            .read_line(&mut address)
            .unwrap();
        assert!(address.starts_with("unix:"), "private bus did not start");
        Self {
            child,
            address: address.trim().into(),
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
    }
}

#[derive(Clone, Copy)]
enum Outcome {
    Success,
    Auth,
    Dhcp,
    NoAddress,
    Foreign,
    AccessPoint,
    Late,
}
struct FakeProfile {
    settings: Settings,
    unsaved: bool,
}
struct Checkpoint {
    profiles: HashSet<String>,
    active: String,
    state: u32,
    mode: u32,
    address: String,
}
struct FakeState {
    profiles: BTreeMap<String, FakeProfile>,
    active: String,
    state: u32,
    mode: u32,
    address: String,
    reason: u32,
    checkpoint: Option<Checkpoint>,
    outcome: Outcome,
    activation_count: usize,
    saved_count: usize,
    scan_count: usize,
    reject_scan: bool,
    next_profile: usize,
}
impl FakeState {
    fn new(outcome: Outcome) -> Self {
        let mut old = nm::wifi_settings(
            OLD_UUID,
            "known network",
            &[255, 1, 2],
            "wpa-psk",
            "oldpassword",
        );
        old.get_mut("connection")
            .unwrap()
            .insert("autoconnect".into(), true.into());
        let mut recovery =
            nm::wifi_settings(HOTSPOT_UUID, "NabOS recovery", b"Nabaztag", "open", "");
        recovery
            .get_mut("802-11-wireless")
            .unwrap()
            .insert("mode".into(), nm::string("ap"));
        Self {
            profiles: BTreeMap::from([
                (
                    OLD.into(),
                    FakeProfile {
                        settings: old,
                        unsaved: false,
                    },
                ),
                (
                    RECOVERY.into(),
                    FakeProfile {
                        settings: recovery,
                        unsaved: false,
                    },
                ),
            ]),
            active: RECOVERY.into(),
            state: 100,
            mode: 3,
            address: HOTSPOT_ADDRESS.into(),
            reason: 0,
            checkpoint: None,
            outcome,
            activation_count: 0,
            saved_count: 0,
            scan_count: 0,
            reject_scan: false,
            next_profile: 3,
        }
    }
    fn rollback(&mut self) {
        if let Some(cp) = self.checkpoint.take() {
            self.profiles.retain(|p, _| cp.profiles.contains(p));
            self.active = cp.active;
            self.state = cp.state;
            self.mode = cp.mode;
            self.address = cp.address;
            self.reason = 0;
        }
    }
}
type Fake = Arc<Mutex<FakeState>>;

struct Manager(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager")]
impl Manager {
    fn get_devices(&self) -> Vec<OwnedObjectPath> {
        vec![path(DEV)]
    }
    #[zbus(property)]
    fn checkpoints(&self) -> Vec<OwnedObjectPath> {
        if self.0.lock().unwrap().checkpoint.is_some() {
            vec![path(CP)]
        } else {
            vec![]
        }
    }
    fn checkpoint_create(
        &self,
        devices: Vec<OwnedObjectPath>,
        rollback_timeout: u32,
        flags: u32,
    ) -> fdo::Result<OwnedObjectPath> {
        assert_eq!(devices, vec![path(DEV)]);
        assert_eq!(rollback_timeout, 90);
        assert_eq!(flags, 2);
        let mut s = self.0.lock().unwrap();
        if s.checkpoint.is_some() {
            return Err(fdo::Error::Failed("checkpoint exists".into()));
        }
        s.checkpoint = Some(Checkpoint {
            profiles: s.profiles.keys().cloned().collect(),
            active: s.active.clone(),
            state: s.state,
            mode: s.mode,
            address: s.address.clone(),
        });
        let state = self.0.clone();
        tokio::spawn(async move {
            tokio::time::sleep(GRACE).await;
            state.lock().unwrap().rollback();
        });
        Ok(path(CP))
    }
    fn checkpoint_rollback(&self, checkpoint: OwnedObjectPath) -> HashMap<String, u32> {
        assert_eq!(checkpoint.as_str(), CP);
        self.0.lock().unwrap().rollback();
        HashMap::from([(DEV.into(), 0)])
    }
    fn checkpoint_destroy(&self, checkpoint: OwnedObjectPath) {
        assert_eq!(checkpoint.as_str(), CP);
        self.0.lock().unwrap().checkpoint = None;
    }
    async fn activate_connection(
        &self,
        connection: OwnedObjectPath,
        device: OwnedObjectPath,
        specific: OwnedObjectPath,
    ) -> fdo::Result<OwnedObjectPath> {
        assert_eq!(device.as_str(), DEV);
        assert_eq!(specific.as_str(), "/");
        let outcome = {
            let mut s = self.0.lock().unwrap();
            let uuid = nm::text(
                &s.profiles
                    .get(connection.as_str())
                    .ok_or_else(|| fdo::Error::UnknownObject("deleted profile".into()))?
                    .settings,
                "connection",
                "uuid",
            )
            .to_owned();
            s.active = connection.to_string();
            s.activation_count += 1;
            if uuid == HOTSPOT_UUID {
                s.state = 100;
                s.mode = 3;
                s.address = HOTSPOT_ADDRESS.into();
                return Ok(path(ACT));
            }
            s.state = 100;
            s.mode = 2;
            s.address = "192.168.5.8".into();
            s.reason = 0;
            match s.outcome {
                Outcome::Auth => {
                    s.state = 120;
                    s.address.clear();
                    s.reason = 7;
                }
                Outcome::Dhcp => {
                    s.state = 120;
                    s.address.clear();
                    s.reason = 17;
                }
                Outcome::NoAddress => {
                    s.address = "169.254.1.2".into();
                }
                Outcome::Foreign => {
                    s.active = OLD.into();
                    s.state = 120;
                    s.reason = 39;
                }
                Outcome::AccessPoint => {
                    s.mode = 3;
                }
                _ => {}
            }
            s.outcome
        };
        if matches!(outcome, Outcome::Late) {
            tokio::time::sleep(Duration::from_secs(6)).await;
        }
        Ok(path(ACT))
    }
}

struct SettingsApi(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager.Settings")]
impl SettingsApi {
    fn list_connections(&self) -> Vec<OwnedObjectPath> {
        self.0
            .lock()
            .unwrap()
            .profiles
            .keys()
            .map(|p| path(p))
            .collect()
    }
    async fn add_connection2(
        &self,
        settings: Settings,
        flags: u32,
        args: Dict,
        #[zbus(object_server)] server: &ObjectServer,
    ) -> zbus::fdo::Result<(OwnedObjectPath, Dict)> {
        assert!(args.is_empty());
        assert!(flags == 1 || flags == 2);
        let p = {
            let mut s = self.0.lock().unwrap();
            let p = format!("{SETTINGS}/{}", s.next_profile);
            s.next_profile += 1;
            s.profiles.insert(
                p.clone(),
                FakeProfile {
                    settings,
                    unsaved: flags == 2,
                },
            );
            p
        };
        server
            .at(
                p.as_str(),
                ProfileApi {
                    state: self.0.clone(),
                    path: p.clone(),
                },
            )
            .await
            .unwrap();
        Ok((path(&p), Dict::new()))
    }
}

struct ProfileApi {
    state: Fake,
    path: String,
}
#[zbus::interface(name = "org.freedesktop.NetworkManager.Settings.Connection")]
impl ProfileApi {
    fn get_settings(&self) -> fdo::Result<Settings> {
        let s = self.state.lock().unwrap();
        let mut settings = copy_settings(
            &s.profiles
                .get(&self.path)
                .ok_or_else(|| fdo::Error::UnknownObject("deleted profile".into()))?
                .settings,
        );
        if let Some(security) = settings.get_mut("802-11-wireless-security") {
            security.remove("psk");
        }
        Ok(settings)
    }
    fn get_secrets(&self, _setting: &str) -> fdo::Result<Settings> {
        let s = self.state.lock().unwrap();
        let settings = &s
            .profiles
            .get(&self.path)
            .ok_or_else(|| fdo::Error::UnknownObject("deleted profile".into()))?
            .settings;
        let mut secrets = Settings::new();
        if let Some(security) = settings.get("802-11-wireless-security") {
            if let Some(psk) = security.get("psk") {
                secrets.insert(
                    "802-11-wireless-security".into(),
                    HashMap::from([("psk".into(), psk.try_clone().unwrap())]),
                );
            }
        }
        Ok(secrets)
    }
    #[zbus(property)]
    fn unsaved(&self) -> bool {
        self.state
            .lock()
            .unwrap()
            .profiles
            .get(&self.path)
            .is_some_and(|p| p.unsaved)
    }
    fn delete(&self) {
        let mut s = self.state.lock().unwrap();
        s.profiles.remove(&self.path);
        if s.active == self.path {
            s.active.clear();
            s.state = 30;
            s.mode = 0;
            s.address.clear();
        }
    }
    fn update2(&self, settings: Settings, flags: u32, args: Dict) -> Dict {
        assert_eq!(flags, 1);
        assert!(args.is_empty());
        assert_eq!(
            settings
                .get("connection")
                .unwrap()
                .get("autoconnect")
                .and_then(|v| bool::try_from(v).ok()),
            Some(true)
        );
        let mut s = self.state.lock().unwrap();
        let p = s
            .profiles
            .get_mut(&self.path)
            .expect("candidate still exists when saved");
        p.settings = settings;
        p.unsaved = false;
        s.saved_count += 1;
        Dict::new()
    }
}

struct DeviceApi(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager.Device")]
impl DeviceApi {
    #[zbus(property)]
    fn device_type(&self) -> u32 {
        2
    }
    #[zbus(property)]
    fn state(&self) -> u32 {
        self.0.lock().unwrap().state
    }
    #[zbus(property)]
    fn state_reason(&self) -> (u32, u32) {
        let s = self.0.lock().unwrap();
        (s.state, s.reason)
    }
    #[zbus(property)]
    fn active_connection(&self) -> OwnedObjectPath {
        if self.0.lock().unwrap().active.is_empty() {
            path("/")
        } else {
            path(ACT)
        }
    }
}
struct WifiApi(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager.Device.Wireless")]
impl WifiApi {
    #[zbus(property)]
    fn mode(&self) -> u32 {
        self.0.lock().unwrap().mode
    }
    fn get_all_access_points(&self) -> Vec<OwnedObjectPath> {
        vec![path(AP)]
    }
    fn request_scan(&self, args: Dict) -> fdo::Result<()> {
        assert!(args.is_empty());
        let mut s = self.0.lock().unwrap();
        s.scan_count += 1;
        if s.reject_scan {
            Err(fdo::Error::Failed("temporarily unsupported on AP".into()))
        } else {
            Ok(())
        }
    }
}
struct ActiveApi(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager.Connection.Active")]
impl ActiveApi {
    #[zbus(property)]
    fn state(&self) -> u32 {
        if self.0.lock().unwrap().state == 100 {
            2
        } else {
            1
        }
    }
    #[zbus(property)]
    fn uuid(&self) -> String {
        let s = self.0.lock().unwrap();
        nm::text(
            &s.profiles.get(&s.active).unwrap().settings,
            "connection",
            "uuid",
        )
        .into()
    }
    #[zbus(property)]
    fn connection(&self) -> OwnedObjectPath {
        path(&self.0.lock().unwrap().active)
    }
    #[zbus(property)]
    fn ip4_config(&self) -> OwnedObjectPath {
        path(IP)
    }
    #[zbus(property)]
    fn ip6_config(&self) -> OwnedObjectPath {
        path("/")
    }
}
struct IpApi(Fake);
#[zbus::interface(name = "org.freedesktop.NetworkManager.IP4Config")]
impl IpApi {
    #[zbus(property)]
    fn address_data(&self) -> Vec<Dict> {
        let s = self.0.lock().unwrap();
        if s.address.is_empty() {
            vec![]
        } else {
            vec![HashMap::from([
                ("address".into(), nm::string(&s.address)),
                ("prefix".into(), 24u32.into()),
            ])]
        }
    }
}
struct ApApi;
#[zbus::interface(name = "org.freedesktop.NetworkManager.AccessPoint")]
impl ApApi {
    #[zbus(property)]
    fn ssid(&self) -> Vec<u8> {
        vec![255, 1, 2]
    }
    #[zbus(property)]
    fn strength(&self) -> u8 {
        80
    }
    #[zbus(property)]
    fn flags(&self) -> u32 {
        1
    }
    #[zbus(property)]
    fn wpa_flags(&self) -> u32 {
        0
    }
    #[zbus(property)]
    fn rsn_flags(&self) -> u32 {
        0x100
    }
}
struct CheckpointApi;
#[zbus::interface(name = "org.freedesktop.NetworkManager.Checkpoint")]
impl CheckpointApi {
    #[zbus(property)]
    fn devices(&self) -> Vec<OwnedObjectPath> {
        vec![path(DEV)]
    }
}

async fn fake_nm(bus: &PrivateBus, state: Fake) -> Connection {
    let conn = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .serve_at(ROOT, Manager(state.clone()))
        .unwrap()
        .serve_at(SETTINGS, SettingsApi(state.clone()))
        .unwrap()
        .serve_at(DEV, DeviceApi(state.clone()))
        .unwrap()
        .serve_at(DEV, WifiApi(state.clone()))
        .unwrap()
        .serve_at(ACT, ActiveApi(state.clone()))
        .unwrap()
        .serve_at(IP, IpApi(state.clone()))
        .unwrap()
        .serve_at(AP, ApApi)
        .unwrap()
        .serve_at(CP, CheckpointApi)
        .unwrap()
        .name(NM)
        .unwrap()
        .build()
        .await
        .unwrap();
    let profiles: Vec<String> = state.lock().unwrap().profiles.keys().cloned().collect();
    for p in profiles {
        conn.object_server()
            .at(
                p.as_str(),
                ProfileApi {
                    state: state.clone(),
                    path: p.clone(),
                },
            )
            .await
            .unwrap();
    }
    conn
}

async fn controller(bus: &PrivateBus) -> (Controller, Api) {
    let shared = Arc::new(Mutex::new(Shared::default()));
    let (commands, receiver) = mpsc::channel(8);
    let api = Api {
        shared: shared.clone(),
        commands: commands.clone(),
    };
    let conn = zbus::connection::Builder::address(bus.address.as_str())
        .unwrap()
        .serve_at(
            PATH,
            Api {
                shared: shared.clone(),
                commands,
            },
        )
        .unwrap()
        .name(SERVICE)
        .unwrap()
        .build()
        .await
        .unwrap();
    (Controller::new(conn, shared, receiver), api)
}

fn request(uuid: &str) -> Request {
    Request {
        ssid: vec![254, 0, 42],
        security: "wpa-psk".into(),
        password: "newpassword".into(),
        uuid: uuid.into(),
        setup: false,
    }
}

#[tokio::test]
async fn private_bus_success_reuses_native_profiles_and_keeps_secrets_private() {
    let bus = PrivateBus::start();
    let state = Arc::new(Mutex::new(FakeState::new(Outcome::Success)));
    let _nm = fake_nm(&bus, state.clone()).await;
    let (mut core, _) = controller(&bus).await;
    core.reconcile_inner().await.unwrap();
    assert_eq!(core.shared.lock().unwrap().status.mode, "hotspot");
    assert_eq!(core.shared.lock().unwrap().profiles.len(), 1);
    let nm = Nm::discover(&core.bus).await.unwrap();
    assert_eq!(nm.networks().await.unwrap()[0].ssid, vec![255, 1, 2]);
    core.attempt(&request(""), &AtomicBool::new(false))
        .await
        .unwrap();
    assert_eq!(core.shared.lock().unwrap().status.mode, "client");
    {
        let s = state.lock().unwrap();
        assert_eq!(s.profiles.len(), 3);
        assert!(s.profiles.contains_key(OLD));
        assert_eq!(s.saved_count, 1);
        assert!(s.checkpoint.is_none());
        assert_eq!(
            nm::text(
                &s.profiles.get(OLD).unwrap().settings,
                "802-11-wireless-security",
                "psk"
            ),
            "oldpassword"
        );
        let new = s.profiles.get(&s.active).unwrap();
        assert!(!new.unsaved);
        assert_eq!(
            nm::text(&new.settings, "802-11-wireless-security", "psk"),
            "newpassword"
        );
    }
    core.attempt(&request(OLD_UUID), &AtomicBool::new(false))
        .await
        .unwrap();
    assert_eq!(state.lock().unwrap().profiles.len(), 3);
    assert_eq!(core.shared.lock().unwrap().status.profile_uuid, OLD_UUID);
    core.reconcile_inner().await.unwrap();
    let client = bus.connect().await;
    let proxy = Proxy::new(&client, SERVICE, PATH, INTERFACE).await.unwrap();
    for name in ["Status", "Networks", "Profiles"] {
        let value: String = proxy.get_property(name).await.unwrap();
        assert!(
            !value.contains("password") && !value.contains("\"psk\":") && !value.contains("token"),
            "{value}"
        );
        let _: serde_json::Value = serde_json::from_str(&value).unwrap();
    }
    let xml = proxy.introspect().await.unwrap();
    assert!(xml.contains("name=\"Connect\"") && xml.contains("name=\"Changed\""));
    let mut signals = proxy.receive_signal("Changed").await.unwrap();
    core.emit().await.unwrap();
    let message = tokio::time::timeout(
        Duration::from_secs(2),
        std::future::poll_fn(|cx| Pin::new(&mut signals).poll_next(cx)),
    )
    .await
    .unwrap()
    .unwrap();
    let (value,): (String,) = message.body().deserialize().unwrap();
    assert!(!value.contains("password") && !value.contains("token"));
}

#[tokio::test]
async fn private_bus_wifi_failures_preserve_old_profiles_and_restore_hotspot() {
    for (outcome, error) in [
        (Outcome::Auth, "wifi-authentication-failed"),
        (Outcome::Dhcp, "no-address"),
        (Outcome::Foreign, "activation-failed"),
        (Outcome::Late, "nm-timeout"),
    ] {
        let bus = PrivateBus::start();
        let state = Arc::new(Mutex::new(FakeState::new(outcome)));
        let _nm = fake_nm(&bus, state.clone()).await;
        let (mut core, _) = controller(&bus).await;
        assert_eq!(
            core.attempt(&request(""), &AtomicBool::new(false)).await,
            Err(error)
        );
        if matches!(outcome, Outcome::Late) {
            // Deliver the original reply after the client timeout and rollback.
            tokio::time::sleep(Duration::from_millis(1200)).await;
        }
        assert_eq!(core.shared.lock().unwrap().status.mode, "hotspot");
        let s = state.lock().unwrap();
        assert_eq!(s.active, RECOVERY);
        assert_eq!(s.profiles.len(), 2);
        assert!(s.profiles.contains_key(OLD));
        assert_eq!(s.saved_count, 0);
    }
}

#[tokio::test]
async fn private_bus_no_address_or_ap_mode_cannot_commit_and_dbus_cancel_stays_responsive() {
    for outcome in [Outcome::NoAddress, Outcome::AccessPoint] {
        let bus = PrivateBus::start();
        let state = Arc::new(Mutex::new(FakeState::new(outcome)));
        let _nm = fake_nm(&bus, state.clone()).await;
        let (mut core, api) = controller(&bus).await;
        core.reconcile_inner().await.unwrap();
        let client = bus.connect().await;
        let proxy = Proxy::new(&client, SERVICE, PATH, INTERFACE).await.unwrap();
        let core_task = tokio::spawn(core.run());
        let id: u64 = proxy
            .call(
                "Connect",
                &(vec![254u8, 0, 42], "wpa-psk", "newpassword", "", ""),
            )
            .await
            .unwrap();
        assert!(proxy
            .call::<_, _, u64>("Connect", &(vec![1u8], "open", "", "", ""))
            .await
            .is_err());
        assert_eq!(
            state.lock().unwrap().activation_count,
            0,
            "Connect returned after radio change"
        );
        let _: String = proxy.get_property("Status").await.unwrap();
        assert!(proxy.call::<_, _, ()>("Cancel", &(id + 1,)).await.is_err());
        tokio::time::timeout(Duration::from_secs(3), async {
            while state.lock().unwrap().activation_count == 0 {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(state.lock().unwrap().saved_count, 0);
        proxy.call::<_, _, ()>("Cancel", &(id,)).await.unwrap();
        tokio::time::timeout(Duration::from_secs(3), async {
            while api.shared.lock().unwrap().active.is_some() {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(api.shared.lock().unwrap().status.phase, "cancelled");
        assert_eq!(state.lock().unwrap().profiles.len(), 2);
        assert_eq!(state.lock().unwrap().saved_count, 0);
        core_task.abort();
    }
}

#[tokio::test]
async fn private_bus_restart_reconstructs_and_does_not_touch_outstanding_checkpoint() {
    let bus = PrivateBus::start();
    let state = Arc::new(Mutex::new(FakeState::new(Outcome::Success)));
    let conn = fake_nm(&bus, state.clone()).await;
    let (mut core, _) = controller(&bus).await;
    core.reconcile_inner().await.unwrap();
    let old_owner = Nm::discover(&core.bus).await.unwrap();
    let cp = old_owner.checkpoint().await.unwrap();
    let uuid = new_uuid().unwrap();
    let settings = nm::wifi_settings(
        &uuid,
        &format!("{}{uuid}", nm::CANDIDATE_PREFIX),
        b"orphan",
        "open",
        "",
    );
    old_owner.add(&settings, false).await.unwrap();
    let (mut restarted, _) = {
        let (tx, rx) = mpsc::channel(8);
        let shared = Arc::new(Mutex::new(Shared::default()));
        (
            Controller::new(core.bus.clone(), shared.clone(), rx),
            Api {
                shared,
                commands: tx,
            },
        )
    };
    restarted.reconcile_inner().await.unwrap();
    assert_eq!(state.lock().unwrap().profiles.len(), 3);
    old_owner.rollback(&cp).await.unwrap();
    // A memory candidate without a checkpoint is safe to clean after restart.
    let orphan = old_owner.add(&settings, false).await.unwrap();
    restarted.reconcile_inner().await.unwrap();
    assert!(!state.lock().unwrap().profiles.contains_key(orphan.as_str()));
    assert!(state.lock().unwrap().profiles.contains_key(OLD));
    conn.close().await.unwrap();
    assert!(old_owner.activate(&path(OLD)).await.is_err());
    let _new_nm = fake_nm(&bus, state.clone()).await;
    restarted.reconcile_inner().await.unwrap();
    assert_ne!(restarted.owner, old_owner.owner);
    assert_eq!(restarted.shared.lock().unwrap().status.mode, "hotspot");
}

#[tokio::test]
async fn private_bus_recovery_grace_retry_reservation_and_manual_scan() {
    let bus = PrivateBus::start();
    let state = Arc::new(Mutex::new(FakeState::new(Outcome::Success)));
    let _nm = fake_nm(&bus, state.clone()).await;
    let (mut core, api) = controller(&bus).await;
    core.reconcile_inner().await.unwrap();
    assert_eq!(state.lock().unwrap().activation_count, 0);
    let token = api.reserve("").unwrap();
    assert!(!api.authorized(&token));
    assert!(api.reserve("").is_err());
    assert!(api.connect(vec![1], "open", "", "", &token).is_err());
    let now = crate::hw::button::monotonic();
    {
        let mut s = api.shared.lock().unwrap();
        let reservation = s.reservation.as_mut().unwrap();
        reservation.created = now - Duration::from_secs(2);
        reservation.last_press = reservation.created;
        s.press(now - Duration::from_secs(1), now);
    }
    assert!(api.authorized(&token));
    assert_eq!(api.reserve(&token).unwrap(), token);
    core.retry = Instant::now() - Duration::from_secs(1);
    core.reconcile_inner().await.unwrap();
    assert_eq!(
        state.lock().unwrap().activation_count,
        0,
        "lease did not suspend retry"
    );
    let nm = Nm::discover(&core.bus).await.unwrap();
    state.lock().unwrap().reject_scan = true;
    assert!(nm.scan().await.is_err());
    core.refresh_networks(&nm).await;
    assert_eq!(
        core.shared.lock().unwrap().networks[0].ssid,
        vec![255, 1, 2]
    );
    assert!(api.forget(HOTSPOT_UUID).await.is_err());
    let id = api.connect(vec![1], "open", "", "", &token).unwrap();
    assert!(
        !api.authorized(&token),
        "physical authorization was not consumed"
    );
    api.cancel(id).unwrap();
    api.shared.lock().unwrap().active = None;
    api.release(&token).unwrap();
    core.reconcile_inner().await.unwrap();
    assert_eq!(state.lock().unwrap().activation_count, 1);
    assert_eq!(state.lock().unwrap().active, OLD);
    core.reconcile_inner().await.unwrap();
    {
        let mut s = state.lock().unwrap();
        s.active.clear();
        s.state = 30;
        s.mode = 0;
        s.address.clear();
    }
    core.reconcile_inner().await.unwrap();
    assert!(core.grace.unwrap() >= Instant::now() + Duration::from_secs(80));
    assert_eq!(
        state.lock().unwrap().activation_count,
        1,
        "hotspot opened before grace elapsed"
    );
    core.grace = Some(Instant::now() - Duration::from_secs(1));
    core.reconcile_inner().await.unwrap();
    assert_eq!(state.lock().unwrap().active, RECOVERY);
    assert_eq!(state.lock().unwrap().activation_count, 2);
}

#[test]
fn presence_uses_edge_clock_and_does_not_extend_with_lease() {
    let mut s = Shared::default();
    s.status.mode = "hotspot";
    let sec = Duration::from_secs;
    s.reservation = Some(Reservation {
        token: "token".into(),
        created: sec(10),
        last_press: sec(10),
        expires: sec(310),
        authorized_until: None,
    });
    s.press(sec(9), sec(11));
    assert!(!s.authorized("token", sec(11)));
    s.press(sec(11), sec(12));
    assert!(s.authorized("token", sec(12)));
    s.reservation.as_mut().unwrap().authorized_until = None;
    s.press(sec(11), sec(13));
    assert!(
        !s.authorized("token", sec(13)),
        "same edge cannot authorize a second use"
    );
    s.press(sec(14), sec(14));
    assert!(s.authorized("token", sec(14)));
    s.reservation.as_mut().unwrap().expires = sec(600);
    assert!(!s.authorized("token", sec(314)));
    s.press(sec(12), sec(400));
    assert!(!s.authorized("token", sec(400)));
    s.press(sec(401), sec(400));
    assert!(!s.authorized("token", sec(400)));
    assert!(!s.authorized("wrong-token", sec(20)));
}

#[test]
fn delayed_press_during_attempt_cannot_authorize_the_next_attempt() {
    let sec = Duration::from_secs;
    for error in ["activation-failed", "cancelled", "bus-unavailable"] {
        let mut s = Shared::default();
        s.status.mode = "hotspot";
        s.reservation = Some(Reservation {
            token: "token".into(),
            created: sec(10),
            last_press: sec(10),
            expires: sec(310),
            authorized_until: None,
        });
        s.press(sec(11), sec(11));
        assert!(s.authorized("token", sec(12)));
        s.reservation.as_mut().unwrap().authorized_until = None;
        s.active = Some((1, Arc::new(AtomicBool::new(false))));
        s.press(sec(20), sec(20));
        assert!(!s.authorized("token", sec(20)));
        s.finish_attempt(2, Err(error), sec(30));
        assert!(
            s.active.is_some(),
            "stale completion must not finish an active attempt"
        );
        s.finish_attempt(1, Err(error), sec(40));
        s.press(sec(20), sec(41));
        assert!(
            !s.authorized("token", sec(41)),
            "{error}: delayed edge granted authorization"
        );
        s.press(sec(42), sec(42));
        assert!(
            s.authorized("token", sec(42)),
            "fresh press must still work"
        );
    }
}

#[test]
fn network_input_and_address_boundaries() {
    for (security, password, valid) in [
        ("open", "", true),
        ("open", "secret", false),
        ("wpa-psk", "short", false),
        ("wpa-psk", "password", true),
        ("sae", "secret", true),
        ("unsupported", "secret", false),
    ] {
        let request = Request {
            ssid: vec![0, 255, 1],
            security: security.into(),
            password: password.into(),
            uuid: String::new(),
            setup: false,
        };
        assert_eq!(request.validate().is_ok(), valid);
    }
    for ip in ["10.41.0.1", "192.168.1.5", "fd00::1", "2001:db8::1"] {
        assert!(nm::usable_address(ip), "{ip}");
    }
    for ip in [
        "0.0.0.0",
        "0.1.2.3",
        "127.0.0.1",
        "169.254.0.2",
        "255.255.255.255",
        "224.0.0.1",
        "::",
        "::1",
        "fe80::1",
        "::ffff:127.0.0.1",
        "::ffff:169.254.1.2",
        "ff02::1",
        "invalid",
    ] {
        assert!(!nm::usable_address(ip), "{ip}");
    }
    assert!(valid_uuid(&new_uuid().unwrap()));
    assert!(start(true).is_none());
}
