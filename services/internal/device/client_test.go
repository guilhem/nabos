package device_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
)

func TestOpenSelectsExplicitBusWithoutSystemFallback(t *testing.T) {
	system := devicetest.New(t)
	systemAddress := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
	private := devicetest.New(t)
	privateAddress := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
	system.Mu.Lock()
	system.Settings.Volume = 7
	system.Mu.Unlock()
	private.Mu.Lock()
	private.Settings.Volume = 42
	private.Mu.Unlock()
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", systemAddress)
	for _, tc := range []struct {
		name, address string
		fixture       *devicetest.Fixture
	}{
		{"system default", "", system},
		{"explicit private", privateAddress, private},
		{"explicit unavailable", "unix:path=" + filepath.Join(t.TempDir(), "missing-bus"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NABOS_DEVICE_BUS_ADDRESS", tc.address)
			client, err := device.Open()
			if tc.fixture == nil {
				if client != nil {
					client.Close()
					t.Fatal("unavailable explicit bus reached the system bus")
				}
				if !errors.Is(err, device.ErrUnavailable) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, settings, err := client.ReadConfig(context.Background()); err != nil || settings.Volume != tc.fixture.Settings.Volume {
				t.Fatal("settings reached the wrong bus", settings, err)
			}
			if _, err := client.Call(context.Background(), "Manager", "RegisterAgent", device.Path("Agent")); err != nil {
				t.Fatal(err)
			}
			tc.fixture.Mu.Lock()
			defer tc.fixture.Mu.Unlock()
			if tc.fixture.AgentSender != client.Conn.Names()[0] {
				t.Fatal("maintenance registration used another connection")
			}
		})
	}
}

func TestTypedDomainsOnOnePrivateConnection(t *testing.T) {
	fixture := devicetest.New(t)
	client, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		value     any
		signature string
	}{
		{device.Settings{}, "(ssub(bs(uu)(uu))b)"},
		{device.UpdateStatus{}, "(ssissbsssbbbsss)"},
		{device.Release{}, "(sstsssbbss)"},
		{device.AudioStatus{}, "(ssu)"},
	} {
		if got := dbus.SignatureOf(tc.value).String(); got != tc.signature {
			t.Fatal(got, tc.signature)
		}
	}
	revision, settings, err := client.ReadConfig(ctx)
	if err != nil || settings.Volume != 100 {
		t.Fatal(settings, err)
	}
	settings.Volume, settings.Updates.Start = 42, device.HM{Hour: 23, Min: 45}
	next, err := client.UpdateConfig(ctx, revision, settings)
	if err != nil || next == revision {
		t.Fatal(next, err)
	}
	if _, err := client.UpdateConfig(ctx, revision, settings); !errors.Is(err, device.ErrRefused) {
		t.Fatal("stale CAS accepted", err)
	}
	_, saved, err := client.ReadConfig(ctx)
	if err != nil || saved != settings {
		t.Fatal("typed nested structure", saved, err)
	}
	sshRevision, _, err := client.SSHKeys(ctx)
	if err != nil || sshRevision == "" {
		t.Fatal(sshRevision, err)
	}
	sshNext, err := client.SetSSHKeys(ctx, sshRevision, "public-key")
	if err != nil || sshNext == sshRevision || sshNext == "" {
		t.Fatal(sshNext, err)
	}
	if revision, keys, err := client.SSHKeys(ctx); err != nil || revision != sshNext || keys != "public-key" {
		t.Fatal(revision, keys, err)
	}
	if _, err := client.SetSSHKeys(ctx, sshRevision, "stale-key"); !errors.Is(err, device.ErrRefused) {
		t.Fatal("stale SSH revision accepted", err)
	}
	sshRenewed, err := client.SetSSHKeys(ctx, sshNext, "public-key")
	if err != nil || sshRenewed == sshNext {
		t.Fatal("identical SSH commit retained revision", sshRenewed, err)
	}
	sshNext = sshRenewed
	if revision, keys, err := client.SSHKeys(ctx); err != nil || revision != sshNext || keys != "public-key" {
		t.Fatal("stale write changed SSH snapshot", revision, keys, err)
	}
	if revision, _, err := client.ReadConfig(ctx); err != nil || revision != next {
		t.Fatal("SSH commit changed config revision", revision, err)
	}
	now := time.Date(2026, 9, 30, 12, 34, 0, 0, time.UTC)
	if err := client.SetTime(ctx, now); err != nil {
		t.Fatal(err)
	}
	if quality, clock, err := client.Clock(ctx); err != nil || quality != "manual" || !clock.Equal(now) {
		t.Fatal(quality, clock, err)
	}
	if connectivity, err := client.Connectivity(ctx); err != nil || connectivity != "ok" {
		t.Fatal(connectivity, err)
	}
	if supported, err := client.VoiceSupported(ctx); err != nil || !supported {
		t.Fatal(supported, err)
	}
	if err := client.EnableVoice(ctx, true); err != nil {
		t.Fatal(err)
	}
	if state, err := client.VoiceState(ctx); err != nil || state != "idle" {
		t.Fatal(state, err)
	}
	if err := client.VoiceCommand(ctx, "start_listening"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "Manager", "RegisterAgent", device.Path("Agent")); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	if fixture.AgentSender != client.Conn.Names()[0] || !fixture.Settings.VoiceEnabled || fixture.LastVoiceCommand != "start_listening" {
		t.Fatal("domains did not share connection/state")
	}
	fixture.Configured = true
	fixture.Catalog = []device.Release{{Tag: "v1.1.0", Ready: true, Size: 9876543210, Published: "2026-09-30T10:00:00Z"}}
	fixture.Mu.Unlock()
	if configured, err := client.UpdatesConfigured(ctx); err != nil || !configured {
		t.Fatal(configured, err)
	}
	if releases, err := client.CheckUpdates(ctx); err != nil || releases[0].Size != 9876543210 {
		t.Fatal(releases, err)
	}
	if releases, err := client.Releases(ctx, "stable"); err != nil || len(releases) != 1 {
		t.Fatal(releases, err)
	}
	if id, err := client.InstallUpdate(ctx, "v1.1.0", "stable", false, true); err != nil || id == "" {
		t.Fatal(id, err)
	}
	if status, err := client.UpdateStatus(ctx); err != nil || status.OperationID == "" {
		t.Fatal(status, err)
	}
	id, err := client.StartAudio(ctx, "file", "/audio/test.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if status, err := client.AudioStatus(ctx); err != nil || status.ID != id {
		t.Fatal(status, err)
	}
	if outcome, err := client.WaitAudio(ctx, id); err != nil || outcome != "completed" {
		t.Fatal(outcome, err)
	}
	if err := client.StopAudio(ctx, id); err != nil {
		t.Fatal(err)
	}
	fixture.Mu.Lock()
	fixture.FailSSH = true
	fixture.Mu.Unlock()
	if _, err := client.SetSSHKeys(ctx, sshNext, "secret"); !errors.Is(err, device.ErrRefused) {
		t.Fatal("remote errors exposed", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := client.ReadConfig(cancelled); err == nil {
		t.Fatal("cancelled call continued")
	}
	// Restarting a domain invalidates old revisions without changing the Go bus.
	fixture.Mu.Lock()
	fixture.Revision = "new-instance:1"
	fixture.SSHRevision, fixture.FailSSH = "ssh-new-instance:1", false
	fixture.Mu.Unlock()
	if _, err := client.UpdateConfig(ctx, next, settings); !errors.Is(err, device.ErrRefused) {
		t.Fatal("restart did not invalidate CAS", err)
	}
	if _, err := client.SetSSHKeys(ctx, sshNext, "stale-daemon-key"); !errors.Is(err, device.ErrRefused) {
		t.Fatal("restart did not invalidate SSH revision", err)
	}
	if revision, keys, err := client.SSHKeys(ctx); err != nil || revision != "ssh-new-instance:1" || keys != "public-key" {
		t.Fatal("restart refusal changed SSH keys", revision, keys, err)
	}
}

func TestCatalogueChecksAllowSlowRepliesAndHonorCallerCancellation(t *testing.T) {
	fixture := devicetest.New(t)
	client, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	fixture.Mu.Lock()
	fixture.CheckDelay = device.Timeout + time.Second
	fixture.Mu.Unlock()
	if _, err := client.CheckUpdates(context.Background()); err != nil {
		t.Fatal("valid slow catalogue rejected", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := client.CheckUpdates(ctx); !errors.Is(err, device.ErrUnavailable) {
		t.Fatal("caller cancellation ignored", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled check waited for catalogue")
	}
}

// Exercise the shared invocation boundary through both its bounded Call entry
// and WaitAudio, which intentionally has no playback-duration deadline.
func TestInvocationsAuthenticateAndFenceOwners(t *testing.T) {
	for _, operation := range []string{"Call", "WaitAudio"} {
		for _, pinned := range []bool{false, true} {
			for _, mode := range []string{"trusted", "wrong account", "account absent", "owner absent", "owner changes during authentication", "owner changes during call", "account changes during call", "pinned account revoked", "pinned public name"} {
				if !pinned && (mode == "pinned account revoked" || mode == "pinned public name") {
					continue
				}
				t.Run(fmt.Sprintf("%s/pinned=%t/%s", operation, pinned, mode), func(t *testing.T) {
					fixture := devicetest.New(t)
					client, err := device.Open()
					if err != nil {
						t.Fatal(err)
					}
					defer client.Close()
					owner := fixture.Conn.Names()[0]
					if pinned {
						client = client.ForOwner(owner)
					}
					if mode == "pinned public name" {
						client = client.ForOwner(device.Destination)
					}
					replacement, err := dbus.ConnectSystemBus()
					if err != nil {
						t.Fatal(err)
					}
					defer replacement.Close()
					changeOwner := func() error {
						if _, err := fixture.Conn.ReleaseName(device.Destination); err != nil {
							return err
						}
						reply, err := replacement.RequestName(device.Destination, dbus.NameFlagDoNotQueue)
						if err != nil {
							return err
						}
						if reply != dbus.RequestNameReplyPrimaryOwner {
							return fmt.Errorf("replacement ownership: %v", reply)
						}
						return nil
					}
					client.Conn.Close()
					conn, err := dbus.ConnectSystemBus(devicetest.CredentialReplies(func() {
						if mode == "owner changes during authentication" {
							if err := changeOwner(); err != nil {
								t.Error(err)
							}
						}
					})...)
					if err != nil {
						t.Fatal(err)
					}
					client.Conn = conn
					defer conn.Close()
					expected := os.Getenv("NABOS_DEVICE_USER")
					t.Setenv("NABOS_DEVICE_USER", expected)
					switch mode {
					case "wrong account":
						t.Setenv("NABOS_DEVICE_USER", devicetest.WrongUser(t))
					case "account absent":
						t.Setenv("NABOS_DEVICE_USER", "nabos-no-such-test-account")
					case "owner absent":
						if _, err := fixture.Conn.ReleaseName(device.Destination); err != nil {
							t.Fatal(err)
						}
					}
					var calls, replacements atomic.Int32
					handle := func(message dbus.Message) *dbus.Error {
						calls.Add(1)
						if got := message.Headers[dbus.FieldDestination].Value(); got != owner {
							t.Errorf("operation destination = %v, want authenticated unique owner %s", got, owner)
						}
						if mode == "owner changes during call" {
							if err := changeOwner(); err != nil {
								return dbus.MakeFailedError(err)
							}
						}
						if mode == "account changes during call" {
							os.Setenv("NABOS_DEVICE_USER", devicetest.WrongUser(t))
						}
						return nil
					}
					if err := fixture.Conn.ExportMethodTable(map[string]interface{}{
						"Start": func(message dbus.Message, kind, source string) (string, *dbus.Error) {
							if kind != "file" || source != "/private/source.wav" {
								t.Error("Start arguments changed", kind, source)
							}
							return "started", handle(message)
						},
						"Wait": func(message dbus.Message, id string) (string, *dbus.Error) {
							if id != "private-audio-id" {
								t.Error("Wait argument changed", id)
							}
							return "completed", handle(message)
						},
					}, device.Path("Audio"), device.Interface("Audio")); err != nil {
						t.Fatal(err)
					}
					if err := replacement.ExportMethodTable(map[string]interface{}{
						"Start": func(message dbus.Message, kind, source string) (string, *dbus.Error) {
							replacements.Add(1)
							if message.Headers[dbus.FieldDestination].Value() != replacement.Names()[0] {
								t.Error("replacement reached through public name")
							}
							return "replacement-start", nil
						},
						"Wait": func(message dbus.Message, id string) (string, *dbus.Error) {
							replacements.Add(1)
							if message.Headers[dbus.FieldDestination].Value() != replacement.Names()[0] {
								t.Error("replacement reached through public name")
							}
							return "replacement-completion", nil
						},
					}, device.Path("Audio"), device.Interface("Audio")); err != nil {
						t.Fatal(err)
					}
					invoke := func() (string, error) {
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						if operation == "WaitAudio" {
							return client.WaitAudio(ctx, "private-audio-id")
						}
						call, err := client.Call(ctx, "Audio", "Start", "file", "/private/source.wav")
						if err != nil {
							if call != nil {
								t.Error("rejected invocation exposed its reply")
							}
							return "", err
						}
						var result string
						if err := call.Store(&result); err != nil {
							return "", err
						}
						return result, nil
					}
					if mode == "pinned account revoked" {
						if _, err := invoke(); err != nil {
							t.Fatal("initial pinned invocation", err)
						}
						os.Setenv("NABOS_DEVICE_USER", devicetest.WrongUser(t))
					}
					result, err := invoke()
					wantCalls := int32(0)
					if mode == "trusted" {
						if err != nil || result == "" {
							t.Fatal("trusted invocation rejected", result, err)
						}
						wantCalls = 1
					} else {
						if !errors.Is(err, device.ErrUnavailable) || result != "" {
							t.Fatal("authentication or ownership failure exposed a result", result, err)
						}
						if mode == "owner changes during call" || mode == "account changes during call" || mode == "pinned account revoked" {
							wantCalls = 1
						}
					}
					if calls.Load() != wantCalls || replacements.Load() != 0 {
						t.Fatal("unauthenticated IPC or replacement fallback", calls.Load(), replacements.Load())
					}
					if mode == "owner changes during call" {
						// The old connection remains alive and still exports the methods.
						// A pinned client must reject it before IPC after the name changes.
						result, err = invoke()
						if pinned {
							if !errors.Is(err, device.ErrUnavailable) || result != "" || replacements.Load() != 0 {
								t.Fatal("stale pinned client used a live old owner or its replacement", result, err)
							}
						} else if err != nil || result == "" || replacements.Load() != 1 {
							t.Fatal("unbound client did not authenticate the replacement", result, err)
						}
						if calls.Load() != 1 {
							t.Fatal("stale old owner received another invocation", calls.Load())
						}
					}
				})
			}
		}
	}
}

func TestWaitAudioAllowsLongPlaybackAndHonorsCallerCancellation(t *testing.T) {
	fixture := devicetest.New(t)
	client, err := device.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := fixture.Conn.ExportMethodTable(map[string]interface{}{
		"Wait": func(id string) (string, *dbus.Error) {
			time.Sleep(device.Timeout + 100*time.Millisecond)
			return "completed", nil
		},
	}, device.Path("Audio"), device.Interface("Audio")); err != nil {
		t.Fatal(err)
	}
	if outcome, err := client.WaitAudio(context.Background(), "long-playback"); err != nil || outcome != "completed" {
		t.Fatal("playback was bounded by the ordinary call timeout", outcome, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if outcome, err := client.WaitAudio(ctx, "cancelled-playback"); !errors.Is(err, device.ErrUnavailable) || outcome != "" {
		t.Fatal("caller cancellation exposed an audio result", outcome, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled audio wait waited for playback")
	}
}
