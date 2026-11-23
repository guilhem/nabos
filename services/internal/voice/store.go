// Package voice installs the French Nabaztag voice on the persistent data partition.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	voiceDir = "voices/fr_FR-nabaztag"
	liveDir  = voiceDir + "/v1"
	pending  = voiceDir + "/.pending"

	modelName    = "fr_FR-nabaztag-medium.onnx"
	configName   = modelName + ".json"
	manifestName = ".manifest.json"

	artifactType = "application/vnd.nabos.voice.v1"
	modelType    = "application/vnd.nabos.voice.model.v1+onnx"
	configType   = "application/vnd.nabos.voice.config.v1+json"

	maxManifest = 64 << 10
	maxModel    = 64 << 20
	maxConfig   = 64 << 10
)

// ErrVolatile indicates that the initrd mounted temporary storage for this boot.
var ErrVolatile = errors.New("voice installation requires persistent storage")

// Store installs a single immutable voice version. Share one Store across requests.
type Store struct {
	DataDir    string
	Repository *remote.Repository
	Digest     digest.Digest

	once sync.Once
	lock chan struct{}
}

// Installed checks the pinned manifest and both regular files' sizes without reading the model.
func (s *Store) Installed() bool {
	if s.Digest.Validate() != nil {
		return false
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return false
	}
	defer root.Close()
	ok, _ := s.installed(root)
	return ok
}

func (s *Store) installed(root *os.Root) (bool, error) {
	for _, dir := range []string{"voices", voiceDir, liveDir} {
		info, err := root.Lstat(dir)
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("expected voice directory at %s", dir)
		}
	}
	version, err := root.OpenRoot(liveDir)
	if err != nil {
		return false, err
	}
	defer version.Close()
	info, err := version.Lstat(manifestName)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxManifest {
		return false, nil
	}
	file, err := version.Open(manifestName)
	if err != nil {
		return false, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxManifest+1))
	if err != nil {
		return false, err
	}
	if int64(len(raw)) != info.Size() || s.Digest.Algorithm().FromBytes(raw) != s.Digest {
		return false, nil
	}
	layers, err := parseLayers(raw)
	if err != nil {
		return false, nil
	}
	for _, layer := range layers {
		info, err := version.Lstat(layer.Annotations[ocispec.AnnotationTitle])
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() || info.Size() != layer.Size {
			return false, nil
		}
	}
	return true, nil
}

// Install fetches and verifies both layers before making the version visible.
// A repeated installation of the same published v1 succeeds offline.
func (s *Store) Install(ctx context.Context) error {
	s.once.Do(func() { s.lock = make(chan struct{}, 1) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.lock <- struct{}{}:
		defer func() { <-s.lock }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(s.DataDir) {
		return errors.New("voice data directory must be absolute")
	}
	// The initrd marks /data, the parent mount of the application's data directory.
	for dir := filepath.Clean(s.DataDir); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".volatile")); err == nil {
			return ErrVolatile
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	if err := s.Digest.Validate(); err != nil {
		return fmt.Errorf("invalid voice manifest digest: %w", err)
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	ok, err := s.installed(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if ok {
		if err := syncDirs(root, voiceDir, "voices", "."); err != nil {
			return err
		}
		return root.RemoveAll(pending)
	}
	if s.Repository == nil {
		return errors.New("missing voice repository")
	}
	if err := root.MkdirAll(voiceDir, 0o750); err != nil {
		return err
	}
	// One fixed staging directory also reclaims an interrupted download after reboot.
	if err := root.RemoveAll(pending); err != nil {
		return err
	}
	if err := root.Mkdir(pending, 0o700); err != nil {
		return err
	}
	if err := s.stage(ctx, root); err != nil {
		return errors.Join(err, root.RemoveAll(pending))
	}
	return publish(root, (*os.File).Sync)
}

func (s *Store) stage(ctx context.Context, root *os.Root) error {
	raw, err := s.fetchManifest(ctx)
	if err != nil {
		return err
	}
	layers, err := parseLayers(raw)
	if err != nil {
		return err
	}
	manifest := ocispec.Descriptor{Digest: s.Digest, Size: int64(len(raw))}
	if err := writeVerified(root, manifestName, manifest, bytes.NewReader(raw)); err != nil {
		return err
	}
	for _, layer := range layers {
		if err := s.writeLayer(ctx, root, layer); err != nil {
			return err
		}
	}
	if err := root.Chmod(pending, 0o750); err != nil {
		return err
	}
	// Persist the files and new parent directories before committing the version name.
	if err := syncDirs(root, pending, "voices", "."); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *Store) fetchManifest(ctx context.Context) ([]byte, error) {
	desc, err := s.Repository.Resolve(ctx, s.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("resolve voice manifest: %w", err)
	}
	if desc.Digest != s.Digest || desc.MediaType != ocispec.MediaTypeImageManifest || desc.Size <= 0 || desc.Size > maxManifest {
		return nil, errors.New("invalid voice manifest descriptor")
	}
	raw, err := content.FetchAll(ctx, s.Repository.Manifests(), desc)
	if err != nil {
		return nil, fmt.Errorf("fetch voice manifest: %w", err)
	}
	return raw, nil
}

func parseLayers(raw []byte) ([]ocispec.Descriptor, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode voice manifest: %w", err)
	}
	empty := ocispec.DescriptorEmptyJSON
	if m.SchemaVersion != 2 || m.MediaType != ocispec.MediaTypeImageManifest || m.ArtifactType != artifactType ||
		!content.Equal(m.Config, empty) || (m.Config.Data != nil && !bytes.Equal(m.Config.Data, empty.Data)) || len(m.Layers) != 2 {
		return nil, errors.New("unexpected voice manifest")
	}
	seen := make(map[string]bool, 2)
	for _, layer := range m.Layers {
		name := layer.Annotations[ocispec.AnnotationTitle]
		validModel := name == modelName && layer.MediaType == modelType && layer.Size <= maxModel
		validConfig := name == configName && layer.MediaType == configType && layer.Size <= maxConfig
		if (!validModel && !validConfig) || seen[name] || layer.Size <= 0 || layer.Digest.Validate() != nil {
			return nil, errors.New("unexpected voice layer descriptor")
		}
		seen[name] = true
	}
	return m.Layers, nil
}

func (s *Store) writeLayer(ctx context.Context, root *os.Root, layer ocispec.Descriptor) error {
	name := layer.Annotations[ocispec.AnnotationTitle]
	body, err := s.Repository.Blobs().Fetch(ctx, layer)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", name, err)
	}
	defer body.Close()
	return writeVerified(root, name, layer, body)
}

func writeVerified(root *os.Root, name string, desc ocispec.Descriptor, body io.Reader) error {
	file, err := root.OpenFile(pending+"/"+name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	reader := content.NewVerifyReader(body, desc)
	if _, err = io.Copy(file, reader); err == nil {
		err = reader.Verify()
	}
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	return nil
}

func syncDirs(root *os.Root, names ...string) error {
	for _, name := range names {
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}

// Replace an existing version in one operation, keeping its directory until the new name is durable.
func publish(root *os.Root, syncParent func(*os.File) error) (result error) {
	cleanPending := true
	defer func() {
		if cleanPending {
			result = errors.Join(result, root.RemoveAll(pending))
		}
	}()
	dir, err := root.Open(voiceDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	exchange := func() error { return unix.Renameat2(fd, ".pending", fd, "v1", unix.RENAME_EXCHANGE) }
	err = exchange()
	replaced := err == nil
	if errors.Is(err, unix.ENOENT) {
		err = root.Rename(pending, liveDir)
	}
	if err != nil {
		return err
	}
	if err = syncParent(dir); err == nil {
		return nil
	}
	if !replaced {
		return errors.Join(err, root.RemoveAll(liveDir), dir.Sync())
	}
	rollbackErr := exchange()
	// If rollback itself fails, preserve the previous files in .pending.
	cleanPending = rollbackErr == nil
	return errors.Join(err, rollbackErr, dir.Sync())
}
