package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageFixture is a bundle with one of each skill source — the built-in pack
// (inherited), one Agent Plugin skill, one bundle skill — loaded, plus an
// empty "workspace root" to stage into. The staged tree is what a kubernetes
// sandbox pod reads (ADR-0055), so these tests pin that it carries all three
// sources and keeps the live-reload contract there.
func stageFixture(t *testing.T) (b *Bundle, bundleDir, workspaceRoot string) {
	t.Helper()
	bundleDir = writePluginBundle(t, "mcp_servers: []\n",
		pluginFixture{dir: "p", manifest: minimalManifest("alpha"), skills: map[string]string{"from-plugin": "A plugin skill."}},
	)
	mustWrite(t, filepath.Join(bundleDir, "skills", "from-bundle", "SKILL.md"),
		"---\nname: from-bundle\ndescription: A bundle skill.\n---\n\nBundle body.\n")
	var err error
	b, err = Load(bundleDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b, bundleDir, t.TempDir()
}

func TestStageSkillsAt_CarriesEverySourceAndFollowsTheDisk(t *testing.T) {
	b, bundleDir, root := stageFixture(t)
	before := b.SkillsDir
	if !IsMaterializedSkillsDir(before) {
		t.Fatalf("precondition: Load should have merged into the data-dir tree, got %q", before)
	}

	staged := filepath.Join(root, "skills")
	if err := b.StageSkillsAt(staged); err != nil {
		t.Fatalf("StageSkillsAt: %v", err)
	}
	if b.SkillsDir != staged {
		t.Fatalf("SkillsDir = %q, want the staged dir %q", b.SkillsDir, staged)
	}
	if IsMaterializedSkillsDir(staged) {
		t.Errorf("IsMaterializedSkillsDir(%q) = true; a staged tree lives in the workspace claim, not the data dir, and must NOT be dropped by the kubernetes mount policy", staged)
	}
	if b.BundleSkillsDir != filepath.Join(bundleDir, "skills") {
		t.Errorf("BundleSkillsDir = %q; staging must not move the author-owned source", b.BundleSkillsDir)
	}

	// All three sources, on disk at the exact path a pod mounts.
	for _, name := range []string{"data-profiler", "from-plugin", "from-bundle"} {
		if _, err := os.Stat(filepath.Join(staged, name, "SKILL.md")); err != nil {
			t.Errorf("%s missing from the staged tree: %v", name, err)
		}
	}
	roster := skillNames(b.Skills())
	for _, name := range []string{"data-profiler", "from-plugin", "from-bundle"} {
		if _, ok := roster[name]; !ok {
			t.Errorf("%s missing from Skills() after staging: %v", name, roster)
		}
	}
	if o := b.SkillOrigin("from-plugin"); o != (SkillOrigin{Source: "plugin", Plugin: "alpha"}) {
		t.Errorf("SkillOrigin(from-plugin) = %+v", o)
	}

	// Live-reload contract, same as the data-dir tree: a body edit, a new
	// plugin folder and a new bundle skill all land in the STAGED tree on the
	// next read — the tree a pod sees is never stale past one read.
	mustWrite(t, filepath.Join(bundleDir, "skills", "from-bundle", "SKILL.md"),
		"---\nname: from-bundle\ndescription: Edited in place.\n---\n\nEdited.\n")
	mustWrite(t, filepath.Join(bundleDir, "plugins", "p", "skills", "later", "SKILL.md"),
		"---\nname: later\ndescription: Plugin folder added after staging.\n---\n\nNew.\n")
	mustWrite(t, filepath.Join(bundleDir, "skills", "also-later", "SKILL.md"),
		"---\nname: also-later\ndescription: Bundle skill added after staging.\n---\n\nNew.\n")
	roster = skillNames(b.Skills())
	if roster["from-bundle"].Description != "Edited in place." {
		t.Errorf("edit not picked up: %+v", roster["from-bundle"])
	}
	for _, name := range []string{"later", "also-later"} {
		if _, ok := roster[name]; !ok {
			t.Errorf("%s missing after being added on disk: %v", name, roster)
		}
		if _, err := os.Stat(filepath.Join(staged, name, "SKILL.md")); err != nil {
			t.Errorf("%s not materialized into the staged tree: %v", name, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(staged, "from-bundle", "SKILL.md"))
	if err != nil || !strings.Contains(string(body), "Edited.") {
		t.Errorf("staged body not refreshed: %q, %v", body, err)
	}

	// The data-dir tree is left where it was — staging is a re-materialization,
	// not a move — so a podman deployment sharing the data dir is unaffected.
	if _, err := os.Stat(before); err != nil {
		t.Errorf("data-dir merged tree %q disappeared: %v", before, err)
	}
}

func TestStageSkillsAt_RespectsManifestKnobs(t *testing.T) {
	dir := writePluginBundle(t, "skills_builtin: false\n",
		pluginFixture{dir: "p", manifest: minimalManifest("alpha"), skills: map[string]string{"from-plugin": "A plugin skill."}},
	)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	staged := filepath.Join(t.TempDir(), "skills")
	if err := b.StageSkillsAt(staged); err != nil {
		t.Fatalf("StageSkillsAt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "data-profiler")); !os.IsNotExist(err) {
		t.Errorf("skills_builtin: false must keep the pack out of the staged tree too (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "from-plugin", "SKILL.md")); err != nil {
		t.Errorf("plugin skill missing with the pack off: %v", err)
	}

	// A bundle with NO merging at all (no pack, no plugins) still stages: on
	// kubernetes the staged copy is the only way skills/ reaches a pod.
	plain := t.TempDir()
	mustWrite(t, filepath.Join(plain, "manifest.yaml"), "skills_builtin: false\n")
	mustWrite(t, filepath.Join(plain, "skills", "only", "SKILL.md"), "---\nname: only\ndescription: The one skill.\n---\n\nBody.\n")
	pb, err := Load(plain)
	if err != nil {
		t.Fatalf("load plain: %v", err)
	}
	if pb.SkillsDir != pb.BundleSkillsDir {
		t.Fatalf("precondition: an unmerged bundle should serve its own skills/, got %q", pb.SkillsDir)
	}
	pstaged := filepath.Join(t.TempDir(), "skills")
	if err := pb.StageSkillsAt(pstaged); err != nil {
		t.Fatalf("StageSkillsAt plain: %v", err)
	}
	if pb.SkillsDir != pstaged {
		t.Errorf("SkillsDir = %q, want %q", pb.SkillsDir, pstaged)
	}
	if _, ok := skillNames(pb.Skills())["only"]; !ok {
		t.Error("bundle skill missing from the staged tree of an unmerged bundle")
	}
}

func TestStageSkillsAt_RefusesTheBundlesOwnDir(t *testing.T) {
	b, bundleDir, _ := stageFixture(t)
	if err := b.StageSkillsAt(filepath.Join(bundleDir, "skills")); err == nil {
		t.Fatal("staging INTO the bundle's own skills/ must be refused: the sync would delete every bundle skill the pack does not name")
	}
	if err := b.StageSkillsAt("  "); err == nil {
		t.Fatal("an empty stage dir must be refused")
	}
}

// The staged tree lives in a volume sandboxes can write to until the
// read-only mount is in place (and before the first boot that mounts it at
// all). Nothing planted there may be adopted or written through.
func TestStageSkillsAt_ReplacesPlantedEntries(t *testing.T) {
	t.Run("root is a symlink", func(t *testing.T) {
		b, _, root := stageFixture(t)
		victim := t.TempDir()
		mustWrite(t, filepath.Join(victim, "canary"), "untouched")
		staged := filepath.Join(root, "skills")
		if err := os.Symlink(victim, staged); err != nil {
			t.Fatal(err)
		}
		if err := b.StageSkillsAt(staged); err != nil {
			t.Fatalf("StageSkillsAt: %v", err)
		}
		info, err := os.Lstat(staged)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			t.Fatalf("staged root still a symlink or missing: %v %v", info, err)
		}
		if got, _ := os.ReadFile(filepath.Join(victim, "canary")); string(got) != "untouched" {
			t.Errorf("symlink target was written through: %q", got)
		}
		if entries, _ := os.ReadDir(victim); len(entries) != 1 {
			t.Errorf("symlink target gained entries: %v", entries)
		}
	})

	t.Run("root is world-writable", func(t *testing.T) {
		b, _, root := stageFixture(t)
		staged := filepath.Join(root, "skills")
		if err := os.Mkdir(staged, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(staged, 0o777); err != nil { // the untrusted mode under test
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(staged, "stray", "SKILL.md"), "planted")
		if err := b.StageSkillsAt(staged); err != nil {
			t.Fatalf("StageSkillsAt: %v", err)
		}
		info, err := os.Stat(staged)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o022 != 0 {
			t.Errorf("staged root still group/world-writable: %04o", info.Mode().Perm())
		}
		if _, err := os.Stat(filepath.Join(staged, "stray")); !os.IsNotExist(err) {
			t.Errorf("planted entry survived the rebuild (err=%v)", err)
		}
	})

	t.Run("file symlink inside is replaced, not followed", func(t *testing.T) {
		b, _, root := stageFixture(t)
		staged := filepath.Join(root, "skills")
		victim := filepath.Join(t.TempDir(), "victim.txt")
		mustWrite(t, victim, "untouched")
		if err := os.MkdirAll(filepath.Join(staged, "data-profiler"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(staged, "data-profiler", "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		if err := b.StageSkillsAt(staged); err != nil {
			t.Fatalf("StageSkillsAt: %v", err)
		}
		info, err := os.Lstat(filepath.Join(staged, "data-profiler", "SKILL.md"))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("SKILL.md is not a regular file: %v %v", info, err)
		}
		if got, _ := os.ReadFile(victim); string(got) != "untouched" {
			t.Errorf("victim written through a planted symlink: %q", got)
		}
	})

	t.Run("hard link inside is unlinked, not truncated", func(t *testing.T) {
		b, _, root := stageFixture(t)
		staged := filepath.Join(root, "skills")
		// Same filesystem as the staged tree so the link can exist at all.
		victim := filepath.Join(root, "victim.txt")
		mustWrite(t, victim, "untouched")
		if err := os.MkdirAll(filepath.Join(staged, "data-profiler"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(victim, filepath.Join(staged, "data-profiler", "SKILL.md")); err != nil {
			t.Skipf("hard links unsupported here: %v", err)
		}
		if err := b.StageSkillsAt(staged); err != nil {
			t.Fatalf("StageSkillsAt: %v", err)
		}
		if got, _ := os.ReadFile(victim); string(got) != "untouched" {
			t.Errorf("hard-linked victim was rewritten in place: %q", got)
		}
		body, _ := os.ReadFile(filepath.Join(staged, "data-profiler", "SKILL.md"))
		if !strings.Contains(string(body), "name: data-profiler") {
			t.Errorf("staged SKILL.md does not carry the skill: %q", body)
		}
	})

	t.Run("symlink where a skill dir belongs, file where a dir belongs", func(t *testing.T) {
		b, _, root := stageFixture(t)
		staged := filepath.Join(root, "skills")
		elsewhere := t.TempDir()
		if err := os.MkdirAll(staged, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(staged, "data-profiler")); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(staged, "from-bundle"), "a file, not a dir")
		if err := b.StageSkillsAt(staged); err != nil {
			t.Fatalf("StageSkillsAt: %v", err)
		}
		for _, name := range []string{"data-profiler", "from-bundle"} {
			info, err := os.Lstat(filepath.Join(staged, name))
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				t.Errorf("%s is not a real directory after staging: %v %v", name, info, err)
			}
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("symlinked dir target was written into: %v", entries)
		}
	})
}

// A crash between two syncs must never leave a torn SKILL.md for a pod to
// read: writes go to a temp file and are renamed over the target.
func TestWriteFileIfChanged_IsAtomicAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "SKILL.md")
	if err := writeFileIfChanged(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileIfChanged(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(path)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("unchanged content rewrote the file (mtime moved) — prompt-cache-relevant reads must stay stable")
	}
	if err := writeFileIfChanged(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Errorf("content = %q, want two", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "nested", ".skill-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
