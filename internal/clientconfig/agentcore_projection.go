package clientconfig

import (
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The bundle's runtime policy is installed into agentcore by BOTH entrypoints
// that run turns: `fleet serve` (cmd/fleet) and the local `fleet task run`
// harness (internal/taskrun). The projections live here, on the bundle, so the
// two cannot drift — a new AgentPolicy field is mapped once and reaches both
// (#1934). TestAgentcorePolicyMapsEveryField fails if a field is not mapped.

// AgentcorePolicy projects the bundle's agent tool-behavior policy into the
// form agentcore.ConfigureAgentPolicy installs. Fields that only shape the
// interactive approval UX are carried too; a scheduled run ignores them.
func (b *Bundle) AgentcorePolicy() agentcore.AgentPolicy {
	p := b.AgentPolicy()
	return agentcore.AgentPolicy{
		ParallelSafeTools:             p.ParallelSafeTools,
		CriticalToolSuffixes:          p.CriticalToolSuffixes,
		CriticalToolSubstitutes:       p.CriticalToolSubstitutes,
		CriticalToolTimeouts:          p.CriticalToolTimeouts,
		CriticalToolModes:             p.CriticalToolModes,
		CriticalToolUndoHints:         p.CriticalToolUndoHints,
		CriticalToolAliases:           p.CriticalToolAliases,
		EmailLastToolSuffixes:         p.EmailLastTools,
		SettleableCreateToolSuffixes:  p.SettleableCreateTools,
		BatchSecondsPerDeal:           p.BatchSecondsPerDeal,
		ApprovedCallTimeoutSeconds:    p.ApprovedCallTimeoutSeconds,
		CriticalToolNoSessionApproval: p.CriticalToolNoSessionApproval,
		CriticalToolCardDescribers:    p.CriticalToolCardDescribers,
		CriticalToolResume:            p.CriticalToolResume,
		CriticalToolResumeMaxPerHour:  p.CriticalToolResumeMaxPerHour,
		CriticalToolGroupApproval:     p.CriticalToolGroupApproval,
		CriticalToolProgress:          p.CriticalToolProgress,
		CriticalToolResultCards:       p.CriticalToolResultCards,
		CriticalToolNarrowableArgs:    p.NarrowableArgNames(),
		CriticalToolStop:              p.CriticalToolStop,
		InteractiveHiddenTools:        p.InteractiveHiddenTools,
		UnattendedDisabledTools:       p.UnattendedDisabledTools,
	}
}

// AgentcorePricing projects the bundle's pricing config (#297) into the form
// agentcore.ConfigurePricing installs. A blank fallback is left blank so
// agentcore applies its OpenRouter default; the manifest loader already
// validated the fallback value and the override rates.
func (b *Bundle) AgentcorePricing() agentcore.PricingConfig {
	p := b.Pricing()
	out := agentcore.PricingConfig{Fallback: agentcore.PricingFallback(p.Fallback)}
	if len(p.Overrides) > 0 {
		out.Overrides = make([]agentcore.PricingOverride, 0, len(p.Overrides))
		for _, o := range p.Overrides {
			out.Overrides = append(out.Overrides, agentcore.PricingOverride{
				Model:                          o.Model,
				InputCostPerMillionTokens:      o.InputCostPerMillionTokens,
				OutputCostPerMillionTokens:     o.OutputCostPerMillionTokens,
				CacheReadCostPerMillionTokens:  o.CacheReadCostPerMillionTokens,
				CacheWriteCostPerMillionTokens: o.CacheWriteCostPerMillionTokens,
			})
		}
	}
	return out
}

// AgentcoreHooks projects the bundle's declared lifecycle hooks (#788) into the
// form agentcore.ConfigureLifecycleHooks installs. nil when the bundle declares
// none (the generic default), which agentcore treats as "no hooks".
func (b *Bundle) AgentcoreHooks() []agentcore.LifecycleHook {
	hooks := b.Hooks()
	if len(hooks) == 0 {
		return nil
	}
	out := make([]agentcore.LifecycleHook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, agentcore.LifecycleHook{
			ID:      h.ID,
			Event:   agentcore.HookEvent(h.Event),
			Matcher: h.Matcher,
			Command: h.Command,
			Timeout: time.Duration(h.EffectiveTimeoutSeconds()) * time.Second,
			Enforce: h.Enforce,
		})
	}
	return out
}
