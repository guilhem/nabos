# NabOS runtime protocol v1

Three programs run on the rabbit. NabOS commands and hardware events use local
Mosquitto (MQTT 5, `127.0.0.1:1883`, anonymous, loopback only); Linux device
operations use direct D-Bus calls to the independent device-core daemon:

- `nab-core` (Rust, `core/`): hardware, state machine, queue and choreographies;
  direct Audio/Network D-Bus client and physical-presence reporter.
- `nab-service` (Go, `services/`): web UI, settings, clock, weather, the nine PyNab
  services (tai-chi, surprise, 8-ball, air quality, IFTTT, webhook, radio, book,
  Mastodon), Home Assistant and application settings; direct device-core client.
- `device-core` (Rust, independent repository): Linux network, audio, system
  settings, clock, SSH, voice transport, signed updates and maintenance.

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
| `nabos/v1/core/event/ear_moved` | core → all | 1 | no | manual ear movement |
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

Sequence item: `{"audio": ["res", ...], "stream": "url", "choreography": "res"}`, all optional,
`audio` and `stream` mutually exclusive.

- Audio resources are resolved against the sounds roots, first in `<root>/<locale>/<res>`
  then `<root>/<res>`. A last path component starting with `*` picks a random match
  (`clock/7/*.mp3`). `a;b` tries `a` then `b`. Only `.mp3` and `.wav`.
- Choreography: a resource under the choreographies roots (`system/rfid.chor`),
  or `urn:x-chor:streaming` / `urn:x-chor:streaming:N` (N = palette 0–7).
- Resources are relative paths without `..`, backslash, NUL, leading `/`,
  256 chars max. Remote URLs are not accepted as resources. Continuous radio
  uses the separate loopback `stream` field below.
- `stream` (`play` only, refused in `message`): exactly
  `http://127.0.0.1:<port>/radio/<token>`, port 1024–65535 in plain decimal, token
  16–64 chars `[A-Za-z0-9_-]`, nothing after it. The service serves it on loopback
  (radio relay); the core hands it to `mpg123` and the item ends when the service
  closes the stream. No other URL is accepted.

| Action | Args | Behaviour |
|---|---|---|
| `play` | `{"sequence":[item ≤32], "cancelable":true}` | Queued, played when idle. Audio ≤16 per item. |
| `message` | `{"signature":item?, "body":[item ≤32], "cancelable":true}` | Queued. Ears to 0, signature, body, signature, default streaming choreography. |
| `cancel` | `{"target":"<id>"?}` | Without `target`: cancels the playing command if it is cancelable. With `target`: cancels that command, even a playing `cancelable: false` one (owner cancel); queued targets are removed and answered `canceled`. Errors `not_playing`, `not_cancelable` (untargeted only). |
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
published, including clicks during a `cancelable: false` command, which the
service may handle and stop with a targeted `cancel`. Shutdown and reboot belong
to the service (logind), triggered by `triple_click`.

The core holds no interactive session or lease: exclusivity between service
features is the service's job. If the service dies, the playing command still ends
by itself (finite audio, or a stream closed with the service) and the queue resumes.

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
{"v":1,"event":"down","edge_monotonic_ns":"9007199254740993","time":1790000000.12}
{"v":1,"left":3,"right":null,"time":1790000000.5}
{"v":1,"ear":"left","time":1790000000.1}
{"v":1,"event":"detected","tech":"st25tb","uid":"d0:02:18:00:00:00:00:01","support":"formatted",
 "locked":false,"picture":42,"app":"clock","data":"\u0000","time":1790000000.7}
```

Button events: `down`, `up`, `click`, `double_click`, `triple_click`, `hold` (2 s),
`click_and_hold` (click, then 2 s press), `double_click_and_hold` (two clicks, then
the third press held 10 s, measured by the core). A third press released within
150 ms is a `triple_click`; released later but before 10 s it emits nothing.
Physical `down` events carry optional `edge_monotonic_ns`: the GPIO kernel
`CLOCK_MONOTONIC` timestamp in nanoseconds as a decimal string, preserving integer
precision. `time` remains the wall-clock publication time. Events without edge
metadata retain their normal gesture behaviour but cannot prove admin presence;
publication or receipt time is never a substitute. Deliberate simulated downs
must supply a timestamp from the same Linux monotonic clock.
`ear_moved` is published as soon as an ear is turned by hand (`ear`: `left` or
`right`); the `ears` event with detected positions still follows after 0.5 s
of stillness. RFID `support`: `formatted`, `foreign-data`, `locked`, `empty`,
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
| `NABOS_GPIO_CHIP` / `NABOS_BUTTON_GPIO` | `/dev/gpiochip0` / `17` |
| `NABOS_WS2811_LIB` | `libws2811.so` (loaded with dlopen, GPIO 13, PWM channel 1, DMA 12) |
| `NABOS_LED_BRIGHTNESS` / `NABOS_LED_STRIP` | `200` / `grb` (`rgb`, `grb`, `brg`…) |
| `RUST_LOG`-like `NABOS_LOG` | `info` (`debug` for traces) |

Runtime needs: device-core’s Audio API and `libws2811.so` in the loader path.
The original hardware drivers, mixer and calibration remain in NabOS.
`NABOS_DEVICE_BUS_ADDRESS` must name an explicit private bus in simulation.

Permissions (no root):

- udev: `/dev/ear0`, `/dev/ear1`, `/dev/rfid0`, `/dev/nfc0` → `GROUP="nabos", MODE="0660"`
  (the drivers create them root 0600).
- `/dev/gpiochip0`: group `gpio`.
- LEDs (rpi_ws281x PWM+DMA): `/dev/mem` read/write and `/dev/vcio` (group `video`).
  Recommended unit: `SupplementaryGroups=gpio video kmem`, udev
  `KERNEL=="mem", GROUP="kmem", MODE="0660"`, `AmbientCapabilities=CAP_SYS_RAWIO`,
  `CapabilityBoundingSet=CAP_SYS_RAWIO`. Access to `/dev/mem` is root-equivalent;
  the capability is confined to this one unit.
- Analog audio must stay disabled (PWM1 on GPIO 13 is used by the LEDs); it is
  off in the vendor DTB, and config.txt dtparams do not reach Linux.

Media: copy `assets/sounds/` into `/usr/share/nabos/sounds/` and
`assets/choreographies/` into `/usr/share/nabos/choreographies/`.
Keep locale directories and resource paths intact.

### device-core

`/usr/bin/device-core [--simulate]`, `device-core.service`, `User=nabos`, no
hardware capability. Its source is the commit archive pinned as
`sources.device_core` in `image/sources.lock.json`, outside this repository.
Only its binary and notices are installed; sources, Cargo vendor trees and build
caches stay in archived build inputs.

The system bus name is `io.github.guilhem.DeviceCore1`. Manager is at
`/io/github/guilhem/DeviceCore1`, interface `io.github.guilhem.DeviceCore1.Manager`;
other domains use `/io/github/guilhem/DeviceCore1/<Domain>` and interface
`io.github.guilhem.DeviceCore1.<Domain>`.

| Domain | Contract |
|---|---|
| Manager | `Ready:b`, `Maintenance:b`, `Instance:s`, `Version:s`, `Capabilities:as`; `RegisterAgent(path:o)` |
| Network | Typed properties and leased network FD; [full contract](network-dbus.md) |
| Audio | `Start(kind:s,source:s)→id:s`, `Stop(id:s)`, `Wait(id:s)→outcome:s`, `SetVolume(percent:u)`; `Status:(ssu)` |
| Config | `Read()→(revision:s,settings:(ssub(bs(uu)(uu))b))`, `Update(revision:s,settings)→revision:s` |
| System | `Reboot()`, `PowerOff()`, `SetTime(unix_microseconds:x)`, `GetSSHKeys()→s`, `SetSSHKeys(s)`, `Clock()→(s,x)`, `Connectivity()→s` |
| Voice | `Supported:b`, `Status:s`, `Enable(b)`, `Command(s)`; `Event(event:s,data_json:s)` |
| Updates | `Check()`, `Releases(channel:s)`, `Install(tag:s,channel:s,automatic:b,retry:b)→operation_id:s`, `Reconcile()` and typed `Status` |

Config settings fields, in wire order: locale, timezone, volume,
auto_check_updates, updates (automatic, channel, start HM, end HM), voice_enabled.
HM fields are hour/minute unsigned integers. Revision includes the daemon
incarnation; stale writes fail rather than silently overwriting settings.
Applications never enter this store. Audio playback IDs also include the
incarnation; a stale Stop cannot cancel a newer playback. D-Bus playback is
bound to the actual caller connection. File sources are bounded to configured
roots; streaming uses literal loopback HTTP URLs and no proxy or redirect.

The image sets these Options variables (generic defaults are in device-core’s
`src/options.rs`):

| Variable | NabOS value |
|---|---|
| `DEVICE_CORE_DATA_DIR` | `/data/device-core` |
| `DEVICE_CORE_NETWORK_GUARD` | `/run/lock/device-core/network` |
| `DEVICE_CORE_AUDIO_ROOTS` | `/usr/share/nabos/sounds:/data/nabos/media/sounds` |
| `DEVICE_CORE_ALSA_DEVICE` | `default` through pipewire-alsa |
| `DEVICE_CORE_PRESENCE_UNIT` | `nab-core.service` |
| `DEVICE_CORE_MAINTENANCE_UNITS` | `nab-core.service:nab-service.service` |
| `DEVICE_CORE_HOTSPOT_PREFIX` | `Nabaztag-` |
| `DEVICE_CORE_LVA_UNIT` | `linux-voice-assistant.service` only when `/opt/linux-voice-assistant/.venv/bin/python` is executable; otherwise empty |
| `DEVICE_CORE_BOOT_HEALTH` | `/run/nabos-boot-health` |
| `DEVICE_CORE_IMAGE_VERSION` | image release, independent of the crate version |
| `DEVICE_CORE_UPDATE_REPO` / `DEVICE_CORE_UPDATE_ASSET` | release repository / `nabos-<target>.raucb` |
| `DEVICE_CORE_HTTP_ADDR` | unset: HTTP disabled |
| `HOME` / `XDG_RUNTIME_DIR` | `/var/lib/nabos` / `/run/user/1000` |

The unit uses `RuntimeDirectory=device-core`, `WorkingDirectory=/run/device-core`
and `ProtectSystem=strict`. `PrivateDevices=yes` hides hardware devices from
device-core; ALSA `default` connects to the shared PipeWire session. Writable paths are `/data/device-core`,
`/var/lib/nabos`, `/run/device-core` and `/run/lock/device-core`. The root stays
read-only. Persistent files are `settings.json`, `updates/`, `clock-manual`,
`ssh/authorized_keys` and `voice-enabled` under `/data/device-core`; file modes are
0600 and directories private. LVA’s preferences and downloaded wakewords remain
under `/var/lib/nabos/lva`, bound from `/data/system` by the existing boot-init.
The volatile `/data/.volatile` fallback cannot provide persistence.

Polkit grants NetworkManager mutations, power/time and runtime SSH/NTP/LVA jobs
only to the exact `device-core.service` subject with NoNewPrivileges. D-Bus
policy admits the shared nabos account; physical presence and maintenance agents
are authorized by the daemon from bus credentials and systemd unit identity.
RAUC bus rules retain root health access and narrowly admit the shared account
for InstallBundle/properties/introspection; they cannot distinguish its units.
OpenSSH uses `/data/device-core/ssh/authorized_keys`; host keys remain under
`/data/system/ssh/etc/ssh`. Voice enablement is conditioned by
`/data/device-core/voice-enabled`.

### nab-service

`/usr/bin/nab-service`, system service, `User=nabos`. The image sets
`NABOS_HTTP_ADDR=:80` with `CAP_NET_BIND_SERVICE`; local builds use `:8080`.
Go owns HTTP authentication, applications and MQTT; all device operations go
directly to device-core over D-Bus. It does not implement NetworkManager,
RAUC, system clock, SSH or LVA policy.

| Variable | Default |
|---|---|
| `NABOS_MQTT_HOST` / `NABOS_MQTT_PORT` | `127.0.0.1` / `1883` |
| `NABOS_HTTP_ADDR` | `:8080` |
| `NABOS_DATA_DIR` | `/data/nabos` (`application.json`, application media) |
| `NABOS_SOUNDS_DIRS` | same media roots as nab-core |
| `NABOS_VERSION` | running image version, set by the release environment/build |
| `NABOS_WEATHER_URL` / `NABOS_GEOCODING_URL` | Open-Meteo endpoints |

`application.json` stores administration, application configuration, schedules,
tags and Mastodon credentials/state. Its writes are atomic and mode 0600.
There is no migration or compatibility layer for the old config.json or old
D-Bus name/API. Secrets are not prefilled in web forms.

Pre-admin Wi-Fi setup is admitted only on the socket’s actual hotspot address
`10.41.0.1`, with fresh physical proof for its reservation. Once administration
exists, every network route requires login. NetworkManager provides DHCP and
captive DNS; LAN connectivity is enough to accept a candidate connection.

Only one Go media owner is active at a time. Book/8-ball commands keep ownership
by ID; radio is preempted by audio/sleep. The loopback radio relay uses a bounded
copy buffer and never downloads a whole stream to disk.

`GET /healthz` is loopback-only and verifies MQTT, nab-core availability and
hardware readiness. Before `rauc status mark-good`, the root health script also
requires all three units active and `Manager.Ready` via D-Bus, plus real audio
sink/capture and persistent mounts. Device-core HTTP is never required.

### Releases

Each release tag `vX.Y.Z` (including SemVer prereleases such as `v1.2.0-rc.1`) carries the bundles and a `SHA256SUMS` asset
(`sha256sum` format, one line per asset). device-core only accepts assets whose URL
is `https://github.com/<repo>/releases/download/<tag>/<name>`, redirects to
`*.githubusercontent.com`, the announced size (≤ 2 GiB) and the listed checksum,
resumes interrupted downloads, then hands the local file to RAUC, which checks the
signature and the `compatible`.

The local authenticated `/updates` page lists newer releases and publication
notes, with a `stable` channel (default) or `test` channel (including prereleases).
`POST /updates/settings` accepts `mode=manual|notify|auto`, `channel=stable|test`,
and `start`/`end` local times in `HH:MM` form. Settings retain
`auto_check_updates` and add `updates` with `automatic`, `channel`, `start`, and
`end` (`hour`/`min`). Missing fields default to automatic installation disabled,
stable channel, 03:00–05:00. `POST /updates/check` refreshes the catalogue;
`POST /updates/install` selects a `tag`, with `retry=true` for an explicit manual
retry after a failure. Installations never accept a download URL from the browser.

Automatic checks start after five minutes, then run daily; policy changes trigger
a check. The installation and reboot gates are evaluated every minute. Automatic
updates require a confirmed current boot, an exact clock, the configured window
and an idle core/media/voice stack. Windows use the device timezone and may cross
midnight. One automatic attempt is recorded per window start date, including
across service restarts and daylight-saving changes. A started RAUC write finishes
even when the window closes; an automatic reboot waits for another eligible window.
Manually requested installations keep a manual reboot.

The update journal is `/data/device-core/updates/state.json`. It records the pending
bundle, source/target slots, boot identity, origin, phase, last automatic attempt
and blocked versions using an atomic, durable write. The root-owned runtime
marker `/run/nabos-boot-health` is published by the health script only after
successful `mark-good`; the script can also mark an exhausted system for manual
repair. A rollback blocks that version from automatic installation. Ambiguous
recovery or an unreadable journal suspends automatic installation until an
explicit manual recovery. Catalogue refreshes never replace installation state.

### First password

The first admin password requires opening `/setup` after the client network is
ready and its phase is no longer `connecting`, then pressing the rabbit's head
button again. Setup starts disarmed, including after a service restart. Submitting
pre-admin Wi-Fi `Connect` disarms and clears presence before the D-Bus call, even
if its result is ambiguous. Unavailable, nonready or connecting network status
also disarms setup. The first ready `/setup` request arms it with a monotonic
cutoff; reloads and form errors preserve this cutoff and any fresh proof.
Only a `down` with `cutoff < edge_monotonic_ns <= CLOCK_MONOTONIC now` and an age
strictly less than 5 minutes can prove presence. Expiry uses the original edge,
so delayed delivery and duplicate events cannot renew it. Setup checks presence
again atomically at the settings commit boundary. Two clicks then a third press held 10 s
(`double_click_and_hold`) erase a forgotten password. Sessions are in memory
(32 at most, 7 days).

## End-to-end test

`(cd services && NABOS_INTEGRATION=1 go test -race -count=1 ./tests/integration ./cmd/nab-service)`
starts Mosquitto, a private D-Bus daemon, `device-core --simulate`,
`nab-core --simulate` and `nab-service`, and checks execution, deduplication, expiration, cancel, retained
refusal, validation, broker and core restarts, the web flow, exclusive interactive
playback and RFID dispatch. By default it builds
native binaries; `NAB_CORE_BIN`, `DEVICE_CORE_BIN`, `NAB_SERVICE_BIN` (commands, e.g.
`qemu-arm-static -L <sysroot> <sysroot>/usr/bin/nab-core`) and `MOSQUITTO` override them.
