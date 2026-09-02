package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func stageTestBundle(t *testing.T) *clientconfig.Bundle {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "mcp_servers: []\n")
	write("skills/mine/SKILL.md", "---\nname: mine\ndescription: A bundle skill.\n---\n\nBody.\n")
	b, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

// StageSkillsForBackend is the one boot step that makes skills readable from
// a sandbox pod (ADR-0055). Podman must be untouched by it; kubernetes must
// end up with the complete tree inside the workspace claim, at the path the
// nested-mount rule turns into a read-only subPath mount.
func TestStageSkillsForBackend(t *testing.T) {
	t.Run("podman is a no-op", func(t *testing.T) {
		b := stageTestBundle(t)
		before := b.SkillsDir
		got, err := StageSkillsForBackend(&config.Config{SandboxBackend: sandbox.BackendPodman, WorkspaceRoot: t.TempDir()}, b)
		if err != nil {
			t.Fatalf("podman: %v", err)
		}
		if got != before || b.SkillsDir != before {
			t.Errorf("podman changed the skills dir: %q -> %q", before, got)
		}
		if got, err := StageSkillsForBackend(&config.Config{}, b); err != nil || got != before {
			t.Errorf("unset backend: dir=%q err=%v; want %q, nil", got, err, before)
		}
	})

	t.Run("kubernetes stages the whole tree inside the workspace claim", func(t *testing.T) {
		b := stageTestBundle(t)
		root := t.TempDir()
		cfg := &config.Config{SandboxBackend: sandbox.BackendKubernetes, WorkspaceRoot: root}
		got, err := StageSkillsForBackend(cfg, b)
		if err != nil {
			t.Fatalf("kubernetes: %v", err)
		}
		want := tools.StagedSkillsDir(root)
		if got != want || b.SkillsDir != want {
			t.Fatalf("staged dir = %q (bundle %q), want %q", got, b.SkillsDir, want)
		}
		if filepath.Base(want) != tools.SkillsDirName || filepath.Dir(want) != root {
			t.Errorf("staged dir %q is not <workspace root>/%s", want, tools.SkillsDirName)
		}
		// Built-in pack AND the bundle skill, on disk where a pod will mount it.
		for _, name := range []string{"data-profiler", "mine"} {
			if _, err := os.Stat(filepath.Join(want, name, "SKILL.md")); err != nil {
				t.Errorf("%s not staged: %v", name, err)
			}
		}
		// The pool build must classify it as a workspace-nested read-only root
		// (a subPath mount in every pod), never as a droppable host path.
		nested, others := splitWorkspaceNestedMounts(absSupportingDocs("/opt/fleet/client/protocols", got), root)
		if len(nested) != 1 || nested[0] != want {
			t.Errorf("nested = %v; the staged skills tree must be a workspace-nested mount", nested)
		}
		if len(others) != 1 || others[0] != "/opt/fleet/client/protocols" {
			t.Errorf("others = %v", others)
		}
		if clientconfig.IsMaterializedSkillsDir(got) {
			t.Errorf("the staged tree must not read as a data-dir merged tree, or k8sDocMounts would drop it")
		}
	})

	t.Run("kubernetes with no workspace root uses ./workspace", func(t *testing.T) {
		b := stageTestBundle(t)
		wd := t.TempDir()
		old, _ := os.Getwd()
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(old) })
		got, err := StageSkillsForBackend(&config.Config{SandboxBackend: sandbox.BackendKubernetes}, b)
		if err != nil {
			t.Fatalf("kubernetes default root: %v", err)
		}
		want := tools.StagedSkillsDir(filepath.Join(wd, "workspace"))
		if resolved, _ := filepath.EvalSymlinks(got); resolved != want {
			if got != want {
				t.Errorf("staged dir = %q, want %q", got, want)
			}
		}
	})

	t.Run("a staging failure leaves the bundle dir in place", func(t *testing.T) {
		b := stageTestBundle(t)
		before := b.SkillsDir
		// The workspace root is a FILE, so nothing can be created under it.
		rootFile := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(rootFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := StageSkillsForBackend(&config.Config{SandboxBackend: sandbox.BackendKubernetes, WorkspaceRoot: rootFile}, b)
		if err == nil {
			t.Fatal("expected an error staging under a file")
		}
		if got != before || b.SkillsDir != before {
			t.Errorf("failed staging moved the skills dir: %q -> %q", before, got)
		}
	})
}
