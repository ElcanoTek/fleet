package agentcore

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/openai-go/option"
)

// Multi-provider LLM support (#289). Fleet routed every model through OpenRouter
// as a singleton gateway. This adds native provider backends — Anthropic,
// OpenAI, Ollama (via its OpenAI-compatible API), and since #1703 any
// OpenAI-compatible endpoint, the Vercel AI Gateway and Azure OpenAI —
// alongside OpenRouter, selected by a client-bundle `providers:` block or
// admin-managed rows. The engine itself is already provider-agnostic (it holds
// fantasy.LanguageModel interfaces); the coupling was entirely in how those
// models were constructed (always the OpenRouter provider).
// This layer resolves the RIGHT provider for a given model slug and builds the
// model through it.
//
// Backward compatibility is the hard requirement: a bundle with NO providers:
// block behaves exactly as before — a single catch-all OpenRouter provider — so
// existing deployments that set only OPENROUTER_API_KEY are byte-for-byte
// unchanged (see NewModelResolver).

// ProviderType enumerates the supported LLM provider backends.
type ProviderType string

const (
	// ProviderTypeOpenRouter routes through OpenRouter (the historical default,
	// and a catch-all that accepts any slug). Honors the OPENROUTER_BASE_URL E2E
	// fake-LLM seam.
	ProviderTypeOpenRouter ProviderType = "openrouter"
	// ProviderTypeAnthropic calls the Anthropic API directly.
	ProviderTypeAnthropic ProviderType = "anthropic"
	// ProviderTypeOpenAI calls the OpenAI API directly.
	ProviderTypeOpenAI ProviderType = "openai"
	// ProviderTypeOllama reaches a local (or remote) Ollama server through its
	// OpenAI-compatible endpoint — there is no dedicated fantasy Ollama provider,
	// so it is the OpenAI provider pointed at the Ollama base URL.
	ProviderTypeOllama ProviderType = "ollama"
	// ProviderTypeOpenAICompat reaches any OpenAI-compatible endpoint (vLLM,
	// LM Studio, llama.cpp server, a hosted gateway) through fantasy's
	// openaicompat provider, which also reads the non-OpenAI extensions those
	// servers send (reasoning_content and friends). There is no default
	// endpoint, so base_url is required; the key is optional.
	ProviderTypeOpenAICompat ProviderType = "openaicompat"
	// ProviderTypeVercel calls the Vercel AI Gateway. Its model ids are
	// "<creator>/<model>", the same shape as OpenRouter slugs, so a vercel
	// entry that LISTS one takes that slug over from an OpenRouter catch-all
	// (a listed model beats a catch-all in selectProvider); an explicit
	// "<name>/<creator>/<model>" route reaches it without that.
	ProviderTypeVercel ProviderType = "vercel"
	// ProviderTypeAzure calls an Azure OpenAI resource through its
	// version-free v1 API, the way fantasy's azure provider does, with the key
	// in the Api-Key header. base_url is the resource's endpoint as the Azure
	// portal shows it, and a model slug is one of the resource's deployment
	// names. Keys only: Microsoft Entra ID sign-in is not supported.
	ProviderTypeAzure ProviderType = "azure"
)

// ProviderTypeSpec is what every configuration surface needs to know about one
// provider type: the bundle loader (clientconfig), the admin store, and the
// resolver itself all check a row against the same spec, so its rules live in
// one place. Adding a type still takes a buildProvider case and a probeTarget
// case (TestEveryProviderTypeBuildsAndProbes fails without them) and an entry
// in the web form's list, which a scripts/ test pins to this table.
type ProviderTypeSpec struct {
	Type ProviderType
	// APIKeyRequired: an entry of this type cannot be built without a key.
	APIKeyRequired bool
	// BaseURLRequired: the type has no default endpoint, so base_url must be
	// set.
	BaseURLRequired bool
}

// providerTypeSpecs is the supported set, in the order error messages and the
// admin form list them.
var providerTypeSpecs = []ProviderTypeSpec{
	{Type: ProviderTypeOpenRouter, APIKeyRequired: true},
	{Type: ProviderTypeAnthropic, APIKeyRequired: true},
	{Type: ProviderTypeOpenAI, APIKeyRequired: true},
	{Type: ProviderTypeOllama},
	{Type: ProviderTypeOpenAICompat, BaseURLRequired: true},
	{Type: ProviderTypeVercel, APIKeyRequired: true},
	{Type: ProviderTypeAzure, APIKeyRequired: true, BaseURLRequired: true},
}

// LookupProviderType returns the spec for a provider type name (exact match;
// callers normalize case and whitespace first).
func LookupProviderType(name string) (ProviderTypeSpec, bool) {
	for _, spec := range providerTypeSpecs {
		if string(spec.Type) == name {
			return spec, true
		}
	}
	return ProviderTypeSpec{}, false
}

// ProviderTypes lists the supported provider types in table order.
func ProviderTypes() []ProviderType {
	out := make([]ProviderType, len(providerTypeSpecs))
	for i, spec := range providerTypeSpecs {
		out[i] = spec.Type
	}
	return out
}

// ProviderTypeChoices is the supported set as one "a|b|c" string, for the
// "(want …)" part of an unknown-type error.
func ProviderTypeChoices() string {
	names := make([]string, len(providerTypeSpecs))
	for i, spec := range providerTypeSpecs {
		names[i] = string(spec.Type)
	}
	return strings.Join(names, "|")
}

// defaultOllamaBaseURL is the OpenAI-compatible endpoint a local Ollama server
// exposes when a provider entry omits base_url.
const defaultOllamaBaseURL = "http://localhost:11434/v1"

// providerOwnedOpenAIOptions undoes what the OpenAI Go SDK under fantasy's
// openaicompat and vercel providers copies from fleet's own environment onto
// every request: OPENAI_API_KEY or OPENAI_ADMIN_KEY as the bearer key when the
// provider has none, OPENAI_ORG_ID and OPENAI_PROJECT_ID as the
// OpenAI-Organization and OpenAI-Project headers, and every header
// OPENAI_CUSTOM_HEADERS lists. (OPENAI_BASE_URL needs nothing: fantasy always
// passes a base URL, which overrides it.) Those describe the host's own OpenAI
// setup; on a request to an endpoint an admin named they would hand that
// endpoint fleet's credentials. The SDK's switch to skip its environment
// defaults is internal, so each one is overridden here, as the last options
// applied. With a key, Authorization is set to it explicitly; without one it
// is deleted, so the request carries no key at all. Either way the SDK then
// skips its own key logic, which is what keeps both env keys off the request.
// Azure sends its key in another header, so azureOpenAIOptions shares the
// header removals (hostOpenAIHeaderRemovals) but not this Authorization step.
func providerOwnedOpenAIOptions(apiKey string) []option.RequestOption {
	opts := hostOpenAIHeaderRemovals()
	if apiKey == "" {
		return append(opts, option.WithHeaderDel("Authorization"))
	}
	return append(opts, option.WithHeader("Authorization", "Bearer "+apiKey))
}

// sdkOwnedHeaders are the headers hostOpenAIHeaderRemovals never deletes by
// name: the SDK sets them on every request (Authorization is handled
// separately, from the provider's own key).
var sdkOwnedHeaders = map[string]bool{
	"authorization": true, "content-type": true, "accept": true, "user-agent": true,
}

// hostOpenAIHeaderRemovals deletes the OpenAI-Organization and OpenAI-Project
// headers and every header OPENAI_CUSTOM_HEADERS names. A custom header that
// shares its name with one the SDK sets itself (Content-Type, Accept,
// User-Agent) is left alone: deleting it would strip the SDK's own value too.
func hostOpenAIHeaderRemovals() []option.RequestOption {
	opts := []option.RequestOption{
		option.WithHeaderDel("OpenAI-Organization"),
		option.WithHeaderDel("OpenAI-Project"),
	}
	for _, line := range strings.Split(os.Getenv("OPENAI_CUSTOM_HEADERS"), "\n") {
		if name, _, ok := strings.Cut(line, ":"); ok {
			if name = strings.TrimSpace(name); name != "" && !sdkOwnedHeaders[strings.ToLower(name)] {
				opts = append(opts, option.WithHeaderDel(name))
			}
		}
	}
	return opts
}

// azureEndpointPattern matches an Azure OpenAI resource endpoint on any of the
// host names Azure gives one, capturing the resource name. It is fantasy's
// own pattern (providers/azure, v0.45.3).
var azureEndpointPattern = regexp.MustCompile(`^(?:https?://)?([a-zA-Z0-9-]+)\.(?:openai|cognitiveservices|services\.ai)\.azure\.com(?:/.*)?$`)

// azureBaseURL is the API root an azure entry's base_url resolves to, the same
// as fantasy's azure.WithBaseURL gives (its parser is unexported): an endpoint
// on an Azure host, whatever its path, becomes that resource's v1 API root, and
// any other URL is used as given, with https:// added when it has no scheme.
// Turns and the connection test both resolve through it, so the test reaches
// the endpoint turns use. TestAzureMatchesFantasyAzure pins the parity.
func azureBaseURL(base string) string {
	if m := azureEndpointPattern.FindStringSubmatch(base); m != nil {
		return "https://" + m[1] + ".openai.azure.com/openai/v1"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "https://" + base
	}
	return base
}

// azureOpenAIOptions build what fantasy's azure.New builds: its OpenAI
// provider named "azure", pointed at azureBaseURL(base), with the key in the
// Api-Key header and no Authorization header. azure.New takes no SDK options,
// so it cannot be given the host-header removals every OpenAI-SDK-backed type
// needs; here they come first, then the Api-Key, so a custom header of that
// name cannot replace the provider's own key.
func azureOpenAIOptions(apiKey, base string) []openai.Option {
	sdk := append(hostOpenAIHeaderRemovals(),
		option.WithHeaderDel("Authorization"),
		option.WithHeader("Api-Key", apiKey),
	)
	return []openai.Option{
		openai.WithName(azure.Name),
		openai.WithBaseURL(azureBaseURL(base)),
		openai.WithSDKOptions(sdk...),
	}
}

// ProviderConfig is one resolved LLM provider. The API-key VALUE is resolved
// host-side (from the env var the manifest names) BEFORE this struct is built and
// handed to agentcore — the secret never lives in the manifest or in agentcore's
// config surface, only in the process env and this transient field. Models lists
// the slugs the provider serves; empty means catch-all (matches any slug).
type ProviderConfig struct {
	Name    string
	Type    ProviderType
	APIKey  string
	BaseURL string
	Models  []string
	// ContextWindowTokens is the provider/model's actual input+output context
	// limit when no authoritative catalog is available. Zero selects a
	// conservative provider-specific fail-safe; negative values are invalid.
	ContextWindowTokens int
	// FallbackProviders is the workspace's ordered provider failover chain.
	// cmd/fleet attaches the same immutable slice to bundle-backed entries.
	FallbackProviders []string
}

// selectProvider picks the provider that serves slug and returns it plus the
// provider-local model slug. Routing precedence:
//
//  1. Explicit "<provider-name>/<model>" — when the segment before the first "/"
//     exactly matches a CONFIGURED provider name, that provider is used and the
//     remainder is the model slug. (An OpenRouter slug like
//     "anthropic/claude-opus-4.8" is NOT explicit routing unless a provider is
//     literally named "anthropic"; "anthropic" there is an OpenRouter upstream,
//     not a fleet provider name.)
//  2. Implicit models-list match — the first provider (in manifest order) whose
//     Models list contains the slug.
//  3. Catch-all — the first provider with an empty Models list.
//
// A specifically-listed model always beats a catch-all regardless of order, so a
// catch-all OpenRouter entry can sit anywhere in the list.
func selectProvider(providers []ProviderConfig, slug string) (ProviderConfig, string, error) {
	// 1. Explicit name-prefixed routing.
	if name, rest, ok := strings.Cut(slug, "/"); ok && strings.TrimSpace(rest) != "" {
		for i := range providers {
			if providers[i].Name == name {
				return providers[i], rest, nil
			}
		}
	}
	// 2. Explicit models-list match (in manifest order).
	for i := range providers {
		if len(providers[i].Models) > 0 && slices.Contains(providers[i].Models, slug) {
			return providers[i], slug, nil
		}
	}
	// 3. First catch-all.
	for i := range providers {
		if len(providers[i].Models) == 0 {
			return providers[i], slug, nil
		}
	}
	return ProviderConfig{}, "", fmt.Errorf("no configured provider serves model %q", slug)
}

// buildProvider constructs the fantasy.Provider for a resolved ProviderConfig.
// It performs no network I/O — it builds the client handle; model loading (which
// may touch the network) happens later in Resolve. OpenRouter reuses
// newOpenRouterProvider so the OPENROUTER_BASE_URL E2E seam keeps working.
// The type's required fields are checked against its ProviderTypeSpec first, so
// the bundle loader, the admin store and this constructor agree on them.
func buildProvider(cfg ProviderConfig, headers ProviderHeaders) (fantasy.Provider, error) {
	spec, ok := LookupProviderType(string(cfg.Type))
	if !ok {
		return nil, fmt.Errorf("unknown provider type %q (want %s)", cfg.Type, ProviderTypeChoices())
	}
	key := strings.TrimSpace(cfg.APIKey)
	base := strings.TrimSpace(cfg.BaseURL)
	if spec.APIKeyRequired && key == "" {
		return nil, fmt.Errorf("%s provider requires an API key", cfg.Type)
	}
	if spec.BaseURLRequired && base == "" {
		return nil, fmt.Errorf("%s provider requires a base_url", cfg.Type)
	}
	switch cfg.Type {
	case ProviderTypeOpenRouter:
		return newOpenRouterProvider(cfg.APIKey, headers)
	case ProviderTypeAnthropic:
		opts := []anthropic.Option{anthropic.WithAPIKey(cfg.APIKey)}
		if base != "" {
			opts = append(opts, anthropic.WithBaseURL(base))
		}
		return anthropic.New(opts...)
	case ProviderTypeOpenAI:
		opts := []openai.Option{openai.WithAPIKey(cfg.APIKey)}
		if base != "" {
			opts = append(opts, openai.WithBaseURL(base))
		}
		return openai.New(opts...)
	case ProviderTypeOllama:
		// Ollama speaks the OpenAI wire format; reach it via the OpenAI provider
		// pointed at the Ollama endpoint. Local Ollama needs no API key, but a
		// placeholder is still sent when none is configured (Ollama ignores
		// it): with no key of its own the OpenAI SDK would send the host's
		// OPENAI_API_KEY to the Ollama endpoint instead. Do not drop it.
		if base == "" {
			base = defaultOllamaBaseURL
		}
		if key == "" {
			key = "ollama"
		}
		return openai.New(openai.WithAPIKey(key), openai.WithBaseURL(base))
	case ProviderTypeOpenAICompat:
		opts := []openaicompat.Option{
			openaicompat.WithBaseURL(base),
			openaicompat.WithSDKOptions(providerOwnedOpenAIOptions(key)...),
		}
		if key != "" {
			opts = append(opts, openaicompat.WithAPIKey(key))
		}
		return openaicompat.New(opts...)
	case ProviderTypeVercel:
		opts := []vercel.Option{
			vercel.WithAPIKey(key),
			vercel.WithSDKOptions(providerOwnedOpenAIOptions(key)...),
		}
		if base != "" {
			opts = append(opts, vercel.WithBaseURL(base))
		}
		return vercel.New(opts...)
	case ProviderTypeAzure:
		return openai.New(azureOpenAIOptions(key, base)...)
	default:
		// A type in providerTypeSpecs with no case here; the build test fails
		// on this before it can ship.
		return nil, fmt.Errorf("provider type %q has no constructor", cfg.Type)
	}
}
