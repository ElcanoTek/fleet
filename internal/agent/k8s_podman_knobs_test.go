// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// The kubernetes backend refuses podman-only knobs instead of ignoring them:
// a knob that reads as containment while imposing none is the failure mode
// ADR-0010's no-degrade rule exists for. Each case below returns before any
// cluster contact, so this needs no apiserver.
//
// FLEET_SANDBOX_PIDS was the one that slipped through (#1264): it was accepted
// silently, `validate-config` reported the backend healthy, and the sandbox
// ran with an unlimited process count.
func TestBuildKubernetesSandboxPoolRefusesPodmanOnlyKnobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime string
		seccomp string
		cfg     *config.Config
		pool    sandbox.PoolConfig
		want    string // substring the error must name, so the operator can act on it
	}{
		{
			name: "pids limit",
			cfg:  &config.Config{},
			pool: sandbox.PoolConfig{Container: sandbox.ContainerConfig{PidsLimit: 64}},
			want: "FLEET_SANDBOX_PIDS=64",
		},
		{
			name: "pids limit names the replacement",
			cfg:  &config.Config{},
			pool: sandbox.PoolConfig{Container: sandbox.ContainerConfig{PidsLimit: 64}},
			want: "podPidsLimit",
		},
		{
			name:    "oci runtime",
			runtime: "kata",
			cfg:     &config.Config{},
			want:    "FLEET_SANDBOX_K8S_RUNTIME_CLASS",
		},
		{
			name:    "seccomp profile",
			seccomp: "/etc/fleet/seccomp.json",
			cfg:     &config.Config{},
			want:    "FLEET_SANDBOX_K8S_SECCOMP_PROFILE",
		},
		{
			name: "allowlisted egress",
			cfg:  &config.Config{DefaultNetworkMode: sandbox.NetworkModeAllowlisted},
			want: "allowlisted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.seccomp != "" {
				t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", tc.seccomp)
			} else {
				// The refusal reads the environment directly, so a value left
				// over from the ambient shell would fail the wrong case.
				if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
					t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
				}
			}
			_, err := buildKubernetesSandboxPool(tc.cfg, tc.pool, tc.runtime, nil)
			if err == nil {
				t.Fatalf("expected a fail-closed refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must name %q so the operator knows what to do; got: %v", tc.want, err)
			}
		})
	}
}

// A pids limit of zero is "unset" (the pool default), not a configured knob —
// refusing it would break every deployment that never set it.
func TestBuildKubernetesSandboxPoolAcceptsUnsetPidsLimit(t *testing.T) {
	if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
		t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
	}
	_, err := buildKubernetesSandboxPool(&config.Config{}, sandbox.PoolConfig{}, "", nil)
	if err != nil && strings.Contains(err.Error(), "FLEET_SANDBOX_PIDS") {
		t.Errorf("an unset pids limit must not be refused; got: %v", err)
	}
}
