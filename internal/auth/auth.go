// Package auth handles the dashboard's PIN-based login.
//
// Spec §9: the local dashboard requires login — never rely on "it's on
// the local network" as access control.
//
// v1 model: a single owner account (username "owner") with a PBKDF2-
// SHA256 hashed PIN (100k iterations), 24h sessions, and a lockout after
// MaxFailedAttempts wrong PINs (plan §7.8).
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

// DefaultIterations is the PBKDF2 iteration count (OWASP 2023 minimum
// for SHA-256).
const DefaultIterations = 100_000

// SaltSize is the salt length in bytes.
const SaltSize = 16

// MinPINLength is the minimum PIN length.
const MinPINLength = 8

// SessionTTL is how long a login session lives.
const SessionTTL = 24 * time.Hour

// MaxFailedAttempts wrong PINs in a row lock the account for LockoutDuration.
const (
	MaxFailedAttempts = 5
	LockoutDuration   = 15 * time.Minute
)

const ownerUsername = "owner"

// Auth is the authentication service. One per process.
type Auth struct {
	store *storage.Storage
}

// New builds an Auth service. The store must be migrated.
func New(store *storage.Storage) *Auth {
	return &Auth{store: store}
}

// ErrPINTooShort is returned by SetupPIN for PINs below MinPINLength.
var ErrPINTooShort = fmt.Errorf("auth: PIN must be at least %d characters", MinPINLength)

// SetupPIN sets (or resets) the owner's PIN and clears any lockout.
func (a *Auth) SetupPIN(ctx context.Context, pin string) error {
	if len(pin) < MinPINLength {
		return ErrPINTooShort
	}
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("auth: salt: %w", err)
	}
	hash := pbkdf2SHA256(pin, salt, DefaultIterations)
	return a.store.SetDashboardUserPIN(ctx, ownerUsername,
		base64.StdEncoding.EncodeToString(hash),
		base64.StdEncoding.EncodeToString(salt),
		DefaultIterations,
	)
}

// IsPINSet reports whether the owner has a PIN configured.
func (a *Auth) IsPINSet(ctx context.Context) (bool, error) {
	u, err := a.store.GetDashboardUser(ctx, ownerUsername)
	if err != nil {
		return false, err
	}
	return u.PinHash != "", nil
}

// LockedError is returned by Login while the account is locked.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("auth: too many wrong PINs; locked until %s", e.Until.Local().Format("15:04"))
}

// Login checks the PIN and, on success, returns a fresh session id.
// Returns ErrInvalidPIN on bad credentials and *LockedError while locked.
func (a *Auth) Login(ctx context.Context, pin, userAgent string) (string, error) {
	u, err := a.store.GetDashboardUser(ctx, ownerUsername)
	if err != nil {
		return "", fmt.Errorf("auth: lookup: %w", err)
	}
	if !u.IsActive {
		return "", errors.New("auth: account disabled")
	}
	if u.PinHash == "" {
		return "", errors.New("auth: PIN not set")
	}
	if time.Now().Before(u.LockUntil) {
		return "", &LockedError{Until: u.LockUntil}
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
	if subtle.ConstantTimeCompare(want, got) != 1 {
		until, rerr := a.store.RecordLoginFailure(ctx, ownerUsername, MaxFailedAttempts, LockoutDuration)
		if rerr != nil {
			return "", fmt.Errorf("auth: record failure: %w", rerr)
		}
		if !until.IsZero() {
			return "", &LockedError{Until: until}
		}
		return "", ErrInvalidPIN
	}

	if err := a.store.RecordLoginSuccess(ctx, ownerUsername); err != nil {
		return "", fmt.Errorf("auth: record success: %w", err)
	}
	sid, err := newSessionID()
	if err != nil {
		return "", err
	}
	if err := a.store.CreateSession(ctx, sid, u.ID, time.Now().Add(SessionTTL), userAgent); err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return sid, nil
}

// Logout deletes a session. Idempotent.
func (a *Auth) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return a.store.DeleteSession(ctx, sessionID)
}

// Verify returns the user_id for a session, or ErrInvalidSession.
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

// ErrInvalidPIN is returned for bad credentials.
var ErrInvalidPIN = errors.New("auth: invalid credentials")

// ErrInvalidSession is returned for missing or expired sessions.
var ErrInvalidSession = errors.New("auth: invalid session")

func pbkdf2SHA256(pin string, salt []byte, iters int) []byte {
	return pbkdf2Key([]byte(pin), salt, iters, 32, sha256.New)
}

func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
