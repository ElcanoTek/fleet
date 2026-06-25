package handlers

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

type contextKey string

const (
	nodeContextKey   = contextKey("node")
	tokenContextKey  = contextKey("token")
	userContextKey   = contextKey("user")
	apiKeyContextKey = contextKey("apiKey")
)

// AdminAuthMiddleware requires the X-API-Key header to match the configured Admin API Key.
func (h *Handlers) AdminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.verifyAdminKey(r) {
			writeError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AdminOrUserAuthMiddleware allows access with either an admin API key, a scoped API key, or a user token.
func (h *Handlers) AdminOrUserAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check admin API key first
		if h.verifyAdminKey(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Next-proxy header-trust path (#157). The Next.js layer is the SOLE
		// client of this backend: it verifies the user's session cookie, then
		// forwards the identity as X-User-Email guarded by the shared
		// X-Orchestrator-Server-Token (the chat-server token; see Config.SharedToken).
		// This is what lets a /chat-cookie user open the Operations Center without
		// a second (moc bearer) login. The token is impersonation-load-bearing, so
		// this is fail-closed: a PRESENT-but-wrong token is rejected outright (no
		// fall-through to the weaker scoped-key/bearer/cookie paths). Only an ABSENT
		// token continues to those direct-client paths below. Mirrors chat-server's
		// authMiddleware (shared token + X-User-Email, then the membership gate).
		if tok := r.Header.Get("X-Orchestrator-Server-Token"); tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(h.config.SharedToken)) != 1 {
				writeError(w, http.StatusForbidden, "Forbidden")
				return
			}
			email := strings.ToLower(strings.TrimSpace(r.Header.Get("X-User-Email")))
			if email == "" {
				writeError(w, http.StatusBadRequest, "missing X-User-Email")
				return
			}
			user, err := h.lookupMember(r.Context(), email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, "Membership check failed")
				return
			}
			if user == nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Check for scoped API key (X-API-Key header that's not admin)
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "" {
			perm := models.PermissionViewTasks
			valid, key, _ := h.apiKeys.ValidateKey(apiKey, &perm, nil, nil, nil)
			if valid && key != nil {
				// Store the API key in context for the handler to use
				ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// Check for user Bearer token (username/password login path).
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			user, err := h.storage.GetUserByToken(token)
			if err == nil && user != nil {
				ctx := context.WithValue(r.Context(), userContextKey, user)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// Invalid bearer token: fall through to the cookie path before 401.
		}

		// Check for the Elcano unified-auth cookie ("Use Elcano email"). Scoped
		// tier: the cookie proves identity (verified natively with the Ed25519
		// public key); the email must also be in the user-list, otherwise
		// 403 not_a_member — they're signed in, just not provisioned here.
		if sess := h.elcanoSessionFromRequest(r); sess != nil {
			user, err := h.lookupMember(r.Context(), sess.Email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, "Membership check failed")
				return
			}
			if user == nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		writeError(w, http.StatusUnauthorized, "Unauthorized")
	})
}

// NodeAuthMiddleware requires a valid X-API-Key for a registered node.
// It places the *models.Node in the context.
func (h *Handlers) NodeAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, err := h.verifyNodeKey(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "Invalid node API key")
			return
		}
		ctx := context.WithValue(r.Context(), nodeContextKey, node)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RegistrationAuthMiddleware checks for the registration token.
func (h *Handlers) RegistrationAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.verifyRegistrationToken(r); err != nil {
			if strings.Contains(err.Error(), "disabled") {
				writeError(w, http.StatusServiceUnavailable, "Registration is disabled. Set REGISTRATION_TOKEN environment variable.")
			} else {
				writeError(w, http.StatusUnauthorized, "Invalid registration token")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetNodeFromContext retrieves the node from the context.
func GetNodeFromContext(ctx context.Context) *models.Node {
	node, _ := ctx.Value(nodeContextKey).(*models.Node)
	return node
}

// GetUserFromContext retrieves the user from the context.
func GetUserFromContext(ctx context.Context) *models.User {
	user, _ := ctx.Value(userContextKey).(*models.User)
	return user
}

// GetAPIKeyFromContext retrieves the API key from the context.
func GetAPIKeyFromContext(ctx context.Context) *apikeys.APIKey {
	key, _ := ctx.Value(apiKeyContextKey).(*apikeys.APIKey)
	return key
}

// RateLimitMiddleware applies rate limiting for registration.
func (h *Handlers) RateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		if !h.regRateLimiter.Allow(clientIP) {
			writeError(w, http.StatusTooManyRequests, "Rate limit exceeded. Try again later.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MaxJSONBodySize is the maximum size of JSON request bodies (1MB)
const MaxJSONBodySize = 1 << 20 // 1 MB

// BodySizeLimitMiddleware limits the size of request bodies to prevent DoS attacks
func (h *Handlers) BodySizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only limit POST, PUT, PATCH requests
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
			// Skip for file uploads (they have their own limit in HandleUpload)
			if !strings.HasPrefix(r.URL.Path, "/upload") {
				r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodySize)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// SecurityHeadersMiddleware adds security headers including CSP
func (h *Handlers) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content Security Policy
		csp := "default-src 'self'; " +
			"script-src 'self'; " + // unsafe-inline removed
			"style-src 'self' 'unsafe-inline'; " + // Allow inline styles for Chart.js and other libraries
			"font-src 'self'; " +
			"img-src 'self' data:; " +
			// openrouter.ai is required by the model picker, which fetches the
			// public /api/v1/models catalog directly from the user's browser.
			"connect-src 'self' https://openrouter.ai; " +
			"frame-ancestors 'none'; " +
			"base-uri 'self'; " +
			"form-action 'self'"
		w.Header().Set("Content-Security-Policy", csp)

		// Other security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		next.ServeHTTP(w, r)
	})
}

// CSRFMiddleware verifies CSRF token for state-changing operations
func (h *Handlers) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip CSRF check for GET, HEAD, OPTIONS (safe methods)
		if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip CSRF check for API endpoints using token-based authentication.
		// Token-based auth is not vulnerable to CSRF because tokens are explicitly
		// set by the client and not automatically included by the browser like cookies.

		// Skip for API key authentication
		if r.Header.Get("X-API-Key") != "" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip for registration token authentication
		if r.Header.Get("X-Registration-Token") != "" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip for Bearer token authentication
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}

		// Skip CSRF for login endpoint (it creates a session, doesn't require one)
		if r.URL.Path == "/auth/login" {
			next.ServeHTTP(w, r)
			return
		}

		// Cookie/session requests: verify the Origin matches our host. This is
		// stateless (no token store, nothing to go stale on restart) and mirrors
		// chat's CSRF defense: browsers always send Origin on cross-origin
		// mutating requests and can't be tricked into forging it, while
		// same-origin dashboard requests carry the right Origin automatically.
		// API-key / registration / bearer requests are already exempt above
		// (not browser-auto-sent), so this only gates the elcano_auth cookie.
		if !originMatchesHost(r) {
			writeError(w, http.StatusForbidden, "Cross-origin request blocked")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// originMatchesHost reports whether the request's Origin header host matches
// the host the user is reaching the server at (X-Forwarded-Host when behind
// Caddy, else Host). A missing or malformed Origin on a mutating request is
// treated as cross-origin and rejected — real browsers always send Origin on
// POST/PUT/DELETE/PATCH. Stateless CSRF defense, mirroring chat's verifyOrigin.
func originMatchesHost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	expected := r.Header.Get("X-Forwarded-Host")
	if expected == "" {
		expected = r.Host
	}
	return strings.EqualFold(u.Host, expected)
}
