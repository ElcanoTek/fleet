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
)

var (
	pathPlaceholderRe       = regexp.MustCompile(`\{([^}]+)\}`)
	attachmentValueTargetRe = regexp.MustCompile(`^(/me|/users/[^/]+)/(messages|events)/[^/]+/attachments/[^/]+/\$value$`)
	mentionTagRe            = regexp.MustCompile(`(?i)<at\s+user-id=["']([0-9a-fA-F-]{36})["']\s*>([^<]+)</at>`)
	existingAtIDRe          = regexp.MustCompile(`(?i)<at\s+id=["'](\d+)["']`)
)

func (h *Handler) executeToolCall(ctx context.Context, r *http.Request, token, toolName string, args map[string]any) (any, bool, error) {
	if res, isErr, handled, err := h.executeBuiltInUtilityTool(ctx, r, token, toolName, args); handled {
		return res, isErr, err
	}

	spec, ok := h.toolsByName[toolName]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("Unknown tool: %s", toolName)}, true, nil
	}

	path, err := interpolateGraphPath(spec.PathPattern, args)
	if err != nil {
		return nil, true, err
	}

	if spec.ReturnDownloadURL || toolName == "get-mail-attachment-content" {
		return h.mintAttachmentDownloadResult(ctx, r, token, path)
	}

	query, autoExpandedAttachments := buildGraphQueryParams(spec, toolName, args)
	headers := buildGraphHeaders(spec, toolName, query, args)
	reqBody := buildGraphRequestBody(spec, toolName, args)

	res, err := h.graphRequestWithExpandFallback(ctx, token, spec.Method, path, query, reqBody, headers, autoExpandedAttachments)
	if err != nil {
		return nil, true, err
	}

	if fetchAll, ok := args["fetchAllPages"].(bool); ok && fetchAll && spec.Method == "GET" {
		res = h.followAllPages(ctx, token, res, headers)
	}

	return postProcessGraphResult(toolName, query, args, res), false, nil
}

func (h *Handler) executeBuiltInUtilityTool(ctx context.Context, r *http.Request, token, toolName string, args map[string]any) (any, bool, bool, error) {
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
		res, isErr, err := h.executeGetDownloadURL(ctx, r, token, target)
		return res, isErr, true, err
	}
	return nil, false, false, nil
}

func buildGraphQueryParams(spec ToolSpec, toolName string, args map[string]any) (url.Values, bool) {
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
	for _, odataName := range []string{"top", "skip", "search", "filter", "count", "orderby", "select", "expand", "skiptoken", "startDateTime", "endDateTime"} {
		paramKey := "$" + odataName
		if odataName == "startDateTime" || odataName == "endDateTime" {
			paramKey = odataName
			if spec.Name != "get-calendar-view" {
				continue
			}
		}
		if query.Get(paramKey) != "" {
			continue
		}
		if val, exists := lookupQueryArg(args, odataName); exists {
			setQueryParamValue(query, paramKey, val)
		}
	}

	autoExpandedAttachments := false
	if isMailMessageTool(toolName) && query.Get("$expand") == "" {
		query.Set("$expand", defaultMailAttachmentExpand)
		autoExpandedAttachments = true
	}

	if rawSearch := query.Get("$search"); rawSearch != "" && isMailEndpoint(spec.PathPattern) {
		query.Set("$search", normalizeMailSearchQuery(rawSearch))
	}

	if expProps, ok := args["expandExtendedProperties"].([]any); ok && len(expProps) > 0 {
		var ids []string
		for _, item := range expProps {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				ids = append(ids, fmt.Sprintf("id eq '%s'", strings.ReplaceAll(strings.TrimSpace(s), "'", "''")))
			}
		}
		if len(ids) > 0 {
			clause := fmt.Sprintf("singleValueExtendedProperties($filter=%s)", strings.Join(ids, " or "))
			if cur := query.Get("$expand"); cur != "" && !autoExpandedAttachments {
				query.Set("$expand", cur+","+clause)
			} else {
				query.Set("$expand", clause)
				autoExpandedAttachments = false
			}
		}
	}
	return query, autoExpandedAttachments
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
	}
	return headers
}

func buildGraphRequestBody(spec ToolSpec, toolName string, args map[string]any) any {
	if spec.Method != "POST" && spec.Method != "PATCH" && spec.Method != "PUT" && spec.Method != "DELETE" {
		return nil
	}
	var reqBody any
	if len(spec.BodyParams) == 1 && spec.BodyParams[0] == "body" {
		if b, ok := args["body"]; ok {
			reqBody = b
		} else if spec.Method == "POST" {
			reqBody = map[string]any{}
		}
	} else if len(spec.BodyParams) > 0 {
		bodyMap := map[string]any{}
		if nested, ok := args["body"].(map[string]any); ok {
			for k, v := range nested {
				bodyMap[k] = v
			}
		}
		for _, bp := range spec.BodyParams {
			if v, ok := args[bp]; ok {
				bodyMap[bp] = v
			}
		}
		reqBody = bodyMap
	}
	if isTeamsMessageSendTool(toolName) {
		if bodyMap, ok := reqBody.(map[string]any); ok {
			reqBody = rewriteTeamsMentions(bodyMap)
		}
	}
	return reqBody
}

func postProcessGraphResult(toolName string, query url.Values, args map[string]any, res any) any {
	if isMailMessageTool(toolName) {
		normalizeMailResponseAttachments(res)
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
		return map[string]any{"success": true}
	}
	return cleanODataMetadata(res)
}

func (h *Handler) executeGetDownloadURL(ctx context.Context, r *http.Request, token, target string) (any, bool, error) {
	if !isValidGraphTargetPath(target) {
		return map[string]any{
			"error": `target must be a relative Microsoft Graph path starting with "/", e.g. /me/messages/{id}/attachments/{id}/$value or /drives/{drive-id}/items/{driveItem-id}/content.`,
		}, true, nil
	}

	if strings.HasSuffix(strings.ToLower(target), "/$value") || attachmentValueTargetRe.MatchString(target) {
		return h.mintAttachmentDownloadResult(ctx, r, token, target)
	}

	itemPath := target
	if strings.HasSuffix(strings.ToLower(itemPath), "/content") {
		itemPath = itemPath[:len(itemPath)-len("/content")]
	}
	res, err := h.graphRequest(ctx, token, "GET", itemPath, nil, nil, nil)
	if err != nil {
		if errors.Is(err, errGraphUnauthorized) {
			return nil, true, err
		}
		if strings.HasSuffix(strings.ToLower(target), "/content") {
			return h.mintAttachmentDownloadResult(ctx, r, token, target)
		}
		return nil, true, err
	}
	itemMap, _ := res.(map[string]any)
	if itemMap != nil {
		if dlURL, _ := itemMap["@microsoft.graph.downloadUrl"].(string); strings.TrimSpace(dlURL) != "" {
			out := map[string]any{
				"downloadUrl": dlURL,
			}
			if name, _ := itemMap["name"].(string); name != "" {
				out["name"] = name
			}
			if size, ok := itemMap["size"]; ok && size != nil {
				out["size"] = size
			}
			if fileObj, _ := itemMap["file"].(map[string]any); fileObj != nil {
				if mt, _ := fileObj["mimeType"].(string); mt != "" {
					out["contentType"] = mt
				}
			}
			return out, false, nil
		}
	}

	contentPath := itemPath + "/content"
	return h.mintAttachmentDownloadResult(ctx, r, token, contentPath)
}

func (h *Handler) mintAttachmentDownloadResult(ctx context.Context, r *http.Request, token, target string) (any, bool, error) {
	var probedName, probedCT string
	var probedSize any

	if strings.HasSuffix(strings.ToLower(target), "/$value") {
		metaPath := target[:len(target)-len("/$value")]
		q := url.Values{}
		q.Set("$select", "id,name,contentType,size")
		if metaRes, err := h.graphRequest(ctx, token, "GET", metaPath, q, nil, nil); err == nil {
			if m, ok := metaRes.(map[string]any); ok {
				probedName, _ = m["name"].(string)
				probedCT, _ = m["contentType"].(string)
				probedSize = m["size"]
			}
		} else if errors.Is(err, errGraphUnauthorized) {
			return nil, true, err
		}
	}

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

func (h *Handler) graphRequestWithExpandFallback(ctx context.Context, token, method, path string, query url.Values, body any, headers map[string]string, autoExpanded bool) (any, error) {
	res, err := h.graphRequest(ctx, token, method, path, query, body, headers)
	if err != nil && autoExpanded && query.Get("$expand") != "" && query.Get("$search") != "" {
		clonedQuery := url.Values{}
		for k, v := range query {
			if k != "$expand" {
				clonedQuery[k] = append([]string(nil), v...)
			}
		}
		if retryRes, retryErr := h.graphRequest(ctx, token, method, path, clonedQuery, body, headers); retryErr == nil {
			return retryRes, nil
		}
	}
	return res, err
}

func (h *Handler) graphRequest(ctx context.Context, token, method, path string, query url.Values, body any, headers map[string]string) (any, error) {
	var fullURL string
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		fullURL = path
	} else {
		base := strings.TrimRight(h.cfg.GraphBaseURL, "/")
		if base == "" {
			base = defaultGraphBaseURL
		}
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
				return nil, fmt.Errorf("marshal request body: %w", err)
			}
			bodyReader = bytes.NewReader(encoded)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build Graph request: %w", err)
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

	resp, err := h.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("graph request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, fmt.Errorf("read Microsoft Graph response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errGraphUnauthorized
	}

	if resp.StatusCode >= 400 {
		errText := strings.TrimSpace(string(rawBytes))
		if errText == "" {
			errText = resp.Status
		}
		return nil, fmt.Errorf("graph HTTP %d: %s", resp.StatusCode, errText)
	}

	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(rawBytes)) == 0 {
		return map[string]any{"status": "success", "statusCode": resp.StatusCode}, nil
	}

	var parsed any
	if err := json.Unmarshal(rawBytes, &parsed); err != nil {
		return map[string]any{"rawText": string(rawBytes)}, nil
	}
	return parsed, nil
}

func (h *Handler) followAllPages(ctx context.Context, token string, firstPage any, headers map[string]string) any {
	firstMap, ok := firstPage.(map[string]any)
	if !ok {
		return firstPage
	}
	combined, ok := firstMap["value"].([]any)
	if !ok {
		return firstPage
	}
	nextLink, _ := firstMap["@odata.nextLink"].(string)
	pages := 1
	for nextLink != "" && pages < maxPaginationPages && len(combined) < maxPaginationItems {
		pageRes, err := h.graphRequest(ctx, token, "GET", nextLink, nil, nil, headers)
		if err != nil {
			break
		}
		pageMap, ok := pageRes.(map[string]any)
		if !ok {
			break
		}
		if vals, ok := pageMap["value"].([]any); ok {
			combined = append(combined, vals...)
		}
		nextLink, _ = pageMap["@odata.nextLink"].(string)
		pages++
	}
	firstMap["value"] = combined
	if nextLink == "" {
		delete(firstMap, "@odata.nextLink")
	} else {
		firstMap["@odata.nextLink"] = nextLink
	}
	return firstMap
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
	return strings.Contains(pathPattern, "/messages") || strings.Contains(pathPattern, "/mailFolders")
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
			if _, exists := clean[k]; !exists && !strings.HasPrefix(k, "@odata.") {
				clean[k] = v
			}
		}
		out = append(out, clean)
	}
	return out
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
			if strings.HasPrefix(k, "@odata.") || strings.HasPrefix(k, "@microsoft.graph.") {
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
			val[k] = cleanODataMetadata(child)
		}
		return val
	default:
		return v
	}
}
