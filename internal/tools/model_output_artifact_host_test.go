//go:build fleet_host_executor

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

func TestSandboxModelOutputArtifactRoundTripAndBoundedReplacement(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx := WithForcedWorkingDir(context.Background(), root)
	ctx, release, err := WithSandboxModelOutputArtifacts(ctx, sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	view := NewViewFileTool(sb)
	type advertised struct{ path, content string }
	retained := make([]advertised, 0, modelOutputArtifactSlots)
	for i := 0; i < modelOutputArtifactSlots+3; i++ {
		content := fmt.Sprintf("governed artifact %02d %s", i, strings.Repeat("x", 256))
		path, stageErr := StageModelOutputArtifact(ctx, "tool", fmt.Sprintf("call-%d", i), "text", content)
		if i >= modelOutputArtifactSlots {
			if !errors.Is(stageErr, ErrModelOutputArtifactCapacity) || path != "" {
				t.Fatalf("stage %d after capacity: path=%q err=%v", i, path, stageErr)
			}
			continue
		}
		if stageErr != nil {
			t.Fatalf("stage %d: %v", i, stageErr)
		}
		if filepath.IsAbs(path) || filepath.ToSlash(filepath.Dir(filepath.Dir(path))) != modelOutputArtifactDir {
			t.Fatalf("path must be workspace-relative: %q", path)
		}
		retained = append(retained, advertised{path: path, content: content})
	}
	// Verify only after every parallel-step-equivalent result has staged. No
	// advertised path may have wrapped to another result before the model can use
	// the recovery metadata.
	for i, item := range retained {
		input := fmt.Sprintf(`{"path":%q,"limit":1024}`, item.path)
		resp, runErr := view.Run(ctx, fantasy.ToolCall{ID: "view", Name: "view_file", Input: input})
		viewContent := resp.Content
		if marker := strings.Index(viewContent, "\n\n(file metadata: sha256="); marker >= 0 {
			viewContent = viewContent[:marker]
		}
		if runErr != nil || resp.IsError || viewContent != item.content {
			t.Fatalf("view_file recovery %d: err=%v response=%q", i, runErr, resp.Content)
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(modelOutputArtifactDir)))
	if err != nil {
		t.Fatal(err)
	}
	if got := artifactSlotEntryCount(entries); got != modelOutputArtifactSlots {
		t.Fatalf("retention created %d artifact slots, want fixed capacity of %d", got, modelOutputArtifactSlots)
	}
	// Slot 00 remains the content its advertised path promised; capacity never
	// aliases it to artifact 16.
	replaced, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(retained[0].path)))
	if err != nil || !strings.HasPrefix(string(replaced), "governed artifact 00 ") {
		t.Fatalf("slot immutability: err=%v content=%q", err, replaced)
	}
}

func TestSandboxModelOutputArtifactRejectsUnboundedRetention(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	_, err = StageModelOutputArtifact(ctx, "tool", "huge", "text", strings.Repeat("x", modelOutputArtifactMaxBytes+1))
	if !errors.Is(err, ErrModelOutputArtifactTooLarge) {
		t.Fatalf("oversized artifact err=%v, want %v", err, ErrModelOutputArtifactTooLarge)
	}
	if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(modelOutputArtifactDir))); !os.IsNotExist(statErr) {
		t.Fatalf("oversized artifact unexpectedly wrote a file: %v", statErr)
	}
}

func TestSandboxModelOutputArtifactRegistryReleaseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	_, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	_, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	releaseA()
	releaseA()
	releaseB()
	absRoot, _ := filepath.Abs(root)
	artifactRootRegistry.Lock()
	_, present := artifactRootRegistry.roots[filepath.Clean(absRoot)]
	artifactRootRegistry.Unlock()
	if present {
		t.Fatal("released workspace remained in the process registry")
	}
}

func TestSandboxModelOutputArtifactConcurrentCapacityIsBoundedAndImmutable(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	var wg sync.WaitGroup
	type stageResult struct {
		path, content string
		err           error
	}
	results := make(chan stageResult, 48)
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("artifact %d", i)
			path, stageErr := StageModelOutputArtifact(ctx, "concurrent", fmt.Sprintf("call-%d", i), "text", content)
			results <- stageResult{path: path, content: content, err: stageErr}
		}(i)
	}
	wg.Wait()
	close(results)
	advertised := make(map[string]string)
	capacity := 0
	for result := range results {
		switch {
		case result.err == nil:
			if prior, exists := advertised[result.path]; exists {
				t.Fatalf("reused advertised path %q for %q and %q", result.path, prior, result.content)
			}
			advertised[result.path] = result.content
		case errors.Is(result.err, ErrModelOutputArtifactCapacity):
			capacity++
		default:
			t.Errorf("unexpected stage error: %v", result.err)
		}
	}
	if len(advertised) != modelOutputArtifactSlots || capacity != 48-modelOutputArtifactSlots {
		t.Fatalf("successes=%d capacity=%d, want %d/%d", len(advertised), capacity, modelOutputArtifactSlots, 48-modelOutputArtifactSlots)
	}
	for path, want := range advertised {
		got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil || string(got) != want {
			t.Fatalf("advertised %q changed: err=%v got=%q want=%q", path, readErr, got, want)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(modelOutputArtifactDir)))
	if err != nil {
		t.Fatal(err)
	}
	if got := artifactSlotEntryCount(entries); got != modelOutputArtifactSlots {
		t.Fatalf("concurrent retention created %d artifact slots, want %d", got, modelOutputArtifactSlots)
	}
}

func TestSandboxModelOutputArtifactHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := StageModelOutputArtifact(cancelled, "tool", "cancelled", "text", "must not be retained"); err == nil {
		t.Fatal("cancelled artifact write unexpectedly succeeded")
	}
}

func TestSandboxModelOutputArtifactRejectsInteriorSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := os.Symlink(outside, filepath.Join(root, ".fleet")); err != nil {
		t.Fatal(err)
	}
	_, err = StageModelOutputArtifact(ctx, "tool", "call", "text", "must stay confined")
	if !errors.Is(err, sandbox.ErrFileOpUnsafePath) {
		t.Fatalf("symlink escape err=%v, want %v", err, sandbox.ErrFileOpUnsafePath)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("artifact escaped bound root: err=%v entries=%v", readErr, entries)
	}
}

func TestSandboxModelOutputArtifactRejectsWholeRootExchange(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	ctx, release, err := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if err := os.Rename(root, filepath.Join(parent, "old-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err = StageModelOutputArtifact(ctx, "tool", "call", "text", "must not bless replacement")
	if !errors.Is(err, sandbox.ErrFileOpUnsafePath) {
		t.Fatalf("whole-root exchange err=%v, want %v", err, sandbox.ErrFileOpUnsafePath)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("artifact wrote into exchanged root: err=%v entries=%v", readErr, entries)
	}
}

func TestSandboxModelOutputArtifactCursorSurvivesStagerAndSandboxRecreation(t *testing.T) {
	root := t.TempDir()
	sbA := sandbox.NewHost(nil)
	ctxA, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sbA, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := StageModelOutputArtifact(ctxA, "tool", "first", "text", "first")
	if err != nil {
		t.Fatal(err)
	}
	releaseA()
	sbA.Close()

	sbB := sandbox.NewHost(nil)
	t.Cleanup(sbB.Close)
	ctxB, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sbB, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseB)
	second, err := StageModelOutputArtifact(ctxB, "tool", "second", "text", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first != modelOutputArtifactPath(0, "first") || second != modelOutputArtifactPath(1, "second") {
		t.Fatalf("recreated stager reused a live recovery path: first=%q second=%q", first, second)
	}
}

func TestSandboxModelOutputArtifactNeverTrustsResetCursorOverExistingSlot(t *testing.T) {
	root := t.TempDir()
	sbA := sandbox.NewHost(nil)
	ctxA, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sbA, root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := StageModelOutputArtifact(ctxA, "tool", "first", "text", "promised first content")
	if err != nil {
		t.Fatal(err)
	}
	releaseA()
	sbA.Close()

	// Simulate a crash/operator/workspace-tool resetting the advisory cursor.
	// The existing advertised file remains authoritative and must not be reused.
	statePath := filepath.Join(root, filepath.FromSlash(modelOutputArtifactState))
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	sbB := sandbox.NewHost(nil)
	t.Cleanup(sbB.Close)
	ctxB, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sbB, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseB)
	second, err := StageModelOutputArtifact(ctxB, "tool", "second", "text", "second content")
	if err != nil {
		t.Fatal(err)
	}
	if first != modelOutputArtifactPath(0, "promised first content") || second != modelOutputArtifactPath(1, "second content") {
		t.Fatalf("reset cursor reused an advertised path: first=%q second=%q", first, second)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(first)))
	if err != nil || string(got) != "promised first content" {
		t.Fatalf("reset cursor changed promised content: err=%v got=%q", err, got)
	}
}

func TestSandboxModelOutputArtifactDeletedCursorAndArtifactNeverReuseAdvertisedPath(t *testing.T) {
	root := t.TempDir()
	sbA := sandbox.NewHost(nil)
	ctxA, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sbA, root)
	if err != nil {
		t.Fatal(err)
	}
	firstContent := "persisted history promised these first bytes"
	first, err := StageModelOutputArtifact(ctxA, "tool", "first", "text", firstContent)
	if err != nil {
		t.Fatal(err)
	}
	removeArtifactFilesThroughSandbox(ctxA, t, sbA, root,
		modelOutputArtifactState, modelOutputArtifactSlotMarker(0), first)
	releaseA()
	sbA.Close()

	sbB := sandbox.NewHost(nil)
	t.Cleanup(sbB.Close)
	ctxB, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sbB, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseB)
	secondContent := "different bytes from a later process lifetime"
	second, err := StageModelOutputArtifact(ctxB, "tool", "second", "text", secondContent)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second != modelOutputArtifactPath(1, secondContent) {
		t.Fatalf("deleted cursor/artifact reused advertised path: first=%q second=%q", first, second)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(first))); !os.IsNotExist(err) {
		t.Fatalf("old advertised path was recreated for different bytes: %v", err)
	}
}

func TestSandboxModelOutputArtifactContentAddressPreventsAliasAfterAllAllocatorEvidenceDeleted(t *testing.T) {
	root := t.TempDir()
	sbA := sandbox.NewHost(nil)
	ctxA, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sbA, root)
	if err != nil {
		t.Fatal(err)
	}
	firstContent := "first immutable recovery payload"
	first, err := StageModelOutputArtifact(ctxA, "tool", "first", "text", firstContent)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the complete slot directory. That necessarily removes its child
	// artifact too, preserving the 16-file disk bound. With no allocator state
	// left, slot zero may be selected again, but content addressing must still
	// make it impossible for different bytes to alias the historical path.
	removeArtifactTreesThroughSandbox(ctxA, t, sbA, root,
		modelOutputArtifactState, modelOutputArtifactSlotDir(0))
	releaseA()
	sbA.Close()

	sbB := sandbox.NewHost(nil)
	t.Cleanup(sbB.Close)
	ctxB, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sbB, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseB)
	secondContent := "different immutable recovery payload"
	second, err := StageModelOutputArtifact(ctxB, "tool", "second", "text", secondContent)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second != modelOutputArtifactPath(0, secondContent) {
		t.Fatalf("content-addressed slot aliased old history: first=%q second=%q", first, second)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(second)))
	if err != nil || string(got) != secondContent {
		t.Fatalf("new content-addressed artifact: err=%v content=%q", err, got)
	}
}

func TestSandboxModelOutputArtifactSlotDirectoriesPreserveCapacityAfterAllocatorFileDeletion(t *testing.T) {
	root := t.TempDir()
	sbA := sandbox.NewHost(nil)
	ctxA, releaseA, err := WithSandboxModelOutputArtifacts(context.Background(), sbA, root)
	if err != nil {
		t.Fatal(err)
	}
	removable := []string{modelOutputArtifactState}
	for slot := uint64(0); slot < modelOutputArtifactSlots; slot++ {
		if _, err := StageModelOutputArtifact(ctxA, "tool", fmt.Sprintf("call-%d", slot), "text", fmt.Sprintf("payload-%d", slot)); err != nil {
			t.Fatalf("stage slot %d: %v", slot, err)
		}
		removable = append(removable, modelOutputArtifactSlotMarker(slot))
	}
	// Remove every standalone allocator file but leave the advertised children.
	// Their containing directories remain unambiguous used-slot tombstones.
	removeArtifactFilesThroughSandbox(ctxA, t, sbA, root, removable...)
	releaseA()
	sbA.Close()

	sbB := sandbox.NewHost(nil)
	t.Cleanup(sbB.Close)
	ctxB, releaseB, err := WithSandboxModelOutputArtifacts(context.Background(), sbB, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseB)
	if path, err := StageModelOutputArtifact(ctxB, "tool", "overflow", "text", "must remain bounded"); !errors.Is(err, ErrModelOutputArtifactCapacity) || path != "" {
		t.Fatalf("deleted allocator files bypassed capacity: path=%q err=%v", path, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(modelOutputArtifactDir)))
	if err != nil {
		t.Fatal(err)
	}
	if got := artifactSlotEntryCount(entries); got != modelOutputArtifactSlots {
		t.Fatalf("allocator reset retained %d slot directories, want %d", got, modelOutputArtifactSlots)
	}
}

func removeArtifactFilesThroughSandbox(ctx context.Context, t *testing.T, sb *sandbox.Sandbox, root string, relative ...string) {
	removeArtifactPathsThroughSandbox(ctx, t, sb, root, "rm -f -- ", relative...)
}

func removeArtifactTreesThroughSandbox(ctx context.Context, t *testing.T, sb *sandbox.Sandbox, root string, relative ...string) {
	removeArtifactPathsThroughSandbox(ctx, t, sb, root, "rm -rf -- ", relative...)
}

func removeArtifactPathsThroughSandbox(ctx context.Context, t *testing.T, sb *sandbox.Sandbox, root, command string, relative ...string) {
	t.Helper()
	quoted := make([]string, 0, len(relative))
	for _, path := range relative {
		quoted = append(quoted, fmt.Sprintf("%q", filepath.Join(root, filepath.FromSlash(path))))
	}
	result, err := sb.RunBash(ctx, sandbox.BashRequest{Command: command + strings.Join(quoted, " "), WorkingDir: root})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("remove allocator files through sandbox: err=%v exit=%d stderr=%s", err, result.ExitCode, result.Stderr)
	}
}

func artifactSlotEntryCount(entries []os.DirEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "slot-") {
			count++
		}
	}
	return count
}

func TestSandboxModelOutputArtifactGitIgnoreKeepsMainAndLinkedWorktreesClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-q")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "Test")
	trackedPath := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("consumer content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "tracked.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "initial")
	excludePath := filepath.Join(repo, ".git", "info", "exclude")
	excludeBefore, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}

	stage := func(root, content string) {
		t.Helper()
		sb := sandbox.NewHost(nil)
		ctx, release, stageErr := WithSandboxModelOutputArtifacts(context.Background(), sb, root)
		if stageErr != nil {
			sb.Close()
			t.Fatal(stageErr)
		}
		if _, stageErr = StageModelOutputArtifact(ctx, "tool", "call", "text", content); stageErr != nil {
			release()
			sb.Close()
			t.Fatal(stageErr)
		}
		release()
		sb.Close()
	}
	stage(repo, "main artifact one")
	stage(repo, "main artifact two") // reconstructed stager rewrites idempotently
	ignore, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(modelOutputArtifactIgnore)))
	if err != nil || string(ignore) != "*\n" {
		t.Fatalf("artifact .gitignore: err=%v content=%q", err, ignore)
	}
	if got, err := os.ReadFile(trackedPath); err != nil || string(got) != "consumer content\n" {
		t.Fatalf("stager changed a consumer file: err=%v content=%q", err, got)
	}
	excludeAfter, err := os.ReadFile(excludePath)
	if err != nil || string(excludeAfter) != string(excludeBefore) {
		t.Fatalf("stager mutated host git metadata: err=%v\nbefore=%q\nafter=%q", err, excludeBefore, excludeAfter)
	}
	if status := strings.TrimSpace(runGitTest(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("main worktree status contains artifact files:\n%s", status)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	runGitTest(t, repo, "worktree", "add", "-q", "-b", "artifact-test", linked)
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", linked).Run() })
	stage(linked, "linked artifact")
	if status := strings.TrimSpace(runGitTest(t, linked, "status", "--porcelain")); status != "" {
		t.Fatalf("linked worktree status contains artifact files:\n%s", status)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
