package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// TestConversationExport_Formats drives GET /conversations/{id}/export across
// the three artifacts the download dialog offers. Each must arrive as a saved
// file of the right type with a recognizable name — the browser's Save dialog
// is the whole UI here, so the headers ARE the user experience.
func TestConversationExport_Formats(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConv(t, s, user) // "failed tasks": one user turn, one assistant turn
	h := s.Routes()

	cases := []struct {
		name        string
		query       string
		contentType string
		ext         string
		contains    []string
	}{
		{
			name:        "default stays JSON for existing callers",
			query:       "",
			contentType: "application/json",
			ext:         ".json",
			contains:    []string{`"conversation"`, `"history"`, `"exported_at"`},
		},
		{
			name:        "web page",
			query:       "?format=html",
			contentType: "text/html",
			ext:         ".html",
			contains:    []string{"<!doctype html>", "<h1>failed tasks</h1>", "nightly-etl"},
		},
		{
			name:        "markdown document",
			query:       "?format=markdown",
			contentType: "text/markdown",
			ext:         ".md",
			contains:    []string{"# Conversation: failed tasks", "## User"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/export"+tc.query, nil, user)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
				t.Errorf("Content-Type = %q, want %q", ct, tc.contentType)
			}
			cd := w.Header().Get("Content-Disposition")
			if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, tc.ext) {
				t.Errorf("Content-Disposition = %q, want an attachment named *%s", cd, tc.ext)
			}
			if !strings.Contains(cd, "failed-tasks") {
				t.Errorf("Content-Disposition = %q, want the conversation title in the filename", cd)
			}
			// A download of model-authored text must never be re-typed by the
			// browser into something it would render or run.
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			for _, want := range tc.contains {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body missing %q:\n%s", want, w.Body.String())
				}
			}
		})
	}
}

// TestConversationExport_IncludeScope proves the dialog's "include the agent's
// work" checkbox reaches the renderer, and that leaving it off is what gives a
// reader the short, readable document.
func TestConversationExport_IncludeScope(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConvWithToolCall(t, s, user)
	h := s.Routes()

	readable := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/export?format=html", nil, user).Body.String()
	if strings.Contains(readable, "Used tool") {
		t.Errorf("the default export should leave out the working trail:\n%s", readable)
	}
	full := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/export?format=html&include=full", nil, user).Body.String()
	if !strings.Contains(full, "Used tool: bash") {
		t.Errorf("include=full should carry the working trail:\n%s", full)
	}

	// JSON is the archival shape: it carries everything regardless of scope.
	jsonBody := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/export", nil, user).Body.String()
	if !strings.Contains(jsonBody, "tool_call") {
		t.Errorf("JSON export dropped a tool call:\n%s", jsonBody)
	}
}
