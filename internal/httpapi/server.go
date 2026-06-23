package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Server wires the agent Manager + store + shared-secret auth into an
// http.Handler that Next.js talks to.
type Server struct {
	cfg           *config.Config
	agent         turnEngine
	store         chatStore
	sharedToken   string
	rate          *rateLimiter
	hasUsers      atomic.Bool
	lastUserCheck atomic.Int64

	// isMember reports whether an email may use chat — the scoped-tier
	// gate consulted by membershipMiddleware. nil in production, where it
	// falls back to store.IsUser. Tests whose subject isn't membership
	// override it to allow-all so fixtures needn't provision every email;
	// membership_test points it back at the real store.IsUser.
	isMember func(ctx context.Context, email string) (bool, error)

	// inflight tracks the currently-running turn for each conversation.
	// Lets the server keep generating after the SSE connection drops
	// (so phone-lock + long agent turns don't lose work) while still
	// honoring an explicit Stop from the client via
	// POST /conversations/{id}/cancel.
	//
	// Each entry carries a monotonic token so a turn whose handler is
	// cleaning up doesn't accidentally clobber a fresher entry that
	// another submit installed in the meantime.
	inflightMu      sync.Mutex
	inflight        map[string]inflightEntry
	inflightCounter uint64

	// clientConfig is the loaded client bundle that backs GET /client-config
	// (branding + empty-state). nil in tests / mock mode that don't supply one;
	// the endpoint then returns neutral generic defaults.
	clientConfig *clientconfig.Bundle

	// permissions is the in-flight permission-request registry for EXTERNAL acp
	// agents: the rendezvous between the turn goroutine's blocked broker and the
	// POST /conversations/{id}/permissions/{requestId} decision handler.
	permissions *permissionRegistry
}

// Option customizes a Server at construction.
type Option func(*Server)

// WithClientConfig wires the loaded client bundle so GET /client-config can
// serve the deployment's branding + chat empty-state to the web.
func WithClientConfig(b *clientconfig.Bundle) Option {
	return func(s *Server) { s.clientConfig = b }
}

// inflightEntry pairs the cancel-func for a turn with a unique token,
// the per-turn event buffer, and a finishedAt timestamp.
//
// Two-phase lifecycle:
//   - while running: buf accepts Emit calls and fans events out to any
//     live Attach subscribers. finishedAt is zero.
//   - after Finish: buf is sealed but kept in the map for
//     bufferRetainTTL so a client reconnecting within that window still
//     sees the full replay. finishedAt is set; eventual eviction is
//     scheduled by a timer goroutine.
type inflightEntry struct {
	cancel     context.CancelFunc
	token      uint64
	buf        *turnBuffer
	turnID     string
	finishedAt time.Time
}

// IsRunning reports whether the turn is still generating (buffer open).
func (e inflightEntry) IsRunning() bool {
	return e.finishedAt.IsZero()
}

// New wires a Server. Call Routes() to get the http.Handler.
//
// mgr is the interactive agent engine (the turnEngine contract). It may be nil
// in mock mode and in tests that exercise only the DB-backed, mock-turn, or
// auth paths — the live turn path short-circuits before touching it. cmd/fleet
// (P6b) supplies the concrete engine implementation.
func New(cfg *config.Config, mgr turnEngine, st chatStore, opts ...Option) *Server {
	s := &Server{
		cfg:         cfg,
		agent:       mgr,
		store:       st,
		sharedToken: cfg.SharedToken,
		rate:        newRateLimiter(cfg.RatePerMinute, cfg.RatePerDay),
		inflight:    make(map[string]inflightEntry),
		permissions: newPermissionRegistry(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// defaultTurnExecutionTimeout caps how long a single turn may run
// server-side once detached from the SSE connection. Well above the
// per-turn cost + iteration ceilings, which are the real safety nets.
// Operators can override via CHAT_TURN_TIMEOUT_SECONDS (config.TurnTimeoutSeconds).
const defaultTurnExecutionTimeout = 30 * time.Minute

// turnTimeout resolves the configured per-turn wall-clock cap, falling
// back to defaultTurnExecutionTimeout when unset or non-positive.
func (s *Server) turnTimeout() time.Duration {
	if s.cfg != nil && s.cfg.TurnTimeoutSeconds > 0 {
		return time.Duration(s.cfg.TurnTimeoutSeconds) * time.Second
	}
	return defaultTurnExecutionTimeout
}

// bufferRetainTTL is how long a finished turn's event buffer stays in
// the inflight map after completion. Long enough for a mobile client
// that locked its screen mid-turn to return, reopen the tab, and see
// the full replay.
const bufferRetainTTL = 2 * time.Minute

// registerTurn installs a fresh turnBuffer + cancel entry for convID.
// Cancels + evicts any prior in-flight turn for the same conversation
// (a user submitting a new turn before the previous one finished is a
// clear "abandon the old one" signal — replay for the old turn is
// also dropped). Returns the buffer, the turn ID, and a token that
// finishTurn must present so stale finishers can't clobber a fresher
// entry.
func (s *Server) registerTurn(convID string, cancel context.CancelFunc) (*turnBuffer, string, uint64) {
	s.inflightMu.Lock()
	prev, hadPrev := s.inflight[convID]
	if hadPrev {
		prev.cancel()
		delete(s.inflight, convID)
	}
	s.inflightCounter++
	token := s.inflightCounter
	turnID := uuid.NewString()
	buf := newTurnBuffer(convID, turnID)
	s.inflight[convID] = inflightEntry{
		cancel: cancel,
		token:  token,
		buf:    buf,
		turnID: turnID,
	}
	s.inflightMu.Unlock()

	// Seal the evicted buffer after releasing inflightMu: Finish drains
	// the persister (DB writes with multi-second budgets), and holding
	// the server-wide lock across that would stall every conversation's
	// /chat, /stream and /cancel behind one slow Postgres round-trip.
	// The entry is already out of the map, so nobody else can reach it;
	// Finish is idempotent against the evicted turn's own deferred
	// finishTurn.
	if hadPrev && prev.buf != nil {
		prev.buf.Finish()
	}
	return buf, turnID, token
}

// finishTurn seals the buffer and marks the entry finished, keeping it
// retained for bufferRetainTTL so a late reconnect still sees replay.
// A stale token (a newer turn already replaced us) makes this a no-op
// so we don't evict the fresher entry.
func (s *Server) finishTurn(convID string, token uint64) {
	// Snapshot the buffer under the lock, but seal it after releasing:
	// Finish waits out the persister drain + a FinishTurn UPDATE (5s DB
	// budgets each), and holding the server-wide inflightMu across that
	// would block every conversation on one slow Postgres round-trip.
	s.inflightMu.Lock()
	cur, ok := s.inflight[convID]
	if !ok || cur.token != token {
		s.inflightMu.Unlock()
		return
	}
	buf := cur.buf
	s.inflightMu.Unlock()

	// Seal first, then flip finishedAt, so "finished" always implies
	// "buffer sealed" for /inflight pollers and late Attach calls.
	if buf != nil {
		buf.Finish()
	}

	s.inflightMu.Lock()
	cur, ok = s.inflight[convID]
	if !ok || cur.token != token {
		// A newer turn replaced us while we were sealing; it owns the
		// entry now. Our buffer is sealed either way.
		s.inflightMu.Unlock()
		return
	}
	cur.finishedAt = time.Now()
	s.inflight[convID] = cur
	s.inflightMu.Unlock()

	// Evict the retained buffer after TTL. If another turn has replaced
	// this one by then (token mismatch), leave it alone.
	time.AfterFunc(bufferRetainTTL, func() {
		s.inflightMu.Lock()
		defer s.inflightMu.Unlock()
		if cur, ok := s.inflight[convID]; ok && cur.token == token {
			delete(s.inflight, convID)
		}
	})
}

// cancelInflight cancels the currently-running turn for convID, if any.
// Returns true if a cancellation was issued (i.e. the turn was still
// running — a cancel against an already-finished buffer is a no-op).
func (s *Server) cancelInflight(convID string) bool {
	s.inflightMu.Lock()
	entry, ok := s.inflight[convID]
	s.inflightMu.Unlock()
	if !ok || !entry.IsRunning() {
		return false
	}
	entry.cancel()
	return true
}

// getInflight returns a snapshot of the current entry for convID.
func (s *Server) getInflight(convID string) (inflightEntry, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	entry, ok := s.inflight[convID]
	return entry, ok
}

// Routes returns the top-level http.Handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)

	// auth = shared-secret + X-User-Email (identity). member adds the
	// scoped-tier user-list gate. /auth/verify stays on auth alone so the
	// password pre-login path can answer for not-yet-known emails without
	// leaking the user-list (see membershipMiddleware).
	auth := s.authMiddleware
	member := s.membershipMiddleware
	mux.Handle("/chat", auth(member(s.rateLimitMiddleware(http.HandlerFunc(s.postChat)))))
	mux.Handle("/attachments", auth(member(http.HandlerFunc(s.postAttachments))))
	mux.Handle("/conversations", auth(member(http.HandlerFunc(s.listOrCreateConversations))))
	mux.Handle("/conversations/", auth(member(http.HandlerFunc(s.conversationByID))))
	mux.Handle("/memories", auth(member(http.HandlerFunc(s.memories))))
	mux.Handle("/memories/", auth(member(http.HandlerFunc(s.memoryByID))))
	mux.Handle("/personas", auth(member(http.HandlerFunc(s.listPersonas))))
	mux.Handle("/mcp-servers", auth(member(http.HandlerFunc(s.listMCPServerCatalog))))
	mux.Handle("/server-config", auth(member(http.HandlerFunc(s.serverConfig))))
	mux.Handle("/client-config", auth(member(http.HandlerFunc(s.clientConfigHandler))))
	mux.Handle("/auth/membership", auth(member(http.HandlerFunc(s.handleMembership))))
	mux.Handle("/auth/verify", auth(http.HandlerFunc(s.handleAuthVerify)))
	mux.Handle("/admin/stats", auth(member(s.adminMiddleware(http.HandlerFunc(s.handleAdminStats)))))
	return recoverMiddleware(bodyLimitMiddleware(mux))
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
		case http.MethodPost, http.MethodPut, http.MethodPatch:
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
				log.Printf("panic in chat handler: %v\n%s", rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

// ── /memories ─────────────────────────────────────────────────────────────

type memoryRequest struct {
	Content string `json:"content"`
}

func (s *Server) memories(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		memories, err := s.store.ListMemories(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"memories": memories})
	case http.MethodPost:
		var req memoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		memory, err := s.store.CreateMemory(r.Context(), user, req.Content, "manual")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, memory)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) memoryByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/memories/"), "/")
	if rest == "" {
		http.Error(w, "memory id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if sub == "accept" && r.Method == http.MethodPost {
		memory, err := s.store.AcceptMemoryProposal(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, memory)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req memoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		memory, err := s.store.UpdateMemory(r.Context(), user, id, req.Content)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, memory)
	case http.MethodDelete:
		if err := s.store.DeleteMemory(r.Context(), user, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /personas ──────────────────────────────────────────────────────────────

type personasResponse struct {
	Personas []string `json:"personas"`
	Default  string   `json:"default"`
}

// serverConfigResponse is the small "what does this server expose" payload
// the frontend reads at startup to decide which capability-gated UI to
// render. Currently just the lockdown affordance — extend if more
// operator-toggled UI surfaces appear.
//
//   - LockdownAvailable: lockdown UI should be shown (sandbox image is
//     configured).
//   - LockdownOnly: lockdown is enforced for every chat — frontend
//     hides the regular "+" button and always shows the badge.
//   - LockdownAllowedModels: slug allow-list, used by the model picker
//     filter.
type serverConfigResponse struct {
	LockdownAvailable     bool     `json:"lockdown_available"`
	LockdownOnly          bool     `json:"lockdown_only"`
	LockdownAllowedModels []string `json:"lockdown_allowed_models"`
}

func (s *Server) serverConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := serverConfigResponse{
		LockdownAvailable: s.cfg.LockdownAvailable(),
		LockdownOnly:      s.cfg.LockdownOnly,
	}
	if resp.LockdownAvailable {
		resp.LockdownAllowedModels = append(resp.LockdownAllowedModels, s.cfg.LockdownAllowedModels...)
	}
	writeJSON(w, resp)
}

// ── /client-config ──────────────────────────────────────────────────────────

// clientConfigResponse is the white-label surface the web renders: branding
// strings + the chat empty-state catalog. Sourced from the loaded client
// bundle's manifest; neutral generic defaults when no bundle is wired.
type clientConfigResponse struct {
	Branding   clientConfigBranding   `json:"branding"`
	EmptyState clientConfigEmptyState `json:"empty_state"`
	// Runtimes is the runtime-flavor catalog for the chat flavor picker, plus
	// the default flavor. Empty/single-flavor bundles let the frontend hide the
	// picker entirely.
	Runtimes       []clientConfigRuntime `json:"runtimes"`
	DefaultRuntime string                `json:"default_runtime"`
}

// clientConfigRuntime is one selectable runtime flavor for the chat picker.
type clientConfigRuntime struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Beta        bool   `json:"beta"`
}

type clientConfigBranding struct {
	AppName          string `json:"app_name"`
	LoginTitle       string `json:"login_title"`
	LoginTagline     string `json:"login_tagline"`
	ShareTitle       string `json:"share_title"`
	ShareDescription string `json:"share_description"`
}

type clientConfigEmptyState struct {
	Cards         []map[string]any `json:"cards"`
	ProtocolPills []map[string]any `json:"protocol_pills"`
}

func (s *Server) clientConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := clientConfigResponse{
		Branding: clientConfigBranding{
			AppName:          "Fleet",
			LoginTitle:       "Welcome aboard.",
			LoginTagline:     "Sign in to your workspace and pick up where you left off.",
			ShareTitle:       "Fleet — your team's AI workspace",
			ShareDescription: "An AI workspace with real tool use.",
		},
		EmptyState: clientConfigEmptyState{
			Cards:         []map[string]any{},
			ProtocolPills: []map[string]any{},
		},
	}
	if s.clientConfig != nil {
		b := s.clientConfig
		resp.Branding = clientConfigBranding{
			AppName:          b.Branding.AppName,
			LoginTitle:       b.Branding.LoginTitle,
			LoginTagline:     b.Branding.LoginTagline,
			ShareTitle:       b.Branding.ShareTitle,
			ShareDescription: b.Branding.ShareDescription,
		}
		if len(b.EmptyState.Cards) > 0 {
			resp.EmptyState.Cards = b.EmptyState.Cards
		}
		if len(b.EmptyState.ProtocolPills) > 0 {
			resp.EmptyState.ProtocolPills = b.EmptyState.ProtocolPills
		}
		for _, rt := range b.Runtimes() {
			resp.Runtimes = append(resp.Runtimes, clientConfigRuntime{
				Name:        rt.Name,
				DisplayName: displayNameOrName(rt.DisplayName, rt.Name),
				Description: rt.Description,
				Beta:        rt.Beta,
			})
		}
		resp.DefaultRuntime = b.DefaultRuntime()
	}
	writeJSON(w, resp)
}

// displayNameOrName returns the display name when set, else the raw name.
func displayNameOrName(display, name string) string {
	if strings.TrimSpace(display) != "" {
		return display
	}
	return name
}

// runtimeKnown reports whether name is a flavor declared in the bundle catalog.
// When no bundle is wired, only native-inprocess is accepted.
func (s *Server) runtimeKnown(name string) bool {
	if s.clientConfig == nil {
		return name == clientconfig.RuntimeNativeInprocess
	}
	_, ok := s.clientConfig.Runtime(name)
	return ok
}

// persistChatRuntime persists a per-turn runtime-flavor override onto the
// conversation when the request named a NEW, recognized flavor. An empty or
// unknown value is a no-op (the stored flavor — or the bundle default — stands),
// mirroring the model-override semantics in postChat.
func (s *Server) persistChatRuntime(ctx context.Context, user string, conv *store.Conversation, requested string) error {
	rt := strings.TrimSpace(requested)
	if rt == "" || rt == conv.Runtime || !s.runtimeKnown(rt) {
		return nil
	}
	if err := s.store.SetRuntime(ctx, user, conv.ID, rt); err != nil {
		return err
	}
	conv.Runtime = rt
	return nil
}

func (s *Server) listPersonas(w http.ResponseWriter, _ *http.Request) {
	names, err := s.agent.ListPersonas()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, personasResponse{
		Personas: names,
		Default:  s.cfg.PersonaDefault,
	})
}

// ── /mcp-servers ───────────────────────────────────────────────────────────

// listMCPServerCatalog returns the Optional MCP server catalog without any
// per-conversation opt-in state. The frontend calls this on startup so the
// Tools picker can render for brand-new chats (before a conversation row
// exists). `enabled` reflects each server's EnabledByDefault (so default-on
// connectors like gamma start toggled on for a fresh chat); per-conversation
// state is fetched from /conversations/{id}/mcp-servers once a conversation
// is open and overrides this seed.
func (s *Server) listMCPServerCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	catalog := s.agent.MCPServerCatalog()
	servers := make([]map[string]any, 0, len(catalog))
	for _, info := range catalog {
		servers = append(servers, map[string]any{
			"name":         info.Name,
			"display_name": info.DisplayName,
			"description":  info.Description,
			"tools":        info.Tools,
			"tool_count":   info.ToolCount,
			"enabled":      info.EnabledByDefault,
			"beta":         info.Beta,
			// Separate group so adding this longer key doesn't re-align
			// the block above.
			"enabled_by_default": info.EnabledByDefault,
			// Provisioned credential-account seat names (never secret values).
			"accounts": info.Accounts,
		})
	}
	writeJSON(w, map[string]any{"servers": servers})
}

// ── /conversations ─────────────────────────────────────────────────────────

type createConversationRequest struct {
	Title   string `json:"title"`
	Persona string `json:"persona"`
	Model   string `json:"model"`
	// Lockdown, when true, marks this conversation as locked-down: the
	// agent loop forces a per-turn container sandbox and the model slug
	// is restricted to CHAT_LOCKDOWN_ALLOWED_MODELS. Frontend exposes
	// this as a "New lockdown chat" affordance. Server rejects when
	// CHAT_LOCKDOWN_ENABLED is false.
	Lockdown bool `json:"lockdown,omitempty"`
}

func (s *Server) listOrCreateConversations(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.List(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"conversations": list})
	case http.MethodDelete:
		// Bulk delete — removes every unpinned conversation for this user.
		// Pinned conversations are intentionally untouched; a user who
		// wants those gone clicks Delete on each individually.
		n, err := s.store.DeleteAllUnpinned(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"deleted": n})
	case http.MethodPost:
		var req createConversationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		persona := strings.TrimSpace(req.Persona)
		if persona == "" {
			persona = s.cfg.PersonaDefault
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = "New conversation"
		}
		lockdown := req.Lockdown || s.cfg.LockdownOnly
		if lockdown && !s.cfg.LockdownAvailable() {
			http.Error(w, "lockdown is unavailable on this server (no sandbox image configured)", http.StatusBadRequest)
			return
		}
		model := strings.TrimSpace(req.Model)
		if lockdown && model != "" && !s.cfg.LockdownAllows(model) {
			http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
			return
		}
		conv, err := s.store.CreateConversation(r.Context(), user, title, persona, model, lockdown)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, conv)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// /conversations/{id}
// /conversations/{id}/pin
// /conversations/{id}/messages
//
//nolint:gocyclo // sub-route dispatcher: complexity tracks the number of routes, not branch density
func (s *Server) conversationByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/conversations/")
	if rest == "" {
		http.Error(w, "conversation id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 3)
	id := parts[0]
	sub := ""
	subArg := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}
	if len(parts) == 3 {
		subArg = parts[2]
	}

	// Approval resolution lives at /conversations/{id}/approvals/{approvalId}.
	if sub == "approvals" && subArg != "" {
		s.handleApproval(w, r, id, subArg)
		return
	}

	// External-agent permission decision lives at
	// /conversations/{id}/permissions/{requestId} (allow/deny; default-deny on
	// timeout is enforced server-side by the broker).
	if sub == "permissions" && subArg != "" {
		s.handlePermissionDecision(w, r, id, subArg)
		return
	}

	// Stream reattach + inflight probe — see handleStream/handleInflight.
	if sub == "stream" && r.Method == http.MethodGet {
		s.handleStream(w, r, id)
		return
	}
	if sub == "inflight" && r.Method == http.MethodGet {
		s.handleInflight(w, r, id)
		return
	}

	// Workspace file fetch — `GET /conversations/{id}/workspace/<path>`
	// streams a file from the conversation's per-turn workspace dir so
	// markdown image references like `![](spend_chart.png)` written by
	// the agent during run_python actually render in the chat UI.
	if sub == "workspace" && r.Method == http.MethodGet {
		s.handleWorkspaceFile(w, r, id, subArg)
		return
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		history, err := s.store.LoadHistory(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pending, err := s.store.ListPendingApprovals(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Shape pending approvals the same way tool.approval_required
		// events do, so the frontend reuses its render path.
		approvals := make([]map[string]any, 0, len(pending))
		for _, a := range pending {
			approvals = append(approvals, map[string]any{
				"approval_id": a.ID,
				"tool":        a.ToolName,
				"summary":     summarizeApprovalInput(a.ToolName, a.ArgsJSON, id),
			})
		}
		// Pending memory proposals — same pattern as approvals. Without
		// these, the visibilitychange/focus auto-refetch in chat-experience
		// wipes the Save/Don't-Save card every time the user clicks away
		// and back.
		pendingMems, err := s.store.ListPendingMemoryProposalsForConversation(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		memProposals := make([]map[string]any, 0, len(pendingMems))
		for _, m := range pendingMems {
			memProposals = append(memProposals, map[string]any{
				"proposal_id": m.ID,
				"content":     m.Content,
			})
		}
		writeJSON(w, map[string]any{
			"conversation":             conv,
			"history":                  history,
			"pending_approvals":        approvals,
			"pending_memory_proposals": memProposals,
		})
	case sub == "" && r.Method == http.MethodDelete:
		if err := s.store.Delete(r.Context(), user, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "truncate" && r.Method == http.MethodPost:
		// Retry/regenerate: drop every message after the latest user turn
		// so the next turn regenerates the assistant tail from scratch.
		// With ?mode=edit_last we drop the latest user turn too, which is
		// what the edit-and-resend flow needs.
		mode := r.URL.Query().Get("mode")
		var pivot int64
		var err error
		if mode == "edit_last" {
			// Truncate after the SECOND-to-last user so the latest user
			// turn (and its assistant tail) are both removed. If no prior
			// user exists, zero is fine — everything gets wiped.
			pivot, err = s.store.SecondMaxMessageIDForRole(r.Context(), id, "user")
		} else {
			pivot, err = s.store.MaxMessageIDForRole(r.Context(), id, "user")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.TruncateAfter(r.Context(), user, id, pivot); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "pin" && r.Method == http.MethodPost:
		var req struct {
			Pinned bool `json:"pinned"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.SetPinned(r.Context(), user, id, req.Pinned); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "rename" && r.Method == http.MethodPost:
		var req struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			http.Error(w, "title required", http.StatusBadRequest)
			return
		}
		if len(title) > 200 {
			title = title[:200]
		}
		if err := s.store.UpdateTitle(r.Context(), user, id, title); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}{ID: id, Title: title})
	case sub == "model" && r.Method == http.MethodPost:
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		model := strings.TrimSpace(req.Model)
		if model != "" {
			conv, err := s.store.Get(r.Context(), user, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if conv == nil {
				http.NotFound(w, r)
				return
			}
			if conv.Lockdown && !s.cfg.LockdownAllows(model) {
				http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
				return
			}
		}
		if err := s.store.SetModel(r.Context(), user, id, model); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "runtime" && r.Method == http.MethodPost:
		// Per-conversation runtime-flavor selection (the chat flavor picker). An
		// empty value clears the override (the bundle default applies); a
		// non-empty value must name a flavor in the bundle catalog.
		var req struct {
			Runtime string `json:"runtime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		runtime := strings.TrimSpace(req.Runtime)
		if runtime != "" && !s.runtimeKnown(runtime) {
			http.Error(w, "unknown runtime flavor", http.StatusBadRequest)
			return
		}
		if err := s.store.SetRuntime(r.Context(), user, id, runtime); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "mcp-servers" && r.Method == http.MethodGet:
		// Per-conversation MCP-server catalog. Response shape:
		//   { "servers": [{ name, description, tools: [...], tool_count,
		//                   enabled }, ...] }
		// `enabled` is true when the conversation currently opted this
		// server in. Non-optional servers are NOT listed — the UI only
		// renders the toggle row for Optional ones. Reads from
		// Manager.MCPServerCatalog() (frozen at server startup) + the
		// conversation's opt-in list (fresh from Postgres).
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		enabled := make(map[string]bool, len(conv.OptionalMCPServersEnabled))
		for _, n := range conv.OptionalMCPServersEnabled {
			enabled[n] = true
		}
		servers := make([]map[string]any, 0, len(s.agent.MCPServerCatalog()))
		for _, info := range s.agent.MCPServerCatalog() {
			servers = append(servers, map[string]any{
				"name":         info.Name,
				"display_name": info.DisplayName,
				"description":  info.Description,
				"tools":        info.Tools,
				"tool_count":   info.ToolCount,
				"enabled":      enabled[info.Name],
				"beta":         info.Beta,
				// Separate group so adding this longer key doesn't re-align
				// the block above.
				"enabled_by_default": info.EnabledByDefault,
			})
		}
		writeJSON(w, map[string]any{"servers": servers})
	case sub == "mcp-servers" && r.Method == http.MethodPost:
		// Body: { "enabled_optional": ["gamma", ...] } — full set,
		// replacing the previous opt-in list. Unknown / non-optional
		// server names are dropped silently; the server's catalog is
		// the authoritative whitelist.
		var req struct {
			EnabledOptional []string `json:"enabled_optional"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Intersect with the known-optional catalog so a bad frontend
		// can't persist garbage. Dedup + sort for a canonical payload.
		valid := make(map[string]bool, len(s.agent.MCPServerCatalog()))
		for _, info := range s.agent.MCPServerCatalog() {
			valid[info.Name] = true
		}
		seen := make(map[string]bool, len(req.EnabledOptional))
		clean := make([]string, 0, len(req.EnabledOptional))
		for _, n := range req.EnabledOptional {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" || !valid[n] || seen[n] {
				continue
			}
			seen[n] = true
			clean = append(clean, n)
		}
		sort.Strings(clean)
		if err := s.store.SetOptionalMCPServers(r.Context(), user, id, clean); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"enabled_optional": clean})
	case sub == "export" && r.Method == http.MethodGet:
		// JSON export of the full conversation (metadata + history).
		// Returned as a downloadable attachment so the browser triggers
		// a Save dialog; reuses the same fields as GET /conversations/{id}.
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		history, err := s.store.LoadHistory(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body := map[string]any{
			"conversation": conv,
			"history":      history,
			"exported_at":  time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set(
			"Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`, exportFilename(conv.Title, conv.ID)),
		)
		_ = json.NewEncoder(w).Encode(body)
	case sub == "cancel" && r.Method == http.MethodPost:
		// Explicit Stop button. Owner-scoped: confirm the conversation
		// belongs to the caller before issuing the cancel so a token
		// leak can't cancel arbitrary chats.
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		s.cancelInflight(id)
		w.WriteHeader(http.StatusNoContent)
	case sub == "summarize" && r.Method == http.MethodPost:
		s.handleSummarize(w, r, user, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /chat (SSE) ────────────────────────────────────────────────────────────

type chatRequest struct {
	ConversationID string `json:"conversation_id"` // if empty, a new one is created
	Message        string `json:"message"`
	Persona        string `json:"persona"`
	Title          string `json:"title"` // only honored on first turn of a new conversation
	// Model is the per-turn OpenRouter slug. On a new conversation it gets
	// persisted; on an existing one it overrides whatever was stored. Empty
	// = use whatever the conversation already has, or the configured default.
	Model string `json:"model"`
	// Attachments carries the metadata returned by a prior POST /attachments
	// call. Paths are re-validated against the uploads root before use; any
	// entry that fails validation is silently dropped.
	Attachments []chatAttachment `json:"attachments,omitempty"`
	// EnabledOptional seeds the optional MCP server opt-in list on a
	// brand-new conversation so the Tools picker's pre-chat selections
	// take effect on the very first turn. Honored only when no
	// ConversationID is provided. Unknown / non-optional names are
	// dropped silently (same rules as POST /mcp-servers).
	EnabledOptional []string `json:"enabled_optional,omitempty"`
	// Lockdown mirrors createConversationRequest.Lockdown — honored
	// only when no ConversationID is provided (lockdown is set once
	// at conversation creation and immutable thereafter).
	Lockdown bool `json:"lockdown,omitempty"`
	// Runtime is the per-conversation execution flavor (fleet's ACP runtime
	// selection). On a new conversation it is persisted; on an existing one a
	// non-empty value overrides the stored flavor. Empty = keep whatever's
	// stored (the dedicated POST /conversations/{id}/runtime clears it).
	// Unknown flavors fall back to the bundle default at run time.
	Runtime string `json:"runtime,omitempty"`
}

func memoryContents(memories []store.Memory) []string {
	out := make([]string, 0, len(memories))
	for _, memory := range memories {
		if memory.Source == "proposed" {
			continue
		}
		if len(out) >= 50 {
			break
		}
		content := strings.TrimSpace(memory.Content)
		if content != "" {
			out = append(out, content)
		}
	}
	return out
}

func (s *Server) postChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	// Resolve conversation: find existing, or create new.
	var (
		conv *store.Conversation
		err  error
	)
	reqModel := strings.TrimSpace(req.Model)
	if req.ConversationID != "" {
		conv, err = s.store.Get(r.Context(), user, req.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		// Per-turn model override: if the caller passed a NEW non-empty slug,
		// persist it so the next reload reflects the user's choice. An empty
		// reqModel is treated as "no opinion, keep whatever's stored" —
		// otherwise a transient state race on the client (new-chat reset,
		// reload before the `conversation` event rehydrates selectedModel)
		// silently wipes the stored override and the next turn quietly
		// falls back to the server primary. To explicitly clear the
		// override, the dedicated PATCH /conversations/{id}/model endpoint
		// can send "".
		if reqModel != "" && reqModel != conv.Model {
			if err := s.store.SetModel(r.Context(), user, conv.ID, reqModel); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conv.Model = reqModel
		}
		// Per-turn runtime override: persist a new, recognized flavor so the
		// next reload reflects the picker.
		if err := s.persistChatRuntime(r.Context(), user, conv, req.Runtime); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		persona := strings.TrimSpace(req.Persona)
		if persona == "" {
			persona = s.cfg.PersonaDefault
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			// derive from first message; shortened later
			title = truncateForTitle(req.Message, 80)
		}
		lockdown := req.Lockdown || s.cfg.LockdownOnly
		if lockdown {
			if !s.cfg.LockdownAvailable() {
				http.Error(w, "lockdown is unavailable on this server (no sandbox image configured)", http.StatusBadRequest)
				return
			}
			if reqModel != "" && !s.cfg.LockdownAllows(reqModel) {
				http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
				return
			}
		}
		conv, err = s.store.CreateConversation(r.Context(), user, title, persona, reqModel, lockdown)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Seed the optional MCP server opt-in list from the chat request
		// so pre-chat Tools picker selections take effect on this first
		// turn. Intersect with the catalog so a bad frontend can't
		// persist garbage (mirrors POST /conversations/{id}/mcp-servers).
		if len(req.EnabledOptional) > 0 {
			valid := make(map[string]bool, len(s.agent.MCPServerCatalog()))
			for _, info := range s.agent.MCPServerCatalog() {
				valid[info.Name] = true
			}
			seen := make(map[string]bool, len(req.EnabledOptional))
			clean := make([]string, 0, len(req.EnabledOptional))
			for _, n := range req.EnabledOptional {
				n = strings.ToLower(strings.TrimSpace(n))
				if n == "" || !valid[n] || seen[n] {
					continue
				}
				seen[n] = true
				clean = append(clean, n)
			}
			sort.Strings(clean)
			if len(clean) > 0 {
				if err := s.store.SetOptionalMCPServers(r.Context(), user, conv.ID, clean); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				conv.OptionalMCPServersEnabled = clean
			}
		}
		// Seed the per-conversation runtime flavor from the chat request so the
		// flavor picker's pre-chat selection takes effect on the first turn.
		if err := s.persistChatRuntime(r.Context(), user, conv, req.Runtime); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Load history before we even allocate a buffer — if this errors, the
	// client never sees a partial SSE stream.
	history, err := s.store.LoadHistory(r.Context(), conv.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	memories, err := s.store.ListMemories(r.Context(), user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Detach the turn's lifecycle from the SSE connection. r.Context()
	// dies the moment the HTTP request goes away (browser tab closed,
	// phone screen locks, mobile network blip), but the agent might
	// have 90 seconds of useful work to do. We run the agent in a
	// goroutine publishing into a per-turn event buffer; this HTTP
	// response simply Attaches to the buffer and streams from it. A
	// later GET /conversations/{id}/stream can attach to the same
	// buffer and pick up where this one left off via Last-Event-ID.
	//
	// Explicit cancellation (the Stop button) routes through
	// POST /conversations/{id}/cancel, which fires the cancel func we
	// register here.
	turnCtx, turnCancel := context.WithTimeout(context.Background(), s.turnTimeout())
	buf, turnID, turnToken := s.registerTurn(conv.ID, turnCancel)

	// Wire incremental persistence so a crash mid-turn leaves a
	// recoverable ledger in turn_events. Non-fatal — if the DB is
	// flaky, live streaming still works; crash recovery just won't.
	persistCtx, persistCancelAttach := context.WithTimeout(r.Context(), 5*time.Second)
	if err := buf.attachPersister(persistCtx, s.store); err != nil {
		log.Printf("attachPersister (user=%s conv=%s): %v", user, conv.ID, err)
	}
	persistCancelAttach()

	// Attachment metadata (if any) is re-validated server-side, then split
	// by kind. Images flow into the model as multimodal vision input via
	// TurnInput.ImageAttachments; other files keep the legacy markdown
	// reference path so view_file etc. can still reach them. Both kinds
	// are mentioned in the appended block so the agent sees what arrived.
	validAttachments := s.validateAttachments(req.Attachments)
	imageAttachments, otherAttachments := splitAttachmentsByKind(validAttachments)
	userMessage := appendAttachmentsBlock(req.Message, imageAttachments, otherAttachments)
	// Surface files persisted from earlier turns. The agent's run_python
	// kernel resets each turn but its workspace dir doesn't — without this,
	// a report downloaded on turn 1 gets forgotten by turn 4 even though
	// it's still on disk. Empty workspaces (first turn) skip the block.
	userMessage = appendWorkspaceInventoryBlock(userMessage, tools.WorkspaceDirForConversation(conv.ID))

	// Prime the buffer with the metadata events so a late reattach
	// still sees conversation identity + turn id in its replay. The
	// `user.message` event is replay-only — when the client refreshes
	// mid-turn, the user message slot they see locally is gone (the
	// Postgres history doesn't have it yet; AppendHistory only fires
	// after RunTurn completes), and reattach reconstructs it from this
	// event so the chat doesn't appear as just a stranded "Thinking…"
	// indicator with no question above it.
	buf.Emit("conversation", map[string]any{
		"id":      conv.ID,
		"title":   conv.Title,
		"persona": conv.Persona,
		"model":   conv.Model,
	})
	buf.Emit("turn.started", map[string]any{
		"turn_id": turnID,
		"persona": conv.Persona,
	})
	buf.Emit("user.message", map[string]any{
		"text": userMessage,
	})

	// Run the turn in a goroutine so the buffer stays alive even if
	// this HTTP response disconnects. turnCtx is intentionally NOT
	// derived from r.Context(): the turn must outlive the HTTP request.
	go s.runTurnAsync(turnCtx, turnCancel, buf, turnToken, conv, user, req.Message, userMessage, history, memoryContents(memories), toAgentImageAttachments(imageAttachments)) //nolint:gosec // turnCtx lifetime is intentional, see comment above

	// Attach this HTTP response as the initial subscriber. Blocks until
	// the turn finishes or the client disconnects.
	if err := buf.Attach(r.Context(), 0, w); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Attach (user=%s conv=%s): %v", user, conv.ID, err)
	}
}

// runTurnAsync executes the agent turn, persists the result, emits the
// optional title_updated event, and then finishes the buffer. Lives in
// its own goroutine so the HTTP POST can disconnect without killing
// generation.
func (s *Server) runTurnAsync(
	turnCtx context.Context,
	turnCancel context.CancelFunc,
	buf *turnBuffer,
	turnToken uint64,
	conv *store.Conversation,
	user, userInput, userMessage string,
	history []agent.HistoryEntry,
	memories []string,
	imageAttachments []agent.ImageAttachment,
) {
	// Order matters: finishTurn must seal the buffer and schedule
	// retention AFTER title_updated has been emitted. turnCancel runs
	// first to release any resources the agent still holds.
	defer s.finishTurn(conv.ID, turnToken)
	defer turnCancel()
	// This goroutine is intentionally detached from the HTTP request, so an
	// unrecovered panic here would crash the whole single-host process. Recover
	// so a panic fails only THIS turn. Registered after the cleanup defers, so it
	// runs FIRST on unwind: emit a terminal error, then turnCancel + finishTurn
	// seal the buffer and the user sees an error instead of a stuck "Thinking…".
	defer safe.Recover("httpapi.runTurnAsync", func(any) {
		buf.Emit("turn.error", map[string]any{"message": "the turn ended unexpectedly due to an internal error"})
	})

	// Mock mode: short-circuit the LLM loop with a scripted stream for
	// Playwright + CI. Skips history replay + provider call entirely.
	if s.cfg.MockMode {
		if err := runMockTurn(turnCtx, s.store, conv, userInput, buf); err != nil {
			log.Printf("runMockTurn error (user=%s conv=%s): %v", user, conv.ID, err)
		}
		sweepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, _, err := s.store.SweepExpired(sweepCtx,
			time.Duration(s.cfg.ConversationTTL)*24*time.Hour, s.cfg.UnpinnedCap); err != nil {
			log.Printf("post-turn sweep error: %v", err)
		}
		return
	}

	res, err := s.agent.RunTurn(turnCtx, TurnInput{
		UserMessage:               userMessage,
		Persona:                   conv.Persona,
		Model:                     conv.Model,
		History:                   history,
		Memories:                  memories,
		ConversationID:            conv.ID,
		OptionalMCPServersEnabled: conv.OptionalMCPServersEnabled,
		Lockdown:                  conv.Lockdown,
		Runtime:                   conv.Runtime,
		ImageAttachments:          imageAttachments,
		ApprovalStager: &approvalStager{
			ctx:            turnCtx,
			store:          s.store,
			conversationID: conv.ID,
			userEmail:      user,
			sink:           buf,
			mcpClient:      s.agent.MCPClient(),
		},
		MemoryProposer: &memoryProposer{
			ctx:            turnCtx,
			store:          s.store,
			conversationID: conv.ID,
			userEmail:      user,
			sink:           buf,
		},
		// External (acp) flavors route session/request_permission to the human
		// through this broker (default-deny on timeout, no approve-all). The
		// native flavors ignore it — they are governed in-loop.
		PermissionBroker: &permissionBroker{
			registry:       s.permissions,
			conversationID: conv.ID,
			sink:           buf,
		},
	}, buf)
	if err != nil {
		log.Printf("RunTurn error (user=%s conv=%s): %v", user, conv.ID, err)
		// The resilience layer inside RunTurn emits `turn.model_required`
		// itself on any non-cancellation failure (see agent/resilience.go).
		// Avoid emitting a redundant — and misleading — `turn.error` in
		// that case; the frontend already has the structured reason and
		// model slug it needs.
		if !errors.Is(err, ErrModelSelectionRequired) {
			buf.Emit("turn.error", map[string]any{"message": err.Error()})
		}
		return
	}

	// Persist with a fresh context. turnCtx may already be cancelled if
	// the turn ended via Stop; RunTurn handles that gracefully and
	// returns a partial TurnResult, but the DB writes below need a
	// live context. A 10s budget is plenty for Postgres + a small
	// title generation; if anything in the persist path takes longer
	// than that something's actually wrong and timing out is the right
	// call.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer persistCancel()

	if err := s.store.AppendHistory(persistCtx, conv.ID, res.NewHistory); err != nil {
		log.Printf("persist history error (user=%s conv=%s): %v", user, conv.ID, err)
	}

	// Record metrics so the admin dashboard can aggregate cost per user.
	// A failed/errored turn doesn't reach this code path (we returned early
	// above); cancelled turns DO, and are flagged for separate accounting.
	if err := s.store.RecordTurn(persistCtx, store.TurnMetric{
		ConversationID:   conv.ID,
		UserEmail:        user,
		CompletedAt:      time.Now().Unix(),
		CostUSD:          res.CostUSD,
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		CachedTokens:     res.CachedTokens,
		Cancelled:        res.Cancelled,
	}); err != nil {
		log.Printf("RecordTurn: %v", err)
	}

	// First-turn auto-title: on the opening turn, summarize the exchange
	// into a 5-7 word sidebar title. Emits via the buffer so both the
	// initial client and any reattach see it.
	if len(history) == 0 && !res.Cancelled && strings.TrimSpace(res.FinalText) != "" {
		// Independent of persistCtx (and the request — this whole goroutine
		// is detached): titling makes its own LLM call, and sharing persistCtx's
		// 10s budget would let it starve the post-turn sweep below. The wait
		// never blocks the user — the title lands via buf.Emit + reattach. 18s
		// is wide margin over the default titling model's ~2-3s (SuggestTitle's
		// own 20s deadline is the backstop); a slower configured model that
		// overruns just leaves the default title in place.
		titleCtx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		title := s.agent.SuggestTitle(titleCtx, userInput, res.FinalText)
		cancel()
		if title != "" {
			if err := s.store.UpdateTitle(persistCtx, user, conv.ID, title); err != nil {
				log.Printf("auto-title UpdateTitle failed: %v", err)
			} else {
				buf.Emit("conversation.title_updated", map[string]any{
					"id":    conv.ID,
					"title": title,
				})
			}
		}
	}

	// Sweep expired conversations after every turn.
	if expired, evicted, err := s.store.SweepExpired(persistCtx,
		time.Duration(s.cfg.ConversationTTL)*24*time.Hour, s.cfg.UnpinnedCap); err != nil {
		log.Printf("post-turn sweep error: %v", err)
	} else if expired > 0 || evicted > 0 {
		log.Printf("sweep: %d expired, %d evicted", expired, evicted)
	}

	// Attachment sweep — see comment in cmd/chat-server/main.go.
	if removed, err := store.SweepAttachments(s.cfg.EmailAttachmentDir,
		time.Duration(s.cfg.ConversationTTL)*24*time.Hour); err != nil {
		log.Printf("post-turn attachment sweep error: %v", err)
	} else if removed > 0 {
		log.Printf("attachment sweep: %d files removed", removed)
	}

	// Orphan-workspace sweep: per-conversation workspace dirs whose
	// conversation row is gone (TTL, cap-evict, or user-account delete
	// cascade) get removed here. Anything the agent downloaded via
	// mcp_email_download_attachment into <root>/<convID>/ goes with
	// them.
	if removed, err := s.store.SweepOrphanWorkspaces(persistCtx,
		tools.WorkspaceDirForConversation("")); err != nil {
		log.Printf("post-turn workspace sweep error: %v", err)
	} else if removed > 0 {
		log.Printf("workspace sweep: %d orphan dirs removed", removed)
	}
}

// handleStream reattaches an SSE client to an in-flight (or recently
// finished) turn's event buffer. Query param turn_id, if present, must
// match the buffer's turn to guard against stale client retries racing
// a superseding turn. Last-Event-ID tells us how much the client has
// already processed.
//
// Fallback order when the in-memory buffer is gone (evicted after TTL
// or wiped by a restart):
//  1. If the query carries an explicit turn_id we know about in the
//     `turns` table, replay from turn_events.
//  2. Otherwise 204 — client should reload the DB-backed history.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	lastEventID := parseLastEventID(r)
	requestedTurnID := r.URL.Query().Get("turn_id")

	entry, ok := s.getInflight(convID)
	// If the client is asking about a different (older) turn than the one
	// we currently have buffered, fall through to the DB lookup below.
	if ok && entry.buf != nil && (requestedTurnID == "" || requestedTurnID == entry.turnID) {
		if err := entry.buf.Attach(r.Context(), lastEventID, w); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("stream Attach (user=%q conv=%q): %v", user, convID, err) //nolint:gosec // identifiers are %q-quoted
		}
		return
	}

	// DB fallback. Needs an explicit turn_id so we know which row to
	// look up — without one, the client should just reload history.
	if requestedTurnID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	turn, err := s.store.LookupTurn(r.Context(), requestedTurnID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if turn == nil || turn.ConversationID != convID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	events, err := s.store.LoadTurnEvents(r.Context(), requestedTurnID, lastEventID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := replayEventsFromDB(w, events); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("DB replay (user=%q conv=%q turn=%q): %v", user, convID, requestedTurnID, err) //nolint:gosec // identifiers are %q-quoted
	}
}

// replayEventsFromDB writes a slice of persisted events as SSE frames
// using the same framing the live buffer uses. Sets the SSE headers
// + flushes per event.
func replayEventsFromDB(w http.ResponseWriter, events []store.TurnEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for _, e := range events {
		if err := writeSSEFrame(w, flusher, bufferedEvent{
			ID:   e.EventID,
			Name: e.Name,
			Data: e.Data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// handleInflight is a cheap JSON probe the client calls on mount /
// visibilitychange / online to decide whether to open a reattach
// stream. Returns {inflight, turn_id?, last_event_id?}.
func (s *Server) handleInflight(w http.ResponseWriter, r *http.Request, convID string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	entry, ok := s.getInflight(convID)
	if !ok || entry.buf == nil {
		writeJSON(w, map[string]any{"inflight": false})
		return
	}

	writeJSON(w, map[string]any{
		"inflight":      entry.IsRunning(),
		"turn_id":       entry.turnID,
		"last_event_id": entry.buf.HighestID(),
	})
}

// handleWorkspaceFile streams a single file from the per-conversation
// workspace dir so the chat UI can render images / files the agent
// produced via run_python or wrote with write_file. Used by the
// markdown img interceptor in chat-experience.tsx — when the agent
// writes `![chart](spend_chart.png)` and saves spend_chart.png to its
// workspace, the UI rewrites the relative src to
// `/api/conversations/<convID>/workspace/spend_chart.png` and the
// browser fetches it from this handler.
//
// Auth: same as every other conversation route — the caller must own
// the conversation. relPath is interpreted relative to the conv's
// workspace dir; .. traversal and absolute paths are rejected. The
// resolved file must still live under the workspace dir after symlink
// resolution (filepath.EvalSymlinks) so a maliciously-placed symlink
// can't escape.
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request, convID, relPath string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	if relPath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}
	// The path arrives as a URL segment — decode percent-encoded chars
	// (spaces, parens etc. that pandas/matplotlib filenames sometimes
	// carry) before further validation.
	decoded, err := url.PathUnescape(relPath)
	if err != nil {
		http.Error(w, "bad path encoding", http.StatusBadRequest)
		return
	}
	relPath = decoded

	// Hard-reject obvious traversal + absolute paths up front. The
	// EvalSymlinks check below is the load-bearing safety net, but a
	// fast no on `..`/leading-`/` keeps the error message readable.
	if strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "..") || strings.ContainsRune(relPath, 0) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	wsDir, err := filepath.Abs(tools.WorkspaceDirForConversation(convID))
	if err != nil {
		http.Error(w, "resolve workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	full := filepath.Join(wsDir, filepath.FromSlash(relPath))

	// Resolve symlinks and confirm the result still lives under wsDir.
	// Without this, a `ln -s /etc/passwd workspace/<conv>/p` written by
	// the agent (or a malicious upload) would let any user with the
	// conversation read host secrets via this endpoint.
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "resolve path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		http.Error(w, "abs path: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if resolvedAbs != wsDir && !strings.HasPrefix(resolvedAbs, wsDir+string(filepath.Separator)) {
		http.Error(w, "path escapes workspace", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(resolvedAbs)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	// Content-Type from extension; mime.TypeByExtension handles the
	// common suffixes (.png/.jpg/.jpeg/.svg/.pdf/.csv/.json/.txt) and
	// returns "" for unknown — http.ServeContent's sniffing then takes
	// over. Either way the browser gets something it can render or
	// download.
	ctype := mime.TypeByExtension(filepath.Ext(resolvedAbs))
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	// Workspace files are effectively immutable from the user's point
	// of view: each run_python that saves a chart picks a new filename
	// (the agent emits e.g. `report__8a75730b.csv` or `chart_<uuid>.png`),
	// so a cache hit on a known URL is always the right answer. A
	// generous max-age + immutable directive is what stops the
	// scroll-flicker on mobile: when the user scrolls past a chart and
	// back, the browser serves from cache without revalidating instead
	// of paint-blanking while it re-decodes a 304. 24h is more than
	// enough for an active session and the file is gone after the
	// orphan-workspace sweep anyway.
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")

	f, err := os.Open(resolvedAbs) //nolint:gosec // resolvedAbs is validated to live under the workspace dir
	if err != nil {
		http.Error(w, "open: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(resolvedAbs), info.ModTime(), f)
}

// parseLastEventID extracts the `Last-Event-ID` header, falling back
// to the `last_event_id` query param. Returns 0 for missing/invalid
// values — the caller will replay from the beginning.
func parseLastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	if raw == "" {
		return 0
	}
	var id uint64
	if _, err := fmt.Sscanf(raw, "%d", &id); err != nil {
		return 0
	}
	return id
}

// ── helpers ────────────────────────────────────────────────────────────────

// exportFilename builds a safe, recognizable filename for the JSON
// download: slugified title + short id + .json. Keeps the Save dialog
// self-explanatory without trusting user-chosen characters in the
// Content-Disposition header.
func exportFilename(title, id string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == ' ', r == '-', r == '_':
			return '-'
		}
		return -1
	}, title)
	slug = strings.Trim(strings.ReplaceAll(slug, "--", "-"), "-")
	if len(slug) > 50 {
		slug = slug[:50]
	}
	if slug == "" {
		slug = "chat"
	}
	shortID := id
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return slug + "-" + shortID + ".json"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func truncateForTitle(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "New conversation"
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
