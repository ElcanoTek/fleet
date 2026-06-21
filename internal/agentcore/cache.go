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
}

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
	return false
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
}

// CacheOption mutates the cache step configuration.
type CacheOption func(*cacheOption)

// WithCacheEnvPrefix selects the env-var family for the kill-switch.
func WithCacheEnvPrefix(p EnvPrefix) CacheOption {
	return func(c *cacheOption) { c.envPrefix = p }
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
//  3. Last two non-system messages — rolling recency window.
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

		// Breakpoint 1: last system message, if any.
		lastSystemIdx := -1
		for i, msg := range msgs {
			if msg.Role == fantasy.MessageRoleSystem {
				lastSystemIdx = i
			}
		}
		if lastSystemIdx >= 0 {
			msgs[lastSystemIdx].ProviderOptions = cc
		}

		// Breakpoint 2 (additive): compaction summary, if enabled and present.
		summaryIdx := -1
		if cfg.compactionSummary {
			for i := len(msgs) - 1; i >= 0; i-- {
				if cfg.compactionPrefixFn(msgs[i]) {
					summaryIdx = i
					break
				}
			}
			if summaryIdx >= 0 {
				msgs[summaryIdx].ProviderOptions = cc
			}
		}

		// Breakpoints 3 & 4: the last two non-system messages, skipping the
		// summary index so we don't waste the rolling budget on a message that
		// already carries a breakpoint.
		marked := 0
		for i := len(msgs) - 1; i >= 0 && marked < 2; i-- {
			if msgs[i].Role == fantasy.MessageRoleSystem {
				continue
			}
			if i == summaryIdx {
				continue
			}
			msgs[i].ProviderOptions = cc
			marked++
		}

		return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
	}
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
