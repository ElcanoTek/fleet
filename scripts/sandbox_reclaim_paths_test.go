// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"path/filepath"
	"strings"
	"testing"
)

const reclaimPrelude = `
warn() { printf 'WARN %s\n' "$*"; }
ok() { printf 'OK %s\n' "$*"; }
SANDBOX_RECLAIM_FILE="$LIST"
`

func (s *reclaimStore) env(extra ...string) []string {
	return append([]string{"LIST=" + s.list, "LOG=" + s.log, "STORE=" + s.store, "INUSE=" + s.inUse}, extra...)
}

// TestUpdateReclaimsRenamedTagOnReleaseBump — the published path's own
// lines: when sandbox.tag is renamed in the same update that pulls a new
// release, the previous image carries both the old digest name and the OLD
// tag. Dropping only the digest would untag it; the old tag is queued too, so
// the image is actually removed. The new ref and tag are never touched.
func TestUpdateReclaimsRenamedTagOnReleaseBump(t *testing.T) {
	const (
		repo    = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		pubPrev = repo + "aaaa000000000000000000000000000000000000000000000000000000000000"
		pubRef  = repo + "bbbb000000000000000000000000000000000000000000000000000000000000"
		oldTag  = "localhost/fleet-sandbox:latest"
		newTag  = "localhost/acme-sandbox:latest"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	block := updateBlock(t, update, "      sandbox_superseded=\"\"\n      [[ -n \"$pub_prev\"",
		"      sandbox_podman image prune -f >/dev/null 2>&1 || true")

	run := func(refPrev string) *reclaimStore {
		t.Helper()
		s := newReclaimStore(t, map[string]string{pubPrev: "id-old", oldTag: "id-old", pubRef: "id-new", newTag: "id-new"})
		libBash(t, reclaimStub+reclaimPrelude+`pub_prev="$PP"; pub_ref="$PR"; ref_prev="$RP"; ref_now="$RN"; sandbox_image_ref=""
`+block, s.env("PP="+pubPrev, "PR="+pubRef, "RP="+refPrev, "RN="+newTag)...)
		return s
	}

	s := run(oldTag)
	if s.has(pubPrev) || s.has(oldTag) {
		t.Errorf("renamed tag + release bump: old image still named (rmi calls %q)", s.rmis())
	}
	if !s.has(pubRef) || !s.has(newTag) {
		t.Error("the new release image was touched")
	}
	if got := s.pending(); got != nil {
		t.Errorf("pending = %q, want none", got)
	}

	// Tag unchanged: the tag now names the NEW image and is never queued.
	s = run(newTag)
	for _, c := range s.rmis() {
		if c != "rmi "+pubPrev {
			t.Errorf("unexpected rmi %q with an unchanged tag", c)
		}
	}

	// A tag entry is honoured only in its tag: form, and never when it is
	// protected or names a protected image.
	s = newReclaimStore(t, map[string]string{oldTag: "id-old", newTag: "id-new"})
	writeTestFile(t, s.list, oldTag+"\ntag:"+newTag+"\n")
	s.run("", pubRef, newTag)
	if got := s.rmis(); len(got) != 0 {
		t.Errorf("rmi ran on a bare tag line or the protected tag: %q", got)
	}
	if got := s.pending(); len(got) != 1 || got[0] != "tag:"+newTag {
		t.Errorf("pending = %q, want [tag:%s] (protected: kept, skipped)", got, newTag)
	}
}

// TestUpdatePostRestartReclaimProtectsTheTagOnlyWhenUsed — after the
// restart, ref_now is protected only when the sandbox resolves through
// sandbox.tag. With a prebuilt sandbox.image the tag is a leftover: a pending
// release digest sharing its image ID must still be removed (the prebuilt
// image itself stays protected).
func TestUpdatePostRestartReclaimProtectsTheTagOnlyWhenUsed(t *testing.T) {
	const (
		repo     = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		oldRel   = repo + "cccc000000000000000000000000000000000000000000000000000000000000"
		tag      = "localhost/fleet-sandbox:latest"
		prebuilt = "registry.example/sandbox@sha256:dddd000000000000000000000000000000000000000000000000000000000000"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	block := updateBlock(t, update, "      sandbox_current=\"\"\n      if [[ \"$sandbox_published\"",
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "" "$sandbox_current" "$sandbox_current_tag" "$sandbox_base" "$sandbox_image_ref"`)

	run := func(imageRef string) *reclaimStore {
		t.Helper()
		s := newReclaimStore(t, map[string]string{oldRel: "id-rel", tag: "id-rel", prebuilt: "id-pre"})
		writeTestFile(t, s.list, oldRel+"\n")
		digest := filepath.Join(t.TempDir(), "sandbox-image.digest")
		writeTestFile(t, digest, oldRel+"\n")
		libBash(t, reclaimStub+reclaimPrelude+`sandbox_published=1; SANDBOX_DIGEST_FILE="$DF"; ref_now="$TAG"; sandbox_base=""; sandbox_image_ref="$IMG"
`+block, s.env("DF="+digest, "TAG="+tag, "IMG="+imageRef)...)
		return s
	}

	// Prebuilt override: the tag (and the stale digest file) are not
	// protected, so the old release digest goes.
	s := run(prebuilt)
	if got := s.rmis(); len(got) != 1 || got[0] != "rmi "+oldRel {
		t.Errorf("with a prebuilt sandbox.image: rmi calls %q, want [rmi %s]", got, oldRel)
	}
	if !s.has(prebuilt) {
		t.Error("the prebuilt sandbox.image was removed")
	}
	if got := s.pending(); got != nil {
		t.Errorf("pending = %q, want none", got)
	}

	// Tag path: that digest is what the tag runs — protected, kept listed.
	s = run("")
	if got := s.rmis(); len(got) != 0 {
		t.Errorf("on the tag path the current image was removed: %q", got)
	}
	if got := s.pending(); strings.Join(got, " ") != oldRel {
		t.Errorf("pending = %q, want [%s]", got, oldRel)
	}
}
