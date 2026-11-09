package ms365mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMailAttachmentExpand = "attachments($select=id,name,contentType,size,isInline)"
	maxPaginationPages          = 100
	maxPaginationItems          = 5000
	maxGraphJSONBytes           = 20 << 20
)

var (
	pathPlaceholderRe            = regexp.MustCompile(`\{([^}]+)\}`)
	mailAttachmentValueTargetRe  = regexp.MustCompile(`(?i)^(/me|/users/[^/]+)/(messages|mailFolders/[^/]+/messages)/[^/]+/attachments/[^/]+/\$value$`)
	eventAttachmentValueTargetRe = regexp.MustCompile(`(?i)^(/me|/users/[^/]+)/(events|calendars/[^/]+/events)/[^/]+/attachments/[^/]+/\$value$`)
	driveItemTargetRe            = regexp.MustCompile(`(?i)^(/drives/[^/]+/items/[^/]+|(/me|/users/[^/]+|/sites/[^/]+|/groups/[^/]+)/drive/items/[^/]+|/shares/[^/]+/driveItem)(/content)?$`)
	driveItemContentTargetRe     = regexp.MustCompile(`(?i)^(/drives/[^/]+/items/[^/]+|(/me|/users/[^/]+|/sites/[^/]+|/groups/[^/]+)/drive/items/[^/]+|/shares/[^/]+/driveItem)/content$`)
	mentionTagRe                 = regexp.MustCompile(`(?i)<at\s+user-id=["']([0-9a-fA-F-]{36})["']\s*>([^<]+)</at>`)
	existingAtIDRe               = regexp.MustCompile(`(?i)<at\s+id=["'](\d+)["']`)
)

func (h *Handler) executeToolCall(ctx context.Context, r *http.Request, token, toolName string, args map[string]any) (any, bool, error) {
	_, toolsByName, catOK := h.snapshotCatalog()
	if !catOK {
		return map[string]any{"error": "Microsoft 365 tool catalog is unavailable"}, true, nil
	}
	spec, ok := toolsByName[toolName]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("Unknown tool: %s", toolName)}, true, nil
	}

	if res, isErr, handled, err := h.executeBuiltInUtilityTool(ctx, r, token, toolName, args, toolsByName); handled {
		return res, isErr, err
	}

	pathPattern := spec.PathPattern
	if toolName == "get-calendar-view" {
		if calID, ok := lookupStringArg(args, "calendarId"); ok && strings.TrimSpace(calID) != "" {
			pathPattern = "/me/calendars/{calendar-id}/calendarView"
		}
	}

	path, err := interpolateGraphPath(pathPattern, args)
	if err != nil {
		return nil, true, err
	}

	if spec.ReturnDownloadURL || toolName == "get-mail-attachment-content" {
		fallbackName, _ := lookupStringArg(args, "fileName")
		return h.mintAttachmentDownloadResultWithFallback(ctx, r, token, path, fallbackName)
	}

	query, autoExpandedAttachments := buildGraphQueryParams(spec, toolName, args)
	headers := buildGraphHeaders(spec, toolName, query, args)
	reqBody, err := buildGraphRequestBody(spec, toolName, args)
	if err != nil {
		return nil, true, err
	}

	res, respHeaders, firstPageBytes, err := h.graphRequestWithExpandFallback(ctx, token, spec.Method, path, query, reqBody, headers, autoExpandedAttachments)
	if err != nil {
		return nil, true, err
	}

	if fetchAll, ok := args["fetchAllPages"].(bool); ok && fetchAll && spec.Method == "GET" {
		res, err = h.followAllPages(ctx, token, res, firstPageBytes, headers)
		if err != nil {
			return nil, true, err
		}
	}

	return postProcessGraphResult(toolName, query, args, res, respHeaders), false, nil
}

func (h *Handler) executeBuiltInUtilityTool(ctx context.Context, r *http.Request, token, toolName string, args map[string]any, toolsByName map[string]ToolSpec) (any, bool, bool, error) {
	switch toolName {
	case "parse-teams-url":
		rawURL, _ := args["url"].(string)
		if strings.TrimSpace(rawURL) == "" {
			return map[string]any{"error": "url is required."}, true, true, nil
		}
		res := ParseTeamsURL(rawURL)
		return res, res.Error != "", true, nil

	case "read-mail-text":
		res, err := h.executeReadMailText(ctx, token, args)
		if err != nil {
			return nil, true, true, err
		}
		if m, ok := res.(map[string]any); ok {
			if _, hasErr := m["error"]; hasErr {
				return m, true, true, nil
			}
		}
		return res, false, true, nil

	case "get-download-url":
		target, _ := args["target"].(string)
		target = strings.TrimSpace(target)
		if target == "" {
			return map[string]any{"error": "target is required and must be a non-empty string."}, true, true, nil
		}
		res, isErr, err := h.executeGetDownloadURL(ctx, r, token, target, toolsByName)
		return res, isErr, true, err
	}
	return nil, false, false, nil
}

func buildGraphQueryParams(spec ToolSpec, toolName string, args map[string]any) (url.Values, bool) {
	if toolName == "list-chat-members" {
		return url.Values{}, false
	}
	query := url.Values{}
	for _, qp := range spec.QueryParams {
		val, exists := lookupQueryArg(args, qp)
		if !exists {
			continue
		}
		paramKey := qp
		if isODataShortName(qp) && !strings.HasPrefix(qp, "$") {
			paramKey = "$" + qp
		}
		setQueryParamValue(query, paramKey, val)
	}

	autoExpandedAttachments := false
	if isMailMessageTool(toolName) && query.Get("$expand") == "" {
		query.Set("$expand", defaultMailAttachmentExpand)
		autoExpandedAttachments = true
	}
	if toolName == "list-mail-attachments" && query.Get("$select") == "" {
		query.Set("$select", "id,name,contentType,size,isInline")
	}

	if rawSearch := query.Get("$search"); rawSearch != "" && isMailEndpoint(spec.PathPattern) {
		query.Set("$search", normalizeMailSearchQuery(rawSearch))
	}

	if ids := extractExtendedPropertyIDs(args["expandExtendedProperties"]); len(ids) > 0 {
		var filters []string
		for _, id := range ids {
			filters = append(filters, fmt.Sprintf("id eq '%s'", strings.ReplaceAll(id, "'", "''")))
		}
		clause := fmt.Sprintf("singleValueExtendedProperties($filter=%s)", strings.Join(filters, " or "))
		if cur := query.Get("$expand"); cur != "" {
			query.Set("$expand", cur+","+clause)
		} else {
			query.Set("$expand", clause)
		}
	}
	return query, autoExpandedAttachments
}

func extractExtendedPropertyIDs(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func setQueryParamValue(query url.Values, paramKey string, val any) {
	strVal := formatQueryParamValue(val)
	if strVal == "" {
		return
	}
	if paramKey == "$skiptoken" {
		k, v := normalizeSkipTokenValue(strVal)
		query.Set(k, v)
		return
	}
	query.Set(paramKey, strVal)
}

func buildGraphHeaders(spec ToolSpec, toolName string, query url.Values, args map[string]any) map[string]string {
	headers := map[string]string{}
	var preferParts []string

	if query.Get("$search") != "" || strings.EqualFold(query.Get("$count"), "true") {
		headers["ConsistencyLevel"] = "eventual"
	}
	if isMailEndpoint(spec.PathPattern) && query.Get("$filter") != "" && query.Get("$orderby") != "" {
		headers["ConsistencyLevel"] = "eventual"
	}

	for _, hp := range spec.HeaderParams {
		if v, ok := args[hp].(string); ok && strings.TrimSpace(v) != "" {
			if strings.EqualFold(hp, "Prefer") {
				preferParts = append(preferParts, strings.TrimSpace(v))
			} else {
				headers[hp] = strings.TrimSpace(v)
			}
		}
	}

	if spec.SupportsTimezone {
		if tz, ok := args["timezone"].(string); ok && strings.TrimSpace(tz) != "" {
			preferParts = append(preferParts, fmt.Sprintf(`outlook.timezone="%s"`, strings.ReplaceAll(strings.TrimSpace(tz), `"`, "")))
		}
	}

	if spec.Method == "GET" && isMailMessageTool(toolName) {
		preferPlain := true
		if rawPrefer, ok := args["preferPlainText"].(bool); ok {
			preferPlain = rawPrefer
		}
		if preferPlain {
			preferParts = append(preferParts, `outlook.body-content-type="text"`)
		}
	}
	if len(preferParts) > 0 {
		headers["Prefer"] = strings.Join(preferParts, ", ")
	}
	if spec.ContentType != "" {
		headers["Content-Type"] = spec.ContentType
	} else if _, isStr := args["body"].(string); isStr && isMailWriteTool(toolName) {
		headers["Content-Type"] = "text/plain"
	}
	return headers
}

func isMailWriteTool(toolName string) bool {
	switch toolName {
	case "send-mail", "create-draft-email", "create-reply-draft", "create-reply-all-draft",
		"create-forward-draft", "reply-mail-message", "reply-all-mail-message", "forward-mail-message":
		return true
	}
	return false
}

func buildGraphRequestBody(spec ToolSpec, toolName string, args map[string]any) (any, error) {
	if spec.Method != "POST" && spec.Method != "PATCH" && spec.Method != "PUT" && spec.Method != "DELETE" {
		return nil, nil
	}
	var reqBody any
	if len(spec.BodyParams) == 1 && spec.BodyParams[0] == "body" {
		if b, ok := args["body"]; ok {
			if bMap, isMap := b.(map[string]any); isMap {
				normalized := make(map[string]any, len(bMap))
				for k, v := range bMap {
					normalized[normalizeActionFieldKey(k)] = v
				}
				reqBody = normalized
			} else {
				reqBody = b
			}
		} else if spec.Method == "POST" {
			reqBody = map[string]any{}
		}
	} else if len(spec.BodyParams) > 0 {
		bodyMap := map[string]any{}
		if nested, ok := args["body"].(map[string]any); ok {
			for k, v := range nested {
				bodyMap[normalizeActionFieldKey(k)] = v
			}
		}
		for _, bp := range spec.BodyParams {
			if v, ok := lookupActionFieldArg(args, bp); ok {
				bodyMap[bp] = v
			}
		}
		reqBody = bodyMap
	}
	if toolName == "reply-to-chat-message" {
		return buildChatReplyWithQuoteBody(reqBody, args)
	} else if isTeamsMessageSendTool(toolName) {
		if bodyMap, ok := reqBody.(map[string]any); ok {
			reqBody = rewriteTeamsMentions(bodyMap)
		}
	}
	return reqBody, nil
}

var actionFieldAliases = map[string]string{
	"Comment":         "comment",
	"SendResponse":    "sendResponse",
	"Message":         "message",
	"ToRecipients":    "toRecipients",
	"DestinationId":   "destinationId",
	"ProposedNewTime": "proposedNewTime",
	"SaveToSentItems": "saveToSentItems",
}

func normalizeActionFieldKey(k string) string {
	if canonical, ok := actionFieldAliases[k]; ok {
		return canonical
	}
	return k
}

func lookupActionFieldArg(args map[string]any, canonicalKey string) (any, bool) {
	if v, ok := args[canonicalKey]; ok {
		return v, true
	}
	for pascal, camel := range actionFieldAliases {
		if camel == canonicalKey {
			if v, ok := args[pascal]; ok {
				return v, true
			}
		}
	}
	return nil, false
}

func buildChatReplyWithQuoteBody(reqBody any, args map[string]any) (map[string]any, error) {
	bodyMap, _ := reqBody.(map[string]any)
	if bodyMap == nil {
		bodyMap = map[string]any{}
	}
	msgIDs := extractChatReplyMessageIDs(bodyMap, args)
	if len(msgIDs) == 0 {
		return nil, fmt.Errorf("reply-to-chat-message requires messageIds or messageId (or chatMessageId)")
	}
	var replyMsg map[string]any
	if rm, ok := bodyMap["replyMessage"].(map[string]any); ok && len(rm) > 0 {
		replyMsg = rm
	} else if rm, ok := args["replyMessage"].(map[string]any); ok && len(rm) > 0 {
		replyMsg = rm
	} else if nestedBody, ok := args["body"].(map[string]any); ok {
		if _, hasInnerBody := nestedBody["body"]; hasInnerBody {
			replyMsg = nestedBody
		} else if _, hasContent := nestedBody["content"]; hasContent {
			replyMsg = map[string]any{"body": nestedBody}
		}
	}
	if len(replyMsg) == 0 {
		return nil, fmt.Errorf("reply-to-chat-message requires replyMessage or body with message content")
	}
	return map[string]any{
		"messageIds":   msgIDs,
		"replyMessage": rewriteTeamsMentions(replyMsg),
	}, nil
}

func extractChatReplyMessageIDs(bodyMap, args map[string]any) []string {
	for _, raw := range []any{bodyMap["messageIds"], args["messageIds"]} {
		switch v := raw.(type) {
		case []string:
			var out []string
			for _, s := range v {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
			if len(out) > 0 {
				return out
			}
		case []any:
			var out []string
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	for _, key := range []string{"messageId", "chatMessageId"} {
		if id, ok := lookupStringArg(args, key); ok && strings.TrimSpace(id) != "" {
			return []string{strings.TrimSpace(id)}
		}
		if id, ok := lookupStringArg(bodyMap, key); ok && strings.TrimSpace(id) != "" {
			return []string{strings.TrimSpace(id)}
		}
	}
	return nil
}

func postProcessGraphResult(toolName string, query url.Values, args map[string]any, res any, respHeaders http.Header) any {
	if isMailMessageTool(toolName) {
		normalizeMailResponseAttachments(res)
	}
	if toolName == "list-mail-attachments" {
		stripAttachmentListBytes(res)
	}
	if selectRaw := query.Get("$select"); selectRaw != "" {
		res = projectSelectFields(res, selectRaw, isMailMessageTool(toolName))
	}
	if toolName == "list-chat-members" || toolName == "list-team-members" {
		if nameFilter, ok := args["name"].(string); ok && strings.TrimSpace(nameFilter) != "" {
			res = filterConversationMembersByName(res, nameFilter)
		}
	}
	if excl, ok := args["excludeResponse"].(bool); ok && excl {
		out := map[string]any{"success": true}
		if inc, ok := args["includeHeaders"].(bool); ok && inc {
			if hdrs := extractGraphResponseHeaders(respHeaders); len(hdrs) > 0 {
				out["_headers"] = hdrs
			}
		}
		return out
	}
	cleaned := cleanODataMetadata(res)
	if inc, ok := args["includeHeaders"].(bool); ok && inc {
		if hdrs := extractGraphResponseHeaders(respHeaders); len(hdrs) > 0 {
			if m, ok := cleaned.(map[string]any); ok {
				m["_headers"] = hdrs
				return m
			}
			return map[string]any{
				"data":     cleaned,
				"_headers": hdrs,
			}
		}
	}
	return cleaned
}

func extractGraphResponseHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) > 0 {
			out[strings.ToLower(k)] = strings.Join(vals, ", ")
		}
	}
	return out
}

func hasAnyToolInCatalog(toolsByName map[string]ToolSpec, names ...string) bool {
	for _, name := range names {
		if _, ok := toolsByName[name]; ok {
			return true
		}
	}
	return false
}

func (h *Handler) executeGetDownloadURL(ctx context.Context, r *http.Request, token, target string, toolsByName map[string]ToolSpec) (any, bool, error) {
	if !isValidGraphTargetPath(target) {
		return map[string]any{
			"error": `target must be a relative Microsoft Graph path starting with "/", e.g. /me/messages/{id}/attachments/{id}/$value or /drives/{drive-id}/items/{driveItem-id}/content.`,
		}, true, nil
	}
	pathPart := target
	if q := strings.IndexByte(pathPart, '?'); q >= 0 {
		pathPart = pathPart[:q]
	}

	switch {
	case mailAttachmentValueTargetRe.MatchString(pathPart):
		if !hasAnyToolInCatalog(toolsByName, "get-mail-attachment-content", "list-mail-attachments", "get-mail-message", "list-mail-messages", "list-mail-folder-messages") {
			return map[string]any{"error": "mail attachment downloads are not enabled in this tool catalog."}, true, nil
		}
		return h.mintAttachmentDownloadResult(ctx, r, token, pathPart)

	case eventAttachmentValueTargetRe.MatchString(pathPart):
		if !hasAnyToolInCatalog(toolsByName, "get-calendar-event", "list-calendar-events", "get-calendar-view") {
			return map[string]any{"error": "calendar event attachment downloads are not enabled in this tool catalog."}, true, nil
		}
		return h.mintAttachmentDownloadResult(ctx, r, token, pathPart)

	case driveItemTargetRe.MatchString(pathPart):
		if !hasAnyToolInCatalog(toolsByName, "get-drive-item", "list-folder-files", "search-onedrive-files") {
			return map[string]any{"error": "drive item downloads are not enabled in this tool catalog."}, true, nil
		}
	default:
		return map[string]any{
			"error": `target must be a valid Microsoft Graph attachment $value path (e.g. /me/messages/{id}/attachments/{id}/$value) or driveItem path (e.g. /drives/{drive-id}/items/{driveItem-id}/content).`,
		}, true, nil
	}

	itemPath := pathPart
	if strings.HasSuffix(strings.ToLower(itemPath), "/content") {
		itemPath = itemPath[:len(itemPath)-len("/content")]
	}
	res, _, err := h.graphRequest(ctx, token, "GET", itemPath, nil, nil, nil)
	if err != nil {
		if errors.Is(err, errGraphUnauthorized) {
			return nil, true, err
		}
		if strings.HasSuffix(strings.ToLower(pathPart), "/content") {
			return h.mintAttachmentDownloadResult(ctx, r, token, pathPart)
		}
		return nil, true, err
	}

	var probedName, probedCT string
	var probedSize any
	if itemMap, ok := res.(map[string]any); ok && itemMap != nil {
		probedName, _ = itemMap["name"].(string)
		probedSize = itemMap["size"]
		if fileObj, _ := itemMap["file"].(map[string]any); fileObj != nil {
			probedCT, _ = fileObj["mimeType"].(string)
		}
	}

	// Never return Graph's pre-authenticated @microsoft.graph.downloadUrl
	// capability URL into model context or logs; always mint a host-side
	// single-use ticket for <itemPath>/content.
	contentPath := itemPath + "/content"
	return h.mintAttachmentDownloadResultWithMeta(r, token, contentPath, probedName, probedCT, probedSize)
}

func (h *Handler) mintAttachmentDownloadResult(ctx context.Context, r *http.Request, token, target string) (any, bool, error) {
	return h.mintAttachmentDownloadResultWithFallback(ctx, r, token, target, "")
}

func (h *Handler) mintAttachmentDownloadResultWithFallback(ctx context.Context, r *http.Request, token, target, fallbackName string) (any, bool, error) {
	var probedName, probedCT string
	var probedSize any

	if strings.HasSuffix(strings.ToLower(target), "/$value") {
		metaPath := target[:len(target)-len("/$value")]
		q := url.Values{}
		q.Set("$select", "id,name,contentType,size")
		if metaRes, _, err := h.graphRequest(ctx, token, "GET", metaPath, q, nil, nil); err == nil {
			if m, ok := metaRes.(map[string]any); ok {
				probedName, _ = m["name"].(string)
				probedCT, _ = m["contentType"].(string)
				probedSize = m["size"]
			}
		} else if errors.Is(err, errGraphUnauthorized) {
			return nil, true, err
		}
	}
	if strings.TrimSpace(probedName) == "" && strings.TrimSpace(fallbackName) != "" {
		probedName = strings.TrimSpace(fallbackName)
	}

	return h.mintAttachmentDownloadResultWithMeta(r, token, target, probedName, probedCT, probedSize)
}

func (h *Handler) mintAttachmentDownloadResultWithMeta(r *http.Request, token, target, probedName, probedCT string, probedSize any) (any, bool, error) {
	ticketID, err := h.tickets.Issue(token, target, h.cfg.AttachmentURLTTLSeconds, probedName, probedCT)
	if err != nil {
		return nil, true, err
	}
	baseURL := h.cfg.baseURLForRequest(r)
	expiresAt := time.Now().UTC().Add(time.Duration(h.cfg.AttachmentURLTTLSeconds) * time.Second).Format(time.RFC3339)

	out := map[string]any{
		"downloadUrl": fmt.Sprintf("%s/attachment?t=%s", baseURL, url.QueryEscape(ticketID)),
		"expiresAt":   expiresAt,
		"singleUse":   true,
		"note":        "Served by this server, not by Microsoft Graph. Valid for one fetch until it expires. Pass downloadUrl to download_url to save the file into your workspace.",
	}
	if probedName != "" {
		out["name"] = probedName
	}
	if probedCT != "" {
		out["contentType"] = probedCT
	}
	if probedSize != nil {
		out["size"] = probedSize
	}
	return out, false, nil
}

func (h *Handler) graphRequestWithExpandFallback(ctx context.Context, token, method, path string, query url.Values, body any, headers map[string]string, autoExpanded bool) (any, http.Header, int, error) {
	res, respHeaders, nBytes, err := h.graphRequestWithSize(ctx, token, method, path, query, body, headers)
	if err != nil && autoExpanded && query.Get("$expand") != "" && query.Get("$search") != "" {
		clonedQuery := url.Values{}
		for k, v := range query {
			if k != "$expand" {
				clonedQuery[k] = append([]string(nil), v...)
			}
		}
		if stripped := stripAutoMailAttachmentExpand(query.Get("$expand")); stripped != "" {
			clonedQuery.Set("$expand", stripped)
		}
		if retryRes, retryHeaders, retryBytes, retryErr := h.graphRequestWithSize(ctx, token, method, path, clonedQuery, body, headers); retryErr == nil {
			return retryRes, retryHeaders, retryBytes, nil
		}
	}
	return res, respHeaders, nBytes, err
}

func stripAutoMailAttachmentExpand(expand string) string {
	if expand == defaultMailAttachmentExpand {
		return ""
	}
	return strings.TrimPrefix(expand, defaultMailAttachmentExpand+",")
}

func (h *Handler) graphRequest(ctx context.Context, token, method, path string, query url.Values, body any, headers map[string]string) (any, http.Header, error) {
	res, hdrs, _, err := h.graphRequestWithSize(ctx, token, method, path, query, body, headers)
	return res, hdrs, err
}

func (h *Handler) graphRequestWithSize(ctx context.Context, token, method, path string, query url.Values, body any, headers map[string]string) (any, http.Header, int, error) {
	base := strings.TrimRight(h.cfg.GraphBaseURL, "/")
	if base == "" {
		base = defaultGraphBaseURL
	}
	var fullURL string
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		if !strings.HasPrefix(path, base+"/") && path != base {
			return nil, nil, 0, fmt.Errorf("graph pagination URL does not match configured Graph base URL")
		}
		fullURL = path
	} else {
		fullURL = base + path
		if len(query) > 0 {
			fullURL += "?" + encodeGraphQuery(query)
		}
	}

	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			bodyReader = strings.NewReader(v)
		case []byte:
			bodyReader = bytes.NewReader(v)
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("marshal request body: %w", err)
			}
			bodyReader = bytes.NewReader(encoded)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader) //nolint:gosec // fullURL is pinned to configured GraphBaseURL.
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build Graph request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := h.httpClient().Do(req) //nolint:gosec // fullURL is pinned to configured GraphBaseURL.
	if err != nil {
		return nil, nil, 0, fmt.Errorf("graph request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphJSONBytes+1))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read Microsoft Graph response: %w", err)
	}
	if len(rawBytes) > maxGraphJSONBytes {
		return nil, resp.Header, len(rawBytes), fmt.Errorf("graph response exceeded maximum size (%d bytes)", maxGraphJSONBytes)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.Header, len(rawBytes), errGraphUnauthorized
	}

	if resp.StatusCode >= 400 {
		errText := strings.TrimSpace(string(rawBytes))
		if errText == "" {
			errText = resp.Status
		}
		return nil, resp.Header, len(rawBytes), fmt.Errorf("graph HTTP %d: %s", resp.StatusCode, errText)
	}

	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(rawBytes)) == 0 {
		return map[string]any{"status": "success", "statusCode": resp.StatusCode}, resp.Header, len(rawBytes), nil
	}

	var parsed any
	if err := json.Unmarshal(rawBytes, &parsed); err != nil {
		return map[string]any{"rawText": string(rawBytes)}, resp.Header, len(rawBytes), nil
	}
	return parsed, resp.Header, len(rawBytes), nil
}

func (h *Handler) followAllPages(ctx context.Context, token string, firstPage any, firstPageBytes int, headers map[string]string) (any, error) {
	firstMap, ok := firstPage.(map[string]any)
	if !ok {
		return firstPage, nil
	}
	combined, ok := firstMap["value"].([]any)
	if !ok {
		return firstPage, nil
	}
	nextLink, _ := firstMap["@odata.nextLink"].(string)
	if len(combined) > maxPaginationItems {
		combined = combined[:maxPaginationItems]
		nextLink = ""
	}
	pages := 1
	totalBytes := firstPageBytes
	for nextLink != "" && pages < maxPaginationPages && len(combined) < maxPaginationItems {
		pageRes, _, pageBytes, err := h.graphRequestWithSize(ctx, token, "GET", nextLink, nil, nil, headers)
		if err != nil {
			return nil, err
		}
		totalBytes += pageBytes
		if totalBytes > maxGraphJSONBytes {
			return nil, fmt.Errorf("graph paginated response exceeded maximum aggregate size (%d bytes)", maxGraphJSONBytes)
		}
		pageMap, ok := pageRes.(map[string]any)
		if !ok {
			break
		}
		pageNextLink, _ := pageMap["@odata.nextLink"].(string)
		if vals, ok := pageMap["value"].([]any); ok {
			if len(combined) > 0 && len(combined)+len(vals) > maxPaginationItems {
				// Stop before appending a page that would cross the item cap so
				// nextLink continues to point at the start of this unconsumed page.
				break
			}
			if len(combined)+len(vals) > maxPaginationItems {
				combined = append(combined, vals[:maxPaginationItems-len(combined)]...)
				nextLink = ""
				break
			}
			combined = append(combined, vals...)
		}
		nextLink = pageNextLink
		pages++
	}
	firstMap["value"] = combined
	if nextLink == "" {
		delete(firstMap, "@odata.nextLink")
	} else {
		firstMap["@odata.nextLink"] = nextLink
	}
	return firstMap, nil
}

func encodeGraphQuery(q url.Values) string {
	// Encode query parameters while keeping $ in OData parameter names unescaped
	// for maximum compatibility with Microsoft Graph and test servers.
	encoded := q.Encode()
	return strings.ReplaceAll(encoded, "%24", "$")
}

func interpolateGraphPath(pattern string, args map[string]any) (string, error) {
	var missing []string
	out := pathPlaceholderRe.ReplaceAllStringFunc(pattern, func(match string) string {
		paramName := match[1 : len(match)-1]
		camelName := kebabToCamel(paramName)
		val, ok := lookupStringArg(args, camelName)
		if !ok {
			val, ok = lookupStringArg(args, paramName)
		}
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, camelName)
			return match
		}
		if strings.Contains(pattern, fmt.Sprintf("'%s'", match)) {
			// Inside OData function quotes like search(q='{q}'), double single quotes first.
			escaped := strings.ReplaceAll(val, "'", "''")
			return encodeGraphPathSegment(escaped)
		}
		return encodeGraphPathSegment(val)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing required path parameter(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func encodeGraphPathSegment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
}

func kebabToCamel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func lookupStringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	switch val := v.(type) {
	case string:
		return val, true
	case float64:
		return strconv.FormatInt(int64(val), 10), true
	case int:
		return strconv.Itoa(val), true
	default:
		return fmt.Sprintf("%v", val), true
	}
}

func intArg(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return int(val), true
	case int:
		return val, true
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func lookupQueryArg(args map[string]any, key string) (any, bool) {
	if v, ok := args[key]; ok && v != nil {
		return v, true
	}
	trimmed := strings.TrimPrefix(key, "$")
	if v, ok := args[trimmed]; ok && v != nil {
		return v, true
	}
	if v, ok := args["$"+trimmed]; ok && v != nil {
		return v, true
	}
	return nil, false
}

func isODataShortName(k string) bool {
	switch k {
	case "top", "skip", "search", "filter", "count", "orderby", "select", "expand", "skiptoken", "deltatoken":
		return true
	}
	return false
}

func formatQueryParamValue(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int:
		return strconv.Itoa(val)
	case []any:
		var parts []string
		for _, item := range val {
			if s := strings.TrimSpace(fmt.Sprintf("%v", item)); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(val, ",")
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func normalizeSkipTokenValue(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if parsed, err := url.Parse(raw); err == nil {
			q := parsed.Query()
			if st := q.Get("$skiptoken"); st != "" {
				return "$skiptoken", st
			}
			if st := q.Get("skiptoken"); st != "" {
				return "$skiptoken", st
			}
			if sk := q.Get("$skip"); sk != "" {
				return "$skip", sk
			}
		}
	}
	return "$skiptoken", raw
}

func isMailMessageTool(toolName string) bool {
	switch toolName {
	case "list-mail-messages", "list-mail-folder-messages", "get-mail-message":
		return true
	}
	return false
}

func isMailEndpoint(pathPattern string) bool {
	if strings.Contains(pathPattern, "/chats") || strings.Contains(pathPattern, "/teams") {
		return false
	}
	return strings.Contains(pathPattern, "/messages") || strings.Contains(pathPattern, "/mailFolders") || strings.Contains(pathPattern, "/sendMail")
}

func normalizeMailSearchQuery(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `"`, `\"`)
		return `"` + inner + `"`
	}
	escaped := strings.ReplaceAll(s, `\"`, `"`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

func isTeamsMessageSendTool(toolName string) bool {
	switch toolName {
	case "send-chat-message", "reply-to-chat-message", "send-channel-message", "reply-to-channel-message":
		return true
	}
	return false
}

func rewriteTeamsMentions(bodyMap map[string]any) map[string]any {
	innerBody, ok := bodyMap["body"].(map[string]any)
	if !ok {
		return bodyMap
	}
	contentType, _ := innerBody["contentType"].(string)
	content, _ := innerBody["content"].(string)
	if !strings.EqualFold(contentType, "html") || !mentionTagRe.MatchString(content) {
		return bodyMap
	}

	nextID := 0
	var mentions []any
	if existing, ok := bodyMap["mentions"].([]any); ok {
		mentions = append(mentions, existing...)
		for _, item := range existing {
			if m, ok := item.(map[string]any); ok {
				if idVal, ok := intArg(m, "id"); ok && idVal >= nextID {
					nextID = idVal + 1
				}
			}
		}
	}
	for _, m := range existingAtIDRe.FindAllStringSubmatch(content, -1) {
		if idVal, err := strconv.Atoi(m[1]); err == nil && idVal >= nextID {
			nextID = idVal + 1
		}
	}

	newContent := mentionTagRe.ReplaceAllStringFunc(content, func(full string) string {
		sub := mentionTagRe.FindStringSubmatch(full)
		if len(sub) != 3 {
			return full
		}
		userID := sub[1]
		displayName := strings.TrimSpace(sub[2])
		id := nextID
		nextID++
		mentions = append(mentions, map[string]any{
			"id":          id,
			"mentionText": displayName,
			"mentioned": map[string]any{
				"user": map[string]any{
					"id":               userID,
					"displayName":      displayName,
					"userIdentityType": "aadUser",
				},
			},
		})
		return fmt.Sprintf(`<at id="%d">%s</at>`, id, displayName)
	})

	clonedInner := make(map[string]any, len(innerBody))
	for k, v := range innerBody {
		clonedInner[k] = v
	}
	clonedInner["content"] = newContent

	cloned := make(map[string]any, len(bodyMap)+1)
	for k, v := range bodyMap {
		cloned[k] = v
	}
	cloned["body"] = clonedInner
	cloned["mentions"] = mentions
	return cloned
}

func summarizeAttachments(raw any) []map[string]any {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		clean := map[string]any{}
		for _, k := range []string{"id", "name", "contentType", "size", "isInline"} {
			if v, exists := m[k]; exists {
				clean[k] = v
			}
		}
		for k, v := range m {
			if k == "contentBytes" {
				continue
			}
			if _, exists := clean[k]; !exists && !strings.HasPrefix(k, "@odata.") {
				clean[k] = v
			}
		}
		out = append(out, clean)
	}
	return out
}

func stripAttachmentListBytes(res any) {
	m, ok := res.(map[string]any)
	if !ok {
		return
	}
	if vals, ok := m["value"].([]any); ok {
		for _, item := range vals {
			if attMap, ok := item.(map[string]any); ok {
				delete(attMap, "contentBytes")
			}
		}
	}
	delete(m, "contentBytes")
}

func normalizeMailResponseAttachments(res any) {
	m, ok := res.(map[string]any)
	if !ok {
		return
	}
	if vals, ok := m["value"].([]any); ok {
		for _, item := range vals {
			if msgMap, ok := item.(map[string]any); ok {
				if atts := summarizeAttachments(msgMap["attachments"]); atts != nil {
					msgMap["attachments"] = atts
				}
			}
		}
		return
	}
	if atts := summarizeAttachments(m["attachments"]); atts != nil {
		m["attachments"] = atts
	}
}

func projectSelectFields(res any, selectRaw string, keepAttachments bool) any {
	var fields []string
	for _, part := range strings.Split(selectRaw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			fields = append(fields, trimmed)
		}
	}
	if len(fields) == 0 {
		return res
	}
	if keepAttachments {
		fields = appendUniqueString(fields, "attachments")
	}

	projectMap := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if strings.HasPrefix(k, "@odata.") {
				out[k] = v
			}
		}
		for _, f := range fields {
			root := strings.Split(f, "/")[0]
			if v, exists := m[root]; exists {
				out[root] = v
			}
		}
		return out
	}

	m, ok := res.(map[string]any)
	if !ok {
		return res
	}
	if vals, ok := m["value"].([]any); ok {
		projected := make([]any, 0, len(vals))
		for _, item := range vals {
			if itemMap, ok := item.(map[string]any); ok {
				projected = append(projected, projectMap(itemMap))
			} else {
				projected = append(projected, item)
			}
		}
		m["value"] = projected
		return m
	}
	return projectMap(m)
}

func filterConversationMembersByName(res any, needle string) any {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return res
	}
	m, ok := res.(map[string]any)
	if !ok {
		return res
	}
	vals, ok := m["value"].([]any)
	if !ok {
		return res
	}
	var kept []any
	for _, item := range vals {
		mem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		dn, _ := mem["displayName"].(string)
		em, _ := mem["email"].(string)
		if strings.Contains(strings.ToLower(dn), needle) || strings.Contains(strings.ToLower(em), needle) {
			kept = append(kept, mem)
		}
	}
	if kept == nil {
		kept = []any{}
	}
	m["value"] = kept
	return m
}

func cleanODataMetadata(v any) any {
	switch val := v.(type) {
	case []any:
		for i := range val {
			val[i] = cleanODataMetadata(val[i])
		}
		return val
	case map[string]any:
		for k, child := range val {
			if strings.HasPrefix(k, "@odata.") && k != "@odata.nextLink" && k != "@odata.deltaLink" && k != "@odata.count" {
				delete(val, k)
				continue
			}
			if strings.HasPrefix(strings.ToLower(k), "@microsoft.graph.") && strings.Contains(strings.ToLower(k), "downloadurl") {
				delete(val, k)
				continue
			}
			val[k] = cleanODataMetadata(child)
		}
		return val
	default:
		return v
	}
}
