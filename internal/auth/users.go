package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// OwnerUsername is the account created on first run. It is kept as a
// constant because an upgrade from the single-PIN build has to keep
// working with the PIN that is already set.
const OwnerUsername = ownerUsername

// usernamePattern is what a sign-in name may contain. It is typed on a
// phone by someone in a hurry, so it stays short and unambiguous.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// ErrInvalidUsername explains what a sign-in name may be.
var ErrInvalidUsername = errors.New(
	"a sign-in name may use letters, numbers, dots, dashes and underscores, and must be at least 2 characters")

// AddUser creates a person and sets their first PIN.
func (a *Auth) AddUser(ctx context.Context, username, displayName, role, pin string) (storage.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return storage.User{}, ErrInvalidUsername
	}
	if len(pin) < MinPINLength {
		return storage.User{}, ErrPINTooShort
	}
	if !storage.ValidRole(role) {
		return storage.User{}, fmt.Errorf("auth: %q is not a role", role)
	}
	id, err := a.store.CreateUser(ctx, username, displayName, role)
	if err != nil {
		return storage.User{}, err
	}
	if err := a.SetPINFor(ctx, username, pin); err != nil {
		// Roll the half-made account back rather than leaving one that
		// nobody can sign in to and the owner cannot see why.
		_ = a.store.DeleteUser(ctx, id)
		return storage.User{}, err
	}
	return a.store.UserByID(ctx, id)
}

// SetPINFor sets or replaces one person's PIN and signs them out
// everywhere, so a PIN changed because it was shared takes effect at
// once rather than whenever the old session happens to expire.
func (a *Auth) SetPINFor(ctx context.Context, username, pin string) error {
	if len(pin) < MinPINLength {
		return ErrPINTooShort
	}
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("auth: salt: %w", err)
	}
	hash := pbkdf2SHA256(pin, salt, DefaultIterations)
	if err := a.store.SetDashboardUserPIN(ctx, strings.ToLower(strings.TrimSpace(username)),
		base64.StdEncoding.EncodeToString(hash),
		base64.StdEncoding.EncodeToString(salt),
		DefaultIterations,
	); err != nil {
		return err
	}
	u, err := a.store.GetDashboardUser(ctx, username)
	if err != nil {
		return nil // the PIN is set; not being able to re-read is not fatal
	}
	return a.store.DeleteSessionsForUser(ctx, u.ID)
}

// LoginAs signs in a named user. It is what the login form calls once a
// station has more than one person on it.
//
// The lockout counter is per account, so one member of staff getting
// their PIN wrong repeatedly cannot lock the owner out of their own
// dashboard.
func (a *Auth) LoginAs(ctx context.Context, username, pin, userAgent string) (string, storage.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	u, err := a.store.GetDashboardUser(ctx, username)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Do the hashing anyway. Returning immediately for an unknown
			// name makes the response time say whether the name exists.
			_ = pbkdf2SHA256(pin, make([]byte, SaltSize), DefaultIterations)
			return "", storage.User{}, ErrInvalidPIN
		}
		return "", storage.User{}, fmt.Errorf("auth: lookup: %w", err)
	}
	if !u.IsActive {
		return "", storage.User{}, errors.New("that account has been switched off")
	}
	if u.PinHash == "" {
		return "", storage.User{}, errors.New("that account has no PIN yet")
	}
	if time.Now().Before(u.LockUntil) {
		return "", storage.User{}, &LockedError{Until: u.LockUntil}
	}

	salt, err := base64.StdEncoding.DecodeString(u.PinSalt)
	if err != nil {
		return "", storage.User{}, fmt.Errorf("auth: bad salt: %w", err)
	}
	want, err := base64.StdEncoding.DecodeString(u.PinHash)
	if err != nil {
		return "", storage.User{}, fmt.Errorf("auth: bad hash: %w", err)
	}
	if subtle.ConstantTimeCompare(want, pbkdf2SHA256(pin, salt, u.PinIters)) != 1 {
		until, rerr := a.store.RecordLoginFailure(ctx, username, MaxFailedAttempts, LockoutDuration)
		if rerr != nil {
			return "", storage.User{}, fmt.Errorf("auth: record failure: %w", rerr)
		}
		if !until.IsZero() {
			return "", storage.User{}, &LockedError{Until: until}
		}
		return "", storage.User{}, ErrInvalidPIN
	}

	if err := a.store.RecordLoginSuccess(ctx, username); err != nil {
		return "", storage.User{}, fmt.Errorf("auth: record success: %w", err)
	}
	sid, err := newSessionID()
	if err != nil {
		return "", storage.User{}, err
	}
	if err := a.store.CreateSession(ctx, sid, u.ID, time.Now().Add(SessionTTL), userAgent); err != nil {
		return "", storage.User{}, fmt.Errorf("auth: create session: %w", err)
	}
	full, err := a.store.UserByID(ctx, u.ID)
	if err != nil {
		return sid, storage.User{ID: u.ID, Username: u.Username, Role: storage.RoleOwner}, nil
	}
	return sid, full, nil
}

// CurrentUser resolves a session to the person holding it, so every page
// knows who is looking at it and every change knows who made it.
func (a *Auth) CurrentUser(ctx context.Context, sessionID string) (storage.User, error) {
	id, err := a.Verify(ctx, sessionID)
	if err != nil {
		return storage.User{}, err
	}
	u, err := a.store.UserByID(ctx, id)
	if err != nil {
		return storage.User{}, ErrInvalidSession
	}
	if !u.IsActive {
		return storage.User{}, ErrInvalidSession
	}
	return u, nil
}

// ErrWrongCurrentPIN is returned when the PIN being replaced was not
// typed correctly.
var ErrWrongCurrentPIN = errors.New("that is not your current PIN")

// ErrSamePIN is returned when the new PIN is the old one.
var ErrSamePIN = errors.New("the new PIN is the same as the old one")

// ChangeOwnPIN lets a signed-in person replace their own PIN, having
// proved they know the current one.
//
// This is the ordinary path, and it is the one that was missing: without
// it, a member of staff who suspects their PIN is known has no way to
// change it, and a PIN shared once stays shared. Knowing the current PIN
// is required so that an unattended, still-signed-in browser cannot be
// used to lock the real owner out of their own account.
func (a *Auth) ChangeOwnPIN(ctx context.Context, userID int64, currentPIN, newPIN string) error {
	if len(newPIN) < MinPINLength {
		return ErrPINTooShort
	}
	if currentPIN == newPIN {
		return ErrSamePIN
	}
	u, err := a.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	row, err := a.store.GetDashboardUser(ctx, u.Username)
	if err != nil {
		return err
	}
	salt, err := base64.StdEncoding.DecodeString(row.PinSalt)
	if err != nil {
		return fmt.Errorf("auth: bad salt: %w", err)
	}
	want, err := base64.StdEncoding.DecodeString(row.PinHash)
	if err != nil {
		return fmt.Errorf("auth: bad hash: %w", err)
	}
	if subtle.ConstantTimeCompare(want, pbkdf2SHA256(currentPIN, salt, row.PinIters)) != 1 {
		// Count it against the lockout: this is a PIN guess like any
		// other, and an open session must not be a way to try PINs
		// without limit.
		if _, rerr := a.store.RecordLoginFailure(ctx, u.Username, MaxFailedAttempts, LockoutDuration); rerr != nil {
			return fmt.Errorf("auth: record failure: %w", rerr)
		}
		return ErrWrongCurrentPIN
	}
	return a.SetPINFor(ctx, u.Username, newPIN)
}
