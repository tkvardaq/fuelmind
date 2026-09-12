package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/cloudctl"
	"github.com/fuelmind/fuelmind/internal/config"
	"github.com/fuelmind/fuelmind/internal/hardware"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
	"github.com/fuelmind/fuelmind/internal/logging"
	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/normalizer"
	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/sync"
	"github.com/fuelmind/fuelmind/internal/web"
)

// version is overridden at build time via -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	rebuildMart := flag.Bool("rebuild-mart", false, "rebuild all business mart tables from transaction history and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("fuelmind-core %s\n", version)
		return
	}
	if err := run(*rebuildMart); err != nil {
		fmt.Fprintf(os.Stderr, "fuelmind: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(rebuildMartOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	// Detect hardware tier for LLM sizing (Phase 7)
	hwTier := hardware.DetectHardwareTier()
	cfg.HardwareTier = hwTier
	logger.Info("hardware tier detected", slog.String("tier", hwTier))

	logger.Info("fuelmind starting",
		slog.String("version", version),
		slog.String("config_path", cfg.Path),
		slog.String("data_dir", cfg.DataDir),
		slog.String("log_level", cfg.LogLevel),
		slog.String("hardware_tier", hwTier),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Open the local SQLite database with WAL + foreign keys.
	store, err := storage.Open(cfg.Path)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("storage ready", slog.String("path", cfg.Path))

	// Persist the database file path so DiskFreeGB can report free space.
	if _, err := store.GetLocalConfig(ctx, "db_file_path"); err != nil {
		if err == storage.ErrNotFound {
			if err := store.SetLocalConfig(ctx, "db_file_path", cfg.Path, "", false); err != nil {
				logger.Warn("could not persist db_file_path to local_config", "err", err)
			}
		} else {
			logger.Warn("could not check db_file_path in local_config", "err", err)
		}
	}

	// Persist the detected hardware tier into local_config so the
	// rest of the system (sync, update agent, future LLM) can read
	// the same value the dashboard / heartbeat report.
	if err := store.SetLocalConfig(ctx, ident.ConfigKeyHardwareTier, hwTier, "", false); err != nil {
		logger.Warn("could not persist hardware_tier to local_config", "err", err)
	}

	// Resolve and create the POS drop folder.
	posDrop := filepath.Join(cfg.DataDir, "pos_drop")
	if err := os.MkdirAll(posDrop, 0o755); err != nil {
		return fmt.Errorf("mkdir pos_drop: %w", err)
	}

	// Verify the csv_watch adapter is registered (blank import in
	// csvwatch's init() registers it). This is an early-fail check
	// against an accidental refactor that breaks the registration.
	if _, err := posadapter.New(csvwatch.AdapterName); err != nil {
		return fmt.Errorf("csv_watch adapter: %w", err)
	}

	// Build the normalizer (Phase 2): converts raw → canonical
	// transactions. This is what the dashboard (Phase 4) and the
	// data mart (Phase 3) will read from.
	norm, err := normalizer.New(store, logger)
	if err != nil {
		return fmt.Errorf("normalizer: %w", err)
	}

	// Build the data mart (Phase 3): pre-aggregates the normalized
	// transactions into query-ready tables (daily_sales, fuel_margin,
	// credit_outstanding, station_health_score).
	m := mart.New(store, logger)

	if rebuildMartOnly {
		logger.Info("rebuilding all business mart tables from transaction history...")
		if err := m.MaterializeAll(ctx); err != nil {
			return fmt.Errorf("rebuild mart: %w", err)
		}
		logger.Info("business mart rebuild completed successfully")
		return nil
	}

	// Nightly database backup agent (WAL checkpoint + rotation to data_dir/backups).
	backupAgent := backup.NewAgent(backup.Config{
		DataDir: cfg.DataDir,
		Logger:  logger,
	})
	backupAgent.Start(ctx)
	logger.Info("backup agent started")

	// Start the actual file watcher. This is what does the real work
	// for v1: watches the folder, parses CSVs, writes to raw layer,
	// runs the normalizer, materializes the mart, archives the file.
	watcher, err := csvwatch.NewWatcher(store, norm, m, logger)
	if err != nil {
		return fmt.Errorf("csvwatch.NewWatcher: %w", err)
	}
	defer watcher.Close()

	// 15-minute periodic mart refresh (spec §3.3: "materialization job
	// runs after every ingestion batch or on a schedule, e.g. every
	// 15 min"). Cheap because the UPSERT is bounded.
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -7)); err != nil {
					logger.Warn("periodic mart refresh", "err", err)
				}
			}
		}
	}()
	logger.Info("mart ticker started", "interval", "15m")

	// Phase 5: per-station identity + sync agent. Generate (or
	// re-load) the station_id + api_key on every boot, then start
	// the heartbeat loop if FUELMIND_CLOUD_URL is set. If unset,
	// the local core runs in local-only mode — no network at all.
	identity, err := loadOrCreateIdentity(ctx, store, logger)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	logger.Info("station identity",
		slog.String("station_id", identity.StationID.String()),
		slog.String("api_key", ident.MaskAPIKey(identity.APIKey)),
	)

	// Check for a rollback record from a previous update attempt.
	if rec, ok, err := launcher.ReadRollbackRecord(cfg.DataDir); ok && err == nil {
		// Store it in local_config so the sync agent can include it in the next heartbeat.
		data, err := json.Marshal(rec)
		if err == nil {
			if err := store.SetLocalConfig(ctx, "last_rollback", string(data), "", false); err == nil {
				logger.Info("update agent: found rollback record from previous attempt",
					"version", rec.FailedVersion,
					"reason", rec.Reason,
				)
			}
		}
		// Delete the rollback record file so we don't process it again.
		if err := launcher.DeleteRollbackRecord(cfg.DataDir); err != nil {
			logger.Warn("update agent: failed to delete rollback record", "err", err)
		}
	}

	if cfg.SyncEnabled {
		client := sync.NewHTTPClient(cfg.CloudURL)
		agent := sync.NewAgent(sync.AgentConfig{
			Store:           store,
			Client:          client,
			Logger:          logger,
			Cycle:           cfg.SyncCycle,
			Identity:        identity,
			SoftwareVersion: version,
		})
		agent.Start(ctx)
		logger.Info("sync agent started",
			slog.String("cloud_url", cfg.CloudURL),
			slog.Duration("cycle", cfg.SyncCycle),
		)
	} else {
		logger.Info("sync disabled (FUELMIND_CLOUD_URL unset; local-only mode)")
	}

	// Phase 6: Unattended update mechanism. Periodically check for updates
	// via /v1/updates/check, download to staging folder, verify checksum,
	// stage the update, and request a handoff to the launcher for the actual swap.
	if cfg.SyncEnabled {
		updateAgent := newUpdateAgent(store, logger, identity, version, cfg.CloudURL, cfg.DataDir)
		go updateAgent.Start(ctx)
		logger.Info("update agent started")
	}

	// Phase 4: local dashboard. Listens on 127.0.0.1:<port> and
	// serves PIN-protected HTML over the LAN. The auth service
	// checks if a PIN is set; on first run, the dashboard's /setup
	// page prompts the owner to create one.
	a := auth.New(store)
	srv, err := web.New(store, a, logger, cfg.Port)
	if err != nil {
		return fmt.Errorf("web.New: %w", err)
	}
	// Start the POS watcher BEFORE the dashboard blocks, so CSV
	// drops are picked up immediately on boot instead of only after
	// a restart. The watcher is the only path that writes to
	// raw_pos_transactions; without it the mart stays empty and
	// every dashboard number reads zero.
	go func() {
		if err := watcher.Start(ctx, posDrop); err != nil {
			logger.Error("pos watcher stopped", "err", err)
		}
	}()
	logger.Info("pos watcher started", slog.String("folder", posDrop))

	if err := srv.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}

	// Block until shutdown.
	<-ctx.Done()
	logger.Info("fuelmind shutting down", slog.String("reason", ctx.Err().Error()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	<-shutdownCtx.Done()
	if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
		logger.Warn("shutdown timed out, exiting anyway")
	}
	return nil
}

// loadOrCreateIdentity returns the install's station_id + api_key,
// generating fresh ones on first run and persisting both into
// local_config. Subsequent boots read the persisted values back.
//
// Errors here are treated as fatal (the install can't talk to the
// cloud at all without these); we surface them with full context
// rather than falling back to a transient in-memory identity.
func loadOrCreateIdentity(ctx context.Context, store *storage.Storage, logger *slog.Logger) (ident.Identity, error) {
	sidRow, sidErr := store.GetLocalConfig(ctx, ident.ConfigKeyStationID)
	keyRow, keyErr := store.GetLocalConfig(ctx, ident.ConfigKeyAPIKey)
	if sidErr == nil && keyErr == nil && sidRow.Value != "" && keyRow.Value != "" {
		// Persisted — validate the station_id shape (defensive
		// against a hand-edited DB).
		if ident.IsValidStationID(sidRow.Value) {
			return ident.Identity{
				StationID: ident.StationID(sidRow.Value),
				APIKey:    ident.APIKey(keyRow.Value),
			}, nil
		}
		logger.Warn("persisted station_id is malformed, regenerating",
			slog.String("old_value", sidRow.Value),
		)
	}
	id, err := ident.GenerateIdentity()
	if err != nil {
		return ident.Identity{}, fmt.Errorf("generate: %w", err)
	}
	if err := store.SetLocalConfig(ctx, ident.ConfigKeyStationID, id.StationID.String(), "", false); err != nil {
		return ident.Identity{}, fmt.Errorf("persist station_id: %w", err)
	}
	if err := store.SetLocalConfig(ctx, ident.ConfigKeyAPIKey, id.APIKey.String(), "", false); err != nil {
		return ident.Identity{}, fmt.Errorf("persist api_key: %w", err)
	}
	logger.Info("generated new station identity")
	return id, nil
}

// newUpdateAgent creates an unattended update agent that periodically
// checks /v1/updates/check, downloads available updates, verifies
// SHA-256 checksums, stages the update, and requests a handoff to the
// launcher for the actual swap.
func newUpdateAgent(store *storage.Storage, logger *slog.Logger, identity ident.Identity, softwareVersion, cloudURL string, baseDir string) *updateAgent {
	stagingDir := filepath.Join(baseDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		logger.Warn("update agent: could not create staging dir", "err", err)
	}
	return &updateAgent{
		store:           store,
		logger:          logger,
		identity:        identity,
		softwareVersion: softwareVersion,
		cloudURL:        cloudURL,
		baseDir:         baseDir,
		stagingDir:      stagingDir,
		checkInterval:   30 * time.Minute,
		rollbackTimeout: 60 * time.Second,
	}
}

// updateAgent handles the unattended update flow (core side).
type updateAgent struct {
	store           *storage.Storage
	logger          *slog.Logger
	identity        ident.Identity
	softwareVersion string
	cloudURL        string
	baseDir         string
	stagingDir      string
	checkInterval   time.Duration
	rollbackTimeout time.Duration
}

// Start launches the update check loop.
func (u *updateAgent) Start(ctx context.Context) {
	go u.run(ctx)
}

func (u *updateAgent) run(ctx context.Context) {
	// First check: a small random delay so stations don't thunder.
	firstDelay := time.Duration(rand.IntN(60)) * time.Second
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			u.logger.Info("update agent stopped", "reason", ctx.Err().Error())
			return
		case <-timer.C:
			u.tick(ctx)
			nextInterval := u.checkInterval
			u.logger.Debug("update agent next check", "interval", nextInterval)
			timer.Reset(nextInterval)
		}
	}
}

func (u *updateAgent) tick(ctx context.Context) {
	u.logger.Info("update agent: checking for updates")
	checkURL := u.cloudURL + "/v1/updates/check?" + u.checkQueryParams()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, checkURL, nil)
	if err != nil {
		u.logger.Warn("update agent: failed to create request", "err", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		u.logger.Warn("update agent: check request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		u.logger.Info("update agent: no update available")
		return
	}
	if resp.StatusCode != http.StatusOK {
		u.logger.Warn("update agent: unexpected check response", "status", resp.StatusCode)
		return
	}

	var release cloudctl.UpdateRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		u.logger.Warn("update agent: failed to decode update manifest", "err", err)
		return
	}
	u.logger.Info("update agent: update available",
		"version", release.Version,
		"rollout_pct", release.RolloutPct,
	)

	shouldReceive := u.shouldReceiveUpdate(release.RolloutPct)
	if !shouldReceive {
		u.logger.Info("update agent: station not in rollout subset")
		return
	}

	artifactPath, err := u.downloadResumable(ctx, release)
	if err != nil {
		u.logger.Warn("update agent: download failed", "err", err)
		return
	}

	if !u.verifyChecksum(artifactPath, release.ChecksumSHA256) {
		u.logger.Warn("update agent: checksum mismatch, discarding artifact")
		os.Remove(artifactPath)
		return
	}
	u.logger.Info("update agent: checksum verified")

	if err := u.stageUpdate(release.Version, artifactPath); err != nil {
		u.logger.Warn("update agent: staging failed", "err", err)
		return
	}

	u.requestHandoff()
}

// checkQueryParams returns the query parameters for the /v1/updates/check endpoint.
func (u *updateAgent) checkQueryParams() string {
	hwTier := "basic"
	if row, err := u.store.GetLocalConfig(context.Background(), ident.ConfigKeyHardwareTier); err == nil {
		hwTier = row.Value
	}
	v := url.Values{}
	v.Set("station_id", u.identity.StationID.String())
	v.Set("version", u.softwareVersion)
	v.Set("tier", hwTier)
	return v.Encode()
}

// shouldReceiveUpdate determines if this station should receive the update
// based on rollout_pct. For simplicity, we always accept rollout_pct=100.
func (u *updateAgent) shouldReceiveUpdate(rolloutPct int) bool {
	if rolloutPct >= 100 {
		return true
	}
	hash := u.stationHash()
	return hash%100 < rolloutPct
}

// stationHash returns a deterministic hash based on station_id.
func (u *updateAgent) stationHash() int {
	h := sha256.Sum256([]byte(u.identity.StationID.String()))
	return int(h[0])
}

// downloadResumable downloads the artifact from the manifest URL to a
// temporary file in the staging directory, supporting range requests for
// interrupted downloads.
func (u *updateAgent) downloadResumable(ctx context.Context, release cloudctl.UpdateRelease) (string, error) {
	artifactPath := filepath.Join(u.stagingDir, filepath.Base(release.ArtifactURL))
	partialPath := artifactPath + ".partial"

	f, err := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size()

	var req *http.Request
	if start > 0 {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, release.ArtifactURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, release.ArtifactURL, nil)
		if err != nil {
			return "", err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		if _, err := io.Copy(f, resp.Body); err != nil {
			return "", err
		}
		if start > 0 && resp.StatusCode == http.StatusOK {
			if err := f.Truncate(0); err != nil {
				return "", err
			}
			if err := f.Close(); err != nil {
				return "", err
			}
			f, err = os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			start = 0
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, release.ArtifactURL, nil)
			if err != nil {
				return "", err
			}
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			if _, err := io.Copy(f, resp.Body); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(partialPath, artifactPath); err != nil {
		return "", err
	}
	return artifactPath, nil
}

// verifyChecksum computes the SHA-256 of the file and compares it to the
// expected checksum.
func (u *updateAgent) verifyChecksum(filePath, expectedHash string) bool {
	actualHash, err := computeSHA256(filePath)
	if err != nil {
		u.logger.Warn("update agent: failed to compute checksum", "err", err)
		return false
	}
	return actualHash == expectedHash
}

// stageUpdate extracts the verified artifact into versions/<version>/ and
// writes pending.json. It does NOT restart anything itself.
func (u *updateAgent) stageUpdate(version, artifactPath string) error {
	tmpDir := filepath.Join(u.baseDir, "versions", ".tmp-"+version)
	finalDir := filepath.Join(u.baseDir, "versions", version)

	if err := extractZip(artifactPath, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	return launcher.WritePendingVersion(u.baseDir, version)
}

// requestHandoff exits with the code the launcher watches for. The
// launcher's job object and the OS close-on-exit handle any in-flight
// file handles; SQLite WAL means a mid-write shutdown rolls back to a
// consistent state on the next boot.
func (u *updateAgent) requestHandoff() {
	u.logger.Info("update agent: requesting handoff to launcher")
	os.Exit(launcher.ExitCodeRequestRestart)
}

// extractZip extracts a zip file to the destination directory.
// v1 placeholder: copies the artifact as a single binary.
func extractZip(src, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return copyFile(src, filepath.Join(dest, "FuelMindCore.exe"))
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

// computeSHA256 computes the SHA-256 hex hash of a file.
func computeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
