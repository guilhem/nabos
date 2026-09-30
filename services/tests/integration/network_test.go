package integration

import (
	"bufio"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/network"
)

// Start the real daemon on a private bus; simulation changes no host services.
func (h *harness) startDeviceCore(t *testing.T) {
	daemon := exec.Command(which(t, "dbus-daemon"), "--session", "--nofork", "--print-address=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	h.procs["dbus"] = daemon
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", strings.TrimSpace(address))
	h.startSystemdIdentity(t)
	h.startDeviceDaemon()
	api, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	h.deviceAPI = api
	t.Cleanup(api.Close)
	h.waitFor(15*time.Second, "device-core Config ready", func() bool {
		_, _, err := api.ReadConfig(context.Background())
		return err == nil
	})
	if err := api.SetTime(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	net := &network.Client{Client: api}
	h.waitFor(15*time.Second, "device-core network initialized", func() bool {
		status, err := net.ReadStatus(context.Background())
		return err == nil && status.Mode == "hotspot"
	})
	if _, err := net.Connect(context.Background(), network.SSID("Test"), "open", "", "", ""); err != nil {
		t.Fatal(err)
	}
	h.waitFor(15*time.Second, "simulated client network", func() bool {
		status, err := net.ReadStatus(context.Background())
		return err == nil && status.ClientReady() && status.Phase != "connecting"
	})
}
func (h *harness) startDeviceDaemon() {
	units := ""
	if h.requireAgents {
		units = "nab-core.service:nab-service.service"
	}
	assets := os.Getenv("NABOS_TEST_ASSETS")
	if assets == "" {
		assets = filepath.Join(repo, "assets")
	}
	h.spawn("device", append(h.device, "--simulate"),
		"DEVICE_CORE_BUS_ADDRESS="+os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"),
		"DEVICE_CORE_DATA_DIR="+filepath.Join(h.tmp, "device-data"),
		"DEVICE_CORE_NETWORK_GUARD="+filepath.Join(h.tmp, "network-guard"),
		"DEVICE_CORE_HTTP_ADDR=", "DEVICE_CORE_LVA_UNIT=", "DEVICE_CORE_UPDATE_REPO=", "DEVICE_CORE_UPDATE_ASSET=",
		"DEVICE_CORE_MAINTENANCE_UNITS="+units,
		"DEVICE_CORE_AUDIO_ROOTS="+filepath.Join(assets, "sounds"), "DEVICE_CORE_SIM_AUDIO_MS=200",
		"DEVICE_CORE_IMAGE_VERSION=v0.0.1", "DEVICE_CORE_DEFAULT_LOCALE=fr_FR", "DEVICE_CORE_DEFAULT_VOLUME=100")
}

// Checks the Go ABI against the real daemon, including transferred FD lifetime
// and stale revisions after a daemon replacement on the same client connection.
func TestDeviceCore(t *testing.T) {
	if os.Getenv("NABOS_INTEGRATION") != "1" {
		t.Skip("set NABOS_INTEGRATION=1")
	}
	binary := os.Getenv("DEVICE_CORE_BIN")
	if binary == "" {
		binary = filepath.Join(repo, "build/device-core/target/debug/device-core")
	}
	h := &harness{t: t, tmp: t.TempDir(), device: commandLine(binary), procs: map[string]*exec.Cmd{}}
	t.Cleanup(func() { h.stop("device", syscall.SIGTERM); h.stop("dbus", syscall.SIGTERM) })
	h.startDeviceCore(t)
	ctx := context.Background()
	var capabilities []string
	if err := h.deviceAPI.Property(ctx, "Manager", "Capabilities", &capabilities); err != nil || slices.Contains(capabilities, "maintenance-agents") {
		t.Fatal("standalone daemon unexpectedly requires agents", capabilities, err)
	}
	revision, settings, err := h.deviceAPI.ReadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Volume = 42
	next, err := h.deviceAPI.UpdateConfig(ctx, revision, settings)
	if err != nil || next == revision {
		t.Fatal(next, err)
	}
	if quality, _, err := h.deviceAPI.Clock(ctx); err != nil || quality != "manual" {
		t.Fatal(quality, err)
	}
	if configured, err := h.deviceAPI.UpdatesConfigured(ctx); err != nil || configured {
		t.Fatal(configured, err)
	}
	if state, err := h.deviceAPI.UpdateStatus(ctx); err != nil || state.State != "unsupported" {
		t.Fatal(state, err)
	}
	if supported, err := h.deviceAPI.VoiceSupported(ctx); err != nil || supported {
		t.Fatal(supported, err)
	}
	assets := os.Getenv("NABOS_TEST_ASSETS")
	if assets == "" {
		assets = filepath.Join(repo, "assets")
	}
	audio, err := h.deviceAPI.StartAudio(ctx, "file", filepath.Join(assets, "sounds/system/abort.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := h.deviceAPI.WaitAudio(ctx, audio); err != nil || outcome != "completed" {
		t.Fatal(outcome, err)
	}
	net := &network.Client{Client: h.deviceAPI}
	snapshot, err := net.Read(ctx)
	if err != nil || !snapshot.Status.ClientReady() {
		t.Fatal(snapshot, err)
	}
	guard, err := net.Guard(ctx, snapshot.Status.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	exclusive, err := os.OpenFile(filepath.Join(h.tmp, "network-guard"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	if err := unix.Flock(int(exclusive.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("guard did not carry shared flock", err)
	}
	h.stop("device", syscall.SIGTERM)
	if err := unix.Flock(int(exclusive.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("daemon disappearance released client's guard", err)
	}
	guard.Close()
	if err := unix.Flock(int(exclusive.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal("client close retained guard", err)
	}
	unix.Flock(int(exclusive.Fd()), unix.LOCK_UN)
	h.startDeviceDaemon()
	h.waitFor(15*time.Second, "replacement daemon", func() bool { current, _, err := h.deviceAPI.ReadConfig(ctx); return err == nil && current != next })
	if _, err := h.deviceAPI.UpdateConfig(ctx, next, settings); !errors.Is(err, device.ErrRefused) {
		t.Fatal("stale daemon revision accepted", err)
	}
	if _, err := net.Guard(ctx, snapshot.Status.Generation); !errors.Is(err, network.ErrRefused) {
		t.Fatal("stale daemon generation accepted", err)
	}
	if _, current, err := h.deviceAPI.ReadConfig(ctx); err != nil || current.Volume != 42 {
		t.Fatal("system settings lost after restart", current, err)
	}
}
