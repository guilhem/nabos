//! Audio client. Processes, ownership cleanup and simulated playback live in device-core.

use super::Cancel;
use crate::device::{bounded, Device, CALL_TIMEOUT};
use serde::Deserialize;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::{oneshot, Mutex};
use zbus::zvariant::{OwnedValue, Type, Value};

#[derive(Debug, Clone, PartialEq)]
pub enum Source {
    File(PathBuf),
    Stream(String),
}

#[derive(Debug, Deserialize, Type, Value, OwnedValue)]
pub struct Status {
    pub id: String,
    pub state: String,
    pub volume: u32,
}

pub struct Player {
    device: Device,
    current: Mutex<Option<String>>,
}

impl Player {
    pub fn new(device: Device) -> Self {
        Self {
            device,
            current: Mutex::new(None),
        }
    }

    pub async fn start(self: &Arc<Self>, source: Source) -> Result<String, String> {
        let (reply, receiver) = oneshot::channel();
        let (accept, accepted) = oneshot::channel();
        let player = self.clone();
        tokio::spawn(async move {
            let result = player.start_inner(source).await;
            let id = result.as_ref().ok().cloned();
            // Aborting a choreography cannot discard an accepted but unclaimed ID.
            if reply.send(result).is_err() || accepted.await.is_err() {
                if let Some(id) = id {
                    if let Err(e) = player.stop(&id).await {
                        warn!("abandoned audio start cleanup failed: {e}");
                    }
                }
            }
        });
        let result = receiver.await.map_err(|_| "audio start task stopped")?;
        let _ = accept.send(());
        result
    }

    async fn start_inner(&self, source: Source) -> Result<String, String> {
        let (kind, source) = match source {
            Source::File(p) => (
                "file",
                p.into_os_string()
                    .into_string()
                    .map_err(|_| "non-UTF8 audio path")?,
            ),
            Source::Stream(u) => ("stream", u),
        };
        // Serialize starts so their replies cannot reverse the current playback ID.
        let mut current = self.current.lock().await;
        let proxy = self.device.proxy("Audio").await?;
        let id: String =
            match tokio::time::timeout(CALL_TIMEOUT, proxy.call("Start", &(kind, source))).await {
                Ok(result) => result.map_err(|e| e.to_string())?,
                Err(_) => {
                    // A lost reply may have started audio: disappearance of this owner
                    // makes the daemon reap it even though the ID is still unknown.
                    let _ = proxy.connection().clone().close().await;
                    return Err("device-core audio start timed out".into());
                }
            };
        if id.is_empty() {
            return Err("device-core returned an empty playback ID".into());
        }
        *current = Some(id.clone());
        Ok(id)
    }

    pub async fn stop(&self, id: &str) -> Result<(), String> {
        let proxy = self.device.proxy("Audio").await?;
        bounded(proxy.call::<_, _, ()>("Stop", &(id,))).await?;
        let mut current = self.current.lock().await;
        if current.as_deref() == Some(id) {
            *current = None;
        }
        Ok(())
    }

    pub async fn stop_current(&self) -> Result<(), String> {
        let id = self.current.lock().await.clone();
        match id {
            Some(id) => self.stop(&id).await,
            None => Ok(()),
        }
    }

    #[allow(dead_code)]
    pub async fn status(&self) -> Result<Status, String> {
        let proxy = self.device.proxy("Audio").await?;
        bounded(proxy.get_property("Status")).await
    }

    /// Wait for this ID, never whatever playback a later start has installed.
    pub async fn wait(&self, id: &str, cancel: &Cancel) -> Result<bool, String> {
        let proxy = self.device.proxy("Audio").await?;
        let args = (id,);
        tokio::select! {
            result = proxy.call::<_, _, String>("Wait", &args) => {
                match result.map_err(|e| e.to_string())?.as_str() {
                    "completed" | "stopped" | "preempted" => Ok(true),
                    outcome => Err(format!("audio playback {id}: {outcome}")),
                }
            }
            _ = cancel.wait() => {
                self.stop(id).await?;
                Ok(false)
            }
        }
    }

    pub async fn wait_current(&self, cancel: &Cancel) -> Result<bool, String> {
        let id = self.current.lock().await.clone();
        match id {
            Some(id) => self.wait(&id, cancel).await,
            None => Ok(true),
        }
    }

    pub async fn play_list(
        self: &Arc<Self>,
        files: &[Source],
        cancel: &Cancel,
    ) -> Result<bool, String> {
        self.stop_current().await?;
        for source in files {
            if cancel.is_cancelled() {
                return Ok(false);
            }
            let id = self.start(source.clone()).await?;
            if !self.wait(&id, cancel).await? {
                return Ok(false);
            }
        }
        Ok(true)
    }
}
