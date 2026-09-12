package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func newTestAuth(t *testing.T) (*Auth, *storage.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return New(s), s
}

func TestIsPINSetDefaultsFalse(t *testing.T) {
	a, _ := newTestAuth(t)
	set, err := a.IsPINSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Error("expected PIN not to be set by default")
	}
}

func TestSetupPIN(t *testing.T) {
	a, _ := newTestAuth(t)
	if err := a.SetupPIN(context.Background(), "12345678"); err != nil {
		t.Fatal(err)
	}
	set, _ := a.IsPINSet(context.Background())
	if !set {
		t.Error("expected PIN to be set after SetupPIN")
	}
}

func TestSetupPINRejectsShort(t *testing.T) {
	a, _ := newTestAuth(t)
	if err := a.SetupPIN(context.Background(), "12"); err == nil {
		t.Error("expected error for short PIN")
	}
}

func TestLoginWrongPIN(t *testing.T) {
	a, _ := newTestAuth(t)
	_ = a.SetupPIN(context.Background(), "12345678")
	_, err := a.Login(context.Background(), "9999", "test-agent")
	if err != ErrInvalidPIN {
		t.Errorf("expected ErrInvalidPIN, got %v", err)
	}
}

func TestLoginRightPIN(t *testing.T) {
	a, _ := newTestAuth(t)
	_ = a.SetupPIN(context.Background(), "12345678")
	sid, err := a.Login(context.Background(), "12345678", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Error("empty session id")
	}
	uid, err := a.Verify(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if uid <= 0 {
		t.Errorf("user id = %d, want > 0", uid)
	}
}

func TestLoginNoPIN(t *testing.T) {
	a, _ := newTestAuth(t)
	_, err := a.Login(context.Background(), "1234", "")
	if err == nil {
		t.Error("expected error when no PIN is set")
	}
}

func TestVerifyRejectsEmpty(t *testing.T) {
	a, _ := newTestAuth(t)
	_, err := a.Verify(context.Background(), "")
	if err != ErrInvalidSession {
		t.Errorf("expected ErrInvalidSession, got %v", err)
	}
}

func TestLogout(t *testing.T) {
	a, _ := newTestAuth(t)
	if err := a.SetupPIN(context.Background(), "12345678"); err != nil {
		t.Fatal(err)
	}
	sid, err := a.Login(context.Background(), "12345678", "")
	if err != nil || sid == "" {
		t.Fatalf("login: sid=%q err=%v", sid, err)
	}
	if _, err := a.Verify(context.Background(), sid); err != nil {
		t.Fatalf("session should be valid before logout: %v", err)
	}
	if err := a.Logout(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	_, err = a.Verify(context.Background(), sid)
	if err != ErrInvalidSession {
		t.Errorf("expected ErrInvalidSession after logout, got %v", err)
	}
}

func TestLockoutAfterFiveWrongPINs(t *testing.T) {
	a, s := newTestAuth(t)
	ctx := context.Background()
	if err := a.SetupPIN(ctx, "12345678"); err != nil {
		t.Fatal(err)
	}
	var lerr *LockedError
	for i := 1; i <= MaxFailedAttempts; i++ {
		_, err := a.Login(ctx, "00000000", "")
		if i < MaxFailedAttempts && err != ErrInvalidPIN {
			t.Fatalf("attempt %d: got %v, want ErrInvalidPIN", i, err)
		}
		if i == MaxFailedAttempts && !errors.As(err, &lerr) {
			t.Fatalf("attempt %d: got %v, want LockedError", i, err)
		}
	}
	// Correct PIN is refused while locked.
	if _, err := a.Login(ctx, "12345678", ""); !errors.As(err, &lerr) {
		t.Fatalf("correct PIN during lockout: got %v, want LockedError", err)
	}
	// Expire the lock: the correct PIN works and the counter resets.
	if _, err := s.DB().Exec(`UPDATE dashboard_users SET lock_until = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "12345678", ""); err != nil {
		t.Fatalf("login after lock expiry: %v", err)
	}
	u, _ := s.GetDashboardUser(ctx, "owner")
	if u.FailedAttempts != 0 || !u.LockUntil.IsZero() {
		t.Errorf("counter not reset: %+v", u)
	}
}

func TestPINResetClearsLockout(t *testing.T) {
	a, _ := newTestAuth(t)
	ctx := context.Background()
	_ = a.SetupPIN(ctx, "12345678")
	for i := 0; i < MaxFailedAttempts; i++ {
		_, _ = a.Login(ctx, "00000000", "")
	}
	if err := a.SetupPIN(ctx, "newpin9999"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, "newpin9999", ""); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
}

func TestPBKDF2Determinism(t *testing.T) {
	// The same pin + salt + iters must produce the same hash.
	salt := []byte("0123456789abcdef")
	h1 := pbkdf2SHA256("1234", salt, 1000)
	h2 := pbkdf2SHA256("1234", salt, 1000)
	if string(h1) != string(h2) {
		t.Error("PBKDF2 not deterministic")
	}
	h3 := pbkdf2SHA256("1235", salt, 1000)
	if string(h1) == string(h3) {
		t.Error("different PINs produced the same hash")
	}
}
