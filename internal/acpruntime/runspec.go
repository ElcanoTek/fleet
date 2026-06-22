package acpruntime

// RunSpec is the per-run configuration the client hands the native ACP agent so
// the agent can reconstruct the SAME agentcore.Run call the in-process path
// makes. It travels in the ACP NewSession request's `_meta` (a reserved ACP
// field for client/agent metadata), JSON-encoded under MetaKeyRunSpec.
//
// What it carries is the run shape, NOT credentials beyond the model key:
//   - the model slugs + provider headers + sampling knobs, so the agent drives
//     the LLM loop itself (the model endpoint is allowed egress);
//   - the system prompt + the mode, so the loop assembles identically;
//   - the per-conversation MCP selection + gates (the agent advertises the same
//     tool surface; MCP credential injection + execution stay host-side).
//
// MCP credentials and the host filesystem are NOT shipped — those ride the
// delegation seam (`_fleet/*`) so they never enter the agent container.
type RunSpec struct {
	// Mode is "interactive" or "scheduled" (agentcore.Mode.String()).
	Mode string `json:"mode"`

	// ModelSlug / FallbackSlug are the OpenRouter slugs the agent resolves.
	ModelSlug    string `json:"modelSlug"`
	FallbackSlug string `json:"fallbackSlug,omitempty"`

	// SystemPrompt is the fully-assembled system prompt (notes already injected
	// by the client so the same text is used in both paths).
	SystemPrompt string `json:"systemPrompt"`

	// Temperature / MaxTokens mirror RunConfig.
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"maxTokens,omitempty"`

	// MaxCostUSD / MaxTotalTokens are the interactive per-turn ceilings.
	MaxCostUSD     float64 `json:"maxCostUSD,omitempty"`
	MaxTotalTokens int     `json:"maxTotalTokens,omitempty"`

	// Label is the human-readable task label echoed in the result.
	Label string `json:"label,omitempty"`

	// ProviderXTitle / ProviderHTTPReferer identify the run to OpenRouter.
	ProviderXTitle      string `json:"providerXTitle,omitempty"`
	ProviderHTTPReferer string `json:"providerHTTPReferer,omitempty"`
}

// PromptMeta is the per-prompt payload (the conversation history + the new user
// turn) carried in the ACP Prompt request's `_meta` under MetaKeyPromptMeta. The
// ACP `prompt` field carries only the latest user text for spec-compliance;
// the structured replayed history rides _meta so the agent rebuilds the exact
// message slice the in-process path would.
type PromptMeta struct {
	// Messages is the JSON-encoded fantasy message slice (history + new turn),
	// serialized by the client and replayed verbatim by the agent. Encoded as
	// raw JSON so neither side needs to depend on the other's message model.
	MessagesJSON string `json:"messagesJSON"`
}

// MetaKeyRunSpec / MetaKeyPromptMeta are the `_meta` map keys the RunSpec /
// PromptMeta travel under.
const (
	MetaKeyRunSpec    = "fleet.runSpec"
	MetaKeyPromptMeta = "fleet.promptMeta"
)
