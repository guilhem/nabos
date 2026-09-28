# NabOS runtime protocol v1

Two programs run on the rabbit and talk through the local Mosquitto broker
(MQTT 5, `127.0.0.1:1883`, anonymous, loopback only):

- `nab-core` (Rust, `core/`): hardware, state machine, queue, choreographies, audio.
- `nab-service` (Go, `services/`): web UI, settings, clock, weather, Home Assistant,
  Linux Voice Assistant, updates.

Every topic starts with `nabos/v1`. Payloads are UTF-8 JSON objects with `"v": 1`.

## Topics

| Topic | Direction | QoS | Retained | Payload |
|---|---|---|---|---|
| `nabos/v1/core/cmd` | service → core | 1 | **never** | command envelope |
| `nabos/v1/core/result` | core → service | 1 | no | result |
| `nabos/v1/core/state` | core → all | 1 | yes | core state |
| `nabos/v1/core/availability` | core → all | 1 | yes (LWT) | `online` / `offline` (plain text) |
| `nabos/v1/core/event/button` | core → all | 1 | no | button event |
| `nabos/v1/core/event/ears` | core → all | 1 | no | ears event |
| `nabos/v1/core/event/rfid` | core → all | 1 | no | RFID event |
| `nabos/v1/service/settings` | service → core | 1 | yes | runtime settings |
| `nabos/v1/service/availability` | service → all | 1 | yes (LWT) | `online` / `offline` |

Both clients connect with `clean_start = true` and session expiry 0, so the broker
keeps no queued commands for an offline core. The service publishes commands only
while connected (no offline queue): a command published while the core is down is
lost, never replayed later.

The core subscribes to `core/cmd` with *Retain As Published*, so a retained command
is recognised both when it is stored on the broker and when it is delivered live.

## Command envelope

```json
{"v":1,"id":"web-3f2a9c","expires_at":1790000000,"action":"play","args":{}}
```

- `id`: 1–64 chars `[A-Za-z0-9_.:-]`, unique per command.
- `expires_at`: Unix time in seconds. The core answers `expired` when the command
  is received after this time **or** when it expires while waiting in the queue.
  Values more than 24 h in the future are rejected.
- The core refuses retained messages (`rejected`, error `retained`), payloads over
  64 KiB, unknown `v`, unknown actions and out-of-bounds arguments.
- Duplicates: the core remembers ids until their `expires_at`
  (at most 1024). A repeated id is not executed again and gets `duplicate`.

## Result

```json
{"v":1,"id":"web-3f2a9c","status":"ok","error":null,"data":null}
```

`status` is one of `ok`, `canceled`, `expired`, `duplicate`, `rejected`
(invalid command, nothing executed), `error` (execution failed). Exactly one result
is published per accepted id, when the command finishes.

## Actions

Sequence item: `{"audio": ["res", ...], "choreography": "res"}`, both optional.

- Audio resources are resolved against the sounds roots, first in `<root>/<locale>/<res>`
  then `<root>/<res>`. A last path component starting with `*` picks a random match
  (`clock/7/*.mp3`). `a;b` tries `a` then `b`. Only `.mp3` and `.wav`.
- Choreography: a resource under the choreographies roots (`system/rfid.chor`),
  or `urn:x-chor:streaming` / `urn:x-chor:streaming:N` (N = palette 0–7).
- Resources are relative paths without `..`, backslash, NUL, leading `/`,
  256 chars max. Remote URLs are not accepted by the core: the service downloads
  into `/data/nabos/media/sounds/cache/` first.

| Action | Args | Behaviour |
|---|---|---|
| `play` | `{"sequence":[item ≤32], "cancelable":true}` | Queued, played when idle. Audio ≤16 per item. |
| `message` | `{"signature":item?, "body":[item ≤32], "cancelable":true}` | Queued. Ears to 0, signature, body, signature, default streaming choreography. |
| `cancel` | `{"target":"<id>"?}` | Cancels the playing command (or the given id). Errors `not_playing`, `not_cancelable`. Queued targets are removed and answered `canceled`. |
| `info` | `{"info_id":"weather", "animation":{"tempo":1–1000,"colors":[{"left":"rrggbb","center":"rrggbb","right":"rrggbb"} ≤64]} or null}` | Idle loop animation, 15 s per info, rotating. `null` removes it. `info_id` ≤ 64 chars, ≤ 16 infos. |
| `indicator` | `{"animation":{…} or null}` | Overrides infos while idle or asleep (voice assistant feedback). |
| `ears` | `{"left":0–16?, "right":0–16?}` | Sets the idle position, moves immediately when idle. |
| `sleep` | `{}` | Queued; LEDs off, ears 10/10. |
| `wakeup` | `{}` | Immediate when asleep. |
| `rfid_write` | `{"tech":"st25tb"|"iso14443a_t2t","uid":"d0:02:…","picture":0–255,"app":"clock" or 0–255,"data":"≤32 bytes","timeout":1–60}` | Queued; nose red while waiting. `data` field of the result: `{"uid":…}`. Error `timeout`, `write_failed`, `no_reader`. |
| `test` | `{"test":"ears"|"leds"}` | Queued hardware test; `error` if an ear is broken. |
| `gestalt` | `{}` | Immediate; `data` = hardware description. |

A `click` while a cancelable command plays cancels
it (with `system/abort.wav`) and is not published. Every other button event is
published. Shutdown and reboot belong to the service (logind), triggered by
`triple_click`.

## State (retained)

```json
{"v":1,"state":"idle","playing":null,"ears":{"left":0,"right":0},
 "hardware":{"model":"2022_NFC","rfid":"st25r391x","left_ear":"ok","right_ear":"ok",
             "leds":true,"button":true,"simulated":false},
 "version":"0.1.0"}
```

`state`: `idle`, `asleep`, `playing`. `playing` is the command id or null.
`model`: `2019_TAG` (no reader), `2019_TAGTAG` (CR14), `2022_NFC` (ST25R391x), `simulated`.
Ear status: `ok`, `broken`, `missing`.

## Events

```json
{"v":1,"event":"click","time":1790000000.12}
{"v":1,"left":3,"right":null,"time":1790000000.5}
{"v":1,"event":"detected","tech":"st25tb","uid":"d0:02:18:00:00:00:00:01","support":"formatted",
 "locked":false,"picture":42,"app":"clock","data":"\u0000","time":1790000000.7}
```

Button events: `down`, `up`, `click`, `double_click`, `triple_click`, `hold`,
`click_and_hold`. RFID `support`: `formatted`, `foreign-data`, `locked`, `empty`,
`unknown`. `app`, `picture` and `data` are present for Nabaztag-formatted tags;
`data` is the application payload decoded as UTF-8 up to the first `0xFF`.
RFID application identifiers use the Nabaztag tag format (1 eightball … 5 clock, 9 weather, 13 webhook, 255 none).

## Service settings (retained)

```json
{"v":1,"locale":"fr_FR","network":"ok"}
```

`network`: `ok`, `lan` (no Internet: orange belly), `offline` (red belly).

## Image contract

### nab-core

`/usr/bin/nab-core [--simulate]`, system service, `User=nabos` (UID 1000).

| Variable | Default |
|---|---|
| `NABOS_MQTT_HOST` / `NABOS_MQTT_PORT` | `127.0.0.1` / `1883` |
| `NABOS_SOUNDS_DIRS` | `/usr/share/nabos/sounds:/data/nabos/media/sounds` |
| `NABOS_CHOREOGRAPHIES_DIRS` | `/usr/share/nabos/choreographies:/data/nabos/media/choreographies` |
| `NABOS_ALSA_DEVICE` | `default` (pipewire-alsa makes it PipeWire) |
| `NABOS_GPIO_CHIP` / `NABOS_BUTTON_GPIO` | `/dev/gpiochip0` / `17` |
| `NABOS_WS2811_LIB` | `libws2811.so` (loaded with dlopen, GPIO 13, PWM channel 1, DMA 12) |
| `NABOS_LED_BRIGHTNESS` / `NABOS_LED_STRIP` | `200` / `grb` (`rgb`, `grb`, `brg`…) |
| `NABOS_SIM_AUDIO_MS` | `50` (simulate mode only: duration of each sound) |
| `RUST_LOG`-like `NABOS_LOG` | `info` (`debug` for traces) |

Runtime needs: `mpg123` (mp3) and `aplay` (wav) from alsa-utils, `pipewire-alsa`,
`XDG_RUNTIME_DIR=/run/user/1000` so the ALSA plugin finds the PipeWire socket,
`libws2811.so` in the loader path.

Permissions (no root):

- udev: `/dev/ear0`, `/dev/ear1`, `/dev/rfid0`, `/dev/nfc0` → `GROUP="nabos", MODE="0660"`
  (the drivers create them root 0600).
- `/dev/gpiochip0`: group `gpio`.
- LEDs (rpi_ws281x PWM+DMA): `/dev/mem` read/write and `/dev/vcio` (group `video`).
  Recommended unit: `SupplementaryGroups=gpio video kmem`, udev
  `KERNEL=="mem", GROUP="kmem", MODE="0660"`, `AmbientCapabilities=CAP_SYS_RAWIO`,
  `CapabilityBoundingSet=CAP_SYS_RAWIO`. Access to `/dev/mem` is root-equivalent;
  the capability is confined to this one unit.
- `dtparam=audio=off` (PWM1 on GPIO 13 is used by the LEDs).

Media: copy `assets/sounds/` into `/usr/share/nabos/sounds/` and
`assets/choreographies/` into `/usr/share/nabos/choreographies/`.
Keep locale directories and resource paths intact.

### nab-service

`/usr/bin/nab-service`, system service, `User=nabos`.

| Variable | Default |
|---|---|
| `NABOS_MQTT_HOST` / `NABOS_MQTT_PORT` | `127.0.0.1` / `1883` |
| `NABOS_HTTP_ADDR` | `:8080` |
| `NABOS_DATA_DIR` | `/data/nabos` (`config.json`, `media/`, `updates/`, `voice-enabled`) |
| `NABOS_SOUNDS_DIRS` | same as the core (used to list sounds) |
| `NABOS_VERSION` | release version (`v1.2.3`), from `/etc/nabos/release` |
| (build) | `go build -ldflags "-X main.version=v1.2.3"` sets the default of `NABOS_VERSION` |
| `NABOS_UPDATE_REPO` | `owner/repo` of the GitHub Releases |
| `NABOS_UPDATE_ASSET` | bundle asset name for this board, e.g. `nabos-zero2-arm64.raucb` |
| `NABOS_GITHUB_API` / `NABOS_GITHUB_DOWNLOAD` | `https://api.github.com` / `https://github.com` |
| `NABOS_LVA_URL` | `ws://127.0.0.1:6055` |
| `NABOS_LVA_UNIT` | `linux-voice-assistant.service` (empty: voice unsupported on this image) |
| `NABOS_WEATHER_URL` / `NABOS_GEOCODING_URL` | Open-Meteo endpoints |
| `NABOS_TIMESYNC_FILE` | `/run/systemd/timesync/synchronized` (`none`: always trusted, tests only) |
| `NABOS_TIMESYNC_CLOCK` | `/var/lib/systemd/timesync/clock` (timesyncd saved clock, must persist) |
| `NABOS_NET_PROBE` | `api.github.com:443` (TCP reachability test for the belly colour) |

- `GET /healthz`: loopback only, no auth. `200` when MQTT is connected and the core is
  `online`, `503` otherwise. Use it before `rauc status mark-good`.
- D-Bus (system bus) through polkit rules for user `nabos`:
  `de.pengutronix.rauc.Installer` (`InstallBundle`, properties),
  `org.freedesktop.login1.Manager.Reboot/PowerOff`,
  `org.freedesktop.systemd1.Manager.StartUnit/StopUnit` on `$NABOS_LVA_UNIT` only,
  `org.freedesktop.timedate1` `SetTime` and `SetNTP` (actions `set-time`, `set-ntp`).
- Volume: `wpctl set-volume @DEFAULT_AUDIO_SINK@` (`wireplumber`), with
  `XDG_RUNTIME_DIR=/run/user/1000`.
- Time zone: `/etc/localtime` is used; the service embeds tzdata for the configured zone.
- Clock (no RTC on the Pi): *exact* when NTP synchronised or set by hand in the UI
  during this boot (`<data>/clock-manual` holds the boot id); *approximate* when
  timesyncd restored its saved clock (offline cold boot): sleep and wakeup run, the
  time is never announced; *unknown* otherwise: nothing is scheduled. The UI shows
  the state and offers a manual setting (timedated).
- LVA must run with `--peripheral-host 127.0.0.1` (port 6055) and
  `ConditionPathExists=/data/nabos/voice-enabled`.
- Data files: `config.json` (0600, atomic writes; a corrupt file is renamed
  `config.json.corrupt-<time>` and defaults are used), `updates/`, `media/sounds/user/`,
  `voice-enabled`, `clock-manual`.

### Releases

Each release tag `vX.Y.Z` carries the bundles and a `SHA256SUMS` asset
(`sha256sum` format, one line per asset). The service only accepts assets whose URL
is `https://github.com/<repo>/releases/download/<tag>/<name>`, redirects to
`*.githubusercontent.com`, the announced size (≤ 2 GiB) and the listed checksum,
resumes interrupted downloads, then hands the local file to RAUC, which checks the
signature and the `compatible`.

### First password

The first admin password can only be set within 5 minutes after a press on the
rabbit's head button (physical presence). A click followed by a hold erases a
forgotten password. Sessions are in memory (32 at most, 7 days).

## End-to-end test

`(cd services && NABOS_INTEGRATION=1 go test -count=1 ./tests/integration)`
starts Mosquitto, `nab-core --simulate` and
`nab-service`, and checks execution, deduplication, expiration, cancel, retained
refusal, validation, broker and core restarts and the web flow. By default it builds
native binaries; `NAB_CORE_BIN`, `NAB_SERVICE_BIN` (commands, e.g.
`qemu-arm-static -L <sysroot> <sysroot>/usr/bin/nab-core`) and `MOSQUITTO` override them.
