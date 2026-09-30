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
        if simulate && address.is_none() {
            return Err("--simulate requires an explicit NABOS_DEVICE_BUS_ADDRESS".into());
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
        // No connection-wide method timeout: Audio.Wait may last for a radio stream.
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
