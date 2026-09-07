package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/netguard"
)

// TestSubstituteTokens covers {param} substitution in each context: URL
// (percent-encoded so a value can't inject path/query structure), JSON body
// (JSON-string-escaped so a value can't inject fields, #600), and raw body
// (verbatim, for non-JSON bodies where the author owns the framing). Unknown
// tokens are left intact rather than blanked.
func TestSubstituteTokens(t *testing.T) {
	args := map[string]interface{}{
		"ticket_id": "PROJ 123/x?y",
		"count":     7,
	}
	if got := substituteTokens("/issue/{ticket_id}", args, substModeURL); got != "/issue/PROJ+123%2Fx%3Fy" {
		t.Errorf("url substitution = %q, want percent-encoded", got)
	}
	// Benign values are untouched by the JSON escape; an unquoted numeric
	// token stays a bare number.
	if got := substituteTokens(`{"id":"{ticket_id}","n":{count}}`, args, substModeJSONBody); got != `{"id":"PROJ 123/x?y","n":7}` {
		t.Errorf("json body substitution = %q", got)
	}
	if got := substituteTokens("id={ticket_id}", args, substModeRawBody); got != "id=PROJ 123/x?y" {
		t.Errorf("raw body substitution = %q, want verbatim", got)
	}
	// Unknown token preserved.
	if got := substituteTokens("/x/{unknown}", args, substModeURL); got != "/x/{unknown}" {
		t.Errorf("unknown token = %q, want left intact", got)
	}
	// No braces: passthrough.
	if got := substituteTokens("/static", args, substModeURL); got != "/static" {
		t.Errorf("no-token passthrough = %q", got)
	}
}

// TestSubstituteTokens_JSONBodyInjection is the #600 regression guard: a
// model-steered string arg carrying JSON punctuation must not break out of the
// template author's quoting and add fields to the outbound (credentialed)
// request. The body must stay valid JSON with the payload contained in the
// intended field, byte-for-byte round-trippable.
func TestSubstituteTokens_JSONBodyInjection(t *testing.T) {
	const payload = `x","admin":true,"y":"`
	got := substituteTokens(`{"channel":"{channel}","text":"{text}"}`, map[string]interface{}{
		"channel": "C123",
		"text":    payload,
	}, substModeJSONBody)

	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, got)
	}
	if len(decoded) != 2 {
		t.Errorf("body gained fields: %v (injection!)", decoded)
	}
	if _, injected := decoded["admin"]; injected {
		t.Error("\"admin\" field injected into the body")
	}
	if decoded["text"] != payload {
		t.Errorf("text = %q, want the payload contained verbatim %q", decoded["text"], payload)
	}

	// A non-string arg abused in a QUOTED position must also stay contained:
	// a map's structural quotes are escaped, never live.
	got = substituteTokens(`{"text":"{obj}"}`, map[string]interface{}{
		"obj": map[string]interface{}{"admin": true},
	}, substModeJSONBody)
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("map-arg body is not valid JSON: %v\nbody: %s", err, got)
	}
	if _, injected := decoded["admin"]; injected {
		t.Error("map arg injected a live \"admin\" field")
	}
}

// TestBodyMode covers the JSON-body detection: a declared Content-Type is
// authoritative in either direction; without one the template shape decides.
func TestBodyMode(t *testing.T) {
	cases := []struct {
		name string
		spec HTTPToolSpec
		want substMode
	}{
		{"json content-type", HTTPToolSpec{Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, BodyTemplate: "text={p}"}, substModeJSONBody},
		{"non-json content-type opts out", HTTPToolSpec{Headers: map[string]string{"content-type": "application/x-www-form-urlencoded"}, BodyTemplate: `{"x":"{p}"}`}, substModeRawBody},
		{"undeclared json object sniffed", HTTPToolSpec{BodyTemplate: ` {"x":"{p}"}`}, substModeJSONBody},
		{"undeclared json array sniffed", HTTPToolSpec{BodyTemplate: `["{p}"]`}, substModeJSONBody},
		{"undeclared non-json stays raw", HTTPToolSpec{BodyTemplate: "text={p}"}, substModeRawBody},
	}
	for _, tc := range cases {
		if got := bodyMode(tc.spec); got != tc.want {
			t.Errorf("%s: bodyMode = %v, want %v", tc.name, got, tc.want)
		}
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

// TestExecuteHTTPTool_JSONBodyInjection drives the #600 fix end-to-end: an
// inline tool with a JSON body template receives a hostile arg, and the body
// that reaches the upstream server must be valid JSON with the payload
// contained in its intended field — no injected keys.
func TestExecuteHTTPTool_JSONBodyInjection(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	const payload = `x","admin":true,"y":"`
	spec := HTTPToolSpec{
		Name:         "post_msg",
		Method:       "POST",
		URL:          srv.URL,
		Headers:      map[string]string{"Content-Type": "application/json"},
		BodyTemplate: `{"channel":"{channel}","text":"{text}"}`,
	}
	if _, err := executeHTTPTool(context.Background(), srv.Client(), spec, map[string]interface{}{
		"channel": "C123",
		"text":    payload,
	}); err != nil {
		t.Fatalf("executeHTTPTool: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("outbound body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if _, injected := decoded["admin"]; injected {
		t.Errorf("injected \"admin\" field reached the wire: %s", gotBody)
	}
	if decoded["text"] != payload {
		t.Errorf("text = %q, want the payload contained verbatim", decoded["text"])
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
	allowLoopbackHTTPToolDial(t)
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

// TestAddHTTPTools_RepeatRegistrationDedupes: re-registering the same tool
// name (the documented mid-session re-load) must update the catalog entry in
// place, not append a duplicate — the spec map already overwrote the request
// behavior, so each repeat registration handed the model another identical
// catalog entry (#1108).
func TestAddHTTPTools_RepeatRegistrationDedupes(t *testing.T) {
	c := NewClient()
	c.AddHTTPTools([]HTTPToolSpec{{Name: "a", Method: "GET", URL: "http://x/a", Description: "v1"}})
	c.AddHTTPTools([]HTTPToolSpec{
		{Name: "a", Method: "GET", URL: "http://x/a2", Description: "v2"},
		{Name: "b", Method: "GET", URL: "http://x/b"},
	})
	tools := c.GetAllTools()
	if len(tools) != 2 {
		t.Fatalf("GetAllTools len = %d, want 2 (a deduped + b)", len(tools))
	}
	var aDesc string
	for _, st := range tools {
		if st.Tool.Name == "a" {
			aDesc = st.Tool.Description
		}
	}
	if aDesc != "v2" {
		t.Errorf("catalog entry for re-registered tool = %q, want the latest registration (v2)", aDesc)
	}
}

// A documented mid-session reload can register HTTP tools while an _http call
// is in flight. tools/call reads the spec map under Server.mu only, so without
// the transport's own lock this is a concurrent map read+write — a fatal
// runtime error in the credential-brokering process. Run under -race.
func TestAddHTTPTools_ConcurrentWithCall(t *testing.T) {
	t.Parallel()
	c := NewClient()
	c.AddHTTPTools([]HTTPToolSpec{{Name: "seed", Method: "GET", URL: "http://127.0.0.1:0/x"}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			c.AddHTTPTools([]HTTPToolSpec{{Name: fmt.Sprintf("t%d", i), Method: "GET", URL: "http://127.0.0.1:0/x"}})
		}
	}()
	for i := 0; i < 200; i++ {
		// The named tool is never registered; the call exercises only the
		// map lookup path, which is the racing read.
		_, _ = c.CallToolOn(context.Background(), HTTPToolServerName, "missing", nil)
	}
	<-done
}

// allowLoopbackHTTPToolDial substitutes a plain dialer for the production SSRF
// guard so a test can drive the synthetic server's OWN client (the CallToolOn
// path) against an httptest server, which listens on loopback — an address the
// guard refuses. Restored on cleanup.
func allowLoopbackHTTPToolDial(t *testing.T) {
	t.Helper()
	prev := httpToolDialContext
	httpToolDialContext = (&net.Dialer{}).DialContext
	t.Cleanup(func() { httpToolDialContext = prev })
}

// TestHTTPToolDialContext_RefusesInternalAddresses pins the production dialer
// to the netguard blocklist: loopback, RFC 1918 and the cloud-metadata
// link-local address are refused before any connection is attempted. This is
// the invariant the inline-HTTP-tool client was missing while it rode
// http.DefaultTransport.
func TestHTTPToolDialContext_RefusesInternalAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:1", "[::1]:1", "10.0.0.1:80", "169.254.169.254:80", "100.100.100.200:80"} {
		conn, err := httpToolDialContext(context.Background(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, errHTTPToolBlockedAddress) {
			t.Errorf("dial %s: err = %v, want errHTTPToolBlockedAddress", addr, err)
		}
	}
}

// TestHTTPToolClient_UsesGuardedTransport asserts the lazily built client
// carries its own Transport (the guarded one) rather than falling through to
// http.DefaultTransport, and that the build happens exactly once under
// concurrent first use.
func TestHTTPToolClient_UsesGuardedTransport(t *testing.T) {
	tr := &httpToolTransport{tools: map[string]HTTPToolSpec{}}
	var first *http.Client
	done := make(chan *http.Client, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- tr.httpClient() }()
	}
	for i := 0; i < 8; i++ {
		c := <-done
		if first == nil {
			first = c
		} else if c != first {
			t.Fatal("httpClient built more than one client")
		}
	}
	ht, ok := first.Transport.(*http.Transport)
	if !ok || ht == nil {
		t.Fatalf("Transport = %T, want a dedicated *http.Transport (not DefaultTransport)", first.Transport)
	}
	if ht.DialContext == nil {
		t.Fatal("Transport.DialContext is nil; the SSRF guard is not wired")
	}
	if first.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil; cross-origin header stripping is not wired")
	}
}

// TestExecuteHTTPTool_RedirectToInternalAddressIsRefused drives the real
// redirect path: the origin (admitted here so the test can use httptest) 302s
// to the cloud-metadata address. stripHeadersOnCrossOriginRedirect drops the
// credential headers on that hop, but only the guarded dialer stops the FETCH
// itself — with DefaultTransport the metadata document would have come back to
// the model.
func TestExecuteHTTPTool_RedirectToInternalAddressIsRefused(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer origin.Close()

	// Production classifier, minus loopback so the httptest origin is reachable.
	client := newHTTPToolClient(guardedDialContext(func(ip net.IP) bool {
		return !ip.IsLoopback() && netguard.IsBlockedIP(ip)
	}))
	spec := HTTPToolSpec{Name: "bounce", Method: "GET", URL: origin.URL + "/go", Headers: map[string]string{"X-Api-Key": "resolved-secret"}}
	res, err := executeHTTPTool(context.Background(), client, spec, nil)
	if err == nil {
		t.Fatalf("expected the redirect hop to be refused, got result %+v", res)
	}
	if !errors.Is(err, errHTTPToolBlockedAddress) {
		t.Fatalf("err = %v, want errHTTPToolBlockedAddress", err)
	}
	if strings.Contains(err.Error(), "resolved-secret") {
		t.Fatalf("error text echoes the credential: %v", err)
	}
}
