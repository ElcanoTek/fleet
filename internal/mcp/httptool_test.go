package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSubstituteTokens covers {param} substitution in both contexts: URL
// (percent-encoded so a value can't inject path/query structure) and body (raw,
// because the template author controls the surrounding JSON quoting). Unknown
// tokens are left intact rather than blanked.
func TestSubstituteTokens(t *testing.T) {
	args := map[string]interface{}{
		"ticket_id": "PROJ 123/x?y",
		"count":     7,
	}
	if got := substituteTokens("/issue/{ticket_id}", args, true); got != "/issue/PROJ+123%2Fx%3Fy" {
		t.Errorf("url substitution = %q, want percent-encoded", got)
	}
	if got := substituteTokens(`{"id":"{ticket_id}","n":{count}}`, args, false); got != `{"id":"PROJ 123/x?y","n":7}` {
		t.Errorf("body substitution = %q, want raw", got)
	}
	// Unknown token preserved.
	if got := substituteTokens("/x/{unknown}", args, true); got != "/x/{unknown}" {
		t.Errorf("unknown token = %q, want left intact", got)
	}
	// No braces: passthrough.
	if got := substituteTokens("/static", args, true); got != "/static" {
		t.Errorf("no-token passthrough = %q", got)
	}
}

// TestApplyResponseJQ covers the jq filter path: a JSON body is transformed; a
// non-JSON body is passed through untouched (ok=false, no error).
func TestApplyResponseJQ(t *testing.T) {
	body := []byte(`{"fields":{"status":{"name":"Open"},"summary":"hi"}}`)
	out, ok, err := applyResponseJQ(`.fields | {summary, status: .status.name}`, body)
	if err != nil {
		t.Fatalf("applyResponseJQ error: %v", err)
	}
	if !ok {
		t.Fatal("applyResponseJQ ok=false on valid JSON")
	}
	if !strings.Contains(out, `"summary":"hi"`) || !strings.Contains(out, `"status":"Open"`) {
		t.Errorf("filtered = %q, want summary+status", out)
	}

	// String result is rendered raw (not JSON-quoted).
	if out, _, err := applyResponseJQ(`.fields.summary`, body); err != nil || out != "hi" {
		t.Errorf("scalar jq = %q err=%v, want hi", out, err)
	}

	// Non-JSON body: passed through (ok=false, no error).
	if _, ok, err := applyResponseJQ(`.`, []byte("<html>nope</html>")); err != nil || ok {
		t.Errorf("non-JSON body: ok=%v err=%v, want ok=false nil", ok, err)
	}
}

// TestExecuteHTTPTool_HappyPath drives a tool end-to-end against a test server:
// URL+body templating, header application, and response_jq all together.
func TestExecuteHTTPTool_HappyPath(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"echo":"` + r.URL.Query().Get("q") + `"}`))
	}))
	defer srv.Close()

	spec := HTTPToolSpec{
		Name:         "demo",
		Method:       "POST",
		URL:          srv.URL + "/issue/{id}?q={id}",
		Headers:      map[string]string{"Authorization": "Bearer SECRET-TOKEN"},
		BodyTemplate: `{"msg":"{text}"}`,
		ResponseJQ:   ".echo",
	}
	res, err := executeHTTPTool(context.Background(), srv.Client(), spec, map[string]interface{}{
		"id":   "AB-1",
		"text": "hello",
	})
	if err != nil {
		t.Fatalf("executeHTTPTool: %v", err)
	}
	if gotPath != "/issue/AB-1" {
		t.Errorf("path = %q, want /issue/AB-1", gotPath)
	}
	if gotAuth != "Bearer SECRET-TOKEN" {
		t.Errorf("auth header = %q, want the secret to reach the SERVER", gotAuth)
	}
	if gotBody != `{"msg":"hello"}` {
		t.Errorf("body = %q, want templated", gotBody)
	}
	if res.IsError {
		t.Error("2xx should not be IsError")
	}
	if got := res.Content[0].Text; got != "AB-1" {
		t.Errorf("jq-filtered result = %q, want AB-1", got)
	}
}

// TestExecuteHTTPTool_Non2xx asserts a non-2xx is returned to the model as
// "status <N>: <body>" with IsError=true rather than as a transport error.
func TestExecuteHTTPTool_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such ticket"))
	}))
	defer srv.Close()

	res, err := executeHTTPTool(context.Background(), srv.Client(), HTTPToolSpec{
		Name: "demo", Method: "GET", URL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatalf("non-2xx must NOT be a transport error, got: %v", err)
	}
	if !res.IsError {
		t.Error("non-2xx should set IsError=true")
	}
	if got := res.Content[0].Text; got != "status 404: no such ticket" {
		t.Errorf("result = %q, want \"status 404: no such ticket\"", got)
	}
}

// TestHTTPToolSecretsNotExposedToModel is the security regression guard: the
// model-facing surface (the registered Tool descriptor returned by GetAllTools
// and the tool RESULT) must never carry the auth header value. The secret lives
// only in the spec's Headers and is written onto the outbound request — it must
// not leak into the schema, description, or response.
func TestHTTPToolSecretsNotExposedToModel(t *testing.T) {
	const secret = "super-secret-token-value"

	var sawSecretOnWire bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), secret) {
			sawSecretOnWire = true
		}
		w.Header().Set("Content-Type", "application/json")
		// The upstream echoes nothing sensitive; the response the model sees is benign.
		_, _ = w.Write([]byte(`{"result":"done"}`))
	}))
	defer srv.Close()

	c := NewClient()
	c.AddHTTPTools([]HTTPToolSpec{{
		Name:        "secret_tool",
		Description: "A tool whose auth is a host-side secret",
		Method:      "GET",
		URL:         srv.URL + "/do",
		Headers:     map[string]string{"Authorization": "Bearer " + secret},
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}})

	// 1. The catalog the model sees must not contain the secret anywhere.
	tools := c.GetAllTools()
	if len(tools) != 1 {
		t.Fatalf("GetAllTools len = %d, want 1", len(tools))
	}
	st := tools[0]
	if st.ServerName != HTTPToolServerName {
		t.Errorf("server name = %q, want %q", st.ServerName, HTTPToolServerName)
	}
	descBytes := st.Tool.Name + "\x00" + st.Tool.Description
	if strings.Contains(descBytes, secret) {
		t.Error("secret leaked into the tool name/description the model sees")
	}
	for k, v := range st.Tool.InputSchema {
		if strings.Contains(k, secret) {
			t.Errorf("secret leaked into input schema key %q", k)
		}
		if s, ok := v.(string); ok && strings.Contains(s, secret) {
			t.Error("secret leaked into input schema value")
		}
	}

	// 2. Dispatch the call through the SAME routing the agent loop uses; the
	//    result the model sees must not contain the secret either.
	res, err := c.CallToolOn(context.Background(), HTTPToolServerName, "secret_tool", map[string]interface{}{})
	if err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if res.Content[0].Text != `{"result":"done"}` {
		t.Errorf("result = %q", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, secret) {
		t.Error("secret leaked into the tool result returned to the model")
	}
	// 3. ...but the secret DID reach the upstream server on the wire (host-side).
	if !sawSecretOnWire {
		t.Error("auth header did not reach the upstream server; host-side credential application broke")
	}

	// 4. CallToolPrefixed (the mcp_<server>_<tool> path) also routes correctly.
	if _, err := c.CallToolPrefixed(context.Background(), "mcp__http_secret_tool", map[string]interface{}{}); err != nil {
		t.Errorf("CallToolPrefixed: %v", err)
	}
}

// TestAddHTTPToolsIdempotentAppend asserts repeated registration appends to the
// same synthetic server rather than dropping earlier tools or double-spawning.
func TestAddHTTPToolsIdempotentAppend(t *testing.T) {
	c := NewClient()
	c.AddHTTPTools([]HTTPToolSpec{{Name: "a", Method: "GET", URL: "http://x/a"}})
	c.AddHTTPTools([]HTTPToolSpec{{Name: "b", Method: "GET", URL: "http://x/b"}})
	if got := len(c.GetAllTools()); got != 2 {
		t.Fatalf("GetAllTools len = %d, want 2 (append, not replace)", got)
	}
	// A skipped empty-name spec must not register.
	c.AddHTTPTools([]HTTPToolSpec{{Name: "", Method: "GET", URL: "http://x/c"}})
	if got := len(c.GetAllTools()); got != 2 {
		t.Errorf("empty-name spec registered a tool; len = %d, want 2", got)
	}
}
