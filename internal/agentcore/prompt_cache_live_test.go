package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// Live prompt-cache regression suite: drives the real Run loop (fleet's own
// breakpoints, openRouterCacheControlTransport wire normalization, upstream
// pins, persisted runtime_today notes, and realistic multi-tool rosters)
// against OpenRouter and asserts per-request cache reads and prefix growth.
//
// Two live tests run when FLEET_CACHE_LIVE=1 and OPENROUTER_API_KEY are set:
//
//  1. TestPromptCacheLive — runs across Fleet's live prompt-cache model set
//     (default fast/low-cost CI tier: anthropic/claude-haiku-5.5 and
//     google/gemini-3.8-flash, which exercise both explicit cache-control
//     wire families; pass FLEET_CACHE_LIVE_MODELS=all for the full 5-model
//     flagship matrix including google/gemini-4-hydrogen,
//     anthropic/claude-opus-5.5, and openai/gpt-6-luna-pro, or a comma-separated
//     model list). For each model, it executes a
//     realistic 3-turn production conversation with a 6-tool Fleet roster:
//     - Turn 1: a 3-step sequential tool chain (tool_search -> query_metrics
//     -> inspect_box -> final synthesis = 4 LLM requests in Turn 1), where
//     each tool returns ~1,200-1,400 tokens of structured operational data
//     so the conversation history grows by ~4,000 tokens across the turn.
//     - Turn 2: replays Turn 1 via liveReplayEntries (coalescing assistant
//     text + tool calls and stripping thinking blocks, matching
//     agent.replayHistory), sends a ~1,300-token follow-up user prompt,
//     and executes read_runbook (2 LLM requests in Turn 2).
//     - Turn 3: replays Turn 2 and runs a final cross-turn synthesis request.
//
//  2. TestPromptCacheLive_LongToolChainCrossTurn — runs a 10-step sequential
//     pagination tool chain in Turn 1 (>20 content blocks across assistant
//     tool_calls and tool results, exceeding Anthropic's 20-block lookback
//     window) followed by Turn 2 after history replay, verifying that:
//     - Rolling breakpoints cache the growing tool chain step-by-step.
//     - The 4th breakpoint on runtime_today is emitted on long turns.
//     - Turn 2 Step 0 still hits the cached prefix even after >20 content
//     blocks and stripped thinking blocks in Turn 1.
//
//     FLEET_CACHE_LIVE=1 OPENROUTER_API_KEY=… go test -tags fleet_host_executor \
//     -run TestPromptCacheLive -v ./internal/agentcore/
var defaultLiveCacheModels = []string{
	"anthropic/claude-haiku-5.5", // Anthropic fast tier (explicit rolling 4-breakpoint cache + content-block normalization)
	"google/gemini-3.8-flash",    // Google Gemini explicit stable-prefix + implicit cache
}

var fullLiveCacheModels = []string{
	DefaultCoreModel,             // google/gemini-4-hydrogen (Google implicit prefix cache, default frontier tier)
	"anthropic/claude-haiku-5.5", // Anthropic fast tier (explicit rolling 4-breakpoint cache)
	"google/gemini-3.8-flash",    // Google Gemini explicit stable-prefix + implicit cache
	DefaultMaxModel,              // anthropic/claude-opus-5.5 (Anthropic flagship, explicit rolling 4-breakpoint cache)
	"openai/gpt-6-luna-pro",      // OpenAI implicit prefix cache + sticky provider routing
}

func resolveLiveCacheModels(env string) []string {
	v := strings.TrimSpace(env)
	if v == "" {
		return append([]string(nil), defaultLiveCacheModels...)
	}
	if strings.EqualFold(v, "all") {
		return append([]string(nil), fullLiveCacheModels...)
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if slug := strings.TrimSpace(p); slug != "" {
			out = append(out, slug)
		}
	}
	return out
}

func TestResolveLiveCacheModels(t *testing.T) {
	if got := resolveLiveCacheModels(""); len(got) != 2 || got[0] != "anthropic/claude-haiku-5.5" || got[1] != "google/gemini-3.8-flash" {
		t.Fatalf("resolveLiveCacheModels(\"\") = %v, want fast 2-model default tier", got)
	}
	if got := resolveLiveCacheModels("all"); len(got) != 5 || got[0] != DefaultCoreModel || got[3] != DefaultMaxModel {
		t.Fatalf("resolveLiveCacheModels(\"all\") = %v, want full 5-model matrix", got)
	}
	if got := resolveLiveCacheModels(" anthropic/claude-opus-5.5 , openai/gpt-6-luna-pro "); len(got) != 2 || got[0] != "anthropic/claude-opus-5.5" || got[1] != "openai/gpt-6-luna-pro" {
		t.Fatalf("resolveLiveCacheModels(custom) = %v", got)
	}
}

func TestPromptCacheLive(t *testing.T) {
	if os.Getenv("FLEET_CACHE_LIVE") != "1" {
		t.Skip("set FLEET_CACHE_LIVE=1 (and OPENROUTER_API_KEY) to run the live prompt-cache check")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Fatal("FLEET_CACHE_LIVE=1 needs OPENROUTER_API_KEY")
	}
	slugs := resolveLiveCacheModels(os.Getenv("FLEET_CACHE_LIVE_MODELS"))
	for _, slug := range slugs {
		t.Run(strings.ReplaceAll(slug, "/", "_"), func(t *testing.T) {
			t.Parallel()
			runPromptCacheLive(t, key, slug)
		})
	}
}

// liveCacheSystemPrompt is a deterministic ~7.5K-token system prompt with a
// per-run nonce up front, so no earlier test run's cache can be hit and turn 1
// step 0 really is cold.
func liveCacheSystemPrompt(nonce string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are Victoria, a terse operations assistant. Session nonce: %s.\n\n", nonce)
	b.WriteString("Follow tool-calling instructions literally and never skip a requested tool step. Call one tool at a time when each step depends on the previous tool's output, and keep final prose answers to one short sentence.\n\n## Reference handbook\n\n")
	for i := 1; i <= 165; i++ {
		fmt.Fprintf(&b, "%d. Policy %d: requests touching region %d must be logged with ticket prefix OPS-%04d, reviewed by the on-call lead within %d hours, and archived after %d days unless legal hold applies.\n", i, i, i%17, i*7, 1+i%12, 30+i%90)
	}
	return b.String()
}

// buildLiveCacheToolRoster constructs a realistic 6-tool Fleet roster matching
// the shape of production turns (tool_search, query_metrics, inspect_box,
// read_runbook, run_python, render_artifact). Each tool returns a deterministic
// ~1,200-1,400 token payload so every tool call step adds substantial token
// volume to the conversation history.
func buildLiveCacheToolRoster() []fantasy.AgentTool {
	toolSearch := fantasy.NewAgentTool(
		"tool_search",
		"Search the active MCP connector and telemetry dataset registry by keyword.",
		func(_ context.Context, in struct {
			Query    string `json:"query" description:"Search keywords"`
			Category string `json:"category,omitempty" description:"Optional category filter"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "tool_search results for query=%q:\n", in.Query)
			b.WriteString("PRIMARY_MATCH: dataset_id=ds_region7_latency_2026 connector=telemetry-warehouse status=ready\n")
			b.WriteString("Next required action: call query_metrics with dataset=\"ds_region7_latency_2026\".\n\n")
			for i := 1; i <= 45; i++ {
				fmt.Fprintf(&b, "- catalog_entry_%02d: connector=ops-catalog-%02d schema_version=2026.11 region_scope=global-%02d retention_days=%d\n", i, i, i%9, 30+i)
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)

	queryMetrics := fantasy.NewAgentTool(
		"query_metrics",
		"Execute a read-only telemetry query against a named dataset.",
		func(_ context.Context, in struct {
			Dataset string `json:"dataset" description:"Dataset ID from tool_search"`
			Region  int    `json:"region,omitempty" description:"Region number"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "query_metrics report for dataset=%q:\n", in.Dataset)
			b.WriteString("DEGRADED_HOST: hostname=fleet-r7-edge-04.internal service=fleet-ingress p99_ms=1840 error_rate=4.2%\n")
			b.WriteString("RUNBOOK_PATH: runbooks/region-7-failover.md\n")
			b.WriteString("Next required action: call inspect_box with hostname=\"fleet-r7-edge-04.internal\".\n\n")
			for i := 1; i <= 48; i++ {
				fmt.Fprintf(&b, "row_%02d | bucket=2026-11-03T%02d:00Z | host=fleet-r7-node-%02d.internal | p50_ms=%d | p95_ms=%d | p99_ms=%d | req_count=%d\n",
					i, i%24, i%12, 18+i%7, 45+i%19, 110+i*3, 12000+i*130)
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)

	inspectBox := fantasy.NewAgentTool(
		"inspect_box",
		"Inspect systemd unit status and recent journal logs on a production host.",
		func(_ context.Context, in struct {
			Hostname string `json:"hostname" description:"Target hostname to inspect"`
			Service  string `json:"service,omitempty" description:"Optional systemd unit name"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "inspect_box diagnostic snapshot for host=%q:\n", in.Hostname)
			b.WriteString("INCIDENT_CODE: INC-7742\n")
			b.WriteString("ROOT_CAUSE: upstream-pool-saturation on region-7 edge relay\n")
			b.WriteString("RUNBOOK_REFERENCE: runbooks/region-7-failover.md\n\n")
			for i := 1; i <= 45; i++ {
				fmt.Fprintf(&b, "2026-11-03T20:%02d:%02dZ %s fleet-ingress[%d]: warn upstream pool saturation active_conns=%d max_conns=512 queue_depth=%d trace_id=tr-%04d\n",
					i%60, (i*7)%60, in.Hostname, 4100+i, 490+i%22, 15+i%30, i*13)
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)

	readRunbook := fantasy.NewAgentTool(
		"read_runbook",
		"Read an operational runbook document by relative path.",
		func(_ context.Context, in struct {
			Path string `json:"path" description:"Runbook path"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "runbook content for path=%q:\n", in.Path)
			b.WriteString("VERIFICATION_TOKEN: REMEDIATION-R7-VERIFIED\n")
			b.WriteString("ESCALATION_SLA: 15 minutes\n")
			b.WriteString("DRAIN_COMMAND: fleet-relay drain --region 7 --grace 60s\n\n")
			for i := 1; i <= 45; i++ {
				fmt.Fprintf(&b, "Step %02d: Verify regional health gate G-%03d before shifting %d%% of edge traffic across standby pool S-%02d.\n",
					i, i*3, (i*5)%100, i%8)
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)

	runPython := fantasy.NewAgentTool(
		"run_python",
		"Execute a short Python expression in the governed sandbox.",
		func(_ context.Context, _ struct {
			Code string `json:"code" description:"Python snippet"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("stdout: ok\n"), nil
		},
	)

	renderArtifact := fantasy.NewAgentTool(
		"render_artifact",
		"Render a structured summary card in the chat transcript.",
		func(_ context.Context, in struct {
			Title   string `json:"title" description:"Card title"`
			Summary string `json:"summary" description:"Card summary"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("artifact rendered: " + in.Title), nil
		},
	)

	return []fantasy.AgentTool{toolSearch, queryMetrics, inspectBox, readRunbook, runPython, renderArtifact}
}

// liveCacheTurn2UserPrompt builds a ~1,300-token user message for Turn 2 so
// the user message itself is large enough (>1,024 tokens) to prove that
// openRouterCacheControlTransport's content-block normalization caused the
// Turn 2 user message to be written to cache on Turn 2 Step 0 and read back on
// Turn 2 Step 1.
func liveCacheTurn2UserPrompt() string {
	var b strings.Builder
	b.WriteString("Here is the regional change-management audit log collected by the on-call engineer:\n\n")
	for i := 1; i <= 36; i++ {
		fmt.Fprintf(&b, "- audit_event_%02d: timestamp=2026-11-03T19:%02d:00Z actor=deploy-bot-%02d change_id=CHG-2026-%04dchecksum=sha256:%08x status=applied\n",
			i, i%60, i%6, 3000+i, i*9876543)
	}
	b.WriteString("\nNow call `read_runbook` with path `runbooks/region-7-failover.md`, and then reply in one short sentence stating the VERIFICATION_TOKEN and ESCALATION_SLA from that runbook.")
	return b.String()
}

type liveCacheRecord struct {
	Turn          int
	Provider      string
	PromptTokens  int
	CachedTokens  int
	CacheWrite    int
	Cost          float64
	Breakpoints   int
	ProviderField json.RawMessage
	Extra         map[string]json.RawMessage
}

// liveCacheTransport forwards to OpenRouter, keeps each request body, and
// pulls the final usage object (and the serving provider) out of the SSE
// stream AS THE CLIENT READS IT: the response body is teed, never buffered
// first, so the first event reaches fantasy as soon as it arrives and the
// first-chunk watchdog behaves exactly as in production.
type liveCacheTransport struct {
	mu      sync.Mutex
	turn    int
	records []liveCacheRecord
}

func (t *liveCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return resp, err
	}
	t.mu.Lock()
	turn := t.turn
	t.mu.Unlock()
	if dir := os.Getenv("FLEET_CACHE_LIVE_DUMP"); dir != "" {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(reqBody, &m)
		_ = os.WriteFile(fmt.Sprintf("%s/%s-%d.json", dir, strings.ReplaceAll(m.Model, "/", "_"), time.Now().UnixNano()), reqBody, 0o600)
	}
	resp.Body = &liveCacheBody{rc: resp.Body, finish: func(raw []byte) { t.record(turn, reqBody, raw) }}
	return resp, nil
}

// liveCacheBody tees the stream into a buffer and hands it to finish once,
// when the client hits EOF or closes the body (whichever comes first).
type liveCacheBody struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	once   sync.Once
	finish func([]byte)
}

func (b *liveCacheBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf.Write(p[:n])
	if err == io.EOF {
		b.once.Do(func() { b.finish(b.buf.Bytes()) })
	}
	return n, err
}

func (b *liveCacheBody) Close() error {
	b.once.Do(func() { b.finish(b.buf.Bytes()) })
	return b.rc.Close()
}

func (t *liveCacheTransport) record(turn int, reqBody, raw []byte) {
	rec := liveCacheRecord{Turn: turn, Extra: map[string]json.RawMessage{}}
	rec.Breakpoints = bytes.Count(reqBody, []byte(`"cache_control"`))
	var reqJSON struct {
		Provider json.RawMessage `json:"provider"`
	}
	_ = json.Unmarshal(reqBody, &reqJSON)
	rec.ProviderField = reqJSON.Provider
	for k, v := range extraTopLevelFields(reqBody) {
		rec.Extra[k] = v
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var chunk struct {
			Provider string `json:"provider"`
			Usage    *struct {
				PromptTokens        int     `json:"prompt_tokens"`
				Cost                float64 `json:"cost"`
				PromptTokensDetails struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk.Provider != "" {
			rec.Provider = chunk.Provider
		}
		if chunk.Usage != nil {
			rec.PromptTokens = chunk.Usage.PromptTokens
			rec.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			rec.CacheWrite = chunk.Usage.PromptTokensDetails.CacheWriteTokens
			rec.Cost = chunk.Usage.Cost
		}
	}
	t.mu.Lock()
	t.records = append(t.records, rec)
	t.mu.Unlock()
}

// extraTopLevelFields returns the request's top-level keys other than the
// ones every chat request carries, so the report shows routing/caching hints
// (provider, prompt_cache_key, user, …) fleet actually sent.
func extraTopLevelFields(body []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	for _, k := range []string{"model", "messages", "tools", "stream", "stream_options", "max_tokens", "temperature", "tool_choice", "provider", "usage"} {
		delete(m, k)
	}
	return m
}

func runPromptCacheLive(t *testing.T, key, slug string) {
	transport := &liveCacheTransport{}
	provider, err := openrouter.New(
		openrouter.WithAPIKey(key),
		openrouter.WithHTTPClient(&http.Client{
			Transport: &openRouterCacheControlTransport{next: transport},
			Timeout:   10 * time.Minute,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	model, err := provider.LanguageModel(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	system := liveCacheSystemPrompt(fmt.Sprintf("%s-%d", slug, time.Now().UnixNano()))
	tools := buildLiveCacheToolRoster()

	userTurns := []string{
		"Investigate the Region 7 latency alert step by step: first call `tool_search` with query `region 7 latency`; second, call `query_metrics` with the `dataset_id` returned by `tool_search`; third, call `inspect_box` with the degraded `hostname` returned by `query_metrics`; finally answer in one short sentence with the INCIDENT_CODE and ROOT_CAUSE.",
		liveCacheTurn2UserPrompt(),
		"Without calling any more tools, summarize in one short sentence the INCIDENT_CODE from Turn 1, the VERIFICATION_TOKEN from Turn 2, and the ticket prefix for policy 12.",
	}
	gap := 5 * time.Second
	if v := os.Getenv("FLEET_CACHE_LIVE_TURN_GAP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			gap = d
		}
	}
	// Per turn: the user message, its runtime_today note, and the replayed
	// assistant/tool messages.
	history := make([]fantasy.Message, 0, 8*len(userTurns))
	var fleetCached, fleetWrite int
	for i, u := range userTurns {
		if i > 0 {
			time.Sleep(gap)
		}
		transport.mu.Lock()
		transport.turn = i + 1
		transport.mu.Unlock()
		note, _ := RuntimeDateMessage(RuntimeToday(time.Now()))
		history = append(history, fantasy.NewUserMessage(u), note)
		session := NewLogSession()
		res, err := Run(ctx, ModeInteractive, RunConfig{
			NativeTools:        tools,
			RuntimeDateInInput: true,
		}, Deps{
			Input:      historyInput{system: system, msgs: append([]fantasy.Message(nil), history...), label: "cache-live"},
			Policy:     newRoundsPolicy(session, 0),
			Executor:   &stubExecutor{},
			Model:      model,
			LogSession: session,
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		fleetCached += session.CachedTokens
		fleetWrite += session.CacheCreationTokens
		history = append(history, liveReplayEntries(res.Entries)...)
	}

	var wireCached, wireWrite, wirePrompt, laterPrompt, laterCached, reads, laterReqs int
	var cost float64
	for i, r := range transport.records {
		hitPct := 0.0
		if r.PromptTokens > 0 {
			hitPct = float64(r.CachedTokens) / float64(r.PromptTokens) * 100.0
		}
		t.Logf("%s req %d (turn %d) provider=%q prompt=%d cached=%d (%.1f%%) write=%d bp=%d cost=$%.5f pin=%s extra=%v",
			slug, i+1, r.Turn, r.Provider, r.PromptTokens, r.CachedTokens, hitPct, r.CacheWrite, r.Breakpoints, r.Cost, r.ProviderField, keys(r.Extra))
		wirePrompt += r.PromptTokens
		wireCached += r.CachedTokens
		wireWrite += r.CacheWrite
		cost += r.Cost
		if i > 0 {
			laterReqs++
			laterPrompt += r.PromptTokens
			laterCached += r.CachedTokens
			if r.CachedTokens > 0 {
				reads++
			}
		}
	}
	warmHitPct := 0.0
	if laterPrompt > 0 {
		warmHitPct = float64(laterCached) / float64(laterPrompt) * 100.0
	}
	t.Logf("%s SUMMARY requests=%d later-requests-with-cache-read=%d/%d warm-hit-rate=%.1f%% (%d/%d) wire cached=%d write=%d | fleet-recorded cached=%d write=%d | cost=$%.4f",
		slug, len(transport.records), reads, laterReqs, warmHitPct, laterCached, laterPrompt, wireCached, wireWrite, fleetCached, fleetWrite, cost)
	if fleetCached != wireCached {
		t.Errorf("%s: fleet recorded %d cached tokens, the wire reported %d", slug, fleetCached, wireCached)
	}
	if fleetWrite != wireWrite {
		t.Logf("%s: fleet recorded %d cache-write tokens, the wire reported %d (fantasy does not parse cache_write_tokens)", slug, fleetWrite, wireWrite)
	}
	if len(transport.records) < 5 {
		t.Fatalf("%s: expected at least 5 LLM requests across the 3-turn multi-tool trajectory, got %d", slug, len(transport.records))
	}

	explicit := supportsExplicitBreakpoints(slug)
	// A model whose upstream has no explicit cache treats fleet's breakpoint as
	// a no-op: every read is the provider's best-effort implicit cache (a
	// request routed to a cold replica reads nothing), so it is held to the
	// implicit rule — one miss allowed — not the explicit one. The assumption
	// is checked, not trusted: such a model must report no cache writes, and
	// if it ever does, it has gained explicit caching and this list is stale.
	if liveImplicitOnly[slug] {
		if wireWrite != 0 {
			t.Errorf("%s: listed as implicit-cache only, but the wire reported %d cache-write tokens — it now has explicit caching; remove it from liveImplicitOnly", slug, wireWrite)
		}
		explicit = false
	}
	implicitMisses := 0
	for i, r := range transport.records {
		if i == 0 || r.CachedTokens > 0 {
			continue
		}
		if explicit && (r.Breakpoints > 0 || !stablePrefixOnly(slug)) {
			t.Errorf("%s: request %d (turn %d) carried %d breakpoint(s) for an explicit-cache family and read 0 tokens from cache", slug, i+1, r.Turn, r.Breakpoints)
			continue
		}
		implicitMisses++
	}
	if implicitMisses > 1 {
		t.Errorf("%s: %d requests left to the implicit cache read nothing from it (at most 1 warm-up miss allowed)", slug, implicitMisses)
	}

	// For explicit rolling-window families (Anthropic), verify both intra-turn
	// tool-step cache growth AND cross-turn history cache preservation.
	if explicit && !stablePrefixOnly(slug) {
		assertLiveExplicitRollingCacheGrowth(t, slug, transport.records, warmHitPct)
	}
}

// liveImplicitOnly lists models that take fleet's stable-prefix breakpoint but
// have no explicit cache upstream (cache.go: Gemini 4 Hydrogen / Argon use
// Google Vertex implicit prefix caching); see their use in TestPromptCacheLive.
var liveImplicitOnly = map[string]bool{
	"google/gemini-4-hydrogen": true,
	"google/gemini-4-argon":    true,
}

func assertLiveExplicitRollingCacheGrowth(t *testing.T, slug string, records []liveCacheRecord, warmHitPct float64) {
	t.Helper()
	req2Cached := records[1].CachedTokens
	if req2Cached < 6500 {
		t.Errorf("%s: request 2 cached_tokens=%d, want >= 6500 (system prompt + 6-tool roster must hit cache on first tool step)", slug, req2Cached)
	}
	lastCached := records[len(records)-1].CachedTokens
	if lastCached < req2Cached+3000 {
		t.Errorf("%s: final request cached_tokens=%d did not grow by >= 3000 tokens over request 2 cached_tokens=%d (multi-step tool outputs and user turns were not cached across turns)",
			slug, lastCached, req2Cached)
	}
	// Find the first request of Turn 2 and Turn 3 and verify cross-turn growth.
	var turn2FirstIdx, turn2SecondIdx, turn3FirstIdx = -1, -1, -1
	for idx, r := range records {
		if r.Turn == 2 {
			if turn2FirstIdx == -1 {
				turn2FirstIdx = idx
			} else if turn2SecondIdx == -1 {
				turn2SecondIdx = idx
			}
		}
		if r.Turn == 3 && turn3FirstIdx == -1 {
			turn3FirstIdx = idx
		}
	}
	if turn2FirstIdx > 1 {
		t2FirstCached := records[turn2FirstIdx].CachedTokens
		if t2FirstCached <= req2Cached+1000 {
			t.Errorf("%s: Turn 2 first request (req %d) cached_tokens=%d did not include Turn 1's replayed tool history (req 2 cached_tokens=%d)",
				slug, turn2FirstIdx+1, t2FirstCached, req2Cached)
		}
	}
	if turn2FirstIdx != -1 && turn2SecondIdx != -1 {
		t2FirstCached := records[turn2FirstIdx].CachedTokens
		t2SecondCached := records[turn2SecondIdx].CachedTokens
		if t2SecondCached <= t2FirstCached+800 {
			t.Errorf("%s: Turn 2 second request (req %d) cached_tokens=%d did not read Turn 2 Step 0's user message from cache (Turn 2 Step 0 cached_tokens=%d) — check openRouterCacheControlTransport user content-block normalization!",
				slug, turn2SecondIdx+1, t2SecondCached, t2FirstCached)
		}
	}
	if turn3FirstIdx != -1 && turn2FirstIdx != -1 {
		t3Cached := records[turn3FirstIdx].CachedTokens
		t2FirstCached := records[turn2FirstIdx].CachedTokens
		if t3Cached <= t2FirstCached+1000 {
			t.Errorf("%s: Turn 3 request (req %d) cached_tokens=%d did not grow past Turn 2 start cached_tokens=%d",
				slug, turn3FirstIdx+1, t3Cached, t2FirstCached)
		}
	}
	if warmHitPct < 75.0 {
		t.Errorf("%s: warm cache hit rate across post-cold requests = %.1f%%, want >= 75.0%%", slug, warmHitPct)
	}
}

// TestPromptCacheLive_LongToolChainCrossTurn exercises a 10-step sequential
// tool chain in Turn 1 (>20 content blocks across assistant tool_calls and
// tool results, exceeding Anthropic's 20-block lookback window) followed by
// Turn 2 after liveReplayEntries strips thinking blocks.
func TestPromptCacheLive_LongToolChainCrossTurn(t *testing.T) {
	if os.Getenv("FLEET_CACHE_LIVE") != "1" {
		t.Skip("set FLEET_CACHE_LIVE=1 (and OPENROUTER_API_KEY) to run the live long-tool-chain prompt-cache check")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Fatal("FLEET_CACHE_LIVE=1 needs OPENROUTER_API_KEY")
	}
	slug := "anthropic/claude-haiku-5.5"
	if v := strings.TrimSpace(os.Getenv("FLEET_CACHE_LIVE_LONG_MODEL")); v != "" {
		slug = v
	}

	transport := &liveCacheTransport{}
	provider, err := openrouter.New(
		openrouter.WithAPIKey(key),
		openrouter.WithHTTPClient(&http.Client{
			Transport: &openRouterCacheControlTransport{next: transport},
			Timeout:   10 * time.Minute,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	model, err := provider.LanguageModel(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	system := liveCacheSystemPrompt(fmt.Sprintf("long-chain-%s-%d", slug, time.Now().UnixNano()))

	const totalPages = 10
	fetchPage := fantasy.NewAgentTool(
		"fetch_audit_page",
		"Fetch one numbered page (1..10) of the regional security audit log. Must be called sequentially one page at a time.",
		func(_ context.Context, in struct {
			Page int `json:"page" description:"Page number to fetch (1..10)"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "audit_page=%d of %d:\n", in.Page, totalPages)
			if in.Page < totalPages {
				fmt.Fprintf(&b, "NEXT_ACTION: immediately call fetch_audit_page with page=%d (do not skip pages or call multiple in parallel).\n", in.Page+1)
			} else {
				b.WriteString("FINAL_PAGE_REACHED: token=AUDIT-CHAIN-10-COMPLETE. Now reply with a one-sentence summary containing AUDIT-CHAIN-10-COMPLETE.\n")
			}
			for i := 1; i <= 15; i++ {
				fmt.Fprintf(&b, "page_%02d_entry_%02d | ts=2026-11-03T20:%02d:%02dZ | principal=svc-node-%02d | digest=sha256:%08x\n",
					in.Page, i, in.Page%60, i%60, i%9, (in.Page*100+i)*2654435)
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)

	history := make([]fantasy.Message, 0, 32)
	transport.turn = 1
	note1, _ := RuntimeDateMessage(RuntimeToday(time.Now()))
	history = append(history,
		fantasy.NewUserMessage("Call `fetch_audit_page` starting at page=1 and step through each page sequentially one at a time until page=10 returns FINAL_PAGE_REACHED, then state the completion token in one short sentence."),
		note1,
	)

	session1 := NewLogSession()
	res1, err := Run(ctx, ModeInteractive, RunConfig{
		NativeTools:        []fantasy.AgentTool{fetchPage},
		RuntimeDateInInput: true,
	}, Deps{
		Input:      historyInput{system: system, msgs: append([]fantasy.Message(nil), history...), label: "cache-live-long-t1"},
		Policy:     newRoundsPolicy(session1, 0),
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session1,
	})
	if err != nil {
		t.Fatalf("Turn 1 long tool chain failed: %v", err)
	}
	history = append(history, liveReplayEntries(res1.Entries)...)

	time.Sleep(2 * time.Second)
	transport.mu.Lock()
	transport.turn = 2
	transport.mu.Unlock()
	note2, _ := RuntimeDateMessage(RuntimeToday(time.Now()))
	history = append(history,
		fantasy.NewUserMessage("Without calling any tools, confirm the completion token from page 10 and the ticket prefix for policy 5 in one short sentence."),
		note2,
	)
	session2 := NewLogSession()
	if _, err := Run(ctx, ModeInteractive, RunConfig{
		NativeTools:        []fantasy.AgentTool{fetchPage},
		RuntimeDateInInput: true,
	}, Deps{
		Input:      historyInput{system: system, msgs: append([]fantasy.Message(nil), history...), label: "cache-live-long-t2"},
		Policy:     newRoundsPolicy(session2, 0),
		Executor:   &stubExecutor{},
		Model:      model,
		LogSession: session2,
	}); err != nil {
		t.Fatalf("Turn 2 follow-up failed: %v", err)
	}

	var turn1Reqs, turn2Reqs []liveCacheRecord
	for i, r := range transport.records {
		hitPct := 0.0
		if r.PromptTokens > 0 {
			hitPct = float64(r.CachedTokens) / float64(r.PromptTokens) * 100.0
		}
		t.Logf("long-chain %s req %d (turn %d) provider=%q prompt=%d cached=%d (%.1f%%) write=%d bp=%d cost=$%.5f",
			slug, i+1, r.Turn, r.Provider, r.PromptTokens, r.CachedTokens, hitPct, r.CacheWrite, r.Breakpoints, r.Cost)
		if r.Turn == 1 {
			turn1Reqs = append(turn1Reqs, r)
		} else {
			turn2Reqs = append(turn2Reqs, r)
		}
	}
	if len(turn1Reqs) < 8 {
		t.Fatalf("expected at least 8 requests in Turn 1 pagination chain, got %d", len(turn1Reqs))
	}
	if len(turn2Reqs) == 0 {
		t.Fatal("expected at least 1 request in Turn 2")
	}
	// Every Turn 1 step after Step 0 must read from cache, and once the tool
	// chain has >=2 messages after runtime_today, all 4 breakpoints must be active.
	for i := 1; i < len(turn1Reqs); i++ {
		if turn1Reqs[i].CachedTokens < 6500 {
			t.Errorf("Turn 1 step %d cached_tokens=%d, want >= 6500", i, turn1Reqs[i].CachedTokens)
		}
		if i >= 2 && turn1Reqs[i].Breakpoints != 4 {
			t.Errorf("Turn 1 step %d breakpoints=%d, want 4 (system + runtime_today + 2 rolling tail messages)", i, turn1Reqs[i].Breakpoints)
		}
	}
	// Turn 1 final step must cache significantly more than Turn 1 step 1 as the
	// 10-step tool chain accumulates in cache.
	if turn1Reqs[len(turn1Reqs)-1].CachedTokens <= turn1Reqs[1].CachedTokens+1500 {
		t.Errorf("Turn 1 final step cached_tokens=%d did not grow past step 1 cached_tokens=%d",
			turn1Reqs[len(turn1Reqs)-1].CachedTokens, turn1Reqs[1].CachedTokens)
	}
	// Turn 2 Step 0 (after >20 content blocks in Turn 1 and stripped thinking blocks)
	// must STILL hit the cache!
	if turn2Reqs[0].CachedTokens < 6500 {
		t.Errorf("Turn 2 Step 0 after %d-step Turn 1 cached_tokens=%d, want >= 6500 (runtime_today 4th breakpoint + rolling tail must preserve cache across >20 blocks)",
			len(turn1Reqs), turn2Reqs[0].CachedTokens)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// liveReplayEntries turns a run's entries into the messages the next turn
// replays, in the shape agent.replayHistory produces (that package imports
// this one, so it cannot be called here): assistant text and tool calls
// coalesce into one assistant message, each followed by its tool results;
// reasoning is never replayed.
func liveReplayEntries(entries []RunEntry) []fantasy.Message {
	var out []fantasy.Message
	var pending []fantasy.MessagePart
	flush := func() {
		if len(pending) > 0 {
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: pending})
			pending = nil
		}
	}
	for _, e := range entries {
		switch e.Type {
		case "text":
			if e.Role == "assistant" {
				pending = append(pending, fantasy.TextPart{Text: e.Text})
			}
		case "tool_call":
			pending = append(pending, fantasy.ToolCallPart{ToolCallID: e.ToolCallID, ToolName: e.ToolName, Input: e.ToolInput})
		case "tool_result":
			flush()
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{ToolCallID: e.ToolCallID, Output: fantasy.ToolResultOutputContentText{Text: e.Text}},
			}})
		}
	}
	flush()
	return out
}
