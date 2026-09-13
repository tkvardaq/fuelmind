// Package storage owns the local SQLite database. It opens the file with
// the pragmas we need (WAL, foreign keys, busy timeout), runs the
// embedded migration set, and exposes typed methods for writing raw data
// and reading it back for the normalizer, mart and dashboard.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver (no CGo). Registered for the "sqlite" driver
	// name so *sql.DB can open it.
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a single-row lookup has no match.
var ErrNotFound = errors.New("fuelmind: not found")

// ErrInvalidSession is returned for missing or expired sessions.
var ErrInvalidSession = errors.New("fuelmind: invalid or expired session")

// Storage wraps a single SQLite database connection. The local core is
// single-writer by design (one process, one station), so the pool is
// capped at one connection — SQLite's locking layer is happier that way.
type Storage struct {
	db *sql.DB
}

// Open creates or opens the SQLite database at path. The DSN sets WAL
// mode, foreign keys, normal synchronous, and a 5s busy timeout.
func Open(path string) (*Storage, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %q: %w", path, err)
	}
	// quick_check is O(pages) but skips the expensive index cross-checks
	// of integrity_check, so boot stays fast on a large database.
	var result string
	if err := db.QueryRowContext(context.Background(), "PRAGMA quick_check").Scan(&result); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: integrity check failed: %w", err)
	}
	if result != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("storage: database corruption detected: %s", result)
	}
	return &Storage{db: db}, nil
}

// NewWithDB wraps an existing *sql.DB (tests).
func NewWithDB(db *sql.DB) *Storage {
	return &Storage{db: db}
}

// DB returns the underlying *sql.DB for the normalizer, mart and tooling.
func (s *Storage) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Storage) Close() error { return s.db.Close() }

// Path returns the on-disk path of the main database file, or "" for an
// in-memory database.
func (s *Storage) Path(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file, nil
		}
	}
	return "", rows.Err()
}

// BackupTo writes a transactionally consistent copy of the database to
// dst using VACUUM INTO. dst must not exist.
func (s *Storage) BackupTo(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("storage: backup target %q already exists", dst)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("storage: vacuum into %q: %w", dst, err)
	}
	return nil
}

// WriteRawTransactions inserts a batch of raw transactions in a single
// transaction. Rows whose payload_hash already exists are skipped (a
// byte-identical re-ingest). Returns the number of rows inserted.
func (s *Storage) WriteRawTransactions(ctx context.Context, batchID, posSourceID string, payloads [][]byte, hashes []string) (int, error) {
	if len(payloads) != len(hashes) {
		return 0, fmt.Errorf("storage: payloads (%d) and hashes (%d) length mismatch", len(payloads), len(hashes))
	}
	if len(payloads) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO raw_pos_transactions (pos_source_id, raw_payload, payload_hash, ingestion_batch_id)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("storage: prepare insert: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	for i, p := range payloads {
		res, err := stmt.ExecContext(ctx, posSourceID, string(p), hashes[i], batchID)
		if err != nil {
			return 0, fmt.Errorf("storage: insert row %d: %w", i, err)
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit: %w", err)
	}
	return inserted, nil
}

// RawTransaction is the minimal view a normalizer needs.
type RawTransaction struct {
	ID          int64
	PosSourceID string
	RawPayload  string
	BatchID     string
	ReceivedAt  string
}

// UnnormalizedTransactions returns raw rows that have been neither
// normalized nor superseded by a newer export of the same POS
// transaction. Capped by limit (0 = no limit).
func (s *Storage) UnnormalizedTransactions(ctx context.Context, limit int) ([]RawTransaction, error) {
	q := `SELECT id, pos_source_id, raw_payload, ingestion_batch_id, received_at
	      FROM raw_unresolved ORDER BY id ASC`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query unnormalized: %w", err)
	}
	defer rows.Close()

	var out []RawTransaction
	for rows.Next() {
		var r RawTransaction
		if err := rows.Scan(&r.ID, &r.PosSourceID, &r.RawPayload, &r.BatchID, &r.ReceivedAt); err != nil {
			return nil, fmt.Errorf("storage: scan unnormalized: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NormalizedTransaction is the destination row from the normalizer.
type NormalizedTransaction struct {
	RawTransactionID int64
	PosSourceID      string
	ExternalID       string
	ProductCode      string
	QuantityLiters   float64
	UnitPrice        float64
	TotalAmount      float64
	PaymentMethod    string
	CustomerPhone    string
	PumpID           string
	Attendant        string
	TransactionTime  string // RFC 3339, station-local offset
	Flags            string // comma-separated data-quality flags
}

// WriteNormalizedTransaction stores one normalized row.
//
// Idempotency has two layers:
//   - raw_transaction_id is UNIQUE, so re-normalizing a raw row is a no-op.
//   - (pos_source_id, external_id) is UNIQUE. When the POS re-exports a
//     transaction it already sent (typically with a correction), the
//     existing row is updated to the newer values and the older raw row
//     is recorded in raw_superseded. Revenue is never counted twice.
//
// It returns true when an existing transaction was replaced.
func (s *Storage) WriteNormalizedTransaction(ctx context.Context, n NormalizedTransaction) (replaced bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("storage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	args := []any{
		n.ProductCode, n.QuantityLiters, n.UnitPrice, n.TotalAmount, n.PaymentMethod,
		nullableString(n.CustomerPhone), nullableString(n.PumpID), nullableString(n.Attendant),
		n.TransactionTime, n.Flags,
	}

	if n.ExternalID != "" {
		var existingID, existingRaw int64
		err := tx.QueryRowContext(ctx, `
			SELECT id, raw_transaction_id FROM transactions
			WHERE pos_source_id = ? AND external_id = ?`, n.PosSourceID, n.ExternalID,
		).Scan(&existingID, &existingRaw)
		switch {
		case err == nil && existingRaw != n.RawTransactionID:
			if _, err := tx.ExecContext(ctx, `
				UPDATE transactions SET
					product_code = ?, quantity_liters = ?, unit_price = ?, total_amount = ?,
					payment_method = ?, customer_phone = ?, pump_id = ?, attendant = ?,
					transaction_time = ?, flags = ?, raw_transaction_id = ?
				WHERE id = ?`, append(args, n.RawTransactionID, existingID)...); err != nil {
				return false, fmt.Errorf("storage: replace transaction: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT OR REPLACE INTO raw_superseded (raw_transaction_id, superseded_by) VALUES (?, ?)`,
				existingRaw, n.RawTransactionID); err != nil {
				return false, fmt.Errorf("storage: record superseded: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM raw_normalize_errors WHERE raw_transaction_id = ?`, n.RawTransactionID); err != nil {
				return false, err
			}
			return true, tx.Commit()
		case err == nil:
			return false, nil // same raw row, already stored
		case !errors.Is(err, sql.ErrNoRows):
			return false, fmt.Errorf("storage: lookup external id: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO transactions (
			product_code, quantity_liters, unit_price, total_amount, payment_method,
			customer_phone, pump_id, attendant, transaction_time, flags,
			raw_transaction_id, pos_source_id, external_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		append(args, n.RawTransactionID, nullableString(n.PosSourceID), nullableString(n.ExternalID))...,
	); err != nil {
		return false, fmt.Errorf("storage: insert transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM raw_normalize_errors WHERE raw_transaction_id = ?`, n.RawTransactionID); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// RecordNormalizeError remembers why a raw row could not be normalized.
// It returns true the first time a row fails, so callers can log once
// instead of on every retry.
func (s *Storage) RecordNormalizeError(ctx context.Context, rawID int64, msg string) (first bool, err error) {
	var attempts int
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO raw_normalize_errors (raw_transaction_id, error) VALUES (?, ?)
		ON CONFLICT(raw_transaction_id) DO UPDATE SET
			error = excluded.error,
			attempts = attempts + 1,
			last_attempt_at = CURRENT_TIMESTAMP
		RETURNING attempts`, rawID, msg).Scan(&attempts)
	if err != nil {
		return false, fmt.Errorf("storage: record normalize error: %w", err)
	}
	return attempts == 1, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ProductAliases returns the full alias table as alias_text -> product_code.
func (s *Storage) ProductAliases(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias_text, product_code FROM product_aliases`)
	if err != nil {
		return nil, fmt.Errorf("storage: query aliases: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var alias, code string
		if err := rows.Scan(&alias, &code); err != nil {
			return nil, fmt.Errorf("storage: scan alias: %w", err)
		}
		out[alias] = code
	}
	return out, rows.Err()
}

// --- Mart read helpers (dashboard) ---

// DailySalesRow is one row of daily_sales joined with the product name.
type DailySalesRow struct {
	Date             string
	ProductCode      string
	DisplayName      string
	VolumeLiters     float64
	Revenue          float64
	TransactionCount int
}

// RecentDailySales returns sales for the last `days` calendar days
// (today included), newest first.
func (s *Storage) RecentDailySales(ctx context.Context, days int) ([]DailySalesRow, error) {
	cutoff := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
		SELECT ds.date, ds.product_code, fp.display_name,
		       ds.volume_liters, ds.revenue, ds.transaction_count
		FROM daily_sales ds
		JOIN fuel_products fp ON fp.product_code = ds.product_code
		WHERE ds.date >= ?
		ORDER BY ds.date DESC, ds.product_code ASC
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("storage: recent sales: %w", err)
	}
	defer rows.Close()
	var out []DailySalesRow
	for rows.Next() {
		var r DailySalesRow
		if err := rows.Scan(&r.Date, &r.ProductCode, &r.DisplayName,
			&r.VolumeLiters, &r.Revenue, &r.TransactionCount); err != nil {
			return nil, fmt.Errorf("storage: scan recent sales: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TodayTotals holds aggregated metrics for one day.
type TodayTotals struct {
	Date             string
	Revenue          float64
	VolumeLiters     float64
	TransactionCount int
	TopProduct       string
}

// TodayTotals returns today's totals from daily_sales (station-local date).
func (s *Storage) TodayTotals(ctx context.Context) (TodayTotals, error) {
	return s.DayTotals(ctx, time.Now().Format("2006-01-02"))
}

// DayTotals returns the totals for one date.
func (s *Storage) DayTotals(ctx context.Context, date string) (TodayTotals, error) {
	t := TodayTotals{Date: date}
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(revenue), 0),
		       COALESCE(SUM(volume_liters), 0),
		       COALESCE(SUM(transaction_count), 0)
		FROM daily_sales WHERE date = ?
	`, date).Scan(&t.Revenue, &t.VolumeLiters, &t.TransactionCount)
	if err != nil {
		return t, fmt.Errorf("storage: day totals: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT product_code FROM daily_sales
		WHERE date = ? ORDER BY revenue DESC LIMIT 1
	`, date).Scan(&t.TopProduct)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("storage: top product: %w", err)
	}
	return t, nil
}

// FuelMindScore is one row of the station_health_score table.
type FuelMindScore struct {
	Date        string
	Overall     int
	Sales       int
	Inventory   int
	Cash        int
	Credit      int
	DataQuality int
	Operations  int
	IssuesJSON  string
}

// LatestScore returns the most recent score, or the zero value if none.
func (s *Storage) LatestScore(ctx context.Context) (FuelMindScore, error) {
	var sc FuelMindScore
	err := s.db.QueryRowContext(ctx, `
		SELECT date, overall_score, sales_score, inventory_score, cash_score,
		       credit_score, data_quality_score, operations_score, issues_json
		FROM station_health_score ORDER BY date DESC LIMIT 1
	`).Scan(&sc.Date, &sc.Overall, &sc.Sales, &sc.Inventory, &sc.Cash,
		&sc.Credit, &sc.DataQuality, &sc.Operations, &sc.IssuesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return sc, nil
	}
	return sc, err
}

// CreditRow is one row of credit_outstanding.
type CreditRow struct {
	CustomerPhone     string
	AsOfDate          string
	OutstandingAmount float64
	TransactionCount  int
	DaysOverdue       int
}

// RecentCredit returns the latest credit snapshot, largest balances first.
func (s *Storage) RecentCredit(ctx context.Context, limit int) ([]CreditRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT customer_phone, as_of_date, outstanding_amount, transaction_count, days_overdue
		FROM credit_outstanding
		WHERE as_of_date = (SELECT MAX(as_of_date) FROM credit_outstanding)
		ORDER BY outstanding_amount DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: recent credit: %w", err)
	}
	defer rows.Close()
	var out []CreditRow
	for rows.Next() {
		var c CreditRow
		if err := rows.Scan(&c.CustomerPhone, &c.AsOfDate, &c.OutstandingAmount,
			&c.TransactionCount, &c.DaysOverdue); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Dashboard user / session ---

// DashboardUser is one row of dashboard_users.
type DashboardUser struct {
	ID             int64
	Username       string
	PinHash        string
	PinSalt        string
	PinIters       int
	IsActive       bool
	LastLoginAt    sql.NullString
	FailedAttempts int
	LockUntil      time.Time // zero when not locked
}

// GetDashboardUser returns the user row, or ErrNotFound.
func (s *Storage) GetDashboardUser(ctx context.Context, username string) (DashboardUser, error) {
	var u DashboardUser
	var active int
	var lock sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, pin_hash, pin_salt, pin_iters, is_active, last_login_at,
		       failed_attempts, lock_until
		FROM dashboard_users WHERE username = ?
	`, username).Scan(&u.ID, &u.Username, &u.PinHash, &u.PinSalt, &u.PinIters, &active,
		&u.LastLoginAt, &u.FailedAttempts, &lock)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.IsActive = active == 1
	if lock.Valid && lock.String != "" {
		if t, perr := time.Parse(time.RFC3339Nano, lock.String); perr == nil {
			u.LockUntil = t
		}
	}
	return u, nil
}

// SetDashboardUserPIN updates the PIN for a user and clears any lockout.
func (s *Storage) SetDashboardUserPIN(ctx context.Context, username, pinHash, pinSalt string, pinIters int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE dashboard_users
		SET pin_hash = ?, pin_salt = ?, pin_iters = ?, failed_attempts = 0, lock_until = NULL
		WHERE username = ?
	`, pinHash, pinSalt, pinIters, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordLoginFailure increments the failed-attempt counter. When it
// reaches maxAttempts the account is locked for lockFor and the counter
// resets. It returns the lock expiry (zero when not locked).
func (s *Storage) RecordLoginFailure(ctx context.Context, username string, maxAttempts int, lockFor time.Duration) (time.Time, error) {
	var attempts int
	err := s.db.QueryRowContext(ctx, `
		UPDATE dashboard_users
		SET failed_attempts = failed_attempts + 1, last_failed_at = ?
		WHERE username = ?
		RETURNING failed_attempts`, time.Now().UTC().Format(time.RFC3339Nano), username).Scan(&attempts)
	if err != nil {
		return time.Time{}, err
	}
	if attempts < maxAttempts {
		return time.Time{}, nil
	}
	until := time.Now().Add(lockFor).UTC()
	_, err = s.db.ExecContext(ctx, `
		UPDATE dashboard_users SET failed_attempts = 0, lock_until = ? WHERE username = ?`,
		until.Format(time.RFC3339Nano), username)
	return until, err
}

// RecordLoginSuccess clears the failure counter and stamps last_login_at.
func (s *Storage) RecordLoginSuccess(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE dashboard_users
		SET failed_attempts = 0, lock_until = NULL, last_login_at = CURRENT_TIMESTAMP
		WHERE username = ?`, username)
	return err
}

// CreateSession writes a new auth_sessions row and purges expired ones.
func (s *Storage) CreateSession(ctx context.Context, id string, userID int64, expiresAt time.Time, userAgent string) error {
	if err := s.purgeExpiredSessions(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_sessions (id, user_id, expires_at, user_agent)
		VALUES (?, ?, ?, ?)
	`, id, userID, expiresAt.UTC(), userAgent)
	return err
}

func (s *Storage) purgeExpiredSessions(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, expires_at FROM auth_sessions`)
	if err != nil {
		return err
	}
	var expired []string
	now := time.Now()
	for rows.Next() {
		var id string
		var exp time.Time
		if err := rows.Scan(&id, &exp); err != nil {
			rows.Close()
			return err
		}
		if now.After(exp) {
			expired = append(expired, id)
		}
	}
	rows.Close()
	for _, id := range expired {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// LookupSession returns the user_id for a session id, or
// ErrInvalidSession if expired/missing.
func (s *Storage) LookupSession(ctx context.Context, id string) (int64, error) {
	var userID int64
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, expires_at FROM auth_sessions WHERE id = ?
	`, id).Scan(&userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrInvalidSession
	}
	if err != nil {
		return 0, err
	}
	if time.Now().After(expiresAt) {
		return 0, ErrInvalidSession
	}
	return userID, nil
}

// DeleteSession removes a session (logout).
func (s *Storage) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id = ?`, id)
	return err
}

// DeleteAllSessions signs every device out (used by PIN reset).
func (s *Storage) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions`)
	return err
}

// LocalConfigRow is one row of the local_config key/value table.
type LocalConfigRow struct {
	Key          string
	Value        string
	ConfigJSON   string
	Synchronized bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// GetLocalConfig returns the row for key, or ErrNotFound.
func (s *Storage) GetLocalConfig(ctx context.Context, key string) (LocalConfigRow, error) {
	var r LocalConfigRow
	var syncFlag int
	var cfgJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT key, value, config_json, synchronized, created_at, updated_at
		FROM local_config WHERE key = ?
	`, key).Scan(&r.Key, &r.Value, &cfgJSON, &syncFlag, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.Synchronized = syncFlag == 1
	if cfgJSON.Valid {
		r.ConfigJSON = cfgJSON.String
	}
	return r, nil
}

// LocalConfigValue returns the value for key, or def when absent.
func (s *Storage) LocalConfigValue(ctx context.Context, key, def string) string {
	row, err := s.GetLocalConfig(ctx, key)
	if err != nil || row.Value == "" {
		return def
	}
	return row.Value
}

// SetLocalConfig upserts a key/value pair.
func (s *Storage) SetLocalConfig(ctx context.Context, key, value, configJSON string, synchronized bool) error {
	syncFlag := 0
	if synchronized {
		syncFlag = 1
	}
	var cfgJSONArg any
	if configJSON != "" {
		cfgJSONArg = configJSON
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO local_config (key, value, config_json, synchronized, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			config_json = excluded.config_json,
			synchronized = excluded.synchronized,
			updated_at = CURRENT_TIMESTAMP
	`, key, value, cfgJSONArg, syncFlag)
	return err
}

// DeleteLocalConfig removes a row.
func (s *Storage) DeleteLocalConfig(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM local_config WHERE key = ?`, key)
	return err
}

// RecordSyncAttempt writes one row to sync_log.
func (s *Storage) RecordSyncAttempt(ctx context.Context, status, reason string, httpStatus, roundTripMs int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_log (status, reason, http_status, round_trip_ms)
		VALUES (?, ?, ?, ?)
	`, status, reason, httpStatus, roundTripMs)
	return err
}

// SyncAttempt is one sync_log row.
type SyncAttempt struct {
	SentAt      time.Time
	Status      string
	Reason      string
	HTTPStatus  int
	RoundTripMs int
}

// LatestSyncAttempt returns the most recent sync_log row, or ErrNotFound.
func (s *Storage) LatestSyncAttempt(ctx context.Context) (SyncAttempt, error) {
	var a SyncAttempt
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT sent_at, status, reason, http_status, round_trip_ms
		FROM sync_log ORDER BY id DESC LIMIT 1
	`).Scan(&a.SentAt, &a.Status, &reason, &a.HTTPStatus, &a.RoundTripMs)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Reason = reason.String
	return a, nil
}

// DBSize returns the logical size of the database in MiB.
func (s *Storage) DBSize(ctx context.Context) (int64, error) {
	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pageCount * pageSize) / (1024 * 1024), nil
}

// DiskFreeGB returns the free space in GiB on the volume holding the
// database (platform-specific; see disk_*.go).
func (s *Storage) DiskFreeGB(ctx context.Context) (int64, error) {
	path, err := s.Path(ctx)
	if err != nil {
		return 0, err
	}
	dir := "."
	if path != "" {
		dir = filepath.Dir(path)
	}
	free, err := diskFreeBytes(dir)
	if err != nil {
		return 0, fmt.Errorf("storage: disk free: %w", err)
	}
	return int64(free / (1024 * 1024 * 1024)), nil
}

// LastPosIngestionAt returns when the most recent raw POS row arrived
// (UTC). Zero time when nothing has been ingested yet.
func (s *Storage) LastPosIngestionAt(ctx context.Context) (time.Time, error) {
	var ts sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM raw_pos_transactions`).Scan(&ts); err != nil {
		return time.Time{}, err
	}
	if !ts.Valid || ts.String == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, ts.String); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("storage: unparseable received_at %q", ts.String)
}

// ActiveAlertsCount returns the number of severity=alert issues in the
// most recent FuelMind Score.
func (s *Storage) ActiveAlertsCount(ctx context.Context) (int, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT issues_json FROM station_health_score ORDER BY date DESC LIMIT 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var issues []struct {
		Severity string `json:"severity"`
	}
	if err := json.Unmarshal([]byte(raw.String), &issues); err != nil {
		return 0, nil // best-effort
	}
	n := 0
	for _, it := range issues {
		if it.Severity == "alert" {
			n++
		}
	}
	return n, nil
}

// ErrorsLast24h counts failed sync attempts in the last 24 hours plus raw
// rows that could not be normalized in that window.
func (s *Storage) ErrorsLast24h(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM sync_log
		        WHERE status = 'failed' AND sent_at >= datetime('now', '-24 hours'))
		     + (SELECT COUNT(*) FROM raw_normalize_errors
		        WHERE last_attempt_at >= datetime('now', '-24 hours'))
	`).Scan(&n)
	return n, err
}

// UnresolvedProduct is a product name the POS used that has no alias.
type UnresolvedProduct struct {
	Alias string
	Rows  int
}

// UnresolvedProducts lists product names in raw rows that could not be
// normalized, most frequent first.
func (s *Storage) UnresolvedProducts(ctx context.Context) ([]UnresolvedProduct, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(u.product_alias, ''), COUNT(*)
		FROM raw_unresolved u
		JOIN raw_normalize_errors e ON e.raw_transaction_id = u.id
		WHERE e.error LIKE 'unknown product%'
		GROUP BY 1 ORDER BY 2 DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnresolvedProduct
	for rows.Next() {
		var p UnresolvedProduct
		if err := rows.Scan(&p.Alias, &p.Rows); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UnreadableRows counts the raw rows that failed to normalize for a
// reason other than an unknown product — a quantity that is not a
// number, a missing price — and returns the most common reason.
//
// These used to be reported as unrecognised product names, which sent
// the owner looking for a fuel that was never the problem.
func (s *Storage) UnreadableRows(ctx context.Context) (count int, reason string, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(e.error), '')
		FROM raw_unresolved u
		JOIN raw_normalize_errors e ON e.raw_transaction_id = u.id
		WHERE e.error NOT LIKE 'unknown product%'`)
	if err := row.Scan(&count, &reason); err != nil {
		return 0, "", fmt.Errorf("storage: unreadable rows: %w", err)
	}
	return count, reason, nil
}
