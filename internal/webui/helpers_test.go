// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

const (
	testSecret     = "test-session-secret-0123456789abcdef"
	testChatToken  = "chat-shared-secret"
	testOrchToken  = "orch-shared-secret"
	testIndexHTML  = `<!doctype html><html><head><script src="/theme-init.js"></script>` + headMarker + `<link rel="stylesheet" href="/api/theme"></head><body><div id="root"></div>` + bootstrapMarker + `<script type="module" src="/assets/index-abc123.js"></script></body></html>`
	testBuildID    = "build-123"
	testAssetJS    = "console.log('fleet')"
	testOrigin     = "https://fleet.example.com"
	testTeamLinkID = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

// testBundle is a minimal built SPA.
func testBundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(testIndexHTML)},
		"build-id.txt":           {Data: []byte(testBuildID + "\n")},
		"assets/index-abc123.js": {Data: []byte(testAssetJS)},
		"theme-init.js":          {Data: []byte("/* theme */")},
		"sw.js":                  {Data: []byte("/* sw */")},
		"icons/core-icons.svg":   {Data: []byte("<svg/>")},
	}
}

// recordedCall is one request a fake backend received.
type recordedCall struct {
	Method     string
	URI        string
	Path       string
	RawPath    string
	Header     http.Header
	Body       string
	RemoteAddr string
}

// fakeBackend records requests and answers with respond (default: 200 {}).
type fakeBackend struct {
	mu      sync.Mutex
	calls   []recordedCall
	respond func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{r.Method, r.RequestURI, r.URL.Path, r.URL.RawPath, r.Header.Clone(), string(body), r.RemoteAddr})
	f.mu.Unlock()
	if f.respond != nil {
		f.respond(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func (f *fakeBackend) last(t *testing.T) recordedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("backend was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type testEnv struct {
	g      *Gateway
	chat   *fakeBackend
	orch   *fakeBackend
	priv   ed25519.PrivateKey
	pubB64 string
}

type envOpt func(*Config, *Options)

func withOrigin(o string) envOpt {
	return func(c *Config, _ *Options) { c.PublicOrigin = parseOrigin(o) }
}

func withOIDC(o *OIDCConfig) envOpt { return func(c *Config, _ *Options) { c.OIDC = o } }

func withBundle(b BundleFS) envOpt { return func(_ *Config, opts *Options) { opts.Bundle = b } }

func newTestEnv(t *testing.T, opts ...envOpt) *testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{chat: &fakeBackend{}, orch: &fakeBackend{}, priv: priv, pubB64: base64.StdEncoding.EncodeToString(pub)}
	cfg := Config{
		SessionSecret:     testSecret,
		ElcanoPubKeyB64:   e.pubB64,
		ChatToken:         testChatToken,
		OrchestratorToken: testOrchToken,
	}
	o := Options{Chat: e.chat, Orchestrator: e.orch, Bundle: testBundle()}
	for _, fn := range opts {
		fn(&cfg, &o)
	}
	g, err := New(cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	return e
}

// response is what the gateway wrote, captured by a recorder: an in-memory
// body with nothing to close (which is also why it is not an *http.Response —
// bodyclose would want every one of them closed).
type response struct {
	StatusCode int
	Header     http.Header
	Body       *bytes.Reader
}

// Cookies parses the Set-Cookie headers like http.Response.Cookies.
func (r *response) Cookies() []*http.Cookie {
	return (&http.Response{Header: r.Header}).Cookies()
}

func (e *testEnv) do(r *http.Request) *response {
	rec := httptest.NewRecorder()
	e.g.ServeHTTP(rec, r)
	return &response{StatusCode: rec.Code, Header: rec.Header(), Body: bytes.NewReader(rec.Body.Bytes())}
}

func mintHMAC(t *testing.T, email string) string {
	t.Helper()
	tok, err := websession.NewHMAC(testSecret).Mint(email, "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *testEnv) mintElcano(t *testing.T, email string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"email": email, "tenant": "", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(e.priv, []byte(body)))
}

func req(method, target string, body string, headers ...string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Host = "fleet.example.com"
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

func withCookie(r *http.Request, name, value string) *http.Request {
	r.AddCookie(&http.Cookie{Name: name, Value: value})
	return r
}

func signedIn(t *testing.T, r *http.Request) *http.Request {
	t.Helper()
	return withCookie(r, websession.CookieName, mintHMAC(t, "alice@example.com"))
}

func readBody(t *testing.T, res *response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setCookies(res *response) string {
	return strings.Join(res.Header.Values("Set-Cookie"), "\n")
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
