package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceSettingsAreIsolatedAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.json")
	st, _ := Open(path)
	uid := "d0:02:18:00:00:00:00:01"
	_, err := st.Update(func(s *Settings) error { s.Tags[uid] = TagAction{"webhook", "http://192.168.1.2/run"}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	copy := st.Get()
	delete(copy.Tags, uid)
	if len(st.Get().Tags) != 1 {
		t.Fatal("Get leaked mutable map")
	}
	failed, err := st.Update(func(s *Settings) error { delete(s.Tags, uid); return errors.New("abort") })
	delete(failed.Tags, uid)
	if err == nil || len(st.Get().Tags) != 1 {
		t.Fatal("failed update changed state")
	}
	if _, err = st.Update(func(s *Settings) error { s.Tags[uid] = TagAction{"radio", "file:///etc/passwd"}; return nil }); err == nil {
		t.Fatal("unsafe URL accepted")
	}
	re, err := Open(path)
	if err != nil || len(re.Get().Tags) != 1 || !re.Get().Services.Books {
		t.Fatal("restart", err)
	}
}

func TestCorruptFileIsMovedAsideAndDefaultsUsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "application.json")
	for name, content := range map[string]string{
		"truncated": `{"version":1,"ears":`,
		"invalid":   `{"version":1,"ears":[900,0]}`,
		"garbage":   "\x00\x00\xff",
	} {
		os.WriteFile(path, []byte(content), 0o600)
		st, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Recovered == "" || st.Get().Ears != Defaults().Ears {
			t.Fatalf("%s: not recovered: %+v", name, st.Get())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: corrupt file left in place", name)
		}
		os.Remove(st.Recovered)
	}
}

func TestUpdateIsAtomicAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "application.json")
	st, _ := Open(path)
	if _, err := st.Update(func(s *Settings) error { s.Ears = [2]int{4, 2}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(func(s *Settings) error { s.Ears = [2]int{90, 0}; return nil }); err == nil {
		t.Fatal("invalid ears accepted")
	}
	if st.Get().Ears != ([2]int{4, 2}) {
		t.Fatal("rejected update leaked into memory")
	}
	re, err := Open(path)
	if err != nil || re.Get().Ears != ([2]int{4, 2}) || re.Recovered != "" {
		t.Fatalf("reload: %v %+v", err, re.Get())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatal("temporary file left behind")
		}
	}
}

func TestApplicationSchemaRejectsSystemAndOldConfiguration(t *testing.T) {
	for _, raw := range []string{`{"version":2}`, `{"version":1,"locale":"fr_FR"}`, `{"version":1,"auto_check_updates":false}`, `{"version":1} {"volume":33}`} {
		path := filepath.Join(t.TempDir(), "application.json")
		os.WriteFile(path, []byte(raw), 0600)
		st, err := Open(path)
		if err != nil || st.Recovered == "" {
			t.Fatalf("system or unsupported schema accepted: %s %v", raw, err)
		}
	}
}

func TestUncertainWritePreservesVisibleAdministrator(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("renamed=%v", renamed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "application.json")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Update(func(*Settings) error { return nil }); err != nil {
				t.Fatal(err)
			}
			next := st.Get()
			next.Admin.Hash = "persisted-admin-proof"
			uncertain := errors.New("directory fsync failed after rename")
			st.mu.Lock()
			_, err = st.commit(next, func(path string, settings Settings) error {
				if renamed {
					if err := writeAtomic(path, settings); err != nil {
						return err
					}
				}
				return uncertain
			})
			st.mu.Unlock()
			if !errors.Is(err, uncertain) {
				t.Fatal("lost uncertain write error", err)
			}
			if _, err := st.Update(func(s *Settings) error { s.Ears = [2]int{1, 2}; return nil }); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			want := Admin{}
			if renamed {
				want = next.Admin
			}
			if got := reopened.Get().Admin; got != want {
				t.Fatalf("later save overwrote administrator: %#v", got)
			}
		})
	}
}
