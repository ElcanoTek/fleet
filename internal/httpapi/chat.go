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
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

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
	// Mode selects what a submission does when a turn is already running
	// (#785): "queue" (default) runs it as the next turn; "steer" offers it
	// to the running turn's next step boundary, falling back to queue if the
	// turn ends first. Ignored when the conversation is idle.
	Mode string `json:"mode,omitempty"`
}

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
		http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
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

	// Idempotent replay (#785): an input_id that was already ACCEPTED (queued,
	// running, or terminal) never runs a duplicate turn — even when the retry
	// lands after the conversation went idle.
	if clientID := strings.TrimSpace(req.InputID); clientID != "" {
		existing, lerr := s.store.LookupInput(r.Context(), conv.ID, clientID)
		if lerr != nil {
			// Fail closed: proceeding without the lookup could run (and bill) a
			// duplicate turn for an input_id that was already accepted. A 500
			// lets the client retry the same input_id safely.
			http.Error(w, "input lookup failed: "+lerr.Error(), http.StatusInternalServerError)
			return
		}
		if existing != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"queued": true,
				"input": map[string]any{
					"id": existing.ID, "client_input_id": existing.ClientInputID,
					"mode": existing.Mode, "state": existing.State, "position": existing.Position,
				},
				"conversation_id": conv.ID,
			})
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

	if !s.startTurn(w, r, user, conv, req, "", releaseSlot) {
		// A concurrent submission won the registerTurn race between our busy
		// check and now; the input must not be lost — queue it instead.
		releaseSlot()
		s.handleBusySubmit(w, r, user, conv, req)
	}
}

// startTurn runs one accepted input as a full turn: history + context prep,
// buffer registration, prompt assembly, and the detached run goroutine. It is
// the shared launch path for direct submissions (w/r set; the response
// attaches to the buffer) and queue-drained inputs (#785; w/r nil, queueRowID
// set). Returns false ONLY when registerTurn refused because a turn is
// already running — every other failure is handled (responded/logged)
// internally. releaseSlot is released by the turn goroutine on completion;
// on false the caller releases it.
func (s *Server) startTurn(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, req chatRequest, queueRowID string, releaseSlot func()) bool {
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
			http.Error(w, err.Error(), status)
			return
		}
		log.Printf("queued turn launch (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
		s.terminalizeQueueRow(queueRowID, store.InputStateQueued)
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
	buf, turnID, turnToken, ok := s.registerTurn(conv.ID, turnCancel, steer)
	if !ok {
		turnCancel()
		return false
	}
	// Bind the mailbox to its turn identity now that registerTurn minted it.
	// The run goroutine (the only Poll consumer) has not launched yet, so no
	// injection can precede the binding.
	steer.turnID, steer.buf = turnID, buf
	if queueRowID != "" {
		// Stamp the REAL turn id on the drained row (the claim used a
		// placeholder): the settle/recovery predicates check THIS turn's
		// durable #798 record, and a stale placeholder would re-queue —
		// double-run — an already-committed input after a crash.
		bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.store.BindInputTurn(bctx, queueRowID, turnID); err != nil {
			log.Printf("bind input turn (input=%s turn=%s): %v", queueRowID, turnID, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
		}
		bcancel()
	}

	// Wire incremental persistence so a crash mid-turn leaves a
	// recoverable ledger in turn_events. Non-fatal — if the DB is
	// flaky, live streaming still works; crash recovery just won't.
	persistCtx, persistCancelAttach := context.WithTimeout(reqCtx, 5*time.Second)
	if err := buf.attachPersister(persistCtx, s.store); err != nil {
		log.Printf("attachPersister (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
	}
	persistCancelAttach()

	// Attachment metadata (if any) is re-validated server-side, then split
	// by kind. Images flow into the model as multimodal vision input via
	// TurnInput.ImageAttachments; other files keep the legacy markdown
	// reference path so view_file etc. can still reach them. Both kinds
	// are mentioned in the appended block so the agent sees what arrived.
	validAttachments := s.validateAttachments(req.Attachments)
	imageAttachments, otherAttachments := splitAttachmentsByKind(validAttachments)
	// Kubernetes backend only: copy non-image attachments into the
	// conversation workspace (the claim every sandbox pod mounts) and
	// advertise those paths — the uploads root is control-plane state no pod
	// can see. Images are exempt: their bytes reach the model host-side as
	// vision input on both backends. Podman keeps its zero-copy read-only
	// mount of the uploads root.
	stagedAttachments := s.attachmentsNeedWorkspaceStaging()
	if stagedAttachments {
		otherAttachments = stageAttachmentsIntoWorkspace(conv.ID, otherAttachments)
	}
	userMessage := appendAttachmentsBlock(req.Message, imageAttachments, otherAttachments, stagedAttachments)
	// Surface files persisted from earlier turns. The agent's run_python
	// kernel resets each turn but its workspace dir doesn't — without this,
	// a report downloaded on turn 1 gets forgotten by turn 4 even though
	// it's still on disk. Empty workspaces (first turn) skip the block.
	userMessage = appendWorkspaceInventoryBlock(userMessage, tools.WorkspaceDirForConversation(conv.ID))
	// Announce the cross-chat shared file library (docs/SHARED-FILES.md) the
	// same way: read-only paths under shared/ the agent can use immediately.
	userMessage = s.appendSharedFilesBlock(turnCtx, userMessage)
	// Composer context handles (#517, opt-in): expand any `@url:<url>` /
	// `@file:"path"` in the user's message into the turn context. A no-op when
	// disabled; failures degrade to notices so the turn always proceeds.
	userMessage = s.applyContextHandles(turnCtx, userMessage, req.Message, conv.ID)
	// Explicit skill invocation (#513 phase 1): a message whose first line starts
	// with "/<skill-name>" (exact match against the bundle roster) gets a block
	// appended telling the agent to read that skill's SKILL.md now. Because the
	// block lands on the persisted user message, the transcript records which
	// skill was invoked. Unknown "/tokens" are ignored — no block, no error.
	userMessage = s.applySkillInvocation(turnCtx, user, userMessage, req.Message)
	// Connector auto-recommendation (#512, opt-in): if the message is relevant to
	// an Optional connector the user hasn't enabled, note it so the agent can
	// suggest connecting it via /settings/connections (never auto-connecting).
	userMessage = s.applyConnectorRecommendations(userMessage, req.Message, req.EnabledOptional)

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
	buf.Emit("user.message", map[string]any{
		"text": userMessage,
	})

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
		s.runTurnAsync(turnCtx, turnCancel, buf, turnToken, conv, user, req.Message, userMessage, history, append(memoryContents(memories), projectMemoryBullets...), projectInstructions, toAgentImageAttachments(imageAttachments), steer)
		// The turn (and its deferred finishTurn) is done: settle this turn's
		// queue rows against the durable #798 record — the drained row
		// completes only if its user entry committed (a pre-commit failure
		// re-queues it; a 202-acknowledged input is never silently lost),
		// and uncommitted injected steers return to the queue unless a tool
		// dispatched after their injection (#823: the model may have acted on
		// the steer; re-running it could duplicate side effects, so those
		// rows cancel instead).
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		requeued, cancelledSteers, serr := s.store.SettleTurnInputs(sctx, turnID, queueRowID)
		scancel()
		if serr != nil {
			log.Printf("settle turn inputs (turn=%s): %v", turnID, serr)
		}
		if cancelledSteers > 0 {
			log.Printf("input queue: cancelled %d injected steer(s) of failed turn %s — tools dispatched after injection, re-running could duplicate side effects (#823)", cancelledSteers, turnID) //nolint:gosec // G706: int count + server-generated turn UUID — no request-authored text is logged.
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
	user, userInput, userMessage string,
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
		if err := runMockTurn(turnCtx, s.store, conv, userInput, buf); err != nil {
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
		UserMessage:               userMessage,
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
		log.Printf("RunTurn error (user=%s conv=%s): %v", user, conv.ID, err) //nolint:gosec // G706: authenticated caller email + server-generated conv id + internal error — no request-authored text.
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
