//! MQTT 5 link to the local broker (docs/protocol-v1.md).

use crate::engine::Input;
use crate::protocol;
use crate::Config;
use rumqttc::v5::mqttbytes::v5::{ConnectProperties, Filter, LastWill, Packet};
use rumqttc::v5::mqttbytes::QoS;
use rumqttc::v5::{AsyncClient, Event, MqttOptions};
use serde_json::{json, Value};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc::UnboundedSender;

pub const CMD: &str = "nabos/v1/core/cmd";
pub const RESULT: &str = "nabos/v1/core/result";
pub const STATE: &str = "nabos/v1/core/state";
pub const AVAILABILITY: &str = "nabos/v1/core/availability";
pub const SETTINGS: &str = "nabos/v1/service/settings";

pub fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

pub fn now_f64() -> f64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs_f64())
        .unwrap_or(0.0)
}

#[derive(Clone)]
pub struct Bus {
    client: AsyncClient,
    last_state: Arc<Mutex<Option<Vec<u8>>>>,
}

impl Bus {
    pub fn start(cfg: &Config, tx: UnboundedSender<Input>) -> Bus {
        let mut opts = MqttOptions::new("nab-core", cfg.mqtt_host.clone(), cfg.mqtt_port);
        opts.set_keep_alive(Duration::from_secs(15));
        opts.set_clean_start(true);
        opts.set_last_will(LastWill::new(
            AVAILABILITY,
            "offline",
            QoS::AtLeastOnce,
            true,
            None,
        ));
        let mut props = ConnectProperties::new();
        props.session_expiry_interval = Some(0);
        props.max_packet_size = Some(protocol::MAX_PAYLOAD as u32 + 1024);
        opts.set_connect_properties(props);
        let (client, mut eventloop) = AsyncClient::new(opts, 256);
        let bus = Bus {
            client: client.clone(),
            last_state: Arc::new(Mutex::new(None)),
        };
        let b = bus.clone();
        tokio::spawn(async move {
            let mut connected = false;
            loop {
                match eventloop.poll().await {
                    Ok(Event::Incoming(Packet::ConnAck(_))) => {
                        connected = true;
                        info!("connected to MQTT broker");
                        // Retain As Published: a live retained command keeps its
                        // retain flag, so it can be refused like a stored one.
                        let mut cmd = Filter::new(CMD, QoS::AtLeastOnce);
                        cmd.preserve_retain = true;
                        let _ = client.try_subscribe_many([cmd]);
                        let _ = client.try_subscribe(SETTINGS, QoS::AtLeastOnce);
                        let _ = client.try_publish(AVAILABILITY, QoS::AtLeastOnce, true, "online");
                        if let Some(s) = b.last_state.lock().unwrap().clone() {
                            let _ = client.try_publish(STATE, QoS::AtLeastOnce, true, s);
                        }
                    }
                    Ok(Event::Incoming(Packet::Publish(p))) => {
                        if p.topic.as_ref() == CMD.as_bytes() {
                            if p.payload.is_empty() {
                                continue; // clearing of a retained command
                            }
                            let _ =
                                tx.send(Input::Cmd(protocol::parse(&p.payload, now()), p.retain));
                        } else if p.topic.as_ref() == SETTINGS.as_bytes() {
                            if let Ok(v) = serde_json::from_slice::<Value>(&p.payload) {
                                let _ = tx.send(Input::Settings(v));
                            }
                        }
                    }
                    Ok(_) => {}
                    Err(e) => {
                        if connected {
                            warn!("MQTT connection lost: {e}");
                            connected = false;
                        }
                        tokio::time::sleep(Duration::from_secs(1)).await;
                    }
                }
            }
        });
        bus
    }

    fn publish(&self, topic: &str, retain: bool, payload: Vec<u8>) {
        if let Err(e) = self
            .client
            .try_publish(topic, QoS::AtLeastOnce, retain, payload)
        {
            warn!("dropping publish on {topic}: {e}");
        }
    }

    pub fn result(&self, id: Option<&str>, status: &str, error: Option<&str>, data: Option<Value>) {
        debug!("result {id:?} {status} {error:?}");
        let v = json!({"v": 1, "id": id, "status": status, "error": error, "data": data});
        self.publish(RESULT, false, v.to_string().into_bytes());
    }

    pub fn state(&self, v: Value) {
        let payload = v.to_string().into_bytes();
        *self.last_state.lock().unwrap() = Some(payload.clone());
        self.publish(STATE, true, payload);
    }

    pub fn event(&self, kind: &str, mut v: Value) {
        v["v"] = json!(1);
        v["time"] = json!(now_f64());
        self.publish(
            &format!("nabos/v1/core/event/{kind}"),
            false,
            v.to_string().into_bytes(),
        );
    }

    /// Remove a retained command left on the command topic.
    pub fn clear_retained_cmd(&self) {
        self.publish(CMD, true, Vec::new());
    }

    pub async fn goodbye(&self) {
        self.publish(AVAILABILITY, true, b"offline".to_vec());
        tokio::time::sleep(Duration::from_millis(300)).await;
        let _ = self.client.try_disconnect();
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}
