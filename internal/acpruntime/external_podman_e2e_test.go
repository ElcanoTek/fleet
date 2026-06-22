package acpruntime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExternalPodmanE2E exercises the REAL external path: fleet's ExternalRuntime
// spawns the credential-free ACP example agent (cmd/acp-example-agent) via
// `podman run -i`, drives Initialize → NewSession → Prompt over real ACP stdio,
// captures the agent's self-reported session/update stream onto the Observer
// (containment-tier audit), and handles its session/request_permission through a
// broker. This is the live end-to-end proof the plan requires (a turn streams +
// a permission request is handled), against a real, credential-free ACP agent —
// the same shape Claude Code / Goose take.
//
// Gated on FLEET_ACP_EXTERNAL_E2E_IMAGE (the example-agent image tag) so the
// standard CI suite — which may lack podman or the image — skips it; the
// deterministic coverage is TestExternalAllowRoundTrip et al.
func TestExternalPodmanE2E(t *testing.T) {
	image := os.Getenv("FLEET_ACP_EXTERNAL_E2E_IMAGE")
	if image == "" {
		t.Skip("set FLEET_ACP_EXTERNAL_E2E_IMAGE to the acp-example-agent image tag to run the external podman e2e")
	}

	t.Run("allow", func(t *testing.T) {
		obs := &recordingObserver{}
		rt := NewExternalRuntime(ExternalConfig{Image: image, StartTimeout: 60 * time.Second})

		broker := brokerFunc(func(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
			// A real human would review req.Title/Locations here; the test allows.
			if req.Title == "" {
				t.Errorf("permission request had no title")
			}
			return PermissionDecision{Allowed: true, OptionID: "allow"}, nil
		})

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		res, err := rt.Run(ctx, "please update the config", ExternalDeps{
			Observer: obs, PermissionBroker: broker,
		})
		if err != nil {
			t.Fatalf("external run: %v", err)
		}
		if !strings.Contains(res.FinalText, "applied") {
			t.Fatalf("final text = %q, want it to reflect the allow (applied)", res.FinalText)
		}
		// The containment tier was stamped into the audit/session log.
		assertGovernanceDelegated(t, obs)
		assertPermissionResolved(t, obs, true)
	})

	t.Run("default-deny", func(t *testing.T) {
		obs := &recordingObserver{}
		rt := NewExternalRuntime(ExternalConfig{Image: image, StartTimeout: 60 * time.Second})

		// Broker never answers; the per-request timeout must DEFAULT-DENY.
		broker := brokerFunc(func(ctx context.Context, _ PermissionRequest) (PermissionDecision, error) {
			<-ctx.Done()
			return PermissionDecision{Allowed: false}, ctx.Err()
		})

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		res, err := rt.Run(ctx, "please update the config", ExternalDeps{
			Observer: obs, PermissionBroker: broker, PermissionTimeout: 500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("external run: %v", err)
		}
		if !strings.Contains(res.FinalText, "skip") {
			t.Fatalf("final text = %q, want it to reflect the default-deny (skip)", res.FinalText)
		}
		assertPermissionResolved(t, obs, false)
	})
}

func assertGovernanceDelegated(t *testing.T, obs *recordingObserver) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, e := range obs.raw {
		if e.eventType != EventGovernance {
			continue
		}
		if tier, _ := e.payload["tier"].(string); tier != string(GovernanceDelegated) {
			t.Fatalf("governance tier = %q, want %q", tier, GovernanceDelegated)
		}
		return
	}
	t.Fatalf("no %q event observed; events=%v", EventGovernance, obs.events)
}
