package backup_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/backup"
	"github.com/fuelmind/fuelmind/internal/storage"
	_ "modernc.org/sqlite"
)

func newAgent(t *testing.T) (*backup.Agent, *storage.Storage, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "fuelmind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return backup.NewAgent(backup.Config{DataDir: dir, Store: s, Logger: logger, Hour: 2}), s, dir
}

func TestPerformBackupAndRetention(t *testing.T) {
	agent, _, dir := newAgent(t)
	for i := 0; i < 9; i++ {
		if err := agent.PerformBackup(context.Background()); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Errorf("kept %d backups, want 7", len(entries))
	}
}

// A backup taken while the watcher is writing must still open and pass an
// integrity check, and must contain the rows committed before it started.
func TestBackupIsConsistentUnderLoad(t *testing.T) {
	agent, store, dir := newAgent(t)
	var written int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n := atomic.AddInt64(&written, 1)
			if _, err := store.WriteRawTransactions(context.Background(), "b", "lane_1",
				[][]byte{[]byte(fmt.Sprintf(`{"n":%d,"pad":"%0300d"}`, n, n))}, []string{fmt.Sprint(n)}); err != nil {
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 3; i++ {
		before := atomic.LoadInt64(&written)
		if err := agent.PerformBackup(context.Background()); err != nil {
			t.Fatalf("backup under load: %v", err)
		}
		if before == 0 {
			t.Fatal("no concurrent writes happened; test is not exercising the race")
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	<-done

	entries, _ := os.ReadDir(filepath.Join(dir, "backups"))
	if len(entries) == 0 {
		t.Fatal("no backups written")
	}
	for _, e := range entries {
		db, err := sql.Open("sqlite", filepath.Join(dir, "backups", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var check string
		var rows int
		err1 := db.QueryRow("PRAGMA integrity_check").Scan(&check)
		err2 := db.QueryRow("SELECT COUNT(*) FROM raw_pos_transactions").Scan(&rows)
		_ = db.Close()
		if err1 != nil || err2 != nil || check != "ok" || rows == 0 {
			t.Errorf("%s: integrity=%q rows=%d err=%v/%v", e.Name(), check, rows, err1, err2)
		}
	}
}

func TestBackupNeedsAStore(t *testing.T) {
	a := backup.NewAgent(backup.Config{DataDir: t.TempDir()})
	if err := a.PerformBackup(context.Background()); err == nil {
		t.Error("expected an error without a database")
	}
}
