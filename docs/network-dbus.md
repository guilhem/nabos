# Network D-Bus API

The independent system bus service `io.github.guilhem.DeviceCore1`, object
`/io/github/guilhem/DeviceCore1/Network`, implements
`io.github.guilhem.DeviceCore1.Network`. nab-core and nab-service are direct
D-Bus clients; nab-core has no NetworkManager controller. The production policy admits only the
NabOS service user; the HTTP service enforces administrator authentication.
NetworkManager owns the radio, connection profiles and secrets.

Read-only properties are typed: `Status:(ssbsaystss)`,
`Networks:a(ayys)` and `Profiles:a(say)`. `Changed(status:(ssbsaystss))` and
PropertiesChanged report progress; clients may also poll properties.
Status field order is `mode` (unavailable, reconnecting, hotspot, client),
`generation` (opaque incarnation plus counter), `ready` (boolean), `address`,
`ssid` (byte array), `profile_uuid`, `attempt_id` (unsigned integer), `phase`
(idle, connecting, succeeded, failed, cancelled), and `error` (safe error code).
Networks contain `ssid` (byte array), `strength` (0–100), `security` (open,
wpa-psk, sae, unsupported). Profiles contain `uuid` and `ssid` (byte array).
No property or signal contains a secret or a setup reservation token.

Reusing a saved profile leaves its settings, secrets and autoconnect preference
unchanged. New profiles start in memory with autoconnect disabled. Once the
candidate has a usable client address, it is saved with its candidate marker;
only a successful checkpoint commit permits its promotion to an ordinary saved
profile with autoconnect enabled. Reconciliation removes orphaned candidates
when no checkpoint protects them, including candidates saved before a restart.
Candidates are excluded from the saved-profile list and automatic retries.

If the final save reply is lost, the core rediscovers NetworkManager and checks
the profile by UUID before reporting success or cleaning up. Promotion and
cleanup use NetworkManager's `VersionId` and conditional `Update2` so a late
promotion cannot race with candidate deletion, even after a core restart. An
empty successful update also [increments the version in NetworkManager 1.52](https://github.com/NetworkManager/NetworkManager/blob/1.52.0/src/core/settings/nm-settings.c#L1161-L1183).
An unavailable or inconclusive result reports `commit-unconfirmed` and leaves
recovery to reconciliation. A crash between checkpoint destruction and promotion can discard
the candidate; the existing reconnection grace and hotspot recovery still apply.

Methods (D-Bus signatures):

- `Scan()`: request an explicit scan; retain cached results on temporary failure.
- `Reserve(token: s) -> s`: create a hotspot reservation with an empty token,
  or renew the matching token for five minutes. Only one reservation is held.
- `Authorized(token: s) -> b`: whether a physical press after this reservation
  has authorized it, with a separate five-minute, one-use expiry.
- `Release(token: s)`: release the matching reservation.
- `Connect(ssid: ay, security: s, password: s, uuid: s, token: s) -> t`:
  submit a connection and return its attempt ID before changing the radio.
  An existing profile is selected by UUID; otherwise SSID is 1–32 bytes.
  An empty token is for authenticated administration. A nonempty token requires
  the hotspot and physical authorization, consumed once on acceptance.
- `Cancel(attempt_id: t)`: cancel only the matching active attempt. Accepted
  cancellations roll back; after the final commit boundary, the method rejects
  cancellation with `attempt-committing`.
- `Forget(uuid: s)`: delete a saved client profile, never the recovery hotspot.

Go binds the reservation token to an HttpOnly browser cookie, never accepts it
from an arbitrary form field, and admits pre-admin requests only when the actual
socket local address is the active hotspot address `10.41.0.1`. Lease renewal
does not renew physical presence. A radio transition is independent of HTTP
request cancellation. With an administrator, all network routes require login.

The physical press consumed by pre-admin Wi-Fi setup does not authorize creating
the administrator. Go disarms and clears admin presence before submitting
`Connect`, including when the D-Bus result is ambiguous. After reconnecting to the
client network, open `/setup`, then press the button again. Go serializes this
readiness check and arming with connection submission: only `mode=client` with a
usable address and `phase != connecting` can arm setup. Unavailable or nonready
status disarms it. Reloads and form errors do not rearm or clear fresh proof.
The optional MQTT `down.edge_monotonic_ns` decimal string carries the original
GPIO `CLOCK_MONOTONIC` edge (see [protocol-v1.md](protocol-v1.md)). Admin proof must
be newer than the arming cutoff, not in the future, and less than five minutes
old; missing metadata, other gestures, delayed old edges and duplicates cannot
renew proof. Service restarts require opening setup and a new press.

`ReportPresence(monotonic_ns:t)` is D-Bus-only. The daemon resolves the actual
sender from bus credentials and systemd, and accepts only `nab-core.service`
configured through `DEVICE_CORE_PRESENCE_UNIT`. MQTT delivery alone does not
prove presence to the Linux network controller. HTTP never exposes this method.

`AcquireGuard(expected_generation:s)→h` checks a ready client connection and
returns an already shared-flocked FD. Keep it open during the protected transfer
or operation; closing the last duplicate releases the flock. All controller
mutations, including scan and recovery, hold an exclusive flock. The lock is
`/run/lock/device-core/network`, created by tmpfiles with no age or truncation,
in a root-owned directory. It is outside RuntimeDirectory cleanup and cannot be
replaced by the daemon. Surviving client FDs protect the same inode across daemon
restarts; an old generation is rejected after a new incarnation starts.

Simulation requires an explicit private bus and never invokes host
NetworkManager. The old `org.nabaztag.Core` service and JSON property protocol
are removed; no compatibility or migration is provided.
