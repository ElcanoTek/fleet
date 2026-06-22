package agentcore

import (
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

// newOpenRouterProvider builds the fantasy OpenRouter provider with the given
// API key and headers.
func newOpenRouterProvider(apiKey string, headers ProviderHeaders) (fantasy.Provider, error) {
	if headers.XTitle == "" {
		headers = DefaultProviderHeaders
	}
	return openrouter.New(
		openrouter.WithAPIKey(apiKey),
		openrouter.WithHeaders(map[string]string{
			"X-Title":      headers.XTitle,
			"HTTP-Referer": headers.HTTPReferer,
		}),
	)
}

// Canonical OpenRouter provider routing names for the upstreams we pin. Each
// matches the spelling OpenRouter's API expects in provider.order / provider.only.
const (
	upstreamProviderGoogle    = "Google"
	upstreamProviderAnthropic = "Anthropic"
	upstreamProviderOpenAI    = "OpenAI"
	upstreamProviderMoonshot  = "Moonshot AI"
)

// canonicalUpstream pins each model family to a single OpenRouter upstream so
// prompt caches (which are per-upstream) survive across calls. strict=true
// (Only + AllowFallbacks=false) is required for Google (encrypted thought
// signatures validate only at the minting upstream); strict=false (Order +
// AllowFallbacks=true) gives cache locality with graceful degradation.
var canonicalUpstream = []struct {
	prefix string
	name   string
	strict bool
}{
	{"google/", upstreamProviderGoogle, true},
	{"anthropic/", upstreamProviderAnthropic, false},
	{"openai/", upstreamProviderOpenAI, false},
	{"moonshotai/", upstreamProviderMoonshot, false},
}

// isAliasModel reports whether the slug is an OpenRouter floating alias
// (`~`-prefixed). Used to suppress reasoning for alias slugs: fantasy's
// send-side reasoning reconstruction keys on the raw slug's family prefix, which
// the `~` sigil defeats, so thinking signatures are silently dropped on the
// round-trip. Pinned slugs are unaffected.
func isAliasModel(modelSlug string) bool {
	return strings.HasPrefix(modelSlug, "~")
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
		return p
	}
	return nil
}

// anthropicSupportsLongContext reports whether a model is a Claude variant that
// supports the 1M-context beta (cutlass's conservative explicit allowlist).
func anthropicSupportsLongContext(model fantasy.LanguageModel) bool {
	if model == nil {
		return false
	}
	slug := strings.ToLower(strings.TrimSpace(model.Model()))
	return strings.Contains(slug, "claude-sonnet-4.5") ||
		strings.Contains(slug, "claude-sonnet-4.6") ||
		strings.Contains(slug, "claude-opus-4.6") ||
		strings.Contains(slug, "claude-opus-4.7") ||
		strings.Contains(slug, "claude-opus-4.8")
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
