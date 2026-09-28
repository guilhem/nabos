package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceSettingsAreIsolatedAndSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
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
	path := filepath.Join(dir, "config.json")
	for name, content := range map[string]string{
		"truncated": `{"version":1,"locale":"fr_`,
		"invalid":   `{"version":1,"volume":900}`,
		"garbage":   "\x00\x00\xff",
	} {
		os.WriteFile(path, []byte(content), 0o600)
		st, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Recovered == "" || st.Get().Volume != Defaults().Volume {
			t.Fatalf("%s: not recovered: %+v", name, st.Get())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: corrupt file left in place", name)
		}
		os.Remove(st.Recovered)
	}
}

func TestUpdateIsAtomicAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	st, _ := Open(path)
	if _, err := st.Update(func(s *Settings) error { s.Volume = 42; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(func(s *Settings) error { s.Locale = "../../etc"; return nil }); err == nil {
		t.Fatal("invalid locale accepted")
	}
	if st.Get().Locale != "fr_FR" {
		t.Fatal("rejected update leaked into memory")
	}
	re, err := Open(path)
	if err != nil || re.Get().Volume != 42 || re.Recovered != "" {
		t.Fatalf("reload: %v %+v", err, re.Get())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatal("temporary file left behind")
		}
	}
}

func TestNewerVersionAndUnknownFieldsStayReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":2,"volume":33,"future_feature":{"x":1}}`), 0o600)
	st, err := Open(path)
	if err != nil || st.Recovered != "" || st.Get().Volume != 33 {
		t.Fatalf("rollback compatibility: %v %+v", err, st.Get())
	}
}

func TestUpdatePreferencesMigrateWithoutEnablingInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"auto_check_updates":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil || st.Get().AutoCheck || st.Get().Updates.Automatic || st.Get().Updates.Channel != "stable" || st.Get().Updates.Start != (HM{3, 0}) {
		t.Fatalf("migration: %v %+v", err, st.Get())
	}
	for _, change := range []func(*Settings){
		func(s *Settings) { s.Updates.Channel = "unknown" },
		func(s *Settings) { s.Updates.Start.Hour = 24 },
		func(s *Settings) { s.Updates.End = s.Updates.Start },
		func(s *Settings) { s.Updates.Automatic = true },
	} {
		if _, err := st.Update(func(s *Settings) error { change(s); return nil }); err == nil {
			t.Fatal("invalid update policy accepted")
		}
	}
	_, err = st.Update(func(s *Settings) error {
		s.AutoCheck, s.Updates.Automatic = true, true
		s.Updates.Channel, s.Updates.Start, s.Updates.End = "test", HM{23, 0}, HM{2, 0}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil || reopened.Get().Updates != st.Get().Updates {
		t.Fatal("update preferences lost on restart", err)
	}
}
