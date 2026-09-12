package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FUELMIND_DATA_DIR", dir)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8765 || c.Bind != "0.0.0.0" || c.SyncCycle != 30*time.Minute {
		t.Errorf("defaults: %+v", c)
	}
	if c.SyncEnabled || c.Supervised || c.UpdateAnytime {
		t.Errorf("expected cloud/sync off by default: %+v", c)
	}
	if c.Path != filepath.Join(dir, "fuelmind.db") {
		t.Errorf("db path = %q", c.Path)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("FUELMIND_DATA_DIR", t.TempDir())
	t.Setenv("FUELMIND_PORT", "9000")
	t.Setenv("FUELMIND_BIND", "127.0.0.1")
	t.Setenv("FUELMIND_CLOUD_URL", "https://cloud.example/")
	t.Setenv("FUELMIND_SYNC_CYCLE", "5m")
	t.Setenv("FUELMIND_HARDWARE_TIER", "PRO")
	t.Setenv("FUELMIND_SUPERVISED", "1")
	t.Setenv("FUELMIND_UPDATE_WINDOW", "any")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.Bind != "127.0.0.1" || !c.SyncEnabled || c.CloudURL != "https://cloud.example" {
		t.Errorf("overrides: %+v", c)
	}
	if c.HardwareTier != "pro" || !c.Supervised || !c.UpdateAnytime || c.SyncCycle != 5*time.Minute {
		t.Errorf("overrides: %+v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	t.Setenv("FUELMIND_DATA_DIR", t.TempDir())
	for _, bad := range []struct{ k, v string }{
		{"FUELMIND_PORT", "0"},
		{"FUELMIND_PORT", "not-a-port"},
		{"FUELMIND_SYNC_CYCLE", "10s"},
		{"FUELMIND_SYNC_CYCLE", "banana"},
		{"FUELMIND_HARDWARE_TIER", "turbo"},
	} {
		t.Setenv(bad.k, bad.v)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q was accepted", bad.k, bad.v)
		}
		t.Setenv(bad.k, "")
	}
}
