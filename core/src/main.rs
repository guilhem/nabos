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
    pub led_sysfs: String,
    pub led_brightness: u8,
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
            led_sysfs: env_or("NABOS_LED_SYSFS", "/sys/class/leds"),
            led_brightness: env_parse("NABOS_LED_BRIGHTNESS", 200),
        }
    }
}

async fn shutdown(hw: &hw::Hw) -> (Result<(), String>, Result<(), String>) {
    // Request motor stop before polling either cleanup; neither error may
    // short-circuit the other device's shutdown.
    hw.ears.request_stop();
    tokio::join!(hw.ears.shutdown(), hw.leds.clear())
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "--help" || a == "-h") {
        println!("usage: nab-hardware [--simulate] [--version] [--stop-hardware]\nConfiguration via NABOS_* variables, see docs/hardware-dbus.md");
        return;
    }
    if args.iter().any(|a| a == "--version") {
        println!("nab-hardware {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    if let Some(arg) = args
        .iter()
        .find(|a| !matches!(a.as_str(), "--simulate" | "--stop-hardware"))
    {
        error!("unknown argument: {arg}");
        std::process::exit(2);
    }
    let level = match env_or("NABOS_LOG", "info").as_str() {
        "error" => 0,
        "warn" => 1,
        "debug" => 3,
        _ => 2,
    };
    LOG_LEVEL.store(level, Ordering::Relaxed);
    if args.iter().any(|a| a == "--stop-hardware") {
        if args.len() != 1 {
            error!("--stop-hardware must be used alone");
            std::process::exit(2);
        }
        // Cut motor drive before any LED sysfs operation can block. Always
        // attempt both devices, without initializing D-Bus or the RFID reader.
        let ears = hw::ears::stop_all(&env_or("NABOS_GPIO_CHIP", "/dev/gpiochip0"));
        let leds = hw::leds::stop_all(&env_or("NABOS_LED_SYSFS", "/sys/class/leds"));
        if let Err(e) = &ears {
            error!("cannot turn ear motors off: {e}");
        }
        if let Err(e) = &leds {
            error!("cannot turn LEDs off: {e}");
        }
        if ears.is_err() || leds.is_err() {
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
                ears.request_stop();
                if let Err(e) = ears.shutdown().await {
                    error!("ear shutdown: {e}");
                }
                // Hardware workers have not been constructed yet, but a
                // previous service instance may have left a desired LED frame.
                if !cfg.simulate {
                    if let Err(e) = hw::leds::stop_all(&cfg.led_sysfs) {
                        error!("LED shutdown: {e}");
                    }
                }
                std::process::exit(1);
            }
        };
        let presence = network::start(device.clone());
        let hw = tokio::task::spawn_blocking(move || {
            std::sync::Arc::new(hw::Hw::open(&cfg, tx, Some(presence), ears))
        })
        .await
        .expect("hardware initialization thread");
        let hardware = bus::Hardware::new(hw.clone());
        let result = bus::run(device, hardware.clone(), rx).await;
        if result.is_err() {
            // Startup/runtime errors can bypass bus::run's normal drain.
            // Fence any still-exported API before attempting final cleanup.
            hw.ears.request_stop();
            // The final shutdown below owns Clear/sync; do not queue another
            // detached drain that could write after its completion barrier.
            hardware.fence(true);
        }
        let (stopped, cleared) = shutdown(&hw).await;
        if let Err(e) = &result {
            error!("hardware service: {e}");
        }
        if let Err(e) = &stopped {
            error!("ear shutdown: {e}");
        }
        if let Err(e) = &cleared {
            error!("LED shutdown: {e}");
        }
        if result.is_err() || stopped.is_err() || cleared.is_err() {
            std::process::exit(1);
        }
    });
}
