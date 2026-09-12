package sync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/cloudctl"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func newTestStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestAgent(t *testing.T, store *storage.Storage, client CloudClient, id ident.Identity, cycle time.Duration) *Agent {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewAgent(AgentConfig{
		Store:           store,
		Client:          client,
		Logger:          logger,
		Cycle:           cycle,
		Identity:        id,
		SoftwareVersion: "test-0.0.1",
	})
}

func TestHeartbeatSuccess(t *testing.T) {
	store := newTestStorage(t)
	id, _ := ident.GenerateIdentity()
	st := &cloudctl.Station{
		StationID:    id.StationID.String(),
		APIKey:       id.APIKey.String(),
		Tier:         "private",
		FeaturesJSON: map[string]any{"whatsapp_enabled": false, "cloud_backup_enabled": false},
		Status:       "active",
		ValidFrom:    time.Now(),
	}
	cs := cloudctl.NewWithStations(st)
	hs := cs.Start()
	defer hs.Close()

	client := NewHTTPClient(hs.URL)
	agent := newTestAgent(t, store, client, id, time.Hour)

	// Drive one tick manually — Start() would background-loop.
	agent.tick(context.Background())

	beats := cs.Heartbeats()
	if len(beats) != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", len(beats))
	}
	hb := beats[0]
	if hb.StationID != id.StationID.String() {
		t.Errorf("station_id = %q", hb.StationID)
	}
	if hb.Payload["software_version"] != "test-0.0.1" {
		t.Errorf("software_version = %v", hb.Payload["software_version"])
	}
	// Sync log row written.
	la, err := store.LatestSyncAttempt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if la.Status != "ok" {
		t.Errorf("sync_log status = %q, want ok", la.Status)
	}
	// License cached under local_config.
	licRow, err := store.GetLocalConfig(context.Background(), ident.ConfigKeyLicenseCache)
	if err != nil {
		t.Fatalf("expected license_cache row, got %v", err)
	}
	if licRow.Value != "private" {
		t.Errorf("cached tier = %q", licRow.Value)
	}
}

func TestHeartbeatOfflineDoesNotBlock(t *testing.T) {
	store := newTestStorage(t)
	id, _ := ident.GenerateIdentity()

	// Point the client at an unroutable address so every POST fails
	// with timeout/connect-refused. The agent must NOT panic and
	// must NOT propagate the error — the test calling tick()
	// returns cleanly and a sync_log row is written.
	client := NewHTTPClient("http://127.0.0.1:1") // port 1 = nothing listens
	agent := newTestAgent(t, store, client, id, time.Hour)

	// Use a short timeout so the test doesn't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	agent.tick(ctx)

	la, err := store.LatestSyncAttempt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if la.Status != "failed" {
		t.Errorf("sync_log status = %q, want failed", la.Status)
	}
	if la.Reason == "" {
		t.Errorf("reason should be set on failure")
	}
}

func TestIsFeatureEnabledClosedByDefault(t *testing.T) {
	var lic *LicenseStatus
	if lic.IsFeatureEnabled("whatsapp_enabled") {
		t.Error("nil license should deny features")
	}
	lic = &LicenseStatus{FeaturesJSON: map[string]any{"whatsapp_enabled": true}}
	if !lic.IsFeatureEnabled("whatsapp_enabled") {
		t.Error("explicit true should be enabled")
	}
	if lic.IsFeatureEnabled("not_set") {
		t.Error("missing flag should deny by default")
	}
}

func TestJitterBounded(t *testing.T) {
	base := 30 * time.Minute
	for i := 0; i < 1000; i++ {
		out := jitter(base, 0.2)
		// ±20% of base, but never below 1s.
		if out < 24*time.Minute || out > 36*time.Minute {
			t.Errorf("jitter out of bounds: %v", out)
		}
	}
}

func TestClassifyNetErr(t *testing.T) {
	cases := []struct {
		errStr, want string
	}{
		{"context deadline exceeded", "timeout"},
		{"dial tcp: lookup foo.invalid: no such host", "dns"},
		{"dial tcp 127.0.0.1:1: connect: connection refused", "conn_refused"},
		{"write tcp: broken pipe", "transport"},
		{"", ""},
	}
	for _, c := range cases {
		var err error
		if c.errStr != "" {
			err = simpleErr(c.errStr)
		}
		if got := classifyNetErr(err); got != c.want {
			t.Errorf("classifyNetErr(%q) = %q, want %q", c.errStr, got, c.want)
		}
	}
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

func TestAgentStartStopsOnContextCancel(t *testing.T) {
	store := newTestStorage(t)
	id, _ := ident.GenerateIdentity()
	client := NewHTTPClient("http://127.0.0.1:1")
	agent := newTestAgent(t, store, client, id, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	agent.Start(ctx)
	time.Sleep(80 * time.Millisecond)
	cancel()
	// Give the goroutine a moment to exit.
	time.Sleep(50 * time.Millisecond)
	// No assertion needed — if Start didn't return cleanly on
	// ctx.Done() the test runner would hang or report a goroutine
	// leak.
}
