package ha

import "testing"

func TestCommandsAreValidated(t *testing.T) {
	b := &Bridge{Node: "nabos_test"}
	for topic, payload := range map[string]string{
		"nabos/nabos_test/sleep/set":     "ON",
		"nabos/nabos_test/volume/set":    "55",
		"nabos/nabos_test/left_ear/set":  "16",
		"nabos/nabos_test/weather/press": "PRESS",
	} {
		if _, ok := b.ParseCommand(topic, []byte(payload)); !ok {
			t.Fatalf("%s %s refused", topic, payload)
		}
	}
	for topic, payload := range map[string]string{
		"nabos/nabos_test/sleep/set":     "maybe",
		"nabos/nabos_test/volume/set":    "250",
		"nabos/nabos_test/right_ear/set": "-1",
		"nabos/other/volume/set":         "10",
		"nabos/nabos_test/reboot/press":  "",
	} {
		if _, ok := b.ParseCommand(topic, []byte(payload)); ok {
			t.Fatalf("%s %s accepted", topic, payload)
		}
	}
	if len(b.Discovery("homeassistant")) != 17 {
		t.Fatal("discovery entities")
	}
	for _, name := range []string{"taichi", "surprise", "eightball", "airquality", "carrot", "birthday", "autopromo", "weather_tomorrow"} {
		if _, ok := b.ParseCommand("nabos/nabos_test/"+name+"/press", []byte("PRESS")); !ok {
			t.Fatal("voice action refused", name)
		}
	}
}
