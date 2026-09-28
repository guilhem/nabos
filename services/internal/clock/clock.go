// Package clock handles the hourly chime and sleep/wakeup schedule.
package clock

import (
	"time"

	"github.com/guilhem/nabos/services/internal/config"
)

// Schedule returns the wakeup and sleep times that apply at now. Until 3 am
// the previous day's settings apply, so a bedtime after midnight works.
func Schedule(c config.Clock, now time.Time) (wakeup, sleep config.HM) {
	if !c.PerDay {
		return c.Wakeup, c.Sleep
	}
	day := now.Add(-3 * time.Hour).Weekday()
	d := c.Days[(int(day)+6)%7] // Monday first
	return d.Wakeup, d.Sleep
}

func before(h, m int, t config.HM) bool { return h < t.Hour || (h == t.Hour && m < t.Min) }

// ShouldSleep evaluates the configured sleep and wakeup times.
func ShouldSleep(c config.Clock, now time.Time) bool {
	w, s := Schedule(c, now)
	h, m := now.Hour(), now.Minute()
	if before(w.Hour, w.Min, s) { // wakeup < sleep
		return before(h, m, w) || !before(h, m, s)
	}
	return before(h, m, w) && !before(h, m, s)
}

// State kept by the scheduler between ticks.
type State struct {
	Asleep    *bool // last known core state, nil while unknown or changing
	LastChime int   // hour of last chime, -1 when none
}

type Actions struct {
	ClearOverride bool
	Sleep, Wakeup bool
	Chime         bool
}

// Quality of the system clock (the Pi has no RTC).
type Quality int

const (
	Unknown Quality = iota // no trustworthy time: nothing scheduled
	Coarse                 // restored from the last saved time, not synchronised
	Exact                  // NTP synchronised or set by hand since boot
)

// Decide is evaluated every minute. A coarse clock still drives sleep and
// wakeup (the rabbit works without Internet) but never announces the time.
func Decide(c config.Clock, now time.Time, st *State, q Quality) Actions {
	var a Actions
	if q == Unknown {
		return a
	}
	should := ShouldSleep(c, now)
	if c.Override != nil {
		if should == *c.Override {
			a.ClearOverride = true
		} else {
			should = *c.Override
		}
	}
	if st.Asleep != nil && should != *st.Asleep {
		a.Sleep, a.Wakeup = should, !should
	}
	if q == Exact && !should && now.Minute() == 0 && c.Chime && st.LastChime != now.Hour() {
		a.Chime = true
		st.LastChime = now.Hour()
	}
	if now.Minute() > 5 { // account for time drifts
		st.LastChime = -1
	}
	return a
}
