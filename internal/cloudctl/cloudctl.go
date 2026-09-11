// Package cloudctl is a tiny in-memory HTTP control plane used
// for local testing of the sync agent (Phase 5) and the unattended
// update flow (Phase 6).
//
 // Real production: a separate Hetzner-hosted service with a
 // Postgres + a real Go binary. cloudctl exists so the local core
 // has something to talk to on a developer laptop or in CI without
 // requiring a network round-trip.
 //
 // The package is intentionally minimal:
 //   - POST /v1/heartbeat accepts a spec §6.3 payload, validates
 //     station_id + api_key, records the row, returns license status.
 //   - GET  /v1/license/{station_id} returns the current license
 //     row + features_json.
 //
 // Tests use NewWithStations to seed a known station + API key,
 // then point the sync agent's CloudClient at the httptest.Server
 // URL.
 //
 // Persisted state is just two in-memory maps; the server is
 // disposable. Do NOT use this in production.
package cloudctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Station is one row of the in-memory stations table. Mirrors the
// shape of the production cloud's stations row but holds enough
// fields to drive license decisions.
type Station struct {
	StationID    string
	APIKey       string
	Tier         string         // "private" | "connected" | "connected_hq"
	FeaturesJSON map[string]any // feature flags per spec §6.2
	Status       string         // "active" | "suspended" | "expired"
	ValidFrom    time.Time
	ValidUntil   *time.Time
}

// UpdateRelease represents the update manifest returned by /v1/updates/check
type UpdateRelease struct {
	Version         string            `json:"version"`
	ReleaseNotes    string            `json:"release_notes,omitempty"`
	ArtifactURL     string            `json:"artifact_url"`
	ChecksumSHA256  string            `json:"checksum_sha256"`
	MinHardwareTier string            `json:"min_hardware_tier,omitempty"`
	RolloutPct      int               `json:"rollout_pct"`
}

// HeartbeatRecord is one row of the in-memory heartbeats table.
// Used by tests to assert "did the agent POST?" without having to
// parse logs.
type HeartbeatRecord struct {
	ReceivedAt time.Time
	StationID  string
	Payload    map[string]any
}

// Server is the in-memory control plane.
type Server struct {
	mu        sync.Mutex
	stations  map[string]*Station   // station_id -> station
	keys      map[string]string     // api_key -> station_id (reverse index)
	beats     []HeartbeatRecord
	maxBeats  int // ring-buffer cap; 0 = unlimited
}

// New returns a fresh Server with no stations preloaded.
func New() *Server {
	return &Server{
		stations: map[string]*Station{},
		keys:     map[string]string{},
	}
}

// NewWithStations is a convenience for tests: a server preloaded
// with the given station records. Each Station is added as-is;
// API keys are indexed for fast reverse lookup.
func NewWithStations(stations ...*Station) *Server {
	s := New()
	for _, st := range stations {
		s.AddStation(st)
	}
	return s
}

// AddStation registers a station. Idempotent — re-adding with the
// same station_id overwrites the prior record.
func (s *Server) AddStation(st *Station) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stations[st.StationID] = st
	s.keys[st.APIKey] = st.StationID
}

// SetHeartbeatCap sets a ring-buffer cap on retained heartbeat
// records. Default is unlimited (which is fine for unit tests
// that run a handful of cycles). Use 1024 or so for fuzz tests.
func (s *Server) SetHeartbeatCap(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxBeats = n
	// Trim if we're already over.
	if n > 0 && len(s.beats) > n {
		s.beats = s.beats[len(s.beats)-n:]
	}
}

// Heartbeats returns a snapshot of all received heartbeats in
// arrival order. Each element is a copy; mutating them won't
// affect server state.
func (s *Server) Heartbeats() []HeartbeatRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HeartbeatRecord, len(s.beats))
	copy(out, s.beats)
	return out
}

// LicenseFor returns the license view of one station. Useful in
// tests that need to assert "what does the server think of
// station X's tier?".
func (s *Server) LicenseFor(stationID string) (*Station, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stations[stationID]
	if !ok {
		return nil, ErrStationNotFound
	}
	return st, nil
}

// ErrStationNotFound is returned by LicenseFor when the station_id
// isn't registered. Mirrors the cloud's "404 Not Found" response.
var ErrStationNotFound = errors.New("cloudctl: station not found")

// Routes returns an http.Handler serving this server's API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/license/", s.handleLicense)
	mux.HandleFunc("/v1/updates/check", s.handleUpdatesCheck)
	mux.HandleFunc("/v1/admin/stations/", s.handleAdminStationLicense) // kept for backward compatibility?
	// New endpoint with /license suffix as per spec
	mux.HandleFunc("/v1/admin/stations/license/", s.handleAdminStationLicense)
	return mux
}

// Start launches an httptest.Server backed by this control plane
// and returns it. The caller must Close() the server when done.
func (s *Server) Start() *httptest.Server {
	return httptest.NewServer(s.Routes())
}

// startWithHandler is the test-friendly form: returns the httptest
// server AND its URL, so callers can plug straight into the
// sync agent's CloudClient.
//
// Deprecated: use Start and read .URL instead.
func (s *Server) startWithHandler() (h *httptest.Server, url string) {
	h = s.Start()
	return h, h.URL
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// handleHeartbeat accepts POST /v1/heartbeat (spec §6.3 payload).
// Validates the api_key against the registered stations; on match
// records the row and returns the current license block.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth: api_key arrives in Authorization: Bearer <key>. We
	// also accept X-FuelMind-Station as a courtesy (the real
	// production server would derive station from the key alone).
	apiKey, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	stationID, _ := payload["station_id"].(string)
	if stationID == "" {
		http.Error(w, "missing station_id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Resolve station_id from key first, then cross-check payload.
	resolvedID, ok := s.keys[apiKey]
	if !ok {
		http.Error(w, "unknown api key", http.StatusUnauthorized)
		return
	}
	if resolvedID != stationID {
		http.Error(w, "station_id does not match api key", http.StatusForbidden)
		return
	}
	st, ok := s.stations[stationID]
	if !ok {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	if st.Status != "active" {
		http.Error(w, "station "+st.Status, http.StatusForbidden)
		return
	}

	// Record the heartbeat (ring-buffered if cap is set).
	s.beats = append(s.beats, HeartbeatRecord{
		ReceivedAt: time.Now(),
		StationID:  stationID,
		Payload:    payload,
	})
	if s.maxBeats > 0 && len(s.beats) > s.maxBeats {
		s.beats = s.beats[len(s.beats)-s.maxBeats:]
	}

	// Reply with the license block.
	writeJSON(w, http.StatusOK, heartbeatResponse{
		OK:      true,
		License: licenseView(st),
	})
}

// handleLicense returns GET /v1/license/{station_id}. The station
// ID is the path suffix after /v1/license/. Returns 404 if no
// such station.
func (s *Server) handleLicense(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/license/")
	if id == "" {
		http.Error(w, "missing station_id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stations[id]
	if !ok {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, licenseView(st))
}

// heartbeatResponse mirrors the spec §6.3 success shape.
type heartbeatResponse struct {
	OK      bool         `json:"ok"`
	License licenseBlock `json:"license"`
}

type licenseBlock struct {
	StationID    string         `json:"station_id"`
	Tier         string         `json:"tier"`
	FeaturesJSON map[string]any `json:"features_json"`
	ValidFrom    time.Time      `json:"valid_from"`
	ValidUntil   *time.Time     `json:"valid_until,omitempty"`
	Status       string         `json:"status"`
}

func licenseView(st *Station) licenseBlock {
	return licenseBlock{
		StationID:    st.StationID,
		Tier:         st.Tier,
		FeaturesJSON: st.FeaturesJSON,
		ValidFrom:    st.ValidFrom,
		ValidUntil:   st.ValidUntil,
		Status:       st.Status,
	}
}

func bearer(h string) (string, bool) {
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return "", false
	}
	return h[len(p):], true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrUnsupported is returned by NewWithStation when a nil station
// is supplied. Reserved for future explicit-validation paths.
var ErrUnsupported = errors.New("cloudctl: unsupported")

// BuildHeartbeatResponse is a tiny helper for tests that want to
// construct a canned response without going through the server.
// Lets tests assert on the agent's parsing without an httptest
// round-trip.
func BuildHeartbeatResponse(st *Station) any {
	return heartbeatResponse{OK: true, License: licenseView(st)}
}

// Describe renders a one-line summary of the station, for use in
// test failure messages.
func (st *Station) Describe() string {
	return fmt.Sprintf("%s(tier=%s, status=%s)", st.StationID, st.Tier, st.Status)
}

// handleUpdatesCheck returns GET /v1/updates/check?version=X&tier=Y.
// It returns 200 with an update manifest if an update is available for
// the station (based on version, tier, and rollout_pct), or 204 (No Content)
// if no update is available.
//
// In this test implementation, we return a hardcoded update if:
// - The station is at version "0.1.0" (the initial version)
// - The rollout_pct is 100 (for simplicity in tests)
// In a real implementation, this would check against stored releases
// and apply the rollout percentage.
func (s *Server) handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	v := r.URL.Query()
	stationID := v.Get("station_id")
	currentVersion := v.Get("version")
	currentTier := v.Get("tier")

	if stationID == "" {
		http.Error(w, "missing station_id", http.StatusBadRequest)
		return
	}
	if currentVersion == "" {
		http.Error(w, "missing version", http.StatusBadRequest)
		return
	}
	if currentTier == "" {
		http.Error(w, "missing tier", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if station exists and is active
	st, ok := s.stations[stationID]
	if !ok {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}
	if st.Status != "active" {
		http.Error(w, "station "+st.Status, http.StatusForbidden)
		return
	}

	// For test simplicity: if we're at version 0.1.0, offer an update to 0.2.0
	// In reality, this would check the update_releases table and apply rollout_pct
	if currentVersion == "0.1.0" && currentTier == "private" {
		// Return a fake update manifest
		update := UpdateRelease{
			Version:         "0.2.0",
			ReleaseNotes:    "Add unattended update mechanism",
			ArtifactURL:     "https://example.com/fuelmind-core-v0.2.0.zip",
			ChecksumSHA256:  "fakechecksum1234567890abcdef",
			MinHardwareTier: "basic",
			RolloutPct:      100,
		}
		writeJSON(w, http.StatusOK, update)
		return
	}

	// No update available
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminStationLicense handles PUT /v1/admin/stations/{station_id}/license
// Updates the station's features_json and/or tier.
func (s *Server) handleAdminStationLicense(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path
	// Support both /v1/admin/stations/{station_id} and /v1/admin/stations/{station_id}/license
	var id string
	if strings.HasPrefix(path, "/v1/admin/stations/") {
		rest := strings.TrimPrefix(path, "/v1/admin/stations/")
		// If rest ends with "/license", strip it
		if strings.HasSuffix(rest, "/license") {
			id = strings.TrimSuffix(rest, "/license")
		} else {
			id = rest
		}
	} else {
		http.Error(w, "invalid path", http.StatusNotFound)
		return
	}
	if id == "" {
		http.Error(w, "missing station_id", http.StatusBadRequest)
		return
	}

	// Expect JSON body with features_json (and optionally tier)
	var req struct {
		FeaturesJSON map[string]any `json:"features_json"`
		Tier         string         `json:"tier,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.stations[id]
	if !ok {
		http.Error(w, "station not found", http.StatusNotFound)
		return
	}

	// Update fields if provided
	if req.FeaturesJSON != nil {
		st.FeaturesJSON = req.FeaturesJSON
	}
	if req.Tier != "" {
		st.Tier = req.Tier
	}

	// Return updated license view
	writeJSON(w, http.StatusOK, licenseView(st))
}