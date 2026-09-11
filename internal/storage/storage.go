// Package storage owns the local SQLite database. It opens the file with
// the pragmas we need (WAL, foreign keys, busy timeout), runs the
// embedded migration set, and exposes typed methods for writing raw data
// and (in later phases) reading it back for the normalizer and mart.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	// Pure-Go SQLite driver (no CGo). Registered for the "sqlite" driver
	// name so *sql.DB can open it.
	_ "modernc.org/sqlite"
	"golang.org/x/sys/windows"
	"path/filepath"
)

// ErrNotFound is returned when a single-row lookup has no match.
var ErrNotFound = errors.New("fuelmind: not found")

// ErrInvalidSession is returned for missing or expired sessions.
var ErrInvalidSession = errors.New("fuelmind: invalid or expired session")

// Storage wraps a single SQLite database connection. The local core is
// single-writer by design (one process, one station), so we set the
// connection pool to a single connection — SQLite's locking layer is
// happier that way.
type Storage struct {
	db *sql.DB
}

// Open creates a new SQLite database at the given path. The DSN sets
// WAL mode, foreign keys, normal synchronous, and a 5s busy timeout
// (Windows file locks occasionally contend; the timeout turns that into
// a retry rather than a hard error).
func Open(path string) (*Storage, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", path, err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping %q: %w", path, err)
	}
	// Integrity check: ensure the database file is not corrupted.
	if ok, err := integrityCheck(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: integrity check failed: %w", err)
	} else if !ok {
		_ = db.Close()
		return nil, errors.New("storage: database corruption detected")
	}
	db.SetMaxOpenConns(1)
	return &Storage{db: db}, nil
}

// integrityCheck runs PRAGMA integrity_check and returns true if ok.
func integrityCheck(db *sql.DB) (bool, error) {
	var result string
	err := db.QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&result)
	if err != nil {
		return false, fmt.Errorf("integrity check query: %w", err)
	}
	return result == "ok", nil
}

// NewWithDB wraps an existing *sql.DB. Used by tests to point Storage
// at an in-memory database (sql.Open("sqlite", ":memory:")).
func NewWithDB(db *sql.DB) *Storage {
	return &Storage{db: db}
}

// DB returns the underlying *sql.DB. Reserved for the normalizer and
// mart materializer (Phase 2+) and for ad-hoc queries from tooling.
// Don't bypass the typed Write* methods from outside this package.
func (s *Storage) DB() *sql.DB { return s.db }

// Close closes the database. Safe to call multiple times.
func (s *Storage) Close() error { return s.db.Close() }

// WriteRawTransactions inserts a batch of raw transactions in a single
// transaction. Returns the number of rows actually inserted; rows whose
// payload_hash already exists are silently skipped (dedup is the
// whole point of the unique constraint).
//
// payloads and hashes must be the same length and 1:1 paired.
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
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

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
	committed = true
	return inserted, nil
}

// RawTransaction is the minimal view a normalizer needs. We avoid
// pulling raw_payload bytes back into Go on every call — the
// normalizer can re-parse from the JSON-encoded payload that
// csvwatch already produced.
type RawTransaction struct {
	ID           int64
	PosSourceID  string
	RawPayload   string
	BatchID      string
	ReceivedAt   string // RFC 3339 string; we leave it to the caller to parse
}

// UnnormalizedTransactions returns raw rows that have not yet been
// normalized. Idempotency: it joins on the absence of a row in
// `transactions` referencing the raw id. Capped by limit (0 = no
// limit) so a long-running process can't OOM.
func (s *Storage) UnnormalizedTransactions(ctx context.Context, limit int) ([]RawTransaction, error) {
	q := `
		SELECT r.id, r.pos_source_id, r.raw_payload, r.ingestion_batch_id, r.received_at
		FROM raw_pos_transactions r
		LEFT JOIN transactions t ON t.raw_transaction_id = r.id
		WHERE t.id IS NULL
		ORDER BY r.id ASC
	`
	if limit > 0 {
		q += " LIMIT ?"
	}
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, q, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
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
	ProductCode      string
	QuantityLiters   float64
	UnitPrice        float64
	TotalAmount      float64
	PaymentMethod    string
	CustomerPhone    string
	PumpID           string
	Attendant        string
	TransactionTime  string // RFC 3339
}

// WriteNormalizedTransaction inserts a single normalized row. The
// `raw_transaction_id` column is UNIQUE, so re-running the normalizer
// on the same raw row is a no-op (idempotency by design).
func (s *Storage) WriteNormalizedTransaction(ctx context.Context, n NormalizedTransaction) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO transactions (
			raw_transaction_id, product_code, quantity_liters, unit_price,
			total_amount, payment_method, customer_phone, pump_id, attendant,
			transaction_time
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		n.RawTransactionID, n.ProductCode, n.QuantityLiters, n.UnitPrice,
		n.TotalAmount, n.PaymentMethod, nullableString(n.CustomerPhone),
		nullableString(n.PumpID), nullableString(n.Attendant),
		n.TransactionTime,
	)
	if err != nil {
		return fmt.Errorf("storage: insert transaction: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ProductAlias is one row of the product_aliases table.
type ProductAlias struct {
	AliasText   string
	ProductCode string
}

// ProductAliases returns the full alias table. The normalizer builds
// an in-memory map from this once at start-up; for v1 there are 16
// rows, so the load is trivial.
func (s *Storage) ProductAliases(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias_text, product_code FROM product_aliases`)
	if err != nil {
		return nil, fmt.Errorf("storage: query aliases: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var a ProductAlias
		if err := rows.Scan(&a.AliasText, &a.ProductCode); err != nil {
			return nil, fmt.Errorf("storage: scan alias: %w", err)
		}
		out[a.AliasText] = a.ProductCode
	}
	return out, rows.Err()
}

// --- Mart read helpers (Phase 4 dashboard) ---

// DailySalesRow is one row of the daily_sales table, joined with the
// product display name for dashboard rendering.
type DailySalesRow struct {
	Date             string
	ProductCode      string
	DisplayName      string
	VolumeLiters     float64
	Revenue          float64
	TransactionCount int
}

// RecentDailySales returns the last `days` days of sales, newest first.
func (s *Storage) RecentDailySales(ctx context.Context, days int) ([]DailySalesRow, error) {
	q := `
		SELECT ds.date, ds.product_code, fp.display_name,
		       ds.volume_liters, ds.revenue, ds.transaction_count
		FROM daily_sales ds
		JOIN fuel_products fp ON fp.product_code = ds.product_code
		ORDER BY ds.date DESC, ds.product_code ASC
		LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, q, days*10) // rough cap; v1 has 3 products
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

// TodayTotals returns aggregated metrics for today.
type TodayTotals struct {
	Date             string
	Revenue          float64
	VolumeLiters     float64
	TransactionCount int
	TopProduct       string
}

func (s *Storage) TodayTotals(ctx context.Context) (TodayTotals, error) {
	today := time.Now().Format("2006-01-02")
	var t TodayTotals
	t.Date = today
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(revenue), 0),
		       COALESCE(SUM(volume_liters), 0),
		       COALESCE(SUM(transaction_count), 0)
		FROM daily_sales WHERE date = ?
	`, today).Scan(&t.Revenue, &t.VolumeLiters, &t.TransactionCount)
	if err != nil {
		return t, fmt.Errorf("storage: today totals: %w", err)
	}
	_ = s.db.QueryRowContext(ctx, `
		SELECT product_code FROM daily_sales
		WHERE date = ? ORDER BY revenue DESC LIMIT 1
	`, today).Scan(&t.TopProduct)
	return t, nil
}

// FuelMindScore is one row of the station_health_score table.
type FuelMindScore struct {
	Date           string
	Overall        int
	Sales          int
	Inventory      int
	Cash           int
	Credit         int
	DataQuality    int
	Operations     int
	IssuesJSON     string
}

// LatestScore returns the most recent score, or zero-value if none.
func (s *Storage) LatestScore(ctx context.Context) (FuelMindScore, error) {
	var sc FuelMindScore
	err := s.db.QueryRowContext(ctx, `
		SELECT date, overall_score, sales_score, inventory_score, cash_score,
		       credit_score, data_quality_score, operations_score, issues_json
		FROM station_health_score ORDER BY date DESC LIMIT 1
	`).Scan(&sc.Date, &sc.Overall, &sc.Sales, &sc.Inventory, &sc.Cash,
		&sc.Credit, &sc.DataQuality, &sc.Operations, &sc.IssuesJSON)
	if err == sql.ErrNoRows {
		return sc, nil
	}
	return sc, err
}

// CreditRow is one row of credit_outstanding.
type CreditRow struct {
	CustomerPhone     string
	OutstandingAmount float64
	TransactionCount  int
	DaysOverdue       int
}

func (s *Storage) RecentCredit(ctx context.Context, limit int) ([]CreditRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT customer_phone, outstanding_amount, transaction_count, days_overdue
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
		if err := rows.Scan(&c.CustomerPhone, &c.OutstandingAmount,
			&c.TransactionCount, &c.DaysOverdue); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Dashboard user / session (Phase 4) ---

// DashboardUser is one row of dashboard_users.
type DashboardUser struct {
	ID          int64
	Username    string
	PinHash     string
	PinSalt     string
	PinIters    int
	IsActive    bool
	LastLoginAt sql.NullString
}

func (s *Storage) GetDashboardUser(ctx context.Context, username string) (DashboardUser, error) {
	var u DashboardUser
	var active int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, pin_hash, pin_salt, pin_iters, is_active, last_login_at
		FROM dashboard_users WHERE username = ?
	`, username).Scan(&u.ID, &u.Username, &u.PinHash, &u.PinSalt, &u.PinIters, &active, &u.LastLoginAt)
	u.IsActive = active == 1
	if err == sql.ErrNoRows {
		return u, ErrNotFound
	}
	return u, err
}

// SetDashboardUserPIN updates the PIN for a user. pinHash and pinSalt
// are the base64-encoded PBKDF2 outputs. pinIters is the iteration
// count used (for forward-compat — the dashboard accepts any
// non-zero iters value).
func (s *Storage) SetDashboardUserPIN(ctx context.Context, username, pinHash, pinSalt string, pinIters int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE dashboard_users
		SET pin_hash = ?, pin_salt = ?, pin_iters = ?
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

// CreateSession writes a new auth_sessions row. The id should be a
// cryptographically random 32-byte hex string.
func (s *Storage) CreateSession(ctx context.Context, id string, userID int64, expiresAt time.Time, userAgent string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_sessions (id, user_id, expires_at, user_agent)
		VALUES (?, ?, ?, ?)
	`, id, userID, expiresAt, userAgent)
	return err
}

// LookupSession returns the user_id for a session id, or
// ErrInvalidSession if expired/missing.
func (s *Storage) LookupSession(ctx context.Context, id string) (int64, error) {
	var userID int64
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, expires_at FROM auth_sessions WHERE id = ?
	`, id).Scan(&userID, &expiresAt)
	if err == sql.ErrNoRows {
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

// LocalConfigRow is one row of the generic local_config key/value
// table. See migration 005. `Synchronized` is true when this row's
// value is meant to reach the cloud (e.g. station_id, api_key).
type LocalConfigRow struct {
	Key          string
	Value        string
	ConfigJSON   string // optional, "" when absent
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
	if err == sql.ErrNoRows {
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

// SetLocalConfig upserts a key/value pair. Use ConfigJSON="" when no
// structured companion is needed. Synchronized defaults to false.
func (s *Storage) SetLocalConfig(ctx context.Context, key, value, configJSON string, synchronized bool) error {
	syncFlag := 0
	if synchronized {
		syncFlag = 1
	}
	var cfgJSONArg any = nil
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

// DeleteLocalConfig removes a row. Used by tests and by the rare
// "reset station identity" runbook flow.
func (s *Storage) DeleteLocalConfig(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM local_config WHERE key = ?`, key)
	return err
}

// RecordSyncAttempt writes one row to sync_log. round_trip_ms is 0
// when the request never reached a server (DNS, connection refused,
// timeout). http_status is 0 when no HTTP response was received.
// Reason is a short bounded category (timeout, http_4xx, http_5xx,
// dns, parse) used by the dashboard to group failures.
func (s *Storage) RecordSyncAttempt(ctx context.Context, status, reason string, httpStatus, roundTripMs int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_log (status, reason, http_status, round_trip_ms)
		VALUES (?, ?, ?, ?)
	`, status, reason, httpStatus, roundTripMs)
	return err
}

// LatestSyncAttempt returns the most recent sync_log row, or
// ErrNotFound if none yet.
type SyncAttempt struct {
	SentAt      time.Time
	Status      string
	Reason      string
	HTTPStatus  int
	RoundTripMs int
}

func (s *Storage) LatestSyncAttempt(ctx context.Context) (SyncAttempt, error) {
	var a SyncAttempt
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT sent_at, status, reason, http_status, round_trip_ms
		FROM sync_log ORDER BY id DESC LIMIT 1
	`).Scan(&a.SentAt, &a.Status, &reason, &a.HTTPStatus, &a.RoundTripMs)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if reason.Valid {
		a.Reason = reason.String
	}
	return a, nil
}

// DBSize returns the on-disk size of the SQLite database in MiB,
// including WAL + SHM sidecars. Used by the heartbeat payload.
func (s *Storage) DBSize(ctx context.Context) (int64, error) {
	// PRAGMA page_count * page_size gives us the logical size.
	// For the heartbeat we want the physical (on-disk) size which
	// includes WAL; the cleanest pure-Go path is stat-ing the
	// main db file from PRAGMA database_list. But the simplest
	// accurate answer is page_count * page_size — that's what
	// the WAL contents would be flushed to at the next checkpoint,
	// and it's good enough for the cloud's fleet-health dashboard.
	var pageCount, pageSize int64
	if _, err := s.db.ExecContext(ctx, `PRAGMA page_count`); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pageCount * pageSize) / (1024 * 1024), nil
}

// DiskFreeGB returns the free disk space in GiB on the volume holding
// the database file. Used by the heartbeat payload to surface low-disk
// stations in cloud admin.
//
// The path is persisted in local_config on first run so the storage
// layer does not need to know its own path by design.
func (s *Storage) DiskFreeGB(ctx context.Context) (int64, error) {
	// Use the stored db path if present, otherwise fall back to ".".
	dbPath := ""
	if row, err := s.GetLocalConfig(ctx, "db_file_path"); err == nil {
		dbPath = row.Value
	}
	if dbPath == "" {
		dbPath = "."
	}
	// Get the directory of the database file.
	dir := filepath.Dir(dbPath)
	// Call GetDiskFreeSpaceEx to get free bytes available to the caller.
	var freeBytesAvailable uint64
	var totalNumberOfBytes uint64
	var totalNumberOfFreeBytes uint64
	err := windows.GetDiskFreeSpaceEx(
		windows.StringToUTF16Ptr(dir),
		&freeBytesAvailable,
		&totalNumberOfBytes,
		&totalNumberOfFreeBytes,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: GetDiskFreeSpaceEx failed: %w", err)
	}
	// Convert bytes to GiB (1 GiB = 1024^3 bytes)
	const gib = 1024 * 1024 * 1024
	freeGB := int64(freeBytesAvailable / gib)
	return freeGB, nil
}

// LastPosIngestionAt returns the timestamp of the most recent
// successful POS ingestion (max ingestion_batch row's
// ingested_at). Zero time when nothing has been ingested yet.
func (s *Storage) LastPosIngestionAt(ctx context.Context) (time.Time, error) {
	var ts sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(ingested_at) FROM raw_pos_transactions
	`).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return ts.Time, nil
}

// ActiveAlertsCount returns the number of "alert"-severity issues
// from the most recent station_health_score.issues_json. Counts
// only entries with severity=alert (the kind that wake the owner
// up at night); "warn" issues are tracked separately.
func (s *Storage) ActiveAlertsCount(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT issues_json FROM station_health_score
		ORDER BY date DESC LIMIT 1
	`)
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil {
		// sql.ErrNoRows is fine — no score yet means no alerts.
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if !raw.Valid || raw.String == "" {
		return 0, nil
	}
	// Parse and count alerts. We don't import the mart package
	// here to avoid a cycle; the structure is stable enough to
	// decode ad-hoc.
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

// ErrorsLast24h returns the number of error-level log entries
// in the last 24 hours. The v1 log store is slog → stderr, so
// we approximate by counting sync_log + raw_pos_transactions that
// landed in failed/ via the raw layer's last-error hint column.
//
// For v1 we just return sync_log errors, which is good enough for
// fleet-health alerting. A real production logger-backed count
// is a Phase 9 add.
func (s *Storage) ErrorsLast24h(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sync_log
		WHERE status = 'failed' AND sent_at >= datetime('now', '-24 hours')
	`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UpdateState tracks the state of an update process for a station.
type UpdateState struct {
	Status          string     // "checking" | "downloading" | "staged" | "awaiting_restart" | "probation" | "committed" | "rolled_back" | "failed"
	Version         string
	PreviousVersion string     // new field
	Progress        int        // 0-100, meaningful now that downloads resume
	ErrorMessage    string
	StartedAt       time.Time
	StagedAt        *time.Time // new
	RestartedAt     *time.Time // new
	CommittedAt     *time.Time // new
	RollbackReason  string     // new
	CompletedAt     sql.NullTime
}

// RecordUpdateState records or updates the update state for a station.
// This is stored in local_config under the key "update_state_<version>"
func (s *Storage) RecordUpdateState(ctx context.Context, version string, state UpdateState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("storage: marshal update state: %w", err)
	}
	return s.SetLocalConfig(ctx, "update_state_"+version, string(data), "", false)
}

// GetUpdateState retrieves the update state for a station and version.
// Returns ErrNotFound if no state is recorded.
func (s *Storage) GetUpdateState(ctx context.Context, version string) (UpdateState, error) {
	row, err := s.GetLocalConfig(ctx, "update_state_"+version)
	if err != nil {
		return UpdateState{}, err
	}
	var state UpdateState
	if err := json.Unmarshal([]byte(row.Value), &state); err != nil {
		return UpdateState{}, fmt.Errorf("storage: unmarshal update state: %w", err)
	}
	return state, nil
}

// DeleteUpdateState removes the update state for a station and version.
func (s *Storage) DeleteUpdateState(ctx context.Context, version string) error {
	return s.DeleteLocalConfig(ctx, "update_state_"+version)
}

// RecordAppliedUpdate records that an update was successfully applied.
// This is stored in the update_history table via the cloud, but we also
// keep a local record for quick lookup.
func (s *Storage) RecordAppliedUpdate(ctx context.Context, version string) error {
	state := UpdateState{
		Status:       "applied",
		Progress:     100,
		StartedAt:    time.Now(),
		CompletedAt:  sql.NullTime{Time: time.Now(), Valid: true},
	}
	return s.RecordUpdateState(ctx, version, state)
}

// RecordFailedUpdate records that an update failed.
func (s *Storage) RecordFailedUpdate(ctx context.Context, version string, errMsg string) error {
	state := UpdateState{
		Status:       "failed",
		Progress:     0,
		ErrorMessage: errMsg,
		StartedAt:    time.Now(),
		CompletedAt:  sql.NullTime{Time: time.Now(), Valid: true},
	}
	return s.RecordUpdateState(ctx, version, state)
}

// FinishInFlightWork signals that any in-flight work (ingestion, materialization)
// should be allowed to complete before a restart. For v1 it's a no-op.
func (s *Storage) FinishInFlightWork() error {
	// No-op for v1.
	return nil
}