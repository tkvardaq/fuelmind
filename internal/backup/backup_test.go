package backup_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func TestPerformBackupAndRetention(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fuelmind.db")
	s, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	agent := backup.NewAgent(backup.Config{
		DataDir: dir,
		Logger:  logger,
	})

	for i := 0; i < 9; i++ {
		time.Sleep(10 * time.Millisecond)
		if err := agent.PerformBackup(context.Background()); err != nil {
			t.Fatalf("PerformBackup %d: %v", i, err)
		}
	}

	backupDir := filepath.Join(dir, "backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) > 7 {
		t.Errorf("expected max 7 backups retained, found %d", len(entries))
	}
	if len(entries) == 0 {
		t.Error("expected at least 1 backup file created")
	}
}
