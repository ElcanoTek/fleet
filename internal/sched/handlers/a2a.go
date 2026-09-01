// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// The A2A (Agent2Agent) protocol server (#1279): the Agent Card discovery
// document and the JSON-RPC 2.0 + SSE binding, spec release pinned in
// internal/a2a (v1.0.1).
//
// This is an I/O ADAPTER over the task seams this package already guards —
// SendMessage is createTaskGoverned (the same pipeline as POST /tasks),
// GetTask/ListTasks are the ADR-0043-scoped reads, CancelTask is
// CancelTaskAtomic + the live-run stopper, streaming polls the task row (the
// DB is the source of truth; see docs/A2A.md for why the in-memory run-log
// buffer is not merged in). It never runs a model and never touches
// agentcore, so the one-governed-loop conformance map is unchanged.
//
// Auth is per JSON-RPC method, in-handler: the route multiplexes reads and
// writes over POST, so the HTTP-verb-based key gating (TypeAllowsMethod) that
// guards the REST routes cannot apply — a fleet_readonly_ key must be able to
// GetTask through a POST. Only the credential the Agent Card declares is
// accepted (X-API-Key: the bootstrap admin key or a typed/legacy key); there
// is deliberately no cookie or bearer path, which is what makes the /a2a CSRF
// exemption in middleware.go sound. Credential failures are HTTP 401/403
// (auth is transport-layer in A2A); everything after auth is a JSON-RPC
// envelope, including the spec-mandated capability errors for features the
// card declares off (-32003 push, -32004 extended card).
//
// Which persona/model an A2A task runs with is OPERATOR policy
// (FLEET_A2A_PERSONA / FLEET_A2A_MODEL), the same posture as webhook
// triggers: callers send messages, not configuration.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A2AConfig is everything the A2A surface needs, assembled in cmd/fleet where
// the config and branding bundle live.
type A2AConfig struct {
	// CardJSON / CardETag are the pre-rendered Agent Card (a2abridge.MarshalCard):
	// the card is static per boot, so it is marshaled once.
	CardJSON []byte
	CardETag string
	// Persona / Model pin what every A2A-created task runs with (operator
	// policy; empty = the deployment's defaults, like any other task).
	Persona string
	Model   string
	// PublicBaseURL prefixes artifact file URLs; empty yields server-relative.
	PublicBaseURL string
	// PushEnabled reports whether per-task push-notification configs can be
	// stored (#1279 Phase 2): true only when the store cipher is configured
	// (FLEET_MCP_OAUTH_ENCRYPTION_KEY), because the caller-supplied webhook
	// secrets are sealed at rest and plaintext storage is not an option. The
	// Agent Card's capabilities.pushNotifications mirrors this, so per spec
	// §3.3.4 the push methods answer -32003 whenever it is false.
	PushEnabled bool
	// ExtendedCardJSON is the pre-rendered authenticated card (#1279 Phase 2),
	// marshaled by a2abridge.MarshalCard so the securityRequirements shadow
	// applies — handing the raw wire.AgentCard to the envelope would resurrect
	// the SDK's schema-invalid scopes shape. Served as a raw JSON result by
	// GetExtendedAgentCard, only ever after authentication (spec §13.3 MUST).
	ExtendedCardJSON []byte
	// UnaryWaitBudget bounds how long a blocking unary SendMessage waits for
	// the task outcome before answering with the freshest snapshot.
	// FLEET_A2A_UNARY_WAIT_SECONDS; zero falls back to a2aUnaryWaitBudget.
	UnaryWaitBudget time.Duration
}

// SetA2A wires the A2A protocol server (#1279). nil (never called) leaves the
// routes answering 501. Mirrors SetTaskStreamProvider: set before serving.
func (h *Handlers) SetA2A(cfg *A2AConfig) {
	h.a2a = cfg
}

// a2aStreamPollInterval is how often a streaming method re-reads the task row.
// Task-level status transitions are seconds-to-minutes apart, so 1s keeps the
// wire honest without hammering Postgres; the row is the source of truth, so
// nothing is ever lost to buffer eviction (docs/A2A.md).
const a2aStreamPollInterval = time.Second

// a2aStreamHeartbeat matches the run-log stream's keep-alive cadence.
const a2aStreamHeartbeat = 15 * time.Second

// a2aStreamMaxLifetime bounds one SSE stream. Unlike the REST run-log stream
// (which attaches to an in-memory buffer), every A2A stream is a persistent
// once-per-second read against Postgres, and a task parked in
// paused_awaiting_wake maps to WORKING — non-terminal — so a subscription to
// a self-waking task would legitimately poll for days. Closing here is
// lossless by design: the row is the source of truth, so a client that still
// cares reconnects with SubscribeToTask and misses nothing (a close without a
// terminal statusUpdate is indistinguishable from any dropped connection,
// which every A2A client already has to handle).
const a2aStreamMaxLifetime = 30 * time.Minute

// a2aMaxConcurrentStreams caps concurrently-held A2A SSE connections
// process-wide. The per-minute rate limiter only gates stream INITIATION, so
// without a ceiling one credential could accumulate hundreds of held
// connections — each a standing DB poller — over an hour.
const a2aMaxConcurrentStreams = 64

// a2aUnaryWaitBudget bounds a blocking unary SendMessage (returnImmediately
// false — the spec default — MUST wait for the outcome). Long enough for the
// task queue plus a real agent run; when it ends the freshest snapshot is
// returned instead, which is the same shape a returnImmediately caller
// accepts, and docs/A2A.md records the bound honestly.
const a2aUnaryWaitBudget = 30 * time.Minute

func a2aDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"error":"a2a_disabled","detail":"the A2A protocol server is not enabled on this deployment (set FLEET_A2A_ENABLED=1 and restart)"}`))
}

// A2AAgentCard handles GET /.well-known/agent-card.json — the public
// discovery document. Public by construction: it names capabilities and the
// endpoint URL, carries no secrets and no per-deployment identity beyond what
// the operator put in the branding bundle. ETag + Cache-Control per spec §8.6.
func (h *Handlers) A2AAgentCard(w http.ResponseWriter, r *http.Request) {
	if h.a2a == nil {
		a2aDisabled(w)
		return
	}
	w.Header().Set("ETag", h.a2a.CardETag)
	w.Header().Set("Cache-Control", "max-age=300")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, h.a2a.CardETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.a2a.CardJSON)
}

// A2ARPC handles POST /a2a: every A2A JSON-RPC method multiplexes over this
// one route; the streaming methods respond with SSE.
func (h *Handlers) A2ARPC(w http.ResponseWriter, r *http.Request) {
	if h.a2a == nil {
		a2aDisabled(w)
		return
	}

	var req a2abridge.Request
	if err := readJSON(r, &req); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(nil, wire.ErrParseError, "invalid JSON payload", nil))
		return
	}
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidRequest, "expected a JSON-RPC 2.0 request with a method", nil))
		return
	}

	// A2A-Version, spec-literally (§3.6.2): absent means 0.3, and this server
	// implements 1.0 only. Checked before dispatch so every method agrees.
	if err := a2abridge.CheckVersion(r.Header.Get("A2A-Version")); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrVersionNotSupported, err.Error(), nil))
		return
	}

	// Authenticate before dispatch: every method requires a credential (the
	// card's securityRequirements has no optional entry). Transport-layer
	// (HTTP) statuses, not JSON-RPC errors, per the binding's auth model.
	p, ok := h.a2aPrincipal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized: send a fleet API key in X-API-Key (see /.well-known/agent-card.json securitySchemes)")
		return
	}

	switch req.Method {
	case a2abridge.MethodSendMessage:
		h.a2aSendMessage(w, r, p, req, false)
	case a2abridge.MethodSendStreamingMessage:
		h.a2aSendMessage(w, r, p, req, true)
	case a2abridge.MethodGetTask:
		h.a2aGetTask(w, r, p, req)
	case a2abridge.MethodListTasks:
		h.a2aListTasks(w, r, p, req)
	case a2abridge.MethodCancelTask:
		h.a2aCancelTask(w, r, p, req)
	case a2abridge.MethodSubscribeToTask:
		h.a2aSubscribeToTask(w, r, p, req)
	case a2abridge.MethodCreatePushConfig:
		h.a2aCreatePushConfig(w, r, p, req)
	case a2abridge.MethodGetPushConfig:
		h.a2aGetPushConfig(w, r, p, req)
	case a2abridge.MethodListPushConfigs:
		h.a2aListPushConfigs(w, r, p, req)
	case a2abridge.MethodDeletePushConfig:
		h.a2aDeletePushConfig(w, r, p, req)
	case a2abridge.MethodGetExtendedAgentCard:
		// Auth already happened above (a2aPrincipal 401s before dispatch),
		// which is the spec §13.3 MUST; the two error branches below follow
		// §3.1.11's declared-but-unconfigured taxonomy.
		if len(h.a2a.ExtendedCardJSON) == 0 {
			a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrExtendedCardNotConfigured,
				"this server declares an extended agent card but none is configured", nil))
			return
		}
		a2aWrite(w, a2abridge.NewResponse(req.ID, json.RawMessage(h.a2a.ExtendedCardJSON)))
	default:
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrMethodNotFound,
			fmt.Sprintf("unknown A2A method %q", req.Method), nil))
	}
}

// a2aWrite emits a single (non-streaming) JSON-RPC response. JSON-RPC-layer
// failures still ride HTTP 200 — the envelope's error member is the signal.
func a2aWrite(w http.ResponseWriter, resp a2abridge.Response) {
	writeJSON(w, http.StatusOK, resp)
}

// a2aPrincipal authenticates the JSON-RPC caller: the bootstrap admin key or
// a typed/legacy API key in X-API-Key — exactly the credential the Agent Card
// declares, and nothing else. No cookie and no bearer path, so the /a2a CSRF
// exemption cannot be leveraged by a browser-riding credential. ValidateKey
// consumes one token of the key's hourly cap, matching the REST create path.
func (h *Handlers) a2aPrincipal(r *http.Request) (principal, bool) {
	if h.verifyAdminKey(r) {
		return principal{isAdmin: true}, true
	}
	if apiKey := r.Header.Get("X-API-Key"); apiKey != "" && h.apiKeys != nil {
		if valid, key, _ := h.apiKeys.ValidateKey(apiKey, nil, nil, nil); valid && key != nil {
			return principal{apiKey: key}, true
		}
	}
	return principal{}, false
}

// a2aCreatorFromPrincipal projects an authenticated A2A principal onto the
// create pipeline's taskCreator, preserving spend attribution and the per-key
// priority ceiling exactly as authorizeTaskCreator would.
func a2aCreatorFromPrincipal(p principal) taskCreator {
	creator := taskCreator{hasAdminPermission: p.hasPermission(models.PermissionAdmin)}
	if p.apiKey != nil {
		keyID := p.apiKey.KeyID
		creator.creatorKey = &keyID
		if p.apiKey.MaxPriority != nil {
			capVal := *p.apiKey.MaxPriority
			creator.creatorKeyMaxPriority = &capVal
		}
	}
	return creator
}

// a2aVisibleTask loads a task and applies ADR-0043 row visibility. Spec
// §3.3.2: not-found and not-authorized MUST NOT be distinguishable, so both
// (and a malformed id, which by construction names no task) come back as the
// same TaskNotFound.
func (h *Handlers) a2aVisibleTask(p principal, rawID wire.TaskID) (*models.Task, error) {
	notFound := fmt.Errorf("%w: no task %q is visible to this credential", wire.ErrTaskNotFound, string(rawID))
	taskID, err := uuid.Parse(string(rawID))
	if err != nil {
		return nil, notFound
	}
	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		return nil, notFound
	}
	if !taskVisibleToPrincipal(p, task) {
		return nil, notFound
	}
	return task, nil
}

// a2aTitle derives the task's display title from the message text: first
// line, trimmed to the title budget.
func a2aTitle(prompt string) string {
	line := strings.TrimSpace(prompt)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	runes := []rune(line)
	if len(runes) > 100 {
		line = string(runes[:100]) + "…"
	}
	return line
}

// a2aPromptFromMessage extracts the task prompt from an inbound message: the
// text parts, joined. Any non-text part is refused — the card honestly
// declares defaultInputModes: ["text/plain"] and nothing else.
func a2aPromptFromMessage(msg *wire.Message) (string, error) {
	if msg == nil || len(msg.Parts) == 0 {
		return "", fmt.Errorf("%w: message with at least one part is required", wire.ErrInvalidParams)
	}
	var texts []string
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if _, isText := part.Content.(wire.Text); !isText {
			return "", fmt.Errorf("%w: only text parts are accepted (declared defaultInputModes: text/plain); file and data parts are not supported", wire.ErrUnsupportedContentType)
		}
		texts = append(texts, part.Text())
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if prompt == "" {
		return "", fmt.Errorf("%w: message text is empty", wire.ErrInvalidParams)
	}
	return prompt, nil
}

// a2aSendMessage implements SendMessage and SendStreamingMessage: a new
// message creates one governed task through the shared create pipeline; a
// message carrying taskId answers an INPUT_REQUIRED task through the resume
// seam. The unary result is the SendMessageResponse oneof ({"task": …});
// streaming hands off to the watcher.
func (h *Handlers) a2aSendMessage(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request, streaming bool) {
	var params wire.SendMessageRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a SendMessageRequest: "+err.Error(), nil))
		return
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	if params.Config != nil && params.Config.PushConfig != nil {
		// Inline registration rides the send (#1279 Phase 2) — validate the
		// shape up front so a bad webhook URL refuses the send before a task
		// exists, and refuse outright while the capability is off: a caller
		// who asked for notifications it cannot have must hear that, not
		// silently miss every callback.
		if !h.a2a.PushEnabled {
			a2aWrite(w, a2aPushRefusal(req.ID))
			return
		}
		if err := a2aValidatePushURL(params.Config.PushConfig.URL); err != nil {
			a2aWrite(w, a2aErrorFrom(req.ID, err))
			return
		}
	}
	prompt, err := a2aPromptFromMessage(params.Message)
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}

	// contextId rules (spec §3.4, CORE-MULTI-002a/005/006). Fleet's contexts
	// are 1:1 with tasks (contextId == taskId by construction), so an
	// arbitrary client-provided context cannot be honored — and §3.4.1 says a
	// context the agent cannot accept MUST be rejected, never silently
	// replaced with a generated one. On a follow-up, a contextId is accepted
	// exactly when it matches the task's own (mismatches MUST error, §3.4.3);
	// omitted, it is inferred from the task.
	if params.Message.TaskID == "" && params.Message.ContextID != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams,
			"this server generates contextId (one context per task) — omit contextId on a new message", nil))
		return
	}
	if params.Message.TaskID != "" && params.Message.ContextID != "" &&
		params.Message.ContextID != string(params.Message.TaskID) {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams,
			"contextId does not match the task's context (this server's contexts are 1:1 with tasks)", nil))
		return
	}

	// Follow-up on an existing task: the INPUT_REQUIRED round-trip. The text
	// answers the pending question through the same resume seam as
	// POST /tasks/{id}/resume.
	if params.Message.TaskID != "" {
		h.a2aAnswerTask(w, r, p, req, params.Message.TaskID, prompt, params.Config, streaming)
		return
	}

	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key type cannot create tasks")
		return
	}

	tc := models.TaskCreate{
		Prompt:  prompt,
		Title:   a2aTitle(prompt),
		Persona: h.a2a.Persona,
	}
	if h.a2a.Model != "" {
		model := h.a2a.Model
		tc.Model = &model
	}
	task, err := h.createTaskGoverned(r.Context(), a2aCreatorFromPrincipal(p), tc)
	if err != nil {
		a2aWrite(w, a2aCreateError(req.ID, err))
		return
	}
	log.Printf("Task created via A2A: %s (prompt: %.50s...)", task.ID, logSafe(task.Prompt))

	// Inline push registration binds to the task this send just created (its
	// URL was validated before the create, so a failure here is storage).
	if params.Config != nil && params.Config.PushConfig != nil {
		if err := h.a2aStorePushConfigInline(r, task.ID, params.Config.PushConfig); err != nil {
			a2aWrite(w, a2aPushStoreError(req.ID, err))
			return
		}
	}

	if streaming {
		h.a2aStreamTask(w, r, req.ID, task.ID)
		return
	}
	// Spec: a unary SendMessage with returnImmediately false — the DEFAULT —
	// MUST wait until the task reaches a terminal or interrupted state before
	// returning. A conformant non-streaming client treats this result as the
	// outcome, so answering with the just-created SUBMITTED row would hand it
	// a delegation that appears to have produced nothing.
	final := task
	if params.Config == nil || !params.Config.ReturnImmediately {
		final = h.a2aAwaitOutcome(r.Context(), task.ID, task)
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, wire.StreamResponse{Event: a2abridge.BuildTask(final, h.a2a.PublicBaseURL, true)}))
}

// a2aAwaitOutcome implements the unary SendMessage wait contract: poll the
// task row (the same event source the streams use) until the task reaches a
// terminal or interrupted (INPUT_REQUIRED / AUTH_REQUIRED) state, the caller
// disconnects, or the wait budget ends — and return the freshest snapshot
// either way, because the caller can only be told what is true now.
//
// Deliberately NOT counted against the concurrent-stream ceiling: each wait
// is tied to one just-created or just-resumed task, so it is already bounded
// by the create path's rate limit and the key's hourly cap, while
// subscriptions can be opened without limit against a single task.
func (h *Handlers) a2aAwaitOutcome(ctx context.Context, taskID uuid.UUID, latest *models.Task) *models.Task {
	settled := func(t *models.Task) bool {
		state, _ := a2abridge.TaskStateFor(t.Status)
		return state.Terminal() || state == wire.TaskStateInputRequired || state == wire.TaskStateAuthRequired
	}
	if settled(latest) {
		return latest
	}
	waitBudget := a2aUnaryWaitBudget
	if h.a2a.UnaryWaitBudget > 0 {
		waitBudget = h.a2a.UnaryWaitBudget
	}
	budget := time.NewTimer(waitBudget)
	defer budget.Stop()
	ticker := time.NewTicker(a2aStreamPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return latest
		case <-budget.C:
			return latest
		case <-ticker.C:
		}
		task, err := h.storage.GetTask(taskID)
		if err != nil || task == nil {
			// The row is the source of truth; if it cannot be read, the last
			// good snapshot is the most honest thing left to say.
			return latest
		}
		latest = task
		if settled(latest) {
			return latest
		}
	}
}

// a2aAnswerTask resumes a paused-awaiting-input task with the message text.
// Only the task's creator (or a cancel-scoped operator credential) may answer
// — answering steers the run, so the bar matches the other mutating surface.
func (h *Handlers) a2aAnswerTask(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request, rawID wire.TaskID, answer string, cfg *wire.SendMessageConfig, streaming bool) {
	task, err := h.a2aVisibleTask(p, rawID)
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}
	if !taskCreatedByPrincipal(p, task) && !p.hasPermission(models.PermissionCancelTask) {
		// Visible (fleet-wide read grant) but not the creator: reads only.
		writeError(w, http.StatusForbidden, "answering a task is limited to its creator or an operator credential")
		return
	}
	if task.Status != models.TaskStatusPausedAwaitingInput {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrUnsupportedOperation,
			fmt.Sprintf("task %s is not awaiting input (state %s): a follow-up message is only accepted while the task asks a question", task.ID, task.Status), nil))
		return
	}
	resumed, err := h.storage.ResumeTask(r.Context(), task.ID, answer)
	if err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInternalError, "failed to resume the task", nil))
		return
	}
	if !resumed {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrUnsupportedOperation, "the task is no longer awaiting input", nil))
		return
	}
	log.Printf("Task resumed via A2A: %s", task.ID)
	// An inline push config on a follow-up binds to the task being answered —
	// the way a caller subscribes to the outcome of the answer it just gave.
	if cfg != nil && cfg.PushConfig != nil {
		if err := a2aValidatePushURL(cfg.PushConfig.URL); err != nil {
			a2aWrite(w, a2aErrorFrom(req.ID, err))
			return
		}
		if err := h.a2aStorePushConfigInline(r, task.ID, cfg.PushConfig); err != nil {
			a2aWrite(w, a2aPushStoreError(req.ID, err))
			return
		}
	}
	if streaming {
		h.a2aStreamTask(w, r, req.ID, task.ID)
		return
	}
	updated, err := h.storage.GetTask(task.ID)
	if err != nil || updated == nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInternalError, "the task was resumed but could not be re-read", nil))
		return
	}
	// The same unary wait contract as a fresh send: the resumed task has to
	// reach its next outcome before a default-config caller is answered.
	if cfg == nil || !cfg.ReturnImmediately {
		updated = h.a2aAwaitOutcome(r.Context(), task.ID, updated)
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, wire.StreamResponse{Event: a2abridge.BuildTask(updated, h.a2a.PublicBaseURL, true)}))
}

// a2aCreateError maps the shared create pipeline's refusals onto the wire: a
// 400-class refusal is invalid params (the validation text passes through
// verbatim), the other typed refusals (run_if/priority 403s, the storage
// failure) and a budget refusal are the implementation-defined server error —
// all of those carry handler- or budget-authored text — with the budget's
// retry hint in the ErrorInfo metadata. Anything else out of the pipeline is
// an INFRASTRUCTURE failure (the budget gate's contract: typically a wrapped
// Postgres error), and its text belongs in the server log, never on the wire
// to an external caller — the REST path masks it the same way
// (writeBudgetRefusal's fail-closed 500).
func a2aCreateError(id json.RawMessage, err error) a2abridge.Response {
	var refusal *createRefusalError
	if errors.As(err, &refusal) {
		if refusal.status == http.StatusBadRequest {
			return a2abridge.NewErrorResponse(id, wire.ErrInvalidParams, refusal.detail, nil)
		}
		return a2abridge.NewErrorResponse(id, wire.ErrServerError, refusal.detail, nil)
	}
	var exceeded *budget.ExceededError
	if errors.As(err, &exceeded) {
		var meta map[string]string
		if exceeded.RetryAfter > 0 {
			meta = map[string]string{"retryAfterSeconds": strconv.Itoa(int(exceeded.RetryAfter.Seconds()))}
		}
		return a2abridge.NewErrorResponse(id, wire.ErrServerError, exceeded.Error(), meta)
	}
	log.Printf("A2A: task create failed (infrastructure): %v", err)
	return a2abridge.NewErrorResponse(id, wire.ErrInternalError, "task creation failed on the server; try again", nil)
}

// a2aErrorFrom renders an error already wrapping one of the wire sentinels.
func a2aErrorFrom(id json.RawMessage, err error) a2abridge.Response {
	for _, sentinel := range []error{
		wire.ErrTaskNotFound, wire.ErrTaskNotCancelable, wire.ErrUnsupportedOperation,
		wire.ErrUnsupportedContentType, wire.ErrInvalidParams, wire.ErrPushNotificationNotSupported,
		wire.ErrVersionNotSupported,
	} {
		if errors.Is(err, sentinel) {
			return a2abridge.NewErrorResponse(id, sentinel, err.Error(), nil)
		}
	}
	return a2abridge.NewErrorResponse(id, wire.ErrInternalError, err.Error(), nil)
}

// a2aGetTask implements GetTask: an ADR-0043-scoped read. The result is the
// Task object itself (only SendMessage wraps its result in the oneof).
func (h *Handlers) a2aGetTask(w http.ResponseWriter, _ *http.Request, p principal, req a2abridge.Request) {
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key cannot read tasks")
		return
	}
	var params wire.GetTaskRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a GetTaskRequest: "+err.Error(), nil))
		return
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	task, err := h.a2aVisibleTask(p, params.ID)
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, a2abridge.BuildTask(task, h.a2a.PublicBaseURL, true)))
}

// a2aPageToken encodes the list cursor. Opaque to callers per spec §3.1.4;
// internally it is just the next offset.
func a2aPageToken(offset int) string {
	return "o" + strconv.Itoa(offset)
}

func a2aParsePageToken(token string) (int, error) {
	rest, ok := strings.CutPrefix(token, "o")
	if !ok {
		return 0, fmt.Errorf("%w: invalid pageToken", wire.ErrInvalidParams)
	}
	offset, err := strconv.Atoi(rest)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("%w: invalid pageToken", wire.ErrInvalidParams)
	}
	return offset, nil
}

// a2aListTasks implements ListTasks: the creator-scoped listing with
// cursor-based pagination. nextPageToken is ALWAYS present — empty string
// when there are no more pages (spec §3.1.4).
func (h *Handlers) a2aListTasks(w http.ResponseWriter, _ *http.Request, p principal, req a2abridge.Request) {
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key cannot read tasks")
		return
	}
	var params wire.ListTasksRequest
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a ListTasksRequest: "+err.Error(), nil))
			return
		}
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	if params.StatusTimestampAfter != nil {
		// Refused loudly rather than silently ignored: a dropped filter would
		// hand the caller a result set that lies about what it is.
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "statusTimestampAfter is not supported by this server", nil))
		return
	}
	size := params.PageSize
	switch {
	case size == 0:
		size = 50
	case size < 1 || size > 100:
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "pageSize must be between 1 and 100", nil))
		return
	}
	offset := 0
	if params.PageToken != "" {
		var err error
		if offset, err = a2aParsePageToken(params.PageToken); err != nil {
			a2aWrite(w, a2aErrorFrom(req.ID, err))
			return
		}
	}

	filter := db.TaskFilter{}
	if !p.fleetWideTaskVisibility() {
		switch {
		case p.user != nil:
			filter.VisibleToUserID = &p.user.ID
		case p.apiKey != nil:
			filter.VisibleToKeyID = &p.apiKey.KeyID
		}
	}
	if params.Status != "" {
		statuses, known := a2abridge.FleetStatusesFor(params.Status)
		if !known {
			a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams,
				fmt.Sprintf("unknown status filter %q", string(params.Status)), nil))
			return
		}
		if len(statuses) == 0 {
			// A state no fleet task ever reports (REJECTED, AUTH_REQUIRED):
			// legitimately matches nothing.
			a2aWrite(w, a2abridge.NewResponse(req.ID, &wire.ListTasksResponse{
				Tasks: []*wire.Task{}, TotalSize: 0, PageSize: size, NextPageToken: "",
			}))
			return
		}
		filter.StatusIn = statuses
	}
	if params.ContextID != "" {
		// v1 contexts are 1:1 with tasks (BuildTask sets contextId = task id),
		// so a context filter is a visibility-gated point read; an unknown or
		// invisible context is an empty page, matching §3.3.2's
		// indistinguishability rule.
		tasks := []*wire.Task{}
		if task, err := h.a2aVisibleTask(p, wire.TaskID(params.ContextID)); err == nil {
			state, _ := a2abridge.TaskStateFor(task.Status)
			if params.Status == "" || state == params.Status {
				tasks = append(tasks, a2abridge.BuildTask(task, h.a2a.PublicBaseURL, params.IncludeArtifacts))
			}
		}
		a2aWrite(w, a2abridge.NewResponse(req.ID, &wire.ListTasksResponse{
			Tasks: tasks, TotalSize: len(tasks), PageSize: size, NextPageToken: "",
		}))
		return
	}

	tasks, total, err := h.storage.GetTasksFiltered(filter, size, offset)
	if err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInternalError, "failed to list tasks", nil))
		return
	}
	out := make([]*wire.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, a2abridge.BuildTask(t, h.a2a.PublicBaseURL, params.IncludeArtifacts))
	}
	next := ""
	if offset+size < total {
		next = a2aPageToken(offset + size)
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, &wire.ListTasksResponse{
		Tasks:         out,
		TotalSize:     total,
		PageSize:      size,
		NextPageToken: next,
	}))
}

// a2aCancelTask implements CancelTask. Authorization is the creator or a
// cancel-scoped credential — a DELIBERATE extension over the REST surface,
// where an API key never gains cancel through ownership: an A2A caller that
// created a task must be able to cancel it, or the protocol's cancel method
// is unusable by exactly the credential the card tells callers to use.
// Scoped to this surface; recorded in ADR-0051.
func (h *Handlers) a2aCancelTask(w http.ResponseWriter, _ *http.Request, p principal, req a2abridge.Request) {
	var params wire.CancelTaskRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a CancelTaskRequest: "+err.Error(), nil))
		return
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	task, err := h.a2aVisibleTask(p, params.ID)
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}
	if !taskCreatedByPrincipal(p, task) && !p.hasPermission(models.PermissionCancelTask) {
		writeError(w, http.StatusForbidden, "cancelling a task is limited to its creator or an operator credential")
		return
	}
	if task.Status == models.TaskStatusCancelled {
		// Idempotent: a repeated cancel reports the already-cancelled task
		// rather than an error the caller can do nothing about.
		a2aWrite(w, a2abridge.NewResponse(req.ID, a2abridge.BuildTask(task, h.a2a.PublicBaseURL, true)))
		return
	}

	who := p.stopLabel()
	cancelled, err := h.storage.CancelTaskAtomic(task.ID, "stopped by "+who)
	if err != nil {
		if strings.Contains(err.Error(), "cannot cancel") {
			a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrTaskNotCancelable,
				fmt.Sprintf("task %s is in state %s, which cannot be cancelled", task.ID, task.Status), nil))
			return
		}
		if strings.Contains(err.Error(), "no rows") {
			a2aWrite(w, a2aErrorFrom(req.ID, fmt.Errorf("%w: no task %q is visible to this credential", wire.ErrTaskNotFound, task.ID)))
			return
		}
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInternalError, "failed to cancel the task", nil))
		return
	}
	// Interrupt a live in-process run, exactly like DELETE /tasks/{id} (#508).
	if h.taskStopper != nil && h.taskStopper(task.ID, who) {
		log.Printf("Task cancelled via A2A: %s (live run interrupted, stopped by %s)", task.ID, logSafe(who)) //nolint:gosec // G706: task.ID is a parsed uuid.UUID; who passes logSafe.
	} else {
		log.Printf("Task cancelled via A2A: %s (stopped by %s)", task.ID, logSafe(who)) //nolint:gosec // G706: task.ID is a parsed uuid.UUID; who passes logSafe.
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, a2abridge.BuildTask(cancelled, h.a2a.PublicBaseURL, true)))
}

// a2aSubscribeToTask implements SubscribeToTask — the recovery path for a
// dropped SendStreamingMessage connection. Per spec §3.5.2 a subscription to
// an already-terminal task is refused with UnsupportedOperationError; the
// terminal result is GetTask's to serve.
func (h *Handlers) a2aSubscribeToTask(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request) {
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "insufficient key scope: this key cannot read tasks")
		return
	}
	var params wire.SubscribeToTaskRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a SubscribeToTaskRequest: "+err.Error(), nil))
		return
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	task, err := h.a2aVisibleTask(p, params.ID)
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}
	if task.Status.IsTerminal() {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrUnsupportedOperation,
			fmt.Sprintf("task %s is already terminal (state %s); use GetTask for the result", task.ID, task.Status), nil))
		return
	}
	h.a2aStreamTask(w, r, req.ID, task.ID)
}

// a2aStreamTask serves one task-lifecycle SSE stream (spec §3.1.2): it MUST
// open with the Task snapshot, then carries statusUpdate/artifactUpdate
// events in order, and MUST close when the task reaches a terminal state —
// closure is the completion signal (there is no `final` flag in v1.0).
//
// Every data: line is a complete JSON-RPC envelope reusing the request id.
// The task ROW is the event source, polled at a2aStreamPollInterval: the
// in-memory run-log buffer would only lower transition latency below a
// second, at the cost of merging two event sources — and it evicts after two
// minutes, while the row never lies. Nothing is lost to a dropped connection:
// SubscribeToTask re-reads the row.
func (h *Handlers) a2aStreamTask(w http.ResponseWriter, r *http.Request, rpcID json.RawMessage, taskID uuid.UUID) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	// Refused BEFORE the SSE headers, so the ceiling comes back as an
	// ordinary JSON-RPC error the client can act on, not a dead stream.
	if n := atomic.AddInt64(&h.a2aStreams, 1); n > a2aMaxConcurrentStreams {
		atomic.AddInt64(&h.a2aStreams, -1)
		a2aWrite(w, a2abridge.NewErrorResponse(rpcID, wire.ErrServerError,
			fmt.Sprintf("this server is at its concurrent A2A stream ceiling (%d); retry shortly, or poll GetTask", a2aMaxConcurrentStreams), nil))
		return
	}
	defer atomic.AddInt64(&h.a2aStreams, -1)
	lifetime := time.NewTimer(a2aStreamMaxLifetime)
	defer lifetime.Stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(resp a2abridge.Response) bool {
		data, err := json.Marshal(resp)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	emitEvent := func(ev wire.Event) bool {
		return emit(a2abridge.NewResponse(rpcID, wire.StreamResponse{Event: ev}))
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		emit(a2abridge.NewErrorResponse(rpcID, wire.ErrInternalError, "the task could not be read", nil))
		return
	}
	// Spec: the stream begins with the Task snapshot.
	snapshot := a2abridge.BuildTask(task, h.a2a.PublicBaseURL, true)
	if !emitEvent(snapshot) {
		return
	}
	lastState := snapshot.Status.State
	if lastState.Terminal() {
		return
	}

	ticker := time.NewTicker(a2aStreamPollInterval)
	defer ticker.Stop()
	lastFrame := time.Now()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			// Lifetime bound (see a2aStreamMaxLifetime): close without a
			// terminal statusUpdate, which a conformant client treats as a
			// dropped connection and recovers from with SubscribeToTask.
			return
		case <-ticker.C:
		}

		task, err = h.storage.GetTask(taskID)
		if err != nil || task == nil {
			emit(a2abridge.NewErrorResponse(rpcID, wire.ErrInternalError, "the task could not be re-read; resubscribe with SubscribeToTask", nil))
			return
		}
		state, _ := a2abridge.TaskStateFor(task.Status)
		if state == lastState {
			if time.Since(lastFrame) >= a2aStreamHeartbeat {
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
				lastFrame = time.Now()
			}
			continue
		}
		lastState = state
		lastFrame = time.Now()

		rendered := a2abridge.BuildTask(task, h.a2a.PublicBaseURL, true)
		if state.Terminal() {
			// Artifacts first, then the terminal status — generation order —
			// then close: the close IS the completion signal.
			for _, artifact := range rendered.Artifacts {
				if !emitEvent(&wire.TaskArtifactUpdateEvent{
					TaskID:    rendered.ID,
					ContextID: rendered.ContextID,
					Artifact:  artifact,
					LastChunk: true,
				}) {
					return
				}
			}
			emitEvent(&wire.TaskStatusUpdateEvent{
				TaskID:    rendered.ID,
				ContextID: rendered.ContextID,
				Status:    rendered.Status,
			})
			return
		}
		if !emitEvent(&wire.TaskStatusUpdateEvent{
			TaskID:    rendered.ID,
			ContextID: rendered.ContextID,
			Status:    rendered.Status,
		}) {
			return
		}
	}
}
