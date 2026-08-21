package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded skills pack must itself be well-formed — the skills analogue of
// TestBuiltinRemoteCatalog: a malformed built-in skill fails CI, not a
// customer boot.
func TestBuiltinSkillsPackWellFormed(t *testing.T) {
	merged, err := materializeMergedSkills(filepath.Join(t.TempDir(), "no-bundle-skills"), true, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	skills, problems := ReadSkills(merged)
	if len(problems) > 0 {
		t.Fatalf("builtin pack has malformed skills: %v", problems)
	}
	if len(skills) < 7 {
		t.Fatalf("builtin pack should ship at least 7 skills, got %d", len(skills))
	}
	for _, sk := range skills {
		if len(sk.Description) < 40 {
			t.Errorf("skill %q: description too thin to be useful: %q", sk.Name, sk.Description)
		}
	}
}

// Bundle integration: the merged dir inherits the pack, bundle skills win
// name collisions, knobs opt out / hide, and editing a bundle skill in place
// still takes effect on the next Skills() read (live-reload contract).
func TestMergedSkills(t *testing.T) {
	writeBundle := func(t *testing.T, manifest string, skills map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, desc := range skills {
			sdir := filepath.Join(dir, "skills", name)
			if err := os.MkdirAll(sdir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nDo the thing.\n"
			if err := os.WriteFile(filepath.Join(sdir, "SKILL.md"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	names := func(skills []Skill) map[string]string {
		out := map[string]string{}
		for _, sk := range skills {
			out[sk.Name] = sk.Description
		}
		return out
	}

	t.Run("inherits pack and bundle wins collisions", func(t *testing.T) {
		dir := writeBundle(t, "mcp_servers: []\n", map[string]string{
			"my-skill":      "a bundle-authored skill that should appear alongside the pack",
			"data-profiler": "the bundle's own profiler which must override the built-in one",
		})
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if b.SkillsDir == b.BundleSkillsDir {
			t.Fatal("SkillsDir should point at the merged dir")
		}
		got := names(b.Skills())
		if _, ok := got["my-skill"]; !ok {
			t.Error("bundle skill missing from merged roster")
		}
		if _, ok := got["web-research-brief"]; !ok {
			t.Error("builtin skill missing from merged roster")
		}
		if _, ok := got["bento-slides"]; !ok {
			t.Error("builtin skill with bundled templates/references missing from merged roster")
		}
		if _, ok := got["browserbase"]; !ok {
			t.Error("builtin skill pairing with a hosted MCP connector missing from merged roster")
		}
		if !strings.Contains(got["data-profiler"], "bundle's own profiler") {
			t.Errorf("bundle must win the name collision, got %q", got["data-profiler"])
		}
	})

	t.Run("opt-out keeps the bundle dir untouched", func(t *testing.T) {
		dir := writeBundle(t, "skills_builtin: false\n", map[string]string{
			"my-skill": "the only skill this bundle wants",
		})
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if b.SkillsDir != b.BundleSkillsDir {
			t.Errorf("opt-out should keep SkillsDir on the bundle: %q vs %q", b.SkillsDir, b.BundleSkillsDir)
		}
		got := names(b.Skills())
		if len(got) != 1 {
			t.Errorf("want only the bundle skill, got %v", got)
		}
	})

	t.Run("hidden tombstones drop individual builtins", func(t *testing.T) {
		dir := writeBundle(t, "skills_hidden: [data-profiler]\n", nil)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		got := names(b.Skills())
		if _, ok := got["data-profiler"]; ok {
			t.Error("hidden builtin still present")
		}
		if _, ok := got["release-notes"]; !ok {
			t.Error("other builtins should survive a tombstone")
		}
	})

	t.Run("live reload of an edited bundle skill", func(t *testing.T) {
		dir := writeBundle(t, "mcp_servers: []\n", map[string]string{
			"my-skill": "first revision of the description text goes right here",
		})
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		body := "---\nname: my-skill\ndescription: second revision picked up without a restart\n---\n\nDo it better.\n"
		if err := os.WriteFile(filepath.Join(dir, "skills", "my-skill", "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got := names(b.Skills())
		if !strings.Contains(got["my-skill"], "second revision") {
			t.Errorf("edit not picked up: %q", got["my-skill"])
		}
	})

	t.Run("deleted bundle skill leaves the merged dir", func(t *testing.T) {
		dir := writeBundle(t, "mcp_servers: []\n", map[string]string{
			"ephemeral": "a skill the operator is about to delete from the bundle",
		})
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "skills", "ephemeral")); err != nil {
			t.Fatal(err)
		}
		if _, ok := names(b.Skills())["ephemeral"]; ok {
			t.Error("deleted bundle skill still in the merged roster")
		}
	})
}

// TestMergedSkillsLivesUnderDataDir proves the merged tree is no longer the
// predictable /tmp/fleet-skills path (#1121).
func TestMergedSkillsLivesUnderDataDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	merged, err := materializeMergedSkills(filepath.Join(t.TempDir(), "skills"), true, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	wantPrefix := filepath.Join(data, mergedSkillsDirName)
	if !strings.HasPrefix(merged, wantPrefix+string(os.PathSeparator)) && merged != wantPrefix {
		t.Fatalf("merged path %q is not under data dir %q", merged, wantPrefix)
	}
	if strings.Contains(merged, filepath.Join("fleet-skills", "")) && strings.HasPrefix(merged, os.TempDir()) {
		t.Fatalf("merged path still under shared /tmp: %q", merged)
	}
}

func TestVerifyExistingDir_RejectsUnowned(t *testing.T) {
	dir := os.TempDir()
	if os.Geteuid() == 0 {
		// Root owns everything it creates and typically owns /tmp too.
		// Chown a planted dir to nobody so the uid check has something
		// to refuse — the pre-owned-dir case (#1121).
		dir = t.TempDir()
		if err := os.Chown(dir, 65534, 65534); err != nil {
			t.Skipf("chown nobody: %v", err)
		}
	}
	if err := verifyExistingDir(dir); err == nil {
		t.Fatalf("verifyExistingDir(%q) succeeded, want refuse", dir)
	}
}

func TestEnsureTrustedDir_RejectsWorldWritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := ensureTrustedDir(dir); err == nil {
		t.Fatal("expected refuse of a world-writable directory")
	}
}

func TestEnsureTrustedDir_RejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrustedDir(link); err == nil {
		t.Fatal("expected refuse of a symlink")
	}
}

func TestEnsureTrustedDir_RejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTrustedDir(path); err == nil {
		t.Fatal("expected refuse of a regular file")
	}
}

func TestMaterializeMergedSkills_DoesNotAdoptUntrustedPath(t *testing.T) {
	data := t.TempDir()
	t.Setenv("FLEET_DATA_DIR", data)
	planted := filepath.Join(data, mergedSkillsDirName)
	if err := os.MkdirAll(planted, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planted, 0o777); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(t.TempDir(), "skills")
	got, err := materializeMergedSkills(bundle, true, nil)
	if err == nil {
		t.Fatal("expected error for untrusted pre-existing path")
	}
	if got != bundle {
		t.Fatalf("adopted %q instead of falling back to the bundle dir %q", got, bundle)
	}
}
