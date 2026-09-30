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
- `Cancel(attempt_id: t)`: cancel only the matching active attempt.
- `Forget(uuid: s)`: delete a saved client profile, never the recovery hotspot.

Go binds the reservation token to an HttpOnly browser cookie, never accepts it
from an arbitrary form field, and admits pre-admin requests only when the actual
socket local address is the active hotspot address `10.41.0.1`. Lease renewal
does not renew physical presence. A radio transition is independent of HTTP
request cancellation. With an administrator, all network routes require login.

Simulation never connects to the system bus or claims this production name.
