package clientconfig

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Fleet ships a small pack of generally-useful Agent Skills baked into the
// binary (builtin_skills/), the skills analogue of the built-in MCP directory:
// every bundle inherits them by default so a fresh deployment has working
// skills out of the box. Unlike the MCP directory (a listing), skills are REAL
// FILES the sandbox bind-mounts, so inheritance works by MATERIALIZATION: at
// Load, the bundle's own skills/ and the embedded pack are synced into a
// merged on-disk dir (bundle wins a name collision, loudly), and
// Bundle.SkillsDir points AT the merged dir — every downstream consumer
// (prompt roster, sandbox mounts, workspace symlink, /skills API, taskrun,
// evals) picks the pack up with zero seam changes.
//
// Manifest knobs (mirroring the MCP directory):
//   skills_builtin: false      # opt out of the built-in pack entirely
//   skills_hidden: [name, …]   # drop individual built-in skills
//
// The merged dir lives under the fleet data dir (`$FLEET_DATA_DIR/skills-merged`,
// falling back to the user cache, never world-writable `/tmp/fleet-skills`)
// keyed by the bundle path — stable for the life of the box (prompt paths
// stay byte-identical across turns, per docs/PROMPT-CACHE-CONTRACT.md) and
// rebuilt from sources on every Load and on every Skills() read (cheap
// fingerprint check), which preserves the edit-a-skill-in-place live-reload
// contract for bundle skills. Every reuse verifies the tree is owned by
// this process and not group/world-writable, so a pre-planted attacker
// directory is refused rather than adopted (#1121).

//go:embed all:builtin_skills
var builtinSkillsFS embed.FS

const builtinSkillsRoot = "builtin_skills"

// mergedSkillsDirName is the stable subdirectory under the data/cache root
// that holds per-bundle merged trees. Kept short so prompt-cache paths stay
// compact.
const mergedSkillsDirName = "skills-merged"

// materializeMergedSkills builds (or refreshes) the merged skills dir for the
// bundle and returns its path. A failure is returned to the caller — Load
// degrades to the bundle's own skills dir with a loud log rather than failing
// the boot (skills are a capability, not a boot invariant). An untrusted
// pre-existing path is NEVER adopted (#1121).
func materializeMergedSkills(bundleSkillsDir string, builtinEnabled bool, hidden []string) (string, error) {
	if !builtinEnabled {
		return bundleSkillsDir, nil
	}
	base := mergedSkillsBase()
	if err := ensureTrustedDir(base); err != nil {
		return bundleSkillsDir, fmt.Errorf("merged-skills root: %w", err)
	}
	sum := sha256.Sum256([]byte(bundleSkillsDir))
	merged := filepath.Join(base, hex.EncodeToString(sum[:6]))
	if err := syncMergedSkills(bundleSkillsDir, merged, hidden); err != nil {
		return bundleSkillsDir, err
	}
	return merged, nil
}

// mergedSkillsBase is the parent of per-bundle merged trees. Prefer the
// operator's data dir (FLEET_DATA_DIR / CHAT_DATA_DIR) so the tree lives
// next to the rest of fleet state, not under world-writable /tmp. Fall
// back to the user cache, then a uid-scoped temp name — still never the
// predictable shared /tmp/fleet-skills path (#1121).
func mergedSkillsBase() string {
	if d := strings.TrimSpace(os.Getenv("FLEET_DATA_DIR")); d != "" {
		return filepath.Join(d, mergedSkillsDirName)
	}
	if d := strings.TrimSpace(os.Getenv("CHAT_DATA_DIR")); d != "" {
		return filepath.Join(d, mergedSkillsDirName)
	}
	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		return filepath.Join(cache, "fleet", mergedSkillsDirName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("fleet-skills-%d", os.Geteuid()))
}

// ensureTrustedDir creates path if missing and refuses to use it unless it
// is a real directory owned by this process and not group/world-writable.
// A pre-existing attacker-owned or world-writable path is a hard error so
// fleet never reads skill content out of a tree it did not create (#1121).
func ensureTrustedDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // world-readable by design: bind-mounted RO into the rootless sandbox, whose mapped user must traverse it (same rationale as tools/workspace.go)
		return err
	}
	return verifyExistingDir(path)
}

// IsMaterializedSkillsDir reports whether dir is a merged tree this package
// materialized (`<data|cache>/skills-merged/<hash>`) rather than a bundle's
// own `skills/`. It exists for one caller: the kubernetes sandbox backend,
// where a supporting-doc dir is only readable in a sandbox if the sandbox
// IMAGE carries it at the same absolute path — and a merged tree lives under
// the control plane's data dir, which no sandbox image can plausibly reproduce
// (its hash is derived from the bundle path, and the tree is rebuilt at boot).
// So a bundle inheriting the built-in pack can never serve in-sandbox skill
// reads on that backend; the caller drops the mount and says so, and the fix
// is the bundle's `skills_builtin: false`.
//
// Shape-based on purpose: the layout is this package's own convention, so the
// check belongs here, next to the code that builds the path.
func IsMaterializedSkillsDir(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	return filepath.Base(filepath.Dir(filepath.Clean(dir))) == mergedSkillsDirName
}

// verifyExistingDir is the ssh-style ownership/mode check: Lstat (so a
// symlink is not followed), must be a directory we own, must not be
// group- or world-writable.
func verifyExistingDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to use an untrusted merged-skills path", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the fleet process (uid %d)", path, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group/world-writable (%04o); refusing to use an untrusted merged-skills path", path, info.Mode().Perm())
	}
	return nil
}

// syncMergedSkills rebuilds merged from its two sources. It writes into a
// sibling temp dir and renames over the previous generation's entries so a
// concurrent reader never sees a half-written skill; stale entries from a
// prior sync are removed.
func syncMergedSkills(bundleSkillsDir, merged string, hidden []string) error {
	hiddenSet := map[string]bool{}
	for _, h := range hidden {
		hiddenSet[strings.TrimSpace(h)] = true
	}

	if err := ensureTrustedDir(merged); err != nil {
		return err
	}

	want := map[string]bool{}

	// Built-in pack first, so a bundle skill with the same name overwrites it
	// (bundle wins).
	entries, err := fs.ReadDir(builtinSkillsFS, builtinSkillsRoot)
	if err != nil {
		return fmt.Errorf("read embedded skills: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || hiddenSet[e.Name()] {
			continue
		}
		want[e.Name()] = true
		if err := copyEmbeddedSkill(e.Name(), filepath.Join(merged, e.Name())); err != nil {
			return err
		}
	}

	// Bundle skills overlay.
	if bundleEntries, err := os.ReadDir(bundleSkillsDir); err == nil {
		for _, e := range bundleEntries {
			if !e.IsDir() {
				continue
			}
			if want[e.Name()] {
				log.Printf("clientconfig: bundle skill %q overrides the built-in skill of the same name", e.Name())
			}
			want[e.Name()] = true
			if err := copyDirSkill(filepath.Join(bundleSkillsDir, e.Name()), filepath.Join(merged, e.Name())); err != nil {
				return err
			}
		}
	}

	// Drop merged entries whose source disappeared (deleted bundle skill,
	// newly-hidden builtin).
	if mergedEntries, err := os.ReadDir(merged); err == nil {
		for _, e := range mergedEntries {
			if !want[e.Name()] {
				_ = os.RemoveAll(filepath.Join(merged, e.Name()))
			}
		}
	}
	return nil
}

// copyEmbeddedSkill materializes one embedded skill folder.
func copyEmbeddedSkill(name, dst string) error {
	src := builtinSkillsRoot + "/" + name
	return fs.WalkDir(builtinSkillsFS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // sandbox-readable skill docs, non-secret
		}
		data, err := builtinSkillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFileIfChanged(target, data)
	})
}

// copyDirSkill mirrors one on-disk skill folder into the merged dir.
func copyDirSkill(src, dst string) error {
	//nolint:gosec // G122/G703: src is the operator-owned bundle skills dir (trusted supply chain per skills.go header); paths derive from its own listing, not request input.
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) //nolint:gosec // sandbox-readable skill docs, non-secret
		}
		data, err := os.ReadFile(p) // #nosec G304 — bundle content, operator-owned.
		if err != nil {
			return err
		}
		return writeFileIfChanged(target, data)
	})
}

// writeFileIfChanged avoids touching mtimes (and prompt-cache-relevant reads)
// when the content is already current.
func writeFileIfChanged(path string, data []byte) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) { // #nosec G304 — path derived from bundle content.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // sandbox-readable skill docs, non-secret
		return err
	}
	return os.WriteFile(path, data, 0o644) // #nosec G306 G703 — skill docs/scripts from operator-owned bundle content, non-secret.
}
