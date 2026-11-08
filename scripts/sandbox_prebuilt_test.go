// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateQueuesReleaseImageOnSwitchToPrebuilt — a box that ran the
// release image and now resolves a prebuilt sandbox.image: step 3 queues the
// old digest ref and its old tag (without removing anything while the old
// service may still run on them) and clears the digest file so they are
// queued once; the post-restart retry then removes them, with the prebuilt
// image — and anything sharing its ID — protected.
func TestUpdateQueuesReleaseImageOnSwitchToPrebuilt(t *testing.T) {
	const (
		repo     = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		oldRel   = repo + "eeee000000000000000000000000000000000000000000000000000000000000"
		tag      = "localhost/fleet-sandbox:latest"
		prebuilt = "registry.example/sandbox@sha256:ffff000000000000000000000000000000000000000000000000000000000000"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	queue := leftoverBlock(t, update)
	retry := updateBlock(t, update, "      sandbox_current=\"\"\n      if [[ \"$sandbox_published\"",
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "" "$sandbox_current" "$sandbox_current_tag" "$sandbox_base" "$sandbox_image_ref"`)
	if !strings.Contains(update, `if [[ -z "$sandbox_image_ref" ]] && fleet_sandbox_uses_release_image`) {
		t.Error("update.sh must not ask fleet_sandbox_uses_release_image when a prebuilt image is set")
	}

	run := func(s *reclaimStore) {
		t.Helper()
		state := filepath.Dir(s.list)
		digest := filepath.Join(state, "sandbox-image.digest")
		writeTestFile(t, digest, oldRel+"\n")
		writeTestFile(t, filepath.Join(state, "sandbox-image.ref"), tag+"\n")
		prelude := reclaimStub + reclaimPrelude + leftoverPrelude + `ref_prev="$TAG"; ref_now="$TAG"; sandbox_image_ref="$IMG"
sandbox_published=0; sandbox_base=""; sandbox_cf=/nonexistent/Containerfile; cf_now=prebuilt
`
		env := s.env("ST="+state, "DF="+digest, "TAG="+tag, "IMG="+prebuilt)
		// Step 3 queues; nothing is removed yet.
		libBash(t, prelude+queue, env...)
		if got := strings.Join(s.pending(), " "); got != oldRel+" tag:"+tag {
			t.Fatalf("queued %q, want [%s tag:%s]", got, oldRel, tag)
		}
		if len(s.rmis()) != 0 {
			t.Fatalf("step 3 removed something: %q", s.rmis())
		}
		if _, err := os.Stat(digest); !os.IsNotExist(err) {
			t.Fatalf("the digest file was not cleared (%v)", err)
		}
		// A second update: nothing is queued again.
		libBash(t, prelude+queue, env...)
		if got := strings.Join(s.pending(), " "); got != oldRel+" tag:"+tag {
			t.Fatalf("queued twice: %q", got)
		}
		// After the restart.
		libBash(t, prelude+retry, env...)
	}

	s := newReclaimStore(t, map[string]string{oldRel: "id-rel", tag: "id-rel", prebuilt: "id-pre"})
	run(s)
	if s.has(oldRel) || s.has(tag) {
		t.Errorf("the old release image was not reclaimed after the restart: rmi %q", s.rmis())
	}
	if !s.has(prebuilt) {
		t.Error("the prebuilt image was removed")
	}
	if got := s.pending(); got != nil {
		t.Errorf("pending = %q, want none", got)
	}

	// The prebuilt image IS the old release image under another name: both
	// queued names are protected by ID — kept listed, nothing removed.
	s = newReclaimStore(t, map[string]string{oldRel: "id-same", tag: "id-same", prebuilt: "id-same"})
	run(s)
	if len(s.rmis()) != 0 {
		t.Errorf("removed a name of the running prebuilt image: %q", s.rmis())
	}
	if got := strings.Join(s.pending(), " "); got != oldRel+" tag:"+tag {
		t.Errorf("pending = %q, want both kept", got)
	}
}

// TestUpdateSkipsContextHashForPrebuiltImage — with a prebuilt sandbox.image
// nothing reads the build-context hash, so update.sh never computes it.
func TestUpdateSkipsContextHashForPrebuiltImage(t *testing.T) {
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	snippet := updateStampSnippet(t, update)
	if i, j := strings.Index(update, snippet), strings.Index(update, `sandbox_image_ref="$(resolve_sandbox_image`); i < j {
		t.Error("the context hash is computed before sandbox.image is resolved")
	}
	marker := filepath.Join(t.TempDir(), "called")
	stub := `fleet_sandbox_context_hash() { : > "$MARK"; printf 'ctx4-x'; }
STAMP_FILE=/nonexistent; sandbox_cf=/nonexistent/Containerfile
`
	out := libBash(t, stub+`sandbox_image_ref="registry.example/sandbox:1"
`+snippet+`printf 'CF_NOW=%s\n' "$cf_now"`, "MARK="+marker)
	if !strings.Contains(out, "CF_NOW=prebuilt") {
		t.Errorf("cf_now with a prebuilt image: %s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the context was hashed although a prebuilt sandbox.image is set")
	}
	out = libBash(t, stub+`sandbox_image_ref=""
`+snippet+`printf 'CF_NOW=%s\n' "$cf_now"`, "MARK="+marker)
	if !strings.Contains(out, "CF_NOW=ctx4-x") {
		t.Errorf("cf_now without a prebuilt image: %s", out)
	}
}

// TestSandboxContextHashFollowsASymlinkedContainerfile — the build reads the
// bytes a symlinked Containerfile resolves to, inside the context or not, so
// editing the target changes the hash; a link that does not resolve to a
// readable file fails closed (and never counts as the release image).
func TestSandboxContextHashFollowsASymlinkedContainerfile(t *testing.T) {
	for _, where := range []string{"outside", "inside"} {
		t.Run(where, func(t *testing.T) {
			root := t.TempDir()
			cf := genericSandboxContext(t, root, "sandbox")
			target := filepath.Join(root, "shared", "Containerfile")
			if where == "inside" {
				target = filepath.Join(filepath.Dir(cf), "real.Containerfile")
			}
			writeTestFile(t, target, "FROM registry.invalid/base:1\n")
			if err := os.Remove(cf); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, cf); err != nil {
				t.Fatal(err)
			}
			before, ok, out := tryContextHash(t, cf)
			if !ok {
				t.Fatalf("symlinked Containerfile did not hash:\n%s", out)
			}
			writeTestFile(t, target, "FROM registry.invalid/base:2\n")
			after, ok, out := tryContextHash(t, cf)
			if !ok || after == before {
				t.Errorf("editing the symlink target left the hash unchanged (%s → %s):\n%s", before, after, out)
			}
			// Unreadable target: fail closed.
			writeTestFile(t, target, "UNREADABLE\n")
			if _, ok, out := tryContextHash(t, cf); ok || !strings.Contains(out, target) {
				t.Errorf("an unreadable symlink target still hashed, or was not named:\n%s", out)
			}
			// Dangling: fail closed, not "absent".
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if h, ok, out := tryContextHash(t, cf); ok {
				t.Errorf("a dangling Containerfile symlink hashed to %q:\n%s", h, out)
			}
		})
	}

	// A dangling symlink at the default path is a custom (broken) bundle —
	// never "ships no Containerfile, use the release image".
	bundle := sandboxBundle(t, "", map[string]string{})
	if err := os.MkdirAll(filepath.Join(bundle, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bundle, "missing"), filepath.Join(bundle, "sandbox", "Containerfile")); err != nil {
		t.Fatal(err)
	}
	if usesReleaseImage(t, bundle) {
		t.Error("a dangling Containerfile symlink counted as the release image")
	}
}
