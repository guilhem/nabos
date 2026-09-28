//! nab-core: NabOS hardware core, driven over MQTT 5 (see docs/protocol-v1.md).

use std::sync::atomic::{AtomicU8, Ordering};

static LOG_LEVEL: AtomicU8 = AtomicU8::new(2);

pub fn logline(level: u8, msg: String) {
    if level <= LOG_LEVEL.load(Ordering::Relaxed) {
        // "<N>" prefixes are journald priorities.
        eprintln!("<{}>{}", [3, 4, 6, 7][level as usize], msg);
    }
}

#[macro_export]
macro_rules! error { ($($a:tt)*) => { $crate::logline(0, format!($($a)*)) } }
#[macro_export]
macro_rules! warn { ($($a:tt)*) => { $crate::logline(1, format!($($a)*)) } }
#[macro_export]
macro_rules! info { ($($a:tt)*) => { $crate::logline(2, format!($($a)*)) } }
#[macro_export]
macro_rules! debug { ($($a:tt)*) => { $crate::logline(3, format!($($a)*)) } }

mod bus;
mod chor;
mod engine;
mod hw;
mod playback;
mod protocol;
mod resources;

use std::path::PathBuf;
use std::sync::Arc;

pub struct Config {
    pub simulate: bool,
    pub mqtt_host: String,
    pub mqtt_port: u16,
    pub sounds_dirs: Vec<PathBuf>,
    pub chor_dirs: Vec<PathBuf>,
    pub alsa_device: String,
    pub gpio_chip: String,
    pub button_gpio: u32,
    pub ws2811_lib: String,
    pub led_brightness: u8,
    pub led_strip: String,
    pub sim_audio_ms: u64,
}

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name)
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| default.to_string())
}

fn env_parse<T: std::str::FromStr>(name: &str, default: T) -> T {
    match std::env::var(name) {
        Ok(v) if !v.is_empty() => v.parse().unwrap_or_else(|_| {
            warn!("invalid {name}={v}, using default");
            default
        }),
        _ => default,
    }
}

fn dirs(name: &str, default: &str) -> Vec<PathBuf> {
    env_or(name, default)
        .split(':')
        .filter(|s| !s.is_empty())
        .map(PathBuf::from)
        .collect()
}

impl Config {
    fn from_env(simulate: bool) -> Config {
        Config {
            simulate,
            mqtt_host: env_or("NABOS_MQTT_HOST", "127.0.0.1"),
            mqtt_port: env_parse("NABOS_MQTT_PORT", 1883),
            sounds_dirs: dirs(
                "NABOS_SOUNDS_DIRS",
                "/usr/share/nabos/sounds:/data/nabos/media/sounds",
            ),
            chor_dirs: dirs(
                "NABOS_CHOREOGRAPHIES_DIRS",
                "/usr/share/nabos/choreographies:/data/nabos/media/choreographies",
            ),
            alsa_device: env_or("NABOS_ALSA_DEVICE", "default"),
            gpio_chip: env_or("NABOS_GPIO_CHIP", "/dev/gpiochip0"),
            button_gpio: env_parse("NABOS_BUTTON_GPIO", 17),
            ws2811_lib: env_or("NABOS_WS2811_LIB", "libws2811.so"),
            led_brightness: env_parse("NABOS_LED_BRIGHTNESS", 200),
            led_strip: env_or("NABOS_LED_STRIP", "grb"),
            sim_audio_ms: env_parse("NABOS_SIM_AUDIO_MS", 50),
        }
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "--help" || a == "-h") {
        println!("usage: nab-core [--simulate] [--version]\nConfiguration via NABOS_* variables, see docs/protocol-v1.md");
        return;
    }
    if args.iter().any(|a| a == "--version") {
        println!("nab-core {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    let level = match env_or("NABOS_LOG", "info").as_str() {
        "error" => 0,
        "warn" => 1,
        "debug" => 3,
        _ => 2,
    };
    LOG_LEVEL.store(level, Ordering::Relaxed);
    let cfg = Config::from_env(args.iter().any(|a| a == "--simulate"));
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("tokio runtime");
    rt.block_on(async move {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        let hw = Arc::new(hw::Hw::open(&cfg, tx.clone()));
        let bus = bus::Bus::start(&cfg, tx.clone());
        let shutdown = tx.clone();
        tokio::spawn(async move {
            let mut term =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                    .expect("signal");
            tokio::select! {
                _ = term.recv() => {}
                _ = tokio::signal::ctrl_c() => {}
            }
            let _ = shutdown.send(engine::Input::Shutdown);
        });
        info!(
            "nab-core {} starting (simulate={})",
            env!("CARGO_PKG_VERSION"),
            cfg.simulate
        );
        engine::Engine::new(hw, bus.clone(), tx).run(rx).await;
        bus.goodbye().await;
    });
}
