package ms365mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	trackingParams = map[string]bool{
		"utm_source": true, "utm_medium": true, "utm_campaign": true, "utm_term": true, "utm_content": true, "utm_id": true,
		"mc_cid": true, "mc_eid": true,
		"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "twclid": true, "igshid": true, "yclid": true,
		"_hsenc": true, "_hsmi": true, "hsctatracking": true,
		"mkt_tok": true, "vero_id": true, "oly_enc_id": true, "oly_anon_id": true, "rb_clickid": true, "s_kwcid": true,
	}

	angleBracketURLRe  = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	bareURLRe          = regexp.MustCompile(`https?://[^\s<>)"'\]]+`)
	multiBlankLinesRe  = regexp.MustCompile(`\n{3,}`)
	originalMessageRe  = regexp.MustCompile(`(?i)^-{2,}\s*(Original Message|Forwarded message)\s*-{2,}`)
	outlookHeaderBlkRe = regexp.MustCompile(`(?i)^From:\s+.+`)
	outlookHeaderSubRe = regexp.MustCompile(`(?i)^(Sent|Date|To|Subject):`)
	onWroteSingleRe    = regexp.MustCompile(`(?i)^On\s+.+wrote:\s*$`)
	onWroteStartRe     = regexp.MustCompile(`(?i)^On\s+.+`)
	wroteEndRe         = regexp.MustCompile(`(?i)wrote:\s*$`)
	proofpointV2Re     = regexp.MustCompile(`[-_][0-9A-Fa-f]{2}`)
)

func unwrapURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())

	if strings.HasSuffix(host, ".safelinks.protection.outlook.com") {
		if inner := u.Query().Get("url"); inner != "" {
			return stripTrackingParams(inner)
		}
	}

	if host == "urldefense.proofpoint.com" {
		if d := u.Query().Get("u"); d != "" {
			decoded := proofpointV2Re.ReplaceAllStringFunc(d, func(m string) string {
				if strings.HasPrefix(m, "_") {
					return "/"
				}
				return "%" + m[1:]
			})
			if unescaped, err := url.QueryUnescape(decoded); err == nil {
				return stripTrackingParams(unescaped)
			}
		}
	}

	if host == "urldefense.com" && strings.HasPrefix(u.Path, "/v3/__") {
		rest := strings.TrimPrefix(u.Path, "/v3/__")
		if endIdx := strings.Index(rest, "__;"); endIdx >= 0 {
			encodedURL := rest[:endIdx]
			after := rest[endIdx+3:]
			tokenPart := strings.Split(after, "!")[0]
			var tokenBytes []byte
			if decoded, err := base64.StdEncoding.DecodeString(tokenPart); err == nil {
				tokenBytes = decoded
			} else if decoded, err := base64.RawStdEncoding.DecodeString(tokenPart); err == nil {
				tokenBytes = decoded
			}
			offset := 0
			var out strings.Builder
			for i := 0; i < len(encodedURL); i++ {
				if encodedURL[i] == '*' && offset < len(tokenBytes) {
					out.WriteByte(tokenBytes[offset])
					offset++
				} else {
					out.WriteByte(encodedURL[i])
				}
			}
			return stripTrackingParams(out.String())
		}
	}

	if (host == "www.google.com" || host == "google.com") && u.Path == "/url" {
		q := u.Query()
		if target := q.Get("q"); strings.HasPrefix(target, "http") {
			return stripTrackingParams(target)
		}
		if target := q.Get("url"); strings.HasPrefix(target, "http") {
			return stripTrackingParams(target)
		}
	}

	return stripTrackingParams(raw)
}

func stripTrackingParams(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		if trackingParams[strings.ToLower(k)] {
			q.Del(k)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func cleanMailText(text string, stripQuotes bool) string {
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	text = angleBracketURLRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		return unwrapURL(inner)
	})
	text = bareURLRe.ReplaceAllStringFunc(text, unwrapURL)

	lines := strings.Split(text, "\n")
	if stripQuotes {
		cutIdx := len(lines)
		for i := 0; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			if originalMessageRe.MatchString(trimmed) {
				cutIdx = i
				break
			}
			if outlookHeaderBlkRe.MatchString(trimmed) && i+1 < len(lines) && outlookHeaderSubRe.MatchString(strings.TrimSpace(lines[i+1])) {
				cutIdx = i
				break
			}
			if onWroteSingleRe.MatchString(trimmed) {
				cutIdx = i
				break
			}
			if onWroteStartRe.MatchString(trimmed) && i+1 < len(lines) && wroteEndRe.MatchString(strings.TrimSpace(lines[i+1])) {
				cutIdx = i
				break
			}
		}
		lines = lines[:cutIdx]
		filtered := lines[:0]
		for _, l := range lines {
			if !strings.HasPrefix(strings.TrimSpace(l), ">") {
				filtered = append(filtered, l)
			}
		}
		lines = filtered
	}

	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	joined := strings.TrimSpace(multiBlankLinesRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	return joined
}

func formatRecipient(r any) string {
	m, ok := r.(map[string]any)
	if !ok {
		return ""
	}
	ea, _ := m["emailAddress"].(map[string]any)
	if ea == nil {
		return ""
	}
	name, _ := ea["name"].(string)
	addr, _ := ea["address"].(string)
	if name != "" && name != addr {
		return fmt.Sprintf("%s <%s>", name, addr)
	}
	return addr
}

func formatRecipientList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s := formatRecipient(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (h *Handler) executeReadMailText(ctx context.Context, token string, args map[string]any) (any, error) {
	messageIDs := parseMessageIDsArg(args["messageIds"])
	folderID, _ := args["folderId"].(string)
	if len(messageIDs) == 0 && strings.TrimSpace(folderID) == "" {
		return map[string]any{
			"error": "Provide either messageIds (single ID or array of IDs) or folderId (e.g. \"inbox\" or a folder ID from list-mail-folders).",
		}, nil
	}

	maxChars := 3000
	if rawMax, ok := intArg(args, "maxChars"); ok && rawMax > 0 {
		maxChars = clampInt(rawMax, 1, 300000)
	}
	stripQuoted := true
	if rawStrip, ok := args["stripQuoted"].(bool); ok {
		stripQuoted = rawStrip
	}

	messages, err := h.fetchMessagesForReadMailText(ctx, token, messageIDs, folderID, args)
	if err != nil {
		return nil, err
	}
	return formatReadMailTextResults(messages, maxChars, stripQuoted), nil
}

func parseMessageIDsArg(rawIDs any) []string {
	var messageIDs []string
	switch v := rawIDs.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			messageIDs = []string{strings.TrimSpace(v)}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				messageIDs = append(messageIDs, strings.TrimSpace(s))
			}
		}
	}
	return messageIDs
}

func (h *Handler) fetchMessagesForReadMailText(ctx context.Context, token string, messageIDs []string, folderID string, args map[string]any) ([]map[string]any, error) {
	selectFields := "id,subject,from,toRecipients,ccRecipients,receivedDateTime,isRead,hasAttachments,body"
	extraHeaders := map[string]string{
		"Prefer": `outlook.body-content-type="text"`,
	}

	if len(messageIDs) > 0 {
		var messages []map[string]any
		for _, id := range messageIDs {
			path := fmt.Sprintf("/me/messages/%s", encodeGraphPathSegment(id))
			q := url.Values{}
			q.Set("$select", selectFields)
			q.Set("$expand", defaultMailAttachmentExpand)
			res, err := h.graphRequest(ctx, token, "GET", path, q, nil, extraHeaders)
			if err != nil {
				messages = append(messages, map[string]any{
					"id":    id,
					"error": err.Error(),
				})
				continue
			}
			if m, ok := res.(map[string]any); ok {
				messages = append(messages, m)
			}
		}
		return messages, nil
	}

	filter, _ := args["filter"].(string)
	search, _ := args["search"].(string)
	limit := 20
	if rawTop, ok := intArg(args, "top"); ok && rawTop > 0 {
		limit = clampInt(rawTop, 1, 50)
	} else if rawLimit, ok := intArg(args, "limit"); ok && rawLimit > 0 {
		limit = clampInt(rawLimit, 1, 50)
	}

	path := fmt.Sprintf("/me/mailFolders/%s/messages", encodeGraphPathSegment(folderID))
	if strings.EqualFold(strings.TrimSpace(folderID), "inbox") {
		path = "/me/mailFolders/inbox/messages"
	}
	q := url.Values{}
	q.Set("$select", selectFields)
	q.Set("$top", fmt.Sprintf("%d", limit))
	q.Set("$expand", defaultMailAttachmentExpand)
	if strings.TrimSpace(filter) != "" {
		q.Set("$filter", filter)
	}
	if strings.TrimSpace(search) != "" {
		q.Set("$search", normalizeMailSearchQuery(search))
		extraHeaders["ConsistencyLevel"] = "eventual"
	} else {
		q.Set("$orderby", "receivedDateTime desc")
	}
	res, err := h.graphRequestWithExpandFallback(ctx, token, "GET", path, q, nil, extraHeaders, true)
	if err != nil {
		return nil, err
	}
	var messages []map[string]any
	if m, ok := res.(map[string]any); ok {
		if vals, ok := m["value"].([]any); ok {
			for _, item := range vals {
				if msgMap, ok := item.(map[string]any); ok {
					messages = append(messages, msgMap)
				}
			}
		}
	}
	return messages, nil
}

func formatReadMailTextResults(messages []map[string]any, maxChars int, stripQuoted bool) []map[string]any {
	results := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if errStr, ok := msg["error"].(string); ok && errStr != "" {
			results = append(results, msg)
			continue
		}
		var rawBody string
		if bodyObj, ok := msg["body"].(map[string]any); ok {
			rawBody, _ = bodyObj["content"].(string)
		}
		cleaned := cleanMailText(rawBody, stripQuoted)
		truncated := maxChars > 0 && len(cleaned) > maxChars
		bodyText := cleaned
		if truncated {
			bodyText = cleaned[:maxChars] + "…"
		}
		toList := formatRecipientList(msg["toRecipients"])
		if toList == nil {
			toList = []string{}
		}
		ccList := formatRecipientList(msg["ccRecipients"])
		if ccList == nil {
			ccList = []string{}
		}
		entry := map[string]any{
			"id":               msg["id"],
			"subject":          msg["subject"],
			"from":             formatRecipient(msg["from"]),
			"to":               toList,
			"cc":               ccList,
			"received":         msg["receivedDateTime"],
			"receivedDateTime": msg["receivedDateTime"],
			"isRead":           msg["isRead"],
			"hasAttachments":   msg["hasAttachments"],
			"text":             bodyText,
			"body":             bodyText,
			"chars":            len(bodyText),
			"truncated":        truncated,
		}
		if atts := summarizeAttachments(msg["attachments"]); len(atts) > 0 {
			entry["attachments"] = atts
		}
		if truncated {
			entry["fullLength"] = len(cleaned)
		}
		results = append(results, entry)
	}
	return results
}
