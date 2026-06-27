package handlers

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"text/template"

	"database/sql"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// maxWebhookBody caps the inbound webhook payload at 1 MiB to prevent memory
// exhaustion from oversized requests.
const maxWebhookBody = 1 << 20

// triggerSlugShape bounds an inbound slug to the same URL-safe shape the create
// CLI enforces. An out-of-shape slug can never match a stored trigger, so the
// handler treats it exactly like an unknown slug (no DB probe, same 401) — this
// caps the per-request index lookup and narrows the timing surface.
var triggerSlugShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// dummyTriggerSecret is a per-process random HMAC key used to equalize work on
// the slug-miss path: an unknown (or malformed) slug still computes one
// HMAC-SHA256 before returning 401, so response timing cannot distinguish a
// valid slug with a bad signature from a slug that does not exist. Its value is
// never security-load-bearing — it only ever produces a guaranteed-failing
// comparison — but sourcing it from crypto/rand makes it unguessable regardless.
var dummyTriggerSecret = newDummyTriggerSecret()

func newDummyTriggerSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail; a fixed fallback keeps the package
		// loadable and still yields a failing comparison.
		return "fleet-dummy-trigger-secret"
	}
	return hex.EncodeToString(buf)
}

// triggerTemplateData is the data passed to a trigger's prompt_template. The
// template is a Go text/template (NOT html/template — the output is a plain-text
// prompt, not HTML).
type triggerTemplateData struct {
	// Payload is the full raw JSON request body as a string.
	Payload string
	// Body is the decoded JSON object, for dot-path / index access:
	//   {{ index .Body "action" }}
	Body map[string]interface{}
	// Headers carries selected forwarded request headers.
	Headers triggerHeaders
}

type triggerHeaders struct {
	ContentType string
	UserAgent   string
}

// HandleWebhookTrigger handles POST /triggers/{slug} (#177). It authenticates
// SOLELY via the per-trigger HMAC-SHA256 secret — never the admin API key — so
// external services (GitHub, Slack, CI) can call it without admin credentials.
//
// Unknown slug and bad signature both return an identical 401 AND perform the
// same HMAC work (a dummy compare on the miss path), so neither the response nor
// its timing lets an attacker enumerate valid slugs.
func (h *Handlers) HandleWebhookTrigger(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	// Read the (capped) body first, before the DB lookup, so the work done is the
	// same shape whether or not the slug exists.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	sigHeader := r.Header.Get("X-Hub-Signature-256")
	if sigHeader == "" {
		sigHeader = r.Header.Get("X-Fleet-Signature-256")
	}

	// Look up the trigger only for well-formed slugs; a malformed slug can't match
	// a stored row, so skip the DB probe (DoS guard) and fall through to the
	// equalized dummy-HMAC miss path below.
	var trig *models.TaskTrigger
	if triggerSlugShape.MatchString(slug) {
		t, lookupErr := h.storage.GetTriggerBySlug(r.Context(), slug)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			//nolint:gosec // G706: slug is sanitized via logSafe (CR/LF stripped) before interpolation.
			log.Printf("webhook trigger lookup error for slug %q: %v", logSafe(slug), lookupErr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		trig = t // nil on ErrNoRows
	}

	// Always compute one HMAC — against the real secret when the slug exists, else
	// a per-process dummy — so the unknown-slug and bad-signature paths are
	// timing-indistinguishable. Then fail closed (identical 401) if the slug was
	// unknown OR the signature did not verify.
	secret := dummyTriggerSecret
	if trig != nil {
		secret = trig.Secret
	}
	sigOK := verifyHMACSHA256(body, secret, sigHeader)
	if trig == nil || !sigOK {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	prompt, err := renderTriggerTemplate(trig.PromptTemplate, body, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "template render error")
		return
	}

	runID, err := h.storage.SpawnWebhookRun(r.Context(), trig, prompt)
	if err != nil {
		//nolint:gosec // G706: slug is sanitized via logSafe (CR/LF stripped) before interpolation.
		log.Printf("webhook trigger %q enqueue error: %v", logSafe(slug), err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID.String()})
}

// verifyHMACSHA256 reports whether sigHeader is a valid HMAC-SHA256 of body
// under secret. It accepts the GitHub-style "sha256=" prefix and compares in
// constant time. An empty secret or malformed signature fails closed.
func verifyHMACSHA256(body []byte, secret, sigHeader string) bool {
	if secret == "" {
		return false
	}
	sig := strings.TrimPrefix(sigHeader, "sha256=")
	if len(sig) != hex.EncodedLen(sha256.Size) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sig)), []byte(expected)) == 1
}

// renderTriggerTemplate renders a trigger's prompt_template against the inbound
// payload. An empty template yields an empty string (the spawn path then falls
// back to the template task's own prompt). The body is best-effort JSON-decoded
// for {{ index .Body ... }} access; a non-JSON body leaves .Body nil but still
// exposes the raw .Payload.
func renderTriggerTemplate(tmpl string, body []byte, r *http.Request) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return "", nil
	}
	var decoded map[string]interface{}
	_ = json.Unmarshal(body, &decoded) // best-effort; nil on non-object payloads

	data := triggerTemplateData{
		Payload: string(body),
		Body:    decoded,
		Headers: triggerHeaders{
			ContentType: r.Header.Get("Content-Type"),
			UserAgent:   r.Header.Get("User-Agent"),
		},
	}

	// G708: the template is operator-configured (set via `fleet-admin sched
	// trigger create --template`, an admin-only path), NEVER attacker-supplied.
	// The inbound webhook payload is exposed only as DATA (.Payload/.Body), not as
	// the template text. The rendered output is a plain-text LLM prompt, not
	// executed code — hence text/template (not html/template) is the right choice.
	//nolint:gosec // G708: prompt_template is operator-authored, not request input.
	t, err := template.New("trigger").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}
