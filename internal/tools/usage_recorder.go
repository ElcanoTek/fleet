package tools

import (
	"context"

	"charm.land/fantasy"
)

// Auxiliary model-call usage recorder (#1118).
//
// A few native tools make their OWN model calls on behalf of the run that
// invoked them (the #191 suggest_branch_name / suggest_commit_message /
// suggest_pr_description git-metadata tools). Those calls are model-invocable —
// the model chooses how often to make them — so their tokens/cost must count
// against the run's ceilings, not vanish. The governed run loop
// (agentcore.Run) installs this recorder on the run context as a capability
// closure over the run's usage accounting; a tool that generates meters each
// call through it. The seam lives HERE — colocated with the tools, exactly
// like the ask/notify handlers — because the tools package must stay free of
// an agentcore import (agentcore imports tools).
//
// nil recorder (a tool exercised outside a governed run: unit tests, direct
// invocation) means "nowhere to meter": the tool still works, it just has no
// run to charge.

// UsageRecorder meters one auxiliary model call's usage into the surrounding
// run's accounting. modelSlug is the model that produced the call (it selects
// a per-model price override, #297); metadata carries the provider-returned
// cost.
type UsageRecorder func(modelSlug string, usage fantasy.Usage, metadata fantasy.ProviderMetadata)

type usageRecorderKey struct{}

// WithUsageRecorder installs the run's usage recorder on ctx (nil leaves it
// untouched). Installed by agentcore.Run for every governed run.
func WithUsageRecorder(ctx context.Context, r UsageRecorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, usageRecorderKey{}, r)
}

// UsageRecorderFromContext returns the installed recorder, or nil when the
// call is not inside a governed run.
func UsageRecorderFromContext(ctx context.Context) UsageRecorder {
	if r, ok := ctx.Value(usageRecorderKey{}).(UsageRecorder); ok {
		return r
	}
	return nil
}
