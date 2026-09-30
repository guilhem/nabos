//! Five WS2812 LEDs on GPIO 13 (PWM channel 1, DMA 12) through rpi_ws281x,
//! loaded at runtime with dlopen (no link-time dependency). Software pulsing
//! uses a 100 ms period and 10 steps.

use super::Cancel;
use crate::Config;
use std::ffi::{c_char, c_int, c_void, CStr, CString};
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
        let lib = cfg.ws2811_lib.clone();
        let brightness = cfg.led_brightness;
        let strip = strip_type(&cfg.led_strip);
        std::thread::spawn(move || {
            let mut strip = if sim {
                None
            } else {
                match Ws2811::open(&lib, brightness, strip) {
                    Ok(s) => Some(s),
                    Err(e) => {
                        error!("LEDs unavailable: {e}");
                        None
                    }
                }
            };
            available.store(sim || strip.is_some(), Ordering::Relaxed);
            let _ = ready_tx.send(());
            run(rx, |frame| {
                let result = if let Some(s) = strip.as_mut() {
                    s.show(frame)
                } else if sim {
                    Ok(())
                } else {
                    Err("leds-unavailable".into())
                };
                available.store(result.is_ok(), Ordering::Relaxed);
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
        self.set((0..COUNT).map(|i| (i, [0; 3])).collect(), Cancel::default())
            .await
    }
}

struct Pulse {
    target: [f32; 3],
    current: [f32; 3],
    up: bool,
}

fn run(rx: mpsc::Receiver<Cmd>, mut show: impl FnMut(&[u32; COUNT]) -> Result<(), String>) {
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
        let mut replies = Vec::new();
        for c in pending {
            let (cancel, reply) = match &c {
                Cmd::Set(_, c, r) | Cmd::Pulse(_, _, c, r) => (c, r),
            };
            let admitted = !reply.is_closed() && cancel.admit();
            let reply = match c {
                Cmd::Set(colors, _, reply) => {
                    if admitted {
                        for (led, [r, g, b]) in colors {
                            pulses[led] = None;
                            frame[led] = ((r as u32) << 16) | ((g as u32) << 8) | b as u32;
                        }
                    }
                    reply
                }
                Cmd::Pulse(led, [r, g, b], _, reply) => {
                    if admitted {
                        frame[led] = 0;
                        pulses[led] = Some(Pulse {
                            target: [r as f32, g as f32, b as f32],
                            current: [0.0; 3],
                            up: true,
                        });
                        next_pulse.get_or_insert_with(Instant::now);
                    }
                    reply
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
        let rendered = if changed { show(&frame) } else { Ok(()) };
        for (reply, admitted) in replies {
            let _ = reply.send(if admitted {
                rendered.clone()
            } else {
                Err("canceled".into())
            });
        }
    }
}

fn strip_type(name: &str) -> c_int {
    match name {
        "rgb" => 0x0010_0800,
        "rbg" => 0x0010_0008,
        "gbr" => 0x0008_0010,
        "brg" => 0x0000_1008,
        "bgr" => 0x0000_0810,
        _ => 0x0008_1000, // grb, rpi_ws281x GRB pixel order
    }
}

// Layout of ws2811_t / ws2811_channel_t from rpi_ws281x ws2811.h.
#[repr(C)]
struct Channel {
    gpionum: c_int,
    invert: c_int,
    count: c_int,
    strip_type: c_int,
    leds: *mut u32,
    brightness: u8,
    wshift: u8,
    rshift: u8,
    gshift: u8,
    bshift: u8,
    gamma: *mut u8,
}

#[repr(C)]
struct Ws2811T {
    render_wait_time: u64,
    device: *mut c_void,
    rpi_hw: *const c_void,
    freq: u32,
    dmanum: c_int,
    channel: [Channel; 2],
}

type InitFn = unsafe extern "C" fn(*mut Ws2811T) -> c_int;
type RenderFn = unsafe extern "C" fn(*mut Ws2811T) -> c_int;
type StrFn = unsafe extern "C" fn(c_int) -> *const c_char;

struct Ws2811 {
    raw: Box<Ws2811T>,
    render: RenderFn,
    strerr: StrFn,
    failures: u32,
}

unsafe fn sym<T>(lib: *mut c_void, name: &str) -> Result<T, String> {
    let c = CString::new(name).unwrap();
    let p = libc::dlsym(lib, c.as_ptr());
    if p.is_null() {
        return Err(format!("symbol {name} not found"));
    }
    Ok(std::mem::transmute_copy(&p))
}

impl Ws2811 {
    fn open(lib: &str, brightness: u8, strip: c_int) -> Result<Ws2811, String> {
        unsafe {
            let name = CString::new(lib).map_err(|e| e.to_string())?;
            let handle = libc::dlopen(name.as_ptr(), libc::RTLD_NOW);
            if handle.is_null() {
                let e = libc::dlerror();
                return Err(if e.is_null() {
                    format!("dlopen {lib} failed")
                } else {
                    CStr::from_ptr(e).to_string_lossy().into()
                });
            }
            let init: InitFn = sym(handle, "ws2811_init")?;
            let render: RenderFn = sym(handle, "ws2811_render")?;
            let strerr: StrFn = sym(handle, "ws2811_get_return_t_str")?;
            let mut raw: Box<Ws2811T> = Box::new(std::mem::zeroed());
            raw.freq = 800_000;
            raw.dmanum = 12;
            raw.channel[1].gpionum = 13;
            raw.channel[1].count = COUNT as c_int;
            raw.channel[1].brightness = brightness;
            raw.channel[1].strip_type = strip;
            let rc = init(&mut *raw);
            if rc != 0 {
                return Err(format!(
                    "ws2811_init: {}",
                    CStr::from_ptr(strerr(rc)).to_string_lossy()
                ));
            }
            Ok(Ws2811 {
                raw,
                render,
                strerr,
                failures: 0,
            })
        }
    }

    fn show(&mut self, frame: &[u32; COUNT]) -> Result<(), String> {
        unsafe {
            let leds = self.raw.channel[1].leds;
            if leds.is_null() {
                return Err("LED buffer unavailable".into());
            }
            for (i, c) in frame.iter().enumerate() {
                *leds.add(i) = *c;
            }
            let rc = (self.render)(&mut *self.raw);
            if rc != 0 {
                let reason = format!(
                    "ws2811_render: {}",
                    CStr::from_ptr((self.strerr)(rc)).to_string_lossy()
                );
                if self.failures < 5 {
                    self.failures += 1;
                    error!("{reason}");
                }
                return Err(reason);
            }
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn batch_is_one_frame_and_canceled_commands_never_render() {
        let (tx, rx) = mpsc::channel();
        let frames = std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        let f = frames.clone();
        let worker = std::thread::spawn(move || {
            run(rx, |fr| {
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
        let worker = std::thread::spawn(move || run(rx, |_| Err("render-failed".into())));
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
