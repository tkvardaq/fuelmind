// Package config loads FuelMind's runtime configuration from the
// environment. The installer sets nothing: every value has a working
// default, and support can override any of them per station.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is the runtime configuration.
type Config struct {
	Path     string // absolute path to fuelmind.db
	DataDir  string // absolute path to FuelMind's data directory
	LogLevel string // "debug" | "info" | "warn" | "error"
	Port     int    // dashboard port
	Bind     string // dashboard listen address (default 0.0.0.0)

	CloudURL    string        // control plane base URL; empty = local-only
	SyncCycle   time.Duration // heartbeat cycle (1m..6h, default 30m)
	SyncEnabled bool          // derived from CloudURL != ""

	HardwareTier string // detected, or forced with FUELMIND_HARDWARE_TIER
	OllamaURL    string // local LLM endpoint

	// Supervised is true when the launcher started this process, which
	// means updates can be applied by exiting for a handoff.
	Supervised bool
	// UpdateAnytime disables the 02:00-05:00 maintenance window.
	UpdateAnytime bool
	// UpdateInterval is how often the station asks whether a new version
	// is available. Zero means the update agent's own default (30m).
	// It exists so support can watch an update happen on a station that
	// is misbehaving, instead of waiting half an hour per attempt.
	UpdateInterval time.Duration
}

// Load reads configuration from environment variables and defaults:
//
//	FUELMIND_DATA_DIR       default %ProgramData%\FuelMind, else ~/.fuelmind
//	FUELMIND_LOG_LEVEL      debug|info|warn|error (default info)
//	FUELMIND_PORT           dashboard port (default 8765)
//	FUELMIND_BIND           listen address (default 0.0.0.0, all interfaces)
//	FUELMIND_CLOUD_URL      control plane; unset = no network at all
//	FUELMIND_SYNC_CYCLE     heartbeat interval (default 30m, 1m..6h)
//	FUELMIND_HARDWARE_TIER  basic|standard|enhanced|pro (default: detected)
//	FUELMIND_OLLAMA_URL     local LLM endpoint (default http://127.0.0.1:11434)
//	FUELMIND_UPDATE_WINDOW  "any" applies updates immediately
//	FUELMIND_UPDATE_INTERVAL how often to check for a new version (1m..24h)
//	FUELMIND_SUPERVISED     set to 1 by the launcher
func Load() (*Config, error) {
	dataDir := os.Getenv("FUELMIND_DATA_DIR")
	if dataDir == "" {
		var err error
		if dataDir, err = defaultDataDir(); err != nil {
			return nil, err
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
	bind := os.Getenv("FUELMIND_BIND")
	if bind == "" {
		bind = "0.0.0.0"
	}

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

	tier := strings.ToLower(strings.TrimSpace(os.Getenv("FUELMIND_HARDWARE_TIER")))
	switch tier {
	case "", "basic", "standard", "enhanced", "pro":
	default:
		return nil, fmt.Errorf("config: FUELMIND_HARDWARE_TIER %q must be basic, standard, enhanced or pro", tier)
	}

	ollama := os.Getenv("FUELMIND_OLLAMA_URL")
	if ollama == "" {
		ollama = "http://127.0.0.1:11434"
	}

	var updateInterval time.Duration
	if v := os.Getenv("FUELMIND_UPDATE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("config: FUELMIND_UPDATE_INTERVAL %q is not a valid duration: %w", v, err)
		}
		if d < time.Minute || d > 24*time.Hour {
			return nil, fmt.Errorf("config: FUELMIND_UPDATE_INTERVAL %q out of range [1m, 24h]", v)
		}
		updateInterval = d
	}

	cloudURL := strings.TrimRight(os.Getenv("FUELMIND_CLOUD_URL"), "/")
	return &Config{
		Path:           filepath.Join(dataDir, "fuelmind.db"),
		DataDir:        dataDir,
		LogLevel:       lvl,
		Port:           port,
		Bind:           bind,
		CloudURL:       cloudURL,
		SyncCycle:      syncCycle,
		SyncEnabled:    cloudURL != "",
		HardwareTier:   tier,
		OllamaURL:      ollama,
		Supervised:     os.Getenv("FUELMIND_SUPERVISED") == "1",
		UpdateAnytime:  strings.EqualFold(os.Getenv("FUELMIND_UPDATE_WINDOW"), "any"),
		UpdateInterval: updateInterval,
	}, nil
}

func defaultDataDir() (string, error) {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "FuelMind"), nil
		}
		return `C:\ProgramData\FuelMind`, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".fuelmind"), nil
}

// ErrNotInitialized is returned by callers that need an initialised data
// directory, so the installer can tell "fresh install" from "corrupt".
var ErrNotInitialized = errors.New("fuelmind: not initialized (run installer first)")
