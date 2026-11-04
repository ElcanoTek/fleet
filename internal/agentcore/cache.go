// agentcore — prompt-caching helpers (merged from chat + cutlass cache.go).
//
// We route everything through OpenRouter. OpenRouter's own hook in the fantasy
// library reads the `anthropic.Name` namespace from each message's
// ProviderOptions and emits `cache_control: {type: "ephemeral"}` on the
// corresponding wire content block — regardless of which upstream the slug
// routes to. So the same per-message marker works for any upstream that accepts
// explicit breakpoints via OpenRouter (today: Anthropic and Google).
//
// Strategy, inspired by crush's battle-tested approach:
//
//  1. Attach cache_control to the LAST system message (keeps the large, stable
//     system-prompt + persona/protocols + MCP roster cached across turns).
//  2. (additive, cutlass) Attach cache_control to the compaction summary, if
//     present — a stable boundary between the cached head and the evolving tail.
//  3. Attach cache_control to the LAST TWO non-system messages (rolling recency
//     window). Anthropic allows 4 cache breakpoints per request; chat uses 3,
//     and the additive compaction breakpoint brings the cutlass path to 4.
//
// Gemini (stablePrefixOnly) gets 1 and 2 only, and none at all once the rest
// of the prompt outgrows the prefix they mark (tailOutgrowsMarkedPrefix).
//
// Divergence reconciliation:
//   - supportsExplicitBreakpoints is chat's version: it strips a leading `~`
//     (OpenRouter floating-alias sigil) before prefix-matching, so an alias
//     inherits the lab's caching policy. cutlass did not strip `~`; chat's is
//     the strict superset and is taken here.
//   - The compaction-summary 4th breakpoint is cutlass-only and is exposed here
//     as an additive option (WithCompactionSummaryBreakpoint) rather than
//     forced on, so the chat 3-breakpoint layout is unchanged by default.
//   - The kill-switch env var is parameterized: <PREFIX>_DISABLE_PROMPT_CACHE,
//     with CHAT_/CUTLASS_ back-compat aliases (see env.go).
package agentcore

import (
	"context"
	"slices"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

// explicitBreakpointPrefixes lists OpenRouter slug prefixes whose upstream
// honors explicit per-message `cache_control` breakpoints. Adding a new
// provider that supports explicit breakpoints = one line here.
var explicitBreakpointPrefixes = []string{
	"anthropic/",
	"google/",
	// Provider-local slugs (#289 native providers). A model routed through a
	// native provider carries the bare model id — selectProvider strips the
	// "<provider-name>/" routing prefix and fantasy's Model() returns what was
	// loaded — so "claude-fable-5" must match here or native-Anthropic runs get
	// NO breakpoints and pay full input price every turn. fantasy's native
	// anthropic provider reads the same anthropic.Name ProviderOptions the
	// OpenRouter hook does, so the markers translate identically. Bare slugs
	// never reach OpenRouter (its catalog requires the "<lab>/" form), so this
	// cannot mis-mark an implicit-cache upstream.
	"claude-",
	"gemini-",
}

// bedrockClaudeMarker is in every Bedrock id for a Claude model: the model's
// own (anthropic.claude-…) and an inference profile's (us.anthropic.claude-…,
// global.anthropic.claude-…). Those ids have no "<lab>/" prefix to match, and
// fantasy's bedrock provider is its anthropic provider, which reads the same
// anthropic.Name markers. No OpenRouter slug contains it. An ARN gets markers
// only when it contains such an id: an application-inference-profile or
// provisioned-throughput ARN names no model and gets none.
const bedrockClaudeMarker = "anthropic.claude-"

// supportsExplicitBreakpoints reports whether the model slug routes to a
// provider that uses explicit per-message cache_control. Everything else falls
// back to the upstream's implicit caching and receives no markers.
//
// Strips a leading `~` (OpenRouter floating-alias sigil) before matching so
// `~anthropic/claude-sonnet-latest` inherits the same caching policy as
// `anthropic/claude-sonnet-4.6` (chat behavior; superset of cutlass).
func supportsExplicitBreakpoints(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "~")
	for _, prefix := range explicitBreakpointPrefixes {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return strings.Contains(m, bedrockClaudeMarker)
}

// stablePrefixOnly reports whether the model's family must carry breakpoints on
// the stable prefix ONLY — never the rolling recency window. Gemini on
// OpenRouter turns the LAST breakpoint of a request into an explicit context
// cache (a billed write, 5-minute TTL that does not refresh) and ignores the
// rest. A breakpoint on the moving tail therefore writes a brand-new cache of
// the whole prompt on every request and never reads one back; measured on
// google/gemini-3.8-flash (2026-10-14): ~2x the prompt billed and a full write
// per request, ~7x the cost of a single system-prompt breakpoint, which writes
// once and is read back on every later request. Gemini 4 Argon has no explicit
// caching (implicit only), so the placement neither helps nor hurts it; the
// same rule keeps the family consistent. See docs/PROMPT-CACHE-CONTRACT.md.
//
// Once what follows the stable prefix outgrows it, such a model gets no
// breakpoint at all (tailOutgrowsMarkedPrefix): a request that uses an
// explicit cache gets nothing past it from Google's implicit cache.
func stablePrefixOnly(model string) bool {
	m := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "~")
	return strings.HasPrefix(m, "google/gemini") || strings.HasPrefix(m, "gemini-")
}

// cacheControlEphemeral is the only cache_control type the codebase emits.
const cacheControlEphemeral = "ephemeral"

// cacheControlOptions returns the ProviderOptions value to attach to a message
// we want to treat as a cache breakpoint. The openrouter hook reads the
// `anthropic.Name` namespace for cache_control regardless of the actual
// upstream provider, so one shape works for every family.
func cacheControlOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: cacheControlEphemeral},
		},
	}
}

// cacheOption configures promptCachingStep. Divergent behaviour between the two
// front-ends is expressed as options rather than forked functions.
type cacheOption struct {
	envPrefix          EnvPrefix
	compactionSummary  bool
	compactionPrefixFn func(fantasy.Message) bool
	stablePrefixTokens int
	stablePrefixOnly   bool
}

// CacheOption mutates the cache step configuration.
type CacheOption func(*cacheOption)

// WithCacheEnvPrefix selects the env-var family for the kill-switch.
func WithCacheEnvPrefix(p EnvPrefix) CacheOption {
	return func(c *cacheOption) { c.envPrefix = p }
}

// WithStablePrefixTokens adds tokens the provider caches with the marked
// prefix but that are not in the messages, the tool definitions, to the prefix
// side of Gemini's comparison (tailOutgrowsMarkedPrefix).
func WithStablePrefixTokens(n int) CacheOption {
	return func(c *cacheOption) { c.stablePrefixTokens = n }
}

// WithStablePrefixOnly restricts explicit cache breakpoints to the reusable
// stable prefix (the system prompt, plus the compaction summary when enabled)
// and suppresses the rolling two-message tail breakpoints even on families
// like Anthropic that otherwise use them. One-shot auxiliary calls
// (summarizer, verifier, reviewer, memory extractors, title/prompt generators)
// send a task-specific user prompt that has no follow-up step to read it back,
// so marking the user message would pay Anthropic's 1.25x cache-write charge
// on unique per-call input without ever reading it.
func WithStablePrefixOnly() CacheOption {
	return func(c *cacheOption) { c.stablePrefixOnly = true }
}

// WithCompactionSummaryBreakpoint enables cutlass's additive 4th breakpoint on
// the compaction-summary message identified by isSummary. When isSummary is nil
// the default isCompactionSummaryMessage matcher (prefix scan) is used.
func WithCompactionSummaryBreakpoint(isSummary func(fantasy.Message) bool) CacheOption {
	return func(c *cacheOption) {
		c.compactionSummary = true
		c.compactionPrefixFn = isSummary
	}
}

// promptCachingStep returns a fantasy PrepareStepFunction that installs cache
// breakpoints on messages just before each step is sent. It is a no-op for
// models that don't support explicit breakpoints, and also a no-op when the
// kill-switch env var is set.
//
// Placement (priority order; Anthropic allows up to 4 breakpoints):
//
//  1. Last system message — stable prefix.
//  2. Compaction summary (only when WithCompactionSummaryBreakpoint is set).
//  3. Last two non-system messages — rolling recency window. Skipped for
//     Gemini (stablePrefixOnly), where only the last breakpoint counts and a
//     moving one re-writes the whole prompt every request.
//
// Gemini drops 1 and 2 as well once the rest of the prompt is estimated larger
// than the prefix they mark (tailOutgrowsMarkedPrefix).
func promptCachingStep(model string, opts ...CacheOption) fantasy.PrepareStepFunction {
	cfg := cacheOption{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.compactionPrefixFn == nil {
		cfg.compactionPrefixFn = isCompactionSummaryMessage
	}
	return func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		if !supportsExplicitBreakpoints(model) || cfg.envPrefix.lookupBool("DISABLE_PROMPT_CACHE") {
			return ctx, fantasy.PrepareStepResult{}, nil
		}

		// Shallow copy so we don't mutate fantasy's internal slice.
		msgs := make([]fantasy.Message, len(opts.Messages))
		copy(msgs, opts.Messages)

		// Defensive nil-sweep: clearing first means our subsequent assignments
		// are authoritative regardless of upstream state.
		for i := range msgs {
			msgs[i].ProviderOptions = nil
		}

		cc := cacheControlOptions()

		lastSystemIdx := -1
		for i, msg := range msgs {
			if msg.Role == fantasy.MessageRoleSystem {
				lastSystemIdx = i
			}
		}
		summaryIdx := -1
		if cfg.compactionSummary {
			for i := len(msgs) - 1; i >= 0; i-- {
				if cfg.compactionPrefixFn(msgs[i]) {
					summaryIdx = i
					break
				}
			}
		}

		// Gemini past its stable prefix: no breakpoint at all, so Google's
		// implicit cache can read back more than the prefix.
		if stablePrefixOnly(model) && tailOutgrowsMarkedPrefix(msgs, max(lastSystemIdx, summaryIdx), cfg.stablePrefixTokens) {
			return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
		}

		// Breakpoint 1: last system message, if any.
		if lastSystemIdx >= 0 {
			msgs[lastSystemIdx].ProviderOptions = cc
		}

		// Breakpoint 2 (additive): compaction summary, if enabled and present.
		if summaryIdx >= 0 {
			msgs[summaryIdx].ProviderOptions = cc
		}

		// Breakpoints 3 & 4: the last two non-system messages, skipping the
		// summary index so we don't waste the rolling budget on a message that
		// already carries a breakpoint, and skipping the request-local budget
		// wind-down notice, whose live spend figures change every step — a
		// breakpoint there can never hit (see isBudgetWindDownNotice).
		rolling := 2
		if cfg.stablePrefixOnly || stablePrefixOnly(model) {
			rolling = 0
		}
		marked := 0
		for i := len(msgs) - 1; i >= 0 && marked < rolling; i-- {
			if msgs[i].Role == fantasy.MessageRoleSystem {
				continue
			}
			if i == summaryIdx || isBudgetWindDownNotice(msgs[i]) {
				continue
			}
			msgs[i] = withAssistantTextCoalesced(msgs[i])
			msgs[i].ProviderOptions = cc
			marked++
		}

		// Spare 4th breakpoint when no compaction summary is present: anchor
		// the most recent unmarked runtime_today message before the rolling
		// tail. During a multi-step tool turn this keeps the turn-start prefix
		// (system + prior history + current user message + runtime_today)
		// refreshed across every tool round; on the NEXT user turn (after
		// replayHistory strips extended-thinking blocks from the tool rounds)
		// it gives Anthropic an explicit breakpoint directly on that turn-start
		// boundary even when the turn made >10 tool calls (>20 content blocks,
		// past Anthropic's 20-block lookback window).
		if rolling > 0 && summaryIdx < 0 {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].ProviderOptions != nil {
					continue
				}
				if isRuntimeDateMessage(msgs[i]) {
					msgs[i].ProviderOptions = cc
					break
				}
			}
		}

		return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
	}
}

// tailOutgrowsMarkedPrefix reports whether the messages after index last (the
// last stable breakpoint) are estimated larger than those up to and including
// it plus extraPrefix, the tokens cached with them that are not in the
// messages (the tool definitions; WithStablePrefixTokens).
//
// For Gemini the stable breakpoint becomes an explicit cache, and a request
// that uses one is read back only up to it: Google's implicit cache adds
// nothing past it. While the marked prefix is most of the prompt (a chat's
// system prompt and a few turns) the explicit cache is the cheaper way: on a
// short conversation it cost about a third of no marker. Once the rest is
// larger (a scheduled run's tool results) the implicit cache, which reads most
// of the earlier prompt back a request or two behind, is: at 130K tokens a
// request cost a quarter as much (google/gemini-3.8-flash, 2026-11-01,
// docs/PROMPT-CACHE-CONTRACT.md). Leaving the explicit cache, the implicit one
// reads nothing until it has warmed (two requests from cold in that probe);
// coming back to it after a compaction costs one fresh cache write.
func tailOutgrowsMarkedPrefix(msgs []fantasy.Message, last, extraPrefix int) bool {
	if last < 0 {
		return false
	}
	return cacheWeightTokens(msgs[last+1:]) > cacheWeightTokens(msgs[:last+1])+extraPrefix
}

// mediaCacheTokens is what tailOutgrowsMarkedPrefix counts for an image or
// other file part, about what Gemini bills for one image (a few hundred to
// about a thousand tokens). estimateModelMessagesTokens counts its base64
// length, an overflow guard's overcount that would make one attached
// screenshot outweigh a whole system prompt and drop the marker a short chat
// needs. A long PDF is counted low instead, which keeps the marker, as before
// this rule.
const mediaCacheTokens = 1000

// cacheWeightTokens is estimateModelMessagesTokens with every file part and
// media tool result counted at mediaCacheTokens.
func cacheWeightTokens(msgs []fantasy.Message) int {
	total := 0
	for _, m := range msgs {
		if !slices.ContainsFunc(m.Content, isMediaPart) {
			total += estimateModelMessagesTokens([]fantasy.Message{m})
			continue
		}
		media := 0
		rest := m
		rest.Content = make([]fantasy.MessagePart, 0, len(m.Content))
		for _, part := range m.Content {
			if isMediaPart(part) {
				media++
				continue
			}
			rest.Content = append(rest.Content, part)
		}
		total += estimateModelMessagesTokens([]fantasy.Message{rest}) + media*mediaCacheTokens
	}
	return total
}

func isMediaPart(part fantasy.MessagePart) bool {
	if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
		return true
	}
	if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
		_, isMedia := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](tr.Output)
		return isMedia
	}
	return false
}

// PromptCachingStep is the exported prompt-caching PrepareStep the DRIVERS
// attach to their follow-up streams (interactive finalize / compaction calls).
// It installs the same per-message cache breakpoints the shared loop uses.
func PromptCachingStep(modelSlug string, opts ...CacheOption) fantasy.PrepareStepFunction {
	return promptCachingStep(modelSlug, opts...)
}

// compactionSummaryPrefix is the exact leading substring every compaction
// summary message starts with. Matches cutlass's buildCompactionSummary.
const compactionSummaryPrefix = "[context compaction"

// isCompactionSummaryMessage reports whether a message is one of the
// placeholder / LLM-summary messages a compaction pass produces. Matched by the
// exact leading substring of the first TextPart — a stable, side-effect-free
// way to tag these without adding a field to fantasy.Message.
func isCompactionSummaryMessage(m fantasy.Message) bool {
	if m.Role != fantasy.MessageRoleUser {
		return false
	}
	for _, part := range m.Content {
		tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
		if !ok {
			continue
		}
		return strings.HasPrefix(tp.Text, compactionSummaryPrefix)
	}
	return false
}

const runtimeDateMessagePrefix = "## Runtime today (authoritative)\n\n- runtime_today: "

// isRuntimeDateMessage reports whether m is a per-turn runtime_today note
// produced by RuntimeDateTurnSuffix / RuntimeDateMessage.
func isRuntimeDateMessage(m fantasy.Message) bool {
	if m.Role != fantasy.MessageRoleUser || len(m.Content) != 1 {
		return false
	}
	tp, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
	return ok && strings.HasPrefix(tp.Text, runtimeDateMessagePrefix)
}

// withAssistantTextCoalesced collapses an assistant message whose Content
// consists solely of multiple TextParts into a single TextPart joined by "\n"
// (the exact concatenation fantasy's OpenRouter hook performs). In fantasy's
// OpenRouter hook (language_model_hooks.go), an assistant message with
// len(Content) > 1 and no tool calls attaches cache_control via
// assistantMsg.Content.SetExtraFields, which openai-go drops when marshaling
// the union wrapper; collapsing to len(Content) == 1 takes the single-part
// branch that preserves cache_control on the message object.
func withAssistantTextCoalesced(m fantasy.Message) fantasy.Message {
	if m.Role != fantasy.MessageRoleAssistant || len(m.Content) <= 1 {
		return m
	}
	var parts []string
	for _, part := range m.Content {
		tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
		if !ok {
			return m
		}
		if tp.Text != "" {
			parts = append(parts, tp.Text)
		}
	}
	if len(parts) == 0 {
		return m
	}
	m.Content = []fantasy.MessagePart{fantasy.TextPart{Text: strings.Join(parts, "\n")}}
	return m
}
