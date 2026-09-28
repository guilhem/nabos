//! Single audio slot. mp3 through mpg123 and wav through aplay, both on the
//! ALSA device (PipeWire's ALSA plugin by default). Starting a sound stops the
//! previous one.

use super::Cancel;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::{oneshot, watch};

pub struct Player {
    device: String,
    sim_ms: Option<u64>,
    state: Mutex<(u64, Option<oneshot::Sender<()>>)>,
    done: watch::Sender<u64>,
}

impl Player {
    pub fn new(device: String, sim_ms: Option<u64>) -> Player {
        Player {
            device,
            sim_ms,
            state: Mutex::new((0, None)),
            done: watch::channel(0).0,
        }
    }

    pub fn start(self: &Arc<Self>, path: PathBuf) {
        let (kill, killed) = oneshot::channel();
        let gen = {
            let mut s = self.state.lock().unwrap();
            if let Some(k) = s.1.take() {
                let _ = k.send(());
            }
            s.0 += 1;
            s.1 = Some(kill);
            s.0
        };
        let me = self.clone();
        tokio::spawn(async move {
            me.play(path, killed).await;
            me.done.send_modify(|d| *d = (*d).max(gen));
        });
    }

    async fn play(&self, path: PathBuf, killed: oneshot::Receiver<()>) {
        debug!("play {}", path.display());
        if let Some(ms) = self.sim_ms {
            tokio::select! {
                _ = tokio::time::sleep(Duration::from_millis(ms)) => {}
                _ = killed => {}
            }
            return;
        }
        let wav = path
            .extension()
            .is_some_and(|e| e.eq_ignore_ascii_case("wav"));
        let mut cmd = if wav {
            let mut c = tokio::process::Command::new("aplay");
            c.args(["-q", "-D", &self.device]);
            c
        } else {
            let mut c = tokio::process::Command::new("mpg123");
            c.args(["-q", "-o", "alsa", "-a", &self.device]);
            c
        };
        cmd.arg(&path)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .kill_on_drop(true);
        match cmd.spawn() {
            Ok(mut child) => tokio::select! {
                status = child.wait() => {
                    if let Ok(s) = status {
                        if !s.success() {
                            warn!("{} exited with {s}", path.display());
                        }
                    }
                }
                _ = killed => {
                    let _ = child.kill().await;
                }
            },
            Err(e) => error!("cannot start audio player for {}: {e}", path.display()),
        }
    }

    fn current(&self) -> u64 {
        self.state.lock().unwrap().0
    }

    /// Wait for the current sound. False when cancelled first.
    pub async fn wait_done(&self, cancel: &Cancel) -> bool {
        let gen = self.current();
        let mut rx = self.done.subscribe();
        tokio::select! {
            _ = rx.wait_for(|d| *d >= gen) => true,
            _ = cancel.wait() => false,
        }
    }

    pub async fn stop(&self) {
        let gen = {
            let mut s = self.state.lock().unwrap();
            if let Some(k) = s.1.take() {
                let _ = k.send(());
            }
            s.0
        };
        let mut rx = self.done.subscribe();
        let _ = rx.wait_for(|d| *d >= gen).await;
    }

    /// Play files in order; false when cancelled (sound stopped).
    pub async fn play_list(self: &Arc<Self>, files: &[PathBuf], cancel: &Cancel) -> bool {
        self.stop().await;
        for f in files {
            if cancel.is_cancelled() {
                return false;
            }
            self.start(f.clone());
            if !self.wait_done(cancel).await {
                self.stop().await;
                return false;
            }
        }
        true
    }
}
