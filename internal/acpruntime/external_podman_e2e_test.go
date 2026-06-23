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

	t.Run("usage decode", func(t *testing.T) {
		// Drive the example agent with FLEET_ACP_EXAMPLE_EMIT_USAGE so it emits a
		// deterministic acp.Usage (on PromptResponse) + a USD SessionUsageUpdate.Cost.
		// This validates fleet's UNSTABLE wire-decode against a REAL provider payload
		// over podman-stdio — not just the in-process struct fakes (#96). The flag
		// rides ProviderEnv → the container --env; the run stays credential-free.
		obs := &recordingObserver{}
		rt := NewExternalRuntime(ExternalConfig{
			Image:        image,
			StartTimeout: 60 * time.Second,
			ProviderEnv:  map[string]string{"FLEET_ACP_EXAMPLE_EMIT_USAGE": "1"},
		})
		broker := brokerFunc(func(_ context.Context, _ PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Allowed: true, OptionID: "allow"}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := rt.Run(ctx, "please update the config", ExternalDeps{Observer: obs, PermissionBroker: broker})
		if err != nil {
			t.Fatalf("external run: %v", err)
		}
		if res.Usage.PromptTokens != 100 || res.Usage.CompletionTokens != 20 ||
			res.Usage.CachedTokens != 5 || res.Usage.CacheCreationTokens != 3 {
			t.Fatalf("token wire-decode mismatch: %+v (want prompt=100 completion=20 cached=5 cacheCreation=3)", res.Usage)
		}
		if res.Usage.CostUSD != 0.42 {
			t.Fatalf("USD cost wire-decode = %v, want 0.42", res.Usage.CostUSD)
		}
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
