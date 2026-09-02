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
//
// On the kubernetes sandbox backend the data dir is invisible to sandbox
// pods, so cmd/fleet re-stages the SAME merged tree into
// <workspace root>/skills — inside the one volume every pod mounts — via
// StageSkillsAt (ADR-0055). Because a sandbox can write to the workspace
// volume before that path is mounted read-only over it, every write below is
// symlink-safe (temp file + rename, never through an existing entry) and a
// pre-planted staged root is replaced rather than adopted.

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
//
// overlays are the Agent Plugin skill contributions (plugins.go), applied
// between the built-in pack and the bundle's own skills. A bundle that opts
// out of the built-in pack still gets a merged tree when any plugin is loaded
// (one overlay per plugin, skills or not yet): the tree is the only way plugin
// skills reach the one skills/ mount, and it must already exist for a skill
// folder added after boot to be picked up on read.
func materializeMergedSkills(bundleSkillsDir string, builtinEnabled bool, hidden []string, overlays []skillOverlay) (string, error) {
	if !builtinEnabled && len(overlays) == 0 {
		return bundleSkillsDir, nil
	}
	base := mergedSkillsBase()
	if err := ensureTrustedDir(base); err != nil {
		return bundleSkillsDir, fmt.Errorf("merged-skills root: %w", err)
	}
	sum := sha256.Sum256([]byte(bundleSkillsDir))
	merged := filepath.Join(base, hex.EncodeToString(sum[:6]))
	if err := syncMergedSkills(bundleSkillsDir, merged, builtinEnabled, hidden, overlays); err != nil {
		return bundleSkillsDir, err
	}
	return merged, nil
}

// mergedSkillsBase is the parent of per-bundle merged trees. Prefer the
// operator's data dir (FLEET_DATA_DIR / CHAT_DATA_DIR) so the tree lives
// next to the rest of fleet state, not under world-writable /tmp. Fall
// back to the user cache, then a uid-scoped temp name — still never the
// predictable shared /tmp/fleet-skills path (#1121).
func mergedSkillsBase() string { return fleetStateBase(mergedSkillsDirName) }

// fleetStateBase resolves <state>/<sub> for the small on-box trees this
// package owns (the merged skills tree, the Agent Plugins' PLUGIN_DATA dirs):
// the operator's data dir first, then the user cache, then a uid-scoped temp
// name — never a predictable shared /tmp path (#1121).
func fleetStateBase(sub string) string {
	if d := strings.TrimSpace(os.Getenv("FLEET_DATA_DIR")); d != "" {
		return filepath.Join(d, sub)
	}
	if d := strings.TrimSpace(os.Getenv("CHAT_DATA_DIR")); d != "" {
		return filepath.Join(d, sub)
	}
	if cache, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cache) != "" {
		return filepath.Join(cache, "fleet", sub)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("fleet-%s-%d", sub, os.Geteuid()))
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
// materialized under the DATA dir (`<data|cache>/skills-merged/<hash>`) rather
// than a bundle's own `skills/` or a tree staged by StageSkillsAt. It exists
// for one caller: the kubernetes sandbox backend, where a supporting-doc dir
// is only readable in a sandbox if a pod can actually see it — and a data-dir
// merged tree lives where no pod mounts and no sandbox image can plausibly
// reproduce (its hash is derived from the bundle path, and the tree is rebuilt
// at boot). That backend normally never reaches this case: it stages the tree
// into the workspace claim at boot (StageSkillsAt, ADR-0055). The check
// remains for the degraded path where staging failed, so the mount is dropped
// and the log says why instead of trusting an anchor nothing mounted.
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

// StageSkillsAt re-materializes the bundle's COMPLETE skills tree — the
// built-in pack (per skills_builtin / skills_hidden), every Agent Plugin's
// skills, and the bundle's own skills/ — into dir, an exact operator-derived
// path, and points SkillsDir at it. Skills() keeps resyncing that tree on read,
// so the edit-a-skill-in-place and plugin-folder-follows-the-disk contracts
// hold there exactly as they do for the data-dir tree.
//
// The one caller is the kubernetes sandbox backend (ADR-0055): sandbox pods
// mount nothing but the workspace claim, so the tree is staged at
// <workspace root>/skills, where every pod sees it by construction and the pod
// spec re-mounts it read-only over the read-write workspace mount — the same
// shape as the shared file library's staged tree (docs/SHARED-FILES.md). It
// runs on EVERY kubernetes boot, built-in pack or not: staging is what makes
// `skills/<name>/SKILL.md` resolve in a pod without baking skills into the
// sandbox image, and it is what lets plugin skills and the built-in pack reach
// a pod at all.
//
// dir lives in a volume sandboxes can write to until the read-only mount is in
// place, so it is never adopted blindly: a pre-existing path that is not a
// plain directory owned by this process (a planted symlink, a world-writable
// dir) is removed and rebuilt, and every file write inside is symlink-safe.
// An error leaves SkillsDir untouched; the caller degrades loudly (skills are
// a capability, not a boot invariant).
func (b *Bundle) StageSkillsAt(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("stage skills: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	if b.BundleSkillsDir == "" {
		b.BundleSkillsDir = b.SkillsDir
	}
	if filepath.Clean(abs) == filepath.Clean(b.BundleSkillsDir) {
		return fmt.Errorf("stage skills: %s is the bundle's own skills/ dir", abs)
	}
	if err := ensureStagedRoot(abs); err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	if err := syncMergedSkills(b.BundleSkillsDir, abs, b.skillsBuiltin, b.skillsHidden, b.livePluginSkillOverlays()); err != nil {
		return fmt.Errorf("stage skills into %s: %w", abs, err)
	}
	b.SkillsDir = abs
	return nil
}

// ensureStagedRoot prepares a staged skills root that lives in a
// sandbox-writable volume. Unlike the data-dir tree (#1121 refuses an
// untrusted path and degrades), nothing but fleet legitimately owns this
// path, so an untrusted pre-existing entry — a symlink planted by a turn
// before the read-only mount existed, a stray file, a group/world-writable
// dir — is REMOVED and the root rebuilt clean, loudly. os.RemoveAll on a
// symlink removes the link, never what it points at.
func ensureStagedRoot(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // the workspace root is sandbox-readable by design
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		if verr := verifyExistingDir(dir); verr != nil {
			log.Printf("clientconfig: staged skills root %s is not a trusted directory (%v); replacing it", dir, verr)
			if rerr := os.RemoveAll(dir); rerr != nil {
				return fmt.Errorf("replace untrusted staged skills root: %w", rerr)
			}
		}
	}
	return ensureTrustedDir(dir)
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

// syncMergedSkills rebuilds merged from its sources, lowest precedence
// first so a later copy overwrites an earlier one: the built-in pack (when
// enabled, minus hidden), then each Agent Plugin's skills (first plugin by
// name wins a collision between plugins), then the bundle's own skills/
// (the bundle author always wins). Files are written in place only when
// their content changed; stale entries from a prior sync are removed.
func syncMergedSkills(bundleSkillsDir, merged string, builtinEnabled bool, hidden []string, overlays []skillOverlay) error {
	hiddenSet := map[string]bool{}
	for _, h := range hidden {
		hiddenSet[strings.TrimSpace(h)] = true
	}

	if err := ensureTrustedDir(merged); err != nil {
		return err
	}

	want := map[string]bool{}

	// Built-in pack first, so a plugin or bundle skill with the same name
	// overwrites it.
	if builtinEnabled {
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
	}

	// Agent Plugin overlays (plugins.go): only the skill folders the plugin
	// loader validated, each copied with containment against its plugin root.
	pluginOwned := map[string]string{}
	for _, ov := range overlays {
		for _, name := range ov.Names {
			if prev, dup := pluginOwned[name]; dup {
				log.Printf("clientconfig: plugin %q skill %q is shadowed by plugin %q's skill of the same name (first plugin wins)", ov.Plugin, name, prev)
				continue
			}
			if want[name] {
				log.Printf("clientconfig: plugin %q skill %q overrides the built-in skill of the same name", ov.Plugin, name)
			}
			pluginOwned[name] = ov.Plugin
			want[name] = true
			if err := copyDirSkill(filepath.Join(ov.SkillsDir, name), filepath.Join(merged, name), ov.Root); err != nil {
				return err
			}
		}
	}

	// Bundle skills overlay.
	if bundleEntries, err := os.ReadDir(bundleSkillsDir); err == nil {
		for _, e := range bundleEntries {
			if !e.IsDir() {
				continue
			}
			if p, fromPlugin := pluginOwned[e.Name()]; fromPlugin {
				log.Printf("clientconfig: bundle skill %q overrides plugin %q's skill of the same name", e.Name(), p)
			} else if want[e.Name()] {
				log.Printf("clientconfig: bundle skill %q overrides the built-in skill of the same name", e.Name())
			}
			want[e.Name()] = true
			if err := copyDirSkill(filepath.Join(bundleSkillsDir, e.Name()), filepath.Join(merged, e.Name()), ""); err != nil {
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
			return ensureRealDir(target)
		}
		data, err := builtinSkillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFileIfChanged(target, data)
	})
}

// copyDirSkill mirrors one on-disk skill folder into the merged dir. When
// containRoot is set (an Agent Plugin skill), every file is resolved through
// symlinks first and skipped — loudly — unless it stays under that root, per
// the Agent Plugins package-boundary rule (spec §4.1); a symlink to a
// directory is likewise not followed. The bundle's own skills pass "" and
// keep the historical trusted-supply-chain behavior.
func copyDirSkill(src, dst, containRoot string) error {
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
			return ensureRealDir(target)
		}
		if containRoot != "" {
			resolved, ok := containedRegularFile(p, containRoot)
			if !ok {
				log.Printf("clientconfig: plugin skill file %s skipped: not a regular file inside the plugin root %s", p, containRoot)
				return nil
			}
			p = resolved
		}
		data, err := os.ReadFile(p) // #nosec G304 — bundle content, operator-owned.
		if err != nil {
			return err
		}
		return writeFileIfChanged(target, data)
	})
}

// ensureRealDir makes target a plain directory. An existing entry that is NOT
// a directory — a symlink (Lstat does not follow it), a regular file — is
// removed first, so a planted symlink can never redirect the files written
// beneath it. Matters for a tree staged in a sandbox-writable volume
// (StageSkillsAt); harmless for the data-dir tree.
func ensureRealDir(target string) error {
	if info, err := os.Lstat(target); err == nil {
		if info.IsDir() {
			return nil
		}
		if rerr := os.RemoveAll(target); rerr != nil {
			return rerr
		}
	}
	return os.MkdirAll(target, 0o755) //nolint:gosec // sandbox-readable skill docs, non-secret
}

// writeFileIfChanged avoids touching mtimes (and prompt-cache-relevant reads)
// when the content is already current, and never writes THROUGH an existing
// entry: the bytes land in a temp file beside the target and are renamed over
// it. Rename replaces the directory entry itself, so a symlink planted at the
// path (in a sandbox-writable staged tree) is replaced, not followed, and a
// hard link to some other file is unlinked, not truncated. Readers in a
// sandbox see the old or the new file, never a torn one.
func writeFileIfChanged(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().IsRegular() {
			if cur, rerr := os.ReadFile(path); rerr == nil && string(cur) == string(data) { // #nosec G304 — path derived from bundle content.
				return nil
			}
		} else if rerr := os.RemoveAll(path); rerr != nil {
			// A symlink, directory or device where a skill file belongs.
			return rerr
		}
	}
	dir := filepath.Dir(path)
	if err := ensureRealDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".skill-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // skill docs/scripts from operator-owned bundle content, non-secret, read by the sandbox uid
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
