// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// leftoverBlock returns update.sh's "leaving a sandbox mode" block, run
// verbatim by the transition tests.
func leftoverBlock(t *testing.T, update string) string {
	t.Helper()
	return updateBlock(t, update, "sandbox_mode=custom\n", `sandbox_queue_leftover "$REF_FILE" "tag:"`) + "  fi\nfi\n"
}

// leftoverPrelude is the state the block reads: the state files under $ST,
// the reclaim list at $LIST, and info/warn.
const leftoverPrelude = `info() { printf 'INFO %s\n' "$*"; }
DRY_RUN="${DRY:-0}"
SANDBOX_DIGEST_FILE="$ST/sandbox-image.digest"; SANDBOX_BASE_FILE="$ST/sandbox-base.ref"; REF_FILE="$ST/sandbox-image.ref"
`

// TestUpdateQueuesLeftoversOnEveryModeTransition — whenever an update stops
// using a sandbox mode, the images that mode persisted are queued for
// reclaim exactly once (the file naming them cleared): the published digest
// when leaving the published path, the FLEET_SANDBOX_BASE digest when no
// longer layering on a base, and the tag when moving to a prebuilt
// sandbox.image. Staying in a mode queues nothing; a dry run changes nothing.
func TestUpdateQueuesLeftoversOnEveryModeTransition(t *testing.T) {
	const (
		repo = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		dig  = repo + "1010000000000000000000000000000000000000000000000000000000000000"
		base = repo + "2020000000000000000000000000000000000000000000000000000000000000"
		tag  = "localhost/fleet-sandbox:latest"
		img  = "registry.example/sandbox:1"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	block := leftoverBlock(t, update)

	type state struct{ digest, base, ref string }
	published := state{digest: dig, ref: tag}
	customNoBase := state{ref: tag}
	customBase := state{base: base, ref: tag}
	prebuiltState := state{}
	type to struct {
		name      string
		published bool   // sandbox_published
		image     string // sandbox_image_ref
		withBase  bool   // the Containerfile declares FLEET_SANDBOX_BASE
		cfAbsent  bool
	}
	toPublished := to{name: "published", published: true}
	toCustom := to{name: "custom"}
	toCustomBase := to{name: "custom+base", withBase: true}
	toPrebuilt := to{name: "prebuilt", image: img}

	cases := []struct {
		from   string
		st     state
		to     to
		queued []string
		left   state // the state files still present afterwards
		dry    bool
	}{
		{"published", published, toPublished, nil, published, false},
		{"published", published, toCustom, []string{dig}, state{ref: tag}, false},
		{"published", published, toCustomBase, []string{dig}, state{ref: tag}, false},
		{"published", published, toPrebuilt, []string{dig, "tag:" + tag}, state{}, false},
		{"custom", customNoBase, toPublished, nil, customNoBase, false},
		{"custom", customNoBase, toCustom, nil, customNoBase, false},
		{"custom", customNoBase, toCustomBase, nil, customNoBase, false},
		{"custom", customNoBase, toPrebuilt, []string{"tag:" + tag}, state{}, false},
		{"custom+base", customBase, toPublished, []string{base}, customNoBase, false},
		{"custom+base", customBase, toCustom, []string{base}, customNoBase, false},
		{"custom+base", customBase, toCustomBase, nil, customBase, false},
		{"custom+base", customBase, toPrebuilt, []string{base, "tag:" + tag}, state{}, false},
		{"prebuilt", prebuiltState, toPublished, nil, state{}, false},
		{"prebuilt", prebuiltState, toCustom, nil, state{}, false},
		{"prebuilt", prebuiltState, toCustomBase, nil, state{}, false},
		{"prebuilt", prebuiltState, toPrebuilt, nil, state{}, false},
		// A dry run only says what it would queue.
		{"published", published, toPrebuilt, nil, published, true},
		// A bundle naming a missing Containerfile: step 3 stops the update.
		{"published", published, to{name: "custom (missing Containerfile)", cfAbsent: true}, nil, published, false},
	}
	for _, c := range cases {
		name := c.from + " → " + c.to.name
		if c.dry {
			name += " (dry run)"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			st := filepath.Join(dir, "state")
			list := filepath.Join(st, "sandbox-image.reclaim")
			write := func(file, v string) {
				if v != "" {
					writeTestFile(t, filepath.Join(st, file), v+"\n")
				}
			}
			write("sandbox-image.digest", c.st.digest)
			write("sandbox-base.ref", c.st.base)
			write("sandbox-image.ref", c.st.ref)
			cf := filepath.Join(dir, "bundle", "sandbox", "Containerfile")
			body := "FROM registry.invalid/base\n"
			if c.to.withBase {
				body = "ARG FLEET_SANDBOX_BASE\nFROM ${FLEET_SANDBOX_BASE}\n"
			}
			writeTestFile(t, cf, body)
			cfNow := "ctx4-x"
			if c.to.cfAbsent {
				cfNow = "absent"
			}
			pub := "0"
			if c.to.published {
				pub = "1"
			}
			dry := "0"
			if c.dry {
				dry = "1"
			}
			out := libBash(t, leftoverPrelude+`SANDBOX_RECLAIM_FILE="$LIST"; sandbox_image_ref="$IMG"; sandbox_published="$PUB"; sandbox_cf="$CF"; cf_now="$CFNOW"
`+block, "ST="+st, "LIST="+list, "IMG="+c.to.image, "PUB="+pub, "CF="+cf, "CFNOW="+cfNow, "DRY="+dry)

			var got []string
			if b, err := os.ReadFile(list); err == nil {
				got = strings.Fields(string(b))
			}
			if strings.Join(got, " ") != strings.Join(c.queued, " ") {
				t.Errorf("queued %q, want %q\n%s", got, c.queued, out)
			}
			read := func(file string) string {
				b, err := os.ReadFile(filepath.Join(st, file))
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(b))
			}
			if left := (state{read("sandbox-image.digest"), read("sandbox-base.ref"), read("sandbox-image.ref")}); left != c.left {
				t.Errorf("state files afterwards %+v, want %+v", left, c.left)
			}
			if c.dry && !strings.Contains(out, "[dry-run] would queue "+dig) {
				t.Errorf("the dry run does not say what it would queue:\n%s", out)
			}

			// Run again on the result: a leftover is queued only once.
			libBash(t, leftoverPrelude+`SANDBOX_RECLAIM_FILE="$LIST"; sandbox_image_ref="$IMG"; sandbox_published="$PUB"; sandbox_cf="$CF"; cf_now="$CFNOW"
`+block, "ST="+st, "LIST="+list, "IMG="+c.to.image, "PUB="+pub, "CF="+cf, "CFNOW="+cfNow, "DRY="+dry)
			var again []string
			if b, err := os.ReadFile(list); err == nil {
				again = strings.Fields(string(b))
			}
			if !c.dry && strings.Join(again, " ") != strings.Join(c.queued, " ") {
				t.Errorf("second run queued again: %q", again)
			}
		})
	}
}

// TestUpdateReclaimsReleaseImageOnSwitchToCustomBuild — published → custom
// Containerfile without FLEET_SANDBOX_BASE: the old release digest is queued
// at step 3 and removed by the custom build's own reclaim call (the tag now
// names the new bundle image, which stays).
func TestUpdateReclaimsReleaseImageOnSwitchToCustomBuild(t *testing.T) {
	const (
		dig = "ghcr.io/elcanotek/fleet-sandbox@sha256:3030000000000000000000000000000000000000000000000000000000000000"
		tag = "localhost/fleet-sandbox:latest"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	block := leftoverBlock(t, update)
	record := updateBlock(t, update, "      mkdir -p \"$STATE_DIR\"\n      printf '%s\\n' \"$sandbox_base\" > \"$SANDBOX_BASE_FILE\"",
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "$sandbox_superseded" "$sandbox_base" "$ref_now" "$sandbox_image_ref"`)
	// After the build: the tag names the new bundle image; the release image
	// is left with only its digest name.
	s := newReclaimStore(t, map[string]string{dig: "id-rel", tag: "id-bundle"})
	st := filepath.Dir(s.list)
	writeTestFile(t, filepath.Join(st, "sandbox-image.digest"), dig+"\n")
	writeTestFile(t, filepath.Join(st, "sandbox-image.ref"), tag+"\n")
	cf := filepath.Join(t.TempDir(), "Containerfile")
	writeTestFile(t, cf, "FROM registry.invalid/base\n")
	libBash(t, reclaimStub+reclaimPrelude+leftoverPrelude+`STATE_DIR="$ST"; STAMP_FILE="$ST/sandbox-containerfile.sha256"
sandbox_image_ref=""; sandbox_published=0; sandbox_cf="$CF"; cf_now=ctx4-x
ref_now="$TAG"; ref_prev="$TAG"; sandbox_base=""; base_prev=""
`+block+record, s.env("ST="+st, "CF="+cf, "TAG="+tag)...)
	if s.has(dig) {
		t.Errorf("the old release image was not reclaimed: rmi %q, pending %q", s.rmis(), s.pending())
	}
	if !s.has(tag) {
		t.Error("the new bundle image was removed")
	}
	if got := s.pending(); got != nil {
		t.Errorf("pending = %q, want none", got)
	}
}

// TestNoScriptAsksForTheReleaseImageWithAPrebuiltImage — every caller of
// fleet_sandbox_uses_release_image skips it when sandbox.image is set (it
// would hash two build contexts for a decision the prebuilt image already
// made), and bootstrap.sh's guard is run here with the helper stubbed.
func TestNoScriptAsksForTheReleaseImageWithAPrebuiltImage(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "scripts", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`(^|[^_a-z])fleet_sandbox_uses_release_image `)
	guard := regexp.MustCompile(`\[\[ -z "\$(sandbox_image_ref|SANDBOX_IMAGE_REF)" \]\] && fleet_sandbox_uses_release_image `)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, ln := range strings.Split(string(b), "\n") {
			code := strings.TrimSpace(ln)
			if strings.HasPrefix(code, "#") || !call.MatchString(code) {
				continue
			}
			if !guard.MatchString(code) {
				t.Errorf("%s:%d calls fleet_sandbox_uses_release_image without the prebuilt-image guard: %s", filepath.Base(f), i+1, code)
			}
		}
		if filepath.Base(f) != "update.sh" && strings.Contains(string(b), "fleet_sandbox_context_hash \"") {
			t.Errorf("%s hashes a build context directly; only update.sh (guarded) may", filepath.Base(f))
		}
	}

	bootstrap := readFile(t, root, "scripts/bootstrap.sh")
	block := updateBlock(t, bootstrap, "sandbox_published=0\n", "  sandbox_published=1") + "fi\n"
	marker := filepath.Join(t.TempDir(), "called")
	stub := `fleet_sandbox_uses_release_image() { : > "$MARK"; return 0; }
CLIENT_CONFIG_DIR=/nonexistent; REPO_ROOT=/nonexistent
`
	out := libBash(t, stub+`SANDBOX_IMAGE_REF="registry.example/sandbox:1"
`+block+`printf 'PUB=%s\n' "$sandbox_published"`, "MARK="+marker)
	if !strings.Contains(out, "PUB=0") {
		t.Errorf("bootstrap with a prebuilt image: %s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("bootstrap asked fleet_sandbox_uses_release_image although sandbox.image is set")
	}
	out = libBash(t, stub+`SANDBOX_IMAGE_REF=""
`+block+`printf 'PUB=%s\n' "$sandbox_published"`, "MARK="+marker)
	if !strings.Contains(out, "PUB=1") {
		t.Errorf("bootstrap without a prebuilt image: %s", out)
	}
}
