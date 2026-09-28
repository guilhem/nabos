package bus

import "testing"

func TestHardwareRequiredForUpdateConfirmation(t *testing.T) {
	h := map[string]any{"left_ear": "ok", "right_ear": "ok", "leds": true, "button": true}
	s := CoreState{Hardware: h}
	if !s.HardwareReady() {
		t.Fatal("healthy TagTagTag rejected")
	}
	for key, value := range h {
		delete(h, key)
		if s.HardwareReady() {
			t.Errorf("missing %s accepted", key)
		}
		h[key] = value
	}
	h["left_ear"] = "broken"
	if s.HardwareReady() {
		t.Fatal("broken ear accepted")
	}
	h["simulated"] = true
	if !s.HardwareReady() {
		t.Fatal("explicit simulation rejected")
	}
	if (CoreState{}).HardwareReady() {
		t.Fatal("absent state accepted")
	}
}
