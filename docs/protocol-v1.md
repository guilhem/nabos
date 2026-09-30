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

`nab-hardware.service` is `Type=dbus`, with
`BusName=io.github.guilhem.NabHardware1`. It alone receives `CAP_SYS_RAWIO` and
supplementary groups `gpio video kmem`. udev grants those groups the ear/RFID,
GPIO, `/dev/vcio` and `/dev/mem` devices. `/dev/mem` access is root-equivalent.
`nabos-rfid.service` probes the reader before hardware starts.

`nabos.service` receives only `CAP_NET_BIND_SERVICE` for HTTP `:80`.
`PrivateDevices=yes` hides physical devices. Its data is `/data/nabos` and its
volatile working directory is `/run/nabos` (`RuntimeDirectory=nabos`).
Hardware works in `/run/nab-hardware` and has no application data write exception.
All three services run as `nabos` with `NoNewPrivileges=yes` and
`ProtectSystem=strict`; device-core has an empty capability bounding set.
The common account does not belong to `gpio`, `video` or `kmem`.

Bus policies filter accounts. Hardware authenticates the actual `nabos.service`
claimant, and Go authenticates `nab-hardware.service`, using ProcessFD and
systemd GetUnitByPIDFD, without a PID fallback. See the
[hardware contract](hardware-dbus.md) for ownership, event types and cleanup.
Device-core accepts physical presence only from `nab-hardware.service` and
maintenance agents from `nabos.service:nab-hardware.service`, in that order.
The application gate refuses busy activity before hardware is paused, including
silences between book chapters. Both agents must quiesce before maintenance.
After acknowledging release, Go waits for `Manager.Maintenance=false` before
resuming application work.

The root remains read-only. Linux settings stay in
`/data/device-core/settings.json`; application settings, administration,
schedules, tags and media stay under `/data/nabos` (`application.json`).
Application writes are atomic and mode 0600. The persistent home and LVA files
remain under `/var/lib/nabos`, bound from `/data/system` by `boot-init`.
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

Hardware retains `NABOS_GPIO_CHIP` (`/dev/gpiochip0`), `NABOS_BUTTON_GPIO` (`17`),
`NABOS_WS2811_LIB` (`libws2811.so`), `NABOS_LED_BRIGHTNESS` (`200`) and
`NABOS_LED_STRIP` (`grb`). `NABOS_LOG=debug` enables detailed logs.
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
(cd services && NABOS_INTEGRATION=1 go test -race -count=1 ./tests/integration ./cmd/nabos)
```

Simulation requires an explicit private `NABOS_DEVICE_BUS_ADDRESS` for hardware
and Go, and `DEVICE_CORE_BUS_ADDRESS` for device-core. It must never use the host
system bus. Hardware event injection exists only in simulation.
The integration harness accepts `NABOS_HARDWARE_BIN`, `DEVICE_CORE_BIN` and
`NABOS_BIN` command overrides. `image/test.sh` supplies wrappers around the
shipped binaries, using their target loader (QEMU ARM1176 for ARMv6), from a
throwaway copy of the image. `MOSQUITTO` selects the Home Assistant fixture
broker. `DBUS_DAEMON` selects a private test bus with ProcessFD support; the
image verifier always selects its locked D-Bus 1.16.2 wrapper. Simulation does not qualify physical drivers or systemd confinement;
use the [release checklist](release-checklist.md) on both boards.
