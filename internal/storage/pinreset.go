package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// PIN reset request states.
const (
	PINResetPending   = "pending"
	PINResetDone      = "done"
	PINResetDismissed = "dismissed"
)

// PINResetRequest is one person saying they cannot get in.
//
// It carries no authority whatsoever: raising one does not change a PIN,
// does not sign anyone in, and does not reveal anything. An owner who is
// already signed in has to act on it. That asymmetry is the whole design
// — it makes the request safe to expose to anyone who can reach the
// login page, which is exactly who needs it.
type PINResetRequest struct {
	ID          int64
	UserID      int64
	Username    string
	DisplayName string
	Role        string
	RequestedAt time.Time
	RemoteAddr  string
	Note        string
	Status      string
	ResolvedAt  time.Time
	ResolvedBy  string
}

// Name is what to show for the person who asked.
func (r PINResetRequest) Name() string {
	if strings.TrimSpace(r.DisplayName) != "" {
		return r.DisplayName
	}
	return r.Username
}

// ErrResetAlreadyRequested means this person already has one waiting.
var ErrResetAlreadyRequested = errors.New("storage: a PIN reset has already been requested for that account")

// RequestPINReset records that someone cannot sign in. It is deliberately
// tolerant: asking twice is not an error the person needs to see, it just
// does not add a second row.
func (s *Storage) RequestPINReset(ctx context.Context, username, remoteAddr, note string) error {
	username = strings.ToLower(strings.TrimSpace(username))
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM dashboard_users WHERE username = ? AND is_active = 1`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(note) > 200 {
		note = note[:200]
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO pin_reset_requests (user_id, remote_addr, note, status)
		VALUES (?, ?, ?, 'pending')`,
		id, nullableString(remoteAddr), nullableString(strings.TrimSpace(note)))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrResetAlreadyRequested
	}
	return err
}

// PendingPINResets lists the requests an owner has not dealt with.
func (s *Storage) PendingPINResets(ctx context.Context) ([]PINResetRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.user_id, u.username, COALESCE(u.display_name, ''), u.role,
		       r.requested_at, COALESCE(r.remote_addr, ''), COALESCE(r.note, ''), r.status
		FROM pin_reset_requests r
		JOIN dashboard_users u ON u.id = r.user_id
		WHERE r.status = 'pending'
		ORDER BY r.requested_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PINResetRequest
	for rows.Next() {
		var q PINResetRequest
		if err := rows.Scan(&q.ID, &q.UserID, &q.Username, &q.DisplayName, &q.Role,
			&q.RequestedAt, &q.RemoteAddr, &q.Note, &q.Status); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// HasPendingPINReset reports whether this person already asked, so the
// login page can say "your request is waiting" instead of pretending
// nothing happened.
func (s *Storage) HasPendingPINReset(ctx context.Context, username string) bool {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pin_reset_requests r
		JOIN dashboard_users u ON u.id = r.user_id
		WHERE u.username = ? AND r.status = 'pending'`,
		strings.ToLower(strings.TrimSpace(username))).Scan(&n)
	return err == nil && n > 0
}

// ResolvePINResets closes every pending request for one person. It is
// called when an owner sets them a new PIN, and when an owner dismisses
// a request they believe was not genuine.
func (s *Storage) ResolvePINResets(ctx context.Context, userID, resolvedBy int64, status string) error {
	if status != PINResetDone && status != PINResetDismissed {
		return errors.New("storage: a PIN reset is resolved as done or dismissed")
	}
	var by any
	if resolvedBy > 0 {
		by = resolvedBy
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE pin_reset_requests
		SET status = ?, resolved_at = CURRENT_TIMESTAMP, resolved_by = ?
		WHERE user_id = ? AND status = 'pending'`, status, by, userID)
	return err
}

// CountRecentPINResets counts requests from one address in a window, so
// the login page can refuse to be used as a way to fill the owner's
// screen with noise.
func (s *Storage) CountRecentPINResets(ctx context.Context, remoteAddr string, within time.Duration) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pin_reset_requests
		WHERE remote_addr = ? AND requested_at >= ?`,
		remoteAddr, time.Now().Add(-within).UTC().Format("2006-01-02 15:04:05")).Scan(&n)
	return n, err
}
