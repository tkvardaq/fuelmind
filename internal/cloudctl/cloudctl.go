// Package cloudctl is a small in-memory control plane for the station
// fleet. It is what the local core talks to in tests and in local
// development (cmd/fuelmind-devcloud), and it is the reference shape for
// the production service:
//
//   - POST /v1/heartbeat            spec §6.3 payload in, licence block out
//   - GET  /v1/license/{station_id} licence block for one station
//   - GET  /v1/updates/check        update manifest, or 204 when up to date
//   - PUT  /v1/admin/stations/{id}/license   flip tier / feature flags
//
// Every endpoint authenticates: stations use their API key as a bearer
// token, the admin endpoint uses the server's admin token (when no admin
// token is configured the admin endpoint is disabled).
//
// State is in memory; the server is disposable.
package cloudctl

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/fuelmind/fuelmind/internal/version"
)

// Station is one row of the in-memory stations table.
type Station struct {
	StationID    string
	APIKey       string
	Tier         string         // "private" | "connected" | "connected_hq"
	FeaturesJSON map[string]any // feature flags per spec §6.2
	Status       string         // "active" | "inactive" | "suspended"
	ValidFrom    time.Time
	ValidUntil   *time.Time
}

// UpdateRelease is the manifest returned by /v1/updates/check.
type UpdateRelease struct {
	Version         string `json:"version"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	ArtifactURL     string `json:"artifact_url"`
	ChecksumSHA256  string `json:"checksum_sha256"`
	MinHardwareTier string `json:"min_hardware_tier,omitempty"`
	RolloutPct      int    `json:"rollout_pct"`
}

// HeartbeatRecord is one row of the in-memory heartbeats table.
type HeartbeatRecord struct {
	ReceivedAt time.Time
	StationID  string
	Payload    map[string]any
}

// Server is the in-memory control plane.
type Server struct {
	mu       sync.Mutex
	stations map[string]*Station // station_id -> station
	keys     map[string]string   // api_key -> station_id
	beats    []HeartbeatRecord
	maxBeats int
	releases []UpdateRelease

	// AdminToken guards /v1/admin/**. Empty disables the admin API.
	AdminToken string
}

// New returns a fresh Server with no stations preloaded.
func New() *Server {
	return &Server{stations: map[string]*Station{}, keys: map[string]string{}}
}

// NewWithStations returns a server preloaded with the given stations.
func NewWithStations(stations ...*Station) *Server {
	s := New()
	for _, st := range stations {
		s.AddStation(st)
	}
	return s
}

// AddStation registers (or replaces) a station.
func (s *Server) AddStation(st *Station) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stations[st.StationID] = st
	s.keys[st.APIKey] = st.StationID
}

// AddRelease publishes an update manifest.
func (s *Server) AddRelease(r UpdateRelease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, r)
}

// SetHeartbeatCap caps retained heartbeat records (0 = unlimited).
func (s *Server) SetHeartbeatCap(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxBeats = n
	if n > 0 && len(s.beats) > n {
		s.beats = s.beats[len(s.beats)-n:]
	}
}

// Heartbeats returns a snapshot of received heartbeats, oldest first.
func (s *Server) Heartbeats() []HeartbeatRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HeartbeatRecord, len(s.beats))
	copy(out, s.beats)
	return out
}

// LicenseFor returns one station, or ErrStationNotFound.
func (s *Server) LicenseFor(stationID string) (*Station, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stations[stationID]
	if !ok {
		return nil, ErrStationNotFound
	}
	return st, nil
}

// ErrStationNotFound mirrors the cloud's 404.
var ErrStationNotFound = errors.New("cloudctl: station not found")

// Routes returns an http.Handler serving the API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/license/", s.handleLicense)
	mux.HandleFunc("/v1/updates/check", s.handleUpdatesCheck)
	mux.HandleFunc("/v1/admin/stations/", s.handleAdminStationLicense)
	return mux
}

// Start launches an httptest.Server backed by this control plane.
func (s *Server) Start() *httptest.Server { return httptest.NewServer(s.Routes()) }

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// stationFor resolves the bearer token to an active station. Callers must
// hold s.mu.
func (s *Server) stationFor(w http.ResponseWriter, r *http.Request, claimedID string) (*Station, bool) {
	key, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return nil, false
	}
	id, ok := s.keys[key]
	if !ok {
		http.Error(w, "unknown api key", http.StatusUnauthorized)
		return nil, false
	}
	if claimedID != "" && claimedID != id {
		http.Error(w, "station_id does not match api key", http.StatusForbidden)
		return nil, false
	}
	st, ok := s.stations[id]
	if !ok {
		http.Error(w, "station not found", http.StatusNotFound)
		return nil, false
	}
	if st.Status != "active" {
		http.Error(w, "station "+st.Status, http.StatusForbidden)
		return nil, false
	}
	return st, true
}

// handleHeartbeat accepts POST /v1/heartbeat.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	st, ok := s.stationFor(w, r, stationID)
	if !ok {
		return
	}
	s.beats = append(s.beats, HeartbeatRecord{ReceivedAt: time.Now(), StationID: stationID, Payload: payload})
	if s.maxBeats > 0 && len(s.beats) > s.maxBeats {
		s.beats = s.beats[len(s.beats)-s.maxBeats:]
	}
	writeJSON(w, http.StatusOK, heartbeatResponse{OK: true, License: licenseView(st)})
}

// handleLicense returns GET /v1/license/{station_id}.
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
	st, ok := s.stationFor(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, licenseView(st))
}

// handleUpdatesCheck returns the best release for the calling station, or
// 204 when it is already up to date.
//
// Query: station_id, version (running), tier (licence tier),
// hardware_tier. The bearer token must belong to station_id.
func (s *Server) handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	stationID, current := q.Get("station_id"), q.Get("version")
	if stationID == "" || current == "" {
		http.Error(w, "missing station_id or version", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stationFor(w, r, stationID); !ok {
		return
	}
	best, ok := s.bestRelease(stationID, current, q.Get("hardware_tier"))
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, best)
}

// bestRelease picks the highest release newer than current that this
// station's hardware supports and whose rollout bucket includes it.
func (s *Server) bestRelease(stationID, current, hardwareTier string) (UpdateRelease, bool) {
	var best UpdateRelease
	found := false
	for _, rel := range s.releases {
		if version.Compare(rel.Version, current) <= 0 ||
			!hardwareAtLeast(hardwareTier, rel.MinHardwareTier) ||
			!version.InRollout(stationID, rel.RolloutPct) {
			continue
		}
		if !found || version.Compare(rel.Version, best.Version) > 0 {
			best, found = rel, true
		}
	}
	return best, found
}

var tierOrder = map[string]int{"": 0, "basic": 1, "standard": 2, "enhanced": 3, "pro": 4}

func hardwareAtLeast(have, need string) bool {
	return tierOrder[strings.ToLower(have)] >= tierOrder[strings.ToLower(need)]
}

// handleAdminStationLicense handles
// PUT /v1/admin/stations/{station_id}[/license] with the admin token.
func (s *Server) handleAdminStationLicense(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.adminAuthorized(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/admin/stations/"), "/license")
	id = strings.Trim(id, "/")
	if id == "" {
		http.Error(w, "missing station_id", http.StatusBadRequest)
		return
	}
	var req struct {
		FeaturesJSON map[string]any `json:"features_json"`
		Tier         string         `json:"tier,omitempty"`
		Status       string         `json:"status,omitempty"`
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
	if req.FeaturesJSON != nil {
		st.FeaturesJSON = req.FeaturesJSON
	}
	if req.Tier != "" {
		st.Tier = req.Tier
	}
	if req.Status != "" {
		st.Status = req.Status
	}
	writeJSON(w, http.StatusOK, licenseView(st))
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	if s.AdminToken == "" {
		return false
	}
	tok, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.AdminToken)) == 1
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
	return strings.TrimSpace(h[len(p):]), true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// BuildHeartbeatResponse builds a canned response for tests.
func BuildHeartbeatResponse(st *Station) any {
	return heartbeatResponse{OK: true, License: licenseView(st)}
}

// Describe renders a one-line summary for test failure messages.
func (st *Station) Describe() string {
	return fmt.Sprintf("%s(tier=%s, status=%s)", st.StationID, st.Tier, st.Status)
}
