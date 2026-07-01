package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/metrics"
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
// slug still performs one HMAC-SHA256 before the identical 401, so neither the
// response nor its timing distinguishes a known slug with a bad signature from a
// slug that does not exist. Per-process random via the shared webhooks package.
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
	if s.shuttingDown.Load() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhooks/"), "/")

	// Look up the trigger before reading the body so the work shape is the same
	// whether or not the slug exists; the miss path below still computes one HMAC.
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

	// Authenticate. verifyWebhookSignature always performs one signature
	// computation — against the real secret when the slug exists, else a
	// per-process dummy — then fails closed on a miss, so the unknown-slug and
	// bad-signature paths are timing-indistinguishable.
	if !s.verifyWebhookSignature(r, body, trig, found) {
		metrics.RecordWebhookTrigger("", "rejected")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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

	prompt, err := renderWebhookPrompt(trig.PromptTemplate, payload, body)
	if err != nil {
		// The template is operator-authored; log the detail host-side but return a
		// generic message so a rendering error can't leak template internals.
		log.Printf("webhook trigger %q template render error: %v", logSafeSlug(trig.Slug), err)
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

	conv, err := s.store.CreateConversation(r.Context(), user, agent.HeuristicTitle(prompt), persona, model, lockdown)
	if err != nil {
		releaseSlot()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	history, err := s.store.LoadHistory(r.Context(), conv.ID)
	if err != nil {
		releaseSlot()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	memories, err := s.store.ListMemories(r.Context(), user)
	if err != nil {
		releaseSlot()
		metrics.RecordWebhookTrigger(trig.Slug, "error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Same detached-turn machinery as postChat, minus the SSE attach: the turn
	// runs fire-and-forget in a goroutine and the conversation surfaces in the
	// notify_user's list the next time they open Fleet.
	turnCtx, turnCancel := context.WithTimeout(context.Background(), s.turnTimeout())
	buf, turnID, turnToken := s.registerTurn(conv.ID, turnCancel)

	persistCtx, persistCancel := context.WithTimeout(r.Context(), 5*time.Second)
	if err := buf.attachPersister(persistCtx, s.store); err != nil {
		log.Printf("attachPersister (webhook user=%s conv=%s): %v", user, conv.ID, err)
	}
	persistCancel()

	buf.Emit("conversation", map[string]any{
		"id":      conv.ID,
		"title":   conv.Title,
		"persona": conv.Persona,
		"model":   conv.Model,
	})
	buf.Emit("turn.started", map[string]any{"turn_id": turnID, "persona": conv.Persona})
	buf.Emit("user.message", map[string]any{"text": prompt})

	// Surface any files persisted from earlier turns (empty on this first turn).
	userMessage := appendWorkspaceInventoryBlock(prompt, tools.WorkspaceDirForConversation(conv.ID))

	s.activeTurns.Add(1)
	s.activeTurnCount.Add(1)
	go func() {
		defer func() {
			s.activeTurnCount.Add(-1)
			s.activeTurns.Done()
		}()
		defer releaseSlot()
		s.runTurnAsync(turnCtx, turnCancel, buf, turnToken, conv, user, prompt, userMessage, history, memoryContents(memories), "", nil)
	}()

	metrics.RecordWebhookTrigger(trig.Slug, "ok")
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"conversation_id": conv.ID})
}

// verifyWebhookSignature reports whether the request carries a valid signature
// for the trigger. It ALWAYS performs one signature computation — against the
// configured secret when the slug exists, else the per-process dummy — and then
// returns found && ok, so an unknown slug fails closed with the same work and
// timing as a known slug with a bad signature (no enumeration oracle).
func (s *Server) verifyWebhookSignature(r *http.Request, body []byte, trig clientconfig.WebhookTriggerDef, found bool) bool {
	if found && trig.UsesSlack() {
		secret := os.Getenv(trig.TokenSecretEnv)
		return webhooks.VerifySlackSignature(body, secret,
			r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), time.Now())
	}
	// GitHub-style HMAC path; also the miss path (dummy secret, one compute).
	secret := webhookDummySecret
	header := clientconfig.DefaultHMACHeader
	if found {
		secret = os.Getenv(trig.HMACSecretEnv)
		header = trig.SignatureHeader()
	}
	ok := webhooks.VerifyHMACSHA256(body, secret, r.Header.Get(header))
	return found && ok
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

// logSafeSlug strips CR/LF from a slug before it is interpolated into a log line
// (log-injection guard). Configured slugs are already shape-constrained, but the
// guard keeps the call site safe regardless.
func logSafeSlug(slug string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(slug)
}
