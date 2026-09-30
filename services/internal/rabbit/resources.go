package rabbit

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type resources struct {
	sounds, chors []string
	mu            sync.RWMutex
	locale        string
}

func (r *resources) setLocale(s string) {
	if len(s) != 5 || s[2] != '_' || s[0] < 'a' || s[0] > 'z' || s[1] < 'a' || s[1] > 'z' || s[3] < 'A' || s[3] > 'Z' || s[4] < 'A' || s[4] > 'Z' {
		return
	}
	r.mu.Lock()
	r.locale = s
	r.mu.Unlock()
}
func contained(root, path string) string {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ""
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() {
		return ""
	}
	return path
}
func (r *resources) find(chor bool, spec string) string {
	if !validResource(spec) {
		return ""
	}
	r.mu.RLock()
	locale := r.locale
	r.mu.RUnlock()
	roots := r.sounds
	if chor {
		roots = r.chors
	}
	allowed := func(p string) bool {
		ext := strings.ToLower(filepath.Ext(p))
		if chor {
			return ext == ".chor"
		}
		return ext == ".mp3" || ext == ".wav"
	}
	for _, part := range strings.Split(spec, ";") {
		name := filepath.Base(part)
		var files []string
		seen := map[string]bool{}
		for _, root := range roots {
			for _, prefix := range []string{filepath.Join(root, locale), root} {
				path := filepath.Join(prefix, part)
				if strings.HasPrefix(name, "*") {
					entries, err := os.ReadDir(filepath.Dir(path))
					if err != nil {
						continue
					}
					for _, entry := range entries {
						n := entry.Name()
						if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, name[1:]) {
							continue
						}
						p := filepath.Join(filepath.Dir(path), n)
						if !allowed(p) {
							continue
						}
						if p = contained(root, p); p != "" && !seen[p] {
							seen[p] = true
							files = append(files, p)
						}
					}
				} else if allowed(path) {
					if p := contained(root, path); p != "" {
						return p
					}
				}
			}
		}
		if len(files) > 0 {
			return files[rand.IntN(len(files))]
		}
	}
	return ""
}
