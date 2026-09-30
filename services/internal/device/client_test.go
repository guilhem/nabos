package device_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/guilhem/nabos/services/internal/device"
	"github.com/guilhem/nabos/services/internal/devicetest"
)

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
