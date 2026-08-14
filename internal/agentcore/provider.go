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
	{"google/", upstreamProviderGoogle, true, nil},
	{"anthropic/", upstreamProviderAnthropic, false, nil},
	{"openai/", upstreamProviderOpenAI, false, nil},
	{"moonshotai/", upstreamProviderMoonshot, false, nil},
	{"z-ai/", upstreamProviderZAI, false, nil},
	// DeepSeek's own endpoint, non-strict. 28 OpenRouter endpoints serve this
	// family at context lengths from 131K to 1M and quantizations from fp4 to
	// fp8, so an unpinned route varies in both window and quality run to run —
	// on top of losing the per-upstream prompt cache. Order (not Only) keeps
	// graceful degradation if the first-party endpoint is unavailable.
	//
	// The fp8 floor is what makes that degradation graceful rather than silent.
	// This family is the recommended everyday default (DefaultCoreModel), so the
	// fallback path is the hot path for ordinary chat turns, and an fp4 serving
	// of a flash-tier model degrades in a way that reads as the model being
	// broken (token-level misspellings, topic drift, runaway output) rather than
	// as a routing event. DeepSeek's first-party endpoint is fp8, so the floor
	// costs nothing on the preferred route.
	{"deepseek/", upstreamProviderDeepSeek, false, fp8AndAbove},
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
