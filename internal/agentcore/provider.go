package agentcore

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// Provider setup + upstream pinning (merged from chat + cutlass fantasy.go).
//
// We route everything through OpenRouter. The provider headers (X-Title,
// HTTP-Referer) identify the fleet runtime (DefaultProviderHeaders); they were
// the one front-end-specific divergence between the merged repos and are now a
// single fixed identity. upstreamPinFor takes chat's `~`-stripping behaviour (the
// superset: it handles OpenRouter floating-alias slugs by stripping the sigil
// before prefix-matching, which cutlass did not). isAliasModel and
// anthropicSupportsLongContext are identical/cutlass-superset and lifted as-is.

// ProviderHeaders are the OpenRouter request headers fleet advertises.
type ProviderHeaders struct {
	XTitle      string
	HTTPReferer string
}

// DefaultProviderHeaders identify the unified fleet runtime to OpenRouter. They
// are fixed: agentcore reads no environment variable to override them.
var DefaultProviderHeaders = ProviderHeaders{
	XTitle:      "fleet",
	HTTPReferer: "https://github.com/ElcanoTek/fleet",
}

// openRouterBaseURLOverrideEnv is the single env knob that redirects every
// OpenRouter HTTP call to a different origin without touching the wire format.
// It exists for deterministic E2E testing: a wire-compatible fake LLM (see
// cmd/fake-llm / internal/fakellm) can be pointed at by exporting this var, so
// the whole real tool loop — provider, SSE parsing, tool_calls, the scheduler,
// the sandbox — runs unchanged while only the LLM origin is swapped. Empty
// (the production default) leaves the upstream OpenRouter URL hardcoded by the
// fantasy library untouched.
const openRouterBaseURLOverrideEnv = "OPENROUTER_BASE_URL"

// openRouterBaseURLOverride returns the configured base-URL override, or "".
// The bare OPENROUTER_BASE_URL is honored first (matching OPENROUTER_API_KEY,
// which is also un-prefixed because OpenRouter vars are vendor-named, not
// fleet-namespaced), then FLEET_OPENROUTER_BASE_URL so it still composes with the prefixed env config when desired.
func openRouterBaseURLOverride() string {
	if v := strings.TrimSpace(envreg.Get(openRouterBaseURLOverrideEnv)); v != "" {
		return v
	}
	return fleetEnv.lookup(openRouterBaseURLOverrideEnv)
}

// newOpenRouterProvider builds the fantasy OpenRouter provider with the given
// API key and DefaultProviderHeaders. Every request runs through openRouterCacheControlTransport
// so explicit per-message cache_control markers on user and assistant messages
// reach OpenRouter inside content-part arrays (where OpenRouter's Anthropic and
// Gemini translators read them) rather than top-level message fields on string
// content (which OpenRouter silently ignores). When OPENROUTER_BASE_URL is set
// (E2E only), it also installs a URL-rewriting transport so the real openai-go
// SDK still builds genuine OpenRouter chat-completions requests but dispatches
// them to the override origin — the provider boundary is the one and only thing
// stubbed.
func newOpenRouterProvider(apiKey string) (fantasy.Provider, error) {
	next := http.DefaultTransport
	if override := openRouterBaseURLOverride(); override != "" {
		if rt, err := newBaseURLRewriteTransport(override); err == nil {
			next = rt
		}
	}
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
		openrouter.WithHeaders(map[string]string{
			"X-Title":      DefaultProviderHeaders.XTitle,
			"HTTP-Referer": DefaultProviderHeaders.HTTPReferer,
		}),
		openrouter.WithHTTPClient(&http.Client{
			Transport: &openRouterCacheControlTransport{next: next},
		}),
	}
	return openrouter.New(opts...)
}

// openRouterCacheControlTransport normalizes outbound OpenRouter
// /chat/completions request bodies for explicit-breakpoint model families
// (supportsExplicitBreakpoints: Anthropic and Google).
//
// Why this is needed: fantasy's OpenRouter hook (language_model_hooks.go)
// serializes single-text-part user and assistant messages as plain string
// `"content": "..."` with `"cache_control": {"type": "ephemeral"}` at the top
// level of the message object. OpenRouter's OpenAI-to-Anthropic/Gemini
// translator honors top-level `cache_control` on `system` and `tool` messages,
// but ignores top-level `cache_control` on `user` and `assistant` messages
// unless `"content"` is a content-part array whose text block carries the
// `"cache_control"` field:
//
//	{"role": "user", "content": [{"type": "text", "text": "...", "cache_control": {"type": "ephemeral"}}]}
//
// Moreover, OpenRouter merges consecutive user messages (`user` + `runtime_today`)
// differently when `"content"` is a plain string vs a content-part array, so
// converting only marked messages would change the token prefix once the
// rolling breakpoint advances to the next turn. Normalizing every non-empty
// string `"content"` on `user` and `assistant` messages to a one-element text
// block array — and moving any top-level `"cache_control"` onto the last
// content block when `tool_calls` is empty — makes every user/assistant
// breakpoint take effect on Step 0 and keeps the prefix byte-identical as the
// rolling window moves forward.
type openRouterCacheControlTransport struct {
	next http.RoundTripper
}

func (t *openRouterCacheControlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") || req.Body == nil {
		return next.RoundTrip(req)
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	rewritten := normalizeOpenRouterCacheControl(raw)
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(rewritten))
	clone.ContentLength = int64(len(rewritten))
	clone.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rewritten)), nil
	}
	return next.RoundTrip(clone)
}

func normalizeOpenRouterCacheControl(raw []byte) []byte {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || !supportsExplicitBreakpoints(probe.Model) {
		return raw
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return raw
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return raw
	}
	changed := false
	for _, m := range msgs {
		var role string
		if err := json.Unmarshal(m["role"], &role); err != nil {
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		rawContent, hasContent := m["content"]
		if !hasContent {
			continue
		}
		hasToolCalls := hasNonEmptyJSONArray(m["tool_calls"])
		var text string
		if err := json.Unmarshal(rawContent, &text); err == nil {
			// Leave empty string content untouched: tool-call-only assistant
			// messages carry content:"" with cache_control on tool_calls[last],
			// and Anthropic rejects empty {"type":"text","text":""} blocks.
			if text == "" {
				continue
			}
			block := map[string]any{
				"type": "text",
				"text": text,
			}
			if cc, hasCC := m["cache_control"]; hasCC && !hasToolCalls {
				block["cache_control"] = cc
				delete(m, "cache_control")
			}
			encoded, err := json.Marshal([]any{block})
			if err != nil {
				continue
			}
			m["content"] = encoded
			changed = true
			continue
		}
		// Multi-part content array (e.g. user message with image/file parts):
		// fantasy attaches cache_control at the top level of the message
		// object; move it onto the last content part so OpenRouter forwards it.
		if cc, hasCC := m["cache_control"]; hasCC && !hasToolCalls {
			var parts []map[string]json.RawMessage
			if err := json.Unmarshal(rawContent, &parts); err == nil && len(parts) > 0 {
				parts[len(parts)-1]["cache_control"] = cc
				delete(m, "cache_control")
				if encoded, err := json.Marshal(parts); err == nil {
					m["content"] = encoded
					changed = true
				}
			}
		}
	}
	if !changed {
		return raw
	}
	encodedMsgs, err := json.Marshal(msgs)
	if err != nil {
		return raw
	}
	top["messages"] = encodedMsgs
	out, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return out
}

func hasNonEmptyJSONArray(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var arr []json.RawMessage
	return json.Unmarshal(raw, &arr) == nil && len(arr) > 0
}

// baseURLRewriteTransport swaps the scheme+host of every outbound request to a
// fixed target origin, preserving the path, query, headers and body. This is
// how the OPENROUTER_BASE_URL override redirects the upstream-hardcoded
// https://openrouter.ai/api/v1/... calls to a fake without the fantasy library
// exposing a base-URL option.
type baseURLRewriteTransport struct {
	scheme string
	host   string
	next   http.RoundTripper
}

func newBaseURLRewriteTransport(target string) (*baseURLRewriteTransport, error) {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return nil, err
	}
	return &baseURLRewriteTransport{
		scheme: u.Scheme,
		host:   u.Host,
		next:   http.DefaultTransport,
	}, nil
}

func (t *baseURLRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.scheme
	clone.URL.Host = t.host
	clone.Host = t.host
	return t.next.RoundTrip(clone)
}

// Canonical OpenRouter provider routing names for the upstreams we pin. Each
// matches the spelling OpenRouter's API expects in provider.order / provider.only.
const (
	upstreamProviderGoogle         = "Google"
	upstreamProviderGoogleAIStudio = "Google AI Studio"
	upstreamProviderAnthropic      = "Anthropic"
	upstreamProviderOpenAI         = "OpenAI"
	upstreamProviderMoonshot       = "Moonshot AI"
	upstreamProviderZAI            = "Z.AI"
	upstreamProviderDeepSeek       = "DeepSeek"
	upstreamProviderDeepInfra      = "DeepInfra"

	// The cloud resellers that serve a vendor's official weights. They are
	// never a canonicalUpstream preference of their own — they appear only
	// inside an officialPoolSlugs allow-list, as the named fallbacks a soft
	// pin is allowed to degrade onto.
	upstreamProviderAzure         = "Azure"
	upstreamProviderAmazonBedrock = "Amazon Bedrock"
	upstreamProviderClaudeOnAWS   = "Claude Platform on AWS"
)

// fp8AndAbove is the quantization allow-list for families whose OpenRouter
// endpoint pool mixes serving precisions. It names every level at or above fp8
// (OpenRouter's spelling), so routing may prefer a HIGHER-precision endpoint but
// can never silently drop to a lower one. "unknown" is deliberately absent:
// an endpoint that does not declare its precision cannot be shown to clear the
// floor, and the whole point of the floor is that it holds on the fallback path
// where nobody is watching.
var fp8AndAbove = []string{"fp8", "fp16", "bf16", "fp32"}

// unpinnedOpenWeightsPrefixes are slugs under a pinned family prefix that the
// family pin must NOT govern: open-weights models the vendor publishes but
// many hosts serve. The google/ pin is strict (Only=[Google], no fallbacks)
// because Gemini's encrypted thought signatures validate only at the minting
// upstream; Gemma has no such signatures, and pinning it to Google's single
// endpoint turned that endpoint's rate limiting into user-facing failures
// (4 of 6 live calls answered 429 on 2026-10-15, while 12 other hosts were up).
// These slugs route like an unpinned family, with a precision floor where one
// is recorded in precisionFloorSlugs.
var unpinnedOpenWeightsPrefixes = []string{"google/gemma"}

// familyPinsFor returns the canonicalUpstream rows that may govern a slug:
// all of them, or none for an unpinned open-weights slug.
func familyPinsFor(matchSlug string) []familyPin {
	for _, p := range unpinnedOpenWeightsPrefixes {
		if strings.HasPrefix(matchSlug, p) {
			return nil
		}
	}
	return canonicalUpstream
}

// canonicalUpstream pins each model family to a single OpenRouter upstream so
// prompt caches (which are per-upstream) survive across calls. strict=true
// (Only + AllowFallbacks=false) is required for Google (encrypted thought
// signatures validate only at the minting upstream); strict=false (Order +
// AllowFallbacks=true) gives cache locality with graceful degradation.
//
// quantizations, when set, is a serving-precision FLOOR applied on top of the
// upstream preference. It is the guard the non-strict pins were missing: Order
// expresses a preference, not a constraint, so with AllowFallbacks=true every
// soft-pinned request is one busy first-party endpoint away from being served
// by whatever else in the pool answers — including a lower-precision quant of
// the same slug. The pin fixed cache locality and left quality unguarded.
type familyPin struct {
	prefix        string
	name          string
	strict        bool
	quantizations []string
}

var canonicalUpstream = []familyPin{
	// The default frontier tier (DefaultCoreModel, Gemini 4 Argon) lives here.
	// OpenRouter serves Gemini from two first-party Google upstreams — "Google"
	// (Vertex AI) and "Google AI Studio". A soft pin prefers "Google" (Vertex AI)
	// for implicit prompt-cache locality while allowing seamless failover onto
	// "Google AI Studio" when a Vertex region blips (closed to the two official
	// Google providers in officialPoolSlugs below; OpenRouter's adapter handles
	// cross-upstream Gemini thought-signature bypass when upstream-specific
	// signatures are stripped in geminiSignatureStep).
	{"google/", upstreamProviderGoogle, false, nil},
	// The fallback frontier tier (DefaultFallbackModel, Claude Opus 5.5; also
	// DefaultCoreModel until 2026-11-09 and Claude Haiku 5.5 before that): a
	// soft pin to Anthropic's own endpoint with graceful degradation onto the
	// cloud resellers of the same weights. The default-pin guard's exemption for
	// those slugs — and the allow-list that enforces it — is recorded per SLUG in
	// officialPoolSlugs, not here.
	{"anthropic/", upstreamProviderAnthropic, false, nil},
	// This family carried the everyday default until 2026-10-15 (GPT-6 Luna
	// Pro, GPT-5.6 Luna Pro before it). Soft pin: OpenAI first, Azure and Amazon Bedrock as
	// fallbacks. No floor: see officialPoolSlugs for the per-slug evidence and
	// the allow-list that holds the pool to it.
	{"openai/", upstreamProviderOpenAI, false, nil},
	{"moonshotai/", upstreamProviderMoonshot, false, nil},
	{"z-ai/", upstreamProviderZAI, false, nil},
	// DeepSeek family (including deepseek/deepseek-v4.1-flash-1017), non-strict
	// with the fp8AndAbove quantization floor. OpenRouter endpoints serve this
	// family at context lengths from 163K to 1M and quantizations from fp4 to
	// fp8. DeepSeek's own first-party endpoint reports quantization "unknown"
	// (and requires paid-model training opt-in), so fp8AndAbove excludes it;
	// preferring DeepInfra (which serves native fp8 with the full
	// 1,048,576-token window and sticky implicit prompt caching — 98.5% warm hit
	// rate verified live on 2026-11-09) keeps the prompt cache warm on one
	// upstream while allowing graceful fallback across the other fp8+ endpoints
	// and strictly blocking fp4/mxfp4.
	{"deepseek/", upstreamProviderDeepInfra, false, fp8AndAbove},
}

// officialPoolAllowlist is one slug's validated endpoint pool: the exact
// OpenRouter routing names found serving the vendor's official weights, and
// the date that pool was read.
type officialPoolAllowlist struct {
	checked   string
	providers []string
}

// officialPoolSlugs lists the exact slugs whose ENTIRE OpenRouter endpoint
// pool was checked and found to serve the vendor's official weights (the
// vendor plus its cloud resellers, quantization unspecified — "unknown" — on
// every endpoint), so their soft pin needs no serving-precision floor: an fp8
// floor would reject the whole pool, and there is no third-party quantized
// serving to degrade onto. This is the third way a default-tier slug may
// satisfy TestDefaultCoreModelCannotBeServedAtArbitraryPrecision — per slug,
// never per family, because a future model in the same family can be picked up
// by third-party hosts. Add a slug only with the endpoint list in hand.
//
// The list is an ALLOW-LIST, not a note: upstreamPinFor sends it as
// provider.only, so the claim is enforced on the request instead of snapshotted
// in a comment (#1589). Be exact about what that enforces, because provider.only
// matches a PROVIDER ROUTING NAME, not an endpoint: a third-party host appearing
// in one of these pools later is refused, which is the case the exemption needs
// closed, but an already-listed provider that adds or re-quantizes an endpoint
// still matches its name. Endpoint-level precision is what the quantization
// floor is for, and it is unusable here — every endpoint in both pools reports
// quantization "unknown", which fp8AndAbove excludes, so a floor would make both
// slugs unroutable. The residue is therefore an assumption, not a guarantee:
// these named vendors and their official resellers keep serving official
// weights, and nothing re-reads endpoint attributes to check it. See
// docs/UPSTREAM-ROUTING-FLOOR.md.
//
// Every name below must be OpenRouter's own routing name for the endpoint —
// a misspelling narrows the pool to nothing.
var officialPoolSlugs = map[string]officialPoolAllowlist{
	// 2026-11-09: Google ×3 (google-vertex/global, google-vertex/us,
	// google-vertex/global/priority), Google AI Studio ×3 (google-ai-studio,
	// google-ai-studio/flex, google-ai-studio/priority) — six first-party
	// Google endpoints, every quantization "unknown". DefaultCoreModel.
	"google/gemini-4-argon": {
		checked: "2026-11-09",
		providers: []string{
			upstreamProviderGoogle,
			upstreamProviderGoogleAIStudio,
		},
	},
	// 2026-10-15: Anthropic ×1, Claude Platform on AWS ×1, Amazon Bedrock ×3,
	// Azure ×2, Google ×3 — ten endpoints, every quantization "unknown".
	// DefaultCoreModel until 2026-10-28.
	"anthropic/claude-haiku-5.5": {
		checked: "2026-10-15",
		providers: []string{
			upstreamProviderAnthropic,
			upstreamProviderClaudeOnAWS,
			upstreamProviderAmazonBedrock,
			upstreamProviderAzure,
			upstreamProviderGoogle,
		},
	},
	// 2026-09-22: OpenAI ×3, Azure ×2 — every endpoint quantization
	// "unknown". Amazon Bedrock was in the pool on 2026-09-21 and is not
	// today; it stays on the allow-list because a reseller of the official
	// weights coming back must not need a code change, and a name with no
	// endpoint behind it simply never matches.
	// 2026-09-22: OpenAI ×3 (openai, openai/flex, openai/fast) — every endpoint
	// quantization "unknown". DefaultCoreModel until 2026-10-15. Azure and Amazon Bedrock resell
	// the official weights for earlier OpenAI models and stay listed so a
	// reseller endpoint appearing later needs no code change.
	"openai/gpt-6-luna-pro": {
		checked:   "2026-09-22",
		providers: []string{upstreamProviderOpenAI, upstreamProviderAzure, upstreamProviderAmazonBedrock},
	},
	// 2026-09-22: Anthropic ×2, Claude Platform on AWS ×1, Amazon Bedrock ×3,
	// Azure ×2, Google ×3 — eleven endpoints, every quantization "unknown".
	// DefaultMaxModel until 2026-10-14.
	"anthropic/claude-opus-5.5": {
		checked: "2026-09-22",
		providers: []string{
			upstreamProviderAnthropic,
			upstreamProviderClaudeOnAWS,
			upstreamProviderAmazonBedrock,
			upstreamProviderAzure,
			upstreamProviderGoogle,
		},
	},
	"openai/gpt-5.6-luna-pro": {
		checked:   "2026-09-22",
		providers: []string{upstreamProviderOpenAI, upstreamProviderAzure, upstreamProviderAmazonBedrock},
	},
	// 2026-09-22: Anthropic ×2, Claude Platform on AWS ×1, Amazon Bedrock ×3,
	// Azure ×2, Google ×3 — eleven endpoints, every quantization "unknown".
	"anthropic/claude-opus-5": {
		checked: "2026-09-22",
		providers: []string{
			upstreamProviderAnthropic,
			upstreamProviderClaudeOnAWS,
			upstreamProviderAmazonBedrock,
			upstreamProviderAzure,
			upstreamProviderGoogle,
		},
	},
}

// precisionFloorSlugs lists exact slugs that get the fp8AndAbove serving-
// precision floor even though their family has no floor, or no canonicalUpstream
// entry at all. It exists for open-weights models whose OpenRouter pool mixes
// precisions inside a family that, as a whole, cannot take a floor.
//
// The qwen/ family is the case that needs it. Alibaba alone serves most qwen/
// slugs, and every one of those endpoints reports quantization "unknown", which
// fp8AndAbove excludes, so a family-wide floor would make them unroutable.
// Qwen's open-weights slugs are the opposite: third-party hosts serve them at
// fp4 through bf16. A floor is only safe per slug, read with the endpoint list
// in hand, and the same per-slug rule as officialPoolSlugs applies: a sibling
// does not inherit the floor.
//
// A listed slug with no canonicalUpstream entry gets the floor alone: no Order,
// so OpenRouter still balances across every endpoint that clears it, and
// preferredUpstreamFor stays "" (there is no preferred upstream for the
// served-upstream attribution to compare against).
var precisionFloorSlugs = map[string]string{
	// 2026-10-15: 13 endpoints. bf16 ×7 (DekaLLM, NextBit, CoreWeave, Io Net,
	// Venice, Parasail, Novita), fp8 ×2 (DeepInfra, SiliconFlow), unknown ×4
	// (Darkbloom, Makora, Cloudflare, Google Vertex). The floor leaves the nine
	// fp8-and-above endpoints; it also drops Google's endpoint, which answered
	// 429 "temporarily rate-limited upstream" to 4 of 6 live calls while the
	// pool answered 6 of 6. Routed without the google/ family pin: see
	// unpinnedOpenWeightsPrefixes.
	"google/gemma-4-26b-a4b-it": "2026-10-15",
	// 2026-10-15: 20 endpoints. fp4 ×3 (Tenstorrent, Darkbloom, ModelRun),
	// fp8 ×9, bf16 ×1 (DeepInfra), fp16 ×1 (Cerebras), unknown ×6 (Wafer,
	// DekaLLM, Phala, Novita, Alibaba, Cloudflare). The floor leaves the 11
	// fp8-and-above endpoints. The fp4 endpoints are among the cheapest in the
	// pool, so price-weighted routing reaches them often without a floor.
	"qwen/qwen3.8-27b": "2026-10-15",
}

// hasPrecisionFloor reports whether the exact slug (alias marker stripped) is
// listed in precisionFloorSlugs.
func hasPrecisionFloor(modelSlug string) bool {
	_, ok := precisionFloorSlugs[strings.TrimPrefix(modelSlug, "~")]
	return ok
}

// upstreamPinFor returns the OpenRouter provider routing policy for a model
// slug, or nil if the family has no canonical upstream. A leading `~` (floating
// alias) is stripped before prefix-matching so the alias inherits the lab's pin
// policy (chat behaviour; superset of cutlass which did not strip).
func upstreamPinFor(modelSlug string) *openrouter.Provider {
	matchSlug := strings.TrimPrefix(modelSlug, "~")
	for _, c := range familyPinsFor(matchSlug) {
		if !strings.HasPrefix(matchSlug, c.prefix) {
			continue
		}
		fallback := !c.strict
		p := &openrouter.Provider{AllowFallbacks: &fallback}
		if c.strict {
			p.Only = []string{c.name}
		} else {
			p.Order = []string{c.name}
			// A slug whose whole pool was validated carries that pool as the
			// request's allow-list: Order still prefers the vendor (prompt-cache
			// locality), Only closes the set the fallback may reach, so the
			// exemption from the quantization floor is a constraint on routing
			// rather than an assertion about a pool that can grow (#1589).
			if pool, ok := officialPoolSlugs[matchSlug]; ok {
				p.Only = append([]string(nil), pool.providers...)
			}
		}
		// Copy: the returned Provider is handed to the request builder, and a
		// shared backing array would let one call's mutation reach every later
		// request for the family.
		if len(c.quantizations) > 0 {
			p.Quantizations = append([]string(nil), c.quantizations...)
		} else if hasPrecisionFloor(matchSlug) {
			p.Quantizations = append([]string(nil), fp8AndAbove...)
		}
		return p
	}
	// No family pin: a slug with a recorded mixed-precision pool still gets the
	// floor, with OpenRouter's normal balancing across the endpoints that clear it.
	if hasPrecisionFloor(matchSlug) {
		fallback := true
		return &openrouter.Provider{
			AllowFallbacks: &fallback,
			Quantizations:  append([]string(nil), fp8AndAbove...),
		}
	}
	return nil
}

// RoutingProviderOptions returns the OpenRouter routing policy for modelSlug
// (upstreamPinFor: the upstream pin, the official-pool allow-list, the serving-
// precision floor) as call options, for one-off model calls made outside the
// run loop on the run's own model: the compaction summarizer and manual
// summarize. Without it those calls go out with no provider block, so a slug
// with a floor could be served below it on exactly the calls that condense a
// long run's history. It carries routing only; the run loop's thinking and
// long-context extras stay with engine.providerOptions. Nil when the slug has
// no policy. Keyed under openrouter.Name like the run loop's options, so a
// natively routed model ignores it.
func RoutingProviderOptions(modelSlug string) fantasy.ProviderOptions {
	p := upstreamPinFor(modelSlug)
	if p == nil {
		return nil
	}
	return fantasy.ProviderOptions{openrouter.Name: &openrouter.ProviderOptions{Provider: p}}
}

// anthropicLongContextSlug reports whether a raw slug string is a long-context
// Claude variant (string form, for slug-only callers).
func anthropicLongContextSlug(slug string) bool {
	s := strings.ToLower(strings.TrimSpace(slug))
	return strings.Contains(s, "claude-sonnet-4.5") ||
		strings.Contains(s, "claude-sonnet-4.6") ||
		strings.Contains(s, "claude-opus-4.6") ||
		strings.Contains(s, "claude-opus-4.7") ||
		strings.Contains(s, "claude-opus-4.8")
}
