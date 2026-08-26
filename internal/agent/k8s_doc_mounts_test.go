package agent

import (
	"path/filepath"
	"testing"
)

// The kubernetes backend keeps or drops each supporting-doc root's fileop
// anchor by one rule set (k8sDocMounts). These tests pin it, because the
// consequence of getting it wrong is invisible until an agent tries to read a
// protocol: too permissive and the anchor trusts a path no pod has, too strict
// and view_file refuses a file the sandbox image really does carry.
func TestK8sDocMountsDropsEverythingWithoutTheDeclaration(t *testing.T) {
	bundle := []string{"/opt/fleet/client/personas", "/opt/fleet/client/protocols", "/opt/fleet/client/system_prompts", "/opt/fleet/client/skills"}
	mounts := append(append([]string{}, bundle...), "/var/lib/fleet/uploads")

	kept, dropped := k8sDocMounts(mounts, bundle, false)
	if len(kept) != 0 {
		t.Errorf("kept = %v; a pod mounts only the workspace claim, so nothing may keep its anchor", kept)
	}
	if len(dropped) != len(mounts) {
		t.Errorf("dropped = %v; want all %d mounts", dropped, len(mounts))
	}
}

func TestK8sDocMountsKeepsOnlyBundleDocsWithTheDeclaration(t *testing.T) {
	bundle := []string{"/opt/fleet/client/personas", "/opt/fleet/client/protocols", "/opt/fleet/client/system_prompts", "/opt/fleet/client/skills"}
	uploads := "/var/lib/fleet/uploads"
	mounts := append(append([]string{}, bundle...), uploads)

	kept, dropped := k8sDocMounts(mounts, bundle, true)
	if len(kept) != len(bundle) {
		t.Fatalf("kept = %v; want the %d bundle doc dirs", kept, len(bundle))
	}
	for i, want := range bundle {
		if kept[i] != want {
			t.Errorf("kept[%d] = %q, want %q (order preserved)", i, kept[i], want)
		}
	}
	// The uploads root is control-plane state; no sandbox image contains it,
	// and the declaration says nothing about it.
	if len(dropped) != 1 || dropped[0] != uploads {
		t.Errorf("dropped = %v; want only %q", dropped, uploads)
	}
}

func TestK8sDocMountsNeverKeepsAMaterializedSkillsTree(t *testing.T) {
	// The merged built-in + bundle skills tree lives under the control plane's
	// data dir with a hash-derived name — a sandbox image cannot carry it, so
	// the declaration must not extend to it even though it IS the bundle's
	// resolved skills dir.
	merged := filepath.Join("/var/lib/fleet", "skills-merged", "f693617985b1")
	bundle := []string{"/opt/fleet/client/protocols", merged}
	mounts := bundle

	kept, dropped := k8sDocMounts(mounts, bundle, true)
	if len(kept) != 1 || kept[0] != "/opt/fleet/client/protocols" {
		t.Errorf("kept = %v; want only the bundle-path protocols dir", kept)
	}
	if len(dropped) != 1 || dropped[0] != merged {
		t.Errorf("dropped = %v; want the merged skills tree %q", dropped, merged)
	}
}

func TestK8sDocMountsIgnoresUnlistedAndEmptyPaths(t *testing.T) {
	bundle := []string{"/opt/fleet/client/protocols/"} // trailing slash, same dir
	mounts := []string{"", "/opt/fleet/client/protocols", "/somewhere/else"}

	kept, dropped := k8sDocMounts(mounts, bundle, true)
	if len(kept) != 1 || kept[0] != "/opt/fleet/client/protocols" {
		t.Errorf("kept = %v; want the protocols dir matched after path cleaning", kept)
	}
	if len(dropped) != 1 || dropped[0] != "/somewhere/else" {
		t.Errorf("dropped = %v; want only the path that is not a bundle doc dir", dropped)
	}
}

// splitWorkspaceNestedMounts is the rule that lets the shared file library's
// staged tree (docs/SHARED-FILES.md) — which lives INSIDE the workspace claim —
// bypass the host-mount drop above: every pod mounts the claim, so a
// workspace-nested read-only root is reachable by construction.
func TestSplitWorkspaceNestedMounts(t *testing.T) {
	root := "/var/lib/fleet/workspace"
	shared := filepath.Join(root, "shared")
	mounts := []string{
		"", // dropped entirely
		"/opt/fleet/client/protocols",
		shared,
		"/var/lib/fleet/uploads",
		root,                             // the root itself is the rw mount, not a nested ro root
		"/var/lib/fleet/workspace-other", // sibling with the root as a string prefix — NOT nested
	}
	nested, others := splitWorkspaceNestedMounts(mounts, root)
	if len(nested) != 1 || nested[0] != shared {
		t.Errorf("nested = %v; want only %q", nested, shared)
	}
	want := []string{"/opt/fleet/client/protocols", "/var/lib/fleet/uploads", root, "/var/lib/fleet/workspace-other"}
	if len(others) != len(want) {
		t.Fatalf("others = %v; want %v", others, want)
	}
	for i := range want {
		if others[i] != want[i] {
			t.Errorf("others[%d] = %q, want %q", i, others[i], want[i])
		}
	}
	// An empty workspace root nests nothing.
	if nested, _ := splitWorkspaceNestedMounts(mounts, ""); len(nested) != 0 {
		t.Errorf("empty root nested = %v; want none", nested)
	}
}
