//! Authenticated maintenance reservations and recovery at hardware quiescence.

use crate::bus::Hardware;
use crate::device::{bounded, Device, ROOT, SERVICE};
use std::io::Read;
use std::sync::Arc;
use std::time::Duration;
use zbus::{fdo, message::Header, Connection, Proxy};

pub const PATH: &str = "/io/github/guilhem/DeviceCore1/Agent";

pub enum Operation {
    Acquire(String),
    Release(String),
    Abort(String),
}

pub struct Observation {
    pub connection: Option<Connection>,
    pub owner: String,
    pub safe: bool,
}

struct Reservation {
    owner: String,
    operation: String,
    token: String,
}

pub struct State {
    recovering: bool,
    held: Option<Reservation>,
    released: Option<(String, String)>,
}

impl Default for State {
    fn default() -> Self {
        Self {
            recovering: true,
            held: None,
            released: None,
        }
    }
}

impl State {
    pub fn blocked(&self) -> bool {
        self.recovering || self.held.is_some()
    }

    pub fn acquire(&mut self, owner: String, operation: String) -> fdo::Result<String> {
        if let Some(held) = &self.held {
            return if held.owner == owner && held.operation == operation {
                Ok(held.token.clone())
            } else {
                Err(fdo::Error::Failed("maintenance-held".into()))
            };
        }
        let mut random = [0u8; 32];
        std::fs::File::open("/dev/urandom")
            .and_then(|mut file| file.read_exact(&mut random))
            .map_err(|e| fdo::Error::Failed(e.to_string()))?;
        let token: String = random.iter().map(|b| format!("{b:02x}")).collect();
        self.held = Some(Reservation {
            owner,
            operation,
            token: token.clone(),
        });
        self.recovering = false;
        Ok(token)
    }

    pub fn release(&mut self, owner: &str, token: &str) -> fdo::Result<String> {
        if self.held.is_none()
            && self
                .released
                .as_ref()
                .is_some_and(|(o, t)| o == owner && t == token)
        {
            return Ok(String::new());
        }
        if !self
            .held
            .as_ref()
            .is_some_and(|h| h.owner == owner && h.token == token)
        {
            return Err(fdo::Error::AccessDenied("invalid-reservation".into()));
        }
        self.clear();
        Ok(String::new())
    }

    pub fn abort(&mut self, owner: &str, operation: &str) -> fdo::Result<String> {
        let Some(held) = &self.held else {
            return Ok(String::new());
        };
        if held.owner != owner || held.operation != operation {
            return Err(fdo::Error::AccessDenied("invalid-operation".into()));
        }
        self.clear();
        Ok(String::new())
    }

    fn clear(&mut self) {
        if let Some(held) = self.held.take() {
            self.released = Some((held.owner, held.token));
        }
        // All agents must release before the daemon opens its admission gate.
        self.recovering = true;
    }

    pub fn observe(&mut self, observation: Observation) {
        if let Some(held) = &self.held {
            if held.owner == observation.owner {
                if self.recovering && observation.safe {
                    self.clear();
                    self.recovering = false;
                }
                return;
            }
            // Disappearance does not prove that an accepted installation stopped.
            self.recovering = true;
        }
        if observation.safe {
            self.clear();
            self.recovering = false;
        } else {
            self.recovering = true;
        }
    }
}

pub struct Agent(pub Arc<Hardware>);

impl Agent {
    async fn request(
        &self,
        bus: &Connection,
        header: Header<'_>,
        operation: Operation,
    ) -> fdo::Result<String> {
        let sender = header
            .sender()
            .ok_or_else(|| fdo::Error::AccessDenied("missing-sender".into()))?;
        let argument = match &operation {
            Operation::Acquire(v) | Operation::Abort(v) | Operation::Release(v) => v,
        };
        if argument.is_empty() || argument.len() > 256 || argument.contains('\0') {
            return Err(fdo::Error::InvalidArgs(
                "invalid-maintenance-argument".into(),
            ));
        }
        self.0
            .maintenance_request(bus, sender.to_string(), operation)
            .await
    }
}

/// Fence a callback, after D-Bus awaits, against the
/// actual current daemon owner. An old authenticated request can become stale.
pub async fn authorize(bus: &Connection, sender: &str) -> fdo::Result<()> {
    let dbus = zbus::fdo::DBusProxy::new(bus).await?;
    let owner = bounded(dbus.get_name_owner(SERVICE.try_into().unwrap()))
        .await
        .map_err(|_| fdo::Error::AccessDenied("daemon-unavailable".into()))?;
    if owner.as_str() != sender {
        return Err(fdo::Error::AccessDenied("unauthorized-daemon".into()));
    }
    crate::bus::authorize_user(bus, sender, "NABOS_DEVICE_USER", "device-core").await?;
    let current = bounded(dbus.get_name_owner(SERVICE.try_into().unwrap()))
        .await
        .map_err(|_| fdo::Error::AccessDenied("daemon-unavailable".into()))?;
    if current.as_str() != sender {
        return Err(fdo::Error::AccessDenied("stale-daemon".into()));
    }
    Ok(())
}

#[zbus::interface(name = "io.github.guilhem.DeviceCore1.Agent")]
impl Agent {
    async fn acquire(
        &self,
        operation: String,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<String> {
        self.request(bus, header, Operation::Acquire(operation))
            .await
    }
    async fn abort(
        &self,
        operation: String,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        self.request(bus, header, Operation::Abort(operation))
            .await
            .map(|_| ())
    }
    async fn release(
        &self,
        token: String,
        #[zbus(connection)] bus: &Connection,
        #[zbus(header)] header: Header<'_>,
    ) -> fdo::Result<()> {
        self.request(bus, header, Operation::Release(token))
            .await
            .map(|_| ())
    }
}

pub fn start(device: Device, hardware: Arc<Hardware>) {
    tokio::spawn(async move {
        let mut exported: Option<Connection> = None;
        let mut registered = String::new();
        let mut last_error = String::new();
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tick.tick().await;
            let observation = async {
                let bus = device.connection().await?;
                if exported
                    .as_ref()
                    .is_none_or(|old| old.is_closed() || old.unique_name() != bus.unique_name())
                {
                    exported = Some(bus.clone());
                    registered.clear();
                }
                let dbus = zbus::fdo::DBusProxy::new(&bus)
                    .await
                    .map_err(|e| e.to_string())?;
                let owner = bounded(dbus.get_name_owner(SERVICE.try_into().unwrap()))
                    .await?
                    .to_string();
                authorize(&bus, &owner).await.map_err(|e| e.to_string())?;
                let manager = zbus::proxy::Builder::<Proxy>::new(&bus)
                    .destination(owner.clone())
                    .map_err(|e| e.to_string())?
                    .path(ROOT)
                    .map_err(|e| e.to_string())?
                    .interface("io.github.guilhem.DeviceCore1.Manager")
                    .map_err(|e| e.to_string())?
                    .cache_properties(zbus::proxy::CacheProperties::No)
                    .build()
                    .await
                    .map_err(|e| e.to_string())?;
                let capabilities: Vec<String> =
                    bounded(manager.get_property("Capabilities")).await?;
                if capabilities.iter().any(|c| c == "maintenance-agents") && registered != owner {
                    let path: zbus::zvariant::ObjectPath<'_> = PATH.try_into().unwrap();
                    bounded(manager.call::<_, _, ()>("RegisterAgent", &(path,))).await?;
                    authorize(&bus, &owner).await.map_err(|e| e.to_string())?;
                    registered = owner.clone();
                }
                let ready: bool = bounded(manager.get_property("Ready")).await?;
                let maintenance: bool = bounded(manager.get_property("Maintenance")).await?;
                Ok::<_, String>(Observation {
                    connection: Some(bus),
                    owner,
                    safe: ready && !maintenance,
                })
            }
            .await
            .unwrap_or_else(|e| {
                if last_error != e {
                    warn!("device-core maintenance unavailable; commands blocked: {e}");
                    last_error = e;
                }
                registered.clear();
                Observation {
                    connection: None,
                    owner: String::new(),
                    safe: false,
                }
            });
            if observation.connection.is_some() {
                if !last_error.is_empty() {
                    info!("device-core maintenance registration recovered");
                }
                last_error.clear();
            }
            hardware.observe(observation).await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reservation_is_idempotent_and_disappearance_keeps_the_barrier() {
        let mut state = State::default();
        let observe = |owner: &str, safe| Observation {
            connection: None,
            owner: owner.into(),
            safe,
        };
        assert!(state.blocked());
        state.observe(observe(":1.1", true));
        assert!(!state.blocked());
        let token = state.acquire(":1.1".into(), "update-a".into()).unwrap();
        assert_eq!(
            state.acquire(":1.1".into(), "update-a".into()).unwrap(),
            token
        );
        assert!(state.acquire(":1.1".into(), "update-b".into()).is_err());
        assert!(state.abort(":1.2", "update-a").is_err());
        assert!(state.abort(":1.1", "update-b").is_err());
        state.observe(observe(":1.1", true));
        assert!(
            state.blocked(),
            "observations cannot release an active reservation"
        );
        state.release(":1.1", &token).unwrap();
        state.release(":1.1", &token).unwrap();
        assert!(state.release(":1.2", &token).is_err());
        assert!(
            state.blocked(),
            "wait for all agents and daemon gate release"
        );
        state.observe(observe(":1.1", true));
        assert!(!state.blocked());
        state.acquire(":1.1".into(), "update-c".into()).unwrap();
        state.abort(":1.1", "update-c").unwrap();
        state.abort(":1.1", "update-c").unwrap();
        state.observe(observe(":1.1", true));
        state.acquire(":1.1".into(), "update-d".into()).unwrap();
        state.observe(observe("", false));
        assert!(state.blocked());
        state.observe(observe(":1.2", false));
        assert!(state.blocked());
        state.observe(observe(":1.2", true));
        assert!(!state.blocked());
    }
}
