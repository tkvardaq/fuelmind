// Package sync runs the heartbeat / license-check loop that the
// local core uses to talk to the cloud control plane.
//
// Phase 5 of the build plan (spec §6). The local core is the
// authoritative source of station data — the cloud only sees a
// thin heartbeat (§6.3, no financial detail) and ships back
// license status + (later, Phase 6) the update manifest.
//
// Hard requirement (spec §6 + Phase 5 demo): the cloud is optional.
// If the cloud is unreachable, the local core must:
//  1. Log the failure.
//  2. Cache the last-seen license status (so feature flags are
//     still readable offline).
//  3. Retry on the next cycle.
//
// Nothing in the local core's day-to-day work may depend on the
// cloud being up.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// HeartbeatPath is the cloud endpoint for POSTing heartbeats.
const HeartbeatPath = "/v1/heartbeat"

// LicensePath is the cloud endpoint for GETting a station's
// current license tier + features_json.
const LicensePath = "/v1/license/"

// DefaultCycle is the default heartbeat interval. 30 minutes matches
// the spec's "every 15-60 min" guidance and the build plan's
// "30 min" default.
const DefaultCycle = 30 * time.Minute

// jitterFraction is the ±fraction of the cycle that the cycle is
// randomly stretched. With 30-min cycle and 0.2 jitter the effective
// range is 24-36 min, which prevents a thundering herd of stations
// synchronising after a regional outage (all retrying exactly on
// the dot).
const jitterFraction = 0.2

// HTTPClientTimeout is the per-request timeout. A heartbeat payload
// is ~1KB so this is generous; the only thing that takes real time
// is a slow TLS handshake to a degraded cloud.
const HTTPClientTimeout = 10 * time.Second

// LastRollbackInfo carries the version / reason / timestamp of a
// previously failed update attempt, so the cloud can surface it in
// fleet-health views.
type LastRollbackInfo struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
	At      string `json:"at"` // RFC 3339
}

// HeartbeatPayload is the spec §6.3 payload the local core sends. No
// financial detail. The health fields are only sent when the owner
// opted in at setup (TelemetryConsent); otherwise the heartbeat is the
// bare licensing check: station id, software version, timestamp.
type HeartbeatPayload struct {
	StationID          string            `json:"station_id"`
	SoftwareVersion    string            `json:"software_version"`
	Timestamp          time.Time         `json:"timestamp"`
	TelemetryConsent   bool              `json:"telemetry_consent"`
	LastRollback       *LastRollbackInfo `json:"last_rollback,omitempty"`
	HardwareTier       string            `json:"hardware_tier"`
	DatabaseSizeMB     int64             `json:"database_size_mb"`
	DataHealthScore    int               `json:"data_health_score"`
	LastPosIngestionAt time.Time         `json:"last_pos_ingestion_at"`
	ActiveAlertsCount  int               `json:"active_alerts_count"`
	DiskFreeGB         int64             `json:"disk_free_gb"`
	AppUptimeHours     int64             `json:"app_uptime_hours"`
	ErrorsLast24h      int               `json:"errors_last_24h"`
}

// MarshalJSON drops every health field when telemetry consent is off.
func (p HeartbeatPayload) MarshalJSON() ([]byte, error) {
	type full HeartbeatPayload
	if p.TelemetryConsent {
		return json.Marshal(full(p))
	}
	return json.Marshal(struct {
		StationID        string            `json:"station_id"`
		SoftwareVersion  string            `json:"software_version"`
		Timestamp        time.Time         `json:"timestamp"`
		TelemetryConsent bool              `json:"telemetry_consent"`
		LastRollback     *LastRollbackInfo `json:"last_rollback,omitempty"`
	}{p.StationID, p.SoftwareVersion, p.Timestamp, false, p.LastRollback})
}

// ConfigKeyTelemetryConsent is the local_config key written by /setup.
const ConfigKeyTelemetryConsent = "telemetry_consent"

// ConfigKeyLastRollback holds a rollback record waiting to be reported.
const ConfigKeyLastRollback = "last_rollback"

// LicenseTier returns the cached license tier ("private" when unknown).
func LicenseTier(ctx context.Context, store *storage.Storage) string {
	return store.LocalConfigValue(ctx, ident.ConfigKeyLicenseCache, "private")
}

// LicenseStatus is what the cloud returns from /v1/license/{station_id}
// AND what we persist in local_config under ConfigKeyLicenseCache
// (so the dashboard's feature-flag UI works offline).
type LicenseStatus struct {
	StationID    string         `json:"station_id"`
	Tier         string         `json:"tier"`          // "private" / "connected" / "connected_hq"
	FeaturesJSON map[string]any `json:"features_json"` // see spec §6.2
	ValidFrom    time.Time      `json:"valid_from"`
	ValidUntil   *time.Time     `json:"valid_until,omitempty"`
	Status       string         `json:"status"` // "active" / "suspended" / "expired"
}

// IsFeatureEnabled is the convenience accessor for template/dashboard
// code: `lic.IsFeatureEnabled("whatsapp_enabled")`.
//
// Returns false for unknown keys (closed-by-default) — a missing
// feature flag is treated as "off" not "on".
func (l *LicenseStatus) IsFeatureEnabled(name string) bool {
	if l == nil || l.FeaturesJSON == nil {
		return false
	}
	v, ok := l.FeaturesJSON[name]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// CloudClient is the HTTP boundary the agent talks to. Defined as
// an interface so tests can stub it with an httptest.Server or a
// pure in-memory fake. The default implementation is httpDoer
// below.
type CloudClient interface {
	// PostHeartbeat returns the license status parsed from the
	// cloud's response, or an error describing the failure mode.
	// Implementations must NOT panic on transport errors; the agent
	// treats every non-nil error as "retry next cycle".
	PostHeartbeat(ctx context.Context, stationID ident.StationID, apiKey ident.APIKey, payload *HeartbeatPayload) (*LicenseStatus, error)
}

// httpDoer is the default CloudClient. Speaks HTTPS to the cloud
// control plane's heartbeat endpoint and parses the license block
// from the response body.
type httpDoer struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient builds the default CloudClient pointed at baseURL
// (e.g. "https://control.fuelmind.app"). The baseURL is stored
// without a trailing slash.
func NewHTTPClient(baseURL string) CloudClient {
	return &httpDoer{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: HTTPClientTimeout},
	}
}

func (c *httpDoer) PostHeartbeat(ctx context.Context, stationID ident.StationID, apiKey ident.APIKey, payload *HeartbeatPayload) (*LicenseStatus, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("sync: marshal heartbeat: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+HeartbeatPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sync: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey.String())
	req.Header.Set("X-FuelMind-Station", stationID.String())

	start := time.Now()
	resp, err := c.http.Do(req)
	roundTripMs := int(time.Since(start) / time.Millisecond)
	if err != nil {
		// Network/DNS/timeout error. Surface a category string so
		// the agent can record it in sync_log without re-parsing.
		return nil, &SyncError{RoundTripMs: roundTripMs, Reason: classifyNetErr(err), Cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 1KB of the body for the sync_log entry —
		// enough to diagnose without filling the disk.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		reason := fmt.Sprintf("http_%dxx", resp.StatusCode/100)
		return nil, &SyncError{
			HTTPStatus:   resp.StatusCode,
			RoundTripMs:  roundTripMs,
			Reason:       reason,
			ResponseBody: string(body),
		}
	}

	var hbResp heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		return nil, &SyncError{
			HTTPStatus:  resp.StatusCode,
			RoundTripMs: roundTripMs,
			Reason:      "parse",
			Cause:       err,
		}
	}
	return &hbResp.License, nil
}

// heartbeatResponse is the JSON shape the cloud returns from
// POST /v1/heartbeat (spec §6.3). The agent only cares about the
// license block; other fields are ignored.
type heartbeatResponse struct {
	OK      bool          `json:"ok"`
	License LicenseStatus `json:"license"`
}

// SyncError is the structured error type the agent recognises.
// The agent categorises failures by Reason (a small bounded
// vocabulary) and never blocks local work because of one.
type SyncError struct {
	HTTPStatus   int
	RoundTripMs  int
	Reason       string // "timeout" / "dns" / "http_4xx" / "http_5xx" / "parse"
	ResponseBody string
	Cause        error
}

func (e *SyncError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("sync: http %d (%s, %dms)", e.HTTPStatus, e.Reason, e.RoundTripMs)
	}
	return fmt.Sprintf("sync: %s (%dms)", e.Reason, e.RoundTripMs)
}

func (e *SyncError) Unwrap() error { return e.Cause }

// classifyNetErr maps a net/http transport error to a short
// category suitable for sync_log.reason. The string matching is
// case-insensitive because Go's stdlib renders "context deadline
// exceeded" with a lowercase 'd' but wraps it as
// *url.Error("Post ...: context deadline exceeded"), and we want
// either form to land in the "timeout" bucket.
func classifyNetErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return "dns"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "connect: "):
		return "conn_refused"
	default:
		return "transport"
	}
}

// Agent is the heartbeat goroutine. One per process. Run it via
// Start(ctx); it returns when ctx is cancelled.
type Agent struct {
	store    *storage.Storage
	client   CloudClient
	logger   *slog.Logger
	cycle    time.Duration
	identity ident.Identity

	// SoftwareVersion is included in every heartbeat. Set by the
	// caller (main wires the build-time version flag here).
	SoftwareVersion string
	// StartedAt is used to compute app_uptime_hours. Set by Start.
	StartedAt time.Time
}

// AgentConfig is what main.go passes to NewAgent. Identity must
// already be persisted in local_config (the storage layer's
// SetLocalConfig is idempotent so it's safe to re-call).
type AgentConfig struct {
	Store           *storage.Storage
	Client          CloudClient
	Logger          *slog.Logger
	Cycle           time.Duration // 0 = DefaultCycle
	Identity        ident.Identity
	SoftwareVersion string
}

// NewAgent builds an Agent. The caller must already have called
// store.Migrate and persisted the Identity to local_config.
func NewAgent(cfg AgentConfig) *Agent {
	cycle := cfg.Cycle
	if cycle <= 0 {
		cycle = DefaultCycle
	}
	return &Agent{
		store:           cfg.Store,
		client:          cfg.Client,
		logger:          cfg.Logger,
		cycle:           cycle,
		identity:        cfg.Identity,
		SoftwareVersion: cfg.SoftwareVersion,
	}
}

// Start launches the heartbeat loop in a goroutine. The first
// heartbeat fires almost immediately (with a small random delay
// so two stations restarting in the same minute don't thunder),
// then on every cycle (cycle ± jitterFraction) until ctx is done.
//
// Every attempt — success or failure — is recorded in sync_log.
// The local_config license_cache is updated on every successful
// heartbeat. On failure the existing license_cache is left alone
// (the dashboard keeps showing the last-known flags).
func (a *Agent) Start(ctx context.Context) {
	a.StartedAt = time.Now()
	go a.run(ctx)
}

func (a *Agent) run(ctx context.Context) {
	// First heartbeat: a small random delay so a fresh restart
	// cluster doesn't thunder. Range 0..30s.
	firstDelay := time.Duration(rand.IntN(30)) * time.Second
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("sync agent stopped", "reason", ctx.Err().Error())
			return
		case <-timer.C:
			a.tick(ctx)
			next := jitter(a.cycle, jitterFraction)
			timer.Reset(next)
		}
	}
}

// jitter returns d ± fraction*d, with the sign randomised.
func jitter(d time.Duration, fraction float64) time.Duration {
	if fraction <= 0 || fraction >= 1 {
		return d
	}
	span := time.Duration(float64(d) * fraction)
	delta := time.Duration(rand.Int64N(int64(2*span+1))) - span
	out := d + delta
	if out < time.Second {
		out = time.Second
	}
	return out
}

// tick builds the payload, calls the cloud, records the outcome.
// All errors are non-fatal; the loop just moves on to the next tick.
func (a *Agent) tick(ctx context.Context) { _ = a.SendOnce(ctx) }

// SendOnce sends one heartbeat now and records the outcome in sync_log.
// Used by the loop and by `fuelmind-setup test-heartbeat`.
func (a *Agent) SendOnce(ctx context.Context) error {
	if a.StartedAt.IsZero() {
		a.StartedAt = time.Now()
	}
	payload, err := a.buildPayload(ctx)
	if err != nil {
		a.logger.Warn("sync: build payload", "err", err)
		return err
	}

	lic, err := a.client.PostHeartbeat(ctx, a.identity.StationID, a.identity.APIKey, payload)
	if err != nil {
		// Categorise and record. We deliberately do NOT mark the
		// agent's overall state as failed — the contract is that
		// local work proceeds; this is just an audit trail.
		var se *SyncError
		var reason string
		var httpStatus, roundTripMs int
		if errors.As(err, &se) {
			reason = se.Reason
			httpStatus = se.HTTPStatus
			roundTripMs = se.RoundTripMs
		} else {
			reason = "unknown"
		}
		if rerr := a.store.RecordSyncAttempt(ctx, "failed", reason, httpStatus, roundTripMs); rerr != nil {
			a.logger.Warn("sync: record failed attempt", "err", rerr)
		}
		a.logger.Warn("sync: heartbeat failed",
			"reason", reason,
			"http_status", httpStatus,
			"round_trip_ms", roundTripMs,
			"station_id", a.identity.StationID.String(),
		)
		return err
	}

	// Success: persist the license cache + record the attempt.
	if err := a.persistLicense(ctx, lic); err != nil {
		a.logger.Warn("sync: persist license cache", "err", err)
	}
	// A reported rollback has reached the cloud; don't report it again.
	if payload.LastRollback != nil {
		if err := a.store.DeleteLocalConfig(ctx, ConfigKeyLastRollback); err != nil {
			a.logger.Warn("sync: clear reported rollback", "err", err)
		}
	}
	// We don't have round_trip_ms here (it's inside the client's
	// SyncError envelope) — record 0 for success rows, which is
	// the canonical "no timing tracked" value.
	if err := a.store.RecordSyncAttempt(ctx, "ok", "", 0, 0); err != nil {
		a.logger.Warn("sync: record ok attempt", "err", err)
	}
	a.logger.Debug("sync: heartbeat ok",
		"station_id", a.identity.StationID.String(),
		"license_tier", lic.Tier,
	)
	return nil
}

// buildPayload gathers the spec §6.3 fields. Returns an error only
// when the storage layer can't be reached (very rare; the local DB
// is always local).
func (a *Agent) buildPayload(ctx context.Context) (*HeartbeatPayload, error) {
	hwTier := "basic"
	if row, err := a.store.GetLocalConfig(ctx, ident.ConfigKeyHardwareTier); err == nil {
		hwTier = row.Value
	}

	dbSize, _ := a.store.DBSize(ctx)
	diskFree, _ := a.store.DiskFreeGB(ctx)
	lastIngest, _ := a.store.LastPosIngestionAt(ctx)
	alertCount, _ := a.store.ActiveAlertsCount(ctx)
	errCount, _ := a.store.ErrorsLast24h(ctx)

	// A rollback recorded by the launcher waits in local_config until a
	// heartbeat delivers it.
	var lastRollback *LastRollbackInfo
	if raw := a.store.LocalConfigValue(ctx, ConfigKeyLastRollback, ""); raw != "" {
		var rec launcher.RollbackRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			a.logger.Warn("sync: unreadable last_rollback record", "err", err)
		} else {
			lastRollback = &LastRollbackInfo{
				Version: rec.FailedVersion,
				Reason:  rec.Reason,
				At:      rec.At.UTC().Format(time.RFC3339),
			}
		}
	}
	consent := a.store.LocalConfigValue(ctx, ConfigKeyTelemetryConsent, "false") == "true"
	return &HeartbeatPayload{
		StationID:          a.identity.StationID.String(),
		SoftwareVersion:    a.SoftwareVersion,
		HardwareTier:       hwTier,
		Timestamp:          time.Now().UTC(),
		TelemetryConsent:   consent,
		DatabaseSizeMB:     dbSize,
		DataHealthScore:    a.dataHealthScore(ctx),
		LastPosIngestionAt: lastIngest,
		ActiveAlertsCount:  alertCount,
		DiskFreeGB:         diskFree,
		AppUptimeHours:     int64(time.Since(a.StartedAt).Hours()),
		ErrorsLast24h:      errCount,
		LastRollback:       lastRollback,
	}, nil
}

// dataHealthScore returns the most recent FuelMind Score, or 0 if
// none yet. Cheap to read — it's already in the mart.
func (a *Agent) dataHealthScore(ctx context.Context) int {
	s, err := a.store.LatestScore(ctx)
	if err != nil {
		return 0
	}
	return s.Overall
}

// persistLicense caches the cloud-returned license under
// ConfigKeyLicenseCache so the dashboard and other local code can
// read flags without a synchronous network call.
func (a *Agent) persistLicense(ctx context.Context, lic *LicenseStatus) error {
	if lic == nil {
		return nil
	}
	// We cache the whole LicenseStatus as JSON so future fields
	// (valid_until, etc.) round-trip without a schema change.
	body, err := json.Marshal(lic)
	if err != nil {
		return err
	}
	return a.store.SetLocalConfig(ctx, ident.ConfigKeyLicenseCache, lic.Tier, string(body), false)
}
