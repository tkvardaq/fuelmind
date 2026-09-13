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

// resetRequestWindow and resetRequestBurst bound how often one address
// may raise a PIN reset request. A request grants nothing, so this is
// not a security boundary — it stops the owner's People page from being
// filled with noise by someone leaning on the button.
const (
	resetRequestWindow = 10 * time.Minute
	resetRequestBurst  = 5
)

// handleAccount is where a signed-in person changes their own PIN.
//
// It is separate from People on purpose: People is the owner managing
// other people, this is anyone managing themselves. A member of staff
// who thinks their PIN has been seen must be able to change it without
// asking permission.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	var formError, notice string
	me := userFrom(r.Context())

	if r.Method == http.MethodPost {
		current := strings.TrimSpace(r.FormValue("current_pin"))
		next := strings.TrimSpace(r.FormValue("new_pin"))
		confirm := strings.TrimSpace(r.FormValue("confirm_pin"))

		switch {
		case next != confirm:
			formError = "The two new PINs don't match. Type the same PIN in both boxes."
		default:
			err := s.auth.ChangeOwnPIN(r.Context(), me.ID, current, next)
			switch {
			case errors.Is(err, auth.ErrWrongCurrentPIN):
				formError = "That is not your current PIN."
				s.audit(r, storage.ActionPINChanged, me.Username,
					"Tried to change their own PIN with the wrong current PIN")
			case errors.Is(err, auth.ErrPINTooShort):
				formError = fmt.Sprintf("Your new PIN must be at least %d characters.", auth.MinPINLength)
			case errors.Is(err, auth.ErrSamePIN):
				formError = "That is already your PIN. Choose a different one."
			case err != nil:
				formError = err.Error()
			default:
				// Changing a PIN signs every session out, including this
				// one, so the next page load lands on the login form. Say
				// so rather than letting it look like a fault.
				s.audit(r, storage.ActionPINChanged, me.Username, "Changed their own PIN")
				s.clearSessionCookie(w)
				s.renderStatus(w, r, http.StatusOK, "login", map[string]any{
					"Notice": "Your PIN has been changed. Sign in again with the new one.",
					"People": s.activeUsersOrNil(r),
					"Multi":  true,
					"Who":    me.Username,
				})
				return
			}
		}
	}

	s.render(w, r, "account", map[string]any{
		"LoggedIn": true,
		"Me":       me,
		"MinPIN":   auth.MinPINLength,
		"Error":    formError,
		"Notice":   notice,
	})
}

// activeUsersOrNil is the login page's people list, best-effort.
func (s *Server) activeUsersOrNil(r *http.Request) []storage.User {
	people, err := s.store.ActiveUsers(r.Context())
	if err != nil {
		s.logger.Warn("login: users", "err", err)
		return nil
	}
	return people
}

// clearSessionCookie signs this browser out.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// handleForgotPIN takes a request from someone who cannot sign in.
//
// The request is a note for the owner and nothing more: it changes no
// PIN, starts no session and discloses nothing. That is what makes it
// safe to offer on a page anyone on the station network can reach —
// the ability to actually set a new PIN never leaves an owner who is
// already signed in, or the shop PC's own command line.
func (s *Server) handleForgotPIN(w http.ResponseWriter, r *http.Request) {
	people, err := s.store.ActiveUsers(r.Context())
	if err != nil {
		s.renderError(w, "forgot: users", err)
		return
	}
	page := func(status int, errMsg, notice, who string) {
		s.renderStatus(w, r, status, "forgot", map[string]any{
			"People": people, "Error": errMsg, "Notice": notice, "Who": who,
		})
	}
	if r.Method == http.MethodGet {
		page(http.StatusOK, "", "", "")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	who := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	note := strings.TrimSpace(r.FormValue("note"))
	if who == "" {
		page(http.StatusOK, "Choose your name first.", "", "")
		return
	}

	addr := clientIP(r)
	if n, err := s.store.CountRecentPINResets(r.Context(), addr, resetRequestWindow); err == nil && n >= resetRequestBurst {
		page(http.StatusTooManyRequests,
			"That is a lot of requests from this device. Wait a few minutes, or ask the owner directly.", "", who)
		return
	}

	// The confirmation is the same whatever happened. An unknown or
	// switched-off account must not be distinguishable from a real one
	// by what this page says back.
	const confirmed = "Your request has been sent. Ask the station owner to open FuelMind and set you a new PIN."

	switch err := s.store.RequestPINReset(r.Context(), who, addr, note); {
	case err == nil:
		s.auditAnonymous(r, storage.ActionPINResetAsked, who,
			"Asked the owner for a new PIN"+noteSuffix(note))
		s.logger.Info("someone asked for a new PIN", "account", who, "from", addr)
		page(http.StatusOK, "", confirmed, who)
	case errors.Is(err, storage.ErrResetAlreadyRequested):
		page(http.StatusOK, "", "You have already asked. The owner can see it waiting for them.", who)
	case errors.Is(err, storage.ErrNotFound):
		// Deliberately the same answer as success.
		s.logger.Warn("a PIN reset was asked for an account that cannot sign in", "account", who, "from", addr)
		page(http.StatusOK, "", confirmed, who)
	default:
		s.renderError(w, "forgot: record request", err)
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

// approvePINReset is the owner clearing a request by setting a new PIN.
func (s *Server) approvePINReset(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
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
	me := userFrom(r.Context())
	if err := s.store.ResolvePINResets(r.Context(), id, me.ID, storage.PINResetDone); err != nil {
		s.logger.Warn("could not close the PIN request", "user", user.Username, "err", err)
	}
	s.audit(r, storage.ActionPINResetDone, user.Username,
		fmt.Sprintf("Set a new PIN for %s after they asked for one", user.Name()))
	return fmt.Sprintf("%s has a new PIN. Tell them what it is, and ask them to change it on the Account page.", user.Name()), ""
}

// dismissPINReset is the owner saying a request was not genuine.
func (s *Server) dismissPINReset(r *http.Request) (notice, formError string) {
	id, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		return "", "That account no longer exists."
	}
	user, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		return "", "That account no longer exists."
	}
	me := userFrom(r.Context())
	if err := s.store.ResolvePINResets(r.Context(), id, me.ID, storage.PINResetDismissed); err != nil {
		return "", err.Error()
	}
	s.audit(r, storage.ActionPINResetDenied, user.Username,
		fmt.Sprintf("Dismissed the PIN request from %s without changing anything", user.Name()))
	return fmt.Sprintf("Dismissed. %s still has their old PIN.", user.Name()), ""
}
