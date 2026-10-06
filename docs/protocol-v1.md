# NabOS runtime architecture

NabOS runs three processes on the system D-Bus. Local application and hardware
traffic uses typed D-Bus; no MQTT broker is installed on the device.

| Process | Responsibility | Contract |
|---|---|---|
| `nab-hardware` (Rust, `core/`) | Ears, LEDs, physical button, RFID/NFC; low-level driver timing and physical presence | [NabHardware1](hardware-dbus.md), root `/io/github/guilhem/NabHardware1` |
| `nabos` (Go, `services/cmd/nabos`) | State machine, media queue, choreography, application scheduling, HTTP administration and Home Assistant | Direct clients of hardware and device-core |
| `device-core` (independently pinned Rust source) | NetworkManager, audio through PipeWire, Linux configuration, clock, SSH, voice and RAUC updates | `io.github.guilhem.DeviceCore1`, root `/io/github/guilhem/DeviceCore1`; [network contract](network-dbus.md) |

Go owns playback and choreography sequencing. Device-core plays audio and
reports progress over D-Bus; Go sends typed LED and ear operations to hardware.
Rust owns driver timing and calibration, without application states or media.
Home Assistant connects to its separately configured MQTT broker. MQTT tooling
on build/test hosts serves only Home Assistant fixtures.

## Units and permissions

`nab-hardware.service` is `Type=notify`, with `NotifyAccess=main` and a one-second
systemd watchdog. It exports `io.github.guilhem.NabHardware1` on D-Bus.
Calibration starts only after systemd acknowledges readiness; the D-Bus `Ready`
property remains false while required hardware is initializing. Worker health
gates watchdog notifications, and `ExecStopPost` invokes
`/usr/bin/nab-hardware --stop-hardware` after process exit. The helper requests
motor outputs low first, then clears all five LEDs and waits for the controller
sync. Both stops are attempted; either error produces a nonzero exit status.
See [ear lifecycle and supervision](hardware-dbus.md#ear-lifecycle-and-supervision)
for recovery behavior and qualification limits.

Hardware has only the supplementary `gpio` group and no capabilities.
Ears use `gpiocdev` directly through `/dev/gpiochip*`, without `/dev/ear*` devices
or an ear kernel module. LEDs use the five Linux multicolor devices at
`/sys/class/leds/multi:indicator-0` through `multi:indicator-4`, with sysfs write
access confined to hardware. No userspace LED DMA or `/dev/mem` access is needed.
The image must expose writable LED attributes inside the confined service.
udev assigns only `/dev/i2c-1` to the dedicated `nab-hardware` group with mode
`0660`; other service accounts have no membership in that group. `i2c-dev` loads
through `modules-load.d`, and the slot's Linux DTB enables bus 1. The udev
`systemd` tag exposes `dev-i2c\x2d1.device`; hardware requires and starts after
that device unit, once udev has applied its permissions. Bus 1 remains required
for the audio codec even when no tag reader is present. Hardware probes
the reader at address `0x50` and uses the external Rust `cr14` or `st25r391x`
crate directly over I²C. Their Git revisions are pinned in `core/Cargo.toml` and
`core/Cargo.lock` and included in Cargo vendor inputs. There are no CR14 or
ST25R391x kernel modules, reader overlays, `/dev/rfid0` or `/dev/nfc0`, or separate
reader probe unit.

`nabos.service` receives only `CAP_NET_BIND_SERVICE` for HTTP `:80`.
`PrivateDevices=yes` hides physical devices. Its data is `/data/nabos` and its
volatile working directory is `/run/nabos` (`RuntimeDirectory=nabos`).
Hardware works in `/run/nab-hardware` and has no application data write exception.
The application runs as `nab-app`, hardware as `nab-hardware`, and system
services as `device-core`. These locked accounts have no login shell.
`nabos` is reserved for SSH administration. `NoNewPrivileges=yes` and
`ProtectSystem=strict` remain enabled, and device-core has no capabilities.

D-Bus policies reserve each service name to its dedicated account. Clients
authenticate the bus-supplied Unix UID and pin the connection's unique name,
including across asynchronous replies. ProcessFD and systemd unit lookups are
not required. Device-core accepts physical presence only from `nab-hardware`
and maintenance agents from `nab-app:nab-hardware`, in that order. Both agents
must quiesce before maintenance. After acknowledging release, Go waits for
`Manager.Maintenance=false` before resuming application work.

PipeWire, WirePlumber and LVA use the separate `nab-audio` account and session
`user@1004`. Device-core connects through the group-restricted native socket
`/run/nabos-audio/pipewire-0`; it can read application media through `nab-media`
without accessing application settings.

The root remains read-only. Linux settings stay in
`/data/device-core/settings.json`; application settings, administration,
schedules, tags and media stay under `/data/nabos` (`application.json`).
Settings writes are atomic and mode 0600. Uploaded sounds use mode 0640 so
device-core can read them through `nab-media`; boot also updates existing sounds.
The operator home and LVA files
remain under `/var/lib/nabos`, bound from `/data/system` by the NixOS initrd.
There is no migration of the former MQTT protocol, API names or settings.

## Configuration

| Application variable | Default |
|---|---|
| `NABOS_HTTP_ADDR` | `:8080` locally, `:80` in the image |
| `NABOS_DATA_DIR` | `/data/nabos` |
| `NABOS_SOUNDS_DIRS` | `/usr/share/nabos/sounds:/data/nabos/media/sounds` |
| `NABOS_CHOREOGRAPHIES_DIRS` | `/usr/share/nabos/choreographies:/data/nabos/media/choreographies` |
| `NABOS_VERSION` | Build/release version |
| `NABOS_WEATHER_URL` / `NABOS_GEOCODING_URL` | Open-Meteo endpoints |
| `NABOS_DEVICE_BUS_ADDRESS` | System bus in production; explicit private bus for tests |

An explicit address applies to hardware and every Go device-core connection,
including the dedicated audio owner, settings and maintenance. Connection
failure never falls back to the system bus.

Hardware retains `NABOS_GPIO_CHIP` (`/dev/gpiochip0`), `NABOS_BUTTON_GPIO` (`17`)
and `NABOS_LED_BRIGHTNESS` (`200`). `NABOS_LED_SYSFS` (`/sys/class/leds`) selects
the LED class root, including temporary fixtures. The driver validates each
`max_brightness` as 255 and maps RGB through its `multi_index`; wire GRB encoding
belongs to the kernel driver. The overlay declares RGB components and labels,
without selectable wire order. `NABOS_WS2811_LIB` and `NABOS_LED_STRIP`
are removed. `NABOS_LOG=debug` enables detailed logs.

LED Set/Pulse replies report asynchronous sysfs acceptance, with no atomic
physical frame guarantee. Initialization and Clear write brightness zero to all
five LEDs, then write `1\n` to the first LED's controller-wide `sync` attribute.
The kernel waits for queued work and retransmits the complete desired frame,
with a 100 ms bound per transfer and error propagation. Maintenance stays
blocked after a failed Clear. Sync completion reports kernel completion,
and requires separate electrical qualification. See
[LED completion and shutdown](hardware-dbus.md#led-completion-and-shutdown).
Device-core's source pin and settings contract are independent of NabOS's
revision. Its image environment and access policy are described in the
[build guide](build.md).

## Health and testing

Loopback-only `GET /healthz` returns
`{hardware: bool, device: bool, engine: bool, version: string}` and fails when a
required runtime component is not ready. Home Assistant is optional.
Before confirming the RAUC slot, the root health script also requires the three
active units, `DeviceCore1.Manager.Ready`, `NabHardware1.Ready`, persistent data
mounts and real TagTagTag playback/capture cards. It does not use device-core HTTP.

```sh
cargo build --locked --manifest-path core/Cargo.toml
(cd services && NABOS_INTEGRATION=1 go test -race -count=1 -skip '^TestEndToEnd$' ./tests/integration ./cmd/nabos)
```

The full `TestEndToEnd` scenario requires root and distinct service UIDs. CI and
`image/test.sh` run it in private mount/network namespaces with a temporary
account database and a private EXTERNAL bus; host accounts are unchanged.
Simulation requires an explicit private `NABOS_DEVICE_BUS_ADDRESS` for hardware
and Go, and `DEVICE_CORE_BUS_ADDRESS` for device-core. It must never use the host
system bus. Hardware event injection exists only in simulation.
The integration harness accepts `NABOS_HARDWARE_BIN`, `DEVICE_CORE_BIN` and
`NABOS_BIN` command overrides. `image/test.sh` supplies wrappers around the
shipped binaries, using their target loader (QEMU ARM1176 for ARMv6), from a
throwaway copy of the image. `MOSQUITTO` selects the Home Assistant fixture
broker. `DBUS_DAEMON` can select an alternative private test bus. Simulation does not qualify physical drivers or systemd confinement;
use the [release checklist](release-checklist.md) on both boards.
