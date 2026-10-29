// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Sign-out of the shared Elcano cookie, and the membership lookup the
// header-trust path gates on.
//
// The browser tier (internal/webui) verifies every session, the elcano_auth
// cookie included, and forwards a trusted identity here (header trust,
// middleware.go); this server never reads that cookie itself.

package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// lookupMember resolves an email to a user for the header-trust membership
// gate. In production it hits the users table (username == lowercase email);
// tests inject memberLookup to avoid a live database.
func (h *Handlers) lookupMember(ctx context.Context, email string) (*models.User, error) {
	if h.memberLookup != nil {
		return h.memberLookup(ctx, email)
	}
	return h.storage.GetUserByUsernameWithContext(ctx, strings.ToLower(email))
}

// ElcanoLogout handles GET /auth/logout. It deletes the shared elcano_auth
// cookie itself (rather than bouncing to auth) and returns the user to the
// server's own login, mirroring chat's logout. Because the cookie lives on the
// shared parent domain, deleting it signs the user out of EVERY Elcano service
// — the expected meaning of "log out". The deletion only takes effect when
// name + domain + path match how auth set the cookie, so we mirror them here.
func (h *Handlers) ElcanoLogout(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	// G124: HttpOnly + SameSite are always set; Secure is conditional ON the
	// request being HTTPS so plain-HTTP local dev still works. This is a
	// DELETION cookie (Value="", MaxAge=-1) carrying no secret, and its
	// attributes must mirror how auth originally set the cookie for the browser
	// to actually clear it — forcing Secure here unconditionally would prevent
	// logout from clearing the cookie over plain HTTP.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure -- same reasoning as the G124 waiver: this is a DELETION cookie (Value="", MaxAge=-1) carrying no secret, and its attributes must mirror how auth set it or the browser will not clear it. Forcing Secure unconditionally would break logout over plain-HTTP dev.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deletion cookie (no secret); Secure is conditional on HTTPS so logout works over plain-HTTP dev, mirroring how the cookie was set — see comment above.
		Name:     h.config.ElcanoCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.config.ElcanoCookieDomain, // "" → host-only (Go omits the attr)
		MaxAge:   -1,                          // delete now
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
