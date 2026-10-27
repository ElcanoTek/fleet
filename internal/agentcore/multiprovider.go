package agentcore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/openai-go/option"
)

// Multi-provider LLM support (#289). Fleet routed every model through OpenRouter
// as a singleton gateway. This adds native provider backends — Anthropic,
// OpenAI, Ollama (via its OpenAI-compatible API), and since #1703 any
// OpenAI-compatible endpoint, the Vercel AI Gateway, Azure OpenAI, the Gemini
// API and Amazon Bedrock — alongside OpenRouter, selected by a client-bundle `providers:`
// block or admin-managed rows. The engine itself is already provider-agnostic
// (it holds fantasy.LanguageModel interfaces); the coupling was entirely in
// how those models were constructed (always the OpenRouter provider).
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
	// names. Keys only: Microsoft Entra ID sign-in is not supported. fantasy
	// tells a reasoning model by its id alone, and only then sends
	// max_completion_tokens and no temperature, which those models require: an
	// id containing "gpt-5" in any case, or starting with o1, o3, o4 or oss or
	// containing -o1, -o3, -o4 or -oss. So a GPT-5, o-series or gpt-oss
	// deployment must carry its model in its name (gpt-5-prod, o4-mini), or
	// Azure rejects every turn on it, and a newer reasoning family outside that
	// match (GPT-6) is sent max_tokens and a temperature whatever its name.
	ProviderTypeAzure ProviderType = "azure"
	// ProviderTypeGoogle calls the Gemini API (Google AI Studio) with a Gemini
	// API key, through fantasy's google provider. Model ids are Gemini's own
	// (gemini-3.8-flash). Vertex AI is not supported: fantasy's Vertex path
	// drops a stored key and signs in with Google credentials found on the
	// host, so fleet always uses the Gemini API backend and refuses the model
	// ids fantasy would send to Vertex instead (geminiOnlyProvider).
	ProviderTypeGoogle ProviderType = "google"
	// ProviderTypeBedrock calls Amazon Bedrock with a Bedrock API key, through
	// fantasy's bedrock provider. That provider is fantasy's anthropic provider
	// over Bedrock's InvokeModel API, so it reaches Claude models only, by
	// their Bedrock ids: an inference profile's (us.anthropic.claude-…), which
	// newer Claude models need on demand, or a model's own (anthropic.claude-…).
	// A key works only in the AWS Region it was made in, so the entry names
	// that Region (Region; us-east-1 by default). AWS credentials found on the
	// host (profiles, SSO, instance roles) are never used: an entry always
	// carries its own key (bedrockKeyClient).
	ProviderTypeBedrock ProviderType = "bedrock"
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
	// Region: the type's endpoint is picked by an AWS Region
	// (ProviderConfig.Region). Every other type refuses one.
	Region bool
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
	{Type: ProviderTypeGoogle, APIKeyRequired: true},
	{Type: ProviderTypeBedrock, APIKeyRequired: true, Region: true},
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

// awsRegionPattern matches an AWS Region name of the form the endpoint host
// bedrock-runtime.<region>.amazonaws.com is built from (us-east-1,
// us-gov-west-1, ap-southeast-4). The Region becomes part of that host name,
// so nothing else may pass. A Region whose endpoints are not under
// amazonaws.com (China, the European Sovereign Cloud) is reached through a
// base_url instead, with no region.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// CheckProviderFields checks an entry's region and base_url against what its
// type accepts, beyond the presence rules of its spec. The bundle loader, the
// admin store and buildProvider all call it, so an entry is refused the same
// way wherever it is written:
//
//   - a region is accepted only by a type that takes one (bedrock), and must
//     be an AWS Region name, since it is put into the endpoint's host name;
//   - a bedrock base_url is an origin only. The Bedrock client rewrites a
//     request's path to /model/<id>/invoke only when that path is exactly
//     /v1/messages, so behind a path prefix it would send the Anthropic API's
//     request instead. A query, a fragment or credentials in it would also
//     make the connection test call another URL than turns do, or put the
//     credentials into error text.
func CheckProviderFields(spec ProviderTypeSpec, baseURL, region string) error {
	baseURL, region = strings.TrimSpace(baseURL), strings.TrimSpace(region)
	switch {
	case region != "" && !spec.Region:
		return fmt.Errorf("region applies only to a bedrock provider, not to type %s", spec.Type)
	case region != "" && !awsRegionPattern.MatchString(region):
		return fmt.Errorf("region %q is not an AWS Region name, such as us-east-1", region)
	}
	if spec.Type == ProviderTypeBedrock && baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(baseURL, "#") {
			return fmt.Errorf("a bedrock base_url is an endpoint's origin with no path, query or credentials, such as https://bedrock-runtime.us-east-1.amazonaws.com")
		}
	}
	return nil
}

// defaultOllamaBaseURL is the OpenAI-compatible endpoint a local Ollama server
// exposes when a provider entry omits base_url.
const defaultOllamaBaseURL = "http://localhost:11434/v1"

// defaultGeminiBaseURL is the Gemini API's root (the SDK adds /v1beta). A
// google entry always passes a base URL: given none, the genai SDK would take
// one from the host's GOOGLE_GEMINI_BASE_URL and send the entry's key there.
const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

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

// azureOpenAIOptions returns the options that build what fantasy's azure.New
// builds: its OpenAI provider named "azure", pointed at azureBaseURL(base),
// with the key in the Api-Key header and no Authorization header. azure.New
// takes no SDK options, so it cannot be given the host-header removals every
// OpenAI-SDK-backed type needs; here they come first, then the Api-Key, so a
// custom header of that name cannot replace the provider's own key.
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
	// Region is the AWS Region a bedrock entry calls (defaultBedrockRegion when
	// empty). No other type takes one.
	Region string
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
	if err := CheckProviderFields(spec, base, cfg.Region); err != nil {
		return nil, err
	}
	switch cfg.Type {
	case ProviderTypeOpenRouter:
		return newOpenRouterProvider(cfg.APIKey, headers)
	case ProviderTypeAnthropic:
		opts := []anthropic.Option{anthropic.WithAPIKey(cfg.APIKey), anthropic.WithHTTPClient(anthropicHTTPClient)}
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
	case ProviderTypeGoogle:
		p, err := google.New(googleOptions(key, base)...)
		if err != nil {
			return nil, err
		}
		return geminiOnlyProvider{p}, nil
	case ProviderTypeBedrock:
		return bedrock.New(bedrockOptions(key, base, strings.TrimSpace(cfg.Region))...)
	default:
		// A type in providerTypeSpecs with no case here; the build test fails
		// on this before it can ship.
		return nil, fmt.Errorf("provider type %q has no constructor", cfg.Type)
	}
}

// googleOptions select the Gemini API backend with the entry's key and always
// name a base URL. The explicit key and backend make the genai SDK ignore the
// host's GOOGLE_API_KEY, GEMINI_API_KEY, GOOGLE_GENAI_USE_VERTEXAI,
// GOOGLE_GENAI_USE_ENTERPRISE, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION and
// GOOGLE_CLOUD_REGION; the explicit base URL keeps GOOGLE_GEMINI_BASE_URL and
// GOOGLE_VERTEX_BASE_URL out too (see defaultGeminiBaseURL).
func googleOptions(apiKey, base string) []google.Option {
	if base == "" {
		base = defaultGeminiBaseURL
	}
	return []google.Option{google.WithGeminiAPIKey(apiKey), google.WithBaseURL(base), google.WithHTTPClient(googleHTTPClient)}
}

// googleHTTPClient carries a google entry's requests. Like the connection
// test it follows a redirect only within the endpoint's origin, as the OpenAI
// SDK does for the azure type. Without it fantasy uses http.DefaultClient,
// which follows one anywhere and copies x-goog-api-key along.
var googleHTTPClient = &http.Client{CheckRedirect: sameOriginRedirect}

// geminiOnlyProvider refuses the model ids fantasy's google provider sends to
// Anthropic on Vertex AI instead of the Gemini API: any id containing "claude"
// or "anthropic". That path signs in with the Google credentials it finds on
// the host, which a google entry never configures, so such an id (an
// OpenRouter slug reaching a google catch-all, say) fails here instead. The
// models it returns pass Gemini only tool schemas it accepts
// (geminiSchemaModel).
type geminiOnlyProvider struct{ fantasy.Provider }

// errNotAGeminiModel is geminiOnlyProvider's refusal. A fallback chain skips
// a google entry that refuses the slug instead of failing the whole resolve.
var errNotAGeminiModel = errors.New("not a Gemini API model")

func (p geminiOnlyProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	if id := strings.ToLower(modelID); strings.Contains(id, "claude") || strings.Contains(id, "anthropic") {
		return nil, fmt.Errorf("model %q is %w: a google provider does not reach Claude models", modelID, errNotAGeminiModel)
	}
	lm, err := p.Provider.LanguageModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	return geminiSchemaModel{lm}, nil
}

// anthropicHTTPClient carries an anthropic entry's requests. It is the
// Anthropic SDK's own default client (anthropicTransport) with one change: it
// follows a redirect only within the endpoint's origin, as the connection
// test does and as the OpenAI SDK does for the OpenAI-wire types. The SDK's
// client follows one to any host, and Go copies x-api-key along (it drops
// only Authorization and cookies), the request body too on a 307 or 308.
var anthropicHTTPClient = &http.Client{Transport: anthropicTransport(), CheckRedirect: refuseCrossOriginRedirect}

// anthropicTransport is http.DefaultTransport with the response-header
// timeout the Anthropic SDK's default client sets, so a server that accepts
// the connection but never answers still fails after 10 minutes instead of
// hanging the call. Response bodies, and so long streams, are not bounded.
func anthropicTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.ResponseHeaderTimeout = 10 * time.Minute
	return t
}

// refuseCrossOriginRedirect is sameOriginRedirect for turns: a redirect to
// another origin fails the request with an error that names it, where the
// SDK would otherwise take the 3xx itself for the reply and fail on its body.
// The error is a ProviderError that is not retryable, carrying the redirect's
// status: the url.Error Go wraps it in is a net.Error, which fantasy's retry
// and fleet's resilience loop would otherwise retry, against the same
// endpoint, before failing the turn.
func refuseCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	err := sameOriginRedirect(req, via)
	if !errors.Is(err, http.ErrUseLastResponse) {
		return err
	}
	status := 0
	if req.Response != nil {
		status = req.Response.StatusCode
	}
	return &fantasy.ProviderError{
		Title:      "endpoint redirected to another host",
		Message:    fmt.Sprintf("refused a redirect to another origin (%s://%s): turns follow a redirect only within the base_url's origin", req.URL.Scheme, req.URL.Host),
		StatusCode: status,
	}
}

// defaultBedrockRegion is the AWS Region a bedrock entry calls when it names
// none, as fantasy's bedrock provider does.
const defaultBedrockRegion = "us-east-1"

// bedrockRuntimeURL is the Bedrock Runtime endpoint a bedrock entry calls,
// for turns and for the connection test alike: its base_url when set, else
// its Region's public endpoint, the one the Anthropic SDK would build.
func bedrockRuntimeURL(base, region string) string {
	if base != "" {
		return strings.TrimRight(base, "/")
	}
	if region == "" {
		region = defaultBedrockRegion
	}
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// bedrockOptions name the entry's endpoint outright and send every request
// with the entry's own key (bedrockKeyClient). Given a key, fantasy loads no
// AWS configuration from the host (credentials, profiles, AWS_REGION); the
// one host setting the SDK still reads, AWS_BEARER_TOKEN_BEDROCK, is
// overwritten on every request. fantasy's bedrock path also skips the
// Anthropic SDK's environment defaults (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL,
// ANTHROPIC_CUSTOM_HEADERS and the rest).
func bedrockOptions(apiKey, base, region string) []bedrock.Option {
	if region == "" {
		region = defaultBedrockRegion
	}
	return []bedrock.Option{
		bedrock.WithAPIKey(apiKey),
		bedrock.WithRegion(region),
		bedrock.WithBaseURL(bedrockRuntimeURL(base, region)),
		bedrock.WithHTTPClient(bedrockKeyClient{key: apiKey}),
	}
}

// bedrockHTTPClient carries a bedrock entry's requests. It is built like
// anthropicHTTPClient, for the same reasons: it keeps the Anthropic SDK's
// response-header timeout, which a client of our own replaces, and it follows
// a redirect only within the endpoint's origin, where the SDK's client would
// follow one anywhere, the request body with it.
var bedrockHTTPClient = &http.Client{Transport: anthropicTransport(), CheckRedirect: refuseCrossOriginRedirect}

// bedrockKeyClient sends each Bedrock request with the entry's own key. The
// Anthropic SDK's Bedrock adapter prefers the host's AWS_BEARER_TOKEN_BEDROCK
// to the key it is given, so on a host with that variable set every bedrock
// entry would send the host's token, to whatever endpoint the entry names.
// The adapter has set Authorization by the time a request gets here, and a
// request made with a key is not signed, so setting it again changes nothing
// else.
type bedrockKeyClient struct{ key string }

func (c bedrockKeyClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.key)
	//nolint:gosec // G704: the URL is the entry's own endpoint (bundle or admin-row config: its base_url or its Region's host), never request or model input; redirects stay within its origin.
	return bedrockHTTPClient.Do(req)
}
