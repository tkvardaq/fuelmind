package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuelmind/fuelmind/internal/storage"
)

func auditStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.MigrateCount(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// The account that existed before this build had a role at all must come
// through the upgrade as the owner, still able to run the station.
func TestExistingAccountBecomesTheOwner(t *testing.T) {
	s := auditStore(t)
	users, err := s.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users after migration, want the one that already existed", len(users))
	}
	if !users[0].IsOwner() {
		t.Errorf("the existing account is %q, want owner", users[0].Role)
	}
	if users[0].Name() == "" {
		t.Error("the existing account has no name to show")
	}
}

func TestUserLifecycle(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "bilal", "Bilal", storage.RoleStaff)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bilal", "Bilal Again", storage.RoleStaff); !errors.Is(err, storage.ErrUserExists) {
		t.Errorf("creating a duplicate = %v, want ErrUserExists", err)
	}

	u, err := s.UserByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.IsOwner() || u.Name() != "Bilal" {
		t.Errorf("user = %+v, want staff named Bilal", u)
	}
	// A new account has no PIN, so it cannot be signed in to yet and
	// must not appear on the login page.
	if u.PINSet {
		t.Error("a new account should have no PIN until one is set")
	}
	active, err := s.ActiveUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == id {
			t.Error("an account with no PIN was offered on the login page")
		}
	}

	if err := s.UpdateUser(ctx, id, "Bilal K", storage.RoleStaff, false); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	u, _ = s.UserByID(ctx, id)
	if u.Name() != "Bilal K" || u.IsActive {
		t.Errorf("after update: %+v, want renamed and switched off", u)
	}

	if err := s.DeleteUser(ctx, id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.UserByID(ctx, id); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("after delete = %v, want ErrNotFound", err)
	}
}

// A station must never be left with nobody who can administer it.
func TestCannotRemoveOrDemoteTheLastOwner(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()
	users, _ := s.Users(ctx)
	owner := users[0]

	if err := s.DeleteUser(ctx, owner.ID); !errors.Is(err, storage.ErrLastOwner) {
		t.Errorf("removing the last owner = %v, want ErrLastOwner", err)
	}
	if err := s.UpdateUser(ctx, owner.ID, owner.Name(), storage.RoleStaff, true); !errors.Is(err, storage.ErrLastOwner) {
		t.Errorf("demoting the last owner = %v, want ErrLastOwner", err)
	}
	if err := s.UpdateUser(ctx, owner.ID, owner.Name(), storage.RoleOwner, false); !errors.Is(err, storage.ErrLastOwner) {
		t.Errorf("switching off the last owner = %v, want ErrLastOwner", err)
	}

	// With a second owner there is no longer a problem.
	second, err := s.CreateUser(ctx, "partner", "Partner", storage.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, owner.ID, owner.Name(), storage.RoleStaff, true); err != nil {
		t.Errorf("demoting with another owner present: %v", err)
	}
	_ = second
}

// Removing someone must not erase what they did. The log is the record
// of who moved money on the books.
func TestRemovingSomeoneKeepsTheirHistory(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()

	id, err := s.CreateUser(ctx, "bilal", "Bilal", storage.RoleStaff)
	if err != nil {
		t.Fatal(err)
	}
	err = s.RecordAudit(ctx, storage.AuditEntry{
		UserID: id, ActorName: "Bilal", ActorRole: storage.RoleStaff,
		Action: storage.ActionCreditPayment, Subject: "+923001234567",
		Detail: "Recorded a repayment of PKR 5,000.00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}

	entries, err := s.RecentAudit(ctx, storage.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries after removing the person, want the entry kept", len(entries))
	}
	// The name is stored as it was, so the log still reads correctly.
	if entries[0].ActorName != "Bilal" {
		t.Errorf("actor = %q, want the name as it was at the time", entries[0].ActorName)
	}
	if !strings.Contains(entries[0].Detail, "5,000") {
		t.Errorf("detail was lost: %q", entries[0].Detail)
	}
}

func TestAuditFilters(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()
	users, _ := s.Users(ctx)
	owner := users[0]

	for _, e := range []storage.AuditEntry{
		{UserID: owner.ID, ActorName: "Owner", ActorRole: storage.RoleOwner, Action: storage.ActionSaleEntered, Detail: "a sale"},
		{UserID: owner.ID, ActorName: "Owner", ActorRole: storage.RoleOwner, Action: storage.ActionPriceSet, Detail: "a price"},
		{ActorName: "bilal", Action: storage.ActionLoginFailed, Detail: "wrong pin"},
	} {
		if err := s.RecordAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.RecentAudit(ctx, storage.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}

	byAction, err := s.RecentAudit(ctx, storage.AuditFilter{Action: storage.ActionSaleEntered})
	if err != nil {
		t.Fatal(err)
	}
	if len(byAction) != 1 || byAction[0].Detail != "a sale" {
		t.Errorf("filtering by action gave %+v", byAction)
	}

	byUser, err := s.RecentAudit(ctx, storage.AuditFilter{UserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUser) != 2 {
		t.Errorf("filtering by person gave %d entries, want 2", len(byUser))
	}

	// A window that excludes everything returns nothing rather than
	// everything, which is the failure that would make the page lie.
	future, err := s.RecentAudit(ctx, storage.AuditFilter{Since: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(future) != 0 {
		t.Errorf("a future window returned %d entries, want none", len(future))
	}

	actions, err := s.AuditActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 3 {
		t.Errorf("actions = %v, want the three that were recorded", actions)
	}
}

// An entry with nobody signed in (a failed login) still has to record.
func TestAuditWithoutAUser(t *testing.T) {
	s := auditStore(t)
	err := s.RecordAudit(context.Background(), storage.AuditEntry{
		ActorName: "bilal", Action: storage.ActionLoginFailed, Detail: "wrong pin",
	})
	if err != nil {
		t.Fatalf("RecordAudit with no user: %v", err)
	}
	entries, _ := s.RecentAudit(context.Background(), storage.AuditFilter{})
	if len(entries) != 1 || entries[0].UserID != 0 {
		t.Errorf("entries = %+v, want one with no user attached", entries)
	}
}
