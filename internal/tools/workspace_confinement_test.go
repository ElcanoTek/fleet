package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// setManagedRootForTest sets the package-global managed workspace root and
// restores the previous value on cleanup. In-package so it can reset to "" (the
// public SetWorkspaceRoot ignores empty by design).
func setManagedRootForTest(t *testing.T, root string) {
	t.Helper()
	managedWorkspaceRootMu.Lock()
	prev := managedWorkspaceRoot
	managedWorkspaceRoot = root
	managedWorkspaceRootMu.Unlock()
	t.Cleanup(func() {
		managedWorkspaceRootMu.Lock()
		managedWorkspaceRoot = prev
		managedWorkspaceRootMu.Unlock()
	})
}

// TestAllowedBaseDirs_ConfinesToWorkspaceRoot proves the sandbox-confinement
// fix at the authorization layer: once a workspace root is registered, the
// host-side file tools' allowlist is the workspace tree (+ temp + operator
// FLEET_ALLOWED_DIRS) and NOT the process cwd — so a sibling DataDir path
// (attachments/uploads/api_keys.json under the same StateDirectory in prod) is
// rejected, while the process cwd no longer blesses it.
func TestAllowedBaseDirs_ConfinesToWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	// Point os.TempDir() at an isolated dir under base so the always-allowed
	// temp entry does NOT cover the sibling workspace/data dirs (t.TempDir()
	// itself lives under the real temp root, which would otherwise allowlist
	// everything under base).
	tmp := filepath.Join(base, "tmp")
	mustMkdir(t, tmp)
	t.Setenv("TMPDIR", tmp)

	wsRoot := filepath.Join(base, "workspace")
	dataDir := filepath.Join(base, "data")
	mustMkdir(t, filepath.Join(wsRoot, "c1"))
	mustMkdir(t, dataDir)
	setManagedRootForTest(t, wsRoot)
	t.Setenv("FLEET_ALLOWED_DIRS", "")

	dirs, err := AllowedBaseDirs()
	if err != nil {
		t.Fatalf("AllowedBaseDirs: %v", err)
	}
	// Workspace root is allowed; its parent (StateDirectory) and the sibling
	// DataDir are not.
	if !isSubPathAny(dirs, filepath.Join(wsRoot, "c1", "out.txt")) {
		t.Errorf("a path under the workspace root should be allowed; dirs=%v", dirs)
	}
	if isSubPathAny(dirs, filepath.Join(dataDir, "api_keys.json")) {
		t.Errorf("a sibling DataDir path must NOT be allowed (sandbox-bypass); dirs=%v", dirs)
	}
	if isSubPathAny(dirs, filepath.Join(base, "anything")) {
		t.Errorf("the StateDirectory parent must NOT be allowed; dirs=%v", dirs)
	}

	// ValidatePath backstops the same containment, including for not-yet-existing
	// write targets (parent-walk) and — critically — symlinks: a link planted in
	// the workspace pointing at the sibling DataDir must be rejected because
	// ValidatePath resolves it and re-checks the real path.
	if _, err := ValidatePath(filepath.Join(wsRoot, "c1", "out.txt")); err != nil {
		t.Errorf("write target under workspace root should validate: %v", err)
	}
	if _, err := ValidatePath(filepath.Join(dataDir, "api_keys.json")); err == nil {
		t.Error("a DataDir path must be rejected by ValidatePath")
	}
	if err := os.Symlink(dataDir, filepath.Join(wsRoot, "c1", "evil")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ValidatePath(filepath.Join(wsRoot, "c1", "evil", "secret.txt")); err == nil {
		t.Error("a symlink from inside the workspace to DataDir must be rejected (EvalSymlinks re-check)")
	}
}

// TestAllowedBaseDirs_HonorsFleetAllowedDirs proves an operator-opted absolute
// dir stays reachable even with the workspace root confinement active — the
// FLEET_ALLOWED_DIRS escape hatch is not broken by the fix.
func TestAllowedBaseDirs_HonorsFleetAllowedDirs(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	mustMkdir(t, tmp)
	t.Setenv("TMPDIR", tmp)

	wsRoot := filepath.Join(base, "workspace")
	extra := filepath.Join(base, "shared")
	mustMkdir(t, wsRoot)
	mustMkdir(t, extra)
	setManagedRootForTest(t, wsRoot)
	t.Setenv("FLEET_ALLOWED_DIRS", extra)

	if _, err := ValidatePath(filepath.Join(extra, "report.csv")); err != nil {
		t.Errorf("an operator FLEET_ALLOWED_DIRS path must remain allowed: %v", err)
	}
}

// TestAllowedBaseDirs_LegacyUsesCwd proves the unregistered path (tests / CLI /
// dev) still allows the process cwd, so those flows are unchanged.
func TestAllowedBaseDirs_LegacyUsesCwd(t *testing.T) {
	setManagedRootForTest(t, "") // explicitly unregistered
	dirs, err := AllowedBaseDirs()
	if err != nil {
		t.Fatalf("AllowedBaseDirs: %v", err)
	}
	cwd, _ := os.Getwd()
	if !isSubPathAny(dirs, filepath.Join(cwd, "somefile")) {
		t.Errorf("with no registered workspace root, the process cwd must be allowed (legacy); dirs=%v", dirs)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}
