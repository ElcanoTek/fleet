package agentcore

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

// Default freshness lookback for "latest" / "check again" mailbox searches
// (#1026). Search [runtime_today-2d, runtime_today] unless the user asked
// for a narrower historical range.
const defaultFreshnessLookbackDays = 2

// runtimeNow is the clock used for per-run date context and stale-window
// annotation. Production uses time.Now; tests override it.
var runtimeNow = time.Now

// RuntimeToday returns the UTC calendar date (YYYY-MM-DD) that this turn
// must treat as "today". Never derive this from conversation history,
// filenames, or prior tool arguments (#1026).
func RuntimeToday(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

// FreshnessWindow returns the inclusive [from, to] UTC dates for a
// "latest" / "check again" search: [today-lookback, today].
func FreshnessWindow(now time.Time) (from, to string) {
	today := now.UTC()
	to = today.Format("2006-01-02")
	from = today.AddDate(0, 0, -defaultFreshnessLookbackDays).Format("2006-01-02")
	return from, to
}

// RuntimeDateTurnSuffix is the per-run message-tail block that restates
// runtime_today. It lives in the evolving tail (not the cached system
// prefix) so a multi-day chat cannot stay anchored to yesterday's bounds
// even if the model ignores the system-prompt date section. Day precision
// only — same contract as the system-prompt Runtime Date Context.
func RuntimeDateTurnSuffix(now time.Time) string {
	today := RuntimeToday(now)
	from, to := FreshnessWindow(now)
	return fmt.Sprintf("## Runtime today (authoritative)\n\n"+
		"- runtime_today: %s\n"+
		"- freshness_window: %s .. %s\n"+
		"- Recompute date_from/date_to from runtime_today on this turn. Do not reuse bounds from prior turns, filenames, or conversation history.\n"+
		"- For \"today\", \"latest\", or \"check again\", search freshness_window unless the user asked for a narrower historical range.\n",
		today, from, to)
}

// appendRuntimeDateMessage appends the per-run date suffix as a trailing
// user message (same append-only pattern as user_prompt_submit hook
// context) so the prompt-cache prefix stays stable.
func appendRuntimeDateMessage(messages []fantasy.Message, now time.Time) []fantasy.Message {
	return append(messages, fantasy.NewUserMessage(RuntimeDateTurnSuffix(now)))
}

// date-upper-bound keys commonly used by mailbox / report MCP tools.
var dateToKeys = []string{"date_to", "on_or_before", "end_date", "before", "until"}

// date-lower-bound keys, included in the annotation for the model.
var dateFromKeys = []string{"date_from", "start_date", "after", "since"}

// AnnotateDateWindow appends engine-authored reminders to a tool result
// when the call's date upper bound is before runtime_today, and when a
// mailbox search returned zero matches. Empty exact search is not treated
// as proof of absence (#1026). The original result is unchanged when
// neither condition applies.
func AnnotateDateWindow(toolName, rawInput, result string, now time.Time) string {
	if result == "" {
		return result
	}
	today := RuntimeToday(now)
	from, to := FreshnessWindow(now)

	args := parseToolArgs(rawInput)
	dateTo, dateToKey, dateToOK := firstDateArg(args, dateToKeys)
	dateFrom, _, _ := firstDateArg(args, dateFromKeys)

	var notes []string
	if dateToOK && dateTo < today {
		notes = append(notes, fmt.Sprintf(
			"[fleet date-window] runtime_today=%s freshness_window=%s..%s. This call used %s=%s%s, which does not include today. Records received on runtime_today were not searched. Re-run with %s>=runtime_today (or freshness_window) unless the user explicitly asked for a historical range.",
			today, from, to, dateToKey, dateTo, formatOptionalFrom(dateFrom), dateToKey,
		))
	}
	if isMailboxSearchTool(toolName, args) && isZeroMatchResult(result) {
		notes = append(notes, "[fleet search] matches_found=0 is not proof of absence. If this was an exact sender/subject query, also run independent recipient, sender-domain, and fuzzy searches and merge by id. Do not declare a source missing until those fallbacks complete.")
	}
	if len(notes) == 0 {
		return result
	}
	return result + "\n\n" + strings.Join(notes, "\n")
}

func formatOptionalFrom(dateFrom string) string {
	if dateFrom == "" {
		return ""
	}
	return " date_from=" + dateFrom
}

func parseToolArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil
	}
	return args
}

func firstDateArg(args map[string]any, keys []string) (value, key string, ok bool) {
	if args == nil {
		return "", "", false
	}
	for _, k := range keys {
		v, present := args[k]
		if !present || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		day, parsed := parseDateBound(s)
		if !parsed {
			continue
		}
		return day, k, true
	}
	return "", "", false
}

func parseDateBound(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02"), true
		}
	}
	// date-only already handled; also accept a leading YYYY-MM-DD prefix
	// from values like "2026-08-12 23:59:59".
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t.UTC().Format("2006-01-02"), true
		}
	}
	return "", false
}

func isMailboxSearchTool(name string, args map[string]any) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "search_email") || strings.Contains(lower, "find_latest_report") {
		return true
	}
	if !strings.Contains(lower, "search") {
		return false
	}
	for _, k := range []string{"sender_contains", "recipient_contains", "subject_contains", "subject_keywords"} {
		if _, ok := args[k]; ok {
			return true
		}
	}
	return false
}

func isZeroMatchResult(result string) bool {
	var payload map[string]any
	if err := json.Unmarshal([]byte(result), &payload); err == nil {
		if n, ok := jsonNumber(payload["matches_found"]); ok {
			return n == 0
		}
		if emails, ok := payload["emails"].([]any); ok && len(emails) == 0 {
			if status, _ := payload["status"].(string); status == "success" || status == "" {
				return true
			}
		}
		return false
	}
	trimmed := strings.TrimSpace(result)
	return strings.Contains(trimmed, `"matches_found": 0`) || strings.Contains(trimmed, `"matches_found":0`)
}

func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
