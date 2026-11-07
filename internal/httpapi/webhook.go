package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/webhooks"
)

// maxWebhookBody caps an inbound webhook payload at 1 MiB — the same shape guard
// the orchestrator's task-trigger endpoint uses, so an oversized body can't be a
// pre-auth memory lever on the shared-host box.
const maxWebhookBody = 1 << 20

// webhookTriggersPerMinutePerSlug bounds how many conversations a single
// CONFIGURED trigger may spawn per minute. Overridable via
// FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE. The cap is consulted only AFTER a request
// authenticates (see postWebhook), so it throttles authenticated turn-spawns and
// never creates a bucket for an attacker-supplied unknown slug.
const webhookTriggersPerMinutePerSlug = 10

// webhookDummySecret equalizes the slug-miss path: an unknown (or malformed)
// slug still performs the same signature computations as a configured trigger
// before the identical 401, so neither the response nor its timing
// distinguishes a known slug with a bad signature from a slug that does not
// exist. Per-process random via the shared webhooks package.
var webhookDummySecret = webhooks.NewDummySecret()

// postWebhook handles POST /webhooks/{slug} (#268): an external system that
// presents a valid signature starts a fresh interactive conversation under the
// trigger's configured notify_user, seeded with a prompt rendered from the
// trigger's template against the request payload. The turn runs through the SAME
// governed core (runTurnAsync → agent.RunTurn → agentcore.Run) as any chat turn
// — this is an inbound I/O adapter, not a second agent loop.
//
// The endpoint is deliberately registered OUTSIDE the auth(member(mutate(…)))
// chain (like /healthz and /shared/): external callers (GitHub, Slack, CI)
// cannot present a Fleet session token, so authenticity is proven instead by the
// per-trigger HMAC / Slack signing secret. An unknown slug and a bad signature
// return an identical timing-equalized 401 so a caller cannot enumerate slugs.
// See docs/adr/0016-webhook-triggered-conversations.md for the security model.
func (s *Server) postWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Draining (#278): once graceful shutdown begins, admit no new turns.
	if s.IsDraining() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhooks/"), "/")

	// Look up the trigger before reading the body so the work shape is the same
	// whether or not the slug exists; the miss path below still performs the
	// same signature computations over the body.
	var (
		trig  clientconfig.WebhookTriggerDef
		found bool
	)
	if s.clientConfig != nil {
		trig, found = s.clientConfig.WebhookTrigger(slug)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	// Authenticate. verifyWebhookSignature performs the SAME signature
	// computations on every request — against the real secret when the slug
	// exists, else a per-process dummy — then fails closed on a miss, so the
	// unknown-slug and bad-signature paths are timing-indistinguishable.
	if !s.verifyWebhookSignature(r, body, trig, found) {
		metrics.RecordWebhookTrigger("", "rejected")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse the JSON payload (Slack Events API + GitHub webhooks are JSON).
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Slack URL-verification handshake: echo the challenge and create nothing.
	// This request is signed like any Slack event, so it is only reached after a
	// successful signature check above.
	if trig.UsesSlack() {
		if t, _ := payload["type"].(string); t == "url_verification" {
			challenge, _ := payload["challenge"].(string)
			writeJSON(w, map[string]any{"challenge": challenge})
			return
		}
	}

	// Delivery idempotency: senders deliver at least once (Slack retries an
	// event it did not see answered in time, with X-Slack-Retry-Num; GitHub's
	// Redeliver re-sends the same X-GitHub-Delivery). Claim the delivery before
	// anything else so a repeat of an accepted one answers 200 with the
	// original conversation and starts nothing — and spends none of the slug's
	// rate budget. A delivery counts as accepted only once its turn's input is
	// durable (CommitWebhookUserMessage completes the claim in the same
	// transaction that commits the user message), and the handler answers 202
	// only after that, so a crash before it leaves the delivery retryable.
	// Every failure gives the claim back (and discards the conversation
	// created for it while still empty), so a delivery that was not accepted
	// is processed afresh when it comes again. The claim token makes those
	// calls no-ops once a repeat has taken a stale claim over.
	deliveryKey := webhookDeliveryKey(r, body, payload, trig)
	claim, err := s.store.ClaimWebhookDelivery(r.Context(), trig.Slug, deliveryKey)
	if err != nil {
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, "could not record the delivery", http.StatusInternalServerError)
		return
	}
	if !claim.Claimed {
		if !claim.Completed {
			// The first request for this delivery has not made its turn
			// durable yet. 409 rather than 200: if that request fails, the
			// sender's next retry should be processed, not swallowed.
			metrics.RecordWebhookTrigger(trig.Slug, "conflict")
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "this delivery is already being processed"})
			return
		}
		metrics.RecordWebhookTrigger(trig.Slug, "duplicate")
		writeJSONStatus(w, http.StatusOK, map[string]any{"conversation_id": claim.ConversationID, "duplicate": true})
		return
	}
	var (
		bound    bool // the conversation is created and bound to the claim
		launched bool // the turn goroutine is started; it settles the claim itself
	)
	defer func() {
		switch {
		case launched:
		case bound:
			_, _ = s.abandonWebhookConversation(trig.Slug, deliveryKey, claim.Token, "")
		default:
			s.releaseWebhookClaim(trig.Slug, deliveryKey, claim.Token)
		}
	}()

	// Authenticated: throttle authenticated turn-spawns per configured slug. The
	// limiter is consulted here (not before auth) so an attacker probing unknown
	// slugs never creates a bucket and cannot use rate-limit behavior as a
	// slug-existence oracle.
	if ok, retry := s.webhookRL.Allow(trig.Slug); !ok {
		metrics.RecordWebhookTrigger(trig.Slug, "throttled")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	prompt, err := renderWebhookPrompt(trig.PromptTemplate, payload, body)
	if err != nil {
		// The template is operator-authored; log the detail host-side but return a
		// generic message so a rendering error can't leak template internals.
		logging.From(r.Context()).Error("webhook trigger template render error", "slug", safe.LogSafe(trig.Slug), "err", err)
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, "template render error", http.StatusInternalServerError)
		return
	}

	persona := strings.TrimSpace(trig.Persona)
	if persona == "" {
		persona = s.cfg.PersonaDefault
	}
	user := strings.TrimSpace(trig.NotifyUser)
	model := strings.TrimSpace(trig.Model)

	// Honor the server-wide lockdown seal exactly like postChat and
	// POST /conversations (server.go). A webhook is an external caller and cannot
	// opt a conversation OUT of the global seal, so lockdown is simply
	// s.cfg.LockdownOnly (no per-request override). This matters precisely on the
	// untrusted-payload path: on a CHAT_LOCKDOWN_ONLY box the triggered turn must
	// run in the same --network=none sandbox as every human turn. Fail closed
	// (never create an unenforceable lockdown conversation) on misconfiguration.
	lockdown := s.cfg.LockdownOnly
	if lockdown {
		if !s.cfg.LockdownAvailable() {
			metrics.RecordWebhookTrigger(trig.Slug, "error")
			http.Error(w, "lockdown is unavailable on this server (no sandbox image configured)", http.StatusInternalServerError)
			return
		}
		if model != "" && !s.cfg.LockdownAllows(model) {
			metrics.RecordWebhookTrigger(trig.Slug, "error")
			http.Error(w, "webhook trigger model not allowed in lockdown mode", http.StatusInternalServerError)
			return
		}
	}

	// Concurrency admission keyed by the trigger owner, so a burst of webhooks
	// can't hold every worker slot. admitConcurrentTurn writes its own 429.
	releaseSlot, admitted := s.admitConcurrentTurn(w, user)
	if !admitted {
		metrics.RecordWebhookTrigger(trig.Slug, "throttled")
		return
	}
	s.activeTurns.Add(1)
	s.activeTurnCount.Add(1)
	releaseTurn := func() {
		s.activeTurnCount.Add(-1)
		s.activeTurns.Done()
		releaseSlot()
	}
	if s.IsDraining() {
		releaseTurn()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	memories, err := s.store.ListMemories(r.Context(), user)
	if err != nil {
		releaseTurn()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create the conversation and bind it to the delivery claim in one
	// transaction: a turn is only ever launched for a delivery whose record
	// names its conversation. Always a new conversation under the current
	// settings (lockdown seal, model), also after a takeover.
	conv, err := s.store.CreateWebhookConversation(r.Context(), trig.Slug, deliveryKey, claim.Token,
		user, agent.HeuristicTitle(prompt), persona, model, lockdown)
	if errors.Is(err, store.ErrWebhookClaimLost) {
		// This request outlived its claim and a repeat now owns the delivery.
		releaseTurn()
		metrics.RecordWebhookTrigger(trig.Slug, "conflict")
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "this delivery is already being processed"})
		return
	}
	if err != nil {
		releaseTurn()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bound = true
	s.autoShareNewConversationIfConfigured(r.Context(), user, conv)

	history, err := s.store.LoadHistory(r.Context(), conv.ID)
	if err != nil {
		releaseTurn()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Same detached-turn machinery as postChat, minus the SSE attach: the turn
	// runs fire-and-forget in a goroutine and the conversation surfaces in the
	// notify_user's list the next time they open Fleet.
	turnCtx, turnCancel := context.WithTimeout(context.Background(), s.turnTimeout())
	buf, turnID, turnToken, ok := s.registerTurn(conv.ID, turnCancel)
	if !ok {
		// Unreachable in practice — the conversation was created just above,
		// so no turn can be running on it. Fail closed rather than panic.
		turnCancel()
		releaseTurn()
		metrics.RecordWebhookTrigger(trig.Slug, "conflict")
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "conversation is busy"})
		return
	}

	// Background-rooted for the same reason as startTurn: the turn outlives
	// the webhook POST, and its ledger must exist whether or not the sender
	// waits for the response.
	//
	// Unlike a chat turn, a failure here is fatal: without the turn row,
	// startup recovery could not account for a turn whose input later
	// committed.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = buf.attachPersister(persistCtx, s.store)
	persistCancel()
	if err != nil {
		turnCancel()
		s.finishTurn(conv.ID, turnToken)
		releaseTurn()
		logging.From(turnCtx).Error("webhook trigger: start turn failed", "conv_id", conv.ID, "err", err)
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, "could not start the turn", http.StatusInternalServerError)
		return
	}

	buf.Emit("conversation", map[string]any{
		"id":      conv.ID,
		"title":   conv.Title,
		"persona": conv.Persona,
		"model":   conv.Model,
	})
	buf.Emit("turn.started", map[string]any{"turn_id": turnID, "persona": conv.Persona})
	buf.Emit("user.message", map[string]any{"text": prompt})

	// Surface any files persisted from earlier turns (empty on this first
	// turn). Accumulated as the turn's INJECTED context, separate from the
	// trigger prompt itself, exactly as the chat path does (ADR-0058).
	injected := appendWorkspaceInventoryBlock("", tools.WorkspaceDirForConversation(conv.ID))

	// The turn's user-message commit (the shared #798 commit point, before
	// the first provider call) also completes the delivery claim, in one
	// transaction (store.CommitWebhookUserMessage), and reports its result
	// here. A turn whose claim is gone fails that commit and runs nothing.
	commit := &webhookCommitStore{
		chatStore: s.store, slug: trig.Slug, deliveryKey: deliveryKey, token: claim.Token,
		done: make(chan error, 1),
	}
	buf.userCommitStore = commit
	launched = true
	go func() {
		defer releaseTurn()
		// No signed-in sender: the trigger owner is the conversation's user
		// for scoping, but nobody at a keyboard sent this turn, so the
		// bundle's ${FLEET_USER_EMAIL} keys are dropped (ADR-0087).
		s.runTurnAsync(turnCtx, turnCancel, buf, turnToken, conv, user, prompt, injected, "", history, memoryContents(memories), "", nil, nil, agentcore.MCPUserIdentity{})
		// A turn that ended without reaching its input commit (it failed
		// while preparing) reports that, so the handler stops waiting.
		commit.report(errWebhookTurnEndedEarly)
		// Webhook turns participate in the #785 drain loop too: anything
		// queued behind this turn must not stall until the next submission.
		s.maybeDrainQueue(conv.ID)
	}()

	// Answer 202 only once the turn's input is durable and the delivery
	// complete: a 2xx tells the sender the event was taken, and it will not
	// send it again.
	var commitErr error
	select {
	case commitErr = <-commit.done:
	case <-time.After(webhookInputCommitTimeout):
		commitErr = errWebhookInputTimeout
	}
	if commitErr != nil {
		// Stop the turn, then give the delivery back. Cancelling does not
		// stop an input commit already in flight (it runs on its own
		// context), so the order that keeps the answer honest is the delivery
		// row: AbandonWebhookConversation and CommitWebhookUserMessage both
		// lock it and both require it unfinished, so exactly one wins. If the
		// commit won, the delivery IS accepted and the answer is 202; if the
		// abandon won, the commit finds no claim and the turn runs nothing,
		// and the answer is 500 so the sender retries.
		turnCancel()
		completed, err := s.abandonWebhookConversation(trig.Slug, deliveryKey, claim.Token, turnID)
		if !completed || err != nil {
			logging.From(turnCtx).Error("webhook trigger: turn input not saved", "conv_id", conv.ID, "err", commitErr)
			metrics.RecordWebhookTrigger(trig.Slug, "error")
			http.Error(w, "could not start the turn", http.StatusInternalServerError)
			return
		}
	}

	metrics.RecordWebhookTrigger(trig.Slug, "ok")
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"conversation_id": conv.ID})
}

// webhookInputCommitTimeout bounds how long the webhook handler waits for the
// turn's input to be saved before answering. Past it the handler gives the
// delivery back and answers 500, so the sender retries.
var webhookInputCommitTimeout = 10 * time.Second

var (
	errWebhookTurnEndedEarly = errors.New("the turn ended before its input was saved")
	errWebhookInputTimeout   = errors.New("timed out waiting for the turn's input to be saved")
)

// webhookCommitStore is the store a webhook turn commits through: identical,
// except that committing the turn's user message also completes the delivery
// claim, in one transaction (store.CommitWebhookUserMessage), and reports the
// result on done for the waiting handler. The turn runs the shared path
// (runTurnAsync, agentcore.Run) unchanged.
type webhookCommitStore struct {
	chatStore
	slug, deliveryKey, token string
	done                     chan error // capacity 1; the first report wins
}

func (c *webhookCommitStore) CommitUserMessage(ctx context.Context, convID, turnID string, entry agent.HistoryEntry) (int64, error) {
	id, err := c.CommitWebhookUserMessage(ctx, c.slug, c.deliveryKey, c.token, convID, turnID, entry)
	c.report(err)
	return id, err
}

// report delivers the first outcome to the handler; later ones are dropped.
func (c *webhookCommitStore) report(err error) {
	select {
	case c.done <- err:
	default:
	}
}

// releaseWebhookClaim drops the delivery claim of a request that did not
// create a conversation, so the sender's retry is processed afresh.
// Background-rooted: the request context may already be cancelled, and a
// leaked claim would refuse the retry until it goes stale.
func (s *Server) releaseWebhookClaim(slug, deliveryKey, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.ReleaseWebhookDelivery(ctx, slug, deliveryKey, token); err != nil {
		logging.From(ctx).Error("webhook trigger: release delivery claim failed", "err", err)
	}
}

// abandonWebhookConversation gives back a delivery whose turn input was not
// saved and discards the conversation this request created for it while it is
// still empty (ownTurnID, the request's own turn row, does not count), so the
// sender's retry is processed afresh. completed reports that the input commit
// won the race and the delivery is accepted after all.
func (s *Server) abandonWebhookConversation(slug, deliveryKey, token, ownTurnID string) (completed bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err = s.store.AbandonWebhookConversation(ctx, slug, deliveryKey, token, ownTurnID)
	if err != nil {
		logging.From(ctx).Error("webhook trigger: give back delivery failed", "err", err)
	}
	return completed, err
}

// webhookDeliveryKey names one delivery for the per-slug idempotency claim, in
// order of preference:
//
//   - X-GitHub-Delivery — GitHub's per-delivery GUID, kept on redelivery;
//   - a Slack trigger's payload event_id — the same on every retry of an
//     Events API event (those carry X-Slack-Retry-Num; their signature and
//     timestamp change per attempt, so the body hash below would not match);
//   - otherwise the SHA-256 of the signature header and the body, so the same
//     signed request delivered twice is one delivery.
//
// The value is hashed so a sender-chosen id has a bounded size in the table.
func webhookDeliveryKey(r *http.Request, body []byte, payload map[string]any, trig clientconfig.WebhookTriggerDef) string {
	hashed := func(source string, parts ...[]byte) string {
		h := sha256.New()
		for i, p := range parts {
			if i > 0 {
				h.Write([]byte{0})
			}
			h.Write(p)
		}
		return source + ":" + hex.EncodeToString(h.Sum(nil))
	}
	if id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")); id != "" {
		return hashed("github", []byte(id))
	}
	sigHeader := trig.SignatureHeader()
	if trig.UsesSlack() {
		if id, _ := payload["event_id"].(string); strings.TrimSpace(id) != "" {
			return hashed("slack", []byte(strings.TrimSpace(id)))
		}
		sigHeader = "X-Slack-Signature"
	}
	return hashed("body", []byte(r.Header.Get(sigHeader)), body)
}

// verifyWebhookSignature reports whether the request carries a valid signature
// for the trigger. EVERY request — known or unknown slug, Slack or GitHub-style
// trigger, any signature header — performs BOTH full-body signature
// computations (one Slack-style, one HMAC-SHA256), each against the configured
// secret when the slug supplies it and the per-process dummy otherwise, and
// then fails closed on a miss. Running the unconditional pair (rather than
// mirroring only the configured verifier's shape) keeps the work independent of
// slug existence AND of the trigger's verifier type, so a caller cannot use
// response timing to enumerate slugs (#572: a Slack or custom-hmac_header
// trigger used to do full-body work only on a hit, a measurable gap the
// previous single-compute miss path did not cover). The verifiers themselves
// compute their HMAC before any header-shape check for the same reason.
func (s *Server) verifyWebhookSignature(r *http.Request, body []byte, trig clientconfig.WebhookTriggerDef, found bool) bool {
	slackSecret := webhookDummySecret
	hmacSecret := webhookDummySecret
	header := clientconfig.DefaultHMACHeader
	if found {
		if trig.UsesSlack() {
			slackSecret = envreg.Unregistered(trig.TokenSecretEnv)
		} else {
			hmacSecret = envreg.Unregistered(trig.HMACSecretEnv)
			header = trig.SignatureHeader()
		}
	}
	slackOK := webhooks.VerifySlackSignature(body, slackSecret,
		r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), time.Now())
	hmacOK := webhooks.VerifyHMACSHA256(body, hmacSecret, r.Header.Get(header))
	if !found {
		return false
	}
	if trig.UsesSlack() {
		return slackOK
	}
	return hmacOK
}

// renderWebhookPrompt renders a trigger's prompt_template against the inbound
// payload. The template addresses the decoded JSON body via the lowercase
// `.payload` key (e.g. {{.payload.pull_request.title}}) and the full raw body
// via `.raw`. An empty template falls back to the raw JSON body as the prompt,
// so a trigger with no template still produces a usable (if unstructured)
// message.
//
// The template is a Go text/template (NOT html/template — the output is a
// plain-text LLM prompt, not HTML). The inbound payload is exposed only as DATA,
// never as the template text.
func renderWebhookPrompt(tmpl string, payload map[string]any, raw []byte) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return string(raw), nil
	}
	// prompt_template is operator-authored (a manifest field, an admin-only path),
	// NEVER attacker-supplied. The inbound payload is exposed only as DATA
	// (.payload / .raw), not as the template text, and the rendered output is a
	// plain-text LLM prompt consumed inside the mandatory sandbox — never
	// interpolated into a host-side shell — so text/template is the right choice.
	t, err := template.New("webhook").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, map[string]any{"payload": payload, "raw": string(raw)}); err != nil {
		return "", err
	}
	return sb.String(), nil
}
