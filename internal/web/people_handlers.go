package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/storage"
)

// handlePeople is where the owner gives each person at the station their
// own sign-in, so the Activity page can say who did what.
//
// Owner-only: adding an account is adding someone who can record a sale
// and a credit repayment.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	var formError, notice string

	if r.Method == http.MethodPost {
		switch r.FormValue("action") {
		case "add":
			notice, formError = s.addPerson(r)
		case "update":
			notice, formError = s.updatePerson(r)
		case "pin":
			notice, formError = s.resetPersonPIN(r)
		case "remove":
			notice, formError = s.removePerson(r)
		case "reset_approve":
			notice, formError = s.approvePINReset(r)
		case "reset_dismiss":
			notice, formError = s.dismissPINReset(r)
		default:
			formError = "Unknown action."
		}
	}

	people, err := s.store.Users(r.Context())
	if err != nil {
		s.renderError(w, "people", err)
		return
	}
	requests, err := s.store.PendingPINResets(r.Context())
	if err != nil {
		s.renderError(w, "people: pin requests", err)
		return
	}
	s.render(w, r, "people", map[string]any{
		"LoggedIn": true,
		"User":     userFrom(r.Context()),
		"People":   people,
		"Requests": requests,
		"MinPIN":   auth.MinPINLength,
		"Error":    formError,
		"Notice":   notice,
	})
}

func (s *Server) addPerson(r *http.Request) (notice, formError string) {
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	name := strings.TrimSpace(r.FormValue("display_name"))
	role := strings.TrimSpace(r.FormValue("role"))
	pin := strings.TrimSpace(r.FormValue("pin"))

	if name == "" {
		name = username
	}
	user, err := s.auth.AddUser(r.Context(), username, name, role, pin)
	switch {
	case errors.Is(err, storage.ErrUserExists):
		return "", fmt.Sprintf("There is already an account called %q.", username)
	case errors.Is(err, auth.ErrInvalidUsername):
		return "", err.Error()
	case errors.Is(err, auth.ErrPINTooShort):
		return "", fmt.Sprintf("Their PIN must be at least %d characters.", auth.MinPINLength)
	case err != nil:
		return "", err.Error()
	}
	s.audit(r, storage.ActionUserAdded, user.Username,
		fmt.Sprintf("Added %s as %s", user.Name(), storage.RoleLabel(user.Role)))
	return fmt.Sprintf("%s can now sign in. Give them their PIN and ask them to change it.", user.Name()), ""
}

func (s *Server) updatePerson(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return "", "That account no longer exists."
	}
	before, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		return "", "That account no longer exists."
	}
	name := strings.TrimSpace(r.FormValue("display_name"))
	role := strings.TrimSpace(r.FormValue("role"))
	active := r.FormValue("is_active") == "on"

	if err := s.store.UpdateUser(r.Context(), id, name, role, active); err != nil {
		if errors.Is(err, storage.ErrLastOwner) {
			return "", "This is the only owner account. Make someone else an owner first, or the station would be left with nobody who can change its settings."
		}
		return "", err.Error()
	}
	after, _ := s.store.UserByID(r.Context(), id)
	if !active {
		// Switching someone off has to take their session with it.
		if err := s.store.DeleteSessionsForUser(r.Context(), id); err != nil {
			s.logger.Warn("could not sign out a disabled account", "user", before.Username, "err", err)
		}
	}
	s.audit(r, storage.ActionUserChanged, before.Username, describeUserChange(before, after))
	return fmt.Sprintf("%s updated.", after.Name()), ""
}

// describeUserChange says what actually changed, so the activity log is
// worth reading rather than a row of "updated".
func describeUserChange(before, after storage.User) string {
	var parts []string
	if before.DisplayName != after.DisplayName {
		parts = append(parts, fmt.Sprintf("renamed %q to %q", before.Name(), after.Name()))
	}
	if before.Role != after.Role {
		parts = append(parts, fmt.Sprintf("changed from %s to %s",
			storage.RoleLabel(before.Role), storage.RoleLabel(after.Role)))
	}
	if before.IsActive != after.IsActive {
		if after.IsActive {
			parts = append(parts, "switched back on")
		} else {
			parts = append(parts, "switched off and signed out")
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Saved %s with no changes", after.Name())
	}
	return strings.ToUpper(parts[0][:1]) + parts[0][1:] + joinRest(parts[1:])
}

func joinRest(rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	return ", " + strings.Join(rest, ", ")
}

func (s *Server) resetPersonPIN(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return "", "That account no longer exists."
	}
	user, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		return "", "That account no longer exists."
	}
	pin := strings.TrimSpace(r.FormValue("pin"))
	if err := s.auth.SetPINFor(r.Context(), user.Username, pin); err != nil {
		if errors.Is(err, auth.ErrPINTooShort) {
			return "", fmt.Sprintf("The PIN must be at least %d characters.", auth.MinPINLength)
		}
		return "", err.Error()
	}
	s.audit(r, storage.ActionPINChanged, user.Username,
		fmt.Sprintf("Set a new PIN for %s and signed them out", user.Name()))
	return fmt.Sprintf("%s has a new PIN and has been signed out everywhere.", user.Name()), ""
}

func (s *Server) removePerson(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return "", "That account no longer exists."
	}
	user, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		return "", "That account no longer exists."
	}
	if user.ID == userFrom(r.Context()).ID {
		return "", "You cannot remove the account you are signed in with."
	}
	if err := s.store.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, storage.ErrLastOwner) {
			return "", "This is the only owner account, so it cannot be removed."
		}
		return "", err.Error()
	}
	s.audit(r, storage.ActionUserRemoved, user.Username,
		fmt.Sprintf("Removed %s. What they did before is still in this log.", user.Name()))
	return fmt.Sprintf("%s removed. Their entries in the activity log are kept.", user.Name()), ""
}

// handleActivity answers "who made this change?" — the one question a
// shared PIN could never answer.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	filter := storage.AuditFilter{Limit: 200}
	if v := strings.TrimSpace(r.URL.Query().Get("action")); v != "" {
		filter.Action = v
	}
	if v := strings.TrimSpace(r.URL.Query().Get("user")); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.UserID = id
		}
	}
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	filter.Since = time.Now().AddDate(0, 0, -days)

	entries, err := s.store.RecentAudit(r.Context(), filter)
	if err != nil {
		s.renderError(w, "activity", err)
		return
	}
	people, err := s.store.Users(r.Context())
	if err != nil {
		s.renderError(w, "activity: people", err)
		return
	}
	actions, err := s.store.AuditActions(r.Context())
	if err != nil {
		s.renderError(w, "activity: actions", err)
		return
	}

	s.render(w, r, "activity", map[string]any{
		"LoggedIn":     true,
		"User":         userFrom(r.Context()),
		"Entries":      entries,
		"People":       people,
		"Actions":      actions,
		"FilterAction": filter.Action,
		"FilterUser":   filter.UserID,
		"Days":         days,
	})
}
