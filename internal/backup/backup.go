// Package backup implements a nightly automatic backup of the SQLite database.
package backup

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// Config holds configuration for the backup agent.
type Config struct {
	// DataDir is the root directory where the live database lives.
	DataDir string
	// Logger for diagnostic output.
	Logger *slog.Logger
}

// Agent runs the backup loop.
type Agent struct {
	cfg Config
}

// NewAgent creates a backup agent with the given config.
func NewAgent(cfg Config) *Agent {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Agent{cfg: cfg}
}

// Start launches the backup goroutine. It calculates the time until the next
// backup window and then ticks every hour to check if it's time.
func (a *Agent) Start(ctx context.Context) {
	go a.run(ctx)
}

// run loops until ctx is done, performing a backup once per day at 2 AM UTC.
func (a *Agent) run(ctx context.Context) {
	// Compute initial delay until the next backup hour (2 AM UTC).
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
	if now.After(next) {
		next = next.Add(24 * time.Hour)
	}
	firstDelay := next.Sub(now)
	a.cfg.Logger.Info("backup agent: initial delay until first backup", "delay", firstDelay)

	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			a.cfg.Logger.Info("backup agent stopped", "reason", ctx.Err().Error())
			return
		case <-timer.C:
			a.performBackup(ctx)
			// After a backup, schedule for same time next day.
			next := time.Now().UTC().Add(24 * time.Hour)
			next = time.Date(next.Year(), next.Month(), next.Day(), 2, 0, 0, 0, time.UTC)
			delay := next.Sub(time.Now().UTC())
			if delay < 0 {
				delay = 24 * time.Hour
			}
			timer.Reset(delay)
		}
	}
}

// PerformBackup copies the SQLite database file to the backup directory with a timestamp,
// checkpoints the WAL to ensure consistency, and enforces retention policy.
func (a *Agent) PerformBackup(ctx context.Context) error {
	a.cfg.Logger.Info("backup agent: starting backup")

	dbPath := filepath.Join(a.cfg.DataDir, "fuelmind.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("backup agent: database not found: %w", err)
	}

	// Open the live database to flush WAL.
	store, err := storage.Open(dbPath)
	if err != nil {
		a.cfg.Logger.Warn("backup agent: could not open database for checkpoint", "err", err)
	} else {
		if _, err := store.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			a.cfg.Logger.Warn("backup agent: wal checkpoint failed", "err", err)
		}
		_ = store.Close()
	}

	// Ensure backup directory exists.
	backupDir := filepath.Join(a.cfg.DataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("backup agent: could not create backup dir: %w", err)
	}

	// Generate backup filename with timestamp.
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	backupName := fmt.Sprintf("fuelmind_%s.db", timestamp)
	backupPath := filepath.Join(backupDir, backupName)

	// Copy the database file.
	if err := copyFile(dbPath, backupPath); err != nil {
		return fmt.Errorf("backup agent: copy failed: %w", err)
	}
	a.cfg.Logger.Info("backup agent: backup copied", "path", backupPath)

	// Enforce retention: keep only the 7 most recent backup files.
	if err := enforceRetention(backupDir, 7); err != nil {
		a.cfg.Logger.Warn("backup agent: retention enforcement failed", "err", err)
	}
	a.cfg.Logger.Info("backup agent: backup completed successfully")
	return nil
}

func (a *Agent) performBackup(ctx context.Context) {
	if err := a.PerformBackup(ctx); err != nil {
		a.cfg.Logger.Warn("backup agent: performBackup error", "err", err)
	}
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

// enforceRetention keeps only the N most recent files (by modification time) in dir.
// It assumes backup files match the pattern fuelmind_*.db.
func enforceRetention(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var backups []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "" {
			// Simple pattern match; could be stricter.
			if len(e.Name()) >= len("fuelmind_.db") && e.Name()[0:8] == "fuelmind" && e.Name()[len(e.Name())-3:] == ".db" {
				backups = append(backups, e)
			}
		}
	}
	if len(backups) <= keep {
		return nil
	}
	// Sort by modification time (oldest first).
	type fileInfo struct {
		entry os.DirEntry
		mod   time.Time
	}
	var infos []fileInfo
	for _, e := range backups {
		info, err := e.Info()
		if err != nil {
			// If we cannot stat, skip this entry.
			continue
		}
		infos = append(infos, fileInfo{entry: e, mod: info.ModTime()})
	}
	// Sort by mod time ascending.
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].mod.After(infos[j].mod) {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	// Delete the oldest excess files.
	for i := 0; i < len(infos)-keep; i++ {
		path := filepath.Join(dir, infos[i].entry.Name())
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}