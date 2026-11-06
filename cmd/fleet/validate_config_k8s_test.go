package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// validate-config must surface a knob that boot will refuse, so an operator
// sees it BEFORE the upgrade that starts refusing it rather than after the
// control plane fails to come back. FLEET_SANDBOX_PIDS was the gap (#1264):
// boot ignored it and validate-config reported the backend healthy, so a
// configured process ceiling that imposed nothing looked fine from both sides.
func TestValidateConfigFlagsPidsKnobUnderKubernetes(t *testing.T) {
	if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
		t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
	}
	res := checkKubernetesSandbox(
		context.Background(),
		checkResult{Name: "sandbox", Blocking: true},
		&config.Config{SandboxPids: 64},
	)
	if res.Status != statusFail {
		t.Fatalf("a pids ceiling the backend cannot impose must fail the check, got %v (%s)", res.Status, res.Detail)
	}
	for _, want := range []string{"FLEET_SANDBOX_PIDS=64", "podPidsLimit"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail must name %q so the operator can act on it; got: %s", want, res.Detail)
		}
	}
}

// The same check must stay quiet when the knob is unset — zero means "pool
// default", not "configured".
func TestValidateConfigAcceptsUnsetPidsKnobUnderKubernetes(t *testing.T) {
	if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
		t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
	}
	res := checkKubernetesSandbox(
		context.Background(),
		checkResult{Name: "sandbox", Blocking: true},
		&config.Config{},
	)
	if res.Status == statusFail && strings.Contains(res.Detail, "FLEET_SANDBOX_PIDS") {
		t.Errorf("an unset pids knob must not fail the check; got: %s", res.Detail)
	}
}
