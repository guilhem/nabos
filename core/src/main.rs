//! NabOS physical hardware service (see docs/hardware-dbus.md).

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
mod device;
mod hw;
mod maintenance;
mod network;
mod supervision;

pub struct Config {
    pub simulate: bool,
    pub gpio_chip: String,
    pub button_gpio: u32,
    pub ws2811_lib: String,
    pub led_brightness: u8,
    pub led_strip: String,
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

impl Config {
    fn from_env(simulate: bool) -> Config {
        Config {
            simulate,
            gpio_chip: env_or("NABOS_GPIO_CHIP", "/dev/gpiochip0"),
            button_gpio: env_parse("NABOS_BUTTON_GPIO", 17),
            ws2811_lib: env_or("NABOS_WS2811_LIB", "libws2811.so"),
            led_brightness: env_parse("NABOS_LED_BRIGHTNESS", 200),
            led_strip: env_or("NABOS_LED_STRIP", "grb"),
        }
    }
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "--help" || a == "-h") {
        println!("usage: nab-hardware [--simulate] [--version] [--stop-ears]\nConfiguration via NABOS_* variables, see docs/hardware-dbus.md");
        return;
    }
    if args.iter().any(|a| a == "--version") {
        println!("nab-hardware {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    let level = match env_or("NABOS_LOG", "info").as_str() {
        "error" => 0,
        "warn" => 1,
        "debug" => 3,
        _ => 2,
    };
    LOG_LEVEL.store(level, Ordering::Relaxed);
    if args.iter().any(|a| a == "--stop-ears") {
        if args.len() != 1 {
            error!("--stop-ears must be used alone");
            std::process::exit(2);
        }
        if let Err(e) = hw::ears::stop_all(&env_or("NABOS_GPIO_CHIP", "/dev/gpiochip0")) {
            error!("cannot turn ear motors off: {e}");
            std::process::exit(1);
        }
        return;
    }
    let cfg = Config::from_env(args.iter().any(|a| a == "--simulate"));
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("tokio runtime");
    rt.block_on(async move {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
        // Own the motor outputs low before D-Bus, LED or reader initialization
        // can block. Only bus::run starts calibration after systemd's barrier.
        let ears = hw::ears::Ears::open(cfg.simulate, &cfg.gpio_chip, tx.clone());
        let device = match device::Device::open(cfg.simulate).await {
            Ok(device) => device,
            Err(e) => {
                error!("{e}");
                std::process::exit(1);
            }
        };
        let presence = network::start(device.clone());
        let hw = tokio::task::spawn_blocking(move || {
            std::sync::Arc::new(hw::Hw::open(&cfg, tx, Some(presence), ears))
        })
        .await
        .expect("hardware initialization thread");
        let result = bus::run(device, bus::Hardware::new(hw.clone()), rx).await;
        hw.ears.request_stop();
        let stopped = hw.ears.shutdown().await;
        if let Err(e) = &result {
            error!("hardware service: {e}");
        }
        if let Err(e) = &stopped {
            error!("ear shutdown: {e}");
        }
        if result.is_err() || stopped.is_err() {
            std::process::exit(1);
        }
    });
}
