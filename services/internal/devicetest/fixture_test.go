package devicetest

import "testing"

func TestProcessFDAdmission(t *testing.T) {
	fixture := New(t)
	// Probe the real bus credentials independently of service authentication.
	RequireProcessFD(t, fixture.Conn)
}
