// Chat SSE turn path: POST /chat plus the shared turn-launch pipeline
// (startTurn), the detached turn goroutine (runTurnAsync) and the post-turn
// retention sweeps. Split out of server.go (#1127); the inflight-turn registry
// these build on (registerTurn/finishTurn) stays in server.go with the Server
// struct.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

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
	// MCPAccounts seeds the per-conversation credential-seat overrides
	// (#988; server name → account label) alongside EnabledOptional, so a
	// seat picked before the first message sticks. Same rules as POST
	// /conversations/{id}/mcp-servers: an unknown seat fails the request.
	MCPAccounts map[string]string `json:"mcp_accounts,omitempty"`
	// Lockdown mirrors createConversationRequest.Lockdown — honored
	// only when no ConversationID is provided (lockdown is set once
	// at conversation creation and immutable thereafter).
	Lockdown bool `json:"lockdown,omitempty"`
	// InputID is the caller's idempotency key (#785): a re-POST of the same
	// (conversation, input_id) while queued returns the existing item instead
	// of duplicating the input. Empty = server-generated.
	InputID string `json:"input_id,omitempty"`
	// InputIDScope "user" declares that the caller's input_ids are unique
	// per user, not just per conversation. Only then is a first submission
	// (no conversation yet) looked up by (user, input_id), so a resend whose
	// answer was lost before any header finds the conversation it already
	// started. Without it the key stays conversation-scoped, as documented,
	// so a client that numbers keys per conversation is never answered with
	// another conversation's replay.
	InputIDScope string `json:"input_id_scope,omitempty"`
	// Mode selects what a submission does when a turn is already running
	// (#785): "queue" (default) runs it as the next turn; "steer" offers it
	// to the running turn's next step boundary, falling back to queue if the
	// turn ends first. Ignored when the conversation is idle.
	Mode string `json:"mode,omitempty"`
	// SubmissionID is this POST's own identity (#1592) — NOT an idempotency
	// key (that is InputID). The server stamps it on the turn it starts for
	// this submission, where /inflight echoes it back, and stores it in the
	// queue row's own submission_id column when the submission queues instead
	// — never in client_input_id, which is the idempotency key. It exists
	// because a client whose acknowledgement was lost in transit has no other
	// evidence separating "the turn the server started for me" from "a turn
	// that was already running": the turn id is new either way, and startTurn
	// exposes a turn before its user message commits, so the transcript agrees
	// with both readings. Empty = the caller wants no such echo.
	SubmissionID string `json:"submission_id,omitempty"`
}

// conversationIDHeaderName names the conversation a POST /chat response is
// streaming, on the response headers rather than only in the first SSE frame
// (#1591).
//
// A brand-new chat posts under a client-side pending key and learns its real
// id from the `conversation` frame. If the stream dies between the response
// headers and that frame, the browser holds no id the server knows: /inflight
// and the persisted transcript are both keyed by conversation id, so the whole
// recovery chain has nothing to ask about and the tab shows a failed turn over
// an answer that is being written to the database. The id is known the moment
// the turn registers, which is before any frame is written, so saying it here
// costs nothing and closes the window.
const conversationIDHeaderName = "X-Fleet-Conversation-Id"

// turnIDHeaderName names the turn a POST /chat response is streaming, beside
// the conversation header and for the same reason: it is known the moment the
// turn registers, so a client that must address THIS turn later (a targeted
// Stop, `fleet acp`) never has to wait for the turn.started frame.
const turnIDHeaderName = "X-Fleet-Turn-Id"

// memoryContents renders the injectable memory bullets (#515): retired and
// still-proposed rows are EXCLUDED (retirement is the mechanism that stops
// stale-fact citations — the annotations below are explainability/tiebreaker
// signal, not staleness control), pinned rows survive the cap first (the
// store's list order), and a lightweight annotation carries the kind (when
// not a plain fact) and the user-declared validity window so the model can
// weigh time-scoped facts.
func memoryContents(memories []store.Memory) []string {
	out := make([]string, 0, len(memories))
	for _, memory := range memories {
		if memory.Source == "proposed" || memory.Retired() {
			continue
		}
		if len(out) >= 50 {
			break
		}
		content := strings.TrimSpace(memory.Content)
		if content == "" {
			continue
		}
		if note := memoryAnnotation(&memory); note != "" {
			content += " (" + note + ")"
		}
		out = append(out, content)
	}
	return out
}

// memoryAnnotation builds the parenthetical suffix for one injected memory.
// Kept deliberately lean — most memories are plain facts and get NO suffix,
// so the 50-bullet prompt doesn't pay a per-line token tax.
func memoryAnnotation(m *store.Memory) string {
	var parts []string
	if m.Kind != "" && m.Kind != "fact" {
		parts = append(parts, m.Kind)
	}
	if m.ValidFrom != nil {
		parts = append(parts, "true since "+time.Unix(*m.ValidFrom, 0).UTC().Format("2006-01-02"))
	}
	if m.ValidTo != nil {
		parts = append(parts, "until "+time.Unix(*m.ValidTo, 0).UTC().Format("2006-01-02"))
	}
	return strings.Join(parts, ", ")
}

// applyTurnModelOverride handles postChat's per-turn model override on an
// existing conversation: if the caller passed a NEW non-empty slug, persist it
// so the next reload reflects the user's choice. An empty reqModel is treated
// as "no opinion, keep whatever's stored" — otherwise a transient state race on
// the client (new-chat reset, reload before the `conversation` event rehydrates
// selectedModel) silently wipes the stored override and the next turn quietly
// falls back to the server primary. To explicitly clear the override, the
// dedicated PATCH /conversations/{id}/model endpoint can send "".
//
// Lockdown model allow-list (#568): the override must pass the SAME guard as
// PATCH /conversations/{id}/model and conversation create — otherwise this
// would be the one path that lets a lockdown conversation persist and run a
// model the operator excluded from CHAT_LOCKDOWN_ALLOWED_MODELS. Reports false
// after writing the HTTP error; the caller must return without running the turn.
func (s *Server) applyTurnModelOverride(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, reqModel string) bool {
	if reqModel == "" {
		return true
	}
	if conv.Lockdown && !s.cfg.LockdownAllows(reqModel) {
		// A model the operator excluded is refused, never quietly swapped for
		// another one: a caller that asked for a specific model and got a turn
		// on a different one was told nothing, and a deliberate API client has
		// no way to learn that this deployment forbids the slug it sends
		// (#1588).
		//
		// A bare 400 was not enough on its own, which is why this path spent a
		// release ignoring the slug instead. A lockdown picker only ever offers
		// allow-listed models, so a disallowed slug usually means a browser is
		// echoing something the server itself told it before the list — or the
		// tiers behind it — moved, and refusing that browser with nothing but
		// "no" leaves it looping on a stale slug until the user reloads the
		// page. So the refusal NAMES the model to use instead
		// (lockdownCorrectedModel): an API client gets a real error carrying
		// the answer, and the web adopts that slug and retries the turn once.
		//
		// Both halves are stateless on purpose. The alternative considered and
		// rejected was a per-conversation memo of the exact pre-migration slug:
		// it could not survive a process restart, could not speak for a second
		// tab, and lost the first hop when a conversation was migrated twice.
		// Correcting the client from the allow-list needs no memory at all.
		// The invariant is untouched either way: a disallowed model is never
		// persisted and never runs.
		//
		// What deliberately does NOT reach here: postChat maps a client's echo
		// of the conversation's OWN stored slug to "no opinion" (reqModel ==
		// conv.Model → ""), so a conversation whose persisted model was
		// delisted still migrates on the launch path
		// (reconcileLockdownModelCtx) rather than 400ing at its owner.
		next := s.lockdownCorrectedModel(conv)
		log.Printf("lockdown: conversation %s refused a disallowed model %q from the client and offered %q instead", logSafe(conv.ID), logSafe(reqModel), logSafe(next))
		writeLockdownModelRefusal(w, next)
		return false
	}
	if reqModel != conv.Model {
		if err := s.store.SetModel(r.Context(), user, conv.ID, reqModel); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return false
		}
		conv.Model = reqModel
	}
	return true
}

// lockdownModelRefusalCode is the machine-readable marker on the body of a
// refused lockdown model override. A client keys its self-correction off this
// rather than off the prose, which is free to change.
const lockdownModelRefusalCode = "lockdown_model_not_allowed"

// writeLockdownModelRefusal answers a refused lockdown model override with 400
// AND, when there is one, the slug the caller should adopt (#1588) — so the
// refusal carries its own remedy instead of being a dead end the caller can
// only escape by reloading the page. The body stays a superset of the plain
// error text every other lockdown guard writes: `error` is the same sentence,
// `code` identifies the refusal, `model` is the correction.
func writeLockdownModelRefusal(w http.ResponseWriter, model string) {
	body := map[string]any{
		"error": "model not allowed in lockdown mode",
		"code":  lockdownModelRefusalCode,
	}
	if model != "" {
		body["model"] = model
	}
	writeJSONStatus(w, http.StatusBadRequest, body)
}

// lockdownCorrectedModel names the model a refused override should be replaced
// with: the conversation's own when the allow-list still permits it, otherwise
// the lockdown default its next turn launch would migrate it to anyway
// (reconcileLockdownModelCtx). It is NEVER a slug the allow-list forbids —
// handing one back would put a retrying client straight into the loop this
// mechanism exists to break — so an operator list with no literal slug at all
// yields "" and the refusal carries no correction: the caller must choose.
func (s *Server) lockdownCorrectedModel(conv *store.Conversation) string {
	if s.cfg.LockdownAllows(conv.Model) {
		return conv.Model
	}
	return lockdownDefaultSlug(s.cfg.LockdownModels())
}

// lockdownDefaultSlug picks the slug a delisted lockdown conversation is moved
// to: the first LITERAL entry of the allow-list. A glob (`anthropic/*`) names a
// family, not a model, so it is skipped rather than persisted as a model; an
// operator list made only of globs yields "" and the conversation is left to
// the guard.
func lockdownDefaultSlug(allowed []string) string {
	for _, slug := range allowed {
		slug = strings.TrimSpace(slug)
		if slug == "" || strings.ContainsAny(slug, "*?[") {
			continue
		}
		return slug
	}
	return ""
}

// reconcileLockdownModelCtx moves a lockdown conversation whose PERSISTED model is
// no longer on the allow-list onto the lockdown default — the first entry of
// config.LockdownModels — and persists that choice. A conversation pins its
// model at creation and the manager re-validates it on every turn, so without
// this step every lockdown chat created before the allow-list changed (the
// operator narrowed FLEET_LOCKDOWN_ALLOWED_MODELS, the shipped tier defaults
// moved on an upgrade, or an admin overrode a tier in Settings) would fail each
// turn with "model not allowed in lockdown mode" until the user found the
// picker. This is a migration IN FRONT of the guard, not a bypass of it: the
// manager still rejects whatever it is handed if it is not allow-listed, and
// the explicit per-turn override in postChat still 400s on a disallowed slug.
// A glob-only list (no literal slug to move to) is left to the guard; a list
// that merely STARTS with a glob moves to its first literal slug.
// reconcileLockdownModelCtx is the migration itself. It runs on the shared
// launch path (startTurn), so a direct submission and a queue-drained
// follow-up (#785) get the same treatment, and the `conversation` event the
// launching turn emits carries the new model to the client. Idempotent — a
// conversation already on an allowed model is untouched.
func (s *Server) reconcileLockdownModelCtx(ctx context.Context, user string, conv *store.Conversation) error {
	if !conv.Lockdown || conv.Model == "" || s.cfg.LockdownAllows(conv.Model) {
		return nil
	}
	next := lockdownDefaultSlug(s.cfg.LockdownModels())
	if next == "" {
		return nil
	}
	if err := s.store.SetModel(ctx, user, conv.ID, next); err != nil {
		return err
	}
	log.Printf("lockdown: conversation %s moved from model %q (no longer allow-listed) to the lockdown default %q", logSafe(conv.ID), logSafe(conv.Model), logSafe(next)) //nolint:gosec // G706: logSafe strips CR/LF from the conversation id and both slugs.
	conv.Model = next
	return nil
}

func (s *Server) postChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Draining (#278): once graceful shutdown begins, admit no new turns — the
	// client should retry against a healthy instance. In-flight turns keep going.
	if s.shuttingDown.Load() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
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
	// The key is indexed (btree entries have a size limit), so it is bounded
	// here rather than left to fail as a database error: a UUID, or fleet
	// acp's hashed keys, fit many times over.
	if len(req.InputID) > maxInputIDLen || len(req.SubmissionID) > maxInputIDLen {
		http.Error(w, fmt.Sprintf("input_id and submission_id are limited to %d bytes", maxInputIDLen), http.StatusBadRequest)
		return
	}

	// Resolve conversation: find existing, or create new.
	var (
		conv *store.Conversation
		err  error
	)
	// unlockFirst releases a user-scoped key's lock (lockUserScopedKey)
	// once its key is claimed; deferred too, for every earlier return.
	unlockFirst := func() {}
	defer func() { unlockFirst() }()
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
		// A user-unique key is claimable in any conversation, and the
		// database's uniqueness is per conversation: its lock is held from
		// the lookup below until the key is claimed (or queued), so a
		// concurrent send of the same key into another conversation finds
		// this one's row instead of claiming the key a second time.
		unlockFirst = s.lockUserScopedKey(user, req)
		// A resend of an accepted input is answered before anything below
		// touches the conversation: a replay must not re-apply the original
		// request's model or un-archive the conversation.
		if s.replayAcceptedInput(w, r, user, conv.ID, req) {
			return
		}
		// The web echoes the conversation's stored model on every turn. An echo
		// is "no opinion" — it can never be an override — so it must not trip
		// the lockdown guard when that stored model has since been delisted.
		// The migration itself happens where the turn LAUNCHES (startTurn, the
		// path shared by direct and queue-drained turns), whose `conversation`
		// event then tells the client the new model. Doing it here would also
		// run for a busy-path steer, which launches no turn and so would leave
		// the browser holding a slug the server had already replaced. A
		// genuinely different disallowed slug still 400s below.
		if reqModel == conv.Model {
			reqModel = ""
		}
		if !s.applyTurnModelOverride(w, r, user, conv, reqModel) {
			return
		}
		// Sending a message to an archived conversation un-archives it (#282) —
		// mirrors how replying to an archived email brings it back to the inbox.
		if conv.ArchivedAt != nil {
			if err := s.store.SetArchived(r.Context(), user, conv.ID, false); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conv.ArchivedAt = nil
		}
	} else {
		// A first submission's resend (same input_id, still no conversation id
		// because the original response was lost before any header) must find
		// the input it already accepted — before this would create a second
		// conversation and run the prompt again there.
		unlock, handled := s.recoverFirstSubmission(w, r, user, req)
		if handled {
			return
		}
		unlockFirst = unlock
		persona := strings.TrimSpace(req.Persona)
		if persona == "" {
			persona = s.cfg.PersonaDefault
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			// Instant heuristic title (#302): a real noun-phrase name with zero
			// I/O so the sidebar shows something meaningful immediately; the
			// async LLM titler may upgrade it (unless the user locks it).
			title = agent.HeuristicTitle(req.Message)
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
		// Seed the optional MCP server opt-in list (and seat overrides, #988)
		// from the chat request so pre-chat Tools picker selections take
		// effect on this first turn.
		if !s.seedConversationMCP(w, r, user, conv, req.EnabledOptional, req.MCPAccounts) {
			return
		}
	}

	// Busy path (#785): a running turn means this submission QUEUES — never an
	// implicit cancel. The row is durable before the 202 acknowledgement;
	// steer-mode rows are additionally offered to the running turn's next
	// step boundary. Explicit /cancel remains the only Stop.
	if entry, ok := s.getInflight(conv.ID); ok && entry.IsRunning() {
		s.handleBusySubmit(w, r, user, conv, req)
		return
	}

	// Concurrent-turn admission: cap simultaneous in-flight turns per user so one
	// user can't hold every worker slot with parallel long turns. The slot is
	// held for the turn GOROUTINE's lifetime (released in the goroutine below),
	// not just this HTTP handler's — postChat returns as soon as the turn is
	// launched, but the work continues in the background.
	releaseSlot, admitted := s.admitConcurrentTurn(w, user)
	if !admitted {
		return
	}

	// A direct turn claims its input_id before it launches, in the same
	// table and unique index as queued inputs, so a caller whose stream was
	// lost can resend the same key and get the input it already started back
	// instead of a second run (model spend and tool side effects included).
	// The replay lookup above answers most repeats; the claim is what closes
	// the race between two concurrent submissions of one key.
	direct, handled := s.claimDirectInput(w, r, user, conv, req, releaseSlot)
	unlockFirst() // the key is claimed (or answered): later sends find it
	if handled {
		return
	}

	if !s.startTurn(w, r, user, conv, req, nil, releaseSlot, direct) {
		// A concurrent submission won the registerTurn race between our busy
		// check and now; the input must not be lost — queue it instead. The
		// direct claim is released first, or the queue insert would find it
		// and answer with a "running" row that no turn is running. If the
		// release cannot be confirmed the submission fails rather than being
		// acknowledged: an acknowledgement of that row would promise a run
		// that nothing will ever perform.
		releaseSlot()
		// The key's lock again, for the release and the queue insert: a
		// concurrent user-scoped send must find this input's row, not the
		// gap between the two.
		relock := s.lockUserScopedKey(user, req)
		defer relock()
		if direct != nil && !s.releaseDirectInput(user, conv.ID, direct) {
			http.Error(w, "the message could not be queued behind the running turn; send it again", http.StatusServiceUnavailable)
			return
		}
		if !s.handleBusySubmit(w, r, user, conv, req) && direct != nil {
			// The queue refused it (full, or a store error). A concurrent
			// resend may already have been told the claim is running, so the
			// key must not be left without an outcome: it is settled
			// "cancelled" (nothing ran), which a resend reads as safe to send
			// again, rather than vanishing.
			s.settleUnqueuedInput(user, conv.ID, strings.TrimSpace(req.InputID))
		}
	}
}

// replayAcceptedInput answers a resend of an input_id that was already
// ACCEPTED (queued, running, or terminal) with that input's acknowledgement,
// so it never runs a duplicate turn — even when the retry lands after the
// conversation went idle (#785). A caller declaring user-unique keys
// (input_id_scope "user") is looked up per user, so a resend finds its input
// whichever conversation accepted it; otherwise keys are conversation-scoped.
// A first submission (no conversation yet) is recoverFirstSubmission's.
// handled reports that the response was written.
func (s *Server) replayAcceptedInput(w http.ResponseWriter, r *http.Request, user, convID string, req chatRequest) (handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" {
		return false
	}
	var existing *store.InputQueueRow
	var err error
	if strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		existing, err = s.store.LookupInputForUser(r.Context(), user, clientID)
	} else {
		existing, err = s.store.LookupInput(r.Context(), convID, clientID)
	}
	if err != nil {
		// Fail closed: proceeding without the lookup could run (and bill) a
		// duplicate turn for an input_id that was already accepted. A 500
		// lets the client retry the same input_id safely.
		http.Error(w, "input lookup failed: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	if existing == nil {
		return false
	}
	writeQueueAck(w, http.StatusOK, existing.ConversationID, *existing)
	return true
}

// lockUserScopedKey takes the per-(user, key) lock for a submission that
// declares user-unique keys and returns its (idempotent) unlock; for any
// other submission it locks nothing. The lock is in-process: fleet's control
// plane is single-replica by design (the inflight registry the Stop gate
// relies on is too), and it holds no database connection while the
// protected work asks the pool for one.
func (s *Server) lockUserScopedKey(user string, req chatRequest) (unlock func()) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" || !strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		return func() {}
	}
	return sync.OnceFunc(s.inputKeyLocks.lock(user + "\x00" + clientID))
}

// recoverFirstSubmission answers a first submission (no conversation yet)
// whose user-unique key (input_id_scope "user") was already accepted, with
// that input's acknowledgement. It takes the key's lock first, held until
// the key is claimed (the returned unlock; a no-op when nothing was
// locked), so a concurrent resend waits for the first request's claim
// instead of both creating a conversation. The lock is in-process: fleet's
// control plane is single-replica by design (the inflight registry the Stop
// gate relies on is too), and an in-process lock holds no database
// connection while the protected work asks the pool for one. handled
// reports that the response was written.
func (s *Server) recoverFirstSubmission(w http.ResponseWriter, r *http.Request, user string, req chatRequest) (unlock func(), handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" || !strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		return func() {}, false
	}
	unlock = s.lockUserScopedKey(user, req)
	existing, err := s.store.LookupInputForUser(r.Context(), user, clientID)
	if err != nil {
		unlock()
		http.Error(w, "input lookup failed: "+err.Error(), http.StatusInternalServerError)
		return func() {}, true
	}
	if existing != nil {
		unlock()
		writeQueueAck(w, http.StatusOK, existing.ConversationID, *existing)
		return func() {}, true
	}
	return unlock, false
}

// claimDirectInput claims a direct submission's input_id before its turn
// launches. handled reports that the response was already written (a replay
// of the key, a claim failure, or a Stop that covered the claim).
func (s *Server) claimDirectInput(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, req chatRequest, releaseSlot func()) (claim *directClaim, handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" {
		return nil, false
	}
	claimID := uuid.NewString()
	row, created, err := s.store.ClaimDirectInput(r.Context(), store.InputQueueRow{
		ID: claimID, ConversationID: conv.ID, UserEmail: user,
		ClientInputID: clientID, SubmissionID: strings.TrimSpace(req.SubmissionID),
		Message: req.Message, Attachments: inputAttachmentsJSON(req),
	})
	if err != nil {
		// The claim may have committed with its acknowledgement lost. No
		// turn will run under it, so it is released (by its own id — never
		// another submission's claim) rather than left 'running' to answer
		// every resend "already running".
		releaseSlot()
		s.settleDirectInput(claimID, "") // settled "did not run", never left running
		http.Error(w, "input claim failed: "+err.Error(), http.StatusInternalServerError)
		return nil, true
	}
	if !created {
		releaseSlot()
		writeQueueAck(w, http.StatusOK, conv.ID, row)
		return nil, true
	}
	// The claim is an accepted input, so a Stop scope=all that begins
	// from here on covers it the way it covers a queued row — but the
	// Stop's queue sweep skips it (a claim is already 'running') and no
	// turn exists yet to cancel. So it carries the same gate a drained
	// row does: refused here if a Stop already began after acceptance,
	// and at registration if one begins while the turn is prepared.
	gen, stopped := s.stopGateForRow(conv.ID, row.AcceptedSeq)
	if stopped {
		releaseSlot()
		s.cancelStoppedDirectInput(w, row.ID)
		return nil, true
	}
	return &directClaim{id: row.ID, key: clientID, sweepGen: gen}, false
}

// maxInputIDLen bounds a caller's input_id / submission_id.
const maxInputIDLen = 256

// directClaim is a direct submission's idempotency claim as startTurn needs
// it: the claim row, and the Stop generation read when it was accepted (the
// registration gate refuses the launch if a Stop scope=all begins after).
type directClaim struct {
	id, key  string
	sweepGen uint64
}

// cancelStoppedDirectInput settles a claim that a Stop scope=all covered
// before its turn launched as cancelled (nothing ran), and tells the caller.
func (s *Server) cancelStoppedDirectInput(w http.ResponseWriter, id string) {
	s.settleDirectInput(id, "")
	http.Error(w, "a Stop in this conversation cancelled this message before it started", http.StatusConflict)
}

// directReleaseRetries bounds the background retries of a failed release,
// with delays doubling from directReleaseBackoff (about a minute in total).
const directReleaseRetries = 6

var directReleaseBackoff = time.Second // a var so tests can shorten it

// releaseDirectInput frees a direct claim whose turn never launched, so its
// key can be retried, and reports whether the release landed. It is tried a
// few times in place; one that still fails is retried in the background,
// because a claim left behind with no turn reads as "already running" to
// every resend of its key, and nothing would settle it before the next boot
// recovery.
func (s *Server) releaseDirectInput(user, convID string, c *directClaim) bool {
	if c == nil || c.id == "" {
		return true
	}
	for attempt := range directReleaseAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * directReleasePause)
		}
		if s.tryReleaseDirectInput(c.id) {
			return true
		}
	}
	// Unconfirmed: the delete may have committed with its acknowledgement
	// lost, and a concurrent resend may already have been told the claim is
	// running. Deleting again in the background could leave that key with no
	// record at all, so the key is settled "cancelled" (nothing ran) instead:
	// the claim if it is still there, a cancelled row if it is gone.
	s.settleUnreleasedInput(user, convID, c)
	return false
}

// settleUnreleasedInput gives a claim whose release could not be confirmed a
// durable outcome — cancelled, nothing ran — retried in the background.
func (s *Server) settleUnreleasedInput(user, convID string, c *directClaim) {
	try := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.SettleDirectInput(ctx, c.id, ""); err != nil {
			log.Printf("settle unreleased input (input=%s): %v", c.id, err)
			return false
		}
		if _, _, err := s.store.CancelInputKey(ctx, store.InputQueueRow{
			ID: uuid.NewString(), ConversationID: convID, UserEmail: user, ClientInputID: c.key,
		}); err != nil {
			log.Printf("settle unreleased input (input=%s): %v", c.id, err)
			return false
		}
		return true
	}
	if !try() {
		s.retryDirectInput("settle_unreleased", c.id, 1, directReleaseBackoff, try)
	}
}

// directReleaseAttempts and directReleasePause bound releaseDirectInput's
// in-place attempts (and directBindAttempts bindDirectInput's).
const (
	directReleaseAttempts = 3
	directBindAttempts    = 3
)

var directReleasePause = 100 * time.Millisecond // a var so tests can shorten it

// bindDirectInputIf binds id when there is a direct claim; with none it
// reports bound (nothing to bind).
func (s *Server) bindDirectInputIf(id, turnID string) (bound, stopped bool) {
	if id == "" {
		return true, false
	}
	return s.bindDirectInput(id, turnID)
}

// bindDirectInput stamps a direct claim with its turn id, retrying a few
// times. bound false with stopped true means the claim is no longer running
// (a Stop cancelled it durably); bound false alone means the bind failed.
func (s *Server) bindDirectInput(id, turnID string) (bound, stopped bool) {
	for attempt := range directBindAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * directReleasePause)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		bound, err := s.store.BindInputTurn(ctx, id, turnID)
		cancel()
		if err == nil {
			return bound, !bound // not bound: the claim is no longer running, so do not launch
		}
		log.Printf("bind direct input turn (input=%s turn=%s): %v", id, turnID, err)
	}
	return false, false
}

func (s *Server) tryReleaseDirectInput(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.ReleaseDirectInput(ctx, id); err != nil {
		log.Printf("release direct input (input=%s): %v", id, err)
		return false
	}
	return true
}

// retryDirectInput retries one write to a direct claim in the background,
// doubling the delay, until it lands or directReleaseRetries run out (boot
// recovery then settles the claim). A claim left 'running' with no live turn
// would answer every resend of its key "already running".
func (s *Server) retryDirectInput(what, id string, attempt int, delay time.Duration, try func() bool) {
	if attempt > directReleaseRetries {
		log.Printf("%s direct input (input=%s): giving up after %d retries; boot recovery settles it", what, id, directReleaseRetries)
		return
	}
	s.background.After("httpapi.direct_"+what, delay, func() {
		if !try() {
			s.retryDirectInput(what, id, attempt+1, 2*delay, try)
		}
	})
}

// settleDirectInput resolves a direct claim (completed when turnID's user
// entry committed, otherwise cancelled) on its own bounded context, retrying
// a failure in the background.
func (s *Server) settleDirectInput(id, turnID string) {
	try := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.SettleDirectInput(ctx, id, turnID); err != nil {
			log.Printf("settle direct input (input=%s turn=%s): %v", id, turnID, err)
			return false
		}
		return true
	}
	if !try() {
		s.retryDirectInput("settle", id, 1, directReleaseBackoff, try)
	}
}

// settleTurnInputs settles a finished turn's queue rows (SettleTurnInputs),
// first cancelling what a Stop by key named while the turn ran — an injected
// steer, or the drained row itself — so an uncommitted one is not returned to
// the queue for a drain to run after its Stop.
func (s *Server) settleTurnInputs(buf *turnBuffer, turnID, queueRowID string) (requeued, cancelledSteers int, err error) {
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	for _, id := range buf.stoppedSteerIDs() {
		// Guarded: a steer whose text committed is completed already.
		if _, err := s.store.CancelStoppedSteer(sctx, id); err != nil {
			log.Printf("cancel stopped steer (input=%s turn=%s): %v", id, turnID, err)
		}
	}
	drainedID := queueRowID
	if drainedID != "" && buf.stoppedByKey.Load() && !s.cancelStoppedDrain(queueRowID, turnID) {
		drainedID = "" // left to the background retry: never re-queued here
	}
	return s.store.SettleTurnInputs(sctx, turnID, drainedID)
}

// cancelStoppedDrain cancels the drained row of a turn a Stop named by its
// input key, before the turn's settlement could return it to the queue. The
// Stop may have been answered before the turn's terminal frame (202,
// unconfirmed), so the Stop's own row write cannot be what keeps the input
// from running again. Guarded: an input whose user entry committed ran, and
// the settlement records it completed. It reports whether the write landed;
// on false the row must not be settled now (a re-queue is exactly what it
// prevents) — a background retry cancels and then settles it, and boot
// recovery covers a retry that runs out.
func (s *Server) cancelStoppedDrain(rowID, turnID string) bool {
	try := func(settle bool) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.store.CancelStoppedDrain(ctx, rowID, turnID); err != nil {
			log.Printf("cancel stopped drain (input=%s turn=%s): %v", rowID, turnID, err)
			return false
		}
		if settle {
			if _, _, err := s.store.SettleTurnInputs(ctx, turnID, rowID); err != nil {
				log.Printf("settle stopped drain (input=%s turn=%s): %v", rowID, turnID, err)
				return false
			}
		}
		return true
	}
	if try(false) {
		return true
	}
	s.retryDirectInput("cancel_stopped_drain", rowID, 1, directReleaseBackoff, func() bool { return try(true) })
	return false
}

// inputAttachmentsJSON is the attachments column for an input row.
func inputAttachmentsJSON(req chatRequest) string {
	if len(req.Attachments) > 0 {
		if raw, err := json.Marshal(req.Attachments); err == nil {
			return string(raw)
		}
	}
	return "[]"
}

// startTurn runs one accepted input as a full turn: history + context prep,
// buffer registration, prompt assembly, and the detached run goroutine. It is
// the shared launch path for direct submissions (w/r set; the response
// attaches to the buffer) and queue-drained inputs (#785; w/r nil, queueRowID
// set). Returns false ONLY when registerTurn refused because a turn is
// already running — every other failure is handled (responded/logged)
// internally. releaseSlot is released by the turn goroutine on completion;
// on false the caller releases it. direct is a direct submission's
// idempotency claim (nil for none, and always nil for a queue drain): gated
// against a Stop like a drained row, bound to the turn once it registers
// (the turn is dropped if that fails), released if the turn never launches,
// and settled when it ends.
func (s *Server) startTurn(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, req chatRequest, queued *queuedLaunch, releaseSlot func(), direct *directClaim) bool {
	queueRowID := ""
	// gate carries the Stop generation the launch is checked against: a
	// drained row's, or a direct claim's (both are accepted inputs).
	gate := queued
	if queued != nil {
		queueRowID = queued.rowID
	}
	directInputID := ""
	if direct != nil {
		directInputID = direct.id
		gate = &queuedLaunch{sweepGen: direct.sweepGen, inputKey: direct.key}
	}
	reqCtx := context.Background()
	if r != nil {
		reqCtx = r.Context()
	}
	fail := func(status int, err error) {
		// The turn goroutine never launches on this path, so the concurrency
		// slot admitted before startTurn must be released HERE (the original
		// pre-#785 flow errored before admission; the extraction inverted it).
		releaseSlot()
		if w != nil {
			// Settled cancelled, not deleted: a concurrent resend may
			// already have been told this claim is running, and it must
			// then find an outcome ("did not run"), not a missing row.
			if directInputID != "" {
				s.settleDirectInput(directInputID, "")
			}
			http.Error(w, err.Error(), status)
			return
		}
		log.Printf("queued turn launch (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
		s.terminalizeQueueRow(conv.ID, queued.rowID, queued.claimTurnID, store.InputStateQueued)
		// No turn launched, so no completion tail will re-drain: without an
		// explicit re-kick a 202-acknowledged row stalls until the next
		// submission on this conversation (possibly forever).
		s.rekickDrainAfter(conv.ID, 3*time.Second)
	}
	// Load history before we even allocate a buffer — if this errors, the
	// client never sees a partial SSE stream.
	history, err := s.store.LoadHistory(reqCtx, conv.ID)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return true
	}

	// Project context (#509): a conversation in a project injects the
	// project's standing instructions plus its SHARED memories (tagged
	// "[project] ") alongside personal memory.
	projectInstructions, projectMemoryBullets := s.projectTurnContext(reqCtx, conv)
	memories, err := s.store.ListMemories(reqCtx, user)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return true
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
	steer := newSteerMailbox(s.store, user, conv.ID, "", nil)
	buf, turnID, turnToken, ok, swept := s.registerTurnGated(conv.ID, turnCancel, steer, gate, strings.TrimSpace(req.SubmissionID))
	if swept && queued == nil {
		// A Stop scope=all began while this direct claim's turn was being
		// prepared, or a Stop named its key: the claim belongs to the
		// stopped set, so it is cancelled, never launched.
		turnCancel()
		releaseSlot()
		s.cancelStoppedDirectInput(w, directInputID)
		return true
	}
	if swept {
		// A Stop scope=all began after this drain decided its row was
		// post-Stop (it may still be sweeping, or have finished while we
		// were loading the conversation and history), and the drained row
		// was already claimed, so the sweep could not see it. It belongs to
		// the swept set: cancel it here rather than un-claim it — an
		// un-claimed row would outlive the sweep and launch on the re-kick.
		turnCancel()
		// The user's admission slot goes back FIRST: nothing below needs it,
		// and both writes are best-effort store calls that a slow or
		// unreachable database could stall — the slot must not be held for
		// that long, or repeats of this race exhaust the per-user cap while
		// no turn is running. terminalizeQueueRow bounds its own context (and
		// retries); the queue refresh gets a bounded one here.
		if releaseSlot != nil {
			releaseSlot()
		}
		s.terminalizeQueueRow(conv.ID, queued.rowID, queued.claimTurnID, store.InputStateCancelled)
		qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.emitQueueUpdate(qctx, user, conv.ID)
		qcancel()
		return true
	}
	if !ok {
		turnCancel()
		return false
	}
	// Bind the mailbox to its turn identity now that registerTurn minted it.
	// The run goroutine (the only Poll consumer) has not launched yet, so no
	// injection can precede the binding.
	steer.turnID, steer.buf = turnID, buf
	if bound, stopped := s.bindDirectInputIf(directInputID, turnID); !bound && stopped {
		// A Stop cancelled the claim durably before its turn could start:
		// nothing ran, and the caller is told so (409), not invited to retry.
		turnCancel()
		s.finishTurn(conv.ID, turnToken)
		releaseSlot()
		http.Error(w, "a Stop in this conversation cancelled this message before it started", http.StatusConflict)
		return true
	} else if !bound {
		// Fail closed: an unbound claim cannot be matched to this turn's
		// durable record, so a crash would settle it "never ran" and let a
		// resend run the input again. The turn is dropped before it runs.
		turnCancel()
		s.finishTurn(conv.ID, turnToken)
		fail(http.StatusInternalServerError, errors.New("the message could not be recorded against its turn; send it again"))
		return true
	}
	if queueRowID != "" {
		// Stamp the REAL turn id on the drained row (the claim used a
		// placeholder): the settle/recovery predicates check THIS turn's
		// durable #798 record, and a stale placeholder would re-queue —
		// double-run — an already-committed input after a crash.
		bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
		bound, err := s.store.BindInputTurn(bctx, queueRowID, turnID)
		bcancel()
		if err != nil {
			log.Printf("bind input turn (input=%s turn=%s): %v", queueRowID, turnID, err)
		} else if !bound {
			// The row is no longer running: a Stop cancelled it durably
			// after the drain claimed it. Its turn must not run.
			turnCancel()
			s.finishTurn(conv.ID, turnToken)
			if releaseSlot != nil {
				releaseSlot()
			}
			return true
		}
	}

	// Wire incremental persistence so a crash mid-turn leaves a
	// recoverable ledger in turn_events. Non-fatal — if the DB is
	// flaky, live streaming still works; crash recovery just won't.
	// Rooted in Background, not the request: the turn is designed to outlive
	// the POST (that is what the ledger is FOR), so a client that drops the
	// socket in the window between registerTurn and CreateTurn must not
	// leave the whole turn running with no ledger to replay on reconnect.
	persistCtx, persistCancelAttach := context.WithTimeout(context.Background(), 5*time.Second)
	if err := buf.attachPersister(persistCtx, s.store); err != nil {
		log.Printf("attachPersister (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
	}
	persistCancelAttach()

	// Attachment metadata (if any) is re-validated server-side, then split
	// by kind. Images flow into the model as multimodal vision input via
	// TurnInput.ImageAttachments; other files are staged into this
	// conversation's workspace and referenced by path, so view_file / bash /
	// run_python can reach them. Both kinds are named in the injected block
	// below so the agent sees what arrived.
	// Validated against the CALLER's own uploads subtree: naming another
	// user's upload path is a rejection, not a read (ADR-0058).
	validAttachments := s.validateAttachments(user, req.Attachments)
	imageAttachments, otherAttachments := splitAttachmentsByKind(validAttachments)
	// Copy non-image attachments into THIS conversation's workspace and
	// advertise those paths (ADR-0058). The uploads root is control-plane
	// state no sandbox mounts on either backend, so the staged copy is what
	// makes an attachment readable at all — and a path in one conversation's
	// workspace is not a path another conversation's turn was ever handed.
	// Images are exempt: their bytes reach the model host-side as vision
	// input, never through a sandbox read.
	otherAttachments = stageAttachmentsIntoWorkspace(
		userUploadsRoot(s.cfg.EmailAttachmentDir, user), conv.ID, otherAttachments)
	// Everything appended from here on is SERVER-INJECTED context, not
	// something the user typed: it is accumulated separately, persisted in its
	// own column (migration 056), and joined to the user's text only for the
	// provider call (agent.ComposeUserMessage). That split is what keeps a
	// branch copy — and the transcript's user bubble — free of the owner's
	// attachment paths and the admin's library listing. Chaining the SAME
	// appenders from an empty base preserves their byte layout, so
	// text+injected recomposes exactly the string this used to build.
	injected := appendAttachmentsBlock("", imageAttachments, otherAttachments)
	// Surface files persisted from earlier turns. The agent's run_python
	// kernel resets each turn but its workspace dir doesn't — without this,
	// a report downloaded on turn 1 gets forgotten by turn 4 even though
	// it's still on disk. Empty workspaces (first turn) skip the block.
	injected = appendWorkspaceInventoryBlock(injected, tools.WorkspaceDirForConversation(conv.ID))
	// Announce the cross-chat shared file library (docs/SHARED-FILES.md) the
	// same way: read-only paths under shared/ the agent can use immediately.
	injected = s.appendSharedFilesBlock(turnCtx, injected)
	// Composer context handles (#517, opt-in): expand any `@url:<url>` /
	// `@file:"path"` in the user's message into the turn context. A no-op when
	// disabled; failures degrade to notices so the turn always proceeds.
	injected = s.applyContextHandles(turnCtx, injected, req.Message, conv.ID)
	// Explicit skill invocation (#513 phase 1): a message whose first line starts
	// with "/<skill-name>" (exact match against the bundle roster) gets a block
	// appended telling the agent to read that skill's SKILL.md now. Because the
	// block is persisted with the user message (as its injected context), the
	// transcript still records which skill was invoked. Unknown "/tokens" are
	// ignored — no block, no error.
	injected = s.applySkillInvocation(turnCtx, user, injected, req.Message)
	// Connector auto-recommendation (#512, opt-in): if the message is relevant to
	// an Optional connector the user hasn't enabled, note it so the agent can
	// suggest connecting it via /settings/connections (never auto-connecting).
	// "Enabled" is judged against the conversation's PERSISTED opt-in list —
	// the set the turn runs with — not only the creation-time request seed,
	// which later turns never carry.
	injected = s.applyConnectorRecommendations(injected, req.Message, conv.OptionalMCPServersEnabled, req.EnabledOptional)

	// Lockdown model migration, at the last moment before the client can be
	// TOLD about it: every fallible preparation above (history, memories,
	// turn registration) has succeeded, and the `conversation` event below
	// carries conv.Model to the browser, so a stored model that fell off the
	// allow-list is replaced only on a turn that actually launches. Migrating
	// earlier and then failing a preparation would leave the browser echoing
	// a slug the server had already replaced — a transient 500 turned into
	// 400s until reload. A failed write here is logged and the turn goes on
	// with the stored model; the manager's own guard then decides.
	if err := s.reconcileLockdownModelCtx(turnCtx, user, conv); err != nil {
		log.Printf("lockdown: conversation %s: model migration write failed, running with the stored model: %v", logSafe(conv.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the id and the error text.
	}

	// Prime the buffer with the metadata events so a late reattach
	// still sees conversation identity + turn id in its replay. The
	// `user.message` event is replay-only — reattach reconstructs the
	// user bubble from it so the chat doesn't appear as just a stranded
	// "Thinking…" indicator with no question above it.
	buf.Emit("conversation", map[string]any{
		"id":      conv.ID,
		"title":   conv.Title,
		"persona": conv.Persona,
		"model":   conv.Model,
	})
	turnStarted := map[string]any{
		"turn_id": turnID,
		"persona": conv.Persona,
	}
	if queueRowID != "" {
		// Queue-drained turns carry their input id so clients correlate the
		// chip that just left the queue with the turn that runs it.
		turnStarted["input_id"] = queueRowID
		turnStarted["queued"] = true
	}
	buf.Emit("turn.started", turnStarted)
	// `text` is what the USER typed; `injected_context` is the server-derived
	// suffix, carried as its own field so a client renders it outside the user
	// bubble (or not at all) instead of as words the user wrote. Reload agrees
	// with the live stream: the conversation GET splits the same two halves.
	// Omitted when empty so the common turn's frame does not grow.
	userEvent := map[string]any{"text": req.Message}
	if injected != "" {
		userEvent["injected_context"] = injected
	}
	buf.Emit("user.message", userEvent)
	if queueRowID != "" {
		// Same reason as the metadata events above: seed the queue snapshot so
		// a client that attaches to THIS turn learns the queue's current shape
		// from the replay. It cannot have learned it from the previous turn —
		// that buffer was already sealed when this drain was kicked, so the
		// settle-time queue.updated had no subscribers — and the row this turn
		// is running has since moved queued -> running, which is the
		// difference between a chip offering send-now/remove and an inert one.
		qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.emitQueueUpdate(qctx, user, conv.ID)
		qcancel()
	}

	// Run the turn in a goroutine so the buffer stays alive even if
	// this HTTP response disconnects. turnCtx is intentionally NOT
	// derived from r.Context(): the turn must outlive the HTTP request.
	//
	// Track the goroutine on activeTurns BEFORE launching it so graceful
	// shutdown (DrainTurns) can block on it and Wait never races ahead of Add
	// (#278). The counter mirrors the WaitGroup for the SIGUSR1 status log.
	s.activeTurns.Add(1)
	s.activeTurnCount.Add(1)
	// releaseSlot must run exactly once, and BEFORE the tail drain — the
	// completing turn's own slot is what the next drained turn needs, and a
	// deferred-only release would deterministically starve the queue at cap.
	var releasedOnce sync.Once
	releaseOnce := func() { releasedOnce.Do(releaseSlot) }
	go func() { //nolint:gosec // G118: deliberate — the turn must outlive the HTTP request (see the detachment comment above); Stop routes through /cancel.
		defer func() {
			s.activeTurnCount.Add(-1)
			s.activeTurns.Done()
		}()
		defer releaseOnce()
		s.runTurnAsync(turnCtx, turnCancel, buf, turnToken, conv, user, req.Message, injected, history, append(memoryContents(memories), projectMemoryBullets...), projectInstructions, toAgentImageAttachments(imageAttachments), steer)
		// The turn (and its deferred finishTurn) is done: settle this turn's
		// queue rows against the durable #798 record — the drained row
		// completes only if its user entry committed (a pre-commit failure
		// re-queues it; a 202-acknowledged input is never silently lost),
		// and uncommitted injected steers return to the queue unless a tool
		// dispatched after their injection (#823: the model may have acted on
		// the steer; re-running it could duplicate side effects, so those
		// rows cancel instead).
		requeued, cancelledSteers, serr := s.settleTurnInputs(buf, turnID, queueRowID)
		if directInputID != "" {
			s.settleDirectInput(directInputID, turnID)
		}
		if serr != nil {
			log.Printf("settle turn inputs (turn=%s): %v", turnID, serr)
		}
		if cancelledSteers > 0 {
			log.Printf("input queue: cancelled %d injected steer(s) of failed turn %s — tools dispatched after injection, re-running could duplicate side effects (#823)", cancelledSteers, turnID)
		}
		s.emitQueueUpdate(context.Background(), user, conv.ID)
		releaseOnce()
		s.maybeDrainQueue(conv.ID)
		if requeued > 0 {
			// The re-queued row may be the FIFO head the drain just re-claimed
			// into the same failure; the bounded re-kick breaks livelock.
			s.rekickDrainAfter(conv.ID, 3*time.Second)
		}
	}()

	if w != nil && r != nil {
		// Name the conversation on the response headers, before Attach writes
		// them (#1591). A brand-new chat's POST is the one request whose caller
		// does not yet know which conversation it is talking about, and the
		// `conversation` frame below can be lost with the socket; the header
		// arrives with the response itself, so a stream that dies immediately
		// still leaves the browser holding an id the recovery chain can probe.
		w.Header().Set(conversationIDHeaderName, conv.ID)
		w.Header().Set(turnIDHeaderName, turnID)
		// Attach this HTTP response as the initial subscriber. Blocks until
		// the turn finishes or the client disconnects. The client's declared SSE
		// capabilities (#194) filter which event types it receives; absent header =
		// full stream.
		caps := parseClientCapabilities(r.Header.Get(clientCapabilitiesHeaderName))
		if err := buf.Attach(r.Context(), 0, w, caps); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Attach (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
		}
	}
	return true
}

// seedConversationMCP persists a brand-new conversation's pre-chat Tools
// picker state: the optional-server opt-in list and the per-connector seat
// overrides (#988). The opt-in list is intersected with the whitelist (bundle
// catalog + the caller's remote servers — the picker lists both as
// toggleable) so a bad frontend can't persist garbage (mirrors POST
// /conversations/{id}/mcp-servers); a seat the user does not hold is a 400
// rather than a silent drop. Returns false after writing an error response.
func (s *Server) seedConversationMCP(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, enabledOptional []string, mcpAccounts map[string]string) bool {
	if len(enabledOptional) > 0 {
		valid := s.optionalServerWhitelist(r.Context(), user)
		seen := make(map[string]bool, len(enabledOptional))
		clean := make([]string, 0, len(enabledOptional))
		for _, n := range enabledOptional {
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
				return false
			}
			conv.OptionalMCPServersEnabled = clean
		}
	}
	if len(mcpAccounts) > 0 {
		accounts, verr := s.cleanMCPAccounts(r.Context(), user, mcpAccounts)
		if verr != nil {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return false
		}
		if len(accounts) > 0 {
			if err := s.store.SetConversationMCPAccounts(r.Context(), user, conv.ID, accounts); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return false
			}
			conv.MCPAccounts = accounts
		}
	}
	return true
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
	// userInput is what the user typed; injectedContext is this turn's
	// server-derived suffix (attachment manifest, workspace inventory, shared
	// library, expanded handles, skill note, connector hints). They travel
	// separately all the way into TurnInput: the run loop joins them for the
	// provider call and persists them in separate columns (ADR-0058).
	user, userInput, injectedContext string,
	history []agent.HistoryEntry,
	memories []string,
	projectInstructions string,
	imageAttachments []agent.ImageAttachment,
	steer *steerMailbox,
) {
	// Turn-start timestamp, stamped on every tool-call audit row derived from
	// this turn (the SDK does not propagate per-call timing, so the turn start is
	// the available anchor — see deriveToolCallEntries).
	startedAtUnix := time.Now().Unix()
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
		if err := runMockTurn(turnCtx, s.store, conv, buf.turnID, userInput, buf); err != nil {
			log.Printf("runMockTurn error (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
		}
		sweepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.sweepRetention(sweepCtx)
		return
	}

	// Availability layer (unified connector UX): drop opted-in servers the
	// user has since disabled on the connections page, and carry their default
	// credential-account seats into the turn.
	optionalEnabled, accountDefaults := s.applyConnectorPrefs(turnCtx, user, conv.OptionalMCPServersEnabled, conv.MCPAccounts)

	// User-authored skills (docs/SKILLS.md phase 2): sync the caller's active
	// skills into this conversation's workspace and hand the roster to the
	// prompt builder. Best-effort — a failure runs the turn without them.
	userSkills := s.materializeUserSkills(turnCtx, user, conv.ID)

	// Durable turn journal + gated terminal commit (#798). The journal writer
	// runs inside the agent loop (tool intent before dispatch, governed result
	// before the next provider step); the commit pair brackets the turn: the
	// user entry commits before the first provider call, and the terminal
	// projection commits before turn.completed / turn.cancelled is advertised.
	journal := newTurnJournalWriter(s.store, buf.turnID)
	commits := &turnCommits{store: s.store, convID: conv.ID, turnID: buf.turnID, journal: journal}

	res, err := s.agent.RunTurn(turnCtx, TurnInput{
		UserMessage:               userInput,
		InjectedContext:           injectedContext,
		Persona:                   conv.Persona,
		Model:                     conv.Model,
		History:                   history,
		Memories:                  memories,
		ProjectInstructions:       projectInstructions,
		ConversationID:            conv.ID,
		UserEmail:                 user,
		OptionalMCPServersEnabled: optionalEnabled,
		MCPAccountDefaults:        accountDefaults,
		UserSkills:                userSkills,
		SkillProposer:             &skillProposer{ctx: turnCtx, store: s.store, user: user},
		Lockdown:                  conv.Lockdown,
		ImageAttachments:          imageAttachments,
		UploadsRoot:               userUploadsRoot(s.cfg.EmailAttachmentDir, user),
		ThinkingConfig:            resolveThinkingConfig(conv.ThinkingConfig, s.cfg.DefaultThinkingBudgetTokens),
		ApprovalStager: &approvalStager{
			ctx:             turnCtx,
			store:           s.store,
			conversationID:  conv.ID,
			userEmail:       user,
			sink:            buf,
			mcpBroker:       s.agent.MCPBroker(),
			mcpCatalog:      s.agent.MCPCatalog(),
			sessionRegistry: s.sessionApprovals,
			// Live (admin Features panel > env, #225): read per turn at stager
			// construction so an edit governs the next staged card, no restart.
			globalTimeoutSeconds: s.cfg.LiveApprovalTimeoutSeconds(),
			convTimeoutSeconds:   conv.ApprovalTimeoutSeconds,
			autoApproveInTest:    s.cfg.AutoApproveInTest,
			push:                 s.push,
			bg:                   &s.background,
			taskConnectors:       s.chatTaskConnectorsFor(user, conv.ID),
		},
		MemoryProposer: &memoryProposer{
			ctx:            turnCtx,
			store:          s.store,
			conversationID: conv.ID,
			userEmail:      user,
			sink:           buf,
			origin:         "tool",
		},
		TurnJournal:    journal,
		CommitUser:     commits.commitUser,
		CommitTerminal: commits.commitTerminal,
		SteerSource:    steerSourceOrNil(steer),
	}, buf)
	if err != nil {
		log.Printf("RunTurn error (user=%s conv=%s): %s", logSafe(user), logSafe(conv.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from email, conv id, and the error text.
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

	// Canonical history was committed INSIDE RunTurn (#798): the user entry
	// before the first provider call, the rest transactionally before the
	// terminal event was advertised. Here we only tell the live stream which
	// DB rows this turn's messages became, so the client can backfill
	// Message.dbId without a reload — the Branch button (#454) only renders
	// for persisted messages. Emitted after turn.completed and before
	// finishTurn seals the buffer, so live AND replayed streams carry it.
	if userID, ids := commits.persisted(); userID > 0 {
		buf.Emit("history.persisted", map[string]any{
			"entries": historyPersistedEntries(res.NewHistory, append([]int64{userID}, ids...)),
		})
	}

	// Tool-call audit ledger (#224): derive one row per tool call from the same
	// accumulated history we just persisted, so the choke point is the existing
	// event flow rather than a new instrumentation point in the hot loop. Args
	// are redacted before insertion (see deriveToolCallEntries). Best-effort: a
	// failure is logged but never fails the turn — the ledger is observability,
	// not a turn-blocking dependency.
	if entries := deriveToolCallEntries(res.NewHistory, conv.ID, buf.turnID, user, startedAtUnix); len(entries) > 0 {
		if err := s.store.RecordToolCalls(persistCtx, entries); err != nil {
			log.Printf("RecordToolCalls (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
		}
	}

	// Record metrics so the admin dashboard can aggregate cost per user.
	// A failed/errored turn doesn't reach this code path (we returned early
	// above); cancelled turns DO, and are flagged for separate accounting.
	if err := s.store.RecordTurn(persistCtx, store.TurnMetric{
		ConversationID:      conv.ID,
		UserEmail:           user,
		CompletedAt:         time.Now().Unix(),
		CostUSD:             res.CostUSD,
		PromptTokens:        res.PromptTokens,
		CompletionTokens:    res.CompletionTokens,
		CachedTokens:        res.CachedTokens,
		CacheCreationTokens: res.CacheCreationTokens,
		Cancelled:           res.Cancelled,
	}); err != nil {
		log.Printf("RecordTurn: %v", err)
	}
	// Operational metrics (#176): cost + tokens by model, and a turn-timeout
	// counter when the turn ended because its wall-clock deadline fired (as
	// opposed to a user Stop, which cancels turnCtx without a deadline error).
	metrics.RecordTurnUsage(res.Model, res.CostUSD, res.PromptTokens, res.CompletionTokens, res.CachedTokens)
	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		metrics.RecordTurnTimeout("interactive")
	}

	// First-turn auto-title: on the opening turn, summarize the exchange
	// into a 5-7 word sidebar title. Emits via the buffer so both the
	// initial client and any reattach see it.
	if s.cfg.LiveAutoTitle() && len(history) == 0 && !res.Cancelled && strings.TrimSpace(res.FinalText) != "" {
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
			// UpdateTitle is locked-guarded (#302): if the user manually renamed
			// the conversation mid-turn it returns ErrTitleLocked — a benign skip,
			// not an error, and we must NOT emit the overwrite to the sidebar.
			switch err := s.store.UpdateTitle(persistCtx, user, conv.ID, title); {
			case err == nil:
				buf.Emit("conversation.title_updated", map[string]any{
					"id":    conv.ID,
					"title": title,
				})
			case errors.Is(err, store.ErrTitleLocked):
				// user renamed it; leave their name in place.
			default:
				log.Printf("auto-title UpdateTitle failed: %v", err)
			}
		}
	}

	// Auto-archive (#282): file away unpinned conversations untouched for
	// FLEET_AUTO_ARCHIVE_AFTER_DAYS. Runs before the sweep so freshly archived
	// rows are exempt from the cap eviction on the same pass. Disabled (no-op)
	// unless the operator opts in (default 0). Opportunistic, like the sweep.
	if s.cfg.AutoArchiveAfterDays > 0 {
		if n, err := s.store.AutoArchiveOlderThan(persistCtx,
			time.Duration(s.cfg.AutoArchiveAfterDays)*24*time.Hour); err != nil {
			log.Printf("post-turn auto-archive error: %v", err)
		} else if n > 0 {
			log.Printf("auto-archive: %d conversations archived", n)
		}
	}

	// Reclaim expired conversations, terminal input-queue rows, aged-out turn
	// ledgers, attachment files and orphaned workspace dirs. Rate-gated (see
	// maintenance.go): this is the prompt-cleanup optimization, and cmd/fleet's
	// maintenance ticker is the guarantee that it happens on an idle box too.
	// Pending/running/injected queue work and running turns are never
	// retention-eligible.
	s.runPostTurnMaintenance(persistCtx)

	// Conversation memory auto-indexing (#234): when enabled, mine the completed
	// turn for durable facts and surface each NEW one as a memory PROPOSAL — the
	// SAME seam the propose_memory tool uses, so nothing is written live without
	// the user's Save. Runs LAST so the drain-critical sweeps above never wait on
	// its LLM call, and its context is derived from turnCtx (not Background) so a
	// shutdown/Stop force-cancel propagates into the in-flight extraction rather
	// than pinning the drain. Best-effort: any error is swallowed. Skips
	// cancelled/empty turns. Off by default (opt-in).
	if s.cfg.LiveMemoryAutoIndexEnabled() && !res.Cancelled && strings.TrimSpace(res.FinalText) != "" {
		memCtx, memCancel := context.WithTimeout(turnCtx, 30*time.Second)
		s.autoIndexMemories(memCtx, buf, conv.ID, user, userInput, res.FinalText)
		memCancel()
	}
}

// sweepRetention runs the post-turn database retention sweeps — expired
// conversations, terminal input-queue rows, and finished turns' durable SSE
// ledgers (turns + turn_events + turn_journal, which otherwise outlive their
// usefulness as reattach/recovery state and grow without bound in long-lived
// conversations). Shared by the real and mock turn paths so retention can
// never drift between them. Best-effort: each sweep logs and moves on, and
// the store treats a non-positive TTL as "disabled".
func (s *Server) sweepRetention(ctx context.Context) {
	if expired, evicted, err := s.store.SweepExpired(ctx,
		time.Duration(s.cfg.ConversationTTL)*24*time.Hour, s.cfg.UnpinnedCap); err != nil {
		log.Printf("post-turn sweep error: %v", err)
	} else if expired > 0 || evicted > 0 {
		log.Printf("sweep: %d expired, %d evicted", expired, evicted)
	}
	if purged, err := s.store.PurgeTerminalInputs(ctx,
		time.Duration(s.cfg.InputQueueRetentionDays)*24*time.Hour); err != nil {
		log.Printf("post-turn input-queue purge error: %v", err)
	} else if purged > 0 {
		log.Printf("input-queue purge: %d terminal row(s) removed", purged)
	}
	if swept, err := s.store.SweepTurnEvents(ctx,
		time.Duration(s.cfg.TurnEventRetentionDays)*24*time.Hour); err != nil {
		log.Printf("post-turn turn-ledger sweep error: %v", err)
	} else if swept > 0 {
		log.Printf("turn-ledger sweep: %d aged-out turn(s) removed", swept)
	}
}
