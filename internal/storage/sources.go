package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WatchFolder is one folder FuelMind watches for POS exports.
type WatchFolder struct {
	ID        int64
	Path      string
	Label     string
	Enabled   bool
	BuiltIn   bool
	LastError string
	CreatedAt time.Time
}

// Ingest sources, as written to ingest_events.source.
const (
	IngestSourceFolder = "watch_folder"
	IngestSourceUpload = "upload"
	IngestSourceManual = "manual"
)

// Ingest outcomes, as written to ingest_events.outcome.
const (
	IngestOK      = "ok"
	IngestPartial = "partial"
	IngestFailed  = "failed"
)

// IngestEvent is one thing that arrived: a file from a watch folder, a
// file the owner uploaded, or a sale they typed in.
type IngestEvent struct {
	ID           int64
	OccurredAt   time.Time
	Source       string
	Origin       string
	FileName     string
	RowsRead     int
	RowsInserted int
	RowsRejected int
	Outcome      string
	Detail       string
}

// ErrFolderNotUsable explains why a folder cannot be watched, in words
// the owner can act on.
var ErrFolderNotUsable = errors.New("folder is not usable")

// WatchFolders lists every configured folder, built-in first.
func (s *Storage) WatchFolders(ctx context.Context) ([]WatchFolder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, COALESCE(label, ''), enabled, built_in,
		       COALESCE(last_error, ''), created_at
		FROM pos_watch_folders
		ORDER BY built_in DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatchFolder
	for rows.Next() {
		var f WatchFolder
		var enabled, builtIn int
		if err := rows.Scan(&f.ID, &f.Path, &f.Label, &enabled, &builtIn,
			&f.LastError, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Enabled, f.BuiltIn = enabled == 1, builtIn == 1
		out = append(out, f)
	}
	return out, rows.Err()
}

// EnabledWatchFolders returns the paths currently being watched.
func (s *Storage) EnabledWatchFolders(ctx context.Context) ([]string, error) {
	folders, err := s.WatchFolders(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range folders {
		if f.Enabled {
			out = append(out, f.Path)
		}
	}
	return out, nil
}

// EnsureBuiltInWatchFolder registers FuelMind's own drop folder, so there
// is always one folder to fall back on even if the owner removes the
// others. Calling it again with the same path is a no-op.
func (s *Storage) EnsureBuiltInWatchFolder(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pos_watch_folders (path, label, enabled, built_in)
		VALUES (?, ?, 1, 1)
		ON CONFLICT (path) DO UPDATE SET built_in = 1`,
		filepath.Clean(path), "FuelMind drop folder")
	return err
}

// AddWatchFolder validates a folder and records it. The validation is the
// point: a path typed with a typo, or one the service account cannot
// read, must fail here with something the owner can fix — not silently
// watch nothing.
func (s *Storage) AddWatchFolder(ctx context.Context, path, label string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return fmt.Errorf("%w: type the full path to the folder, for example C:\\POS\\exports", ErrFolderNotUsable)
	}
	if !filepath.IsAbs(clean) {
		// The path does not lead the message: the caller capitalizes the
		// first letter for display, and doing that to what the owner
		// typed would hand their own path back to them altered.
		return fmt.Errorf("%w: that is not a full path. Start from the drive letter, for example C:\\POS\\exports (you typed %s)", ErrFolderNotUsable, clean)
	}
	info, err := os.Stat(clean)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("%w: there is no folder at %s. Check the spelling, or create it first", ErrFolderNotUsable, clean)
	case err != nil:
		return fmt.Errorf("%w: cannot open %s: %v", ErrFolderNotUsable, clean, err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is a file, not a folder. Give the folder the exports are written into", ErrFolderNotUsable, clean)
	}
	// FuelMind moves each file into processed/ or failed/ once it has
	// read it, so it needs to be able to write here, not just read.
	if err := checkWritable(clean); err != nil {
		return fmt.Errorf("%w: FuelMind cannot write to %s (%v). It needs to move each export into a processed folder once it has read it", ErrFolderNotUsable, clean, err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pos_watch_folders (path, label, enabled, built_in)
		VALUES (?, ?, 1, 0)
		ON CONFLICT (path) DO UPDATE SET enabled = 1, label = excluded.label`,
		clean, nullableString(strings.TrimSpace(label)))
	return err
}

// checkWritable confirms the service can create and remove a file here.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".fuelmind-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// SetWatchFolderEnabled turns a folder on or off without forgetting it.
func (s *Storage) SetWatchFolderEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE pos_watch_folders SET enabled = ?, last_error = NULL WHERE id = ?`, v, id)
	return err
}

// RemoveWatchFolder forgets a folder. The built-in folder is kept — it
// can be switched off, but there must always be somewhere to drop a file.
func (s *Storage) RemoveWatchFolder(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pos_watch_folders WHERE id = ? AND built_in = 0`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("storage: that folder is FuelMind's own drop folder; switch it off instead of removing it")
	}
	return nil
}

// SetWatchFolderError records why a folder could not be watched, so the
// dashboard can show it against the folder instead of only in a log.
func (s *Storage) SetWatchFolderError(ctx context.Context, path, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pos_watch_folders SET last_error = ? WHERE path = ?`,
		nullableString(msg), filepath.Clean(path))
	return err
}

// RecordIngestEvent appends one line to the ingest history.
func (s *Storage) RecordIngestEvent(ctx context.Context, e IngestEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ingest_events
			(source, origin, file_name, rows_read, rows_inserted, rows_rejected, outcome, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Source, nullableString(e.Origin), nullableString(e.FileName),
		e.RowsRead, e.RowsInserted, e.RowsRejected, e.Outcome, nullableString(e.Detail))
	return err
}

// RecentIngestEvents returns the newest ingest history first.
func (s *Storage) RecentIngestEvents(ctx context.Context, limit int) ([]IngestEvent, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, occurred_at, source, COALESCE(origin, ''), COALESCE(file_name, ''),
		       rows_read, rows_inserted, rows_rejected, outcome, COALESCE(detail, '')
		FROM ingest_events ORDER BY occurred_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngestEvent
	for rows.Next() {
		var e IngestEvent
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Source, &e.Origin, &e.FileName,
			&e.RowsRead, &e.RowsInserted, &e.RowsRejected, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
