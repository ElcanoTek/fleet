// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Ported from the brand/theme/manifest route tests and serverBranding.test.ts,
// the model-catalog/rankings/check route tests and openrouterModels.test.ts /
// catwalkModels.test.ts, and the discuss route.

func TestThemeIsAlwaysValidCSS(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Chat-Server-Token") != testChatToken || r.Header.Get("X-User-Email") != "" {
			t.Errorf("theme must be token-only: %v", r.Header)
		}
		_, _ = io.WriteString(w, `html:root[data-theme="dark"]{--color-bg:#000}`)
	}
	res := e.do(req("GET", "/api/theme", ""))
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/css; charset=utf-8" || res.Header.Get("Cache-Control") != "no-cache" || res.Header.Get("ETag") == "" || !strings.Contains(readBody(t, res), "--color-bg") {
		t.Fatalf("%d %v", res.StatusCode, res.Header)
	}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	res = e.do(req("GET", "/api/theme", ""))
	if res.StatusCode != 200 || readBody(t, res) != "" {
		t.Fatalf("backend error must yield empty CSS: %d", res.StatusCode)
	}
}

func TestBrandAssetsAllowlistAndFallback(t *testing.T) {
	e := newTestEnv(t)
	serve := func(ct string, status int) {
		e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "IMG")
		}
	}
	serve("image/svg+xml; charset=utf-8", 200)
	res := e.do(req("GET", "/api/brand/logo", ""))
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/svg+xml" || res.Header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" || readBody(t, res) != "IMG" || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("logo: %d %v", res.StatusCode, res.Header)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("logo without nosniff: %v", res.Header)
	}
	// SVG is not an acceptable og:image.
	res = e.do(req("GET", "/api/brand/share-image", ""))
	if res.StatusCode != 307 || res.Header.Get("Location") != "http://fleet.example.com/share.png" {
		t.Fatalf("share svg: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	serve("image/png", 200)
	if res := e.do(req("GET", "/api/brand/share-image", "")); res.StatusCode != 200 || res.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("share png: %d", res.StatusCode)
	}
	serve("text/html", 200)
	// A markup body is never passed through: the redirect carries none of it.
	if res := e.do(req("GET", "/api/brand/logo", "")); res.StatusCode != 307 || !strings.HasSuffix(res.Header.Get("Location"), "/logos/fleet-mark.svg") || strings.Contains(readBody(t, res), "IMG") {
		t.Fatalf("html logo: %d", res.StatusCode)
	}
	serve("image/png", 404)
	if res := e.do(req("GET", "/api/brand/logo", "")); res.StatusCode != 307 {
		t.Fatalf("no logo: %d", res.StatusCode)
	}
}

func TestManifestFromBranding(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"app_name":"Reklaim","background_dark":"#0A0908"}`)
	}
	res := e.do(req("GET", "/manifest.webmanifest", ""))
	var m map[string]any
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Content-Type") != "application/manifest+json" || m["name"] != "Reklaim" || m["short_name"] != "Reklaim" || m["theme_color"] != "#0A0908" || m["start_url"] != "/" || m["display"] != "standalone" {
		t.Fatalf("%v", m)
	}
	icons := m["icons"].([]any)
	if icons[0].(map[string]any)["src"] != "/api/brand/logo" || icons[1].(map[string]any)["purpose"] != "maskable" {
		t.Fatalf("%v", icons)
	}
	// Backend down: still a complete manifest with fleet's defaults.
	e = newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }
	_ = json.NewDecoder(e.do(req("GET", "/manifest.webmanifest", "")).Body).Decode(&m)
	if m["name"] != "Fleet" || m["background_color"] != defaultBackgroundDark {
		t.Fatalf("%v", m)
	}
}

func TestBrandingDefaultsAndCaching(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"app_name":"  ","login_title":7,"share_title":"","background_light":"  #fff  "}`)
	}
	b := e.g.branding.get()
	if b.AppName != "Fleet" || b.LoginTitle != DefaultBranding.LoginTitle || b.ShareTitle != "Fleet" || b.BackgroundLight != "#fff" {
		t.Fatalf("%+v", b)
	}
	n := e.chat.count()
	e.g.branding.get()
	if e.chat.count() != n {
		t.Fatal("branding not memoized")
	}
}

// fixedCatalog is a ModelCatalog seam.
type fixedCatalog struct {
	entries []agentcore.ModelInfo
	at      time.Time
}

func (f fixedCatalog) AllEntries() []agentcore.ModelInfo { return f.entries }
func (f fixedCatalog) LastFetchedAt() time.Time          { return f.at }

func entry(id string, completion float64, created int64, mods ...string) agentcore.ModelInfo {
	return agentcore.ModelInfo{ID: id, Name: id, CompletionPerToken: completion, PromptPerToken: completion / 2, HasCompletionPrice: true, Created: created, OutputModalities: mods}
}

func TestModelCatalogAndRankings(t *testing.T) {
	e := newTestEnv(t)
	at := time.UnixMilli(1_700_000_000_000)
	free := entry("z-ai/glm-6:free", 0, 900)
	noPrice := agentcore.ModelInfo{ID: "openai/unpriced", Created: 999}
	e.g.models.openrouter = fixedCatalog{at: at, entries: []agentcore.ModelInfo{
		entry("anthropic/old", 0.00001, 100),
		entry("anthropic/new", 0.000002, 200),
		entry(agentcore.DefaultCoreModel, 0.00009, 300),
		entry("openai/image-gen", 0.0001, 500, "image"),
		entry("openai/chat", 0.00003, 400, "text"),
		free, noPrice,
		entry("z-ai/glm-5", 0.000001, 800),
		entry("meta/llama", 0.5, 1000),
	}}
	res := e.do(signedIn(t, req("GET", "/api/model-catalog", "")))
	var cat struct {
		Models []catalogModel `json:"models"`
		At     int64          `json:"cached_at"`
	}
	_ = json.NewDecoder(res.Body).Decode(&cat)
	ids := make([]string, 0, len(cat.Models))
	for _, m := range cat.Models {
		ids = append(ids, m.Slug)
	}
	// Text-only, priced, most expensive first.
	if strings.Join(ids, ",") != "meta/llama,"+agentcore.DefaultCoreModel+",openai/chat,anthropic/old,anthropic/new,z-ai/glm-5,z-ai/glm-6:free" || cat.At != at.UnixMilli() {
		t.Fatalf("%v %d", ids, cat.At)
	}
	res = e.do(signedIn(t, req("GET", "/api/model-rankings", "")))
	var rk struct {
		Models []rankedModel `json:"models"`
	}
	_ = json.NewDecoder(res.Body).Decode(&rk)
	ids = make([]string, 0, len(rk.Models))
	for _, m := range rk.Models {
		ids = append(ids, m.Slug)
	}
	// One per major lab in MAJOR_LABS order, newest, pinned/free/non-text out;
	// meta is not a major lab.
	if strings.Join(ids, ",") != "z-ai/glm-5,anthropic/new,openai/chat" {
		t.Fatalf("%v", ids)
	}
	// Empty catalog: 502 with the reason.
	e.g.models.openrouter = fixedCatalog{}
	if res := e.do(signedIn(t, req("GET", "/api/model-catalog", ""))); res.StatusCode != 502 {
		t.Fatalf("%d", res.StatusCode)
	}
	if res := e.do(req("GET", "/api/model-catalog", "")); res.StatusCode != 401 {
		t.Fatalf("unauth: %d", res.StatusCode)
	}
}

func TestModelCheck(t *testing.T) {
	e := newTestEnv(t)
	e.g.models.openrouter = fixedCatalog{entries: []agentcore.ModelInfo{{ID: "x/known", CanonicalSlug: "x/known-2026", CompletionPerToken: 0.000002, HasCompletionPrice: true}}}
	route := `{"allowed":true,"provider_type":"openrouter"}`
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, route) }
	check := func(q string) map[string]any {
		var m map[string]any
		_ = json.NewDecoder(e.do(signedIn(t, req("GET", "/api/model-check"+q, ""))).Body).Decode(&m)
		return m
	}
	if m := check("?slug=+"); m["allowed"] != false || m["reason"] != "empty" || m["models_url"] != modelsPageURL {
		t.Fatalf("%v", m)
	}
	m := check("?slug=x/known-2026")
	if m["allowed"] != true || m["known"] != true || m["completion_usd_per_million"].(float64) < 1.99 {
		t.Fatalf("%v", m)
	}
	if got := e.chat.last(t).URI; got != "/llm-provider-models?slug=x%2Fknown-2026" {
		t.Fatalf("%q", got)
	}
	if m := check("?slug=x/new"); m["allowed"] != true || m["known"] != false || m["completion_usd_per_million"] != nil {
		t.Fatalf("%v", m)
	}
	route = `{"allowed":true,"provider_type":"anthropic"}`
	if m := check("?slug=claude"); m["allowed"] != true || len(m) != 2 {
		t.Fatalf("native: %v", m)
	}
	route = `{"allowed":false,"slug":"s","reason":"provider_unavailable"}`
	if m := check("?slug=s"); m["allowed"] != false || m["reason"] != "provider_unavailable" || m["models_url"] != "/settings/admin/providers" {
		t.Fatalf("refused: %v", m)
	}
}

func TestCatwalk(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/v2/providers" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `[{"id":" anthropic ","name":"","type":"anthropic","models":[{"id":"m1","name":" ","context_window":200000,"cost_per_1m_out":-1},{"id":""}]},{"id":""}]`)
	}))
	defer srv.Close()
	e := newTestEnv(t)
	e.g.models.catwalkURL = srv.URL + "/"
	res := e.do(signedIn(t, req("GET", "/api/catwalk-models", "")))
	body := readBody(t, res)
	if res.StatusCode != 200 || strings.TrimSpace(body) != `{"providers":[{"id":"anthropic","name":"anthropic","type":"anthropic","models":[{"id":"m1","name":"m1","context_window":200000}]}]}` {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	e.do(signedIn(t, req("GET", "/api/catwalk-models", "")))
	if hits != 1 {
		t.Fatalf("catwalk not cached: %d", hits)
	}
	if _, err := parseCatwalkProviders([]byte(`{}`)); err == nil {
		t.Fatal("non-array accepted")
	}
}

func TestDiscussDigest(t *testing.T) {
	e := newTestEnv(t)
	e.orch.respond = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/tasks/"):
			_, _ = io.WriteString(w, `{"id":"t1","name":"","prompt":"Summarize the inbox","status":"done","result":"ok","artifacts":[{"name":"a.csv"},null]}`)
		case strings.HasPrefix(r.URL.Path, "/logs/"):
			_, _ = io.WriteString(w, `{"cost":0.12345,"messages":[{"role":"user","content":"hi"},{"tool_name":"bash","is_error":true,"content":"boom"}]}`)
		}
	}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ID":"c-77"}`) }
	res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks/t1/discuss", "", "Origin", "http://fleet.example.com")))
	if body := readBody(t, res); res.StatusCode != 200 || strings.TrimSpace(body) != `{"conversation_id":"c-77"}` {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var seed struct{ Title, Seed string }
	_ = json.Unmarshal([]byte(e.chat.last(t).Body), &seed)
	for _, want := range []string{"- ID: t1", "- Status: done", "- Cost: $0.1235", "## Result\nok", "## Artifacts\n- a.csv", "[user] hi", "[tool:bash (error)] boom"} {
		if !strings.Contains(seed.Seed, want) {
			t.Errorf("seed lacks %q:\n%s", want, seed.Seed)
		}
	}
	if seed.Title != "Discuss run: Summarize the inbox" {
		t.Fatalf("%q", seed.Title)
	}
	// Task not readable: its status, with the generic error.
	e.orch.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }
	if res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks/t1/discuss", "", "Origin", "http://fleet.example.com"))); res.StatusCode != 404 {
		t.Fatalf("%d", res.StatusCode)
	}
}

func TestDigestBudget(t *testing.T) {
	var log discussLog
	for i := 0; i < 50; i++ {
		log.Messages = append(log.Messages, struct {
			Role     string `json:"role"`
			Content  string `json:"content"`
			ToolName string `json:"tool_name"`
			IsError  bool   `json:"is_error"`
		}{Role: "assistant", Content: strings.Repeat("x", 3000)})
	}
	d := formatDigest(discussTask{ID: "t"}, log)
	if !strings.Contains(d, "earlier transcript message(s) omitted") || !strings.Contains(d, "[…truncated…]") || len(d) > 40_000 {
		t.Fatalf("len %d", len(d))
	}
	if !strings.Contains(formatDigest(discussTask{ID: "t"}, discussLog{}), "(empty transcript)") {
		t.Fatal("empty transcript")
	}
}

// A scheduled run's transcript routinely exceeds a JSON envelope; Discuss
// reads it whole (a truncated body would be invalid JSON and a 502).
func TestDiscussReadsLargeRuns(t *testing.T) {
	e := newTestEnv(t)
	big := strings.Repeat("x", 3<<20) // 3 MiB of output
	e.orch.respond = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/tasks/"):
			_, _ = io.WriteString(w, `{"id":"t1","name":"Big","status":"done","result":"`+big+`"}`)
		case strings.HasPrefix(r.URL.Path, "/logs/"):
			_, _ = io.WriteString(w, `{"messages":[{"role":"assistant","content":"`+big+`"}]}`)
		}
	}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"c-1"}`) }
	res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks/t1/discuss", "", "Origin", "http://fleet.example.com")))
	if body := readBody(t, res); res.StatusCode != 200 || !strings.Contains(body, "c-1") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// A revoked session on the Discuss lookups clears the stale cookie, as the
// proxy path does.
func TestDiscussClearsRevokedSession(t *testing.T) {
	for _, which := range []string{"/tasks/", "/logs/"} {
		e := newTestEnv(t)
		e.orch.respond = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, which) {
				w.Header().Set("X-Session-Revoked", "1")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		}
		res := e.do(signedIn(t, req("POST", "/api/orchestrator/tasks/t1/discuss", "", "Origin", "http://fleet.example.com")))
		if res.StatusCode != http.StatusUnauthorized || !strings.Contains(setCookies(res), "elcano_session=;") {
			t.Fatalf("%s: %d %q", which, res.StatusCode, setCookies(res))
		}
	}
}

// TestCatwalkSharedFetch pins #2041: concurrent callers share ONE outbound
// fetch, the cache mutex is not held across it, and a caller whose context
// ends stops waiting without failing the others.
func TestCatwalkSharedFetch(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = io.WriteString(w, `[{"id":"p","type":"openai","models":[{"id":"m"}]}]`)
	}))
	defer srv.Close()
	m := newModelCatalogs(srv.Client(), srv.URL)

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			providers, err := m.loadCatwalk(context.Background())
			if err == nil && (len(providers) != 1 || providers[0].ID != "p") {
				err = fmt.Errorf("providers %+v", providers)
			}
			errs <- err
		}()
	}
	// Wait until the shared fetch is on the wire.
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// The mutex is free while the fetch is in flight.
	if !m.mu.TryLock() {
		t.Fatal("catwalk cache mutex held across the fetch")
	}
	m.mu.Unlock()
	// A caller that gives up returns its own context error promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.loadCatwalk(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("outbound fetches = %d, want 1", got)
	}
}

// TestCatwalkFailureCached pins the #2041 negative cache: a failed fetch is
// served from cache for catwalkFailureTTL, then retried.
func TestCatwalkFailureCached(t *testing.T) {
	var hits atomic.Int32
	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if !ok.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `[{"id":"p","models":[]}]`)
	}))
	defer srv.Close()
	m := newModelCatalogs(srv.Client(), srv.URL)

	if _, err := m.loadCatwalk(context.Background()); err == nil {
		t.Fatal("failing catwalk returned no error")
	}
	if _, err := m.loadCatwalk(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("cached failure: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("outbound fetches within the failure TTL = %d, want 1", got)
	}
	// Past the TTL the fetch is retried and a success replaces the failure.
	ok.Store(true)
	m.mu.Lock()
	m.catwalkErrAt = time.Now().Add(-catwalkFailureTTL - time.Second)
	m.mu.Unlock()
	if providers, err := m.loadCatwalk(context.Background()); err != nil || len(providers) != 1 {
		t.Fatalf("retry after TTL: %v %+v", err, providers)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("outbound fetches after the TTL = %d, want 2", got)
	}
}
