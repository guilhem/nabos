package chatgpt

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/guilhem/nabos/services/internal/config"
	"golang.org/x/sys/unix"
)

var errStorage = errors.New("private ChatGPT account storage is unavailable")

const maxAccountSize = 1 << 20

// The local protocol stores one private record per registration. NabOS links
// one account. Only Status can cross the settings boundary.
type localAccount struct {
	HostID       string       `json:"host_id"`
	ClientID     string       `json:"client_id,omitempty"`
	Issuer       string       `json:"issuer,omitempty"`
	Subject      string       `json:"subject,omitempty"`
	Email        string       `json:"email,omitempty"`
	NeedsConsent bool         `json:"needs_consent,omitempty"`
	Tokens       *credentials `json:"tokens,omitempty"`
}

type credentials struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	ID      string    `json:"id_token"`
	Type    string    `json:"token_type"`
	Expiry  time.Time `json:"expires_at"`
	Scopes  []string  `json:"scopes"`
}

func (c *Client) directory() string {
	return filepath.Join(c.dataDir, "chatgpt")
}

func (c *Client) read() (localAccount, error) {
	var s localAccount
	if !filepath.IsAbs(c.dataDir) {
		return s, errStorage
	}
	info, err := os.Lstat(c.directory())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return s, errStorage
	}
	f, err := os.OpenFile(filepath.Join(c.directory(), "account.json"), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, errStorage
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxAccountSize {
		return s, errStorage
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxAccountSize+1))
	if err != nil || len(raw) > maxAccountSize || json.Unmarshal(raw, &s) != nil || s.HostID == "" ||
		(s.Subject != "" && (s.ClientID == "" || s.Issuer != issuer)) || (s.Tokens != nil && s.Subject == "") {
		return localAccount{}, errStorage
	}
	return s, nil
}

func (c *Client) prepare() (localAccount, error) {
	s, err := c.read()
	if err != nil {
		return s, err
	}
	// The initrd places this marker at /data when it falls back to tmpfs.
	for _, dir := range []string{c.dataDir, filepath.Dir(c.dataDir)} {
		if _, err := os.Lstat(filepath.Join(dir, ".volatile")); !errors.Is(err, fs.ErrNotExist) {
			return s, errStorage
		}
	}
	if s.HostID != "" {
		return s, nil
	}
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	s.HostID = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	return s, c.write(s)
}

func (c *Client) write(s localAccount) error {
	if err := os.Mkdir(c.directory(), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return errStorage
	}
	info, err := os.Lstat(c.directory())
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errStorage
	}
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > maxAccountSize || config.WriteFile(filepath.Join(c.directory(), "account.json"), raw) != nil {
		return errStorage
	}
	return nil
}
