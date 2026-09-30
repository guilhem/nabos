package devicetest

import "testing"

func TestProcessFDAdmission(t *testing.T) {
	fixture := New(t)
	// Probe the bus before any hardware owner or systemd fixture exists.
	RequireProcessFD(t, fixture.Conn)
}
