package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// Shared orchestration helpers extracted from chat + cutlass orchestration.go.
// openrouterCost is byte-identical between the two repos. hashString lives in
// chat's orchestration.go; cutlass uses the same shape. sendEmail* + recipient
// parsing come from cutlass (its sendEmailSucceeded is the JSON-parsing version
// the lifted orchestration tests rely on).

const nilStringValue = "<nil>"

// hashString returns a short stable hash of s, used as the dedup / repeat-call
// key. 16 hex chars is plenty to avoid collisions on tool-arg payloads.
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// openrouterCost extracts the USD cost from OpenRouter's provider metadata.
// Byte-identical between chat and cutlass.
func openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	raw, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}
	opts, ok := raw.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// sendEmailSucceeded reports whether a send_email tool result indicates the
// send was queued (SendGrid returns status_code 202). cutlass's JSON-parsing
// form: any non-2xx / error payload is a failure.
func sendEmailSucceeded(result string) bool {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return false
	}
	if _, hasError := payload["error"]; hasError {
		return false
	}
	statusValue, hasStatus := payload["status_code"]
	if !hasStatus {
		return false
	}
	statusCode, ok := statusValue.(float64)
	if !ok {
		return false
	}
	return int(statusCode) == 202
}

// emailDedupKey returns the duplicate-send key for an email tool call.
// Prefers the semantic fingerprint (normalized recipients/subject/body) so
// cosmetic JSON differences cannot bypass the duplicate-send guard. Falls back
// to a raw-bytes hash.
func emailDedupKey(rawInput string) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(rawInput), &args); err == nil {
		if fp, ok := sendEmailFingerprint(args); ok {
			return fp
		}
	}
	return hashString(rawInput)
}

// sendEmailFingerprint builds a semantic fingerprint from normalized
// recipients/subject/body. Returns ok=false when the args lack the fields the
// fingerprint needs.
func sendEmailFingerprint(args map[string]interface{}) (string, bool) {
	toEmails := parseRecipientArg(args["to_email"])
	if len(toEmails) == 0 {
		return "", false
	}
	subject, _ := args["subject"].(string)
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", false
	}
	content, _ := args["content"].(string)
	content = strings.TrimSpace(content)
	contentFile, _ := args["content_file"].(string)
	contentFile = strings.TrimSpace(contentFile)
	if content == "" && contentFile == "" {
		return "", false
	}
	ccEmails := parseRecipientArg(args["cc_emails"])
	bccEmails := parseRecipientArg(args["bcc_emails"])
	bodyReference := "content_hash:" + hashString(content)
	if content == "" {
		bodyReference = "content_file:" + contentFile
	}
	fingerprintSource := strings.Join([]string{
		"to=" + strings.Join(toEmails, ","),
		"cc=" + strings.Join(ccEmails, ","),
		"bcc=" + strings.Join(bccEmails, ","),
		"subject=" + strings.ToLower(subject),
		bodyReference,
	}, "|")
	return hashString(fingerprintSource), true
}

func parseRecipientArg(value interface{}) []string {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case string:
		return parseEmailList(typed)
	case []interface{}:
		all := make([]string, 0, len(typed))
		for _, item := range typed {
			all = append(all, parseRecipientArg(item)...)
		}
		sort.Strings(all)
		return all
	case []string:
		all := make([]string, 0, len(typed))
		for _, item := range typed {
			all = append(all, parseEmailList(item)...)
		}
		sort.Strings(all)
		return all
	default:
		return nil
	}
}

func parseEmailList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	})
	unique := make(map[string]struct{})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		normalized := strings.ToLower(strings.TrimSpace(part))
		if normalized == "" {
			continue
		}
		if _, exists := unique[normalized]; exists {
			continue
		}
		unique[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result
}

// summarizeForLog clamps text to maxLen with a head/tail window, used by the
// retry logger when mirroring a provider error into the session log.
func summarizeForLog(text string, maxLen int) string {
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	headLen := maxLen / 2
	tailLen := maxLen - headLen
	if headLen < 200 {
		headLen = 200
	}
	if tailLen < 200 {
		tailLen = 200
	}
	if headLen+tailLen >= len(text) {
		return text
	}
	omitted := len(text) - (headLen + tailLen)
	return fmt.Sprintf("%s ... [truncated %d bytes for log readability] ... %s",
		text[:headLen], omitted, text[len(text)-tailLen:])
}

// summarizeForConsole clamps text to a single-line preview for log.Printf.
func summarizeForConsole(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "<empty>"
	}
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	if maxLen < 32 {
		maxLen = 32
	}
	return text[:maxLen-3] + "..."
}

// truncate clamps a string for SSE / log payloads (chat's helper).
func truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// parseBoolEnvValue is a tolerant bool parser used where strconv.ParseBool's
// strictness would reject "yes"/"on"/etc.
func parseBoolEnvValue(raw string) (val, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return false, false
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		if b, err := strconv.ParseBool(raw); err == nil {
			return b, true
		}
		return false, false
	}
}

// taskTrackerSnapshot is the parsed state of the scheduled-mode task tracker,
// consulted by checkFinishEnforcement.
type taskTrackerSnapshot struct {
	Seen       bool   `json:"seen"`
	Total      int    `json:"total"`
	Todo       int    `json:"todo"`
	InProgress int    `json:"in_progress"`
	Done       int    `json:"done"`
	ActiveTask string `json:"active_task"`
	Summary    string `json:"summary,omitempty"`
}

// toolsBool coerces an interface{} argument to bool, defaulting to false.
func toolsBool(value interface{}) bool {
	flag, _ := value.(bool)
	return flag
}
