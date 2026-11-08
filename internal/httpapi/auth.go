package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
)

// loginRecorder stamps an account's last sign-in at a session mint
// (store.Store.RecordLogin; core.users.last_login). Optional: a store without
// it (a test fake) records nothing.
type loginRecorder interface {
	RecordLogin(ctx context.Context, email string, at time.Time) error
}

// recordLogin stamps the sign-in; a failure is logged, never fails the login.
func (s *Server) recordLogin(ctx context.Context, email string) {
	rec, ok := s.store.(loginRecorder)
	if !ok || strings.TrimSpace(email) == "" {
		return
	}
	if err := rec.RecordLogin(ctx, email, time.Now()); err != nil {
		logging.From(ctx).Warn("auth: record last login failed", "err", err)
	}
}

type externalSessionStore interface {
	// ExternalSessionEpoch creates or returns the generation; login mint only.
	ExternalSessionEpoch(ctx context.Context, issuer, subject, email string) (string, error)
	// LookupExternalSessionEpoch is the read-only request-path check.
	LookupExternalSessionEpoch(ctx context.Context, issuer, subject string) (string, error)
	RevokeExternalSessions(ctx context.Context, eventID, issuer, subject, email string) (string, bool, error)
}

type externalAccessStore interface {
	ExternalAccessState(context.Context, string, string) (store.ExternalAccessState, bool, error)
	ApplyExternalAccess(context.Context, store.ExternalAccessState) (store.ExternalAccessState, bool, error)
}

type externalTeamRenameStore interface {
	ApplyExternalTeamRename(context.Context, store.ExternalTeamRename, int64) (bool, int64, error)
}

// authMiddleware requires a principal that names a user: a browser session
// or a gateway sign-in leg from the gateway, or a credential the listener's
// edge resolved. It does not decide membership (membershipMiddleware does),
// so /auth/* legs that must answer for a not-yet-provisioned email can sit on
// it alone.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.requestPrincipal(w, r)
		if !ok {
			return
		}
		// Normalize once at the chokepoint, matching the store's
		// normalizeEmail. Conversation scoping, rate-limit buckets and
		// approvals all key off this value verbatim — without this,
		// Brad@x.com and brad@x.com both pass membership (the store
		// normalizes for that check) but get disjoint conversation
		// namespaces and separate rate-limit buckets.
		p.Email = strings.ToLower(strings.TrimSpace(p.Email))
		if p.Email == "" {
			http.Error(w, "missing user identity", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}

// requestPrincipal is the request's principal: the one the gateway attached
// in-process, else the one the listener's edge resolves from the credential
// the request presented (edgePrincipal, which writes the refusal itself).
func (s *Server) requestPrincipal(w http.ResponseWriter, r *http.Request) (principal.Principal, bool) {
	if p, ok := principal.From(r.Context()); ok {
		return p, true
	}
	return s.edgePrincipal(w, r)
}

// gatewayMiddleware admits only the gateway's own in-process calls. It
// guards the /auth/* legs that act on an email the caller names — the
// password check, the session-epoch mint, central Auth's verified access and
// session events — which a user principal must never reach: a user could
// otherwise mint an epoch or apply provider access for any address.
func (s *Server) gatewayMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.requestPrincipal(w, r)
		if !ok {
			return
		}
		if p.AuthMethod != principal.MethodGateway {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}

// gatewayUserMiddleware is gatewayMiddleware for a leg that names an email.
func (s *Server) gatewayUserMiddleware(next http.Handler) http.Handler {
	return s.gatewayMiddleware(s.authMiddleware(next))
}

// tokenOnlyMiddleware admits any principal but requires no user identity. It
// is for deployment-wide, non-secret, non-user-scoped data the pre-auth UI
// needs: /theme.css (the brand palette that themes the login page before a
// session exists), /brand/logo (the mark that page may render),
// /brand/share-image (the og:image anonymous unfurl scrapers fetch),
// /brand/meta (the app name, login copy, and share strings the shell and
// unfurl scrapers need), and /shared/ (whose authorization is the share token
// in the path). The gateway reaches them in-process with its own principal.
//
// The bar for adding a route here is that its response is public BY
// CONSTRUCTION — already visible to anyone who can load the login page or scrape
// a shared link — not merely non-sensitive-looking. /client-config stays
// member-gated because it also carries workspace content.
func (s *Server) tokenOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.requestPrincipal(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}

// providerEventMiddleware admits an identity provider's app-wide event the web
// tier has already verified (central Auth's signature, issuer, audience and
// freshness: internal/webui/backchannel.go). Such an event names no person,
// so only the gateway's own principal may deliver one.
func (s *Server) providerEventMiddleware(next http.Handler) http.Handler {
	return s.gatewayMiddleware(next)
}

// handleExternalTeamRename applies a team rename central Auth sent
// (docs/CENTRAL-AUTH-PROVISIONING.md, "Team renames"), already verified by
// the web tier: the share-keeping rename an admin's Rename team does, once
// per version. 204 when applied or already applied (Auth stops sending it),
// 400 for a malformed one, 500 when it could not be written (Auth retries).
func (s *Server) handleExternalTeamRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	renamer, ok := s.store.(externalTeamRenameStore)
	if !ok {
		http.Error(w, "team renames unavailable", http.StatusNotImplemented)
		return
	}
	var body struct {
		EventID string `json:"event_id"`
		Issuer  string `json:"issuer"`
		Version int64  `json:"version"`
		From    string `json:"from"`
		To      string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	applied, users, err := renamer.ApplyExternalTeamRename(r.Context(), store.ExternalTeamRename{
		Issuer: body.Issuer, Version: body.Version, EventID: body.EventID, From: body.From, To: body.To,
	}, time.Now().Unix())
	if err != nil {
		if store.IsInputError(err) {
			http.Error(w, "invalid team rename", http.StatusBadRequest)
			return
		}
		http.Error(w, "team rename failed", http.StatusInternalServerError)
		return
	}
	if applied {
		// The version is recorded in external_team_renames, not logged.
		logging.From(r.Context()).Info("central auth: team renamed", "from", safe.LogSafe(body.From), "to", safe.LogSafe(body.To), "accounts", users)
	}
	w.WriteHeader(http.StatusNoContent)
}

// userFromCtx is the request principal's email ("" when there is none).
func userFromCtx(ctx context.Context) string {
	return principal.Email(ctx)
}

// roleFromCtx returns the caller's RBAC role, as membershipMiddleware
// resolved it from the account row, or "" when unknown (the test seam path).
// "" is treated as a plain member by the gates — never as admin, so an
// un-resolved request can't escalate.
func roleFromCtx(ctx context.Context) string {
	p, _ := principal.From(ctx)
	return p.Role
}

// membershipMiddleware enforces the scoped-tier user-list gate. A request
// that already cleared authMiddleware (a principal naming a user) is admitted
// only if that email is an enabled account. This is what lets people
// authenticate via the shared elcano_auth cookie minted by the auth service
// while fleet keeps owning WHO may use chat — the cookie says who you are,
// this gate says whether you're allowed in.
//
// It is deliberately NOT folded into authMiddleware: /auth/verify (the
// password pre-login check) must stay reachable for not-yet-known emails,
// otherwise a 403 here would let the response be used to enumerate the
// user-list. Wrap user-data routes with this; leave /auth/verify on auth
// alone.
//
// The lookup is principal.Resolve, the same one the Operations Center runs:
// one read of the account's core.users row admits it, checks a session's
// epoch claim, and fills the principal's UserID and Role so downstream gates
// (rejectViewerWrites, adminMiddleware) don't re-query.
func (s *Server) membershipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, _ := principal.From(ctx)

		// Test seam: when isMember is injected, do a membership-only check and
		// skip role enrichment (the fake store embeds a nil *store.Store, so a
		// real lookup would panic). Production leaves isMember nil and takes
		// the resolving path below.
		if s.isMember != nil {
			ok, err := s.isMember(ctx, p.Email)
			if err != nil {
				http.Error(w, "membership check failed", http.StatusInternalServerError)
				return
			}
			if !ok {
				writeNotAMember(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		resolved, _, err := principal.Resolve(ctx, s.directory(), p, principal.Enabled)
		switch {
		case errors.Is(err, principal.ErrNotMember), errors.Is(err, principal.ErrNoIdentity):
			writeNotAMember(w)
			return
		case errors.Is(err, principal.ErrSessionRevoked):
			writeSessionRevoked(w)
			return
		case err != nil:
			http.Error(w, "membership check failed", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(principal.With(ctx, resolved)))
	})
}

// directory is the account lookup membershipMiddleware resolves against: the
// one identity table behind the chat store (set by New).
func (s *Server) directory() principal.Directory {
	if s.dir != nil {
		return s.dir
	}
	if cs, ok := s.store.(*store.Store); ok && cs != nil {
		return cs.Identity()
	}
	return missingDirectory{}
}

// missingDirectory fails every lookup: a store with no identity table cannot
// admit anyone.
type missingDirectory struct{}

func (missingDirectory) GetByEmail(context.Context, string) (*identity.User, error) {
	return nil, errors.New("no identity directory")
}

func (missingDirectory) LookupExternalSessionEpoch(context.Context, string, string) (string, error) {
	return "", errors.New("no identity directory")
}

// writeNotAMember emits the distinct, machine-readable 403 the web tier
// keys on to render the no-access page instead of bouncing back to login.
func writeNotAMember(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"not_a_member"}`))
}

// headerSessionRevoked marks the response below for the web tier's proxy funnel.
// The verdict has to be readable without touching the body, because that funnel
// streams the body straight through to the browser (chatServerPassthrough, the
// SSE routes) and consuming it there would break those callers.
const headerSessionRevoked = "X-Session-Revoked"

// writeSessionRevoked answers a request whose session-epoch claim no longer
// matches the account. 401 rather than the membership 403: the account is fine,
// the session is not, and re-authenticating is the fix — the web client's
// classifier already routes 401 to the login page, and the proxy funnel drops
// the stale cookie on the way out so /login does not bounce it straight back.
func writeSessionRevoked(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerSessionRevoked, "1")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"session_revoked"}`))
}

type sessionEpochResponse struct {
	SessionEpoch string `json:"session_epoch"`
}

// handleSessionEpoch serves GET /auth/session-epoch — the epoch the web tier
// stamps into a session cookie it is about to mint.
//
// It sits outside the membership gate, on the gateway-only chain:
// both mint paths run before any session exists, and an email chat has not
// provisioned must still get a well-formed answer rather than a 403 the login
// flow would have to special-case. Unlike /auth/verify this is not reachable
// from the login form, so it is not an enumeration surface: the only caller is
// the gateway itself (gatewayMiddleware), which can already assert any
// identity it likes.
func (s *Server) handleSessionEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	epoch, err := s.store.SessionEpoch(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, sessionEpochResponse{SessionEpoch: epoch})
}

type externalSessionRequest struct {
	EventID string `json:"event_id"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

func (s *Server) handleExternalSessionEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	externalStore, ok := s.store.(externalSessionStore)
	if !ok {
		http.Error(w, "external sessions unavailable", http.StatusNotImplemented)
		return
	}
	var body externalSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	epoch, err := externalStore.ExternalSessionEpoch(
		r.Context(), body.Issuer, body.Subject, userFromCtx(r.Context()),
	)
	if err != nil {
		http.Error(w, "external session lookup failed", http.StatusInternalServerError)
		return
	}
	// This is the OIDC login's mint step (the gateway asks for the epoch
	// only when it is about to issue the session).
	s.recordLogin(r.Context(), userFromCtx(r.Context()))
	writeJSON(w, r, sessionEpochResponse{SessionEpoch: epoch})
}

func (s *Server) handleExternalSessionRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	externalStore, ok := s.store.(externalSessionStore)
	if !ok {
		http.Error(w, "external sessions unavailable", http.StatusNotImplemented)
		return
	}
	var body externalSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, _, err := externalStore.RevokeExternalSessions(
		r.Context(), body.EventID, body.Issuer, body.Subject, userFromCtx(r.Context()),
	); err != nil {
		http.Error(w, "external session revocation failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type externalAccessRequest struct {
	EventID  string            `json:"event_id"`
	Issuer   string            `json:"issuer"`
	Subject  string            `json:"subject"`
	Action   string            `json:"action"`
	Version  int64             `json:"version"`
	IssuedAt int64             `json:"issued_at"`
	Settings map[string]string `json:"settings"`
}

// parseExternalSettings validates an access event's Fleet settings: exactly
// {chat_role, ops_role}, those two plus team, or plus team and team_rename;
// any other key fails closed. team is nil unless the provider manages the
// account's team (Auth sends it only with team sync on; without it Fleet
// leaves users.team_id alone). teamRename is the provider's last team rename
// when it wrote the team (store.ExternalAccessState.TeamRename), only ever
// next to a team. problem is the 400's text, "" when valid.
func parseExternalSettings(settings map[string]string) (chatRole, opsRole string, team *string, teamRename *int64, problem string) {
	chatRole, opsRole = settings["chat_role"], settings["ops_role"]
	wantKeys := 2
	if value, ok := settings["team"]; ok {
		wantKeys = 3
		value = strings.TrimSpace(value)
		if !store.ValidExternalTeam(value) {
			return "", "", nil, nil, "invalid Fleet team"
		}
		team = &value
		if raw, ok := settings["team_rename"]; ok {
			generation, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || generation < 0 {
				return "", "", nil, nil, "invalid Fleet team rename version"
			}
			wantKeys = 4
			teamRename = &generation
		}
	}
	// ops_role is Auth's contract and keeps its legacy names (none | readonly
	// | client | admin); the new scope names (view | run) are accepted too and
	// stored in the legacy form.
	legacyOps, okOps := store.LegacyExternalOpsRole(opsRole)
	if len(settings) != wantKeys || !store.ValidRole(chatRole) || !okOps ||
		((chatRole == "admin") != (legacyOps == "admin")) {
		return "", "", nil, nil, "invalid Fleet permissions"
	}
	return chatRole, legacyOps, team, teamRename, ""
}

// handleExternalAccess receives only already signature-verified events from
// the web tier. It applies the account's whole access — enabled, role,
// Operations Center scope and team — to its one identity row in one
// transaction (store.ApplyExternalAccess; #2049 M4, ADR-0105 amends
// ADR-0074's "eventually consistent" to atomic). There is no second plane to
// reconcile afterwards, so no "Ops Center provisioning failed" 503.
func (s *Server) handleExternalAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	accessStore, ok := s.store.(externalAccessStore)
	if !ok {
		http.Error(w, "external access unavailable", http.StatusNotImplemented)
		return
	}
	var body externalAccessRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	allowed := body.Action == "grant"
	if !allowed && body.Action != "revoke" {
		http.Error(w, "invalid access action", http.StatusBadRequest)
		return
	}
	previous, hasPrevious, err := accessStore.ExternalAccessState(r.Context(), body.Issuer, body.Subject)
	if err != nil {
		http.Error(w, "external access lookup failed", http.StatusInternalServerError)
		return
	}
	chatRole, opsRole := "", ""
	// team is nil unless the provider manages the account's team: Auth sends a
	// third settings key only with team sync on, and without it Fleet leaves
	// users.team_id alone (an older Auth, a backfill, team sync off).
	var team *string
	var teamRename *int64
	switch {
	case body.Settings != nil:
		var problem string
		if chatRole, opsRole, team, teamRename, problem = parseExternalSettings(body.Settings); problem != "" {
			http.Error(w, problem, http.StatusBadRequest)
			return
		}
	case hasPrevious:
		chatRole, opsRole = previous.ChatRole, previous.OpsRole
	default:
		chatRole, opsRole = store.RoleMember, "none"
		// Migration backfill carries no settings by design: preserve the roles
		// Fleet already owns instead of flattening existing administrators.
		if current, getErr := s.store.GetUser(r.Context(), userFromCtx(r.Context())); getErr == nil {
			chatRole = current.Role
			if role := current.LegacyOpsRole(); role != "" {
				opsRole = role
			}
		}
	}
	desired := store.ExternalAccessState{
		Issuer: body.Issuer, Subject: body.Subject, Email: userFromCtx(r.Context()),
		Version: body.Version, Allowed: allowed, EventID: body.EventID,
		ChatRole: chatRole, OpsRole: opsRole, Team: team, IssuedAt: body.IssuedAt,
		TeamRename: teamRename,
	}
	affectedConvIDs := s.activeMultiplayerAndPendingConvIDsBestEffort(r.Context())
	applied, _, err := accessStore.ApplyExternalAccess(r.Context(), desired)
	if err != nil {
		http.Error(w, "external access update failed", http.StatusInternalServerError)
		return
	}
	if !applied.Allowed {
		s.sessionApprovals.ForgetUser(applied.Email)
	}
	s.reconcileAllMultiplayerAccess(r.Context(), affectedConvIDs...)
	w.WriteHeader(http.StatusNoContent)
}

// rejectViewerWrites blocks the read-only "viewer" role (#237) from MUTATING a
// route it wraps, returning 403 {"error":"read_only"}. It is method-aware:
// safe methods (GET/HEAD/OPTIONS) always pass so a viewer keeps full read
// access, and only state-changing methods (POST/PATCH/PUT/DELETE) are gated.
// This lets it wrap the mixed read+write handlers (/conversations, …) without a
// per-method split at every call site. It runs AFTER membershipMiddleware so
// the role is in context; an un-enriched request (role "", the test seam) is
// treated as a non-viewer and passes — viewer is the only role this stops.
func (s *Server) rejectViewerWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if roleFromCtx(r.Context()) == store.RoleViewer {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"read_only"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleMembership is a no-op endpoint behind auth+membership. Reaching it
// (200) means the request principal is a provisioned user; a non-member is
// rejected by membershipMiddleware with 403 {"error":"not_a_member"} before
// this runs. The web tier's entry check hits it to decide whether to show the
// app or the no-access page for elcano_auth sessions.
func (s *Server) handleMembership(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"member":true}`))
}
