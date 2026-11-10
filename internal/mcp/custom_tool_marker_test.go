package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// TestCustomToolsRefuseRedactedArguments: a saved connector tool called with
// an argument the model copied out of an earlier, redacted call refuses it
// instead of sending or typing "[REDACTED]". Neither tool runs.
func TestCustomToolsRefuseRedactedArguments(t *testing.T) {
	t.Parallel()
	c := NewClient()
	if err := c.AddHTTPToolsOnServer("acme", []HTTPToolSpec{{Name: "login", Method: "POST", URL: "https://a.example.com/"}}); err != nil {
		t.Fatalf("AddHTTPToolsOnServer: %v", err)
	}
	transports := map[string]Transport{
		"playwright": &playwrightToolTransport{tools: map[string]PlaywrightToolSpec{"login": {Name: "login", URL: "https://a.example.com/"}}},
		"http":       c.servers["acme"].transport,
	}
	for name, tr := range transports {
		raw, err := tr.Call(context.Background(), "tools/call", map[string]interface{}{
			"name":      "login",
			"arguments": map[string]interface{}{"password": "[REDACTED]", "nested": map[string]interface{}{"ok": "fine"}},
		})
		if err != nil {
			t.Fatalf("%s: Call: %v", name, err)
		}
		var res ToolResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "the placeholder the conversation shows in place of a secret") {
			t.Errorf("%s: result = %+v, want the redacted argument refused", name, res)
		}
	}
}

// TestBundleHTTPToolsSendQuotedPlaceholders: a bundle's _http tools take
// their credentials from host env headers, not from arguments, so text that
// quotes redacted output (a ticket body, say) is sent as written. And the
// refusal the custom tools give survives the redactor, so the model can read
// it.
func TestBundleHTTPToolsSendQuotedPlaceholders(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient()
	c.AddHTTPTools([]HTTPToolSpec{{Name: "post_note", Method: "POST", URL: srv.URL + "/notes", BodyTemplate: "{{note}}"}})
	tr := c.servers[HTTPToolServerName].transport.(*httpToolTransport)
	tr.client = srv.Client()
	note := "the log said Authorization: Bearer [REDACTED]"
	raw, err := tr.Call(context.Background(), "tools/call", map[string]interface{}{
		"name":      "post_note",
		"arguments": map[string]interface{}{"note": note},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var res ToolResult
	if err := json.Unmarshal(raw, &res); err != nil || res.IsError {
		t.Fatalf("result = %s (%v), want the note sent", raw, err)
	}
	if body := <-got; !strings.Contains(body, "[REDACTED]") {
		t.Errorf("body sent = %q, want the quoted placeholder", body)
	}
	if r := redact.NewRedactor(nil); r.Redact(redact.MarkerArgumentRefusal) != redact.MarkerArgumentRefusal {
		t.Errorf("the refusal is redacted to %q", r.Redact(redact.MarkerArgumentRefusal))
	}
}
