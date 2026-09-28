package clock

import (
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

func at(day, h, m int) time.Time { return time.Date(2026, 9, day, h, m, 0, 0, time.UTC) } // 2026-09-28 is a Monday

func TestScheduleAndChime(t *testing.T) {
	c := config.Defaults().Clock // 7:00 - 22:00
	awake, asleep := false, true
	st := &State{Asleep: &awake, LastChime: -1}
	if a := Decide(c, at(28, 22, 0), st, Exact); !a.Sleep || a.Chime {
		t.Fatalf("22:00 should sleep without chime: %+v", a)
	}
	if a := Decide(c, at(28, 23, 0), st, Unknown); a != (Actions{}) {
		t.Fatal("unknown clock must do nothing")
	}
	st.Asleep = &asleep
	if a := Decide(c, at(29, 7, 0), st, Coarse); !a.Wakeup || a.Chime {
		t.Fatalf("coarse clock: wake up but never announce the time: %+v", a)
	}
	if a := Decide(c, at(29, 7, 0), st, Exact); !a.Wakeup || !a.Chime {
		t.Fatalf("7:00 wake up and chime: %+v", a)
	}
	st.Asleep = &awake
	if a := Decide(c, at(29, 7, 0), st, Exact); a.Chime {
		t.Fatal("chime twice in the same hour")
	}
	Decide(c, at(29, 7, 6), st, Exact)
	if a := Decide(c, at(29, 8, 0), st, Exact); !a.Chime || a.Sleep || a.Wakeup {
		t.Fatalf("8:00: %+v", a)
	}
}

func TestOverrideAndOvernightSchedule(t *testing.T) {
	c := config.Defaults().Clock
	yes := true
	c.Override = &yes
	awake := false
	st := &State{Asleep: &awake, LastChime: -1}
	if a := Decide(c, at(28, 15, 0), st, Exact); !a.Sleep || a.Chime || a.ClearOverride {
		t.Fatalf("override to sleep at 15:00: %+v", a)
	}
	if a := Decide(c, at(28, 23, 0), st, Exact); !a.ClearOverride {
		t.Fatal("override must clear once the schedule agrees")
	}
	// Per-day: Monday bedtime 01:30 (Tuesday morning still uses Monday).
	c = config.Defaults().Clock
	c.PerDay = true
	c.Days[0] = config.Day{Wakeup: config.HM{Hour: 8}, Sleep: config.HM{Hour: 1, Min: 30}}
	if ShouldSleep(c, at(29, 1, 0)) {
		t.Fatal("01:00 on Tuesday uses Monday's 01:30 bedtime")
	}
	if !ShouldSleep(c, at(29, 1, 45)) {
		t.Fatal("01:45 should sleep")
	}
}
