//go:build fleet_host_executor

package scheduledrun

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestConfigureRunWorkspaceNonWorktreeDisablesCrossRunArtifactRetention(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, effective, err := configureRunWorkspace(context.Background(), sb, "", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	want, _ := filepath.Abs(root)
	if effective != want || tools.ForcedWorkingDirFromContext(ctx) != want {
		t.Fatalf("effective root=%q context=%q, want %q", effective, tools.ForcedWorkingDirFromContext(ctx), want)
	}

	pwd, err := sb.RunBash(ctx, sandbox.BashRequest{Command: "pwd"})
	if err != nil || strings.TrimSpace(string(pwd.Stdout)) != want {
		t.Fatalf("sandbox cwd: err=%v stdout=%q want=%q", err, pwd.Stdout, want)
	}
	if _, err := tools.StageModelOutputArtifact(ctx, "scheduled_tool", "call-1", "text", "governed"); !errors.Is(err, tools.ErrModelOutputArtifactScope) {
		t.Fatalf("shared non-worktree scope retained cross-run artifact: %v", err)
	}
}

func TestConfigureRunWorkspacePrefersThreadedWorktreeRoot(t *testing.T) {
	shared := t.TempDir()
	worktree := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	threaded := tools.WithForcedWorkingDir(context.Background(), worktree)
	ctx, release, effective, err := configureRunWorkspace(threaded, sb, "/ignored-worktree", shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	want, _ := filepath.Abs(worktree)
	if effective != want || tools.ForcedWorkingDirFromContext(ctx) != want {
		t.Fatalf("effective root=%q context=%q, want threaded %q", effective, tools.ForcedWorkingDirFromContext(ctx), want)
	}
	path, err := tools.StageModelOutputArtifact(ctx, "scheduled_tool", "call-1", "text", "isolated worktree output")
	digest := sha256.Sum256([]byte("isolated worktree output"))
	wantPath := fmt.Sprintf(".fleet/tool-output/slot-00/artifact-%x.txt", digest)
	if err != nil || path != wantPath {
		t.Fatalf("isolated worktree artifact: path=%q err=%v", path, err)
	}
}
