//! Nabaztag choreography interpreter
//! (MTL opcodes from nominal.010120_as3.mtl, streaming subset).

use crate::hw::leds::{BOTTOM, CENTER, LEFT, NOSE, RIGHT};
use crate::hw::player::Source;
use crate::hw::{Cancel, Hw};
use crate::protocol::STREAMING_URN;
use crate::resources::Kind;
use std::sync::Arc;
use std::time::{Duration, Instant};

const MIDI_LIST: [&str; 24] = [
    "choreographies/1noteA4.mp3",
    "choreographies/1noteB5.mp3",
    "choreographies/1noteBb4.mp3",
    "choreographies/1noteC5.mp3",
    "choreographies/1noteE4.mp3",
    "choreographies/1noteF4.mp3",
    "choreographies/1noteF5.mp3",
    "choreographies/1noteG5.mp3",
    "choreographies/2notesC6C4.mp3",
    "choreographies/2notesC6F5.mp3",
    "choreographies/2notesD4A5.mp3",
    "choreographies/2notesD4G4.mp3",
    "choreographies/2notesD5G4.mp3",
    "choreographies/2notesE5A5.mp3",
    "choreographies/2notesE5C6.mp3",
    "choreographies/2notesE5E4.mp3",
    "choreographies/3notesA4G5G5.mp3",
    "choreographies/3notesB5A5F5.mp3",
    "choreographies/3notesB5D5C6.mp3",
    "choreographies/3notesD4E4G4.mp3",
    "choreographies/3notesE5A5C6.mp3",
    "choreographies/3notesE5C6D5.mp3",
    "choreographies/3notesE5D5A5.mp3",
    "choreographies/3notesF5C6G5.mp3",
];

type Palette = [[u8; 3]; 8];

const PALETTES: [Palette; 7] = [
    [
        [255, 12, 0],
        [0, 255, 31],
        [255, 242, 0],
        [0, 3, 255],
        [255, 242, 0],
        [0, 255, 31],
        [255, 12, 0],
        [0, 0, 0],
    ], // acidulée
    [
        [95, 0, 255],
        [127, 0, 255],
        [146, 0, 255],
        [191, 0, 255],
        [223, 0, 255],
        [255, 0, 223],
        [255, 0, 146],
        [0, 0, 0],
    ], // violet
    [
        [255, 255, 255],
        [255, 255, 255],
        [255, 255, 255],
        [255, 255, 255],
        [255, 255, 255],
        [255, 255, 255],
        [255, 255, 255],
        [0, 0, 0],
    ], // lumière
    [
        [254, 128, 2],
        [243, 68, 2],
        [216, 6, 7],
        [200, 4, 13],
        [170, 0, 24],
        [218, 5, 96],
        [207, 6, 138],
        [0, 0, 0],
    ], // émotion
    [
        [20, 155, 18],
        [255, 0, 0],
        [252, 243, 5],
        [20, 155, 18],
        [252, 243, 5],
        [255, 0, 0],
        [20, 155, 18],
        [0, 0, 0],
    ], // oriental
    [
        [252, 238, 71],
        [206, 59, 69],
        [85, 68, 212],
        [78, 167, 82],
        [243, 75, 153],
        [151, 71, 196],
        [255, 255, 255],
        [0, 0, 0],
    ], // pastel
    [
        [204, 255, 102],
        [204, 255, 0],
        [153, 255, 0],
        [51, 204, 0],
        [0, 153, 51],
        [0, 136, 0],
        [0, 102, 51],
        [0, 0, 0],
    ], // nature
];

/// Choreography LED numbers are reversed compared to the strip.
const LEDS: [usize; 5] = [BOTTOM, RIGHT, CENTER, LEFT, NOSE];

#[derive(Clone, Copy, PartialEq)]
pub enum Opcodes {
    Mtl,
    Streaming,
}

pub struct Interp {
    hw: Arc<Hw>,
    timescale: u32,
    taichi_random: u8,
    directions: [bool; 2],
    palette: Palette,
    palette_colors: [usize; 4],
}

impl Interp {
    pub fn new(hw: Arc<Hw>) -> Interp {
        Interp {
            hw,
            timescale: 0,
            // Original generator: ((rand & 255) * 30) >> 8, 0-29 not quite uniform.
            taichi_random: ((fastrand::u32(0..256) * 30) >> 8) as u8,
            directions: [false; 2],
            palette: [[0; 3]; 8],
            palette_colors: [0; 4],
        }
    }

    fn led(&self, chor: &[u8], i: usize) -> Option<usize> {
        LEDS.get(*chor.get(i)? as usize).copied()
    }

    /// Execute one opcode at index i (arguments start at i). None stops playback.
    async fn op(&mut self, opcode: u8, i: usize, chor: &[u8], set: Opcodes) -> Option<usize> {
        let arg = |k: usize| chor.get(i + k).copied();
        match (set, opcode) {
            (_, 0) => Some(i),
            (Opcodes::Mtl, 1) => {
                self.timescale = 10 * arg(0)? as u32;
                Some(i + 1)
            }
            (Opcodes::Streaming, 1) => Some(i + 1),
            (_, 7) => {
                let led = self.led(chor, i)?;
                self.hw.leds.set(led, [arg(1)?, arg(2)?, arg(3)?]);
                Some(i + 6)
            }
            (Opcodes::Mtl, 8) => {
                let motor = arg(0)? as usize;
                if motor > 1 {
                    return None;
                }
                self.hw.ears.go(motor, arg(1)?, arg(2)? != 0).await;
                Some(i + 3)
            }
            (Opcodes::Mtl, 9) => {
                self.hw.leds.set_all([arg(0)?, arg(1)?, arg(2)?]);
                Some(i + 3)
            }
            (_, 10) => {
                let led = self.led(chor, i)?;
                self.hw.leds.set(led, [0, 0, 0]);
                Some(i + 1)
            }
            (Opcodes::Mtl, 14) => {
                let led = self.led(chor, i)?;
                self.hw.leds.set(led, self.palette[(arg(1)? & 7) as usize]);
                Some(i + 2)
            }
            (Opcodes::Streaming, 14) => {
                let led = self.led(chor, i)?;
                let ix = self.palette_colors[(arg(1)? & 3) as usize];
                self.hw.leds.set(led, self.palette[ix]);
                Some(i + 2)
            }
            (Opcodes::Mtl, 16) => {
                if let Some(p) = self
                    .hw
                    .res
                    .find(Kind::Sound, MIDI_LIST[fastrand::usize(..MIDI_LIST.len())])
                {
                    self.hw.player.start(Source::File(p));
                }
                Some(i)
            }
            (Opcodes::Mtl, 17) => {
                let motor = arg(0)? as usize;
                if motor > 1 {
                    return None;
                }
                self.hw
                    .ears
                    .step(motor, arg(1)?, self.directions[motor])
                    .await;
                Some(i + 2)
            }
            (Opcodes::Mtl, 18) => {
                if self.taichi_random == arg(0)? {
                    return Some(i + 3);
                }
                let rel = i16::from_be_bytes([arg(1)?, arg(2)?]) as isize;
                usize::try_from(i as isize + rel + 3).ok()
            }
            (Opcodes::Mtl, 19) => {
                self.hw.ears.wait_idle().await;
                self.hw.player.wait_done(&Cancel::never()).await;
                Some(i)
            }
            (Opcodes::Mtl, 20) => {
                let motor = arg(0)? as usize;
                if motor > 1 {
                    return None;
                }
                self.directions[motor] = arg(1)? != 0;
                Some(i + 2)
            }
            _ => {
                if opcode != 255 {
                    warn!("unknown choreography opcode {opcode}");
                }
                None
            }
        }
    }

    pub async fn play_binary(&mut self, chor: &[u8], set: Opcodes, timescale: u32) {
        let mut index = if chor.len() >= 4 && chor[..4] == [1, 1, 1, 1] {
            4
        } else {
            0
        };
        self.timescale = timescale;
        let mut next = Instant::now();
        while index < chor.len() {
            // wait * timescale / 1000 seconds
            next += Duration::from_millis(chor[index] as u64 * self.timescale as u64);
            let now = Instant::now();
            if next > now {
                tokio::time::sleep(next - now).await;
            }
            index += 2;
            if index > chor.len() {
                break; // taichi.chor ends with a wait
            }
            match self.op(chor[index - 1], index, chor, set).await {
                Some(i) => index = i,
                None => return,
            }
        }
    }

    async fn play_streaming(&mut self, palette: Option<usize>) {
        let mut ear_chance: Option<u32> = None;
        loop {
            match ear_chance {
                None => {
                    ear_chance = Some(0);
                    let (l, r) = if fastrand::bool() { (0, 10) } else { (10, 0) };
                    self.hw.ears.go(0, l, false).await;
                    self.hw.ears.go(1, r, false).await;
                }
                Some(c) => {
                    if fastrand::u32(0..=c) == 0 {
                        let pick = || [0u8, 5, 10, 14][fastrand::usize(..4)];
                        self.hw.ears.go(0, pick(), false).await;
                        self.hw.ears.go(1, pick(), false).await;
                        ear_chance = Some((c + 1) % 4);
                    }
                }
            }
            let Some(file) = self
                .hw
                .res
                .find(Kind::Choreography, "system/streaming/*.chor")
            else {
                warn!("no streaming choreography found");
                return;
            };
            let Ok(chor) = std::fs::read(&file) else {
                return;
            };
            let tempo = 160 + fastrand::u32(0..=90);
            let loops = 3 + fastrand::u32(0..=17);
            self.palette = PALETTES[palette.unwrap_or_else(|| fastrand::usize(..PALETTES.len()))];
            self.palette_colors = std::array::from_fn(|_| fastrand::usize(0..8));
            for _ in 0..loops {
                self.play_binary(&chor, Opcodes::Streaming, tempo).await;
            }
        }
    }
}

/// Play a choreography reference until it ends (streaming never ends).
pub async fn play(hw: Arc<Hw>, reference: String) {
    let mut it = Interp::new(hw.clone());
    if let Some(rest) = reference.strip_prefix(STREAMING_URN) {
        let palette = rest
            .strip_prefix(':')
            .and_then(|n| n.parse::<usize>().ok())
            .map(|n| n & 7)
            .filter(|n| *n < PALETTES.len());
        it.play_streaming(palette).await;
        return;
    }
    match hw
        .res
        .find(Kind::Choreography, &reference)
        .map(std::fs::read)
    {
        Some(Ok(chor)) => it.play_binary(&chor, Opcodes::Mtl, 0).await,
        Some(Err(e)) => warn!("choreography {reference}: {e}"),
        None => warn!("choreography {reference} not found"),
    }
}
