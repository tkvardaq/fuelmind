package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Message is one message the station wants to send to its owner, and
// what happened when it tried.
type Message struct {
	ID        int64
	Kind      string
	DedupKey  string
	Channel   string
	Recipient string
	Body      string
	Status    string
	Attempts  int
	LastError string
	CreatedAt time.Time
	SentAt    time.Time
}

// Message statuses.
const (
	MessageQueued = "queued"
	MessageSent   = "sent"
	MessageFailed = "failed"
)

// ErrDuplicateMessage means this exact message (kind + day) was already
// queued. Callers treat it as "nothing to do", not as a failure.
var ErrDuplicateMessage = errors.New("storage: message already queued")

// QueueMessage adds a message to the outbox. A second call with the same
// dedup key returns ErrDuplicateMessage and queues nothing, so an evening
// summary survives a restart without being sent twice.
func (s *Storage) QueueMessage(ctx context.Context, m Message) (int64, error) {
	if m.Channel == "" {
		m.Channel = "whatsapp"
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO messages (kind, dedup_key, channel, recipient, body, status)
		VALUES (?, ?, ?, ?, ?, 'queued')`,
		m.Kind, m.DedupKey, m.Channel, m.Recipient, m.Body)
	if err != nil {
		return 0, fmt.Errorf("storage: queue message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrDuplicateMessage
	}
	return res.LastInsertId()
}

// PendingMessages returns queued messages oldest first.
func (s *Storage) PendingMessages(ctx context.Context, limit int) ([]Message, error) {
	return s.queryMessages(ctx, `
		SELECT id, kind, dedup_key, channel, recipient, body, status, attempts,
		       COALESCE(last_error, ''), created_at, sent_at
		FROM messages WHERE status = 'queued' ORDER BY id LIMIT ?`, limit)
}

// RecentMessages returns the newest messages whatever their status.
func (s *Storage) RecentMessages(ctx context.Context, limit int) ([]Message, error) {
	return s.queryMessages(ctx, `
		SELECT id, kind, dedup_key, channel, recipient, body, status, attempts,
		       COALESCE(last_error, ''), created_at, sent_at
		FROM messages ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Storage) queryMessages(ctx context.Context, query string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: read messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var created, sent sql.NullTime
		if err := rows.Scan(&m.ID, &m.Kind, &m.DedupKey, &m.Channel, &m.Recipient,
			&m.Body, &m.Status, &m.Attempts, &m.LastError, &created, &sent); err != nil {
			return nil, fmt.Errorf("storage: scan message: %w", err)
		}
		m.CreatedAt, m.SentAt = created.Time, sent.Time
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMessageSent records a delivered message.
func (s *Storage) MarkMessageSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages SET status = 'sent', attempts = attempts + 1,
		       last_error = NULL, sent_at = CURRENT_TIMESTAMP
		WHERE id = ?`, id)
	return err
}

// MarkMessageAttempt records a failed send. The message stays queued for
// another try until it has used up maxAttempts, after which it is failed
// so a permanently bad recipient cannot be retried forever.
func (s *Storage) MarkMessageAttempt(ctx context.Context, id int64, sendErr error, maxAttempts int) error {
	msg := "unknown error"
	if sendErr != nil {
		msg = sendErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages
		SET attempts = attempts + 1,
		    last_error = ?,
		    status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'queued' END
		WHERE id = ?`, msg, maxAttempts, id)
	return err
}

// MessageCounts returns how many messages are queued, sent and failed.
func (s *Storage) MessageCounts(ctx context.Context) (queued, sent, failed int, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(status = 'queued'), 0),
			COALESCE(SUM(status = 'sent'), 0),
			COALESCE(SUM(status = 'failed'), 0)
		FROM messages`)
	err = row.Scan(&queued, &sent, &failed)
	return queued, sent, failed, err
}
