package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
	authn "github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

type contextKey string

const (
	tokenContextKey  = contextKey("token")
	userContextKey   = contextKey("user")
	apiKeyContextKey = contextKey("apiKey")
)

// AdminAuthMiddleware guards the admin-only route group (/keys, /users,
// /metrics, /admin/*, …). It accepts the bootstrap ADMIN_API_KEY or a
// hash-verified typed admin key (fleet_admin_…, #1081) — the admin type
// carries PermissionAdmin and exists for exactly these routes, so rejecting
// it here left a key type that could not do the thing its name claims. The
// gate stays type-based and does NOT widen to the other classes: a valid key
// of any other type (task/readonly/webhook/legacy) is a definitive 403,
// mirroring AdminOrUserAuthMiddleware's under-scope verdict, and an unknown
// or absent key stays 401.
func (h *Handlers) AdminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.verifyAdminKey(r) {
			ctx := authn.With(r.Context(), authn.Principal{AuthMethod: authn.MethodAdminKey})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
			valid, key, _ := h.apiKeys.ValidateKeyOn(apikeys.SurfaceOrchestrator, apiKey, nil, nil, nil)
			if valid && key != nil {
				if key.Type == apikeys.KeyTypeAdmin {
					ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
					ctx = authn.With(ctx, keyPrincipal(key))
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				writeError(w, http.StatusForbidden, "insufficient key scope for this route")
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "Invalid API key")
	})
}

// userMayMutate is the ONE role gate for a user principal (a gateway
// session; the password bearer is retired, #2049 M4) on the
// orchestrator API. Reads are open to
// every role; a mutating method needs a role whose permission set
// (models.RolePermissions) carries PermissionCreateTask — admin and client do,
// readonly does not. "Mutating" is exactly the method set a readonly typed key
// is refused on (apikeys.TypeAllowsMethod for KeyTypeReadonly), so a readonly
// user and a readonly key draw the same line.
//
// Before this existed the user paths authenticated without consulting the
// role at all: per-handler permission checks covered some routes, but POST
// /tasks, /tasks/batch, /tasks/estimate, /upload, task feedback and own-task
// cancel/delete/resume accepted a readonly user outright. The gate runs
// wherever a user principal is resolved — userPrincipal, which every user
// path goes through — so no handler can forget it. API keys never reach it: their
// scope is the key type's, decided on the key paths, unchanged.
func userMayMutate(r *http.Request, user *models.User) bool {
	if apikeys.TypeAllowsMethod(apikeys.KeyTypeReadonly, r.Method) {
		return true
	}
	return userHasPermission(user, models.PermissionCreateTask)
}

// refuseReadonlyUserMutation writes the 403 for a user principal userMayMutate
// rejects and reports whether it did; on true the caller must stop.
func refuseReadonlyUserMutation(w http.ResponseWriter, r *http.Request, user *models.User) bool {
	if userMayMutate(r, user) {
		return false
	}
	writeError(w, http.StatusForbidden, "insufficient role: a readonly user cannot modify resources")
	return true
}

// headerTrustMember is the membership verdict for a user principal:
// the account's one identity row (nil when there is none) decides, as the
// Operations Center user it is (Role = the legacy Operations Center role), or
// nil for not_a_member.
//
// The Prompt Library is shared with chat (composer picker, "Save as
// workflow…", Settings → Prompts), so on /prompts a signed-in chat user with
// no — or only view — Operations Center access is authorized by their Fleet
// role: member as run-like, viewer as view-like. That is the rule the old
// chat-role seam applied, now read from the same row (#2049 M4). Pure, so the
// authorization differential test runs every account state through it.
func headerTrustMember(account *identity.User, promptLibrary bool) *models.User {
	var user *models.User
	if account.HasOpsAccess() {
		user = opsModel(account)
	}
	if (user == nil || user.Role == identity.LegacyOpsReadonly) && promptLibrary {
		if chatUser := promptLibraryUser(account); chatUser != nil && (user == nil || chatUser.Role != identity.LegacyOpsReadonly) {
			user = chatUser
		}
	}
	return user
}

// AdminOrUserAuthMiddleware allows access with an admin API key, a scoped API
// key, or a user principal (a gateway session).
func (h *Handlers) AdminOrUserAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check admin API key first
		if h.verifyAdminKey(r) {
			ctx := authn.With(r.Context(), authn.Principal{AuthMethod: authn.MethodAdminKey})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// The user principal (#2050): see userPrincipal.
		if user, r2, handled := h.userPrincipal(w, r); handled {
			if user == nil {
				return // rejected fail-closed; response already written
			}
			next.ServeHTTP(w, r2)
			return
		}

		// Check for scoped API key (X-API-Key header that's not admin).
		//
		// Validate for VALIDITY only (nil perm), then apply the access decision:
		//   - Typed keys (#190): the key's type gates the route by method via
		//     TypeAllowsMethod. A valid but under-scoped typed key (e.g. a webhook
		//     key on a read route, or a readonly key on a mutation) is a DEFINITIVE
		//     403 — not a fall-through to the weaker bearer/cookie paths.
		//   - Legacy (untyped sk-) keys keep their exact historical behavior: these
		//     routes required PermissionViewTasks, so without it the request falls
		//     through (→ 401) as before. Adopting typed keys never changes how a
		//     deployed sk- key behaves.
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "" {
			valid, key, _ := h.apiKeys.ValidateKeyOn(apikeys.SurfaceOrchestrator, apiKey, nil, nil, nil)
			if valid && key != nil {
				authorized := false
				switch {
				case key.Type == apikeys.KeyTypeLegacy:
					authorized = key.HasPermission(models.PermissionViewTasks)
				case apikeys.TypeAllowsMethod(key.Type, r.Method):
					authorized = true
				default:
					writeError(w, http.StatusForbidden, "insufficient key scope for this route")
					return
				}
				if authorized {
					// Store the API key in context for the handler to use.
					ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
					ctx = authn.With(ctx, keyPrincipal(key))
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				// Legacy key without ViewTasks: fall through to bearer (→ 401).
			}
		}

		// No password bearer: it is retired (#2049 M4, ADR-0105).
		writeError(w, http.StatusUnauthorized, "Unauthorized")
	})
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

// MaxJSONBodySize is the maximum size of JSON request bodies (1MB)
const MaxJSONBodySize = 1 << 20 // 1 MB

// BodySizeLimitMiddleware limits the size of request bodies to prevent DoS attacks
func (h *Handlers) BodySizeLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only limit POST, PUT, PATCH requests
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
			// Skip for file uploads and dataset row imports: each has its own,
			// larger limit in its handler (HandleUpload; ImportDatasetRows'
			// maxImportBytes), and this 1 MiB cap wrapped first would govern
			// instead — the import's cap was dead code and a 2 MiB CSV failed
			// with the JSON-body message.
			if !strings.HasPrefix(r.URL.Path, "/upload") && !isDatasetImportPath(r.URL.Path) {
				r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodySize)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isDatasetImportPath reports whether path is POST /datasets/{id}/rows — the
// one dataset route carrying a bulk body. Matched structurally (exactly four
// segments, "datasets" and "rows" in place) rather than by prefix so no other
// /datasets route slips out from under the global body cap.
func isDatasetImportPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 3 && parts[0] == "datasets" && parts[2] == "rows"
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

		// Skip for Bearer token authentication
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}

		// Skip CSRF for webhook trigger endpoints (#177): authenticated by a
		// per-trigger HMAC-SHA256 signature the external caller supplies, not a
		// browser-auto-sent cookie, so they are not CSRF-vulnerable.
		if strings.HasPrefix(r.URL.Path, "/triggers/") {
			next.ServeHTTP(w, r)
			return
		}

		// Skip CSRF for the A2A JSON-RPC endpoint (#1279): its dispatcher
		// authenticates with X-API-Key ONLY (a2aPrincipal) — no cookie and no
		// bearer path — so there is no browser-auto-sent credential a
		// cross-site request could ride. Origin-less non-browser callers (the
		// normal A2A client) would otherwise be rejected by the cookie-CSRF
		// origin check below despite never using cookie auth.
		if r.URL.Path == "/a2a" || r.URL.Path == "/a2a/" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip for the gateway's in-process calls (#2050). A principal in the
		// context can only have been put there by fleet code — no network peer
		// writes a request context — and the gateway attaches one only after it
		// enforced the browser's Origin itself (internal/webui), WITHOUT
		// forwarding the Origin header.
		if p, ok := authn.From(r.Context()); ok && p.IsBrowserSession() {
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

// keyPrincipal is the principal of a validated API key: a key names no user,
// so it carries its id and permissions only.
func keyPrincipal(key *apikeys.APIKey) authn.Principal {
	scopes := make([]string, 0, len(key.Permissions))
	for _, perm := range key.Permissions {
		scopes = append(scopes, string(perm))
	}
	return authn.Principal{AuthMethod: authn.MethodAPIKey, KeyID: key.KeyID, Scopes: scopes}
}
