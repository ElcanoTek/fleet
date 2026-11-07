// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"strings"
	"testing"
)

// TestReleasesAreImmutable — a re-run must never rebuild a published tag:
// different bytes under the same tag would overwrite what boxes verified.
func TestReleasesAreImmutable(t *testing.T) {
	raw, jobs := releaseWorkflow(t)
	tagRuns := allRuns(jobs["tag"])
	for _, want := range []string{
		`gh release view "${TAG}" --json isDraft,assets`,
		`echo "build=false"`,
		`echo "build=true"`,
		`skopeo inspect --raw "docker://ghcr.io/elcanotek/${name}:${ref}"`,
	} {
		if !strings.Contains(tagRuns, want) {
			t.Errorf("the tag job's immutability gate no longer runs %q", want)
		}
	}
	if !strings.Contains(raw, "build: ${{ steps.state.outputs.build }}") {
		t.Error("the tag job must expose the gate's verdict as an output")
	}
	if n := strings.Count(raw, "needs.tag.outputs.build == 'true'"); n != 2 {
		t.Errorf("build and publish must both be gated on build == 'true' (found %d)", n)
	}
	pub := allRuns(jobs["publish"])
	if strings.Contains(pub, "--clobber") || strings.Contains(pub, "gh release upload") {
		t.Error("the publish job overwrites release assets; releases are immutable")
	}
	if !strings.Contains(pub, "appeared since the tag job checked; refusing to overwrite it") {
		t.Error("the publish job must re-check GHCR before pushing")
	}
}

// TestEveryBuiltTagPublishes — overlapping runs: an older tag that a newer one
// overtook still publishes (not as Latest); skipping it would strand signed
// images with no release naming them.
func TestEveryBuiltTagPublishes(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	pub := allRuns(jobs["publish"])
	if strings.Contains(pub, "not publishing a release for it") {
		t.Error("the publish job still skips a superseded tag")
	}
	for _, want := range []string{`latest_flag="--latest=false"`, `[ "${TAG}" = "${newest}" ] && latest_flag="--latest"`, `"${latest_flag}"`} {
		if !strings.Contains(pub, want) {
			t.Errorf("publish job missing %q — Latest must follow the newest tag, not gate publication", want)
		}
	}
}
