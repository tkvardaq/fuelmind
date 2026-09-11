// Package auth handles the dashboard's PIN-based login (Phase 4).
//
// Spec §9 says: "Local dashboard requires login (even if it's a
// single shared PIN/password) — don't rely on 'it's on the local
// network' as the only access control; shop PCs are often
// shared/unlocked."
//
// v1 model: a single owner account (username "owner") with a PBKDF2-
// hashed PIN. The installer (Phase 8) prompts for a PIN on first run
// and stores it. The dashboard refuses to render any page until the
// user logs in.
//
// We use PBKDF2 (golang.org/x/crypto/pbkdf2) with SHA-256 and 100,000
// iterations. This is Go's stdlib-adjacent choice and the right
// shape for a "minimum viable but real" auth — far better than
// plain SHA, doesn't require bcrypt as a dependency, and the
// iteration count is forward-tunable.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// PBKDF2 iterations. 100k is the OWASP 2023 minimum for SHA-256.
const DefaultIterations = 100_000

// SaltSize is the salt length in bytes (16 bytes = 128 bits).
const SaltSize = 16

// SessionTTL is how long a login session lives before re-auth required.
const SessionTTL = 24 * time.Hour

// Auth is the authentication service. One per process.
type Auth struct {
	store *storage.Storage
}

// New builds an Auth service. The store must be migrated and the
// `dashboard_users` table must have its default 'owner' row.
func New(store *storage.Storage) *Auth {
	return &Auth{store: store}
}

// SetupPIN sets the owner's PIN. Used by the installer (Phase 8)
// and by the "first run" wizard in the dashboard.
func (a *Auth) SetupPIN(ctx context.Context, pin string) error {
	if len(pin) < 8 {
		return errors.New("auth: PIN must be at least 8 characters")
	}
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("auth: salt: %w", err)
	}
	hash := pbkdf2SHA256(pin, salt, DefaultIterations)
	return a.store.SetDashboardUserPIN(ctx, "owner",
		base64.StdEncoding.EncodeToString(hash),
		base64.StdEncoding.EncodeToString(salt),
		DefaultIterations,
	)
}

// IsPINSet returns true if the owner has a non-empty PIN configured.
// Used by the dashboard to decide whether to show the "set your PIN"
// wizard vs the regular login form on first run.
func (a *Auth) IsPINSet(ctx context.Context) (bool, error) {
	u, err := a.store.GetDashboardUser(ctx, "owner")
	if err != nil {
		return false, err
	}
	return u.PinHash != "", nil
}

// Login checks the PIN and, on success, returns a fresh session id.
// Returns ErrInvalidPIN on bad credentials (deliberate generic
// message — we don't leak which field was wrong).
func (a *Auth) Login(ctx context.Context, pin, userAgent string) (string, error) {
	u, err := a.store.GetDashboardUser(ctx, "owner")
	if err != nil {
		return "", fmt.Errorf("auth: lookup: %w", err)
	}
	if !u.IsActive {
		return "", errors.New("auth: account disabled")
	}
	if u.PinHash == "" {
		return "", errors.New("auth: PIN not set — run installer first")
	}

	salt, err := base64.StdEncoding.DecodeString(u.PinSalt)
	if err != nil {
		return "", fmt.Errorf("auth: bad salt: %w", err)
	}
	want, err := base64.StdEncoding.DecodeString(u.PinHash)
	if err != nil {
		return "", fmt.Errorf("auth: bad hash: %w", err)
	}
	got := pbkdf2SHA256(pin, salt, u.PinIters)

	// Constant-time compare. subtle.ConstantTimeCompare returns 1 on
	// equal, 0 on different; we treat any non-1 as invalid.
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return "", ErrInvalidPIN
	}

	sid, err := newSessionID()
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().Add(SessionTTL)
	if err := a.store.CreateSession(ctx, sid, u.ID, expiresAt, userAgent); err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return sid, nil
}

// Logout deletes a session. Idempotent — deleting a non-existent
// session is a no-op.
func (a *Auth) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return a.store.DeleteSession(ctx, sessionID)
}

// Verify returns the user_id for a session, or ErrInvalidSession.
// Storage-layer "not found" / "expired" errors are translated into
// the auth-layer sentinel so callers don't need to import storage
// just to check session validity.
func (a *Auth) Verify(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, ErrInvalidSession
	}
	uid, err := a.store.LookupSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, storage.ErrInvalidSession) || errors.Is(err, storage.ErrNotFound) {
			return 0, ErrInvalidSession
		}
		return 0, fmt.Errorf("auth: lookup session: %w", err)
	}
	return uid, nil
}

// ErrInvalidPIN is returned for bad credentials. Generic; doesn't
// distinguish "no such user" from "wrong PIN" to prevent username
// enumeration.
var ErrInvalidPIN = errors.New("auth: invalid credentials")

// ErrInvalidSession is returned for missing or expired sessions.
var ErrInvalidSession = errors.New("auth: invalid session")

// pbkdf2SHA256 wraps the stdlib PBKDF2 with our defaults. 1-line
// helper because pbkdf2.Key's signature is verbose.
func pbkdf2SHA256(pin string, salt []byte, iters int) []byte {
	// We use the stdlib golang.org/x/crypto/pbkdf2 via the
	// crypto/pbkdf2 package (added in Go 1.24). For Go 1.23 we
	// fall back to the x/crypto package.
	return pbkdf2Key([]byte(pin), salt, iters, 32, sha256.New)
}

func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
