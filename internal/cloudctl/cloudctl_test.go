package cloudctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHeartbeatAcceptsAndStores(t *testing.T) {
	st := &Station{
		StationID:    "FM-TEST1",
		APIKey:       "abc123",
		Tier:         "private",
		FeaturesJSON: map[string]any{"whatsapp_enabled": false},
		ValidFrom:    time.Now(),
		Status:       "active",
	}
	srv := NewWithStations(st)
	httpSrv := srv.Start()
	defer httpSrv.Close()

	body := map[string]any{
		"station_id":            "FM-TEST1",
		"software_version":      "test",
		"hardware_tier":         "basic",
		"timestamp":             time.Now().UTC().Format(time.RFC3339),
		"database_size_mb":      1,
		"data_health_score":     99,
		"last_pos_ingestion_at": time.Now().UTC().Format(time.RFC3339),
		"active_alerts_count":   0,
		"disk_free_gb":          10,
		"app_uptime_hours":      1,
		"errors_last_24h":       0,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", httpSrv.URL+"/v1/heartbeat", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer abc123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	beats := srv.Heartbeats()
	if len(beats) != 1 {
		t.Fatalf("expected 1 heartbeat, got %d", len(beats))
	}
	if beats[0].StationID != "FM-TEST1" {
		t.Errorf("station_id = %q", beats[0].StationID)
	}
}

func TestHeartbeatRejectsBadKey(t *testing.T) {
	srv := NewWithStations(&Station{
		StationID: "FM-TEST1", APIKey: "right-key",
		Tier: "private", ValidFrom: time.Now(), Status: "active",
	})
	httpSrv := srv.Start()
	defer httpSrv.Close()

	req, _ := http.NewRequest("POST", httpSrv.URL+"/v1/heartbeat",
		bytes.NewReader([]byte(`{"station_id":"FM-TEST1"}`)))
	req.Header.Set("Authorization", "Bearer wrong-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHeartbeatRejectsKeyStationMismatch(t *testing.T) {
	srv := NewWithStations(
		&Station{StationID: "FM-TEST1", APIKey: "key1", Tier: "private", Status: "active", ValidFrom: time.Now()},
		&Station{StationID: "FM-TEST2", APIKey: "key2", Tier: "private", Status: "active", ValidFrom: time.Now()},
	)
	httpSrv := srv.Start()
	defer httpSrv.Close()

	req, _ := http.NewRequest("POST", httpSrv.URL+"/v1/heartbeat",
		bytes.NewReader([]byte(`{"station_id":"FM-TEST2"}`)))
	req.Header.Set("Authorization", "Bearer key1") // key1 belongs to TEST1
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLicenseEndpoint(t *testing.T) {
	srv := NewWithStations(&Station{
		StationID:    "FM-TEST1",
		APIKey:       "k",
		Tier:         "connected",
		FeaturesJSON: map[string]any{"whatsapp_enabled": true},
		ValidFrom:    time.Now(),
		Status:       "active",
	})
	httpSrv := srv.Start()
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/v1/license/FM-TEST1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		StationID string         `json:"station_id"`
		Tier      string         `json:"tier"`
		Features  map[string]any `json:"features_json"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.StationID != "FM-TEST1" || got.Tier != "connected" {
		t.Errorf("got %+v", got)
	}
	if got.Features["whatsapp_enabled"] != true {
		t.Errorf("features = %v", got.Features)
	}
}

func TestLicenseEndpoint404(t *testing.T) {
	srv := New()
	httpSrv := srv.Start()
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/v1/license/FM-NOPE")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRingBufferCap(t *testing.T) {
	srv := NewWithStations(&Station{
		StationID: "FM-TEST1", APIKey: "k", Tier: "private",
		Status: "active", ValidFrom: time.Now(),
	})
	srv.SetHeartbeatCap(3)
	httpSrv := srv.Start()
	defer httpSrv.Close()

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", httpSrv.URL+"/v1/heartbeat",
			bytes.NewReader([]byte(`{"station_id":"FM-TEST1","software_version":"x"}`)))
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", "application/json")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
	}
	beats := srv.Heartbeats()
	if len(beats) != 3 {
		t.Errorf("ring cap not applied: got %d, want 3", len(beats))
	}
}