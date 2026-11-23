package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	testModel  = "model contents"
	testConfig = `{"audio":{"sample_rate":22050}}`
)

type testRegistry struct {
	store       *Store
	content     map[string][]byte
	calls       atomic.Int32
	blobFetches atomic.Int32
	intercept   func(*http.Request, *http.Response) error
}

func newRegistry(t *testing.T, edit func(*ocispec.Manifest)) *testRegistry {
	t.Helper()
	r := &testRegistry{content: make(map[string][]byte)}
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType, Config: ocispec.DescriptorEmptyJSON,
	}
	for _, file := range []struct{ name, mediaType, data string }{
		{modelName, modelType, testModel}, {configName, configType, testConfig},
	} {
		d := content.NewDescriptorFromBytes(file.mediaType, []byte(file.data))
		d.Annotations = map[string]string{ocispec.AnnotationTitle: file.name}
		m.Layers = append(m.Layers, d)
		r.content["blobs/"+d.Digest.String()] = []byte(file.data)
	}
	if edit != nil {
		edit(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	pin := digest.FromBytes(raw)
	r.content["manifests/"+pin.String()] = raw
	repo, err := remote.NewRepository("registry.example/voice")
	if err != nil {
		t.Fatal(err)
	}
	repo.Client = r
	r.store = &Store{DataDir: t.TempDir(), Repository: repo, Digest: pin}
	return r
}

// Do serves the OCI pull endpoints in memory, including HEAD and streaming GET.
func (r *testRegistry) Do(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	key := strings.TrimPrefix(req.URL.Path, "/v2/voice/")
	data, ok := r.content[key]
	resp := &http.Response{
		Request: req, StatusCode: http.StatusOK, Header: make(http.Header),
		ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)),
	}
	if !ok {
		resp.StatusCode = http.StatusNotFound
		return resp, nil
	}
	resp.Header.Set("Docker-Content-Digest", path.Base(key))
	if strings.HasPrefix(key, "manifests/") {
		resp.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
	} else {
		r.blobFetches.Add(1)
		resp.Header.Set("Content-Type", "application/octet-stream")
	}
	if req.Method == http.MethodHead {
		resp.Body = http.NoBody
	}
	if r.intercept != nil {
		if err := r.intercept(req, resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func checkFile(t *testing.T, filename, want string) {
	t.Helper()
	got, err := os.ReadFile(filename)
	if err != nil || string(got) != want {
		t.Fatalf("%s: got %q, %v", filename, got, err)
	}
}

func TestConcurrentInstallPublishesBothFilesOnce(t *testing.T) {
	r := newRegistry(t, func(m *ocispec.Manifest) {
		m.Layers[0], m.Layers[1] = m.Layers[1], m.Layers[0]
	})
	s := r.store
	staging := filepath.Join(s.DataDir, pending)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "old-partial"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.intercept = func(req *http.Request, _ *http.Response) error {
		if path.Base(req.URL.Path) == digest.FromString(testModel).String() {
			close(entered)
			select {
			case <-release:
			case <-req.Context().Done():
				return req.Context().Err()
			}
		}
		return nil
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.Install(ctx) }()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("installation ended before the second layer: %v", err)
	}
	if s.Installed() {
		t.Fatal("voice became visible during transfer")
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, liveDir)); !os.IsNotExist(err) {
		t.Fatal("published directory exists during transfer", err)
	}
	checkFile(t, filepath.Join(staging, configName), testConfig)
	waitCtx, stopWaiting := context.WithCancel(ctx)
	waiting := make(chan error, 1)
	go func() { waiting <- s.Install(waitCtx) }()
	stopWaiting()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled concurrent request", err)
	}
	go func() { second <- s.Install(ctx) }()
	close(release)
	for _, result := range []chan error{first, second} {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if !s.Installed() || r.blobFetches.Load() != 2 {
		t.Fatal("installed state or number of downloads", s.Installed(), r.blobFetches.Load())
	}
	dest := filepath.Join(s.DataDir, liveDir)
	checkFile(t, filepath.Join(dest, modelName), testModel)
	checkFile(t, filepath.Join(dest, configName), testConfig)
	checkFile(t, filepath.Join(dest, manifestName), string(r.content["manifests/"+s.Digest.String()]))
	files, err := os.ReadDir(dest)
	if err != nil || len(files) != 3 {
		t.Fatal("stale file survived", files, err)
	}
	r.intercept = func(*http.Request, *http.Response) error { return errors.New("registry offline") }
	if err := s.Install(t.Context()); err != nil || r.calls.Load() != 4 {
		t.Fatal("repeat installation accessed the registry", err, r.calls.Load())
	}
}

func TestInvalidVoiceManifestRejectsBeforeLayerDownloads(t *testing.T) {
	for name, edit := range map[string]func(*ocispec.Manifest){
		"wrong manifest type": func(m *ocispec.Manifest) { m.MediaType = ocispec.MediaTypeImageIndex },
		"wrong artifact":      func(m *ocispec.Manifest) { m.ArtifactType = modelType },
		"nonempty config":     func(m *ocispec.Manifest) { m.Config = m.Layers[1] },
		"extra layer":         func(m *ocispec.Manifest) { m.Layers = append(m.Layers, m.Layers[0]) },
		"missing layer":       func(m *ocispec.Manifest) { m.Layers = m.Layers[:1] },
		"duplicate title":     func(m *ocispec.Manifest) { m.Layers[1] = m.Layers[0] },
		"invalid title":       func(m *ocispec.Manifest) { m.Layers[1].Annotations[ocispec.AnnotationTitle] = "../file" },
		"wrong layer type":    func(m *ocispec.Manifest) { m.Layers[0].MediaType = configType },
		"oversize model":      func(m *ocispec.Manifest) { m.Layers[0].Size = maxModel + 1 },
		"oversize config":     func(m *ocispec.Manifest) { m.Layers[1].Size = maxConfig + 1 },
		"empty model":         func(m *ocispec.Manifest) { m.Layers[0].Size = 0 },
		"malformed digest":    func(m *ocispec.Manifest) { m.Layers[1].Digest = "tag" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRegistry(t, edit)
			if err := r.store.Install(t.Context()); err == nil || r.blobFetches.Load() != 0 || r.store.Installed() {
				t.Fatal("invalid manifest accepted or blobs downloaded", err, r.blobFetches.Load())
			}
		})
	}
}

func TestFailedTransferPreservesFilesAndRetryRepairsIncompleteVersion(t *testing.T) {
	for _, scenario := range []string{"manifest too large", "bad manifest digest", "bad layer digest", "truncated", "trailing bytes", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRegistry(t, nil)
			s := r.store
			dest := filepath.Join(s.DataDir, liveDir)
			if err := os.MkdirAll(dest, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, modelName), []byte("untouched"), 0o640); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.intercept = func(req *http.Request, resp *http.Response) error {
				switch {
				case strings.Contains(req.URL.Path, "/manifests/"):
					if scenario == "manifest too large" {
						resp.ContentLength = maxManifest + 1
					}
					if scenario == "bad manifest digest" && req.Method == http.MethodGet {
						resp.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", int(resp.ContentLength))))
					}
				case path.Base(req.URL.Path) == digest.FromString(testConfig).String():
					resp.ContentLength = -1 // A registry may stream a blob without Content-Length.
					switch scenario {
					case "bad layer digest":
						resp.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", len(testConfig))))
					case "truncated":
						resp.Body = io.NopCloser(strings.NewReader(testConfig[:3]))
					case "trailing bytes":
						resp.Body = io.NopCloser(strings.NewReader(testConfig + "extra"))
					case "canceled":
						cancel()
					}
				}
				return nil
			}
			err := s.Install(ctx)
			if err == nil || (scenario == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatal("transfer unexpectedly succeeded or lost cancellation", err)
			}
			checkFile(t, filepath.Join(dest, modelName), "untouched")
			if s.Installed() {
				t.Fatal("partial voice was published")
			}
			if _, err := os.Stat(filepath.Join(s.DataDir, pending)); !os.IsNotExist(err) {
				t.Fatal("staging survived error", err)
			}
			r.intercept = nil
			if err := s.Install(t.Context()); err != nil || !s.Installed() {
				t.Fatal("retry did not repair the installation", err)
			}
			checkFile(t, filepath.Join(dest, modelName), testModel)
		})
	}
}

func TestVolatileStorageAndInvalidPinDoNotFetch(t *testing.T) {
	r := newRegistry(t, nil)
	data := r.store.DataDir
	r.store.DataDir = filepath.Join(data, "nested", "nabos")
	if err := os.WriteFile(filepath.Join(data, ".volatile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.store.Install(t.Context()); !errors.Is(err, ErrVolatile) || r.calls.Load() != 0 {
		t.Fatal(err, r.calls.Load())
	}
	if _, err := os.Stat(filepath.Join(data, "nested")); !os.IsNotExist(err) {
		t.Fatal("wrote to volatile storage", err)
	}
	r.store.DataDir = t.TempDir()
	r.store.Digest = "latest"
	if err := r.store.Install(t.Context()); err == nil || r.calls.Load() != 0 {
		t.Fatal("mutable reference accepted", err, r.calls.Load())
	}
}

func TestInstalledRequiresRegularFiles(t *testing.T) {
	r := newRegistry(t, nil)
	if err := r.store.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(r.store.DataDir, liveDir)
	if err := os.Remove(filepath.Join(dest, configName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(modelName, filepath.Join(dest, configName)); err != nil {
		t.Fatal(err)
	}
	if r.store.Installed() {
		t.Fatal("accepted a symlink as a voice file")
	}
}

func TestInstallRepairsTruncatedModelAndDamagedManifest(t *testing.T) {
	r := newRegistry(t, nil)
	s := r.store
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(s.DataDir, liveDir)
	for _, name := range []string{modelName, manifestName} {
		if err := os.Truncate(filepath.Join(dest, name), 4); err != nil {
			t.Fatal(err)
		}
		checkFile(t, filepath.Join(dest, configName), testConfig)
		if s.Installed() {
			t.Fatal("truncation went undetected", name)
		}
		if err := s.Install(t.Context()); err != nil || !s.Installed() {
			t.Fatal("repair failed", name, err)
		}
		checkFile(t, filepath.Join(dest, modelName), testModel)
	}
}

func TestFailedDownloadPreservesPreviouslyInstalledPin(t *testing.T) {
	old := newRegistry(t, nil)
	if err := old.store.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := newRegistry(t, func(m *ocispec.Manifest) {
		m.Annotations = map[string]string{ocispec.AnnotationVersion: "replacement"}
	})
	next.store.DataDir = old.store.DataDir
	if next.store.Installed() {
		t.Fatal("different installed pin was accepted")
	}
	key := "blobs/" + digest.FromString(testConfig).String()
	next.content[key] = bytes.Repeat([]byte{'x'}, len(testConfig))
	if err := next.store.Install(t.Context()); err == nil || !old.store.Installed() {
		t.Fatal("failed download displaced the installed pin", err)
	}
	dest := filepath.Join(old.store.DataDir, liveDir)
	checkFile(t, filepath.Join(dest, modelName), testModel)
	checkFile(t, filepath.Join(dest, configName), testConfig)
	checkFile(t, filepath.Join(dest, manifestName), string(old.content["manifests/"+old.store.Digest.String()]))
}

func TestFailedDirectorySyncRollsBackPublication(t *testing.T) {
	for _, previous := range []bool{false, true} {
		dir := t.TempDir()
		stage := filepath.Join(dir, pending)
		active := filepath.Join(dir, liveDir)
		if err := os.MkdirAll(stage, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, modelName), []byte("new"), 0o640); err != nil {
			t.Fatal(err)
		}
		if previous {
			if err := os.Mkdir(active, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(active, modelName), []byte("previous"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("injected directory sync error")
		err = publish(root, func(*os.File) error { return failure })
		root.Close()
		if !errors.Is(err, failure) {
			t.Fatal("sync error was lost", err)
		}
		if _, err := os.Stat(stage); !os.IsNotExist(err) {
			t.Fatal("failed version survived in staging", err)
		}
		if previous {
			checkFile(t, filepath.Join(active, modelName), "previous")
		} else if _, err := os.Stat(active); !os.IsNotExist(err) {
			t.Fatal("undurable version remained installed", err)
		}
	}
}
