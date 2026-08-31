// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package agent

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// Both egress postures must announce themselves at boot. Before #1264 only
// lockdown did, so the configuration that gives model-authored code
// unrestricted cluster reach was the one that said nothing — an operator
// reading the boot log of an open deployment had no signal at all.
func TestK8sNetworkModeLineSpeaksForBothPostures(t *testing.T) {
	lockdown := k8sNetworkModeLine(sandbox.NetworkModeLockdown)
	for _, want := range []string{"mode=lockdown", "egress=none", "deny-all"} {
		if !strings.Contains(lockdown, want) {
			t.Errorf("lockdown line must mention %q, got: %s", want, lockdown)
		}
	}

	open := k8sNetworkModeLine(sandbox.NetworkModeOpen)
	if open == "" {
		t.Fatal("open mode must not boot silently")
	}
	// The reader needs three things: that this is the risky posture, what is
	// reachable, and the one knob that closes it.
	for _, want := range []string{"WARNING", "mode=open", "egress=open", "UNRESTRICTED", "apiserver", "networkPolicies.openEgress.create"} {
		if !strings.Contains(open, want) {
			t.Errorf("open line must mention %q, got: %s", want, open)
		}
	}

	// An empty mode is the historical spelling of "open" (container.go), and
	// it must not fall through to the reassuring line.
	if got := k8sNetworkModeLine(""); !strings.Contains(got, "mode=open") {
		t.Errorf("an unset mode is open and must warn like it, got: %s", got)
	}

	// Neither line may claim the cluster is enforcing anything — fleet checks
	// that the lockdown policy OBJECT exists and cannot check the open one at
	// all.
	for _, line := range []string{lockdown, open} {
		for _, forbidden := range []string{"is enforced", "guaranteed", "cannot reach"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("boot line must not claim enforcement (%q), got: %s", forbidden, line)
			}
		}
	}
}
