//! Five Linux multicolor LED class devices. Software pulsing uses a 100 ms
//! period and 10 steps; ordinary writes accept work asynchronously. Only clear
//! waits for the controller-wide sysfs sync barrier.

use super::Cancel;
use crate::Config;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::{
    atomic::{AtomicBool, Ordering},
    mpsc, Arc,
};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;

pub const COUNT: usize = 5;

pub type Rgb = [u8; 3];

const PULSING_RATE: Duration = Duration::from_millis(100);
const PULSING_STEPS: f32 = 10.0;

enum Cmd {
    Set(
        Vec<(usize, Rgb)>,
        Cancel,
        oneshot::Sender<Result<(), String>>,
    ),
    Pulse(usize, Rgb, Cancel, oneshot::Sender<Result<(), String>>),
    Clear(oneshot::Sender<Result<(), String>>),
}

pub struct Leds {
    tx: mpsc::Sender<Cmd>,
    ok: Arc<AtomicBool>,
}

impl Leds {
    pub fn open(cfg: &Config) -> Leds {
        let (tx, rx) = mpsc::channel();
        let (ready_tx, ready_rx) = mpsc::channel();
        let ok = Arc::new(AtomicBool::new(false));
        let available = ok.clone();
        let sim = cfg.simulate;
        let root = cfg.led_sysfs.clone();
        let brightness = cfg.led_brightness;
        std::thread::spawn(move || {
            let mut backend = if sim {
                None
            } else {
                match Sysfs::open(&root, brightness) {
                    Ok(s) => Some(s),
                    Err(e) => {
                        error!("LEDs unavailable: {e}");
                        None
                    }
                }
            };
            available.store(sim || backend.is_some(), Ordering::Relaxed);
            let _ = ready_tx.send(());
            run(rx, |frame, clear| {
                let result = if sim {
                    Ok(())
                } else if clear {
                    stop_all(&root)
                } else if let Some(s) = backend.as_mut() {
                    s.show(frame)
                } else {
                    Err("leds-unavailable".into())
                };
                available.store(
                    result.is_ok() && (sim || backend.is_some()),
                    Ordering::Relaxed,
                );
                result
            });
        });
        let _ = ready_rx.recv();
        Leds { tx, ok }
    }

    pub fn available(&self) -> bool {
        self.ok.load(Ordering::Relaxed)
    }

    pub async fn set(&self, colors: Vec<(usize, Rgb)>, cancel: Cancel) -> Result<(), String> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Set(colors, cancel, reply))
            .map_err(|_| "leds-stopped")?;
        rx.await.map_err(|_| "leds-stopped")?
    }
    pub async fn pulse(&self, led: usize, rgb: Rgb, cancel: Cancel) -> Result<(), String> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Pulse(led, rgb, cancel, reply))
            .map_err(|_| "leds-stopped")?;
        rx.await.map_err(|_| "leds-stopped")?
    }
    pub async fn clear(&self) -> Result<(), String> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Clear(reply))
            .map_err(|_| "leds-stopped")?;
        rx.await.map_err(|_| "leds-stopped")?
    }
}

struct Pulse {
    target: [f32; 3],
    current: [f32; 3],
    up: bool,
}

fn run(rx: mpsc::Receiver<Cmd>, mut show: impl FnMut(&[u32; COUNT], bool) -> Result<(), String>) {
    let mut frame = [0u32; COUNT];
    let mut pulses: [Option<Pulse>; COUNT] = Default::default();
    let mut next_pulse: Option<Instant> = None;
    let pack = |c: [f32; 3]| ((c[0] as u32) << 16) | ((c[1] as u32) << 8) | c[2] as u32;
    loop {
        let cmd = match next_pulse {
            Some(t) => match rx.recv_timeout(t.saturating_duration_since(Instant::now())) {
                Ok(c) => Some(c),
                Err(mpsc::RecvTimeoutError::Timeout) => None,
                Err(mpsc::RecvTimeoutError::Disconnected) => return,
            },
            None => match rx.recv() {
                Ok(c) => Some(c),
                Err(_) => return,
            },
        };
        let mut pending: Vec<Cmd> = cmd.into_iter().collect();
        pending.extend(rx.try_iter());
        let mut changed = false;
        let mut replies: Vec<(oneshot::Sender<Result<(), String>>, bool)> = Vec::new();
        for c in pending {
            let (reply, admitted) = match c {
                Cmd::Set(colors, cancel, reply) => {
                    let admitted = !reply.is_closed() && cancel.admit();
                    if admitted {
                        for (led, [r, g, b]) in colors {
                            pulses[led] = None;
                            frame[led] = ((r as u32) << 16) | ((g as u32) << 8) | b as u32;
                        }
                    }
                    (reply, admitted)
                }
                Cmd::Pulse(led, [r, g, b], cancel, reply) => {
                    let admitted = !reply.is_closed() && cancel.admit();
                    if admitted {
                        frame[led] = 0;
                        pulses[led] = Some(Pulse {
                            target: [r as f32, g as f32, b as f32],
                            current: [0.0; 3],
                            up: true,
                        });
                        next_pulse.get_or_insert_with(Instant::now);
                    }
                    (reply, admitted)
                }
                Cmd::Clear(reply) => {
                    frame = [0; COUNT];
                    pulses = Default::default();
                    next_pulse = None;
                    // Keep the barrier at its position in the queue: a later
                    // Set must not restore brightness before clear completes.
                    let result = show(&frame, true);
                    for (reply, admitted) in replies.drain(..) {
                        let _ = reply.send(if admitted {
                            result.clone()
                        } else {
                            Err("canceled".into())
                        });
                    }
                    let _ = reply.send(result);
                    changed = false;
                    continue;
                }
            };
            changed |= admitted;
            replies.push((reply, admitted));
        }
        if pulses.iter().all(Option::is_none) {
            next_pulse = None;
        } else if next_pulse.is_some_and(|t| Instant::now() >= t) {
            for (led, p) in pulses.iter_mut().enumerate() {
                let Some(p) = p else { continue };
                let reached = (0..3).all(|i| p.current[i] as u32 == p.target[i] as u32);
                let dark = p.current.iter().all(|c| *c as u32 == 0);
                if p.up && reached {
                    p.up = false;
                } else if !p.up && dark {
                    p.up = true;
                }
                for i in 0..3 {
                    let incr = p.target[i] / PULSING_STEPS;
                    p.current[i] = if p.up {
                        (p.current[i] + incr).min(p.target[i])
                    } else {
                        (p.current[i] - incr).max(0.0)
                    };
                }
                frame[led] = pack(p.current);
            }
            changed = true;
            next_pulse = next_pulse.map(|t| t + PULSING_RATE);
        }
        let rendered = if changed { show(&frame, false) } else { Ok(()) };
        for (reply, admitted) in replies {
            let _ = reply.send(if admitted {
                rendered.clone()
            } else {
                Err("canceled".into())
            });
        }
    }
}

fn led_dir(root: &Path, index: usize) -> PathBuf {
    root.join(format!("multi:indicator-{index}"))
}

fn write(path: &Path, value: &str) -> Result<(), String> {
    // No create: a missing kernel attribute must fail, including in fixtures.
    OpenOptions::new()
        .write(true)
        .truncate(true)
        .open(path)
        .and_then(|mut file| file.write_all(value.as_bytes()))
        .map_err(|e| format!("{}: {e}", path.display()))
}

/// Recovery and maintenance share the same shutdown barrier. Attempt every
/// LED even after an error, then always sync the whole controller on LED zero.
/// The kernel bounds each transfer to 100 ms and propagates transfer errors.
pub fn stop_all(root: &str) -> Result<(), String> {
    let root = Path::new(root);
    let mut errors = Vec::new();
    for index in 0..COUNT {
        if let Err(e) = write(&led_dir(root, index).join("brightness"), "0\n") {
            errors.push(e);
        }
    }
    if let Err(e) = write(&led_dir(root, 0).join("sync"), "1\n") {
        errors.push(e);
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(errors.join("; "))
    }
}

struct Sysfs {
    root: PathBuf,
    order: [[usize; 3]; COUNT],
    brightness: u8,
}

impl Sysfs {
    fn open(root: &str, brightness: u8) -> Result<Self, String> {
        // Clear even if discovery later fails; never advertise availability
        // until both this barrier and all native attributes have succeeded.
        let cleared = stop_all(root);
        let root = PathBuf::from(root);
        let mut order = [[0; 3]; COUNT];
        for (index, mapping) in order.iter_mut().enumerate() {
            let dir = led_dir(&root, index);
            let max = dir.join("max_brightness");
            let value = fs::read_to_string(&max).map_err(|e| format!("{}: {e}", max.display()))?;
            if value.trim().parse::<u16>() != Ok(255) {
                return Err(format!("{}: expected 255", max.display()));
            }
            let path = dir.join("multi_index");
            let value =
                fs::read_to_string(&path).map_err(|e| format!("{}: {e}", path.display()))?;
            let names: Vec<_> = value.split_whitespace().collect();
            let mut seen = [false; 3];
            if names.len() != 3 {
                return Err(format!("{}: expected red, green and blue", path.display()));
            }
            for (slot, name) in names.iter().enumerate() {
                let channel = ["red", "green", "blue"]
                    .iter()
                    .position(|c| c == name)
                    .filter(|c| !seen[*c])
                    .ok_or_else(|| format!("{}: expected red, green and blue", path.display()))?;
                seen[channel] = true;
                mapping[slot] = channel;
            }
            let path = dir.join("multi_intensity");
            OpenOptions::new()
                .write(true)
                .open(&path)
                .map_err(|e| format!("{}: {e}", path.display()))?;
        }
        cleared?;
        Ok(Self {
            root,
            order,
            brightness,
        })
    }

    fn show(&mut self, frame: &[u32; COUNT]) -> Result<(), String> {
        for (index, color) in frame.iter().enumerate() {
            let dir = led_dir(&self.root, index);
            let rgb = [(color >> 16) as u8, (color >> 8) as u8, *color as u8];
            let [a, b, c] = self.order[index].map(|channel| rgb[channel]);
            write(&dir.join("multi_intensity"), &format!("{a} {b} {c}\n"))?;
            // Clear used brightness zero. Every subsequent color restores the
            // configured brightness; wire GRB encoding belongs to the kernel driver.
            write(&dir.join("brightness"), &format!("{}\n", self.brightness))?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fixture(PathBuf);

    impl Fixture {
        fn new() -> Self {
            static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
            let root = std::env::temp_dir().join(format!(
                "leds-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&root).unwrap();
            for index in 0..COUNT {
                let dir = led_dir(&root, index);
                fs::create_dir(&dir).unwrap();
                fs::write(dir.join("max_brightness"), "255\n").unwrap();
                fs::write(
                    dir.join("multi_index"),
                    if index % 2 == 0 {
                        "green blue red\n"
                    } else {
                        "blue red green\n"
                    },
                )
                .unwrap();
                fs::write(dir.join("brightness"), "200\n").unwrap();
                fs::write(dir.join("multi_intensity"), "9 8 7\n").unwrap();
            }
            fs::write(led_dir(&root, 0).join("sync"), "0\n").unwrap();
            Self(root)
        }

        fn path(&self, index: usize, attr: &str) -> PathBuf {
            led_dir(&self.0, index).join(attr)
        }

        fn read(&self, index: usize, attr: &str) -> String {
            fs::read_to_string(self.path(index, attr)).unwrap()
        }

        fn config(&self, simulate: bool) -> Config {
            Config {
                simulate,
                gpio_chip: "/unused".into(),
                button_gpio: 17,
                led_sysfs: self.0.to_str().unwrap().into(),
                led_brightness: 200,
            }
        }
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            fs::remove_dir_all(&self.0).unwrap();
        }
    }

    #[tokio::test]
    async fn sysfs_maps_each_led_and_restores_brightness_after_clear() {
        let f = Fixture::new();
        let leds = Leds::open(&f.config(false));
        assert!(leds.available());
        for index in 0..COUNT {
            assert_eq!(f.read(index, "brightness"), "0\n");
        }
        assert_eq!(f.read(0, "sync"), "1\n");
        // Ordinary writes must not run the completion barrier.
        fs::write(f.path(0, "sync"), "pending\n").unwrap();
        leds.set(
            (0..COUNT).map(|i| (i, [11, 22, 33])).collect(),
            Cancel::default(),
        )
        .await
        .unwrap();
        for index in 0..COUNT {
            assert_eq!(f.read(index, "brightness"), "200\n");
            assert_eq!(
                f.read(index, "multi_intensity"),
                if index % 2 == 0 {
                    "22 33 11\n"
                } else {
                    "33 11 22\n"
                }
            );
        }
        assert_eq!(f.read(0, "sync"), "pending\n");
        leds.clear().await.unwrap();
        assert_eq!(f.read(0, "sync"), "1\n");
        leds.set(vec![(1, [255, 0, 8])], Cancel::default())
            .await
            .unwrap();
        for index in 0..COUNT {
            assert_eq!(f.read(index, "brightness"), "200\n");
            assert_eq!(
                f.read(index, "multi_intensity"),
                if index == 1 { "8 255 0\n" } else { "0 0 0\n" }
            );
        }
        // Simulation must not touch even a valid, writable sysfs fixture.
        let simulated = Leds::open(&f.config(true));
        simulated
            .pulse(1, [255; 3], Cancel::default())
            .await
            .unwrap();
        simulated.clear().await.unwrap();
        assert_eq!(f.read(1, "multi_intensity"), "8 255 0\n");
        assert_eq!(f.read(1, "brightness"), "200\n");
    }

    #[tokio::test]
    async fn sysfs_pulse_cancellation_and_clear_failure_stop_future_writes() {
        let f = Fixture::new();
        let leds = Leds::open(&f.config(false));
        leds.pulse(4, [200, 0, 100], Cancel::default())
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(2), async {
            while f.read(4, "multi_intensity") == "0 0 0\n" {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        // Set cancels an active pulse; canceled queued work cannot replace it.
        leds.set(vec![(4, [5, 6, 7])], Cancel::default())
            .await
            .unwrap();
        let cancel = Cancel::default();
        cancel.cancel();
        assert_eq!(
            leds.pulse(4, [255; 3], cancel).await,
            Err("canceled".into())
        );
        tokio::time::sleep(Duration::from_millis(230)).await;
        assert_eq!(f.read(4, "multi_intensity"), "6 7 5\n");
        leds.pulse(4, [200, 0, 100], Cancel::default())
            .await
            .unwrap();
        fs::remove_file(f.path(1, "brightness")).unwrap();
        fs::create_dir(f.path(1, "brightness")).unwrap();
        fs::remove_file(f.path(0, "sync")).unwrap();
        let error = leds.clear().await.unwrap_err();
        assert!(error.contains("multi:indicator-1/brightness"));
        assert!(error.contains("multi:indicator-0/sync"));
        assert!(!leds.available());
        assert!(
            !f.path(0, "sync").exists(),
            "missing attributes must not be created"
        );
        tokio::time::sleep(Duration::from_millis(230)).await;
        for index in [0, 2, 3, 4] {
            assert_eq!(f.read(index, "brightness"), "0\n", "all LEDs are attempted");
        }
        fs::remove_dir(f.path(1, "brightness")).unwrap();
        fs::write(f.path(1, "brightness"), "200\n").unwrap();
        fs::write(f.path(0, "sync"), "0\n").unwrap();
        leds.clear().await.unwrap();
        assert!(leds.available());
        assert_eq!(f.read(0, "sync"), "1\n");
        // A brightness error must still execute an otherwise valid sync.
        fs::remove_file(f.path(3, "brightness")).unwrap();
        fs::write(f.path(0, "sync"), "0\n").unwrap();
        assert!(leds
            .clear()
            .await
            .unwrap_err()
            .contains("indicator-3/brightness"));
        assert_eq!(f.read(0, "sync"), "1\n");
    }

    #[test]
    fn sysfs_rejects_absent_and_malformed_native_attributes() {
        for (attr, value) in [
            ("multi_index", "red green"),
            ("multi_index", "red red blue"),
            ("multi_index", "red green white"),
            ("multi_index", "red green blue white"),
            ("max_brightness", "254"),
            ("max_brightness", "invalid"),
        ] {
            let f = Fixture::new();
            fs::write(f.path(2, attr), value).unwrap();
            assert!(Sysfs::open(f.0.to_str().unwrap(), 200)
                .err()
                .unwrap()
                .contains(attr));
            assert!(!Leds::open(&f.config(false)).available());
        }
        for (index, attr) in [
            (2, "max_brightness"),
            (2, "multi_index"),
            (2, "multi_intensity"),
            (2, "brightness"),
            (0, "sync"),
        ] {
            let f = Fixture::new();
            fs::remove_file(f.path(index, attr)).unwrap();
            assert!(Sysfs::open(f.0.to_str().unwrap(), 200).is_err());
            assert!(!f.path(index, attr).exists());
        }
        let f = Fixture::new();
        fs::remove_dir_all(led_dir(&f.0, 4)).unwrap();
        assert!(Sysfs::open(f.0.to_str().unwrap(), 200).is_err());
    }

    #[tokio::test]
    async fn sysfs_initial_sync_and_later_io_errors_keep_availability_false() {
        let f = Fixture::new();
        fs::remove_file(f.path(0, "sync")).unwrap();
        let unavailable = Leds::open(&f.config(false));
        assert!(!unavailable.available());
        assert!(unavailable
            .set(vec![(0, [255; 3])], Cancel::default())
            .await
            .is_err());
        fs::write(f.path(0, "sync"), "0\n").unwrap();
        let leds = Leds::open(&f.config(false));
        assert!(leds.available());
        fs::remove_file(f.path(2, "multi_intensity")).unwrap();
        assert!(leds
            .set(vec![(0, [255; 3])], Cancel::default())
            .await
            .is_err());
        assert!(!leds.available());
        assert!(!f.path(2, "multi_intensity").exists());
    }

    #[tokio::test]
    async fn final_shutdown_attempts_ears_and_led_sync_despite_either_failure() {
        for (ear_failure, led_failure) in
            [(false, false), (true, false), (false, true), (true, true)]
        {
            let f = Fixture::new();
            let (events, _) = tokio::sync::mpsc::unbounded_channel();
            let hw = crate::hw::Hw {
                leds: Leds::open(&f.config(false)),
                ears: crate::hw::ears::Ears::open(
                    !ear_failure,
                    "/nonexistent-nabos-test-gpio",
                    events,
                ),
                rfid: None,
                button: false,
                info: crate::hw::HwInfo {
                    model: "fixture",
                    simulated: false,
                },
            };
            hw.leds
                .set(vec![(0, [255; 3])], Cancel::default())
                .await
                .unwrap();
            fs::write(f.path(0, "sync"), "0\n").unwrap();
            if led_failure {
                fs::remove_file(f.path(0, "sync")).unwrap();
            }
            let (ears, leds) = crate::shutdown(&hw).await;
            assert_eq!(ears.is_err(), ear_failure);
            assert_eq!(leds.is_err(), led_failure);
            for index in 0..COUNT {
                assert_eq!(f.read(index, "brightness"), "0\n");
            }
            if !led_failure {
                assert_eq!(f.read(0, "sync"), "1\n");
            }
        }
    }

    #[tokio::test]
    async fn sysfs_clear_waits_for_sync_before_reply_and_queued_set() {
        use std::io::Read;
        use std::os::unix::{ffi::OsStrExt, fs::OpenOptionsExt};

        let f = Fixture::new();
        let leds = Arc::new(Leds::open(&f.config(false)));
        leds.set(vec![(0, [255; 3])], Cancel::default())
            .await
            .unwrap();
        let path = f.path(0, "sync");
        fs::remove_file(&path).unwrap();
        let name = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
        // A FIFO pauses the fixture's sync write until a reader is admitted.
        assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
        let l = leds.clone();
        let clear = tokio::spawn(async move { l.clear().await });
        tokio::time::timeout(Duration::from_secs(2), async {
            while (0..COUNT).any(|i| f.read(i, "brightness") != "0\n") {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert!(
            !clear.is_finished(),
            "clear cannot reply before sync completes"
        );
        let l = leds.clone();
        let set = tokio::spawn(async move { l.set(vec![(0, [1, 2, 3])], Cancel::default()).await });
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert!(
            !set.is_finished(),
            "queued colors cannot bypass the barrier"
        );
        let mut reader = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&path)
            .unwrap();
        let mut value = Vec::new();
        tokio::time::timeout(Duration::from_secs(2), async {
            while value.len() < 2 {
                let _ = reader.read_to_end(&mut value);
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(value, b"1\n");
        clear.await.unwrap().unwrap();
        set.await.unwrap().unwrap();
        assert_eq!(f.read(0, "brightness"), "200\n");
        assert_eq!(f.read(0, "multi_intensity"), "2 3 1\n");
    }

    #[tokio::test]
    async fn batch_is_one_frame_and_canceled_commands_never_render() {
        let (tx, rx) = mpsc::channel();
        let frames = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        let f = frames.clone();
        let worker = std::thread::spawn(move || {
            run(rx, |fr, _| {
                f.lock().unwrap().push(*fr);
                Ok(())
            })
        });
        let leds = Leds {
            tx,
            ok: Arc::new(AtomicBool::new(true)),
        };
        leds.set(vec![(0, [1, 2, 3]), (4, [4, 5, 6])], Cancel::default())
            .await
            .unwrap();
        assert_eq!(frames.lock().unwrap().len(), 1);
        let cancel = Cancel::default();
        cancel.cancel();
        assert!(leds.pulse(4, [255; 3], cancel).await.is_err());
        assert_eq!(frames.lock().unwrap().len(), 1);
        leds.pulse(4, [200, 0, 100], Cancel::default())
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(2300)).await;
        leds.clear().await.unwrap();
        drop(leds);
        worker.join().unwrap();
        let v = frames.lock().unwrap();
        let peak = v.iter().map(|f| f[4]).max().unwrap();
        assert_eq!(peak, (200 << 16) | 100);
        assert_eq!(*v.last().unwrap(), [0; COUNT]);
    }
    #[tokio::test]
    async fn failed_clear_propagates_the_render_error() {
        let (tx, rx) = mpsc::channel();
        let worker = std::thread::spawn(move || run(rx, |_, _| Err("render-failed".into())));
        let leds = Leds {
            tx,
            ok: Arc::new(AtomicBool::new(true)),
        };
        assert_eq!(
            leds.clear().await,
            Err("render-failed".into()),
            "cleanup cannot declare quiescence after a failed frame"
        );
        drop(leds);
        worker.join().unwrap();
    }
}
