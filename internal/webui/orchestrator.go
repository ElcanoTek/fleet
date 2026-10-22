// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf16"
)

// handleOrchLogin is POST /api/orchestrator/auth/login: moc's
// username/password login for API/CLI clients, which still obtain a bearer
// this way (the web UI itself uses the session cookie, #1115). Public.
func (g *Gateway) handleOrchLogin(w http.ResponseWriter, r *http.Request) {
	if !g.verifyOrigin(w, r) {
		return
	}
	if g.orch == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": "orchestrator unreachable: no backend"})
		return
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	c := call{method: http.MethodPost, target: "/auth/login", header: h, detach: true}
	c.body, c.contentLength = bodyOrNil(r)
	g.dispatch(w, r, g.orch, "orchestrator", c, textJSON)
}

// handleOrchLogout is POST /api/orchestrator/auth/logout: ends the
// orchestrator's own server-side state (best effort, called by every
// surface's sign-out before the top-level /api/auth/logout form). Forwards a
// presented Authorization header and any cookie deletion the orchestrator
// answers with.
func (g *Gateway) handleOrchLogout(w http.ResponseWriter, r *http.Request) {
	if !g.verifyOrigin(w, r) {
		return
	}
	if g.orch == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": "orchestrator unreachable: no backend"})
		return
	}
	h := http.Header{}
	if a := r.Header.Get("Authorization"); a != "" {
		h.Set("Authorization", a)
	}
	g.dispatch(w, r, g.orch, "orchestrator", call{method: http.MethodPost, target: "/auth/logout", header: h, detach: true},
		out{headers: []string{"Content-Type", "Set-Cookie"}, ctFallback: "application/json"})
}

// Discuss-a-run digest bounds.
const (
	digestTotalChars      = 24_000
	digestPerMessageChars = 2_000
)

// clip truncates to max UTF-16 code units, the unit the TypeScript tier
// counted in, so a digest is the same length it always was.
func clip(text string, maxUnits int) string {
	u := utf16.Encode([]rune(text))
	if len(u) <= maxUnits {
		return text
	}
	return string(utf16.Decode(u[:maxUnits])) + "\n[…truncated…]"
}

func jsUnits(s string) int { return len(utf16.Encode([]rune(s))) }

type discussTask struct {
	ID           string  `json:"id"`
	Prompt       string  `json:"prompt"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message"`
	Result       *string `json:"result"`
	CompletedAt  *string `json:"completed_at"`
	Recurrence   *string `json:"recurrence"`
	Artifacts    []*struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"artifacts"`
}

type discussLog struct {
	Messages []struct {
		Role     string `json:"role"`
		Content  string `json:"content"`
		ToolName string `json:"tool_name"`
		IsError  bool   `json:"is_error"`
	} `json:"messages"`
	Cost *float64 `json:"cost"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// formatDigest renders a finished run as the seed of a new conversation.
func formatDigest(task discussTask, log discussLog) string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	add("You are looking at the transcript of a finished scheduled task run. The user wants to discuss it — answer questions about what happened using the transcript below.")
	add("")
	add("## Task")
	add("- ID: " + task.ID)
	if task.Name != "" {
		add("- Name: " + task.Name)
	}
	add("- Status: " + firstNonEmpty(task.Status, "unknown"))
	if v := deref(task.Recurrence); v != "" {
		add("- Recurrence: " + v)
	}
	if v := deref(task.CompletedAt); v != "" {
		add("- Completed: " + v)
	}
	if log.Cost != nil {
		add(fmt.Sprintf("- Cost: $%.4f", *log.Cost))
	}
	add("- Prompt: " + clip(task.Prompt, digestPerMessageChars))
	if v := deref(task.ErrorMessage); v != "" {
		add("- Error: " + clip(v, digestPerMessageChars))
	}
	if v := deref(task.Result); v != "" {
		add("")
		add("## Result")
		add(clip(v, digestPerMessageChars*2))
	}
	var artifacts []string
	for _, a := range task.Artifacts {
		if a == nil {
			continue
		}
		artifacts = append(artifacts, firstNonEmpty(a.Name, a.Path, "unnamed"))
	}
	if len(artifacts) > 0 {
		add("")
		add("## Artifacts")
		for i, a := range artifacts {
			if i == 20 {
				break
			}
			add("- " + a)
		}
	}
	add("")
	add("## Transcript")
	rendered := make([]string, 0, len(log.Messages))
	for _, m := range log.Messages {
		who := firstNonEmpty(m.Role, "unknown")
		if m.ToolName != "" {
			who = "tool:" + m.ToolName
			if m.IsError {
				who += " (error)"
			}
		}
		rendered = append(rendered, "["+who+"] "+clip(m.Content, digestPerMessageChars))
	}
	var kept []string
	used := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		used += jsUnits(rendered[i]) + 1
		if used > digestTotalChars {
			kept = append(kept, fmt.Sprintf("[…%d earlier transcript message(s) omitted…]", i+1))
			break
		}
		kept = append(kept, rendered[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if len(kept) == 0 {
		kept = []string{"(empty transcript)"}
	}
	lines = append(lines, kept...)
	return strings.Join(lines, "\n")
}

// handleDiscuss is POST /api/orchestrator/tasks/{id}/discuss: read the task
// and its transcript with the caller's orchestrator credential (a run the
// caller cannot read 403/404s identically), then open a chat seeded with a
// digest of the run.
func (g *Gateway) handleDiscuss(w http.ResponseWriter, r *http.Request) {
	if !g.verifyOrigin(w, r) {
		return
	}
	s := g.session(r)
	if s == nil {
		unauthorized(w)
		return
	}
	auth, ok := g.resolveOrchAuth(r)
	if !ok {
		unauthorized(w)
		return
	}
	enc := p(r, "taskId")
	taskStatus, taskHdr, taskBody, ok1 := g.callCaptureLimit(g.orch, call{method: http.MethodGet, target: "/tasks/" + enc, header: g.orchHeaders(auth)}, maxBackendDocumentSize)
	logStatus, logHdr, logBody, ok2 := g.callCaptureLimit(g.orch, call{method: http.MethodGet, target: "/logs/" + enc, header: g.orchHeaders(auth)}, maxBackendDocumentSize)
	if !ok1 || !ok2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "orchestrator unreachable: no backend"})
		return
	}
	// A revoked session on either lookup clears the stale cookie, exactly as
	// the proxy path does — otherwise the client loops between /login and the
	// shell on a cookie that will never be accepted again.
	if (taskStatus == http.StatusUnauthorized && taskHdr.Get("X-Session-Revoked") == "1") ||
		(logStatus == http.StatusUnauthorized && logHdr.Get("X-Session-Revoked") == "1") {
		g.clearSessionCookie(w, r)
	}
	if taskStatus < 200 || taskStatus > 299 {
		writeJSON(w, taskStatus, map[string]string{"error": "task not found"})
		return
	}
	if logStatus < 200 || logStatus > 299 {
		writeJSON(w, logStatus, map[string]string{"error": "no transcript for this task"})
		return
	}
	var task discussTask
	var log discussLog
	if !json.Valid(taskBody) || !json.Valid(logBody) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "orchestrator returned an unreadable task"})
		return
	}
	// A field of an unexpected type is skipped, not fatal (the TS tier read
	// these loosely too); decoding continues past a type mismatch.
	_ = json.Unmarshal(taskBody, &task)
	_ = json.Unmarshal(logBody, &log)
	preview := strings.TrimSpace(firstNonEmpty(task.Name, task.Prompt, task.ID))
	if u := utf16.Encode([]rune(preview)); len(u) > 60 {
		preview = string(utf16.Decode(u[:60]))
	}
	h := g.chatHeaders(identityOf(s))
	h.Set("Content-Type", "application/json")
	body, n := jsonReader(struct {
		Title string `json:"title"`
		Seed  string `json:"seed"`
	}{"Discuss run: " + preview, formatDigest(task, log)})
	status, hdr, resp, ok := g.callCapture(g.chat, call{method: http.MethodPost, target: "/conversations", header: h, body: body, contentLength: n})
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "chat-server unreachable: no backend"})
		return
	}
	if status == http.StatusUnauthorized && hdr.Get("X-Session-Revoked") == "1" {
		g.clearSessionCookie(w, r)
	}
	if status < 200 || status > 299 {
		w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write(resp)
		return
	}
	var conv struct {
		LowerID string `json:"id"`
		UpperID string `json:"ID"`
	}
	_ = json.Unmarshal(resp, &conv)
	var id any
	if v := firstNonEmpty(conv.LowerID, conv.UpperID); v != "" {
		id = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_id": id})
}
