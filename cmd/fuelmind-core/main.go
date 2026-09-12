// Command fuelmind-core is the FuelMind local core service: it ingests POS
// exports, normalizes them, materializes the data mart, serves the local
// dashboard, and (when a cloud is configured) sends heartbeats and applies
// unattended updates.
//
// It is normally started by fuelmind-launcher, which supervises it and
// performs version swaps. Run directly it behaves the same, minus updates.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/config"
	"github.com/fuelmind/fuelmind/internal/hardware"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/launcher"
	"github.com/fuelmind/fuelmind/internal/llm"
	"github.com/fuelmind/fuelmind/internal/logging"
	"github.com/fuelmind/fuelmind/internal/mart"
	"github.com/fuelmind/fuelmind/internal/normalizer"
	"github.com/fuelmind/fuelmind/internal/posadapter"
	"github.com/fuelmind/fuelmind/internal/posadapter/csvwatch"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/sync"
	"github.com/fuelmind/fuelmind/internal/update"
	"github.com/fuelmind/fuelmind/internal/web"
)

// version is overridden at build time via -ldflags "-X main.version=1.2.3".
var version = "dev"

// refreshInterval is how often the mart is refreshed and the drop folder
// swept, independent of file-system events (spec §3.3).
const refreshInterval = 15 * time.Minute

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	rebuildMart := flag.Bool("rebuild-mart", false, "rebuild all business mart tables from transaction history and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("fuelmind-core %s\n", version)
		return
	}
	code, err := run(*rebuildMart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuelmind: fatal: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(rebuildMartOnly bool) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 1, fmt.Errorf("config: %w", err)
	}
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	hwTier := cfg.HardwareTier
	detected := hardware.DetectHardwareTier()
	if hwTier == "" {
		hwTier = detected
	}
	logger.Info("fuelmind starting",
		slog.String("version", version),
		slog.String("data_dir", cfg.DataDir),
		slog.String("log_level", cfg.LogLevel),
		slog.String("hardware_tier", hwTier),
		slog.Bool("supervised", cfg.Supervised),
	)

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The launcher asks for a graceful stop by closing our stdin.
	if cfg.Supervised {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			logger.Info("launcher asked for shutdown")
			cancel()
		}()
	}

	store, err := storage.Open(cfg.Path)
	if err != nil {
		return 1, fmt.Errorf("storage: %w", err)
	}
	defer store.Close()
	applied, err := store.MigrateCount(ctx)
	if err != nil {
		return 1, fmt.Errorf("migrate: %w", err)
	}
	logger.Info("storage ready", slog.String("path", cfg.Path), slog.Int("migrations_applied", applied))

	if err := store.SetLocalConfig(ctx, ident.ConfigKeyHardwareTier, hwTier, "", false); err != nil {
		logger.Warn("could not persist hardware_tier", "err", err)
	}

	posDrop := filepath.Join(cfg.DataDir, "pos_drop")
	if err := os.MkdirAll(posDrop, 0o755); err != nil {
		return 1, fmt.Errorf("mkdir pos_drop: %w", err)
	}
	// Fail fast if the csv_watch adapter lost its registration.
	if _, err := posadapter.New(csvwatch.AdapterName); err != nil {
		return 1, fmt.Errorf("csv_watch adapter: %w", err)
	}

	norm, err := normalizer.New(store, logger)
	if err != nil {
		return 1, fmt.Errorf("normalizer: %w", err)
	}
	m := mart.New(store, logger)

	if rebuildMartOnly {
		logger.Info("rebuilding the business mart from transaction history")
		if _, _, err := norm.Run(ctx, 0); err != nil {
			return 1, fmt.Errorf("normalize: %w", err)
		}
		if err := m.MaterializeAll(ctx); err != nil {
			return 1, fmt.Errorf("rebuild mart: %w", err)
		}
		logger.Info("business mart rebuild completed")
		return 0, nil
	}

	// Catch up on anything that arrived while the service was down, and
	// rebuild everything when a migration changed the schema.
	if _, errs, err := norm.Run(ctx, 0); err != nil {
		logger.Warn("startup normalize", "err", err)
	} else if errs > 0 {
		logger.Warn("startup normalize: some rows could not be read", "rows", errs)
	}
	if applied > 0 {
		if err := m.MaterializeAll(ctx); err != nil {
			logger.Warn("post-migration mart rebuild", "err", err)
		}
	} else if err := m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -7)); err != nil {
		logger.Warn("startup mart refresh", "err", err)
	}

	backup.NewAgent(backup.Config{DataDir: cfg.DataDir, Store: store, Logger: logger, Hour: 2}).Start(ctx)

	watcher, err := csvwatch.NewWatcher(store, norm, m, logger)
	if err != nil {
		return 1, fmt.Errorf("csvwatch.NewWatcher: %w", err)
	}
	defer watcher.Close()
	go periodicRefresh(ctx, watcher, norm, m, posDrop, logger)

	identity, err := loadOrCreateIdentity(ctx, store, logger)
	if err != nil {
		return 1, fmt.Errorf("identity: %w", err)
	}
	logger.Info("station identity",
		slog.String("station_id", identity.StationID.String()),
		slog.String("api_key", ident.MaskAPIKey(identity.APIKey)),
	)
	takeOverRollbackRecord(ctx, store, cfg.DataDir, logger)

	var restartRequested atomic.Bool
	if cfg.SyncEnabled {
		agent := sync.NewAgent(sync.AgentConfig{
			Store:           store,
			Client:          sync.NewHTTPClient(cfg.CloudURL),
			Logger:          logger,
			Cycle:           cfg.SyncCycle,
			Identity:        identity,
			SoftwareVersion: version,
		})
		agent.Start(ctx)
		logger.Info("sync agent started", slog.String("cloud_url", cfg.CloudURL), slog.Duration("cycle", cfg.SyncCycle))

		window := update.NightWindow
		if cfg.UpdateAnytime {
			window = update.AnyTime
		}
		updater := update.New(update.Config{
			Store:        store,
			Logger:       logger,
			Identity:     identity,
			Version:      version,
			CloudURL:     cfg.CloudURL,
			BaseDir:      cfg.DataDir,
			Supervised:   cfg.Supervised,
			Window:       window,
			HardwareTier: hwTier,
			HTTPClient:   &http.Client{},
			LicenseTier:  func(c context.Context) string { return sync.LicenseTier(c, store) },
			RequestRestart: func() {
				restartRequested.Store(true)
				cancel()
			},
		})
		go updater.Run(ctx)
		logger.Info("update agent started")
	} else {
		logger.Info("cloud disabled (FUELMIND_CLOUD_URL unset; local-only mode)")
	}

	a := auth.New(store)
	srv, err := web.New(store, a, logger, cfg.Port)
	if err != nil {
		return 1, fmt.Errorf("web.New: %w", err)
	}
	srv.Bind = cfg.Bind
	srv.Version = version
	srv.Router = llm.NewRouter(llm.NewHTTPClient(cfg.OllamaURL), llm.Tier(hwTier))

	// Start the watcher before the dashboard blocks, so exports dropped
	// while the service was down are picked up immediately.
	if err := watcher.Start(ctx, posDrop); err != nil {
		return 1, fmt.Errorf("pos watcher: %w", err)
	}
	logger.Info("pos watcher started", slog.String("folder", posDrop))

	if err := srv.ListenAndServe(ctx); err != nil {
		return 1, fmt.Errorf("dashboard: %w", err)
	}
	if restartRequested.Load() {
		logger.Info("exiting for the launcher to apply a staged update")
		return launcher.ExitCodeRequestRestart, nil
	}
	logger.Info("fuelmind stopped")
	return 0, nil
}

// periodicRefresh re-runs normalization and materialization on a timer and
// sweeps the drop folder, so nothing depends on a file-system event
// arriving (spec §3.3).
func periodicRefresh(ctx context.Context, w *csvwatch.Watcher, n *normalizer.Normalizer, m *mart.Mart, posDrop string, logger *slog.Logger) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.ProcessExisting(ctx, posDrop); err != nil {
				logger.Warn("periodic drop-folder sweep", "err", err)
			}
			if _, _, err := n.Run(ctx, 0); err != nil {
				logger.Warn("periodic normalize", "err", err)
			}
			if err := m.MaterializeSince(ctx, time.Now().AddDate(0, 0, -7)); err != nil {
				logger.Warn("periodic mart refresh", "err", err)
			}
		}
	}
}

// loadOrCreateIdentity returns the install's station_id + api_key,
// generating them on first run and persisting both in local_config.
func loadOrCreateIdentity(ctx context.Context, store *storage.Storage, logger *slog.Logger) (ident.Identity, error) {
	sid := store.LocalConfigValue(ctx, ident.ConfigKeyStationID, "")
	key := store.LocalConfigValue(ctx, ident.ConfigKeyAPIKey, "")
	if sid != "" && key != "" {
		if ident.IsValidStationID(sid) {
			return ident.Identity{StationID: ident.StationID(sid), APIKey: ident.APIKey(key)}, nil
		}
		logger.Warn("persisted station_id is malformed, regenerating", slog.String("old_value", sid))
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

// takeOverRollbackRecord moves a rollback written by the launcher into
// local_config, where the sync agent picks it up for the next heartbeat.
func takeOverRollbackRecord(ctx context.Context, store *storage.Storage, dataDir string, logger *slog.Logger) {
	rec, ok, err := launcher.ReadRollbackRecord(dataDir)
	if err != nil {
		logger.Warn("read rollback record", "err", err)
		return
	}
	if !ok {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := store.SetLocalConfig(ctx, sync.ConfigKeyLastRollback, string(data), "", false); err != nil {
		logger.Warn("store rollback record", "err", err)
		return
	}
	logger.Warn("previous version was rolled back",
		"version", rec.FailedVersion, "reason", rec.Reason, "at", rec.At.Format(time.RFC3339))
	if err := launcher.DeleteRollbackRecord(dataDir); err != nil {
		logger.Warn("delete rollback record", "err", err)
	}
}
