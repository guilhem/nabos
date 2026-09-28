//! Jobs and animations built on the local hardware.

use crate::chor;
use crate::hw::leds::{Rgb, BOTTOM, CENTER, LEFT, NOSE, RIGHT};
use crate::hw::player::Source;
use crate::hw::{Cancel, Hw};
use crate::protocol::{Animation, Item, TestKind, STREAMING_URN};
use crate::resources::Kind;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::task::JoinHandle;

pub const FUCHSIA: Rgb = [255, 0, 255];
pub const INFO_LOOP: Duration = Duration::from_secs(15);
pub const SLEEP_EARS: u8 = 10;

/// Background choreography, kept running across items when unchanged.
pub struct ChorRunner {
    hw: Arc<Hw>,
    task: Option<JoinHandle<()>>,
    current: Option<String>,
}

impl ChorRunner {
    pub fn new(hw: Arc<Hw>) -> ChorRunner {
        ChorRunner {
            hw,
            task: None,
            current: None,
        }
    }

    pub async fn start(&mut self, reference: &str) {
        if self.current.as_deref() == Some(reference)
            && self.task.as_ref().is_some_and(|t| !t.is_finished())
        {
            return;
        }
        self.stop().await;
        self.task = Some(tokio::spawn(chor::play(
            self.hw.clone(),
            reference.to_string(),
        )));
        self.current = Some(reference.to_string());
    }

    pub async fn stop(&mut self) {
        if let Some(t) = self.task.take() {
            t.abort();
            let _ = t.await;
        }
        self.current = None;
    }

    pub async fn wait(&mut self, cancel: &Cancel) {
        if let Some(mut t) = self.task.take() {
            tokio::select! {
                _ = &mut t => {}
                _ = cancel.wait() => {
                    t.abort();
                    let _ = t.await;
                }
            }
        }
        self.current = None;
    }
}

impl Drop for ChorRunner {
    fn drop(&mut self) {
        if let Some(t) = self.task.take() {
            t.abort();
        }
    }
}

struct Loaded {
    audio: Option<Vec<Source>>,
    choreography: Option<String>,
}

fn preload(hw: &Hw, items: &[Item]) -> Vec<Loaded> {
    items
        .iter()
        .map(|it| Loaded {
            audio: match (&it.stream, &it.audio) {
                (Some(url), _) => Some(vec![Source::Stream(url.clone())]),
                (None, list) => list.as_ref().map(|list| {
                    list.iter()
                        .filter_map(|r| {
                            let f = hw.res.find(Kind::Sound, r);
                            if f.is_none() {
                                warn!("could not find sound {r}");
                            }
                            f.map(Source::File)
                        })
                        .collect()
                }),
            },
            choreography: it.choreography.clone(),
        })
        .collect()
}

async fn play_loaded(
    hw: &Arc<Hw>,
    runner: &mut ChorRunner,
    items: &[Loaded],
    default: Option<&str>,
    cancel: &Cancel,
) {
    for it in items {
        if cancel.is_cancelled() {
            break;
        }
        let chor = it.choreography.as_deref().or(default);
        match chor {
            Some(c) => runner.start(c).await,
            None => runner.stop().await,
        }
        if let Some(files) = &it.audio {
            hw.player.play_list(files, cancel).await;
            if chor.is_some() {
                runner.stop().await;
            }
        } else if it.choreography.is_some() {
            runner.wait(cancel).await;
        }
    }
}

pub async fn play_sequence(hw: Arc<Hw>, sequence: &[Item], cancel: &Cancel) {
    let items = preload(&hw, sequence);
    let mut runner = ChorRunner::new(hw.clone());
    play_loaded(&hw, &mut runner, &items, None, cancel).await;
    runner.stop().await;
    hw.player.stop().await;
}

pub async fn play_message(hw: Arc<Hw>, signature: Option<&Item>, body: &[Item], cancel: &Cancel) {
    move_ears_with_leds(&hw, [255, 0, 0], 0, 0).await;
    let sig = preload(&hw, &[signature.cloned().unwrap_or_default()]);
    let body = preload(&hw, body);
    let mut runner = ChorRunner::new(hw.clone());
    for part in [&sig, &body, &sig] {
        play_loaded(&hw, &mut runner, part, Some(STREAMING_URN), cancel).await;
    }
    runner.stop().await;
    hw.player.stop().await;
    hw.leds.set_all([0, 0, 0]);
}

/// If ears are not in position: LEDs to color, move, LEDs off.
pub async fn move_ears_with_leds(hw: &Hw, color: Rgb, left: u8, right: u8) {
    let (l, r) = hw.ears.positions().await;
    let moving =
        (l != Some(left) && !hw.ears.broken(0)) || (r != Some(right) && !hw.ears.broken(1));
    if moving {
        hw.leds.set_all(color);
        hw.ears.move_to(left, right).await;
    }
    hw.leds.set_all([0, 0, 0]);
}

pub fn clear_info(hw: &Hw) {
    for led in [LEFT, CENTER, RIGHT] {
        hw.leds.set(led, [0, 0, 0]);
    }
}

/// Info animation on left/center/right for at most duration (None: forever).
pub async fn play_animation(hw: &Hw, anim: &Animation, duration: Option<Duration>) {
    let start = Instant::now();
    let step = Duration::from_millis(anim.tempo as u64 * 10);
    for frame in anim.frames.iter().cycle() {
        if duration.is_some_and(|d| start.elapsed() >= d) {
            break;
        }
        for (led, c) in [LEFT, CENTER, RIGHT].into_iter().zip(frame) {
            hw.leds.set(led, *c);
        }
        tokio::time::sleep(step).await;
    }
    clear_info(hw);
}

pub async fn rfid_feedback(hw: Arc<Hw>) {
    let mut runner = ChorRunner::new(hw.clone());
    runner.start("system/rfid.chor").await;
    if let Some(f) = hw.res.find(Kind::Sound, "rfid/rfid.wav") {
        hw.player
            .play_list(&[Source::File(f)], &Cancel::never())
            .await;
    }
    runner.stop().await;
    hw.leds.set_all([0, 0, 0]);
}

pub async fn abort_feedback(hw: &Arc<Hw>) {
    if let Some(f) = hw.res.find(Kind::Sound, "system/abort.wav") {
        hw.player
            .play_list(&[Source::File(f)], &Cancel::never())
            .await;
    }
}

pub async fn sleep_setup(hw: &Hw) {
    hw.leds.set_all([0, 0, 0]);
    hw.ears.move_to(SLEEP_EARS, SLEEP_EARS).await;
}

/// Boot/shutdown progress LEDs (five progressive brightness levels).
pub fn boot_leds(hw: &Hw) {
    hw.leds
        .set5([FUCHSIA, [255, 255, 255], FUCHSIA, FUCHSIA, FUCHSIA]);
}

pub async fn test(hw: &Hw, kind: TestKind) -> bool {
    match kind {
        TestKind::Ears => {
            let (l0, r0) = hw.ears.positions().await;
            hw.ears.go(0, 8, true).await;
            hw.ears.go(1, 8, true).await;
            hw.ears.wait_idle().await;
            for backward in [false, true] {
                for _ in 0..17 {
                    hw.ears.step(0, 1, backward).await;
                    hw.ears.step(1, 1, !backward).await;
                    hw.ears.wait_idle().await;
                    tokio::time::sleep(Duration::from_millis(200)).await;
                }
            }
            hw.ears.move_to(0, 0).await;
            if let Some(l) = l0 {
                hw.ears.go(0, l, false).await;
            }
            if let Some(r) = r0 {
                hw.ears.go(1, r, false).await;
            }
            hw.ears.wait_idle().await;
            !hw.ears.broken(0) && !hw.ears.broken(1)
        }
        TestKind::Leds => {
            for c in [
                [0, 0, 0],
                [255, 0, 0],
                [0, 255, 0],
                [0, 0, 255],
                [255, 255, 255],
                [127, 127, 127],
                [0, 0, 0],
            ] {
                for led in [NOSE, LEFT, CENTER, RIGHT, BOTTOM] {
                    hw.leds.set(led, c);
                    tokio::time::sleep(Duration::from_millis(200)).await;
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            hw.leds.ok
        }
    }
}
