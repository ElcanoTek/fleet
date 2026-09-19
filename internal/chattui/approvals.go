package chattui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Email approval is a review of frozen server arguments, not model prose. JSON
// preserves every recipient, attachment and body byte without interpreting HTML,
// Markdown or terminal control characters. The server summary itself may still
// cap content at 1 MiB (content_overflow); that is not a complete review and
// approval is refused until the agent restages a smaller body.
func emailApprovalReview(tool string, summary any) string {
	if tool != "send_email" && tool != "preview_email" && !strings.HasSuffix(tool, "_send_email") {
		return ""
	}
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return "Email review unavailable. Reload approvals before deciding."
	}
	if emailSummaryOverflow(summary) {
		return "INCOMPLETE email review: the frozen body exceeded the 1 MiB summary cap, so the tail is not shown. Approval is refused until the agent restages a smaller body.\n" + string(b)
	}
	return "Frozen email review (all recipients, content and attachments):\n" + string(b)
}

func emailSummaryOverflow(summary any) bool {
	m, _ := summary.(map[string]any)
	if m == nil {
		return false
	}
	overflow, _ := m["content_overflow"].(bool)
	return overflow
}

// approvalSummaryLine renders the server's tool.approval_required summary
// payload as ONE readable line for the terminal. The summary shape is
// tool-specific (built by httpapi.summarizeApprovalInput): known tools get a
// tailored line, anything else falls back to sorted key=value pairs. Display
// only — the staged arguments on the server stay the execution source of
// truth, exactly like the web card.
func approvalSummaryLine(tool string, summary any) string {
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
