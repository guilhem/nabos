# Nabaztag hardware D-Bus API

`nab-hardware` owns `io.github.guilhem.NabHardware1` on the system bus, at
`/io/github/guilhem/NabHardware1`, with the interface of the same name. It handles
only hardware; states, media and choreographies belong to the Go `nabos` process.

## Wire contract

| Member | Arguments / return | Meaning |
|---|---|---|
| `Ready` property | `b` | Required ears, LEDs and button are available; RFID is optional. |
| `Status` property | `(sbssbbsnn)` | model, simulated, left ear status, right ear status, LEDs, button, reader kind, left position, right position. Positions are -1 when unknown. |
| `Claim()` | no arguments or return | Acquire exclusive hardware control for the calling connection. |
| `Release()` | no arguments or return | Drain admitted operations and relinquish control. |
| `SetLeds(colors)` | `a(yyyy)` | Up to five distinct (index, red, green, blue) entries; indices 0–4. |
| `PulseLed(index, red, green, blue)` | `yyyy` | Hardware LED pulsing; indices 0–4. |
| `MoveEar(index, position, backward)` | `yyb` | Index 0–1; driver positions 0–255, including extra turns. |
| `StepEar(index, steps, backward)` | `yyb` | Index 0–1; driver steps 0–255. |
| `ReadEars(detect)` | `b` → `nn` | Read positions, optionally run physical detection; -1 means unknown. |
| `WaitEarsIdle()` | no arguments or return | Wait for already transmitted movements. |
| `StartWrite(tech, uid, picture, app, data, timeout)` | `sayyyayu` → `t` | tech is `st25tb` or `iso14443a_t2t`; data ≤32 bytes, timeout 1–60 seconds. |
| `WaitWrite(id)` | `t` → `s` | `completed`, `canceled`, `timeout`, `write-failed`, or `no-reader`. |
| `CancelWrite(id)` | `t` | Cancel a pending write; an indivisible write already admitted completes. |
| `Changed(status)` signal | `(sbssbbsnn)` | Current hardware snapshot, also published through PropertiesChanged. |
| `Button(gesture, edge)` signal | `st` | Original CLOCK_MONOTONIC nanoseconds for `down`; zero for other gestures. |
| `EarMoved(index)` signal | `y` | Manual ear movement, 0 left / 1 right. |
| `Tag(removed, tech, uid, support, locked, formatted, picture, app, data)` signal | `bsaysbbyyay` | UID and payload retain their bytes; numeric application identifiers are interpreted by Go. |

All mutators require a claim owned by the same unique D-Bus connection. Claim
authenticates the unique sender as the `nab-app` Unix account using
`org.freedesktop.DBus.GetConnectionUnixUser`. Authorization is rechecked before a
deferred mutation. Go verifies that the service owner belongs to
`nab-hardware` before trusting status and events. Maintenance callbacks require
the current device-core owner and its `device-core` Unix account. Explicit
`NABOS_APP_USER`, `NABOS_HARDWARE_USER` and `NABOS_DEVICE_USER` overrides select
alternate account names, including on private test buses; simulation never bypasses
authentication.

Loss of the owner drops pending work, ends pulses and clears LEDs. Already sent
ear movements and indivisible RFID writes finish before control can be reclaimed.
Commands are not replayed after reconnection. Long operations and cleanup have
bounded waits; a timeout never proves that physical work has stopped.

## Maintenance and presence

Hardware and application separately export the existing device-core maintenance
agent interface. Hardware blocks mutations and waits for actual quiescence;
the application holds its complete activity gate, including chapter silences.
Recovery stays blocked until the current authenticated daemon confirms safety.

Only the physical GPIO reader calls device-core `Network.ReportPresence`. The
trusted presence account is `nab-hardware`; HTTP cannot create that proof.

## Simulation

`--simulate` requires an explicit private `NABOS_DEVICE_BUS_ADDRESS`. Hardware
simulation and test event injection never use the host system bus. Test-only
injection is exposed exclusively in simulation through
`io.github.guilhem.NabHardware1.Simulation` at the same object path: `Button(st)`,
`EarMoved(y)` and `Tag(bsaysbbyyay)` emit the corresponding typed events.
