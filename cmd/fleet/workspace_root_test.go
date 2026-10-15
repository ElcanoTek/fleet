package main

import (
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// TestPinWorkspaceRootDefaultsToAbsoluteCwdWorkspace: with no
// FLEET_WORKSPACE_ROOT the boot pin resolves ./workspace against the cwd and
// publishes it, so every lazy env reader sees the same ABSOLUTE root an
// operator would have set by hand — in particular the per-conversation
// workspace, which used to come back relative.
func TestPinWorkspaceRootDefaultsToAbsoluteCwdWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("FLEET_WORKSPACE_ROOT", "")
	t.Setenv("CHAT_WORKSPACE_ROOT", "")
	t.Setenv("CUTLASS_WORKSPACE_ROOT", "")

	cfg := &config.Config{}
	pinWorkspaceRoot(cfg)
	want := filepath.Join(dir, "workspace")
	if cfg.WorkspaceRoot != want {
		t.Fatalf("cfg.WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if got := agentcore.WorkspaceRootDir(); got != want {
		t.Fatalf("agentcore.WorkspaceRootDir() = %q, want %q", got, want)
	}
	if got := tools.WorkspaceDirForConversation("conv-1"); got != filepath.Join(want, "conv-1") {
		t.Fatalf("WorkspaceDirForConversation = %q, want absolute %q", got, filepath.Join(want, "conv-1"))
	}
}

// TestPinWorkspaceRootHonorsOverride: an operator-set root (including a
// legacy alias, and a relative one) wins and is absolutized, never replaced
// by the default.
func TestPinWorkspaceRootHonorsOverride(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("FLEET_WORKSPACE_ROOT", "")

	cfg := &config.Config{WorkspaceRoot: "custom/ws"}
	pinWorkspaceRoot(cfg)
	want := filepath.Join(dir, "custom", "ws")
	if cfg.WorkspaceRoot != want {
		t.Fatalf("cfg.WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if got := agentcore.WorkspaceRootDir(); got != want {
		t.Fatalf("agentcore.WorkspaceRootDir() = %q, want %q", got, want)
	}
}
