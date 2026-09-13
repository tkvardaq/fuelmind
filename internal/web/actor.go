package web

import (
	"context"
	"net/http"

	"github.com/fuelmind/fuelmind/internal/storage"
)

// ctxUser carries the signed-in person through the request.
type ctxUser struct{}

// userFrom returns who is making this request. The zero User means
// nobody is signed in, which only happens on the pages that do not
// require a session.
func userFrom(ctx context.Context) storage.User {
	u, _ := ctx.Value(ctxUser{}).(storage.User)
	return u
}

// withUser puts the signed-in person on the request context.
func withUser(ctx context.Context, u storage.User) context.Context {
	return context.WithValue(ctx, ctxUser{}, u)
}

// requireOwner wraps a handler so only an owner reaches it. Staff can
// record what happened on their shift; changing how the station is set
// up — where data comes from, what is paid per litre, who has an
// account — is the owner's.
func (s *Server) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r.Context()).IsOwner() {
			s.renderStatus(w, r, http.StatusForbidden, "notfound", map[string]any{
				"LoggedIn": true,
				"Title":    "Only the owner can open this page",
				"Message":  "Ask the station owner to make this change, or to give your account owner access.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// audit writes one line to the activity log: who did what, from where.
//
// It never fails a request. The change the owner just made has already
// happened; refusing to show them the result because the log write
// failed would be the worse outcome. A failure is logged loudly instead.
func (s *Server) audit(r *http.Request, action, subject, detail string) {
	u := userFrom(r.Context())
	err := s.store.RecordAudit(r.Context(), storage.AuditEntry{
		UserID:     u.ID,
		ActorName:  u.Name(),
		ActorRole:  u.Role,
		Source:     storage.AuditSourceDashboard,
		Action:     action,
		Subject:    subject,
		Detail:     detail,
		RemoteAddr: clientIP(r),
	})
	if err != nil {
		s.logger.Error("could not record who made this change",
			"action", action, "actor", u.Name(), "err", err)
	}
}

// auditAnonymous records something that happened before anyone was
// signed in — a failed login, most usefully, which is exactly the entry
// nobody is around to attribute.
func (s *Server) auditAnonymous(r *http.Request, action, subject, detail string) {
	err := s.store.RecordAudit(r.Context(), storage.AuditEntry{
		ActorName:  subject,
		ActorRole:  "",
		Source:     storage.AuditSourceDashboard,
		Action:     action,
		Subject:    subject,
		Detail:     detail,
		RemoteAddr: clientIP(r),
	})
	if err != nil {
		s.logger.Error("could not record a sign-in attempt", "action", action, "err", err)
	}
}
