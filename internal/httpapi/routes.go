// Top-level route registration + the outermost HTTP middleware for the chat
// server. Split out of server.go (#1127); the registration order and
// middleware nesting in Routes are load-bearing and documented inline.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/otelsetup"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/version"
)

// Routes returns the top-level http.Handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	// Version discovery (#321): unauthenticated, like /healthz. Also served at
	// /v1/api-info via the apiversion.Router wrapper in cmd/fleet.
	mux.HandleFunc("/api-info", apiversion.InfoHandler(version.Version()))

	// auth = shared-secret + X-User-Email (identity). member adds the
	// scoped-tier user-list gate. /auth/verify stays on auth alone so the
	// password pre-login path can answer for not-yet-known emails without
	// leaking the user-list (see membershipMiddleware).
	auth := s.authMiddleware
	member := s.membershipMiddleware
	// mutate adds the read-only-role gate (#237): a "viewer" account may read
	// everything it could before but is 403'd on any state-changing method. It is
	// method-aware, so it safely wraps mixed read+write handlers. It must sit
	// INSIDE member (member enriches the role mutate reads).
	mutate := s.rejectViewerWrites
	mux.Handle("/chat", auth(member(mutate(s.rateLimitMiddleware(http.HandlerFunc(s.postChat))))))
	mux.Handle("/attachments", auth(member(mutate(s.rateLimitMiddleware(http.HandlerFunc(s.postAttachments))))))
	mux.Handle("/conversations", auth(member(mutate(http.HandlerFunc(s.listOrCreateConversations)))))
	mux.Handle("/conversations/", auth(member(mutate(http.HandlerFunc(s.conversationByID)))))
	mux.Handle("/search", auth(member(http.HandlerFunc(s.search))))
	// Projects / Spaces (#509): shared team workspaces binding instructions +
	// curated connectors + shared memory + membership (team RBAC).
	mux.Handle("/projects", auth(member(mutate(http.HandlerFunc(s.projects)))))
	mux.Handle("/projects/", auth(member(mutate(http.HandlerFunc(s.projectByID)))))
	mux.Handle("/memories", auth(member(mutate(http.HandlerFunc(s.memories)))))
	// Knowledge graph (#523): the exact pattern outranks the /memories/ prefix
	// in ServeMux matching, so "graph" is never mistaken for a memory id.
	// Read-only, hence no mutate wrapper (like /search).
	mux.Handle("/memories/graph", auth(member(http.HandlerFunc(s.memoryGraph))))
	mux.Handle("/memories/", auth(member(mutate(http.HandlerFunc(s.memoryByID)))))
	mux.Handle("/personas", auth(member(http.HandlerFunc(s.listPersonas))))
	// Bundle skill roster (#513 phase 1): name + description per skill, for the
	// composer "/" autocomplete. Read-only over the operator-owned bundle.
	mux.Handle("/skills", auth(member(http.HandlerFunc(s.listSkills))))
	mux.Handle("/skills/", auth(member(http.HandlerFunc(s.skillByName))))
	// User-authored skills (the builder, docs/SKILLS.md phase 2).
	mux.Handle("/user-skills", auth(member(mutate(http.HandlerFunc(s.userSkillsCollection)))))
	mux.Handle("/user-skills/", auth(member(mutate(http.HandlerFunc(s.userSkillByID)))))
	// Dynamic model discovery (#251): the model catalog, routed through the
	// backend so the API key stays server-side and the allow-list is applied
	// before the response reaches the browser.
	mux.Handle("/api/v1/models", auth(member(http.HandlerFunc(s.handleModels))))
	mux.Handle("/mcp-servers", auth(member(http.HandlerFunc(s.listMCPServerCatalog))))
	// Trust-labeled MCP directory (#538): bundled connectors vs curated
	// third-party hosted servers, for the settings catalog UI.
	mux.Handle("/mcp-catalog", auth(member(http.HandlerFunc(s.mcpCatalog))))
	// Per-user connector availability prefs (unified connector UX).
	mux.Handle("/connector-prefs", auth(member(mutate(http.HandlerFunc(s.connectorPrefs)))))
	// Per-user remote (hosted) MCP servers + OAuth (#443). The /oauth/mcp/callback
	// completion is POSTed here by the browser-facing Next.js callback route.
	mux.Handle("/remote-mcp-servers", auth(member(mutate(http.HandlerFunc(s.remoteMCPServers)))))
	mux.Handle("/remote-mcp-servers/", auth(member(mutate(http.HandlerFunc(s.remoteMCPServerByID)))))
	mux.Handle("/oauth/mcp/callback", auth(member(mutate(http.HandlerFunc(s.remoteMCPOAuthCallback)))))
	// Browser Web Push (#292): subscribe/unsubscribe manage only the caller's
	// own rows; the VAPID public key is a non-secret read. All three answer 501
	// until the operator configures VAPID keys (fleet generate-vapid-keys).
	mux.Handle("/push/subscribe", auth(member(mutate(http.HandlerFunc(s.pushSubscribe)))))
	mux.Handle("/push/unsubscribe", auth(member(mutate(http.HandlerFunc(s.pushUnsubscribe)))))
	mux.Handle("/push/vapid-public-key", auth(member(http.HandlerFunc(s.pushVAPIDPublicKey))))
	mux.Handle("/server-config", auth(member(http.HandlerFunc(s.serverConfig))))
	mux.Handle("/client-config", auth(member(http.HandlerFunc(s.clientConfigHandler))))
	// /theme.css themes the shell (incl. the pre-auth login page) from the
	// bundle palette, so it is token-gated but identity-less — see themeCSS.
	mux.Handle("/theme.css", s.tokenOnlyMiddleware(http.HandlerFunc(s.themeCSS)))
	// /brand/logo is the same trust class as /theme.css — a deployment-wide,
	// non-secret brand asset the pre-auth shell may render — so it shares the
	// token-gated, identity-less chain. 404 when the bundle declares no logo.
	mux.Handle("/brand/logo", s.tokenOnlyMiddleware(http.HandlerFunc(s.brandLogo)))
	// /brand/share-image is the bundle's og:image. Same trust class again, and
	// necessarily so: link-unfurl scrapers (Slack, iMessage, Discord, Teams) are
	// anonymous, so an og:image behind a session gate would render no preview at
	// all. 404 when the bundle declares none, and the web falls back to fleet's
	// own neutral card.
	mux.Handle("/brand/share-image", s.tokenOnlyMiddleware(http.HandlerFunc(s.brandShareImage)))
	// /brand/meta is the deployment's white-label identity (name, login copy,
	// share strings, per-mode background) for the web's SERVER-SIDE metadata.
	// Same trust class again: the login card renders pre-session and unfurl
	// scrapers are anonymous, so /client-config's member gate is unreachable
	// from those surfaces. Every field is public by construction — see
	// brand_meta.go.
	mux.Handle("/brand/meta", s.tokenOnlyMiddleware(http.HandlerFunc(s.brandMeta)))
	// Public read-only conversation sharing (#226). Token-gated (shared secret —
	// only the trusted Next proxy reaches it) but IDENTITY-less, like /theme.css:
	// the share token in the path is the authorization, and the handler enforces
	// its own per-token rate limit + expiry. The Next layer exposes /shared/{token}
	// to logged-out viewers.
	mux.Handle("/shared/", s.tokenOnlyMiddleware(http.HandlerFunc(s.handleSharedConversation)))
	// Inbound webhook-triggered conversations (#268). Like /shared, this is
	// registered OUTSIDE the auth(member(mutate(…))) chain because external
	// callers (GitHub, Slack, CI) cannot present a Fleet session token — the
	// per-trigger HMAC / Slack signing secret proves authenticity instead (see
	// postWebhook + docs/adr/0016). It is EXEMPT from the OpenAPI route-parity
	// test, which walks only the orchestrator chi router, not this chat mux.
	mux.Handle("/webhooks/", http.HandlerFunc(s.postWebhook))
	mux.Handle("/auth/membership", auth(member(http.HandlerFunc(s.handleMembership))))
	// Self-serve identity + team (#1157): /me reports the caller's own role and
	// team; PUT /me/team creates-or-leaves a team without an admin round trip
	// (joining an existing one stays admin-gated inside store.SetOwnTeam).
	// mutate keeps a read-only viewer read-only here too.
	mux.Handle("/me", auth(member(http.HandlerFunc(s.handleMe))))
	mux.Handle("/me/team", auth(member(mutate(http.HandlerFunc(s.handleMyTeam)))))
	mux.Handle("/auth/verify", auth(http.HandlerFunc(s.handleAuthVerify)))
	// /auth/session-epoch is on auth alone for the same reason as /auth/verify:
	// the Next.js mint paths call it before a session exists, and it must answer
	// for a not-yet-provisioned email without leaking the user-list.
	mux.Handle("/auth/session-epoch", auth(http.HandlerFunc(s.handleSessionEpoch)))
	mux.Handle("/admin/stats", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminStats)))))
	mux.Handle("/admin/health-summary", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleHealthSummary)))))
	mux.Handle("/admin/server-stats", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleServerStats)))))
	// Storage visibility + reclaim (uploads / temp files / workspaces /
	// old unpinned chats). Read is a walk of the data trees; the cleanup
	// POST is destructive but only for cleanup-eligible rows (pinned /
	// archived / shared / project chats are never touched).
	mux.Handle("/admin/storage", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminStorage)))))
	mux.Handle("/admin/storage/cleanup", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminStorageCleanup)))))
	mux.Handle("/admin/doctor", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleDoctor)))))
	// Admin Users tab (#237): GET list / POST create on the collection;
	// PATCH role-team / DELETE / PUT …/password on the item. Admin-gated like
	// the other /admin/* endpoints; role writes also drive the ops-center seam.
	mux.Handle("/admin/users", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminUsers)))))
	mux.Handle("/admin/users/", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminUserItem)))))
	// Team rename: relabels users.team_id + projects.team_id atomically.
	mux.Handle("/admin/teams/rename", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminTeamRename)))))
	// Migration status (#256): applied vs pending chat-DB migrations. Admin-gated
	// like the other /admin/* reads; strictly read-only (applies nothing).
	mux.Handle("/admin/migrations", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleMigrations)))))
	// Admin-managed LLM providers: CRUD is admin-gated; the names+models read
	// the model picker unions in is member-level (no secret material).
	mux.Handle("/admin/llm-providers", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminLLMProviders)))))
	mux.Handle("/admin/llm-providers/", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminLLMProviderItem)))))
	mux.Handle("/llm-provider-models", auth(member(http.HandlerFunc(s.handleLLMProviderModels))))
	// Admin-managed workspace feature settings (internal/settings): the Features
	// panel. Admin-gated like the rest of /admin/*; values are feature toggles
	// only, never secrets.
	mux.Handle("/admin/settings", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminSettings)))))
	mux.Handle("/admin/settings/", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminSettingItem)))))
	// Admin-managed task notification settings (internal/notifyadmin): secrets
	// are write-only + sealed at rest; the test endpoint sends one real
	// delivery attempt host-side.
	mux.Handle("/admin/notify-settings", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminNotifySettings)))))
	mux.Handle("/admin/notify-settings/test", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminNotifySettingsTest)))))
	// PII redaction probe: run the live redactor over a synthetic sample.
	mux.Handle("/admin/pii-redaction/test", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminPIIProbe)))))
	mux.Handle("/admin/guardrail/test", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminGuardrailProbe)))))
	// One-click Rampart service install (build + run + supervise via podman).
	mux.Handle("/admin/pii-redaction/install", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminPIIInstall)))))
	// ipFilterMiddleware (#314) is the outermost application-layer filter: it sits
	// just inside recoverMiddleware and before bodyLimitMiddleware, so a blocked
	// client IP is dropped before any body parsing, route dispatch, or auth
	// comparison. It is a no-op (returns next unwrapped) when no allow/deny lists
	// are configured, so default behavior is unchanged. /healthz stays exempt.
	// otelsetup.Middleware (#186) wraps the routed mux so the server span sees the
	// matched route pattern; it sits inside bodyLimitMiddleware (after recover/IP
	// filter/body cap) and is a no-op-cost non-recording span when tracing is off,
	// while always setting the X-Request-Id response header.
	return recoverMiddleware(s.ipFilterMiddleware(bodyLimitMiddleware(otelsetup.Middleware(mux))))
}

// maxJSONBodyBytes caps non-upload request bodies on the chat server, matching
// the orchestrator's MaxJSONBodySize (both servers boot in the same process). A
// chat message plus attachment METADATA (the bytes go through /attachments,
// which sets its own multipart cap) fits comfortably; this just removes a
// post-auth single-request OOM lever on the single-host box.
const maxJSONBodyBytes = 1 << 20 // 1 MB

func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			// /attachments has its own (larger) multipart cap; don't double-limit.
			if !strings.HasPrefix(r.URL.Path, "/attachments") {
				r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware converts a panic in a SYNCHRONOUS chat handler into a 500
// rather than letting it crash the single-host process. (The detached turn
// goroutine has its own recovery; see runTurnAsync.) This mirrors the chi
// middleware.Recoverer the orchestrator router already uses.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec) // a deliberate abort is the server's to handle
				}
				// Same structured emission + counter + hooks every recovered panic
				// gets (#241), labeled with the request method+path for correlation.
				safe.EmitPanic("httpapi.handler "+r.Method+" "+r.URL.Path, rec, debug.Stack())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
