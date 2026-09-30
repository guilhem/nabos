# Network D-Bus API

The system bus service `org.nabaztag.Core`, object `/org/nabaztag/Core/Network`,
implements `org.nabaztag.Core.Network1`. The production policy admits only the
NabOS service user; the HTTP service enforces administrator authentication.
NetworkManager owns the radio, connection profiles and secrets.

Read-only properties `Status`, `Networks`, `Profiles` are JSON strings. The
`Changed(status: s)` signal reports progress; clients may also poll properties.
Status contains `mode` (unavailable, reconnecting, hotspot, client), `address`,
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

Simulation never connects to the system bus or claims this production name.
