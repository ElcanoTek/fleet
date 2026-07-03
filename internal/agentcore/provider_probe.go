package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Test-connection probe for admin-managed LLM providers. ProbeProvider makes
// ONE cheap, host-side HTTP call against the provider's real endpoint to
// answer "does this key/endpoint actually work?" before the first chat turn
// has to find out:
//
//   - openrouter: GET {base}/api/v1/key — the key-metadata endpoint; the only
//     OpenRouter read that authenticates (the models catalog is public). Honors
//     the OPENROUTER_BASE_URL fake-LLM seam like every other OpenRouter call.
//   - anthropic:  GET {base}/v1/models with x-api-key.
//   - openai:     GET {base}/models with a Bearer key (base defaults to the
//     OpenAI API; a custom base_url probes the OpenAI-compatible endpoint).
//   - ollama:     GET {base}/models — no key required.
//
// For types whose probe returns a model catalog, the row's listed models are
// checked against it: absences are reported as a WARNING (MissingModels), not
// a failure — gateways and Ollama tags legitimately under-enumerate.
//
// SECURITY: the API key goes into the request Authorization header and
// NOWHERE else — never into ProbeResult fields, errors, or logs. Response
// bodies are size-capped and only parsed for model ids, never echoed.

// ProbeResult is the wire-shaped outcome of one connection test.
type ProbeResult struct {
	OK bool `json:"ok"`
	// Status is the upstream HTTP status (0 when the request never completed).
	Status int `json:"status,omitempty"`
	// Detail is a one-line, key-free human summary.
	Detail string `json:"detail"`
	// ServedModelCount is how many models the endpoint enumerated (when the
	// probe endpoint returns a catalog; 0 otherwise).
	ServedModelCount int `json:"served_model_count,omitempty"`
	// MissingModels are listed on the provider row but absent from the
	// endpoint's catalog — a warning, not a failure.
	MissingModels []string `json:"missing_models,omitempty"`
	// LatencyMS is the round-trip time of the probe call.
	LatencyMS int64 `json:"latency_ms"`
}

const probeTimeout = 8 * time.Second

// probeBodyCap bounds how much of the upstream response is read — enough for
// any real models catalog, small enough that a misconfigured base_url pointing
// at something huge can't balloon memory.
const probeBodyCap = 4 << 20

// ProbeProvider tests cfg against its live endpoint. It never returns an
// error — every failure mode is folded into a !OK result with a key-free
// Detail, because the caller's job is to display it, not branch on it.
func ProbeProvider(ctx context.Context, cfg ProviderConfig) ProbeResult {
	endpoint, header, catalogued := probeTarget(cfg)
	if endpoint == "" {
		return ProbeResult{Detail: fmt.Sprintf("unknown provider type %q", cfg.Type)}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ProbeResult{Detail: "invalid probe URL (check base_url)"}
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// err can embed the URL but never the key (it travels in a header).
		return ProbeResult{Detail: fmt.Sprintf("endpoint unreachable: %v", err), LatencyMS: latency}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyCap))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ProbeResult{
			Status: resp.StatusCode, LatencyMS: latency,
			Detail: "authentication failed — the endpoint rejected the API key",
		}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return ProbeResult{
			Status: resp.StatusCode, LatencyMS: latency,
			Detail: fmt.Sprintf("endpoint answered HTTP %d (check base_url and type)", resp.StatusCode),
		}
	}

	res := ProbeResult{OK: true, Status: resp.StatusCode, LatencyMS: latency, Detail: "connected"}
	if !catalogued {
		res.Detail = "connected — key accepted"
		return res
	}
	served := parseProbeModelIDs(body)
	res.ServedModelCount = len(served)
	for _, m := range cfg.Models {
		if !slices.Contains(served, m) {
			res.MissingModels = append(res.MissingModels, m)
		}
	}
	switch {
	case len(served) == 0:
		res.Detail = "connected — endpoint returned no model catalog"
	case len(res.MissingModels) > 0:
		res.Detail = fmt.Sprintf("connected — %d models served, but %d listed model(s) not reported by the endpoint",
			len(served), len(res.MissingModels))
	default:
		res.Detail = fmt.Sprintf("connected — %d models served", len(served))
	}
	return res
}

// probeTarget picks the endpoint + auth headers per provider type. catalogued
// reports whether the endpoint's response is a models catalog worth parsing.
func probeTarget(cfg ProviderConfig) (endpoint string, header map[string]string, catalogued bool) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	switch cfg.Type {
	case ProviderTypeOpenRouter:
		if base == "" {
			if o := openRouterBaseURLOverride(); o != "" {
				base = strings.TrimRight(o, "/")
			} else {
				base = "https://openrouter.ai"
			}
		}
		return base + "/api/v1/key", map[string]string{"Authorization": "Bearer " + cfg.APIKey}, false
	case ProviderTypeAnthropic:
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return base + "/v1/models", map[string]string{
			"x-api-key":         cfg.APIKey,
			"anthropic-version": "2023-06-01",
		}, true
	case ProviderTypeOpenAI:
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return base + "/models", map[string]string{"Authorization": "Bearer " + cfg.APIKey}, true
	case ProviderTypeOllama:
		if base == "" {
			base = defaultOllamaBaseURL
		}
		h := map[string]string{}
		if strings.TrimSpace(cfg.APIKey) != "" {
			h["Authorization"] = "Bearer " + cfg.APIKey
		}
		return base + "/models", h, true
	default:
		return "", nil, false
	}
}

// parseProbeModelIDs extracts model ids from the OpenAI-compatible (and
// Anthropic, same shape) catalog envelope {"data":[{"id":...}]}.
func parseProbeModelIDs(body []byte) []string {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out
}
