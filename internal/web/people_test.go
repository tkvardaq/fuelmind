package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// staffServer signs in as a member of staff rather than the owner, so
// the role boundary can be exercised.
func staffServer(t *testing.T) (*Server, func(method, path string, form url.Values) *httptest.ResponseRecorder) {
	t.Helper()
	srv, a, _ := newTestServer(t)
	ctx := context.Background()
	if err := a.SetupPIN(ctx, "owner12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddUser(ctx, "bilal", "Bilal", storage.RoleStaff, "bilal12345"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := a.LoginAs(ctx, "bilal", "bilal12345", "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, requestAs(t, srv, sid)
}

func ownerServer(t *testing.T) (*Server, func(method, path string, form url.Values) *httptest.ResponseRecorder) {
	t.Helper()
	srv, a, _ := newTestServer(t)
	ctx := context.Background()
	if err := a.SetupPIN(ctx, "owner12345"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := a.LoginAs(ctx, auth.OwnerUsername, "owner12345", "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, requestAs(t, srv, sid)
}

func requestAs(t *testing.T, srv *Server, sid string) func(string, string, url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var r *http.Request
		if form != nil {
			r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sid})
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		return w
	}
}

// Staff record what happened on their shift. Changing how the station is
// set up — where the data comes from, what is paid per litre, who has an
// account — is the owner's.
func TestStaffCannotChangeTheStationSetup(t *testing.T) {
	_, do := staffServer(t)

	for _, path := range []string{"/settings", "/people"} {
		w := do("GET", path, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("staff opening %s = %d, want 403", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Only the owner") {
			t.Errorf("staff opening %s was refused without saying why", path)
		}
	}

	// The pages staff need are still theirs.
	for _, path := range []string{"/", "/data", "/activity", "/credit"} {
		if w := do("GET", path, nil); w.Code != http.StatusOK {
			t.Errorf("staff opening %s = %d, want 200", path, w.Code)
		}
	}
}

// A page that staff cannot open should not be advertised to them.
func TestNavigationHidesWhatStaffCannotOpen(t *testing.T) {
	_, staffDo := staffServer(t)
	body := staffDo("GET", "/", nil).Body.String()
	nav := navOf(body)
	for _, hidden := range []string{`href="/settings"`, `href="/people"`} {
		if strings.Contains(nav, hidden) {
			t.Errorf("staff navigation offers %s, which they cannot open", hidden)
		}
	}
	if !strings.Contains(nav, `href="/data"`) {
		t.Error("staff navigation is missing the Data page they can use")
	}

	_, ownerDo := ownerServer(t)
	ownerNav := navOf(ownerDo("GET", "/", nil).Body.String())
	for _, shown := range []string{`href="/settings"`, `href="/people"`, `href="/activity"`} {
		if !strings.Contains(ownerNav, shown) {
			t.Errorf("owner navigation is missing %s", shown)
		}
	}
}

func navOf(body string) string {
	start := strings.Index(body, `<nav aria-label="Main">`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</nav>")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}

// The question the shared PIN could never answer.
func TestChangesRecordWhoMadeThem(t *testing.T) {
	srv, do := staffServer(t)
	ing := &fakeIngest{}
	srv.Ingest = ing

	w := do("POST", "/data", url.Values{
		"action": {"manual_sale"}, "date": {"2026-09-13"}, "time": {"10:30"},
		"product": {"DIESEL"}, "liters": {"20"}, "unit_price": {"275.50"},
		"payment_method": {"CASH"},
	})
	if !strings.Contains(w.Body.String(), "Sale recorded") {
		t.Fatalf("the sale was not recorded:\n%s", w.Body.String())
	}

	entries, err := srv.store.RecentAudit(context.Background(), storage.AuditFilter{
		Action: storage.ActionSaleEntered,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("recorded %d entries for a hand-entered sale, want 1", len(entries))
	}
	e := entries[0]
	if e.ActorName != "Bilal" {
		t.Errorf("actor = %q, want the member of staff who entered it", e.ActorName)
	}
	if e.ActorRole != storage.RoleStaff {
		t.Errorf("role = %q, want staff", e.ActorRole)
	}
	// The detail has to say what the sale was, or the log records that
	// something happened without recording what.
	for _, want := range []string{"20", "DIESEL", "275.50", "cash"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail %q is missing %q", e.Detail, want)
		}
	}
}

// A wrong PIN is worth recording: repeated failures on one account are
// something the owner should be able to see.
func TestFailedSignInIsRecorded(t *testing.T) {
	srv, a, _ := newTestServer(t)
	ctx := context.Background()
	if err := a.SetupPIN(ctx, "owner12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddUser(ctx, "bilal", "Bilal", storage.RoleStaff, "bilal12345"); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"username": {"bilal"}, "pin": {"wrongpin99"}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	entries, err := srv.store.RecentAudit(ctx, storage.AuditFilter{Action: storage.ActionLoginFailed})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("recorded %d failed sign-ins, want 1", len(entries))
	}
	if entries[0].Subject != "bilal" {
		t.Errorf("subject = %q, want the account that was attempted", entries[0].Subject)
	}
}

// Each account locks on its own: one member of staff getting their PIN
// wrong must not lock the owner out of their own dashboard.
func TestLockoutIsPerAccount(t *testing.T) {
	_, a, _ := newTestServer(t)
	ctx := context.Background()
	if err := a.SetupPIN(ctx, "owner12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddUser(ctx, "bilal", "Bilal", storage.RoleStaff, "bilal12345"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < auth.MaxFailedAttempts+1; i++ {
		_, _, _ = a.LoginAs(ctx, "bilal", "definitelywrong", "")
	}
	if _, _, err := a.LoginAs(ctx, "bilal", "bilal12345", ""); err == nil {
		t.Error("the staff account should be locked after repeated wrong PINs")
	}
	if _, _, err := a.LoginAs(ctx, auth.OwnerUsername, "owner12345", ""); err != nil {
		t.Errorf("the owner was locked out by someone else's wrong PINs: %v", err)
	}
}

// Owner-only pages are for owners, and the People page is where the
// owner gives everyone their own sign-in.
func TestOwnerCanAddAndRemovePeople(t *testing.T) {
	srv, do := ownerServer(t)

	body := do("POST", "/people", url.Values{
		"action": {"add"}, "username": {"bilal"}, "display_name": {"Bilal"},
		"role": {"staff"}, "pin": {"bilal12345"},
	}).Body.String()
	if !strings.Contains(body, "can now sign in") {
		t.Fatalf("adding a person failed:\n%s", body)
	}

	people, err := srv.store.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var bilal storage.User
	for _, p := range people {
		if p.Username == "bilal" {
			bilal = p
		}
	}
	if bilal.ID == 0 {
		t.Fatal("the new account was not created")
	}
	if bilal.IsOwner() {
		t.Error("a staff account was created as an owner")
	}

	// A PIN below the minimum is refused with a reason.
	body = do("POST", "/people", url.Values{
		"action": {"add"}, "username": {"ali"}, "display_name": {"Ali"},
		"role": {"staff"}, "pin": {"short"},
	}).Body.String()
	if !strings.Contains(body, "at least") {
		t.Errorf("a too-short PIN was not explained:\n%s", body)
	}

	// The only owner cannot remove themselves into a corner.
	owner := people[0]
	body = do("POST", "/people", url.Values{
		"action": {"remove"}, "id": {itoa(owner.ID)},
	}).Body.String()
	if !strings.Contains(body, "cannot remove the account you are signed in with") {
		t.Errorf("removing your own account was allowed:\n%s", body)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
