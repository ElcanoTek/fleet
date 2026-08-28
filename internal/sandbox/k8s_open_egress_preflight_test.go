// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// Under `open`, a sandbox pod that no NetworkPolicy selects has the run of the
// pod network — measured on a stock k3s install, one reached the fleet Service,
// the in-cluster Postgres, the apiserver and the internet (#1264). The docs
// already called the open-egress policy required; the preflight now agrees,
// instead of certifying a cluster where nothing constrains an open sandbox.
func TestPreflightRequiresOpenEgressPolicyInOpenMode(t *testing.T) {
	t.Run("missing policy aborts boot", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.mu.Lock()
		fake.absentNetpols[defaultK8sOpenEgressPolicy] = true
		fake.mu.Unlock()
		backend := fake.backend(t, KubernetesConfig{
			Namespace:          "fleet-sandboxes",
			DefaultNetworkMode: NetworkModeOpen,
		})
		err := backend.Preflight(context.Background())
		if err == nil {
			t.Fatal("open mode with no open-egress policy must fail closed")
		}
		// The message has to carry every exit an operator might take, or the
		// refusal just moves the problem to a support thread.
		for _, want := range []string{
			defaultK8sOpenEgressPolicy,
			"networkPolicies.openEgress.create",
			"lockdown",
			k8sUnrestrictedEgressAckEnv,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal must mention %q, got: %v", want, err)
			}
		}
	})

	t.Run("policy present boots", func(t *testing.T) {
		fake := newFakeKube(t)
		backend := fake.backend(t, KubernetesConfig{
			Namespace:          "fleet-sandboxes",
			DefaultNetworkMode: NetworkModeOpen,
		})
		if err := backend.Preflight(context.Background()); err != nil {
			t.Fatalf("open mode with the policy in place must boot: %v", err)
		}
	})

	t.Run("acknowledgement waives it", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.mu.Lock()
		fake.absentNetpols[defaultK8sOpenEgressPolicy] = true
		fake.mu.Unlock()
		backend := fake.backend(t, KubernetesConfig{
			Namespace:                      "fleet-sandboxes",
			DefaultNetworkMode:             NetworkModeOpen,
			UnrestrictedEgressAcknowledged: true,
		})
		if err := backend.Preflight(context.Background()); err != nil {
			t.Fatalf("an explicit acknowledgement must be honoured: %v", err)
		}
	})

	// The historical spelling of open is the empty string, and it must not be
	// the way past a security requirement.
	t.Run("unset mode counts as open", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.mu.Lock()
		fake.absentNetpols[defaultK8sOpenEgressPolicy] = true
		fake.mu.Unlock()
		backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
		if err := backend.Preflight(context.Background()); err == nil {
			t.Fatal("an unset network mode is open and must be held to the same requirement")
		}
	})

	// Lockdown pods are sealed by the deny-all policy, so the open one is not
	// its business — requiring it there would fail boots for no security gain.
	t.Run("lockdown does not require it", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.mu.Lock()
		fake.absentNetpols[defaultK8sOpenEgressPolicy] = true
		fake.mu.Unlock()
		backend := fake.backend(t, KubernetesConfig{
			Namespace:          "fleet-sandboxes",
			DefaultNetworkMode: NetworkModeLockdown,
		})
		if err := backend.Preflight(context.Background()); err != nil {
			t.Fatalf("lockdown must not require the open-egress policy: %v", err)
		}
	})

	// The sealed policy stays required in EVERY mode: a scheduled run with
	// AllowNetwork off gets a sealed sandbox even on an open deployment.
	t.Run("deny-all still required in open mode", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.mu.Lock()
		fake.absentNetpols[defaultK8sNetworkPolicy] = true
		fake.mu.Unlock()
		backend := fake.backend(t, KubernetesConfig{
			Namespace:          "fleet-sandboxes",
			DefaultNetworkMode: NetworkModeOpen,
		})
		err := backend.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), defaultK8sNetworkPolicy) {
			t.Fatalf("the sealed-egress policy must stay required in open mode, got: %v", err)
		}
	})
}
