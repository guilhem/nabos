package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
