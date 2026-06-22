package agent

import (
	"context"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// SCHEDULED-EXTERNAL (Plan v4, P-ACP-4): run an EXTERNAL ACP agent (Claude Code /
// Goose, via acpruntime.ExternalRuntime) as a SCHEDULED task — FAIL-CLOSED.
//
// This is the deliberate INVERSE of the native-acp fallback in scheduled.go. The
// native-acp path may SILENTLY fall back to the fully-governed in-process loop
// when it cannot faithfully govern (acpScheduledFallback): falling back there is
// SAFE because it runs the SAME native agent under STRONGER governance. An
// external scheduled task must NEVER fall back to a native flavor — that would run
// a DIFFERENT agent than the operator selected, and an under-governed external
// agent must not run at all unless the operator explicitly opted in. So the gate
// here errors LOUDLY instead of degrading:
//
//	┌── scheduled-external gate (fail-closed) ─────────────────────────────────┐
//	│ flag OFF (allow_ungoverned_scheduled_agents=false, the default)          │
//	│   → LOUD ERROR at dispatch, recorded in the run/session log.             │
//	│     NEVER a silent fallback to native. The run is FAILED, not degraded.  │
//	│                                                                          │
//	│ flag ON                                                                  │
//	│   → run via the EXISTING acpruntime.ExternalRuntime at the CONTAINMENT   │
//	│     tier: governance: delegated stamped; SANDBOX REQUIRED (no opt-out);  │
//	│     permissions DEFAULT-DENY (NO PermissionBroker is wired — there is no │
//	│     human on the scheduled loop, so external.go's fail-closed deny       │
//	│     answers every session/request_permission).                          │
//	└──────────────────────────────────────────────────────────────────────────┘
//
// Honesty: this is the SAME containment tier as interactive-external (the agent
// self-executes in a locked sandbox; fleet observes the self-report but does not
// enforce per-tool policy; the agent may transmit the workspace to its own model
// endpoint). It is never conflated with native's full governance — the run record
// stamps governance: delegated. The ONE difference from interactive-external is
// the permission posture: interactive routes session/request_permission to a
// human, scheduled has no approver and therefore default-DENIES every request.

// externalRuntime is the seam the scheduled-external path drives. It is satisfied
// by acpruntime.ExternalRuntime in production and by an in-process fake in tests
// (so the path is exercised with NO podman and NO live key). Run mirrors
// acpruntime.ExternalRuntime.Run exactly.
type externalRuntime interface {
	Run(ctx context.Context, promptText string, deps acpruntime.ExternalDeps) (acpruntime.Result, error)
}

// isExternalFlavor reports whether the scheduled task's resolved runtime flavor is
// an EXTERNAL (self-executing) one — type: acp OR delegated_policy: true. Either
// marker routes through the containment-tier scheduled-external path (and the
// fail-closed gate), never the native flavors. The native flavors leave both
// markers unset.
func (a *Agent) isExternalFlavor() bool {
	return a.runtimeFlavor.Type == clientconfig.RuntimeTypeACP || a.runtimeFlavor.DelegatedPolicy
}

// runScheduledExternal enforces the fail-closed gate, then (when admitted) drives
// one scheduled task through the EXISTING acpruntime.ExternalRuntime at the
// containment tier. It does NOT fork ExternalRuntime — it wires it exactly as the
// interactive-external path does, minus the human PermissionBroker (scheduled has
// no approver), so external.go's existing nil-broker fail-closed behavior denies
// every permission request.
//
// The session log already records the task prompt + (on any error) the fatal
// reason via Execute's deferred handlers; this records the governance reason on
// the fail-closed path as a system error so the run record honestly shows WHY it
// failed.
func (a *Agent) runScheduledExternal(ctx context.Context, task string) error {
	flavor := a.runtimeFlavor

	// FAIL-CLOSED gate. With the per-client opt-in OFF, a scheduled task that
	// selected an external flavor is a LOUD ERROR — recorded, not degraded, and
	// NEVER a silent fallback to a native flavor.
	if !a.allowUngovernedScheduled {
		return fmt.Errorf(
			"scheduled task selected external runtime %q (governance: delegated, containment tier) "+
				"but this client has not opted in: set agent_policy.allow_ungoverned_scheduled_agents=true "+
				"in the manifest to permit ungoverned external agents on the scheduler. "+
				"Refusing to run (fail-closed; no fallback to a native flavor)",
			flavor.Name)
	}

	// SANDBOX REQUIRED — no opt-out for scheduled-external. A scheduled-external
	// attempt without a sandbox image is an ERROR, not a degraded run. The external
	// agent self-executes inside the provider's container image (flavor.Image is
	// that sandbox); without it there is nothing to contain the agent in.
	if flavor.Image == "" {
		return fmt.Errorf(
			"scheduled-external runtime %q requires a sandbox image (containment is mandatory; "+
				"set runtimes.%s.image in the manifest)",
			flavor.Name, flavor.Name)
	}

	// Stamp the dispatch decision into the run log BEFORE spawning so the audit
	// trail records the admitted-by-opt-in posture even if the spawn errors early.
	// ExternalRuntime.Run additionally stamps the governance: delegated tier (the
	// EventGovernance event reused unchanged from interactive-external).
	a.logSession.AddMessageWithMetadata(roleUser,
		fmt.Sprintf("[governance] scheduled-external admitted: runtime=%q tier=%s sandbox=%q permissions=default-deny (no human on the scheduled loop)",
			flavor.Name, acpruntime.GovernanceDelegated, flavor.Image),
		nil, nil, ptr("system_governance"), nil, nil, "")

	rt := a.buildExternalRuntime(acpruntime.ExternalConfig{
		Image: flavor.Image,
		Args:  flavor.Args,
		// SCRUBBED env: ONLY the provider's own model-endpoint credential(s), named
		// by the manifest's model_env. fleet secrets / MCP creds are NEVER shipped
		// to an external agent (the containment-tier invariant, identical to
		// interactive-external).
		ProviderEnv: providerEnv(flavor.ModelEnv),
	})

	res, err := rt.Run(ctx, task, acpruntime.ExternalDeps{
		Observer: &scheduledObserver{session: a.logSession},
		// NO PermissionBroker (nil): scheduled has no human approver. external.go's
		// RequestPermission fail-closes on a nil broker — DENYING every
		// session/request_permission (no approve-all, no hang). A scheduled-external
		// task that needs approval simply cannot take that action, by design.
		PermissionBroker: nil,
	})
	// Record the agent's SELF-REPORTED usage on EVERY exit path (mirroring the
	// native runScheduledACP). This is a containment-tier run — the external agent
	// drives its own model endpoint, so fleet does not meter it; the recorded
	// tokens/cost are the agent's self-report. An unreported cost stays zero, which
	// is documented as unmetered (NOT a true $0) — see issue #31.
	a.recordACPUsage(res.Usage)
	if res.FinalText != "" {
		a.logSession.AddMessage(roleAssistant, res.FinalText, nil, nil)
	}
	if err != nil {
		return err
	}
	return nil
}

// buildExternalRuntime returns the runtime that drives the scheduled-external
// turn: the injected fake in tests, else the real acpruntime.ExternalRuntime.
func (a *Agent) buildExternalRuntime(cfg acpruntime.ExternalConfig) externalRuntime {
	if a.newExternalRuntime != nil {
		return a.newExternalRuntime(cfg)
	}
	return acpruntime.NewExternalRuntime(cfg)
}

// ptr returns a pointer to v. Used for the optional *string metadata fields on
// the session log helper.
func ptr[T any](v T) *T { return &v }
