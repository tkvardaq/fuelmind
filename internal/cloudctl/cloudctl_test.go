package cloudctl

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := NewWithStations(&Station{
		StationID:    "FM-TEST1",
		APIKey:       "abc123",
		Tier:         "private",
		FeaturesJSON: map[string]any{"whatsapp_enabled": false},
		ValidFrom:    time.Now(),
		Status:       "active",
	})
	h := s.Start()
	t.Cleanup(h.Close)
	return s, h.URL
}

func do(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHeartbeatAcceptsAndStores(t *testing.T) {
	s, url := testServer(t)
	body := `{"station_id":"FM-TEST1","software_version":"test","timestamp":"2026-09-11T00:00:00Z"}`
	if resp := do(t, "POST", url+"/v1/heartbeat", "abc123", body); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	beats := s.Heartbeats()
	if len(beats) != 1 || beats[0].StationID != "FM-TEST1" {
		t.Fatalf("heartbeats = %+v", beats)
	}
}

func TestHeartbeatRejectsBadKey(t *testing.T) {
	_, url := testServer(t)
	if resp := do(t, "POST", url+"/v1/heartbeat", "wrong-key", `{"station_id":"FM-TEST1"}`); resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHeartbeatRejectsKeyStationMismatch(t *testing.T) {
	s, url := testServer(t)
	s.AddStation(&Station{StationID: "FM-TEST2", APIKey: "key2", Tier: "private", Status: "active", ValidFrom: time.Now()})
	if resp := do(t, "POST", url+"/v1/heartbeat", "key2", `{"station_id":"FM-TEST1"}`); resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestHeartbeatRejectsSuspendedStation(t *testing.T) {
	s, url := testServer(t)
	st, _ := s.LicenseFor("FM-TEST1")
	st.Status = "suspended"
	if resp := do(t, "POST", url+"/v1/heartbeat", "abc123", `{"station_id":"FM-TEST1"}`); resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLicenseEndpointRequiresOwnKey(t *testing.T) {
	s, url := testServer(t)
	st, _ := s.LicenseFor("FM-TEST1")
	st.Tier = "connected"
	st.FeaturesJSON = map[string]any{"whatsapp_enabled": true}

	if resp := do(t, "GET", url+"/v1/license/FM-TEST1", "", ""); resp.StatusCode != 401 {
		t.Errorf("unauthenticated license read = %d, want 401", resp.StatusCode)
	}
	resp := do(t, "GET", url+"/v1/license/FM-TEST1", "abc123", "")
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
	if got.StationID != "FM-TEST1" || got.Tier != "connected" || got.Features["whatsapp_enabled"] != true {
		t.Errorf("got %+v", got)
	}
}

func TestRingBufferCap(t *testing.T) {
	s, url := testServer(t)
	s.SetHeartbeatCap(3)
	for i := 0; i < 5; i++ {
		do(t, "POST", url+"/v1/heartbeat", "abc123", `{"station_id":"FM-TEST1","software_version":"x"}`)
	}
	if n := len(s.Heartbeats()); n != 3 {
		t.Errorf("ring cap not applied: got %d, want 3", n)
	}
}

// --- update checks ---

func withRelease(t *testing.T) (*Server, string) {
	s, url := testServer(t)
	s.AddRelease(UpdateRelease{
		Version: "1.1.0", ArtifactURL: "/artifacts/fuelmind-core-1.1.0.zip",
		ChecksumSHA256: "a1", MinHardwareTier: "basic", RolloutPct: 100,
	})
	return s, url
}

func TestUpdateCheckNeedsStationKey(t *testing.T) {
	_, url := withRelease(t)
	if resp := do(t, "GET", url+"/v1/updates/check?station_id=FM-TEST1&version=1.0.0", "", ""); resp.StatusCode != 401 {
		t.Errorf("unauthenticated update check = %d, want 401", resp.StatusCode)
	}
}

// The core sends its hardware tier; a release must not be withheld
// because the query says "enhanced" rather than the licence tier.
func TestUpdateCheckOffersReleaseForAnyHardwareTier(t *testing.T) {
	_, url := withRelease(t)
	for _, tier := range []string{"basic", "standard", "enhanced", "pro"} {
		resp := do(t, "GET", url+"/v1/updates/check?station_id=FM-TEST1&version=1.0.0&tier=private&hardware_tier="+tier, "abc123", "")
		if resp.StatusCode != 200 {
			t.Errorf("hardware_tier=%s: status %d, want 200", tier, resp.StatusCode)
		}
	}
}

func TestUpdateCheckRespectsMinTierAndCurrentVersion(t *testing.T) {
	s, url := withRelease(t)
	s.AddRelease(UpdateRelease{Version: "2.0.0", ArtifactURL: "/a.zip", ChecksumSHA256: "b2", MinHardwareTier: "pro", RolloutPct: 100})

	// A basic station gets 1.1.0, not the pro-only 2.0.0.
	resp := do(t, "GET", url+"/v1/updates/check?station_id=FM-TEST1&version=1.0.0&hardware_tier=basic", "abc123", "")
	var rel UpdateRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatal(err)
	}
	if rel.Version != "1.1.0" {
		t.Errorf("basic station offered %q, want 1.1.0", rel.Version)
	}
	// Already on the newest release: 204.
	if r := do(t, "GET", url+"/v1/updates/check?station_id=FM-TEST1&version=2.0.0&hardware_tier=pro", "abc123", ""); r.StatusCode != 204 {
		t.Errorf("up-to-date station got %d, want 204", r.StatusCode)
	}
}

func TestUpdateCheckRespectsRollout(t *testing.T) {
	s, url := testServer(t)
	s.AddRelease(UpdateRelease{Version: "9.9.9", ArtifactURL: "/a.zip", ChecksumSHA256: "c3", RolloutPct: 0})
	if r := do(t, "GET", url+"/v1/updates/check?station_id=FM-TEST1&version=1.0.0", "abc123", ""); r.StatusCode != 204 {
		t.Errorf("rollout_pct=0 offered an update (%d)", r.StatusCode)
	}
}

// --- admin ---

func TestAdminRequiresToken(t *testing.T) {
	s, url := testServer(t)
	s.AdminToken = "s3cret"
	if r := do(t, "PUT", url+"/v1/admin/stations/FM-TEST1/license", "", `{"tier":"connected_hq"}`); r.StatusCode != 401 {
		t.Errorf("unauthenticated admin PUT = %d, want 401", r.StatusCode)
	}
	if r := do(t, "PUT", url+"/v1/admin/stations/FM-TEST1/license", "wrong", `{"tier":"connected_hq"}`); r.StatusCode != 401 {
		t.Errorf("bad admin token = %d, want 401", r.StatusCode)
	}
	st, _ := s.LicenseFor("FM-TEST1")
	if st.Tier != "private" {
		t.Fatalf("tier changed without a valid token: %q", st.Tier)
	}
	if r := do(t, "PUT", url+"/v1/admin/stations/FM-TEST1/license", "s3cret", `{"tier":"connected"}`); r.StatusCode != 200 {
		t.Fatalf("admin PUT = %d, want 200", r.StatusCode)
	}
	if st, _ = s.LicenseFor("FM-TEST1"); st.Tier != "connected" {
		t.Errorf("tier = %q, want connected", st.Tier)
	}
}

func TestAdminDisabledWithoutToken(t *testing.T) {
	_, url := testServer(t)
	if r := do(t, "PUT", url+"/v1/admin/stations/FM-TEST1/license", "anything", `{"tier":"connected"}`); r.StatusCode != 401 {
		t.Errorf("admin API should be disabled when no token is configured, got %d", r.StatusCode)
	}
}

func TestAdminAcceptsBothPathShapes(t *testing.T) {
	s, url := testServer(t)
	s.AdminToken = "tok"
	for _, p := range []string{"/v1/admin/stations/FM-TEST1", "/v1/admin/stations/FM-TEST1/license"} {
		if r := do(t, "PUT", url+p, "tok", `{"status":"active"}`); r.StatusCode != 200 {
			t.Errorf("PUT %s = %d, want 200", p, r.StatusCode)
		}
	}
}
