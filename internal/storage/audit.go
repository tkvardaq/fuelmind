package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Roles a dashboard user can have.
//
// The split is deliberately narrow, because a petrol station is a small
// place: the owner runs it, and staff record what happened during their
// shift. Anything that changes how the station is configured — where
// data comes from, what is paid per litre, who has an account, whether
// answers go out over WhatsApp — belongs to the owner.
const (
	RoleOwner = "owner"
	RoleStaff = "staff"
)

// ValidRole reports whether r is a role FuelMind knows.
func ValidRole(r string) bool { return r == RoleOwner || r == RoleStaff }

// RoleLabel is the role as the owner should read it.
func RoleLabel(r string) string {
	if r == RoleOwner {
		return "Owner"
	}
	return "Staff"
}

// User is one person who can sign in to the dashboard.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Role        string
	IsActive    bool
	CreatedAt   time.Time
	LastLoginAt time.Time
	PINSet      bool
}

// IsOwner reports whether this user may change the station's setup.
func (u User) IsOwner() bool { return u.Role == RoleOwner }

// Name is what to show for this user, falling back to the username so a
// row is never nameless on screen.
func (u User) Name() string {
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Username
}

// ErrUserExists is returned when a username is already taken.
var ErrUserExists = errors.New("storage: that name is already in use")

// ErrLastOwner is returned when removing or demoting a user would leave
// the station with nobody who can administer it.
var ErrLastOwner = errors.New("storage: this is the only owner account")

// Users lists everyone who can sign in, owners first.
func (s *Storage) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, COALESCE(display_name, ''), role, is_active,
		       created_at, last_login_at, pin_hash <> '' AS pin_set
		FROM dashboard_users
		ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END, LOWER(COALESCE(display_name, username))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ActiveUsers lists the people who can sign in right now. The login page
// shows these, so a disabled account cannot even be attempted.
func (s *Storage) ActiveUsers(ctx context.Context) ([]User, error) {
	all, err := s.Users(ctx)
	if err != nil {
		return nil, err
	}
	var out []User
	for _, u := range all {
		if u.IsActive && u.PINSet {
			out = append(out, u)
		}
	}
	return out, nil
}

// UserByID returns one user.
func (s *Storage) UserByID(ctx context.Context, id int64) (User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, COALESCE(display_name, ''), role, is_active,
		       created_at, last_login_at, pin_hash <> '' AS pin_set
		FROM dashboard_users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

type scanner interface{ Scan(dest ...any) error }

func scanUser(sc scanner) (User, error) {
	var u User
	var active, pinSet int
	var lastLogin sql.NullTime
	if err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &active,
		&u.CreatedAt, &lastLogin, &pinSet); err != nil {
		return User{}, err
	}
	u.IsActive, u.PINSet = active == 1, pinSet == 1
	if lastLogin.Valid {
		u.LastLoginAt = lastLogin.Time
	}
	return u, nil
}

// CreateUser adds a person. The PIN is set separately, through the auth
// package, which owns the hashing.
func (s *Storage) CreateUser(ctx context.Context, username, displayName, role string) (int64, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return 0, errors.New("storage: a user needs a sign-in name")
	}
	if !ValidRole(role) {
		return 0, fmt.Errorf("storage: %q is not a role", role)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO dashboard_users (username, display_name, role, pin_hash, pin_salt, pin_iters)
		VALUES (?, ?, ?, '', '', 0)`,
		username, strings.TrimSpace(displayName), role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrUserExists
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUser changes a person's name, role or whether they can sign in.
// It refuses to leave the station without an active owner.
func (s *Storage) UpdateUser(ctx context.Context, id int64, displayName, role string, active bool) error {
	if !ValidRole(role) {
		return fmt.Errorf("storage: %q is not a role", role)
	}
	if role != RoleOwner || !active {
		last, err := s.isLastActiveOwner(ctx, id)
		if err != nil {
			return err
		}
		if last {
			return ErrLastOwner
		}
	}
	v := 0
	if active {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE dashboard_users SET display_name = ?, role = ?, is_active = ? WHERE id = ?`,
		strings.TrimSpace(displayName), role, v, id)
	return err
}

// DeleteUser removes a person. Their entries in the audit log stay: the
// log records what was done and by whom at the time, and removing an
// account must not rewrite history.
func (s *Storage) DeleteUser(ctx context.Context, id int64) error {
	last, err := s.isLastActiveOwner(ctx, id)
	if err != nil {
		return err
	}
	if last {
		return ErrLastOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Signing them out everywhere is part of removing them.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_log SET user_id = NULL WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dashboard_users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// isLastActiveOwner reports whether id is the only owner who can sign in.
func (s *Storage) isLastActiveOwner(ctx context.Context, id int64) (bool, error) {
	var target string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM dashboard_users WHERE id = ?`, id).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if target != RoleOwner {
		return false, nil
	}
	var others int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dashboard_users WHERE role = 'owner' AND is_active = 1 AND id <> ?`, id).Scan(&others)
	return others == 0, err
}

// DeleteSessionsForUser signs one person out everywhere, which is what
// changing their PIN or disabling them has to do.
func (s *Storage) DeleteSessionsForUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id = ?`, id)
	return err
}

// --- the audit log ----------------------------------------------------

// Audit actions. They are stable slugs so the Activity page can group and
// filter on them without matching on English text.
const (
	ActionLogin            = "auth.login"
	ActionLoginFailed      = "auth.login_failed"
	ActionLogout           = "auth.logout"
	ActionPINChanged       = "auth.pin_changed"
	ActionPINResetAsked    = "auth.pin_reset_asked"
	ActionPINResetDone     = "auth.pin_reset_done"
	ActionPINResetDenied   = "auth.pin_reset_denied"
	ActionUserAdded        = "user.added"
	ActionUserChanged      = "user.changed"
	ActionUserRemoved      = "user.removed"
	ActionSaleEntered      = "sale.manual"
	ActionFileImported     = "data.import"
	ActionFolderAdded      = "data.folder_added"
	ActionFolderChanged    = "data.folder_changed"
	ActionFolderRemoved    = "data.folder_removed"
	ActionPriceSet         = "price.set"
	ActionCreditPayment    = "credit.payment"
	ActionSettingChanged   = "settings.changed"
	ActionMessagingChanged = "messaging.changed"
	ActionModelChanged     = "model.changed"
	ActionQuestionAnswered = "question.answered"
)

// Audit sources.
const (
	AuditSourceDashboard = "dashboard"
	AuditSourceWhatsApp  = "whatsapp"
	AuditSourceService   = "service"
)

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID         int64
	OccurredAt time.Time
	UserID     int64
	ActorName  string
	ActorRole  string
	Source     string
	Action     string
	Subject    string
	Detail     string
	RemoteAddr string
}

// RecordAudit appends one entry. A failure here is returned but callers
// treat it as non-fatal: losing the audit line must never undo the change
// the owner just made, though it must always be logged.
func (s *Storage) RecordAudit(ctx context.Context, e AuditEntry) error {
	if e.Source == "" {
		e.Source = AuditSourceDashboard
	}
	if e.ActorName == "" {
		e.ActorName = "unknown"
	}
	var userID any
	if e.UserID > 0 {
		userID = e.UserID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log
			(user_id, actor_name, actor_role, source, action, subject, detail, remote_addr)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, e.ActorName, e.ActorRole, e.Source, e.Action,
		nullableString(e.Subject), nullableString(e.Detail), nullableString(e.RemoteAddr))
	return err
}

// AuditFilter narrows what the Activity page shows.
type AuditFilter struct {
	// Action matches one action slug exactly; empty means every action.
	Action string
	// UserID matches one person; zero means everyone.
	UserID int64
	// Since limits to entries after this time; zero means no limit.
	Since time.Time
	Limit int
}

// RecentAudit returns the newest entries first.
func (s *Storage) RecentAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	// The clauses are built from fixed strings; every value is bound.
	clauses := []string{"1 = 1"}
	args := []any{}
	if f.Action != "" {
		clauses = append(clauses, "action = ?")
		args = append(args, f.Action)
	}
	if f.UserID > 0 {
		clauses = append(clauses, "user_id = ?")
		args = append(args, f.UserID)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "occurred_at >= ?")
		args = append(args, f.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, occurred_at, COALESCE(user_id, 0), actor_name, actor_role,
		       source, action, COALESCE(subject, ''), COALESCE(detail, ''),
		       COALESCE(remote_addr, '')
		FROM audit_log
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.UserID, &e.ActorName, &e.ActorRole,
			&e.Source, &e.Action, &e.Subject, &e.Detail, &e.RemoteAddr); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditActions lists the action slugs that actually appear in the log,
// so the Activity page's filter only offers what there is something to
// see for.
func (s *Storage) AuditActions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT action FROM audit_log ORDER BY action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
