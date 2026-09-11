// Package config loads FuelMind's runtime configuration. The full config
// (POS adapter settings, cloud endpoints, hardware tier overrides) will
// live here as it grows. Phase 0 only needs the data dir, the log level,
// and the path to the SQLite file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config is the runtime configuration. Phase 0 has the minimum fields
// needed to start. Phases 1+ extend it (POS adapters, sync, etc.).
type Config struct {
	Path     string // absolute path to fuelmind.db (Phase 3+)
	DataDir  string // absolute path to FuelMind's data directory
	LogLevel string // "debug" | "info" | "warn" | "error"
	Port     int    // HTTP server port for the local dashboard (Phase 4+)

	// Phase 5: cloud control plane plumbing.
	CloudURL    string        // base URL of the cloud heartbeat/license endpoint (default: empty = sync disabled)
	SyncCycle   time.Duration // heartbeat cycle (default 30m, range 1m..6h)
	SyncEnabled bool          // convenience: derived from CloudURL != ""

	// Phase 7: hardware tier for LLM sizing.
	HardwareTier string        // "basic" | "standard" | "enhanced" | "pro"
}

// Load reads configuration from environment variables and sensible defaults.
//
// Environment overrides:
//
//	FUELMIND_DATA_DIR   absolute path; default C:\ProgramData\FuelMind on
//	                    Windows, $HOME/.fuelmind elsewhere
//	FUELMIND_LOG_LEVEL  "debug" | "info" | "warn" | "error" (default "info")
//	FUELMIND_PORT       HTTP port for the local dashboard (default 8765)
//	FUELMIND_CLOUD_URL  base URL of the cloud control plane; if empty,
//	                    the sync agent is disabled (local-only mode)
//	FUELMIND_SYNC_CYCLE heartbeat interval (e.g. "30m", "1h"); default 30m,
//	                    min 1m, max 6h
//
// On any I/O failure creating the data directory, Load returns an error —
// we never silently fall back, because a FuelMind install that can't write
// its data dir is broken.
func Load() (*Config, error) {
	dataDir := os.Getenv("FUELMIND_DATA_DIR")
	if dataDir == "" {
		if isWindows() {
			dataDir = `C:\ProgramData\FuelMind`
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("config: cannot determine home dir: %w", err)
			}
			dataDir = filepath.Join(home, ".fuelmind")
		}
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: mkdir data dir %q: %w", dataDir, err)
	}

	port := 8765
	if v := os.Getenv("FUELMIND_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("config: FUELMIND_PORT %q is not a valid port", v)
		}
		port = n
	}

	lvl := os.Getenv("FUELMIND_LOG_LEVEL")
	if lvl == "" {
		lvl = "info"
	}

	cloudURL := os.Getenv("FUELMIND_CLOUD_URL")
	syncCycle := 30 * time.Minute
	if v := os.Getenv("FUELMIND_SYNC_CYCLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("config: FUELMIND_SYNC_CYCLE %q is not a valid duration: %w", v, err)
		}
		if d < time.Minute || d > 6*time.Hour {
			return nil, fmt.Errorf("config: FUELMIND_SYNC_CYCLE %q out of range [1m, 6h]", v)
		}
		syncCycle = d
	}

	return &Config{
		Path:        filepath.Join(dataDir, "fuelmind.db"),
		DataDir:     dataDir,
		LogLevel:    lvl,
		Port:        port,
		CloudURL:    cloudURL,
		SyncCycle:   syncCycle,
		SyncEnabled: cloudURL != "",
	}, nil
}

func isWindows() bool { return os.PathSeparator == '\\' }

// ErrNotInitialized is returned by future calls that need the data dir
// to exist; surfaces up so the installer can detect "fresh install" vs
// "corrupt existing install".
var ErrNotInitialized = errors.New("fuelmind: not initialized (run installer first)")
