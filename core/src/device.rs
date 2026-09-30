//! Shared, persistent D-Bus connection to device-core. Mutations are never retried.

use std::sync::Arc;
use std::time::Duration;
use tokio::sync::Mutex;
use zbus::{Connection, Proxy};

pub const SERVICE: &str = "io.github.guilhem.DeviceCore1";
pub const ROOT: &str = "/io/github/guilhem/DeviceCore1";
pub const CALL_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Clone)]
pub struct Device {
    address: Option<String>,
    connection: Arc<Mutex<Option<Connection>>>,
}

impl Device {
    pub async fn open(simulate: bool) -> Result<Self, String> {
        let address = std::env::var("NABOS_DEVICE_BUS_ADDRESS")
            .ok()
            .filter(|s| !s.is_empty());
        if simulate {
            let address = address
                .as_deref()
                .ok_or("--simulate requires an explicit NABOS_DEVICE_BUS_ADDRESS")?;
            private_address(address)?;
        }
        let device = Self {
            address,
            connection: Arc::new(Mutex::new(None)),
        };
        if let Err(e) = device.connection().await {
            warn!("device-core bus unavailable at startup: {e}");
        }
        Ok(device)
    }

    #[cfg(test)]
    pub fn on_bus(address: String) -> Self {
        Self {
            address: Some(address),
            connection: Arc::new(Mutex::new(None)),
        }
    }

    pub async fn connection(&self) -> Result<Connection, String> {
        let mut slot = self.connection.lock().await;
        if let Some(c) = slot.as_ref().filter(|c| !c.is_closed()) {
            return Ok(c.clone());
        }
        let builder = match &self.address {
            Some(address) => zbus::connection::Builder::address(address.as_str()),
            None => zbus::connection::Builder::system(),
        }
        .map_err(|e| e.to_string())?;
        // Every device-core call has its own bounded wait; mutations are not retried.
        let connection = bounded(builder.build()).await?;
        *slot = Some(connection.clone());
        Ok(connection)
    }

    pub async fn proxy(&self, domain: &str) -> Result<Proxy<'static>, String> {
        let connection = self.connection().await?;
        zbus::proxy::Builder::new(&connection)
            .destination(SERVICE)
            .and_then(|b| b.path(format!("/io/github/guilhem/DeviceCore1/{domain}")))
            .and_then(|b| b.interface(format!("io.github.guilhem.DeviceCore1.{domain}")))
            .map_err(|e| e.to_string())?
            .cache_properties(zbus::proxy::CacheProperties::No)
            .build()
            .await
            .map_err(|e| e.to_string())
    }
}

pub async fn bounded<T, E: std::fmt::Display>(
    call: impl std::future::Future<Output = Result<T, E>>,
) -> Result<T, String> {
    tokio::time::timeout(CALL_TIMEOUT, call)
        .await
        .map_err(|_| "device-core call timed out".to_string())?
        .map_err(|e| e.to_string())
}

/// Refuse the host system bus even when explicitly supplied or reached by a
/// socket alias. Environment redirection identifies the test bus, not the host.
fn private_address(address: &str) -> Result<(), String> {
    use std::os::unix::fs::MetadataExt;
    use zbus::address::{transport::UnixSocket, Address, Transport};
    let address: Address = address.parse().map_err(|e: zbus::Error| e.to_string())?;
    if let Transport::Unix(socket) = address.transport() {
        if let UnixSocket::File(path) = socket.path() {
            let resolved = std::fs::canonicalize(path).unwrap_or_else(|_| path.clone());
            for system in [
                "/run/dbus/system_bus_socket",
                "/var/run/dbus/system_bus_socket",
            ] {
                if path == std::path::Path::new(system) || resolved == std::path::Path::new(system)
                {
                    return Err("--simulate cannot use the host system bus".into());
                }
                if let (Ok(candidate), Ok(host)) =
                    (std::fs::metadata(path), std::fs::metadata(system))
                {
                    if candidate.dev() == host.dev() && candidate.ino() == host.ino() {
                        return Err("--simulate cannot use the host system bus".into());
                    }
                }
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    #[test]
    fn simulation_rejects_explicit_host_system_bus() {
        for path in [
            "/run/dbus/system_bus_socket",
            "/var/run/dbus/system_bus_socket",
        ] {
            assert!(super::private_address(&format!("unix:path={path}")).is_err());
        }
        assert!(super::private_address("unix:path=/tmp/nab-hardware-private-bus-test").is_ok());
    }
}
