//! NetworkManager's native system-bus API. Every call is bounded and addressed
//! to one unique bus owner: a restarted daemon cannot receive an old operation.

use super::{Network, Profile, Status, HOTSPOT_ADDRESS, HOTSPOT_UUID};
use serde::de::DeserializeOwned;
use std::collections::HashMap;
use std::future::Future;
use std::net::IpAddr;
use std::time::Duration;
use zbus::zvariant::{OwnedObjectPath, OwnedValue, Str, Type, Value};
use zbus::{Connection, Proxy};

pub const NM: &str = "org.freedesktop.NetworkManager";
pub const ROOT: &str = "/org/freedesktop/NetworkManager";
pub const SETTINGS: &str = "/org/freedesktop/NetworkManager/Settings";
pub const DEVICE: &str = "org.freedesktop.NetworkManager.Device";
pub const WIFI: &str = "org.freedesktop.NetworkManager.Device.Wireless";
pub const CONNECTION: &str = "org.freedesktop.NetworkManager.Settings.Connection";
pub const ACTIVE: &str = "org.freedesktop.NetworkManager.Connection.Active";
pub const CANDIDATE_PREFIX: &str = "nab-core candidate ";
pub const COMMITTED_ID: &str = "NabOS Wi-Fi";
pub type Dict = HashMap<String, OwnedValue>;
pub type Settings = HashMap<String, Dict>;
pub type Result<T> = std::result::Result<T, &'static str>;
const CALL_TIMEOUT: Duration = Duration::from_secs(5);

pub async fn bounded<T>(call: impl Future<Output = zbus::Result<T>>) -> Result<T> {
    tokio::time::timeout(CALL_TIMEOUT, call)
        .await
        .map_err(|_| "nm-timeout")?
        .map_err(|_| "nm-unavailable")
}

#[derive(Clone)]
pub struct Nm {
    pub bus: Connection,
    pub owner: String,
    pub device: OwnedObjectPath,
}

pub struct Saved {
    pub public: Profile,
    pub path: OwnedObjectPath,
    pub candidate: bool,
}

pub struct Snapshot {
    pub status: Status,
    pub failed: bool,
    pub reason: u32,
}

impl Nm {
    pub async fn discover(bus: &Connection) -> Result<Self> {
        let dbus = bounded(Proxy::new(
            bus,
            "org.freedesktop.DBus",
            "/org/freedesktop/DBus",
            "org.freedesktop.DBus",
        ))
        .await?;
        let owner: String = bounded(dbus.call("GetNameOwner", &(NM,))).await?;
        let manager = bounded(Proxy::new(bus, owner.as_str(), ROOT, NM)).await?;
        let devices: Vec<OwnedObjectPath> = bounded(manager.call("GetDevices", &())).await?;
        for device in devices {
            let proxy = bounded(Proxy::new(bus, owner.as_str(), device.as_str(), DEVICE)).await?;
            if bounded(proxy.get_property::<u32>("DeviceType")).await? == 2 {
                return Ok(Self {
                    bus: bus.clone(),
                    owner,
                    device,
                });
            }
        }
        Err("no-wifi-device")
    }

    pub async fn proxy<'a>(&'a self, path: &'a str, interface: &'a str) -> Result<Proxy<'a>> {
        // No property cache: checkpoint validation must use the actual device.
        bounded(
            zbus::proxy::Builder::new(&self.bus)
                .destination(self.owner.as_str())
                .map_err(|_| "nm-unavailable")?
                .path(path)
                .map_err(|_| "nm-unavailable")?
                .interface(interface)
                .map_err(|_| "nm-unavailable")?
                .cache_properties(zbus::proxy::CacheProperties::No)
                .build(),
        )
        .await
    }

    pub async fn property<T>(&self, path: &str, interface: &str, name: &str) -> Result<T>
    where
        T: TryFrom<OwnedValue>,
        T::Error: Into<zbus::Error>,
    {
        bounded(self.proxy(path, interface).await?.get_property(name)).await
    }

    pub async fn call<B, R>(&self, path: &str, interface: &str, method: &str, body: &B) -> Result<R>
    where
        B: serde::Serialize + Type,
        R: DeserializeOwned + Type,
    {
        bounded(self.proxy(path, interface).await?.call(method, body)).await
    }

    pub async fn profiles(&self) -> Result<Vec<Saved>> {
        let paths: Vec<OwnedObjectPath> = self
            .call(
                SETTINGS,
                "org.freedesktop.NetworkManager.Settings",
                "ListConnections",
                &(),
            )
            .await?;
        let mut profiles = Vec::new();
        for path in paths {
            let settings: Settings = self
                .call(path.as_str(), CONNECTION, "GetSettings", &())
                .await?;
            if text(&settings, "connection", "type") != "802-11-wireless" {
                continue;
            }
            let uuid = text(&settings, "connection", "uuid").to_owned();
            let mode = text(&settings, "802-11-wireless", "mode");
            // Saved enterprise networks remain usable through their native UUID.
            if uuid == HOTSPOT_UUID || (!mode.is_empty() && mode != "infrastructure") {
                continue;
            }
            let ssid = settings
                .get("802-11-wireless")
                .and_then(|v| v.get("ssid"))
                .and_then(|v| Vec::<u8>::try_from(v.try_clone().ok()?).ok())
                .unwrap_or_default();
            // The marker survives TO_DISK and NM/core restarts until promotion.
            let candidate =
                text(&settings, "connection", "id") == format!("{CANDIDATE_PREFIX}{uuid}");
            if !uuid.is_empty() && !ssid.is_empty() {
                profiles.push(Saved {
                    public: Profile { uuid, ssid },
                    path,
                    candidate,
                });
            }
        }
        Ok(profiles)
    }

    pub async fn snapshot(&self) -> Result<Snapshot> {
        let state: u32 = self.property(self.device.as_str(), DEVICE, "State").await?;
        let (_, reason): (u32, u32) = self
            .property(self.device.as_str(), DEVICE, "StateReason")
            .await?;
        let active: OwnedObjectPath = self
            .property(self.device.as_str(), DEVICE, "ActiveConnection")
            .await?;
        let mut status = Status::reconnecting();
        if active.as_str() != "/" {
            let active_state: u32 = self.property(active.as_str(), ACTIVE, "State").await?;
            status.profile_uuid = self.property(active.as_str(), ACTIVE, "Uuid").await?;
            let settings_path: OwnedObjectPath =
                self.property(active.as_str(), ACTIVE, "Connection").await?;
            let settings: Settings = self
                .call(settings_path.as_str(), CONNECTION, "GetSettings", &())
                .await?;
            status.ssid = settings
                .get("802-11-wireless")
                .and_then(|s| s.get("ssid"))
                .and_then(|v| Vec::<u8>::try_from(v.try_clone().ok()?).ok())
                .unwrap_or_default();
            let mode: u32 = self.property(self.device.as_str(), WIFI, "Mode").await?;
            let configured_mode = text(&settings, "802-11-wireless", "mode");
            if state == 100 && active_state == 2 {
                status.address = self.address(active.as_str()).await?;
                if mode == 2
                    && (configured_mode.is_empty() || configured_mode == "infrastructure")
                    && !status.address.is_empty()
                    && status.profile_uuid != HOTSPOT_UUID
                {
                    status.mode = "client";
                } else if mode == 3
                    && configured_mode == "ap"
                    && status.profile_uuid == HOTSPOT_UUID
                    && status.address == HOTSPOT_ADDRESS
                {
                    status.mode = "hotspot";
                }
            }
        }
        Ok(Snapshot {
            status,
            failed: state == 120,
            reason,
        })
    }

    async fn address(&self, active: &str) -> Result<String> {
        for (property, interface) in [
            ("Ip4Config", "org.freedesktop.NetworkManager.IP4Config"),
            ("Ip6Config", "org.freedesktop.NetworkManager.IP6Config"),
        ] {
            let path: OwnedObjectPath = self.property(active, ACTIVE, property).await?;
            if path.as_str() == "/" {
                continue;
            }
            let addresses: Vec<Dict> = self
                .property(path.as_str(), interface, "AddressData")
                .await?;
            for address in addresses {
                if let Some(ip) = address
                    .get("address")
                    .and_then(|v| <&str>::try_from(v).ok())
                {
                    if usable_address(ip) {
                        return Ok(ip.to_owned());
                    }
                }
            }
        }
        Ok(String::new())
    }

    pub async fn scan(&self) -> Result<()> {
        self.call(
            self.device.as_str(),
            WIFI,
            "RequestScan",
            &HashMap::<String, OwnedValue>::new(),
        )
        .await
    }

    pub async fn networks(&self) -> Result<Vec<Network>> {
        let aps: Vec<OwnedObjectPath> = self
            .call(self.device.as_str(), WIFI, "GetAllAccessPoints", &())
            .await?;
        let mut networks: Vec<Network> = Vec::new();
        for ap in aps {
            let interface = "org.freedesktop.NetworkManager.AccessPoint";
            let ssid: Vec<u8> = self.property(ap.as_str(), interface, "Ssid").await?;
            if ssid.is_empty() {
                continue;
            }
            let strength: u8 = self.property(ap.as_str(), interface, "Strength").await?;
            let flags: u32 = self.property(ap.as_str(), interface, "Flags").await?;
            let wpa: u32 = self.property(ap.as_str(), interface, "WpaFlags").await?;
            let rsn: u32 = self.property(ap.as_str(), interface, "RsnFlags").await?;
            let security = if rsn & 0x400 != 0 {
                "sae"
            } else if (wpa | rsn) & 0x100 != 0 {
                "wpa-psk"
            } else if flags & 1 == 0 && wpa == 0 && rsn == 0 {
                "open"
            } else {
                "unsupported"
            };
            let item = Network {
                ssid,
                strength: strength.min(100),
                security,
            };
            if let Some(old) = networks
                .iter_mut()
                .find(|n| n.ssid == item.ssid && n.security == item.security)
            {
                if item.strength > old.strength {
                    *old = item;
                }
            } else {
                networks.push(item);
            }
        }
        networks.sort_by_key(|n| std::cmp::Reverse(n.strength));
        Ok(networks)
    }

    pub async fn checkpoints(&self) -> Result<Vec<OwnedObjectPath>> {
        // Do not interrupt an outstanding checkpoint after a core restart.
        let paths: Vec<OwnedObjectPath> = self.property(ROOT, NM, "Checkpoints").await?;
        let mut ours = Vec::new();
        for path in paths {
            let devices: Vec<OwnedObjectPath> = self
                .property(
                    path.as_str(),
                    "org.freedesktop.NetworkManager.Checkpoint",
                    "Devices",
                )
                .await?;
            if devices.contains(&self.device) {
                ours.push(path);
            }
        }
        Ok(ours)
    }

    pub async fn checkpoint(&self) -> Result<OwnedObjectPath> {
        // Native rollback also deletes profiles created during this transaction,
        // including a candidate persisted just before a core crash. All profiles
        // that existed when the checkpoint was created remain untouched.
        self.call(
            ROOT,
            NM,
            "CheckpointCreate",
            &(vec![self.device.clone()], 90u32, 2u32),
        )
        .await
    }

    pub async fn rollback(&self, checkpoint: &OwnedObjectPath) -> Result<()> {
        let results: HashMap<String, u32> = self
            .call(ROOT, NM, "CheckpointRollback", &(checkpoint,))
            .await?;
        if results.get(self.device.as_str()) == Some(&0) {
            Ok(())
        } else {
            Err("rollback-failed")
        }
    }

    pub async fn destroy(&self, checkpoint: &OwnedObjectPath) -> Result<()> {
        self.call(ROOT, NM, "CheckpointDestroy", &(checkpoint,))
            .await
    }

    pub async fn activate(&self, path: &OwnedObjectPath) -> Result<()> {
        let _: OwnedObjectPath = self
            .call(
                ROOT,
                NM,
                "ActivateConnection",
                &(
                    path,
                    &self.device,
                    zbus::zvariant::ObjectPath::from_static_str_unchecked("/"),
                ),
            )
            .await?;
        Ok(())
    }

    pub async fn delete(&self, path: &OwnedObjectPath) -> Result<()> {
        self.call(path.as_str(), CONNECTION, "Delete", &()).await
    }

    pub async fn version(&self, path: &OwnedObjectPath) -> Result<u64> {
        let version = self
            .property(path.as_str(), CONNECTION, "VersionId")
            .await?;
        if version == 0 {
            return Err("invalid-profile-version");
        }
        Ok(version)
    }

    pub async fn discard_candidate(&self, path: &OwnedObjectPath, uuid: &str) -> Result<()> {
        // Read version before settings. A late promotion must invalidate this
        // cleanup, including after the core restarts while NM authorizes it.
        let version = self.version(path).await?;
        let settings: Settings = self
            .call(path.as_str(), CONNECTION, "GetSettings", &())
            .await?;
        if text(&settings, "connection", "uuid") != uuid
            || text(&settings, "connection", "id") != format!("{CANDIDATE_PREFIX}{uuid}")
        {
            return Err("profile-changed");
        }
        // NM increments VersionId even for an empty successful Update2. Fence
        // any pending promotion; an ambiguous reply must never permit Delete.
        self.save(path, &Settings::new(), version).await?;
        self.delete(path).await
    }

    pub async fn add(&self, settings: &Settings, persist: bool) -> Result<OwnedObjectPath> {
        let (path, _): (OwnedObjectPath, Dict) = self
            .call(
                SETTINGS,
                "org.freedesktop.NetworkManager.Settings",
                "AddConnection2",
                &(settings, if persist { 1u32 } else { 2u32 }, Dict::new()),
            )
            .await?;
        Ok(path)
    }

    pub async fn save(
        &self,
        path: &OwnedObjectPath,
        settings: &Settings,
        version: u64,
    ) -> Result<()> {
        let _: Dict = self
            .call(
                path.as_str(),
                CONNECTION,
                "Update2",
                &(
                    settings,
                    1u32,
                    Dict::from([("version-id".into(), version.into())]),
                ),
            )
            .await?;
        Ok(())
    }

    pub async fn hotspot(&self) -> Result<()> {
        let paths: Vec<OwnedObjectPath> = self
            .call(
                SETTINGS,
                "org.freedesktop.NetworkManager.Settings",
                "ListConnections",
                &(),
            )
            .await?;
        for path in paths {
            let settings: Settings = self
                .call(path.as_str(), CONNECTION, "GetSettings", &())
                .await?;
            if text(&settings, "connection", "uuid") == HOTSPOT_UUID {
                if text(&settings, "802-11-wireless", "mode") != "ap" {
                    return Err("invalid-hotspot-profile");
                }
                return self.activate(&path).await;
            }
        }
        let machine = std::fs::read_to_string("/etc/machine-id").unwrap_or_default();
        let suffix: String = machine
            .trim()
            .chars()
            .rev()
            .take(6)
            .collect::<String>()
            .chars()
            .rev()
            .collect();
        let ssid = if suffix.is_empty() {
            "Nabaztag".to_owned()
        } else {
            format!("Nabaztag-{suffix}")
        };
        let mut settings =
            wifi_settings(HOTSPOT_UUID, "NabOS recovery", ssid.as_bytes(), "open", "");
        settings
            .get_mut("802-11-wireless")
            .unwrap()
            .insert("mode".into(), string("ap"));
        settings
            .get_mut("802-11-wireless")
            .unwrap()
            .insert("band".into(), string("bg"));
        settings.insert(
            "ipv4".into(),
            HashMap::from([
                ("method".into(), string("shared")),
                (
                    "address-data".into(),
                    owned(Value::from(vec![HashMap::from([
                        ("address", Value::from(HOTSPOT_ADDRESS)),
                        ("prefix", Value::from(24u32)),
                    ])])),
                ),
            ]),
        );
        settings.insert(
            "ipv6".into(),
            HashMap::from([("method".into(), string("disabled"))]),
        );
        let path = self.add(&settings, true).await?;
        self.activate(&path).await
    }
}

pub fn text<'a>(settings: &'a Settings, section: &str, key: &str) -> &'a str {
    settings
        .get(section)
        .and_then(|s| s.get(key))
        .and_then(|v| <&str>::try_from(v).ok())
        .unwrap_or("")
}

fn owned(value: Value<'_>) -> OwnedValue {
    value.try_to_owned().expect("owned network setting")
}
pub fn string(value: &str) -> OwnedValue {
    OwnedValue::from(Str::from(value))
}

pub fn wifi_settings(
    uuid: &str,
    id: &str,
    ssid: &[u8],
    security: &str,
    password: &str,
) -> Settings {
    let mut settings = HashMap::from([
        (
            "connection".into(),
            HashMap::from([
                ("id".into(), string(id)),
                ("uuid".into(), string(uuid)),
                ("type".into(), string("802-11-wireless")),
                ("autoconnect".into(), false.into()),
            ]),
        ),
        (
            "802-11-wireless".into(),
            HashMap::from([
                ("ssid".into(), owned(Value::from(ssid))),
                ("mode".into(), string("infrastructure")),
            ]),
        ),
        (
            "ipv4".into(),
            HashMap::from([("method".into(), string("auto"))]),
        ),
        (
            "ipv6".into(),
            HashMap::from([("method".into(), string("auto"))]),
        ),
    ]);
    if security != "open" {
        settings.insert(
            "802-11-wireless-security".into(),
            HashMap::from([
                ("key-mgmt".into(), string(security)),
                ("psk".into(), string(password)),
                ("psk-flags".into(), 0u32.into()),
            ]),
        );
    }
    settings
}

pub fn usable_address(address: &str) -> bool {
    match address.parse::<IpAddr>() {
        Ok(IpAddr::V4(ip)) => {
            !ip.is_unspecified()
                && !ip.is_loopback()
                && !ip.is_link_local()
                && !ip.is_multicast()
                && !ip.is_broadcast()
                && ip.octets()[0] != 0
        }
        Ok(IpAddr::V6(ip)) => {
            if let Some(v4) = ip.to_ipv4_mapped() {
                return usable_address(&v4.to_string());
            }
            !ip.is_unspecified()
                && !ip.is_loopback()
                && !ip.is_multicast()
                && !ip.is_unicast_link_local()
        }
        Err(_) => false,
    }
}
