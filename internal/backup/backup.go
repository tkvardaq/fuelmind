// Package backup keeps nightly copies of the SQLite database.
//
// Backups use SQLite's VACUUM INTO on the live connection, so the copy is
// transactionally consistent even while the POS watcher is writing (plan
// §7.9). Copies land in <data dir>\backups and the newest 7 are kept.
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store is the part of storage the agent needs.
type Store interface {
	BackupTo(ctx context.Context, dst string) error
}

// Config holds the agent's settings.
type Config struct {
	// DataDir is where backups\ lives.
	DataDir string
	// Store is the live database. Required.
	Store Store
	// Hour is the local hour to run at (default 2 = 02:00 station time).
	Hour int
	// Keep is how many backups to retain (default 7).
	Keep   int
	Logger *slog.Logger
}

// Agent runs the nightly backup loop.
type Agent struct{ cfg Config }

// NewAgent creates a backup agent.
func NewAgent(cfg Config) *Agent {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 7
	}
	return &Agent{cfg: cfg}
}

// Start runs the loop in the background until ctx is done.
func (a *Agent) Start(ctx context.Context) { go a.run(ctx) }

func (a *Agent) run(ctx context.Context) {
	for {
		wait := time.Until(a.nextRun(time.Now()))
		a.cfg.Logger.Info("backup agent: next backup", "in", wait.Round(time.Minute))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			a.cfg.Logger.Info("backup agent stopped", "reason", ctx.Err().Error())
			return
		case <-timer.C:
			if err := a.PerformBackup(ctx); err != nil {
				a.cfg.Logger.Warn("backup failed", "err", err)
			}
		}
	}
}

// nextRun is the next occurrence of Hour in local (station) time.
func (a *Agent) nextRun(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), a.cfg.Hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// PerformBackup writes one consistent copy and applies retention.
func (a *Agent) PerformBackup(ctx context.Context) error {
	if a.cfg.Store == nil {
		return fmt.Errorf("backup: no database configured")
	}
	dir := filepath.Join(a.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backup: create %s: %w", dir, err)
	}
	dst := filepath.Join(dir, "fuelmind_"+time.Now().Format("2006-01-02_15-04-05")+".db")
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("fuelmind_%s-%d.db", time.Now().Format("2006-01-02_15-04-05"), i))
	}
	if err := a.cfg.Store.BackupTo(ctx, dst); err != nil {
		return err
	}
	a.cfg.Logger.Info("backup written", "path", dst)
	if err := enforceRetention(dir, a.cfg.Keep); err != nil {
		a.cfg.Logger.Warn("backup retention", "err", err)
	}
	return nil
}

// enforceRetention keeps the newest `keep` fuelmind_*.db files. Copies
// taken before a migration (pre-migrate-*.db) are kept separately.
func enforceRetention(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type backupFile struct {
		name string
		mod  time.Time
	}
	var files []backupFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isNightlyBackup(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, backupFile{name, info.ModTime()})
	}
	if len(files) <= keep {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files[:len(files)-keep] {
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	return nil
}

func isNightlyBackup(name string) bool {
	return len(name) > len("fuelmind_.db") &&
		filepath.Ext(name) == ".db" &&
		name[:len("fuelmind_")] == "fuelmind_"
}
