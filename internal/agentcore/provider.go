package agentcore

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// Provider setup + upstream pinning (merged from chat + cutlass fantasy.go).
//
// We route everything through OpenRouter. The provider headers (X-Title,
// HTTP-Referer) are the one front-end-specific divergence and are parameterized
// via ProviderHeaders. upstreamPinFor takes chat's `~`-stripping behaviour (the
// superset: it handles OpenRouter floating-alias slugs by stripping the sigil
// before prefix-matching, which cutlass did not). isAliasModel and
// anthropicSupportsLongContext are identical/cutlass-superset and lifted as-is.

// ProviderHeaders are the OpenRouter request headers a front-end advertises.
// Defaults to the fleet identity; callers may override per binary.
type ProviderHeaders struct {
	XTitle      string
	HTTPReferer string
}

// DefaultProviderHeaders identify the unified fleet runtime to OpenRouter.
// Generic by default; a deployment may override per binary (e.g. via
// OPENROUTER_X_TITLE / OPENROUTER_HTTP_REFERER) to surface its own product
// identity in the provider dashboard.
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
// fleet-namespaced), then the canonical FLEET_/legacy CHAT_/CUTLASS_ family so
// it still composes with the prefixed env config when desired.
func openRouterBaseURLOverride() string {
	if v := strings.TrimSpace(os.Getenv(openRouterBaseURLOverrideEnv)); v != "" {
		return v
	}
	return EnvPrefix("").lookup(openRouterBaseURLOverrideEnv)
}

// newOpenRouterProvider builds the fantasy OpenRouter provider with the given
// API key and headers. When OPENROUTER_BASE_URL is set (E2E only), it installs
// a URL-rewriting HTTP client so the real openai-go SDK still builds genuine
// OpenRouter chat-completions requests but dispatches them to the override
// origin — the provider boundary is the one and only thing stubbed.
func newOpenRouterProvider(apiKey string, headers ProviderHeaders) (fantasy.Provider, error) {
	if headers.XTitle == "" {
		headers = DefaultProviderHeaders
	}
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
		openrouter.WithHeaders(map[string]string{
			"X-Title":      headers.XTitle,
			"HTTP-Referer": headers.HTTPReferer,
		}),
	}
	if override := openRouterBaseURLOverride(); override != "" {
		if rt, err := newBaseURLRewriteTransport(override); err == nil {
			opts = append(opts, openrouter.WithHTTPClient(&http.Client{Transport: rt}))
		}
	}
	return openrouter.New(opts...)
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
	upstreamProviderGoogle    = "Google"
	upstreamProviderAnthropic = "Anthropic"
	upstreamProviderOpenAI    = "OpenAI"
	upstreamProviderMoonshot  = "Moonshot AI"
	upstreamProviderZAI       = "Z.AI"
	upstreamProviderDeepSeek  = "DeepSeek"

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
var canonicalUpstream = []struct {
	prefix        string
	name          string
	strict        bool
	quantizations []string
}{
	// Google serves this family alone, so the pin is STRICT (Only, no
	// fallbacks) and needs no serving-precision floor — there is no second
	// upstream to degrade onto.
	{"google/", upstreamProviderGoogle, true, nil},
	// The strong tier (DefaultMaxModel, Claude Opus 5) lives here: a soft pin
	// to Anthropic's own endpoint with graceful degradation onto the cloud
	// resellers of the same weights. The default-pin guard's exemption for that
	// slug — and the allow-list that enforces it — is recorded per SLUG in
	// officialPoolSlugs, not here.
	{"anthropic/", upstreamProviderAnthropic, false, nil},
	// This family carries the recommended everyday default (DefaultCoreModel,
	// GPT-5.6 Luna Pro), so this is the hot path for ordinary chat turns and
	// every scheduled run. Soft pin: OpenAI first, Azure and Amazon Bedrock as
	// fallbacks. No floor: see officialPoolSlugs for the per-slug evidence and
	// the allow-list that holds the pool to it.
	{"openai/", upstreamProviderOpenAI, false, nil},
	{"moonshotai/", upstreamProviderMoonshot, false, nil},
	{"z-ai/", upstreamProviderZAI, false, nil},
	// DeepSeek's own endpoint, non-strict. 28 OpenRouter endpoints serve this
	// family at context lengths from 131K to 1M and quantizations from fp4 to
	// fp8, so an unpinned route varies in both window and quality run to run —
	// on top of losing the per-upstream prompt cache. Order (not Only) keeps
	// graceful degradation if the first-party endpoint is unavailable.
	//
	// The fp8 floor is what makes that degradation graceful rather than silent:
	// an fp4 serving of a flash-tier model degrades in a way that reads as the
	// model being broken (token-level misspellings, topic drift, runaway output)
	// rather than as a routing event. DeepSeek's first-party endpoint is fp8, so
	// the floor costs nothing on the preferred route. This family is no longer
	// the everyday default, but the pin and the floor stay: operators still
	// select these slugs explicitly, and they are the reason it is safe to.
	{"deepseek/", upstreamProviderDeepSeek, false, fp8AndAbove},
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
	// 2026-09-22: OpenAI ×3, Azure ×2 — every endpoint quantization
	// "unknown". Amazon Bedrock was in the pool on 2026-09-21 and is not
	// today; it stays on the allow-list because a reseller of the official
	// weights coming back must not need a code change, and a name with no
	// endpoint behind it simply never matches.
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

// pinServesOfficialWeightsOnly reports whether the exact slug (alias marker
// stripped) is listed in officialPoolSlugs.
func pinServesOfficialWeightsOnly(modelSlug string) bool {
	_, ok := officialPoolSlugs[strings.TrimPrefix(modelSlug, "~")]
	return ok
}

// upstreamPinFor returns the OpenRouter provider routing policy for a model
// slug, or nil if the family has no canonical upstream. A leading `~` (floating
// alias) is stripped before prefix-matching so the alias inherits the lab's pin
// policy (chat behaviour; superset of cutlass which did not strip).
func upstreamPinFor(modelSlug string) *openrouter.Provider {
	matchSlug := strings.TrimPrefix(modelSlug, "~")
	for _, c := range canonicalUpstream {
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
		}
		return p
	}
	return nil
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
