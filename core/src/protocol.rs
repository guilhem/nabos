//! Command envelope parsing and validation (docs/protocol-v1.md).

use serde::Deserialize;
use serde_json::Value;

pub const MAX_PAYLOAD: usize = 64 * 1024;
pub const MAX_FUTURE_SECS: u64 = 24 * 3600;
pub const STREAMING_URN: &str = "urn:x-chor:streaming";

/// Nabaztag RFID application identifiers.
pub const APPS: &[(u8, &str)] = &[
    (1, "eightball"),
    (2, "airquality"),
    (3, "scenario"),
    (4, "book"),
    (5, "clock"),
    (6, "mastodon"),
    (7, "surprise"),
    (8, "taichi"),
    (9, "weather"),
    (10, "ifttt"),
    (11, "airquality"),
    (12, "radio"),
    (13, "webhook"),
    (255, "none"),
];

pub fn app_name(id: u8) -> String {
    APPS.iter()
        .find(|(i, _)| *i == id)
        .map(|(_, n)| n.to_string())
        .unwrap_or_else(|| id.to_string())
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct Item {
    /// None: no "audio" key. Some(empty) still means "audio item" (an explicit audio item).
    pub audio: Option<Vec<String>>,
    /// Loopback stream served by nab-service (play only, exclusive with audio).
    pub stream: Option<String>,
    pub choreography: Option<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Animation {
    pub tempo: u32,
    /// Frames of [left, center, right] colors.
    pub frames: Vec<[[u8; 3]; 3]>,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Tech {
    St25tb,
    T2t,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum TestKind {
    Ears,
    Leds,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Action {
    Play {
        sequence: Vec<Item>,
        cancelable: bool,
    },
    Message {
        signature: Option<Item>,
        body: Vec<Item>,
        cancelable: bool,
    },
    Cancel {
        target: Option<String>,
    },
    Info {
        info_id: String,
        animation: Option<Animation>,
    },
    Indicator {
        animation: Option<Animation>,
    },
    Ears {
        left: Option<u8>,
        right: Option<u8>,
    },
    Sleep,
    Wakeup,
    RfidWrite {
        tech: Tech,
        uid: Vec<u8>,
        picture: u8,
        app: u8,
        data: Option<Vec<u8>>,
        timeout: u64,
    },
    Test(TestKind),
    Gestalt,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Command {
    pub id: String,
    pub expires_at: u64,
    pub action: Action,
}

/// Rejection: (command id if it could be read, reason).
pub type Rejection = (Option<String>, String);

#[derive(Deserialize)]
struct Envelope {
    v: u32,
    id: String,
    expires_at: u64,
    action: String,
    #[serde(default)]
    args: Value,
}

pub fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.:-".contains(&b))
}

/// Relative resource path, optionally ";"-separated alternatives.
pub fn valid_resource(spec: &str) -> bool {
    !spec.is_empty()
        && spec.len() <= 256
        && spec.split(';').all(|part| {
            !part.is_empty()
                && !part.starts_with('/')
                && !part.contains('\\')
                && !part.contains('\0')
                && part
                    .split('/')
                    .all(|c| !c.is_empty() && c != "." && c != "..")
        })
}

fn valid_choreography(c: &str) -> bool {
    if let Some(rest) = c.strip_prefix(STREAMING_URN) {
        return rest.is_empty()
            || rest
                .strip_prefix(':')
                .is_some_and(|n| n.parse::<u8>().is_ok_and(|n| n < 8));
    }
    valid_resource(c) && !c.contains(':')
}

/// Exactly `http://127.0.0.1:<port>/radio/<token>`: port 1024-65535 in plain
/// decimal, token 16-64 chars `[A-Za-z0-9_-]`, nothing else.
pub fn valid_stream(url: &str) -> bool {
    let Some((port, token)) = url
        .strip_prefix("http://127.0.0.1:")
        .and_then(|r| r.split_once("/radio/"))
    else {
        return false;
    };
    !port.starts_with('0')
        && port.len() <= 5
        && port.bytes().all(|b| b.is_ascii_digit())
        && port.parse::<u16>().is_ok_and(|p| p >= 1024)
        && (16..=64).contains(&token.len())
        && token
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn err<T>(msg: impl Into<String>) -> Result<T, String> {
    Err(msg.into())
}

fn obj(args: &Value) -> Result<&serde_json::Map<String, Value>, String> {
    match args {
        Value::Object(m) => Ok(m),
        Value::Null => {
            static EMPTY: std::sync::OnceLock<serde_json::Map<String, Value>> =
                std::sync::OnceLock::new();
            Ok(EMPTY.get_or_init(serde_json::Map::new))
        }
        _ => err("args must be an object"),
    }
}

fn opt_u8(m: &serde_json::Map<String, Value>, key: &str, max: u64) -> Result<Option<u8>, String> {
    match m.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(v) => match v.as_u64() {
            Some(n) if n <= max => Ok(Some(n as u8)),
            _ => err(format!("{key} must be an integer 0-{max}")),
        },
    }
}

fn opt_bool(m: &serde_json::Map<String, Value>, key: &str, default: bool) -> Result<bool, String> {
    match m.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(b)) => Ok(*b),
        _ => err(format!("{key} must be a boolean")),
    }
}

fn parse_item(v: &Value, allow_stream: bool) -> Result<Item, String> {
    let m = obj(v)?;
    let audio = match m.get("audio") {
        None => None,
        Some(Value::String(s)) => Some(vec![s.clone()]),
        Some(Value::Array(a)) => {
            if a.len() > 16 {
                return err("at most 16 audio resources per item");
            }
            Some(
                a.iter()
                    .map(|x| {
                        x.as_str()
                            .map(str::to_string)
                            .ok_or("audio entries must be strings")
                    })
                    .collect::<Result<Vec<_>, _>>()?,
            )
        }
        Some(Value::Null) => None,
        _ => return err("audio must be a list of resources"),
    };
    if let Some(a) = &audio {
        if let Some(bad) = a.iter().find(|r| !valid_resource(r)) {
            return err(format!("invalid audio resource {bad:?}"));
        }
    }
    let stream = match m.get("stream") {
        None | Some(Value::Null) => None,
        Some(_) if !allow_stream => return err("stream is only allowed in play"),
        Some(_) if audio.is_some() => return err("stream and audio are exclusive"),
        Some(Value::String(s)) if valid_stream(s) => Some(s.clone()),
        Some(other) => return err(format!("invalid stream {other}")),
    };
    let choreography = match m.get("choreography") {
        None | Some(Value::Null) => None,
        Some(Value::String(s)) if valid_choreography(s) => Some(s.clone()),
        Some(other) => return err(format!("invalid choreography {other}")),
    };
    Ok(Item {
        audio,
        stream,
        choreography,
    })
}

fn parse_items(v: Option<&Value>, key: &str, allow_stream: bool) -> Result<Vec<Item>, String> {
    match v {
        Some(Value::Array(a)) if a.len() <= 32 => {
            a.iter().map(|i| parse_item(i, allow_stream)).collect()
        }
        Some(Value::Array(_)) => err(format!("{key}: at most 32 items")),
        _ => err(format!("{key} must be a list")),
    }
}

fn parse_color(v: Option<&Value>) -> Result<[u8; 3], String> {
    match v {
        None | Some(Value::Null) => Ok([0, 0, 0]),
        Some(Value::String(s)) if s.is_empty() => Ok([0, 0, 0]),
        // Short hexadecimal strings are interpreted as left-padded RGB values.
        Some(Value::String(s)) if s.len() <= 6 && s.bytes().all(|b| b.is_ascii_hexdigit()) => {
            let n = u32::from_str_radix(s, 16).map_err(|e| e.to_string())?;
            Ok([(n >> 16) as u8, (n >> 8) as u8, n as u8])
        }
        Some(other) => err(format!("invalid color {other}")),
    }
}

pub fn parse_animation(v: Option<&Value>) -> Result<Option<Animation>, String> {
    let m = match v {
        None | Some(Value::Null) => return Ok(None),
        Some(v) => obj(v)?,
    };
    let tempo = match m.get("tempo").and_then(Value::as_f64) {
        Some(t) if (1.0..=1000.0).contains(&t) => t.round() as u32,
        _ => return err("animation.tempo must be a number 1-1000"),
    };
    let colors = match m.get("colors") {
        Some(Value::Array(a)) if !a.is_empty() && a.len() <= 64 => a,
        _ => return err("animation.colors must be a list of 1-64 frames"),
    };
    let frames = colors
        .iter()
        .map(|c| {
            let f = obj(c)?;
            Ok([
                parse_color(f.get("left"))?,
                parse_color(f.get("center"))?,
                parse_color(f.get("right"))?,
            ])
        })
        .collect::<Result<Vec<_>, String>>()?;
    Ok(Some(Animation { tempo, frames }))
}

fn parse_uid(s: &str, tech: Tech) -> Result<Vec<u8>, String> {
    let hex: String = s.chars().filter(|c| *c != ':').collect();
    if !hex.len().is_multiple_of(2) || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return err("uid must be hexadecimal");
    }
    let uid: Vec<u8> = (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
        .collect();
    let ok = match tech {
        Tech::St25tb => uid.len() == 8,
        Tech::T2t => (4..=10).contains(&uid.len()),
    };
    if ok {
        Ok(uid)
    } else {
        err("uid has an invalid length for this technology")
    }
}

fn parse_action(action: &str, args: &Value) -> Result<Action, String> {
    let m = obj(args)?;
    Ok(match action {
        "play" => Action::Play {
            sequence: parse_items(m.get("sequence"), "sequence", true)?,
            cancelable: opt_bool(m, "cancelable", true)?,
        },
        "message" => Action::Message {
            signature: match m.get("signature") {
                None | Some(Value::Null) => None,
                Some(s) => Some(parse_item(s, false)?),
            },
            body: parse_items(m.get("body"), "body", false)?,
            cancelable: opt_bool(m, "cancelable", true)?,
        },
        "cancel" => Action::Cancel {
            target: match m.get("target") {
                None | Some(Value::Null) => None,
                Some(Value::String(t)) if valid_id(t) => Some(t.clone()),
                _ => return err("invalid target"),
            },
        },
        "info" => {
            let info_id = match m.get("info_id") {
                Some(Value::String(s)) if !s.is_empty() && s.len() <= 64 => s.clone(),
                _ => return err("info_id must be a string of 1-64 chars"),
            };
            Action::Info {
                info_id,
                animation: parse_animation(m.get("animation"))?,
            }
        }
        "indicator" => Action::Indicator {
            animation: parse_animation(m.get("animation"))?,
        },
        "ears" => Action::Ears {
            left: opt_u8(m, "left", 16)?,
            right: opt_u8(m, "right", 16)?,
        },
        "sleep" => Action::Sleep,
        "wakeup" => Action::Wakeup,
        "gestalt" => Action::Gestalt,
        "test" => Action::Test(match m.get("test").and_then(Value::as_str) {
            Some("ears") => TestKind::Ears,
            Some("leds") => TestKind::Leds,
            _ => return err("test must be ears or leds"),
        }),
        "rfid_write" => {
            let tech = match m.get("tech").and_then(Value::as_str) {
                Some("st25tb") => Tech::St25tb,
                Some("iso14443a_t2t") => Tech::T2t,
                _ => return err("tech must be st25tb or iso14443a_t2t"),
            };
            let uid = parse_uid(
                m.get("uid")
                    .and_then(Value::as_str)
                    .ok_or("uid is required")?,
                tech,
            )?;
            let picture = opt_u8(m, "picture", 255)?.ok_or("picture is required")?;
            let app = match m.get("app") {
                Some(Value::String(s)) => APPS
                    .iter()
                    .find(|(_, n)| n == s)
                    .map(|(i, _)| *i)
                    .ok_or("unknown app")?,
                Some(v) => match v.as_u64() {
                    Some(n) if n <= 255 => n as u8,
                    _ => return err("app must be a name or 0-255"),
                },
                None => return err("app is required"),
            };
            let data = match m.get("data") {
                None | Some(Value::Null) => None,
                Some(Value::String(s)) if s.len() <= 32 => Some(s.as_bytes().to_vec()),
                _ => return err("data must be a string of at most 32 bytes"),
            };
            let timeout = match m.get("timeout") {
                None | Some(Value::Null) => 20,
                Some(v) => match v.as_f64() {
                    Some(t) if (1.0..=60.0).contains(&t) => t.ceil() as u64,
                    _ => return err("timeout must be 1-60 seconds"),
                },
            };
            Action::RfidWrite {
                tech,
                uid,
                picture,
                app,
                data,
                timeout,
            }
        }
        other => return err(format!("unknown action {other:?}")),
    })
}

/// Parse a command. Expiration in the past is not an error here: the engine
/// answers "expired" with the id.
pub fn parse(payload: &[u8], now: u64) -> Result<Command, Rejection> {
    if payload.len() > MAX_PAYLOAD {
        return Err((None, "payload too large".into()));
    }
    let env: Envelope = serde_json::from_slice(payload).map_err(|e| {
        let id = serde_json::from_slice::<Value>(payload)
            .ok()
            .and_then(|v| v.get("id").and_then(Value::as_str).map(str::to_string))
            .filter(|id| valid_id(id));
        (id, format!("malformed envelope: {e}"))
    })?;
    if !valid_id(&env.id) {
        return Err((None, "invalid id".into()));
    }
    let id = Some(env.id.clone());
    if env.v != 1 {
        return Err((id, format!("unsupported version {}", env.v)));
    }
    if env.expires_at > now + MAX_FUTURE_SECS {
        return Err((id, "expires_at too far in the future".into()));
    }
    let action = parse_action(&env.action, &env.args).map_err(|e| (id.clone(), e))?;
    Ok(Command {
        id: env.id,
        expires_at: env.expires_at,
        action,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cmd(json: &str) -> Result<Command, Rejection> {
        parse(json.as_bytes(), 1000)
    }

    #[test]
    fn envelope_and_bounds() {
        let ok = cmd(r#"{"v":1,"id":"a1","expires_at":1060,"action":"play","args":{"sequence":[{"audio":["clock/7/*.mp3"],"choreography":"urn:x-chor:streaming:3"}]}}"#).unwrap();
        assert_eq!(ok.id, "a1");
        assert!(matches!(
            ok.action,
            Action::Play {
                cancelable: true,
                ..
            }
        ));
        // traversal, absolute paths and data URIs are refused
        for bad in ["../etc/passwd", "/etc/passwd", "a/../b", "a;/b", "x\\y"] {
            let j = format!(
                r#"{{"v":1,"id":"b","expires_at":1060,"action":"play","args":{{"sequence":[{{"audio":["{bad}"]}}]}}}}"#
            );
            assert!(cmd(&j).is_err(), "{bad}");
        }
        assert!(cmd(r#"{"v":1,"id":"c","expires_at":1060,"action":"play","args":{"sequence":[{"choreography":"data:application/x-nabaztag-mtl-choreography;base64,AA"}]}}"#).is_err());
        assert_eq!(
            cmd(r#"{"v":2,"id":"d","expires_at":1060,"action":"sleep"}"#)
                .unwrap_err()
                .0
                .as_deref(),
            Some("d")
        );
        assert!(cmd(r#"{"v":1,"id":"e","expires_at":999999,"action":"sleep"}"#).is_err());
        assert!(cmd(r#"{"v":1,"id":"bad id","expires_at":1060,"action":"sleep"}"#).is_err());
        assert!(
            cmd(r#"{"v":1,"id":"f","expires_at":1060,"action":"ears","args":{"left":17}}"#)
                .is_err()
        );
        assert!(cmd(r#"{"v":1,"id":"g","expires_at":1060,"action":"reboot"}"#).is_err());
        // past expiration parses (engine answers "expired")
        assert!(cmd(r#"{"v":1,"id":"h","expires_at":10,"action":"wakeup"}"#).is_ok());
    }

    #[test]
    fn streams() {
        let play = |item: &str| {
            cmd(&format!(
                r#"{{"v":1,"id":"s","expires_at":1060,"action":"play","args":{{"sequence":[{item}]}}}}"#
            ))
        };
        let url = "http://127.0.0.1:40123/radio/Ab_-0123456789xyz";
        let ok = play(&format!(
            r#"{{"stream":"{url}","choreography":"urn:x-chor:streaming"}}"#
        ))
        .unwrap();
        let Action::Play { sequence, .. } = ok.action else {
            panic!()
        };
        assert_eq!(sequence[0].stream.as_deref(), Some(url));
        assert_eq!(sequence[0].audio, None);
        for bad in [
            "https://127.0.0.1:40123/radio/Ab_-0123456789xyz",
            "http://localhost:40123/radio/Ab_-0123456789xyz",
            "http://127.0.0.1/radio/Ab_-0123456789xyz",
            "http://127.0.0.1:80/radio/Ab_-0123456789xyz",
            "http://127.0.0.1:65536/radio/Ab_-0123456789xyz",
            "http://127.0.0.1:+4012/radio/Ab_-0123456789xyz",
            "http://127.0.0.1:04012/radio/Ab_-0123456789xyz",
            "http://127.0.0.1:40123/radio/short",
            "http://127.0.0.1:40123/radio/Ab_-0123456789xyz/x",
            "http://127.0.0.1:40123/radio/Ab_-0123456789xyz?x=1",
            "http://127.0.0.1:40123/other/Ab_-0123456789xyz",
            "http://example.com:40123/radio/Ab_-0123456789xyz",
        ] {
            assert!(play(&format!(r#"{{"stream":"{bad}"}}"#)).is_err(), "{bad}");
        }
        assert!(valid_stream(&format!(
            "http://127.0.0.1:65535/radio/{}",
            "a".repeat(64)
        )));
        assert!(!valid_stream(&format!(
            "http://127.0.0.1:65535/radio/{}",
            "a".repeat(65)
        )));
        // exclusive with audio, even an empty audio list
        assert!(play(&format!(r#"{{"stream":"{url}","audio":[]}}"#)).is_err());
        // never in a message
        for args in [
            format!(r#"{{"body":[{{"stream":"{url}"}}]}}"#),
            format!(r#"{{"signature":{{"stream":"{url}"}},"body":[]}}"#),
        ] {
            assert!(cmd(&format!(
                r#"{{"v":1,"id":"m","expires_at":1060,"action":"message","args":{args}}}"#
            ))
            .is_err());
        }
    }

    #[test]
    fn animations_and_rfid() {
        let a = parse_animation(Some(&serde_json::json!({"tempo":16,"colors":[{"left":"00000","center":"003399","right":"ffffff"}]}))).unwrap().unwrap();
        assert_eq!(a.frames[0], [[0, 0, 0], [0, 0x33, 0x99], [255, 255, 255]]);
        let c = cmd(r#"{"v":1,"id":"r","expires_at":1060,"action":"rfid_write","args":{"tech":"st25tb","uid":"d0:02:18:01:02:03:04:05","picture":3,"app":"clock","data":"\u0001"}}"#).unwrap();
        assert_eq!(
            c.action,
            Action::RfidWrite {
                tech: Tech::St25tb,
                uid: vec![0xd0, 2, 0x18, 1, 2, 3, 4, 5],
                picture: 3,
                app: 5,
                data: Some(vec![1]),
                timeout: 20
            }
        );
        assert!(cmd(r#"{"v":1,"id":"r","expires_at":1060,"action":"rfid_write","args":{"tech":"st25tb","uid":"d0:02","picture":3,"app":5}}"#).is_err());
    }
}
