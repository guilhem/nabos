// Package pynab contains the resource selection and RFID formats of PyNab.
package pynab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var Languages = []string{"default", "fr_FR", "de_DE", "en_US", "en_GB", "it_IT", "es_ES", "ja_JP", "pt_BR"}
var SurpriseKinds = []string{"surprise", "carrot", "birthday", "autopromo", "02-14"}

func index(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return -1
}

func Encode(app, language, kind string) (string, error) {
	l := index(Languages, language)
	if l < 0 {
		return "", errors.New("langue inconnue")
	}
	if app == "eightball" {
		return string([]byte{byte(l)}), nil
	}
	k := index(SurpriseKinds, kind)
	if app != "surprise" || k < 0 {
		return "", errors.New("type de surprise inconnu")
	}
	return string([]byte{byte(l), byte(k)}), nil
}

func Decode(app, data string) (language, kind string) {
	l, k := 0, 0
	if len(data) > 0 && (app == "eightball" || len(data) > 1) && int(data[0]) < len(Languages) {
		l = int(data[0])
	}
	if app == "surprise" && len(data) > 1 && int(data[1]) < len(SurpriseKinds) {
		k = int(data[1])
	}
	return Languages[l], SurpriseKinds[k]
}

func prefix(language string) string {
	if language == "" || language == "default" {
		return ""
	}
	return language + "/"
}

func Surprise(now time.Time, language, kind string) map[string]any {
	p := prefix(language) + "surprise/"
	var resource string
	if kind == "" {
		resource = p + now.Format("01-02") + "/*.mp3;" + p + "*.mp3"
	} else if kind == "surprise" {
		resource = p + "*.mp3"
	} else {
		resource = p + kind + "/*.mp3"
	}
	return map[string]any{"signature": map[string]any{"audio": []string{"surprise/respirations/*.mp3"}},
		"body": []any{map[string]any{"audio": []string{resource}}}}
}

func Eightball(language string) map[string]any {
	// The upstream catalogue has no answers in Italian, Japanese or Portuguese.
	return map[string]any{"body": []any{map[string]any{"audio": []string{prefix(language) + "eightball/answers/*.mp3;fr_FR/eightball/answers/*.mp3"}}}}
}

// Delay follows the original frequencies; the caller persists the deadline.
func Delay(service string, frequency int, random float64) time.Duration {
	if frequency == 0 {
		return 0
	}
	if service == "taichi" {
		return time.Duration(float64(256-frequency) * 60 * (random*255 + 64) / 128 * float64(time.Second))
	}
	lo, hi := 7200., 10800.
	switch frequency {
	case 250:
		lo, hi = 0, 1200
	case 125:
		lo, hi = 1200, 3600
	case 50:
		lo, hi = 3600, 7200
	}
	return time.Duration((lo + (hi-lo)*random) * float64(time.Second))
}

var isbnRE = regexp.MustCompile(`^(?:[0-9]{10}|[0-9]{13})$`)
var voiceRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func BookTag(voice, isbn string) (string, error) {
	isbn = strings.ReplaceAll(isbn, "-", "")
	value := voice + "/" + isbn
	if !voiceRE.MatchString(voice) || !isbnRE.MatchString(isbn) || len(value) > 32 {
		return "", errors.New("livre ou voix invalide")
	}
	return value, nil
}

func ParseBook(data string) (voice, isbn string, err error) {
	voice, isbn, ok := strings.Cut(data, "/")
	if !ok {
		return "", "", errors.New("étiquette livre invalide")
	}
	_, err = BookTag(voice, isbn)
	return
}

type Voice struct{ ID, Description string }
type Book struct {
	ISBN, Title string
	Voices      []Voice
}

// Books lists only the catalogue contained in a configured media root.
func Books(roots []string) []Book {
	byISBN := map[string]*Book{}
	for _, root := range roots {
		dir := filepath.Join(root, "book", "books")
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() || !isbnRE.MatchString(entry.Name()) {
				continue
			}
			isbn := entry.Name()
			b := byISBN[isbn]
			if b == nil {
				b = &Book{ISBN: isbn, Title: isbn}
				byISBN[isbn] = b
			}
			if raw, err := os.ReadFile(filepath.Join(dir, isbn, "title.txt")); err == nil {
				b.Title = strings.TrimSpace(string(raw))
			}
			voices, _ := os.ReadDir(filepath.Join(dir, isbn))
			for _, v := range voices {
				if !v.IsDir() || !voiceRE.MatchString(v.Name()) {
					continue
				}
				found := false
				for _, old := range b.Voices {
					if old.ID == v.Name() {
						found = true
					}
				}
				if found {
					continue
				}
				desc := v.Name()
				if raw, err := os.ReadFile(filepath.Join(dir, isbn, v.Name(), "description.txt")); err == nil {
					desc = strings.TrimSpace(string(raw))
				}
				b.Voices = append(b.Voices, Voice{v.Name(), desc})
			}
		}
	}
	out := make([]Book, 0, len(byISBN))
	for _, b := range byISBN {
		sort.Slice(b.Voices, func(i, j int) bool { return b.Voices[i].ID < b.Voices[j].ID })
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ISBN < out[j].ISBN })
	return out
}

func ChapterExists(roots []string, voice, isbn string, chapter int) bool {
	if _, err := BookTag(voice, isbn); err != nil || chapter < 1 {
		return false
	}
	// The core performs the final canonical-path containment check before playback.
	for _, root := range roots {
		p := filepath.Join(root, "book", "books", isbn, voice, fmt.Sprintf("%d.mp3", chapter))
		if f, err := os.Stat(p); err == nil && f.Mode().IsRegular() {
			return true
		}
	}
	return false
}
