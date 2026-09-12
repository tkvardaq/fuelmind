package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/cloudctl"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func rig(t *testing.T, urlSuffix string) (*Agent, *storage.Storage, *cloudctl.Server) {
	t.Helper()
	store := newTestStorage(t)
	id, _ := ident.GenerateIdentity()
	cs := cloudctl.NewWithStations(&cloudctl.Station{StationID: id.StationID.String(), APIKey: id.APIKey.String(),
		Tier: "private", Status: "active", ValidFrom: time.Now()})
	hs := cs.Start()
	t.Cleanup(hs.Close)
	return newTestAgent(t, store, NewHTTPClient(hs.URL+urlSuffix), id, time.Hour), store, cs
}

func TestTelemetryOffSendsOnlyLicensingFields(t *testing.T) {
	a, store, cs := rig(t, "")
	_ = store.SetLocalConfig(context.Background(), ConfigKeyTelemetryConsent, "false", "", false)
	if err := a.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := cs.Heartbeats()[0].Payload
	for _, k := range []string{"hardware_tier", "database_size_mb", "disk_free_gb", "errors_last_24h", "data_health_score"} {
		if _, ok := p[k]; ok {
			t.Errorf("telemetry off but %q was sent", k)
		}
	}
	if p["station_id"] == nil || p["software_version"] == nil {
		t.Errorf("licensing fields missing: %v", p)
	}
}

func TestTelemetryOnSendsHealthFields(t *testing.T) {
	a, store, cs := rig(t, "")
	ctx := context.Background()
	_ = store.SetLocalConfig(ctx, ConfigKeyTelemetryConsent, "true", "", false)
	_, _ = store.WriteRawTransactions(ctx, "b", "lane_1", [][]byte{[]byte(`{}`)}, []string{"h"})
	if err := a.SendOnce(ctx); err != nil {
		t.Fatal(err)
	}
	p := cs.Heartbeats()[0].Payload
	if _, ok := p["disk_free_gb"]; !ok {
		t.Error("telemetry on but disk_free_gb missing")
	}
	if ts, _ := p["last_pos_ingestion_at"].(string); ts == "" || ts == "0001-01-01T00:00:00Z" {
		t.Errorf("last_pos_ingestion_at = %q, want the ingest time", ts)
	}
}

func TestRollbackReportedOnceWithVersion(t *testing.T) {
	a, store, cs := rig(t, "/") // trailing slash in the cloud URL is tolerated
	ctx := context.Background()
	rec, _ := json.Marshal(launcher.RollbackRecord{FailedVersion: "0.2.0", Reason: "crash loop", At: time.Now()})
	_ = store.SetLocalConfig(ctx, ConfigKeyLastRollback, string(rec), "", false)
	if err := a.SendOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rb, _ := cs.Heartbeats()[0].Payload["last_rollback"].(map[string]any)
	if rb["version"] != "0.2.0" || rb["reason"] != "crash loop" {
		t.Errorf("last_rollback = %v", rb)
	}
	if _, err := store.GetLocalConfig(ctx, ConfigKeyLastRollback); err != storage.ErrNotFound {
		t.Errorf("rollback record not cleared after a successful heartbeat: %v", err)
	}
	_ = a.SendOnce(ctx)
	if _, again := cs.Heartbeats()[1].Payload["last_rollback"]; again {
		t.Error("rollback reported twice")
	}
}
