package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
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

func TestCustomToolsRefuseVaultReferenceArguments(t *testing.T) {
	t.Parallel()
	c := NewClient()
	if err := c.AddHTTPToolsOnServer("acme", []HTTPToolSpec{{Name: "login", Method: "POST", URL: "https://a.example.com/", BodyTemplate: `{"password":"{password}"}`}}); err != nil {
		t.Fatalf("AddHTTPToolsOnServer: %v", err)
	}
	transports := map[string]Transport{
		"playwright": &playwrightToolTransport{tools: map[string]PlaywrightToolSpec{"login": {Name: "login", URL: "https://a.example.com/"}}},
		"http":       c.servers["acme"].transport,
	}
	for name, tr := range transports {
		raw, err := tr.Call(context.Background(), "tools/call", map[string]interface{}{
			"name":      "login",
			"arguments": map[string]interface{}{"password": "$secret:password"},
		})
		if err != nil {
			t.Fatalf("%s: Call: %v", name, err)
		}
		var res ToolResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != redact.VaultReferenceArgumentRefusal {
			t.Errorf("%s: result = %+v, want VaultReferenceArgumentRefusal", name, res)
		}
	}
}

func TestCustomToolsRefusePersistedMarkersAndVaultRefs(t *testing.T) {
	t.Parallel()
	c := NewClient()
	if err := c.AddHTTPToolsOnServer("acme", []HTTPToolSpec{
		{Name: "body_marker", Method: "POST", URL: "https://a.example.com/", BodyTemplate: `{"key":"[REDACTED]"}`},
		{Name: "hdr_marker", Method: "GET", URL: "https://a.example.com/", Headers: map[string]string{"Authorization": "Bearer [REDACTED]"}},
		{Name: "hdr_vault", Method: "GET", URL: "https://a.example.com/", Headers: map[string]string{"Authorization": "Bearer $secret:api_token"}},
	}); err != nil {
		t.Fatalf("AddHTTPToolsOnServer: %v", err)
	}
	httpTr := c.servers["acme"].transport
	for _, toolName := range []string{"body_marker", "hdr_marker", "hdr_vault"} {
		raw, err := httpTr.Call(context.Background(), "tools/call", map[string]interface{}{
			"name":      toolName,
			"arguments": map[string]interface{}{},
		})
		if err != nil {
			t.Fatalf("http %s: Call: %v", toolName, err)
		}
		var res ToolResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("http %s: decode: %v", toolName, err)
		}
		if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != redact.SavedConnectorMarkerRefusal {
			t.Errorf("http %s: result = %+v, want SavedConnectorMarkerRefusal", toolName, res)
		}
	}

	pwTr := &playwrightToolTransport{tools: map[string]PlaywrightToolSpec{
		"step_marker": {
			Name:  "step_marker",
			URL:   "https://a.example.com/",
			Steps: []playwrightrunner.Step{{Action: "fill", Selector: "#pw", Value: "[REDACTED]"}},
		},
		"cred_marker": {
			Name:        "cred_marker",
			URL:         "https://a.example.com/",
			Credentials: playwrightrunner.Credentials{Password: "[REDACTED]"},
		},
		"cookie_marker": {
			Name: "cookie_marker",
			URL:  "https://a.example.com/",
			Credentials: playwrightrunner.Credentials{
				StorageState: map[string]any{"cookies": []any{map[string]any{"value": "[REDACTED]"}}},
			},
		},
	}}
	for _, toolName := range []string{"step_marker", "cred_marker", "cookie_marker"} {
		raw, err := pwTr.Call(context.Background(), "tools/call", map[string]interface{}{
			"name":      toolName,
			"arguments": map[string]interface{}{},
		})
		if err != nil {
			t.Fatalf("playwright %s: Call: %v", toolName, err)
		}
		var res ToolResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("playwright %s: decode: %v", toolName, err)
		}
		if !res.IsError || len(res.Content) == 0 || res.Content[0].Text != redact.SavedConnectorMarkerRefusal {
			t.Errorf("playwright %s: result = %+v, want SavedConnectorMarkerRefusal", toolName, res)
		}
	}
}
