// Package update is the core side of the unattended update flow (spec §7,
// plan Phase 6):
//
//  1. Ask the cloud GET /v1/updates/check (authenticated with the station
//     API key, sending the licence tier and hardware tier).
//  2. Download the artifact (resumable) into <data>\staging.
//  3. Verify its SHA-256.
//  4. Extract FuelMindCore.exe into <data>\versions\<v>\ and write
//     pending.json.
//  5. Inside the maintenance window (default 02:00–05:00 local) ask the
//     launcher to hand off by exiting with launcher.ExitCodeRequestRestart.
//
// The launcher then swaps versions, runs a /healthz self-check and rolls
// back on failure.
package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/version"
)

// Release is the manifest returned by /v1/updates/check.
type Release struct {
	Version         string `json:"version"`
	ReleaseNotes    string `json:"release_notes,omitempty"`
	ArtifactURL     string `json:"artifact_url"`
	ChecksumSHA256  string `json:"checksum_sha256"`
	MinHardwareTier string `json:"min_hardware_tier,omitempty"`
	RolloutPct      int    `json:"rollout_pct"`
}

// State is persisted in local_config ("update_state") for support.
type State struct {
	Version string    `json:"version"`
	Status  string    `json:"status"` // downloading | staged | handoff | failed
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// ConfigKeyState is the local_config key holding the last State.
const ConfigKeyState = "update_state"

// Window decides whether an update may be applied at t.
type Window func(t time.Time) bool

// NightWindow allows handoff between 02:00 and 05:00 local time.
func NightWindow(t time.Time) bool { h := t.Hour(); return h >= 2 && h < 5 }

// AnyTime allows handoff immediately.
func AnyTime(time.Time) bool { return true }

// Config wires an Agent.
type Config struct {
	Store          *storage.Storage
	Logger         *slog.Logger
	Identity       ident.Identity
	Version        string // running core version
	CloudURL       string
	BaseDir        string        // data dir shared with the launcher
	Interval       time.Duration // default 30m
	Supervised     bool          // running under the launcher (handoff possible)
	Window         Window        // default NightWindow
	RequestRestart func()        // asks main to exit with ExitCodeRequestRestart
	HTTPClient     *http.Client
	LicenseTier    func(ctx context.Context) string
	HardwareTier   string
}

// Agent runs the update loop.
type Agent struct{ cfg Config }

// New builds an Agent with defaults applied.
func New(cfg Config) *Agent {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Minute
	}
	if cfg.Window == nil {
		cfg.Window = NightWindow
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.LicenseTier == nil {
		cfg.LicenseTier = func(context.Context) string { return "private" }
	}
	cfg.CloudURL = strings.TrimRight(cfg.CloudURL, "/")
	return &Agent{cfg: cfg}
}

// Run checks for updates until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	timer := time.NewTimer(time.Duration(rand.IntN(60)) * time.Second)
	defer timer.Stop()
	// A staged update is re-checked every few minutes so it is applied
	// promptly once the maintenance window opens.
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := a.cfg.Interval
			if staged, err := a.Tick(ctx); err != nil {
				a.cfg.Logger.Warn("update check failed", "err", err)
			} else if staged && next > 5*time.Minute {
				next = 5 * time.Minute
			}
			timer.Reset(next)
		}
	}
}

// Tick runs one check/download/stage/handoff pass. It returns true when a
// staged update is waiting for the maintenance window.
func (a *Agent) Tick(ctx context.Context) (staged bool, err error) {
	if v, ok := a.stagedVersion(); ok {
		return a.maybeHandoff(ctx, v), nil
	}
	rel, err := a.check(ctx)
	if err != nil || rel == nil {
		return false, err
	}
	a.cfg.Logger.Info("update available", "version", rel.Version, "rollout_pct", rel.RolloutPct)
	if err := a.stage(ctx, *rel); err != nil {
		a.saveState(ctx, State{Version: rel.Version, Status: "failed", Error: err.Error()})
		return false, err
	}
	a.saveState(ctx, State{Version: rel.Version, Status: "staged"})
	return a.maybeHandoff(ctx, rel.Version), nil
}

func (a *Agent) stagedVersion() (string, bool) {
	v, ok, err := launcher.ReadPending(a.cfg.BaseDir)
	if err != nil || !ok {
		return "", false
	}
	if _, err := os.Stat(launcher.CoreBinaryPath(a.cfg.BaseDir, v)); err != nil {
		return "", false
	}
	return v, true
}

// maybeHandoff requests the restart when allowed; returns true if the
// update is still waiting.
func (a *Agent) maybeHandoff(ctx context.Context, v string) bool {
	switch {
	case !a.cfg.Supervised:
		a.cfg.Logger.Info("update staged; restart the FuelMind service to apply it", "version", v)
		return true
	case !a.cfg.Window(time.Now()):
		a.cfg.Logger.Debug("update staged; waiting for the maintenance window", "version", v)
		return true
	}
	a.cfg.Logger.Info("applying update: handing off to the launcher", "version", v)
	a.saveState(ctx, State{Version: v, Status: "handoff"})
	if a.cfg.RequestRestart != nil {
		a.cfg.RequestRestart()
	}
	return false
}

// check asks the cloud for a newer release. nil, nil = no update.
func (a *Agent) check(ctx context.Context) (*Release, error) {
	q := url.Values{}
	q.Set("station_id", a.cfg.Identity.StationID.String())
	q.Set("version", a.cfg.Version)
	q.Set("tier", a.cfg.LicenseTier(ctx))
	q.Set("hardware_tier", a.cfg.HardwareTier)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, a.cfg.CloudURL+"/v1/updates/check?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Identity.APIKey.String())
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update check: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("update check: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update check: bad manifest: %w", err)
	}
	switch {
	case !version.Valid(rel.Version):
		return nil, fmt.Errorf("update check: invalid version %q in manifest", rel.Version)
	case version.Compare(rel.Version, a.cfg.Version) <= 0:
		return nil, nil
	case launcher.IsVersionQuarantined(a.cfg.BaseDir, rel.Version):
		// This version already failed here and was rolled back; do
		// not install it again (support clears the quarantine by
		// deleting data\failed_versions.json).
		a.cfg.Logger.Warn("skipping a release that previously failed on this station", "version", rel.Version)
		return nil, nil
	case !version.InRollout(a.cfg.Identity.StationID.String(), rel.RolloutPct):
		a.cfg.Logger.Info("update not yet rolled out to this station", "version", rel.Version, "rollout_pct", rel.RolloutPct)
		return nil, nil
	case len(rel.ChecksumSHA256) != 64:
		return nil, fmt.Errorf("update check: manifest has no valid sha256")
	}
	return &rel, nil
}

func (a *Agent) stage(ctx context.Context, rel Release) error {
	artifactURL, err := url.Parse(rel.ArtifactURL)
	if err != nil {
		return fmt.Errorf("bad artifact url: %w", err)
	}
	if !artifactURL.IsAbs() {
		base, _ := url.Parse(a.cfg.CloudURL + "/")
		artifactURL = base.ResolveReference(artifactURL)
	}
	stagingDir := filepath.Join(a.cfg.BaseDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	artifact := filepath.Join(stagingDir, "fuelmind-core-"+rel.Version+".zip")
	a.saveState(ctx, State{Version: rel.Version, Status: "downloading"})
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := downloadWithRestart(dctx, a.cfg.HTTPClient, artifactURL.String(), artifact); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	sum, err := fileSHA256(artifact)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, strings.TrimSpace(rel.ChecksumSHA256)) {
		_ = os.Remove(artifact)
		return fmt.Errorf("checksum mismatch: got %s, manifest says %s", sum, rel.ChecksumSHA256)
	}
	if err := Stage(a.cfg.BaseDir, rel.Version, artifact); err != nil {
		return err
	}
	_ = os.Remove(artifact)
	a.cfg.Logger.Info("update verified and staged", "version", rel.Version)
	return nil
}

// Stage extracts FuelMindCore.exe from the artifact zip into
// versions\<v>\ and writes pending.json. Re-staging a version replaces
// the earlier copy unless it is the version currently running.
func Stage(baseDir, v, artifact string) error {
	if !version.Valid(v) {
		return fmt.Errorf("invalid version %q", v)
	}
	if cur, _ := launcher.ReadCurrentVersion(baseDir); cur == v {
		return fmt.Errorf("version %s is already running", v)
	}
	tmp := filepath.Join(launcher.VersionsDir(baseDir), ".tmp-"+v)
	final := filepath.Join(launcher.VersionsDir(baseDir), v)
	_ = os.RemoveAll(tmp)
	if err := extractCore(artifact, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.RemoveAll(final); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return launcher.WritePendingVersion(baseDir, v)
}

// extractCore unpacks the artifact zip into dir. It must contain
// FuelMindCore.exe at its root; other files (manifest.json, notes) are
// extracted alongside. Entries that would escape dir are rejected.
func extractCore(artifact, dir string) error {
	zr, err := zip.OpenReader(artifact)
	if err != nil {
		return fmt.Errorf("artifact is not a zip: %w", err)
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	found := false
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, `\`, "/"))
		if f.FileInfo().IsDir() {
			continue
		}
		if path.IsAbs(name) || strings.HasPrefix(name, "../") || name == ".." || strings.Contains(name, ":") {
			return fmt.Errorf("artifact entry %q escapes the version directory", f.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, dst); err != nil {
			return err
		}
		if name == launcher.CoreExeName {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("artifact does not contain %s", launcher.CoreExeName)
	}
	return nil
}

func writeZipEntry(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, 512<<20)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// downloadWithRestart runs Download and honours errRangeRestart: when the
// server rejects the resume range (the artifact was replaced while a
// partial file was on disk), Download has already deleted the stale
// partial, so a single clean retry succeeds. Without this the update
// waited for the next check cycle to make progress it could make now.
func downloadWithRestart(ctx context.Context, client *http.Client, rawURL, dst string) error {
	err := Download(ctx, client, rawURL, dst)
	if errors.Is(err, errRangeRestart) {
		return Download(ctx, client, rawURL, dst)
	}
	return err
}

// errRangeRestart asks the caller to retry the download from scratch.
var errRangeRestart = errors.New("server rejected the resume range; restarting download")

// Download fetches url into dst, resuming a previous dst+".partial".
func Download(ctx context.Context, client *http.Client, rawURL, dst string) error {
	partial := dst + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Append to what we already have.
	case http.StatusOK:
		// Server ignored Range: start over with this same response.
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	case http.StatusRequestedRangeNotSatisfiable:
		_ = f.Close()
		_ = os.Remove(partial)
		return errRangeRestart
	default:
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err // partial file kept for the next resume
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Rename(partial, dst)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a *Agent) saveState(ctx context.Context, s State) {
	if a.cfg.Store == nil {
		return
	}
	s.At = time.Now().UTC()
	b, _ := json.Marshal(s)
	if err := a.cfg.Store.SetLocalConfig(ctx, ConfigKeyState, s.Status, string(b), false); err != nil {
		a.cfg.Logger.Warn("save update state", "err", err)
	}
}
