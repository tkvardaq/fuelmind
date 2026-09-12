package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	up      string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("storage: read migrations dir: %w", err)
	}
	var all []migration
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".up.sql") {
			continue
		}
		// "001_raw_layer.up.sql" -> version 1, name "001_raw_layer"
		base := strings.TrimSuffix(n, ".up.sql")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("storage: bad migration filename %q (expected NNN_name.up.sql)", n)
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("storage: bad migration version in %q: %w", n, err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+n)
		if err != nil {
			return nil, fmt.Errorf("storage: read %q: %w", n, err)
		}
		all = append(all, migration{version: v, name: base, up: string(body)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	return all, nil
}

// Migrate runs any pending embedded migrations. See MigrateCount.
func (s *Storage) Migrate(ctx context.Context) error {
	_, err := s.MigrateCount(ctx)
	return err
}

// MigrateCount runs pending migrations and returns how many were
// applied. Each migration and its schema_migrations row are applied in
// one transaction, so a failing migration leaves no partial schema
// behind. When an existing database is about to be migrated, a
// consistent copy is first written to <db dir>/backups/ so a rollback to
// the previous binary can restore the pre-migration data.
func (s *Storage) MigrateCount(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return 0, fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	all, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	applied := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("storage: list applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		applied[v] = true
	}
	rows.Close()

	var pending []migration
	for _, m := range all {
		if !applied[m.version] {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	if len(applied) > 0 {
		if err := s.preMigrationBackup(ctx, pending[0].version); err != nil {
			return 0, fmt.Errorf("storage: pre-migration backup: %w", err)
		}
	}

	for _, m := range pending {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("storage: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.up); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("storage: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name,
		); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("storage: record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("storage: commit migration %s: %w", m.name, err)
		}
	}
	return len(pending), nil
}

func (s *Storage) preMigrationBackup(ctx context.Context, firstPending int) error {
	path, err := s.Path(ctx)
	if err != nil || path == "" {
		return err // in-memory database: nothing to back up
	}
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, fmt.Sprintf("pre-migrate-%03d_%s.db", firstPending, time.Now().Format("2006-01-02_15-04-05")))
	return s.BackupTo(ctx, dst)
}
