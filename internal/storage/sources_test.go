package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func sourcesStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.MigrateCount(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// A watch folder is how the owner points FuelMind at the folder their POS
// already writes to. A path that cannot work must be refused here, with a
// reason the owner can act on — not accepted and then silently watched
// for nothing.
func TestAddWatchFolderRefusesUnusablePaths(t *testing.T) {
	s := sourcesStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	file := filepath.Join(dir, "export.csv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, wantIn string
	}{
		{"empty", "", "full path"},
		{"relative", "exports", "not a full path"},
		{"missing", filepath.Join(dir, "nope"), "no folder at"},
		{"a file", file, "is a file, not a folder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.AddWatchFolder(ctx, c.path, "")
			if err == nil {
				t.Fatalf("AddWatchFolder(%q) succeeded, want a refusal", c.path)
			}
			if !errors.Is(err, storage.ErrFolderNotUsable) {
				t.Errorf("error is not ErrFolderNotUsable: %v", err)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error %q does not explain the problem (want %q in it)", err, c.wantIn)
			}
			// A Windows path in the message must read exactly the way the
			// owner typed it: not with doubled separators, and not
			// re-cased by the display layer.
			if strings.Contains(err.Error(), `\\`) {
				t.Errorf("error doubles the path separators, which the owner did not type: %q", err)
			}
			if c.path != "" && !strings.Contains(err.Error(), c.path) {
				t.Errorf("error %q does not quote back what the owner typed (%q)", err, c.path)
			}
		})
	}
}

func TestWatchFolderLifecycle(t *testing.T) {
	s := sourcesStore(t)
	ctx := context.Background()
	builtIn, extra := t.TempDir(), t.TempDir()

	if err := s.EnsureBuiltInWatchFolder(ctx, builtIn); err != nil {
		t.Fatalf("EnsureBuiltInWatchFolder: %v", err)
	}
	// Registering it again must not create a second row.
	if err := s.EnsureBuiltInWatchFolder(ctx, builtIn); err != nil {
		t.Fatalf("EnsureBuiltInWatchFolder twice: %v", err)
	}
	if err := s.AddWatchFolder(ctx, extra, "POS exports"); err != nil {
		t.Fatalf("AddWatchFolder: %v", err)
	}

	folders, err := s.WatchFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2: %+v", len(folders), folders)
	}
	if !folders[0].BuiltIn {
		t.Errorf("the built-in folder should be listed first, got %+v", folders[0])
	}

	enabled, err := s.EnabledWatchFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 {
		t.Errorf("enabled = %v, want both", enabled)
	}

	// Switching one off leaves it configured but not watched.
	var extraID int64
	for _, f := range folders {
		if !f.BuiltIn {
			extraID = f.ID
		}
	}
	if err := s.SetWatchFolderEnabled(ctx, extraID, false); err != nil {
		t.Fatal(err)
	}
	if enabled, _ = s.EnabledWatchFolders(ctx); len(enabled) != 1 {
		t.Errorf("after switching one off, enabled = %v, want 1", enabled)
	}

	// The built-in folder can be switched off but never removed: there
	// must always be somewhere to drop a file.
	if err := s.RemoveWatchFolder(ctx, folders[0].ID); err == nil {
		t.Error("removing the built-in folder succeeded, want a refusal")
	}
	if err := s.RemoveWatchFolder(ctx, extraID); err != nil {
		t.Errorf("RemoveWatchFolder: %v", err)
	}
	if folders, _ = s.WatchFolders(ctx); len(folders) != 1 {
		t.Errorf("after removal, %d folders remain, want 1", len(folders))
	}
}

// The ingest history is how the owner tells "no sales today" from "the
// export stopped three days ago".
func TestIngestEventsNewestFirst(t *testing.T) {
	s := sourcesStore(t)
	ctx := context.Background()

	for _, e := range []storage.IngestEvent{
		{Source: storage.IngestSourceFolder, FileName: "day1.csv", RowsRead: 10, RowsInserted: 10, Outcome: storage.IngestOK},
		{Source: storage.IngestSourceUpload, FileName: "usb.csv", RowsRead: 5, RowsInserted: 4, RowsRejected: 1, Outcome: storage.IngestPartial, Detail: "1 row rejected"},
		{Source: storage.IngestSourceManual, Origin: "dashboard", RowsRead: 1, RowsInserted: 1, Outcome: storage.IngestOK},
	} {
		if err := s.RecordIngestEvent(ctx, e); err != nil {
			t.Fatalf("RecordIngestEvent: %v", err)
		}
	}

	events, err := s.RecentIngestEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Source != storage.IngestSourceManual {
		t.Errorf("newest event = %q, want the manual one last recorded", events[0].Source)
	}
	if events[1].RowsRejected != 1 || events[1].Detail != "1 row rejected" {
		t.Errorf("partial upload not recorded faithfully: %+v", events[1])
	}
}

func TestSetWatchFolderError(t *testing.T) {
	s := sourcesStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := s.AddWatchFolder(ctx, dir, ""); err != nil {
		t.Fatal(err)
	}

	if err := s.SetWatchFolderError(ctx, dir, "the network share is not reachable"); err != nil {
		t.Fatal(err)
	}
	folders, _ := s.WatchFolders(ctx)
	if folders[0].LastError != "the network share is not reachable" {
		t.Errorf("LastError = %q, want the recorded reason", folders[0].LastError)
	}

	// Switching a folder on clears the stale reason, so the page does not
	// keep showing a problem that has been dealt with.
	if err := s.SetWatchFolderEnabled(ctx, folders[0].ID, true); err != nil {
		t.Fatal(err)
	}
	folders, _ = s.WatchFolders(ctx)
	if folders[0].LastError != "" {
		t.Errorf("LastError = %q, want it cleared", folders[0].LastError)
	}
}
