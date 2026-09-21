package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// openrouterServedProvider extracts the upstream that ACTUALLY served a step
// from OpenRouter's provider metadata ("" when absent). upstreamPinFor states a
// preference; this is the outcome, and the two diverge exactly when a soft pin
// falls back. Without it a response degraded by a fallback route is
// indistinguishable from the model itself being bad, which is precisely the
// question you need answered when a turn comes back wrong.
func openrouterServedProvider(metadata fantasy.ProviderMetadata) string {
	raw, ok := metadata[openrouter.Name]
	if !ok {
		return ""
	}
	opts, ok := raw.(*openrouter.ProviderMetadata)
	if !ok {
		return ""
	}
	return strings.TrimSpace(opts.Provider)
}

// preferredUpstreamFor returns the upstream name a slug is pinned to (""
// when the family has no canonical pin). Read alongside the served provider to
// detect a fallback.
func preferredUpstreamFor(modelSlug string) string {
	matchSlug := strings.TrimPrefix(modelSlug, "~")
	for _, c := range canonicalUpstream {
		if strings.HasPrefix(matchSlug, c.prefix) {
			return c.name
		}
	}
	return ""
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
// recipients/subject/body and the attachment set. Returns ok=false when the
// args lack the fields the fingerprint needs.
//
// Attachments are part of the identity on purpose. A production run sent a
// weekly report whose first send failed on an attachment path, then sent the
// same body WITHOUT the CSV (queued), then retried WITH the CSV — and the guard
// suppressed that corrective resend as a duplicate, so the client got the
// report without its deliverable and the run dead-lettered on verification.
// An email that carries a file is not the same email as one that does not.
// Attachments are keyed by their cleaned path (case and directories preserved,
// sorted), and inline attachments additionally by the content id the body
// references them through: two workspace files that differ only by directory
// or case can hold entirely different bytes, and an inline image re-sent under
// a corrected cid is a materially different email. The guard therefore errs
// toward letting a differently-referenced attachment send rather than
// silently suppressing a corrective resend — a byte-identical loop still
// dedupes exactly as before.
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
		"attachments=" + joinIdentities(attachmentNames(args["attachments"])),
		"inline=" + joinIdentities(attachmentNames(args["inline_attachments"])),
	}, "|")
	return hashString(fingerprintSource), true
}

// joinIdentities joins the (already per-component hashed) identities; the
// entries are fixed-width hex, so the list delimiter can never be forged by
// path or cid content.
func joinIdentities(ids []string) string {
	return strings.Join(ids, ",")
}

// attachmentNames normalizes a send_email attachments argument into a sorted
// list of identities. The wire shape is a list of objects with a "path" key
// (the form tools.MaterializeAttachmentPaths consumes) and, for inline
// attachments, a "cid" or "content_id"; bare strings and a single string are
// accepted too, since models emit both. The identity is the cleaned path with
// case and directory components intact (files that differ only there can hold
// different bytes), prefixed by the content id when one is present (the body's
// cid: reference decides whether the recipient sees the image at all).
func attachmentNames(value interface{}) []string {
	type entry struct{ path, cid string }
	var raw []entry
	switch typed := value.(type) {
	case string:
		raw = []entry{{path: typed}}
	case []interface{}:
		for _, item := range typed {
			switch e := item.(type) {
			case string:
				raw = append(raw, entry{path: e})
			case map[string]interface{}:
				p, _ := e["path"].(string)
				if p == "" {
					// The inline shape also accepts the "file" alias
					// (httpapi expandCidImagesToDataURLs reads both).
					p, _ = e["file"].(string)
				}
				cid, _ := e["cid"].(string)
				if cid == "" {
					cid, _ = e["content_id"].(string)
				}
				raw = append(raw, entry{path: p, cid: cid})
			}
		}
	case []string:
		for _, p := range typed {
			raw = append(raw, entry{path: p})
		}
	}
	names := make([]string, 0, len(raw))
	for _, e := range raw {
		p := strings.TrimSpace(e.path)
		if p == "" {
			continue
		}
		// Each component is hashed on its own before they are combined, so no
		// character inside a cid or a path can move the boundary between them.
		id := hashString(filepath.Clean(p))
		if cid := strings.TrimSpace(e.cid); cid != "" {
			id = hashString(cid) + ":" + id
		}
		names = append(names, id)
	}
	sort.Strings(names)
	return names
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

// summarizeForConsole clamps text to a single-line preview (200 bytes) for
// log.Printf and event payloads.
func summarizeForConsole(text string) string {
	const maxLen = 200
	text = strings.TrimSpace(text)
	if text == "" {
		return "<empty>"
	}
	if len(text) <= maxLen {
		return text
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
