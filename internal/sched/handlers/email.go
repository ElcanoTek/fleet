package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"text/template"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/webhooks"
)

// InboundEmail is the vendor-neutral normalized shape fleet accepts for an
// email-ingress trigger (#511). An email provider's inbound-parse webhook
// (Postmark/Mailgun/SendGrid, or a small forwarder) is configured to POST this
// JSON to POST /triggers/email/{slug}. The provider performs DKIM/SPF
// verification and reports the RESULT here — fleet cannot verify DKIM without
// the raw signed message and DNS, so it consumes the provider's verdict and
// enforces policy on it.
type InboundEmail struct {
	// MessageID is the RFC 5322 Message-ID header — the idempotency key.
	MessageID string `json:"message_id"`
	// From is the sender (may be "Name <addr@domain>"; the address is extracted).
	From string `json:"from"`
	// To is the inbound address the mail was sent to (informational).
	To string `json:"to"`
	// Subject is the email subject.
	Subject string `json:"subject"`
	// Text is the plain-text body; HTML is the optional HTML alternative.
	Text string `json:"text"`
	HTML string `json:"html"`
	// SPF / DKIM are the provider-reported verification results ("pass", "fail",
	// "neutral", …). Policy enforces the required ones (see EmailTriggerPolicy).
	SPF  string `json:"spf"`
	DKIM string `json:"dkim"`
	// Attachments carries attachment METADATA only (v1 does not ingest content);
	// the policy's count/size limits are enforced against it.
	Attachments []InboundAttachment `json:"attachments"`
}

// InboundAttachment is attachment metadata (v1 does not fetch attachment bytes).
type InboundAttachment struct {
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// HandleEmailTrigger handles POST /triggers/email/{slug} (#511). Authentication
// mirrors the generic webhook trigger EXACTLY — one timing-equalized HMAC over
// the raw body, an identical 401 for an unknown slug, a non-email slug, or a bad
// signature, so an attacker cannot enumerate slugs or probe a trigger's kind.
// After auth it applies the email-kind security controls (approved senders,
// DKIM/SPF policy, attachment limits, Message-ID dedup) before spawning a
// governed run whose connector inheritance follows the template's
// allow_event_triggers opt-in.
func (h *Handlers) HandleEmailTrigger(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	sigHeader := r.Header.Get("X-Hub-Signature-256")
	if sigHeader == "" {
		sigHeader = r.Header.Get("X-Fleet-Signature-256")
	}

	// Look up the trigger only for well-formed slugs, and only ACCEPT it when it
	// is an email-kind trigger. A malformed slug, an unknown slug, or a
	// webhook-kind slug all leave trig nil → the dummy-HMAC miss path below → an
	// identical, timing-equalized 401, so neither existence nor kind leaks.
	var trig *models.TaskTrigger
	if triggerSlugShape.MatchString(slug) {
		t, lookupErr := h.storage.GetTriggerBySlug(r.Context(), slug)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			//nolint:gosec // G706: slug is sanitized via logSafe (CR/LF stripped) before interpolation.
			log.Printf("email trigger lookup error for slug %q: %v", logSafe(slug), lookupErr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if t != nil && t.KindOrWebhook() == models.TriggerKindEmail {
			trig = t
		}
	}

	// Always compute one HMAC — against the real secret when an email trigger
	// matched, else a per-process dummy — so the miss and bad-signature paths are
	// timing-indistinguishable. Then fail closed (identical 401).
	secret := dummyTriggerSecret
	if trig != nil {
		secret = trig.Secret
	}
	if trig == nil || !webhooks.VerifyHMACSHA256(body, secret, sigHeader) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var email InboundEmail
	if err := json.Unmarshal(body, &email); err != nil {
		writeError(w, http.StatusBadRequest, "invalid email payload")
		return
	}

	policy := trig.EmailPolicy // nil ⇒ most-restrictive posture (fail closed)
	if reason, ok := checkEmailPolicy(policy, email); !ok {
		// A policy rejection is a definitive, authenticated 403 — the caller proved
		// it holds the secret, so revealing WHY (sender/DKIM/attachment) is fine and
		// aids the operator; it does not help slug enumeration.
		writeError(w, http.StatusForbidden, reason)
		return
	}
	if tooLarge, reason := attachmentsExceedLimits(policy, email.Attachments); tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, reason)
		return
	}

	// Dedup: the provider may deliver the same email more than once. Record the
	// event keyed by Message-ID (or a content hash when absent); a duplicate is a
	// success no-op that spawns NO second run.
	ev := &models.TriggerEvent{
		TriggerID:      trig.ID,
		IdempotencyKey: emailIdempotencyKey(email),
		Sender:         emailAddress(email.From),
		Subject:        email.Subject,
		MessageID:      strings.TrimSpace(email.MessageID),
	}
	inserted, err := h.storage.RecordTriggerEvent(r.Context(), ev)
	if err != nil {
		//nolint:gosec // G706: slug is sanitized via logSafe before interpolation.
		log.Printf("email trigger %q dedup error: %v", logSafe(slug), err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !inserted {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	prompt, err := renderEmailPrompt(trig.PromptTemplate, email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "template render error")
		return
	}

	// The security default: an email-spawned run inherits the template's
	// write-capable connectors ONLY when the template opted in.
	inheritConnectors := false
	if task, terr := h.storage.GetTask(trig.TaskID); terr == nil && task != nil {
		inheritConnectors = task.AllowEventTriggers
	}

	runID, err := h.storage.SpawnEmailRun(r.Context(), trig, prompt, inheritConnectors)
	if err != nil {
		//nolint:gosec // G706: slug is sanitized via logSafe before interpolation.
		log.Printf("email trigger %q enqueue error: %v", logSafe(slug), err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Link the event to the run so the run is traceable to its inbound email and
	// reply-back can recover the sender. Best-effort: a failure here does not
	// fail the (already-spawned) run.
	if err := h.storage.SetTriggerEventRunID(r.Context(), ev.ID, runID); err != nil {
		//nolint:gosec // G706: slug is sanitized via logSafe before interpolation.
		log.Printf("email trigger %q event-link warning: %v", logSafe(slug), err)
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID.String()})
}

// checkEmailPolicy enforces the sender allowlist and the DKIM/SPF requirements.
// A nil policy is the most-restrictive posture: no approved senders (reject
// all), DKIM required. Returns (reason, ok); ok=false means reject with reason.
func checkEmailPolicy(policy *models.EmailTriggerPolicy, email InboundEmail) (string, bool) {
	requireDKIM := true
	requireSPF := false
	var approved []string
	if policy != nil {
		requireDKIM = policy.RequireDKIM
		requireSPF = policy.RequireSPF
		approved = policy.ApprovedSenders
	}

	if !senderApproved(email.From, approved) {
		return "sender not approved", false
	}
	if requireDKIM && !strings.EqualFold(strings.TrimSpace(email.DKIM), "pass") {
		return "DKIM verification required", false
	}
	if requireSPF && !strings.EqualFold(strings.TrimSpace(email.SPF), "pass") {
		return "SPF verification required", false
	}
	return "", true
}

// senderApproved reports whether from matches any allowlist entry. An entry with
// an "@" is a full-address match; an entry without is a domain match (any
// address at that domain). Matching is case-insensitive on the extracted
// address. An empty allowlist approves NO ONE (an email trigger must name senders).
func senderApproved(from string, approved []string) bool {
	addr := emailAddress(from)
	if addr == "" {
		return false
	}
	domain := ""
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		domain = addr[at+1:]
	}
	for _, entry := range approved {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		if strings.Contains(e, "@") {
			if e == addr {
				return true
			}
			continue
		}
		if domain != "" && e == domain {
			return true
		}
	}
	return false
}

// attachmentsExceedLimits reports whether the attachments violate the policy's
// count or per-attachment size caps. A nil policy (or zero MaxAttachments)
// disallows attachments entirely; a positive MaxAttachmentBytes bounds each one.
func attachmentsExceedLimits(policy *models.EmailTriggerPolicy, atts []InboundAttachment) (bool, string) {
	maxCount := 0
	var maxBytes int64
	if policy != nil {
		maxCount = policy.MaxAttachments
		maxBytes = policy.MaxAttachmentBytes
	}
	if len(atts) > maxCount {
		return true, fmt.Sprintf("too many attachments (%d > %d allowed)", len(atts), maxCount)
	}
	for _, a := range atts {
		if a.Size > 0 && (maxBytes <= 0 || a.Size > maxBytes) {
			return true, "attachment exceeds size limit"
		}
	}
	return false, ""
}

// emailAddress extracts the bare, lowercased address from a From value that may
// be "Name <addr@domain>". Falls back to the trimmed lowercased input when the
// value does not parse.
func emailAddress(from string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(from)); err == nil {
		return strings.ToLower(strings.TrimSpace(a.Address))
	}
	return strings.ToLower(strings.TrimSpace(from))
}

// emailIdempotencyKey is the dedup key: the Message-ID when present, else a
// content hash so a provider that omits Message-ID still dedups on re-delivery.
func emailIdempotencyKey(email InboundEmail) string {
	if id := strings.TrimSpace(email.MessageID); id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(email.From + "\x00" + email.Subject + "\x00" + email.Text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// emailPromptData is the data exposed to an email trigger's prompt_template.
type emailPromptData struct {
	From    string
	Subject string
	Text    string
	HTML    string
	To      string
}

// renderEmailPrompt renders the trigger's prompt_template against the email. An
// empty template yields a sensible DEFAULT prompt built from the email so an
// email trigger is useful with no template configured (and the email content
// always reaches the run rather than falling back to the bare template prompt).
func renderEmailPrompt(tmpl string, email InboundEmail) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		var b strings.Builder
		b.WriteString("You received an email. Act on it according to your task instructions.\n\n")
		fmt.Fprintf(&b, "From: %s\n", email.From)
		fmt.Fprintf(&b, "Subject: %s\n\n", email.Subject)
		b.WriteString(email.Text)
		return b.String(), nil
	}

	data := emailPromptData{
		From:    email.From,
		Subject: email.Subject,
		Text:    email.Text,
		HTML:    email.HTML,
		To:      email.To,
	}
	// G708: prompt_template is operator-authored (set via the admin CLI), never
	// request input. The email is exposed only as DATA. Output is a plain-text LLM
	// prompt consumed inside the mandatory sandbox — text/template is correct.
	//nolint:gosec // G708: prompt_template is operator-authored, not request input.
	t, err := template.New("email-trigger").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}
