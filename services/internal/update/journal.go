package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"
)

const (
	phaseInstalling = "installing" // recorded before RAUC is called
	phaseInstalled  = "installed"  // RAUC reported success, the new slot boots next
)

// BootState is what RAUC and the health check report about this boot.
type BootState struct {
	BootID    string
	Slot      string // booted RAUC slot (bootname)
	Health    string // "good" (slot confirmed), "stranded" (left unconfirmed) or "" (undecided)
	Operation string // RAUC operation, "idle" when no installation runs
}

type pending struct {
	Tag       string `json:"tag"`
	SHA256    string `json:"sha256"`
	From      string `json:"from"` // version running when installing
	FromSlot  string `json:"from_slot"`
	ToSlot    string `json:"to_slot"`
	Channel   string `json:"channel"`
	BootID    string `json:"boot_id"`
	Phase     string `json:"phase"`
	Automatic bool   `json:"automatic"`
}

func otherSlot(slot string) string {
	switch slot {
	case "A":
		return "B"
	case "B":
		return "A"
	default:
		return ""
	}
}

// journal is the durable installation record (Dir/state.json), written only
// at lifecycle steps, never for progress.
type journal struct {
	Pending    *pending          `json:"pending,omitempty"`
	Target     string            `json:"target,omitempty"`
	LastWindow string            `json:"last_window,omitempty"`
	LastResult string            `json:"last_result,omitempty"`
	Suspended  string            `json:"suspended,omitempty"` // automatic updates suspended, why
	Blocked    map[string]string `json:"blocked,omitempty"`   // tag -> why automatic updates skip it
}

func (j *journal) block(tag, reason string) {
	if j.Blocked == nil {
		j.Blocked = map[string]string{}
	}
	j.Blocked[tag] = reason
}

func (u *Updater) journalPath() string { return filepath.Join(u.Dir, "state.json") }

// journal returns the installation journal, loaded on first use (u.mu held).
// An unreadable journal could hide an installation in progress: it is
// archived and replaced by one suspending automatic updates. If that cannot
// be written, the unreadable journal stays and suspends again after a restart.
func (u *Updater) journal() *journal {
	if u.j != nil {
		return u.j
	}
	path := u.journalPath()
	j := &journal{}
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, j)
	}
	if p := j.Pending; err == nil && p != nil {
		_, versionOK := parseVersion(p.Tag)
		if !versionOK || p.BootID == "" || otherSlot(p.FromSlot) == "" || p.ToSlot != otherSlot(p.FromSlot) || p.Phase != phaseInstalling && p.Phase != phaseInstalled {
			err = errors.New("invalid pending installation")
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		archive := fmt.Sprintf("%s.unreadable-%d", path, time.Now().UnixNano())
		msg := "the update journal was unreadable (kept as " + filepath.Base(archive) + "); automatic updates suspended"
		j = &journal{Suspended: msg, LastResult: msg}
		if os.Rename(path, archive) == nil && writeAtomic(path, j) != nil {
			os.Rename(archive, path)
		}
	}
	u.j = j
	return j
}

// commit applies fn to a copy of the journal and keeps it once durable (u.mu held).
func (u *Updater) commit(fn func(*journal)) error {
	next := *u.journal()
	next.Blocked = maps.Clone(next.Blocked)
	if next.Pending != nil {
		p := *next.Pending
		next.Pending = &p
	}
	fn(&next)
	if err := writeAtomic(u.journalPath(), &next); err != nil {
		return err
	}
	u.j = &next
	return nil
}

func writeAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ClaimWindow records an automatic attempt for window; false when that
// window was already claimed.
func (u *Updater) ClaimWindow(window string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if window == "" || u.journal().LastWindow == window {
		return false, nil
	}
	err := u.commit(func(j *journal) { j.LastWindow = window })
	return err == nil, err
}

// Reconcile compares the journal with RAUC and this boot, records the outcome
// of a finished installation and refreshes Status. It leaves a running
// installation of this process alone.
func (u *Updater) Reconcile() error {
	if !u.Configured() || !u.operation.TryLock() {
		return nil
	}
	defer u.operation.Unlock()
	_, err := u.reconcile()
	return err
}

func (u *Updater) reconcile() (BootState, error) {
	boot, err := u.Probe()
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reconcileLocked(boot, err)
}

func (u *Updater) reconcileLocked(boot BootState, err error) (BootState, error) {
	if err != nil {
		return boot, fmt.Errorf("cannot read the boot state: %w", err)
	}
	j := u.journal()
	state := "idle"
	if boot.Operation != "idle" {
		state = "installing"
	}
	suspend := func(msg string) error {
		return u.commit(func(j *journal) {
			j.Pending, j.Suspended, j.LastResult = nil, msg, msg
		})
	}
	if p := j.Pending; p != nil {
		sameBoot := p.BootID == boot.BootID
		switch {
		case boot.Operation != "idle":
			state = "installing" // RAUC may outlive nab-service, even after Completed
		case sameBoot && p.Phase == phaseInstalled:
			state = "reboot"
		case sameBoot: // RAUC finished but its result was lost
			state = "uncertain"
			if j.Suspended == "" {
				msg := fmt.Sprintf("the result of installing %s is unknown; automatic updates suspended", p.Tag)
				err = u.commit(func(j *journal) { j.Suspended, j.LastResult = msg, msg })
			}
		case boot.Slot == p.ToSlot && u.Current == p.Tag && boot.Health == "good":
			err = u.commit(func(j *journal) {
				j.Pending, j.Suspended = nil, ""
				for tag := range j.Blocked {
					if tag == p.Tag || !Newer(tag, u.Current) {
						delete(j.Blocked, tag)
					}
				}
				j.LastResult = fmt.Sprintf("updated from %s to %s", p.From, p.Tag)
			})
		case boot.Slot == p.ToSlot && u.Current == p.Tag && boot.Health == "stranded":
			err = suspend(fmt.Sprintf("%s could not be confirmed healthy; automatic updates suspended", p.Tag))
		case boot.Slot == p.ToSlot && u.Current == p.Tag:
			state = "confirming"
		case boot.Slot == p.FromSlot && p.Phase == phaseInstalled:
			err = u.commit(func(j *journal) {
				j.Pending = nil
				j.block(p.Tag, "rolled back")
				j.LastResult = fmt.Sprintf("%s did not start correctly, back to %s", p.Tag, u.Current)
			})
		default:
			err = suspend(fmt.Sprintf("installing %s was interrupted by a restart; automatic updates suspended", p.Tag))
		}
	}
	if state == "idle" && u.status.Error != "" {
		state = "error"
	}
	u.status.State = state
	return boot, err
}
