//! Forward original GPIO CLOCK_MONOTONIC edges without blocking the reader.

use crate::device::{bounded, Device};
use std::time::Duration;
use tokio::sync::mpsc;

#[derive(Clone)]
pub struct Presence(mpsc::Sender<u64>);

impl Presence {
    pub fn press(&self, edge: Duration) {
        let Ok(edge) = u64::try_from(edge.as_nanos()) else {
            warn!("GPIO presence timestamp overflow");
            return;
        };
        if self.0.try_send(edge).is_err() {
            warn!("dropping GPIO presence: device-core channel full or closed");
        }
    }
}

pub fn start(device: Device) -> Presence {
    let (tx, mut rx) = mpsc::channel::<u64>(8);
    tokio::spawn(async move {
        while let Some(edge) = rx.recv().await {
            let result = async {
                let proxy = device.proxy("Network").await?;
                bounded(proxy.call::<_, _, ()>("ReportPresence", &(edge,))).await
            }
            .await;
            if let Err(e) = result {
                // Replaying an old physical confirmation after recovery is unsafe.
                warn!("device-core presence failed: {e}");
            }
        }
    });
    Presence(tx)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gpio_reports_are_bounded_and_keep_original_nanoseconds() {
        let (tx, mut rx) = mpsc::channel(8);
        let presence = Presence(tx);
        for ns in 123..132 {
            presence.press(Duration::from_nanos(ns));
        }
        for ns in 123..131 {
            assert_eq!(rx.try_recv().unwrap(), ns);
        }
        assert!(rx.try_recv().is_err());
        presence.press(Duration::MAX);
        assert!(rx.try_recv().is_err());
    }
}
