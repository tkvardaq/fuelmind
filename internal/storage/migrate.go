package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs any pending embedded migrations against the database.
// Idempotent: re-running is a no-op. Migrations are applied in version
// order; each is its own implicit transaction (SQLite DDL is
// transactional). Failure aborts the whole run.
func (s *Storage) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("storage: read migrations dir: %w", err)
	}

	type migration struct {
		version int
		name    string
		up      string
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
			return fmt.Errorf("storage: bad migration filename %q (expected NNN_name.up.sql)", n)
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("storage: bad migration version in %q: %w", n, err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+n)
		if err != nil {
			return fmt.Errorf("storage: read %q: %w", n, err)
		}
		all = append(all, migration{version: v, name: base, up: string(body)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })

	for _, m := range all {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version,
		).Scan(&exists)
		if err != nil {
			return fmt.Errorf("storage: query applied version %d: %w", m.version, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, m.up); err != nil {
			return fmt.Errorf("storage: apply migration %s: %w", m.name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name,
		); err != nil {
			return fmt.Errorf("storage: record migration %s: %w", m.name, err)
		}
	}
	return nil
}
