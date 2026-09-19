package chattui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

// frozenApprovalReview is the last thing a terminal user sees before a
// decision: the complete execution arguments (ArgsJSON), JSON-escaped so
// C0/C1/ESC/CR and bidi controls cannot drive the TTY. Local schedule_task
// edits are overlaid so the printed object is what the POST will send.
// Incomplete, typed-nil, or truncated snapshots are not a review.
func frozenApprovalReview(a pendingApproval) string {
	if !a.reviewComplete() {
		return "INCOMPLETE frozen argument review: the server did not provide a complete snapshot of the arguments that will execute. Approval is refused."
	}
	b, err := json.MarshalIndent(a.executionArgs(), "", "  ")
	if err != nil {
		return "INCOMPLETE frozen argument review: the snapshot could not be encoded. Approval is refused."
	}
	return "Frozen argument review (complete execution args):\n" + sanitizeTerminalText(string(b))
}

func (a pendingApproval) reviewComplete() bool {
	return a.frozenPresent && a.frozenComplete && a.frozenArgs != nil
}

func (a pendingApproval) refuseApprove() string {
	if a.executing {
		return ""
	}
	if !a.reviewComplete() {
		return "Refusing to approve: complete frozen arguments are unavailable or truncated. Deny, reload, or wait for a restage."
	}
	if _, err := json.Marshal(a.executionArgs()); err != nil {
		return "Refusing to approve: frozen arguments could not be encoded."
	}
	return ""
}

// executionArgs is the object that will be sent (frozen snapshot plus any
// local schedule_task edits). The server still revalidates edits.
func (a pendingApproval) executionArgs() map[string]any {
	out := make(map[string]any, len(a.frozenArgs)+3)
	for k, v := range a.frozenArgs {
		out[k] = v
	}
	if a.edits == nil {
		return out
	}
	if a.edits.Name != nil {
		out["name"] = *a.edits.Name
	}
	if a.edits.Prompt != nil {
		out["prompt"] = *a.edits.Prompt
	}
	if a.edits.Cron != nil {
		out["cron"] = *a.edits.Cron
	}
	return out
}

var errTrailingJSON = errors.New("trailing JSON")

// decodeJSONNumbers decodes one JSON value with json.Number so integers above
// 2^53 and exponent literals survive. A second token is rejected so this is
// not more lenient than json.Unmarshal.
func decodeJSONNumbers(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); err {
	case io.EOF:
		return nil
	case nil:
		return errTrailingJSON
	default:
		return err
	}
}

// parseFrozenArgs copies the server's frozen_args object. Wire JSON (SSE/GET)
// arrives as json.RawMessage and is decoded with json.Number. A missing or
// typed-nil payload is not present (legacy servers); a present object with
// complete:true and an object args is the only approvable snapshot.
func parseFrozenArgs(v any) (args map[string]any, complete, present bool) {
	switch t := v.(type) {
	case nil:
		return nil, false, false
	case json.RawMessage:
		return parseFrozenArgsRaw(t)
	case map[string]any:
		if t == nil {
			return nil, false, false
		}
		return frozenArgsFromMap(t)
	default:
		return nil, false, false
	}
}

func parseFrozenArgsRaw(raw json.RawMessage) (args map[string]any, complete, present bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, false
	}
	var v any
	if err := decodeJSONNumbers(raw, &v); err != nil {
		return nil, false, true
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, false, true
	}
	return frozenArgsFromMap(m)
}

func frozenArgsFromMap(m map[string]any) (args map[string]any, complete, present bool) {
	complete, _ = m["complete"].(bool)
	raw, hasArgs := m["args"]
	if !hasArgs || raw == nil {
		return nil, false, true
	}
	args, ok := raw.(map[string]any)
	if !ok || args == nil {
		return nil, false, true
	}
	return args, complete, true
}

// approvalSummaryLine renders the server's tool.approval_required summary
// payload as ONE readable line for the terminal. The summary shape is
// tool-specific (built by httpapi.summarizeApprovalInput): known tools get a
// tailored line, anything else falls back to sorted key=value pairs. Display
// only — the staged arguments on the server stay the execution source of
// truth, exactly like the web card.
func approvalSummaryLine(tool string, summary any) string {
	return sanitizeTerminal(formatApprovalSummary(tool, summary))
}

func formatApprovalSummary(tool string, summary any) string {
	m, _ := summary.(map[string]any)
	if m == nil {
		return ""
	}
	if strings.HasSuffix(tool, "_send_email") {
		tool = "send_email"
	}
	switch tool {
	case "schedule_task":
		name := strField(m, "name")
		when := "runs as soon as a worker is free"
		if cron := strField(m, "cron"); cron != "" {
			when = "cron " + cron
			if n, ok := m["runs_per_month"].(float64); ok {
				when += fmt.Sprintf(" (≈%d runs/month)", int(n))
			}
		} else if runAt := strField(m, "run_at"); runAt != "" {
			when = "one-time at " + runAt
		}
		line := fmt.Sprintf("%q — %s", name, when)
		if preview := strField(m, "prompt_preview"); preview != "" {
			line += " — " + truncateRunes(preview, 80)
		}
		return line
	case "manage_tasks":
		action := strField(m, "action")
		match := strField(m, "match")
		if action != "" {
			return truncateRunes(strings.TrimSpace(action+" "+match), 140)
		}
	case "send_email", "preview_email":
		to := displayValue(m["to"])
		subject := strField(m, "subject")
		if to == "null" && subject == "" {
			return genericSummaryLine(m)
		}
		return fmt.Sprintf("to %s — %q", to, truncateRunes(subject, 80))
	case "bash":
		if cmd := strField(m, "command"); cmd != "" {
			return truncateRunes(cmd, 120)
		}
	case "suggest_advanced_model":
		if slug := strField(m, "recommend_model"); slug != "" {
			return "switch to " + slug
		}
	}
	return genericSummaryLine(m)
}

// genericSummaryLine is the honest floor for a tool with no tailored line:
// the summary's scalar fields as sorted key=value pairs (nested values
// compacted), truncated to card-friendly size — mirroring the web's generic
// approval card.
func genericSummaryLine(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+displayValue(m[k]))
	}
	return truncateRunes(strings.Join(parts, " "), 160)
}

func displayValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case float64, bool:
		return fmt.Sprintf("%v", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

func strField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// sanitizeTerminal keeps one-liners from driving the terminal: C0/C1/DEL and
// bidi controls become visible \uXXXX escapes so a staged recipient or
// command cannot clear the viewport, inject OSC writes, or reverse text.
func sanitizeTerminal(s string) string {
	return sanitizeTerminalRunes(s, false)
}

// sanitizeTerminalText is the same neutralization for multi-line terminal
// sinks (frozen JSON review, result_text, errors). Newlines and tabs stay so
// JSON indent remains readable; every other control is escaped.
func sanitizeTerminalText(s string) string {
	return sanitizeTerminalRunes(s, true)
}

func sanitizeTerminalRunes(s string, keepSpaceControls bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case keepSpaceControls && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\t' && !keepSpaceControls:
			b.WriteByte(' ')
		case r < 32 || r == 127 || (r >= 0x80 && r <= 0x9f) || unicode.Is(unicode.Bidi_Control, r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
