// Approval result cards: the settled card reads the call's own result.
//
// A bundle can list a critical tool in agent_policy.critical_tool_result_cards
// (docs/APPROVAL-RESULT-CARDS.md, ADR-0084). Its result JSON then may carry a
// top-level `result_card` object: a one-line outcome, a row per record with
// its ids, status and link, and the record keys a retry may name. When the
// approved call (or the call a notify-mode record records) returns, fleet
// checks that object against the fixed schema below, refuses it if the secret
// redaction would alter it, and stores it on the approval row. The web renders
// it as the completed card. Fleet makes no second call: the card is read from
// the result the approved call already returned, and only from it.
//
// Any failure (no `result_card`, a result that is not a JSON object, output
// that is not exactly the schema, an outcome that contradicts its rows, a
// secret-like value, a failed store write) leaves the card off, and the
// settled card renders as before: "Applied · <title>" over the raw result.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// resultCardKey is the top-level key of a tool result that carries the card.
const resultCardKey = "result_card"

// approvalResultMaxScanBytes bounds the result text fleet decodes to find the
// card. A larger result keeps today's card (the stored card itself is bounded
// by approvalCardMaxBytes, like a describer's).
const approvalResultMaxScanBytes = 4 << 20

// Result-card limits beyond the shared card limits (approval_card.go).
const (
	approvalResultMaxRetryKeys = approvalCardMaxItems
	approvalResultMaxKey       = approvalCardMaxTitle
)

// The overall outcome a card states. It sets the card's tone (success,
// warning, danger), so fleet holds it to the rows (validateResultCard).
const (
	resultOutcomeDone     = "done"     // every record applied (or already done)
	resultOutcomePartial  = "partial"  // some applied, some not
	resultOutcomeFailed   = "failed"   // nothing applied
	resultOutcomeMismatch = "mismatch" // applied, but the read-back did not match
	resultOutcomeUnclear  = "unclear"  // fleet's caller cannot tell yet
)

// The per-record status.
const (
	resultItemDone     = "done"     // applied, and verified when the tool verifies
	resultItemFailed   = "failed"   // confirmed not applied
	resultItemMismatch = "mismatch" // applied, but the read-back differs
	resultItemUnclear  = "unclear"  // may or may not have happened
	resultItemSkipped  = "skipped"  // nothing to do (already done earlier)
	resultItemNotRun   = "not_run"  // never attempted (left out, or stopped before it)
)

var resultOutcomes = map[string]bool{
	resultOutcomeDone: true, resultOutcomePartial: true, resultOutcomeFailed: true,
	resultOutcomeMismatch: true, resultOutcomeUnclear: true,
}

var resultItemStatuses = map[string]bool{
	resultItemDone: true, resultItemFailed: true, resultItemMismatch: true,
	resultItemUnclear: true, resultItemSkipped: true, resultItemNotRun: true,
}

// ApprovalResultCard is the schema a result's `result_card` must match,
// exactly. Unknown keys, wrong types, nulls and trailing data are refused.
type ApprovalResultCard struct {
	Outcome  string                   `json:"outcome"`
	Title    string                   `json:"title"`
	Subtitle string                   `json:"subtitle,omitempty"`
	Items    []ApprovalResultCardItem `json:"items"`
	Footer   string                   `json:"footer,omitempty"`
	Retry    *ApprovalResultCardRetry `json:"retry,omitempty"`
}

// ApprovalResultCardItem is one record the call acted on.
type ApprovalResultCardItem struct {
	// Key is the record's key in the call (the id a retry names). Optional,
	// unique within the card when given.
	Key   string `json:"key,omitempty"`
	Label string `json:"label"`
	// ID is the buyer-facing id; InternalID the system's own (for later
	// updates). Both optional.
	ID         string `json:"id,omitempty"`
	InternalID string `json:"internal_id,omitempty"`
	Link       string `json:"link,omitempty"`
	// LinkLabel names the link, e.g. "Open in Index Exchange". Optional.
	LinkLabel string                `json:"link_label,omitempty"`
	Status    string                `json:"status"`
	Note      string                `json:"note,omitempty"`
	Changes   []ApprovalCardChange  `json:"changes,omitempty"`
	Settings  []ApprovalCardSetting `json:"settings,omitempty"`
	Flags     []ApprovalCardFlag    `json:"flags,omitempty"`
}

// ApprovalResultCardRetry names the records a retry may run again. The web
// offers "Retry failed (N)", which sends a user turn naming these keys; it
// never runs anything itself, so the retry is staged for approval again.
type ApprovalResultCardRetry struct {
	Keys []string `json:"keys"`
}

// parseApprovalResultCard strictly decodes and validates a `result_card` and
// returns it re-marshaled in canonical form.
func parseApprovalResultCard(raw []byte) (*ApprovalResultCard, []byte, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("empty card")
	}
	if len(raw) > approvalCardMaxBytes {
		return nil, nil, fmt.Errorf("card is %d bytes, over the %d-byte limit", len(raw), approvalCardMaxBytes)
	}
	if !utf8.Valid(raw) {
		return nil, nil, errors.New("card is not valid UTF-8")
	}
	if err := refuseJSONNulls(raw); err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var card ApprovalResultCard
	if err := dec.Decode(&card); err != nil {
		return nil, nil, fmt.Errorf("not the result card schema: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trailing data after the card")
	}
	if err := validateResultCard(&card); err != nil {
		return nil, nil, err
	}
	canonical, err := json.Marshal(&card)
	if err != nil {
		return nil, nil, err
	}
	if len(canonical) > approvalCardMaxBytes {
		return nil, nil, fmt.Errorf("card is %d bytes, over the %d-byte limit", len(canonical), approvalCardMaxBytes)
	}
	return &card, canonical, nil
}

func validateResultCard(c *ApprovalResultCard) error {
	if !resultOutcomes[c.Outcome] {
		return fmt.Errorf("outcome %q is not one of done, partial, failed, mismatch, unclear", c.Outcome)
	}
	if err := cardText("title", c.Title, approvalCardMaxTitle, true); err != nil {
		return err
	}
	if err := cardText("subtitle", c.Subtitle, approvalCardMaxText, false); err != nil {
		return err
	}
	if err := cardText("footer", c.Footer, approvalCardMaxText, false); err != nil {
		return err
	}
	if c.Items == nil {
		return errors.New("items is required")
	}
	if len(c.Items) > approvalCardMaxItems {
		return fmt.Errorf("%d items, over the %d-item limit", len(c.Items), approvalCardMaxItems)
	}
	keys := map[string]string{} // key -> status
	counts := map[string]int{}
	rows := 0
	for i := range c.Items {
		it := &c.Items[i]
		at := fmt.Sprintf("items[%d]", i)
		if !resultItemStatuses[it.Status] {
			return fmt.Errorf("%s.status %q is not one of done, failed, mismatch, unclear, skipped, not_run", at, it.Status)
		}
		counts[it.Status]++
		for _, f := range []struct {
			name     string
			v        string
			max      int
			required bool
		}{
			{"label", it.Label, approvalCardMaxTitle, true},
			{"key", it.Key, approvalResultMaxKey, false},
			{"id", it.ID, approvalCardMaxTitle, false},
			{"internal_id", it.InternalID, approvalCardMaxTitle, false},
			{"link_label", it.LinkLabel, approvalCardMaxTitle, false},
			{"note", it.Note, approvalCardMaxText, false},
		} {
			if err := cardText(at+"."+f.name, f.v, f.max, f.required); err != nil {
				return err
			}
		}
		if it.Key != "" {
			if _, dup := keys[it.Key]; dup {
				return fmt.Errorf("%s.key repeats an earlier item's key", at)
			}
			keys[it.Key] = it.Status
		}
		if it.Link != "" {
			if err := cardLink(at+".link", it.Link); err != nil {
				return err
			}
		} else if it.LinkLabel != "" {
			return fmt.Errorf("%s.link_label names no link", at)
		}
		n, err := validateCardRows(at, it.Changes, it.Settings, it.Flags)
		if err != nil {
			return err
		}
		rows += n
	}
	if rows > approvalCardMaxRows {
		return fmt.Errorf("%d rows across the card, over the %d-row limit", rows, approvalCardMaxRows)
	}
	// The outcome sets the card's tone, so it may not contradict the rows: a
	// green "done" card never carries a failed, mismatched or unclear record,
	// and a red "failed" card never claims a record was applied.
	notDone := counts[resultItemFailed] + counts[resultItemMismatch] + counts[resultItemUnclear]
	applied := counts[resultItemDone] + counts[resultItemMismatch]
	switch {
	case c.Outcome == resultOutcomeDone && notDone > 0:
		return errors.New(`outcome "done" with a failed, mismatched or unclear item`)
	case c.Outcome == resultOutcomeFailed && applied > 0:
		return errors.New(`outcome "failed" with an applied item`)
	}
	if c.Retry != nil {
		if err := validateResultRetry(c.Retry, keys); err != nil {
			return err
		}
	}
	return nil
}

// validateResultRetry holds the retry list to the rows: every key names an
// item of the card, once, and that item is confirmed not applied (failed) or
// never attempted (not_run). An unclear or mismatched record is never offered
// for retry: for a create it may exist, and running it again could make a
// second one.
func validateResultRetry(r *ApprovalResultCardRetry, items map[string]string) error {
	if len(r.Keys) == 0 {
		return errors.New("retry.keys is empty; leave retry out instead")
	}
	if len(r.Keys) > approvalResultMaxRetryKeys {
		return fmt.Errorf("retry has %d keys, over the %d limit", len(r.Keys), approvalResultMaxRetryKeys)
	}
	seen := make(map[string]bool, len(r.Keys))
	for i, k := range r.Keys {
		at := fmt.Sprintf("retry.keys[%d]", i)
		if err := cardText(at, k, approvalResultMaxKey, true); err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("%s repeats a key", at)
		}
		seen[k] = true
		status, ok := items[k]
		if !ok {
			return fmt.Errorf("%s names no item of the card", at)
		}
		if status != resultItemFailed && status != resultItemNotRun {
			return fmt.Errorf("%s names a %s item; only failed or not_run items may be retried", at, status)
		}
	}
	return nil
}

// validateCardRows checks an item's changes, settings and flags under the
// describer card's rules, and returns how many rows they add.
func validateCardRows(at string, changes []ApprovalCardChange, settings []ApprovalCardSetting, flags []ApprovalCardFlag) (int, error) {
	if len(changes) > approvalCardMaxRowsPerSet || len(settings) > approvalCardMaxRowsPerSet {
		return 0, fmt.Errorf("%s has more than %d changes or settings", at, approvalCardMaxRowsPerSet)
	}
	if len(flags) > approvalCardMaxFlags {
		return 0, fmt.Errorf("%s has more than %d flags", at, approvalCardMaxFlags)
	}
	for j, ch := range changes {
		cat := fmt.Sprintf("%s.changes[%d]", at, j)
		if err := cardText(cat+".label", ch.Label, approvalCardMaxTitle, true); err != nil {
			return 0, err
		}
		if ch.After == nil {
			return 0, fmt.Errorf("%s.after is required", cat)
		}
		if err := cardText(cat+".after", *ch.After, approvalCardMaxText, false); err != nil {
			return 0, err
		}
		if ch.Before != nil {
			if err := cardText(cat+".before", *ch.Before, approvalCardMaxText, false); err != nil {
				return 0, err
			}
		}
	}
	for j, st := range settings {
		sat := fmt.Sprintf("%s.settings[%d]", at, j)
		if err := cardText(sat+".label", st.Label, approvalCardMaxTitle, true); err != nil {
			return 0, err
		}
		if err := cardText(sat+".value", st.Value, approvalCardMaxText, false); err != nil {
			return 0, err
		}
	}
	for j, f := range flags {
		fat := fmt.Sprintf("%s.flags[%d]", at, j)
		if !approvalCardFlagCode.MatchString(f.Code) {
			return 0, fmt.Errorf("%s.code is not a short lowercase code", fat)
		}
		if err := cardText(fat+".label", f.Label, approvalCardMaxTitle, true); err != nil {
			return 0, err
		}
	}
	return len(changes) + len(settings) + len(flags), nil
}

// extractResultCard finds the `result_card` of a tool result and returns its
// canonical JSON. ok is false with an empty reason when the result simply
// carries none (not a JSON object, or no such key): the normal case for a
// result that is not a card, which is not worth a log line. A card that is
// present but refused returns its reason.
func extractResultCard(text string) (canonical []byte, ok bool, reason string) {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '{' || len(text) > approvalResultMaxScanBytes {
		return nil, false, ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &top); err != nil {
		return nil, false, ""
	}
	raw, present := top[resultCardKey]
	if !present {
		return nil, false, ""
	}
	_, canonical, err := parseApprovalResultCard(bytes.TrimSpace(raw))
	if err != nil {
		return nil, false, redactedForLog(err)
	}
	if tools.RedactionWouldAlter(canonical) {
		return nil, false, "the card carries a secret-like value"
	}
	return canonical, true, ""
}

// recordApprovalResultCard stores the result card of an approved call of an
// opted-in tool, from the GOVERNED text (redaction, PII and guardrail
// screening applied, size bounding not: a long batch result is still read
// whole). It returns the canonical card, or nil when there is none or it
// could not be stored. Never fails the approval.
func (s *Server) recordApprovalResultCard(ctx context.Context, user string, approval *store.Approval, governed string) json.RawMessage {
	if approval == nil {
		return nil
	}
	return storeApprovalResultCard(ctx, s.store, user, approval.ID, approval.ToolName, governed)
}

// storeApprovalResultCard is the shared half of recording a result card: an
// approved call (recordApprovalResultCard) and a notify-mode record
// (approvalStager.RecordActionResult) both read the card from the call's
// governed result and write it once with SetApprovalResultCard.
func storeApprovalResultCard(ctx context.Context, st chatStore, user, approvalID, toolName, governed string) json.RawMessage {
	if !agentcore.ApprovalResultCard(toolName) {
		return nil
	}
	card, ok, reason := extractResultCard(governed)
	if !ok {
		if reason != "" {
			log.Printf("approval result card: %q result card refused: %q", toolName, reason) //nolint:gosec // G706: every value is rendered with %q, which escapes CR/LF; the reason is already redacted.
		}
		return nil
	}
	stored, err := st.SetApprovalResultCard(ctx, user, approvalID, string(card))
	if err != nil || !stored {
		log.Printf("approval result card: %q: not stored (stored=%v, err=%q)", toolName, stored, redactedForLog(err)) //nolint:gosec // G706: strings are rendered with %q, which escapes CR/LF; the error is redacted.
		return nil
	}
	return card
}

// RecordActionResult implements agentcore.ActionResultRecorder: the call a
// notify-mode record announced has returned. For a tool in
// critical_tool_result_cards whose call succeeded, the outcome card its result
// carries is stored on the record and pushed to the live card as
// tool.action_record_card. Anything else changes nothing: the record stays as
// RecordAction wrote it.
func (a *approvalStager) RecordActionResult(toolName, toolCallID, resultText string, succeeded bool) {
	a.recordMu.Lock()
	approvalID := a.recordIDs[toolCallID]
	delete(a.recordIDs, toolCallID)
	a.recordMu.Unlock()
	if approvalID == "" || !succeeded {
		return
	}
	card := storeApprovalResultCard(a.ctx, a.store, a.userEmail, approvalID, toolName, resultText)
	if card == nil {
		return
	}
	a.sink.Emit("tool.action_record_card", map[string]any{
		"approval_id": approvalID,
		"tool":        toolName,
		"result_card": card,
	})
}

// approvalResultCardPayload is the result card a client receives for a
// settled approval row, or nil. It is served only on a row whose outcome is a
// success fleet recorded (approved, is_err false: a finished call or a notify
// record), and re-validated on every read, so a row written by another
// version or edited by hand never puts an off-schema card on the wire, and a
// failed or unknown outcome never shows a card.
func (s *Server) approvalResultCardPayload(a *store.Approval) json.RawMessage {
	if a == nil || a.ResultCardJSON == "" || a.Status != "approved" {
		return nil
	}
	if flags := s.approvalOutcomeFlags(a); flags["is_err"] != false {
		return nil
	}
	_, canonical, err := parseApprovalResultCard([]byte(a.ResultCardJSON))
	if err != nil || tools.RedactionWouldAlter(canonical) {
		return nil
	}
	return canonical
}

// withApprovalResultCard adds the row's result card to a client payload when
// it has one. Without one the payload is unchanged.
func (s *Server) withApprovalResultCard(payload map[string]any, a *store.Approval) map[string]any {
	if card := s.approvalResultCardPayload(a); card != nil {
		payload["result_card"] = card
	}
	return payload
}
