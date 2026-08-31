// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Boot-time preflight for the kubernetes sandbox backend (#989 / ADR-0049).
//
// When FLEET_SANDBOX_BACKEND=kubernetes is selected, a cluster that cannot
// actually run sandbox pods must abort boot — never silently fall back to
// podman or host execution (the same no-degrade posture as PreflightRuntime
// and PreflightAllowlistedNetwork). The checks run in failure-likelihood
// order so the first error an operator sees is the most actionable one:
//
//  1. apiserver reachable + credentials valid (GET /version)
//  2. RBAC: create/get/list/delete pods and create pods/exec in the sandbox
//     namespace (SelfSubjectAccessReview — precise "which verb is missing")
//  3. the shared workspace PVC exists
//  4. the sealed-egress NetworkPolicy object exists (the chart ships it;
//     the OBJECT check cannot prove the CNI enforces it, and the docs say so)
//  5. the RuntimeClass exists, when one is configured
//
// Deliberately NOT checked: image pullability (imagePullSecrets are resolved
// by the kubelet per node — the only faithful probe is running a pod, which
// the first turn does, failing fast on ErrImagePull in waitForRunning) and
// PVC access mode (RWX vs RWO is advisory in the API; a wrong mode surfaces
// as a scheduling error the docs' checklist covers).

import (
	"context"
	"fmt"
	"log"
	"time"
)

// k8sPreflightTimeout bounds the whole preflight sequence. Generous for a
// cold connection; short enough that a dead apiserver fails boot promptly.
const k8sPreflightTimeout = 30 * time.Second

// k8sRBACChecks are the (verb, resource, subresource) grants the backend
// needs. Listed as data so the error message names exactly what is missing.
var k8sRBACChecks = []struct {
	verb, resource, subresource string
}{
	{"create", "pods", ""},
	{"get", "pods", ""},
	{"list", "pods", ""},
	{"delete", "pods", ""},
	{"create", "pods", "exec"},
	// The exec stream is a WebSocket upgrade, which gorilla/websocket issues as
	// an HTTP GET (client.go: Method: http.MethodGet). The apiserver derives the
	// RBAC verb from the method, so `get` — not `create` — is what every
	// bash/run_python/fileop call is actually authorized against. Checking only
	// `create` let a Role that grants just that pass preflight and then 403 on
	// the first tool call, with boot having reported the cluster fine.
	{"get", "pods", "exec"},
}

// Preflight verifies, fail-closed, that the cluster can deliver what the
// kubernetes sandbox backend promises. Called from the single production
// pool-construction path (agent.buildSandboxPool) and from
// `fleet validate-config` — mirroring PreflightRuntime's contract. Callers
// must treat any error as fatal to boot; there is no degraded mode.
func (b *KubernetesBackend) Preflight(ctx context.Context) error {
	if b.cfg.WorkspaceClaim == "" {
		return fmt.Errorf("kubernetes sandbox preflight: no workspace PVC configured — set FLEET_SANDBOX_K8S_WORKSPACE_CLAIM (or the bundle manifest's sandbox.kubernetes.workspace_claim) to the ReadWriteMany claim shared with the control plane")
	}
	preCtx, cancel := context.WithTimeout(ctx, k8sPreflightTimeout)
	defer cancel()

	version, err := b.client.serverVersion(preCtx)
	if err != nil {
		return fmt.Errorf("kubernetes sandbox preflight: apiserver unreachable or credentials rejected: %w", err)
	}

	for _, check := range k8sRBACChecks {
		allowed, err := b.client.selfSubjectAccessReview(preCtx, b.cfg.Namespace, check.verb, check.resource, check.subresource)
		if err != nil {
			return fmt.Errorf("kubernetes sandbox preflight: access review for %s %s/%s failed: %w", check.verb, check.resource, check.subresource, err)
		}
		if !allowed {
			target := check.resource
			if check.subresource != "" {
				target += "/" + check.subresource
			}
			return fmt.Errorf("kubernetes sandbox preflight: the fleet service account may not %s %s in namespace %q — grant the fleet-runner Role from the Helm chart (deploy/helm/fleet) or an equivalent RoleBinding", check.verb, target, b.cfg.Namespace)
		}
	}

	if err := b.client.getPVC(preCtx, b.cfg.Namespace, b.cfg.WorkspaceClaim); err != nil {
		return fmt.Errorf("kubernetes sandbox preflight: workspace PVC %q not readable in namespace %q (it must be a ReadWriteMany claim mounted by the control plane at the same path): %w", b.cfg.WorkspaceClaim, b.cfg.Namespace, err)
	}

	if err := b.client.getNetworkPolicy(preCtx, b.cfg.Namespace, b.cfg.NetworkPolicyName); err != nil {
		return fmt.Errorf("kubernetes sandbox preflight: sealed-egress NetworkPolicy %q not found in namespace %q — the deny-all policy for pods labeled %s=none must exist before sealed sandboxes can be trusted (the Helm chart ships it): %w",
			b.cfg.NetworkPolicyName, b.cfg.Namespace, k8sLabelEgress, err)
	}

	// Open mode with nothing selecting the pods is the one posture where a
	// clean preflight would certify an unrestricted sandbox. Require the
	// companion policy the same way the sealed one is required, so "the docs
	// say this policy is required" and "fleet requires this policy" stop being
	// different statements.
	if b.cfg.DefaultNetworkMode != NetworkModeLockdown {
		switch {
		case b.cfg.UnrestrictedEgressAcknowledged:
			// Say it every boot. An acknowledgement that scrolls past once at
			// install time is not an informed posture six months later.
			log.Printf("sandbox: WARNING open sandbox egress is UNVERIFIED by fleet — %s is set, so boot did not require the %q NetworkPolicy. Sandbox pods labeled %s=open can reach whatever your cluster's own policy allows; if that is nothing, this is fine, and if you are not sure, it is not",
				k8sUnrestrictedEgressAckEnv, b.cfg.OpenEgressPolicyName, k8sLabelEgress)
		default:
			if err := b.client.getNetworkPolicy(preCtx, b.cfg.Namespace, b.cfg.OpenEgressPolicyName); err != nil {
				return fmt.Errorf("kubernetes sandbox preflight: open-egress NetworkPolicy %q not found in namespace %q — with FLEET_DEFAULT_NETWORK_MODE=open, a sandbox pod labeled %s=open that no policy selects can reach the fleet Service, the in-cluster database, the apiserver and the node metadata endpoint from model-authored code. Enable the policy the chart ships (networkPolicies.openEgress.create=true, with your cluster ranges in blockedCIDRs), or use FLEET_DEFAULT_NETWORK_MODE=lockdown, or — only if your cluster shapes this egress with policy fleet cannot see — set %s=true to state that deliberately: %w",
					b.cfg.OpenEgressPolicyName, b.cfg.Namespace, k8sLabelEgress, k8sUnrestrictedEgressAckEnv, err)
			}
		}
	}

	if b.cfg.RuntimeClassName != "" {
		if err := b.client.getRuntimeClass(preCtx, b.cfg.RuntimeClassName); err != nil {
			return fmt.Errorf("kubernetes sandbox preflight: RuntimeClass %q not found — a hypervisor runtime that cannot be verified must abort boot, never degrade to the default runtime (ADR-0010 posture): %w", b.cfg.RuntimeClassName, err)
		}
	}

	log.Printf("sandbox: kubernetes backend preflight OK — apiserver %s, sandbox namespace %q", version, b.cfg.Namespace)
	return nil
}
