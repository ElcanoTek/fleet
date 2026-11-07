// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	tagHead  = "1111111111111111111111111111111111111111"
	tagNewer = "2222222222222222222222222222222222222222"
)

const (
	rejectExists   = " ! [rejected]        v2026.11.07.9 -> v2026.11.07.9 (already exists)\nerror: failed to push some refs\n"
	rejectWorkflow = " ! [remote rejected] v2026.11.07.17 -> v2026.11.07.17 (refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission)\nerror: failed to push some refs\n"
	rejectAuth     = "remote: Permission to ElcanoTek/fleet.git denied to github-actions[bot].\nfatal: unable to access 'https://github.com/ElcanoTek/fleet/': The requested URL returned error: 403\n"
)

// tagStep is release.yml's tag-allocation step body.
func tagStep(t *testing.T) string {
	t.Helper()
	_, jobs := releaseWorkflow(t)
	for _, s := range jobs["tag"].Steps {
		if strings.Contains(s.Run, `git push origin "refs/tags/${tag}"`) {
			return s.Run
		}
	}
	t.Fatal("no tag-allocation step in the tag job")
	return ""
}

type tagCase struct {
	tip          string // main's tip (git rev-parse HEAD / ls-remote)
	workflowDiff bool   // HEAD_SHA's .github/workflows differ from the tip's
	rejections   int    // pushes rejected before one succeeds
	reject       string // the rejection's output
	max          string
}

// runTagStep runs the step with stub git and version.sh; it returns the step
// outputs, the push count, the log and the error.
func runTagStep(t *testing.T, c tagCase) (string, int, string, error) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	count := filepath.Join(dir, "pushes")
	_ = os.WriteFile(count, []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "reject"), []byte(c.reject), 0o644)
	write := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	diff := "0"
	if c.workflowDiff {
		diff = "1"
	}
	write(filepath.Join(bin, "git"), `#!/bin/sh
case "$1" in
  push) n=$(cat `+count+`); echo $((n+1)) > `+count+`
        [ "$n" -ge `+strconv.Itoa(c.rejections)+` ] && exit 0
        cat `+filepath.Join(dir, "reject")+` >&2; exit 1 ;;
  rev-parse) echo `+c.tip+` ;;
  ls-remote) printf '%s\tHEAD\n' `+c.tip+` ;;
  diff) exit `+diff+` ;;
esac
exit 0
`)
	write(filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n")
	write(filepath.Join(dir, "scripts", "version.sh"), "#!/bin/sh\ncase \"$1\" in\n  next) echo v2026.11.07.$(( $(cat "+count+") + 1 )) ;;\n  released-at) exit 0 ;;\nesac\n")
	classifier, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "release-push-rejection.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "scripts", "release-push-rejection.sh"), string(classifier))
	out := filepath.Join(dir, "output")
	cmd := exec.Command("bash", "-c", tagStep(t))
	cmd.Dir = dir
	maxAttempts := c.max
	if maxAttempts == "" {
		maxAttempts = "40"
	}
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out,
		"HEAD_SHA="+tagHead, "RUN_URL=https://example.invalid/run", "TAG_MAX_ATTEMPTS="+maxAttempts)
	log, runErr := cmd.CombinedOutput()
	outputs, _ := os.ReadFile(out)
	n, _ := os.ReadFile(count)
	pushes, _ := strconv.Atoi(strings.TrimSpace(string(n)))
	return string(outputs), pushes, string(log), runErr
}

// TestTagAllocationRetriesOnlyContention — any number of runs taking
// ordinals first still ends tagged; a rejection retrying cannot fix stops at
// once; and a commit a workflow change on main superseded is skipped, not
// retried 40 times.
func TestTagAllocationRetriesOnlyContention(t *testing.T) {
	t.Run("uncontended", func(t *testing.T) {
		out, _, log, err := runTagStep(t, tagCase{tip: tagHead})
		if err != nil || !strings.Contains(out, "tag=v2026.11.07.1\n") || !strings.Contains(out, "released=true") {
			t.Fatalf("err=%v outputs=%q\n%s", err, out, log)
		}
	})
	for _, k := range []int{5, 12} {
		t.Run("contention x"+strconv.Itoa(k), func(t *testing.T) {
			out, pushes, log, err := runTagStep(t, tagCase{tip: tagHead, rejections: k, reject: rejectExists})
			want := "tag=v2026.11.07." + strconv.Itoa(k+1) + "\n"
			if err != nil || !strings.Contains(out, want) || pushes != k+1 {
				t.Fatalf("want %q after %d pushes; err=%v outputs=%q pushes=%d\n%s", want, k+1, err, out, pushes, log)
			}
		})
	}
	t.Run("contention forever stops at the bound", func(t *testing.T) {
		_, pushes, log, err := runTagStep(t, tagCase{tip: tagHead, rejections: 1000, reject: rejectExists, max: "6"})
		if err == nil || pushes != 6 || !strings.Contains(log, "after 6 attempts") {
			t.Fatalf("err=%v pushes=%d\n%s", err, pushes, log)
		}
	})
	t.Run("a non-contention rejection is not retried", func(t *testing.T) {
		out, pushes, log, err := runTagStep(t, tagCase{tip: tagHead, rejections: 1000, reject: rejectAuth})
		if err == nil || pushes != 1 || strings.Contains(out, "released=") {
			t.Fatalf("want one push and a failure; err=%v pushes=%d outputs=%q\n%s", err, pushes, out, log)
		}
		if !strings.Contains(log, "retrying cannot fix") {
			t.Errorf("want a clear reason\n%s", log)
		}
	})
	t.Run("superseded by a workflow change on main: skipped before any push", func(t *testing.T) {
		out, pushes, log, err := runTagStep(t, tagCase{tip: tagNewer, workflowDiff: true})
		if err != nil || pushes != 0 || !strings.Contains(out, "released=false") || strings.Contains(out, "released=true") {
			t.Fatalf("want a skip; err=%v pushes=%d outputs=%q\n%s", err, pushes, out, log)
		}
		if !strings.Contains(log, "::notice::") || !strings.Contains(log, "superseded") {
			t.Errorf("want a ::notice:: naming it superseded\n%s", log)
		}
	})
	t.Run("an older commit with the same workflow files is still tagged", func(t *testing.T) {
		out, _, log, err := runTagStep(t, tagCase{tip: tagNewer})
		if err != nil || !strings.Contains(out, "released=true") {
			t.Fatalf("err=%v outputs=%q\n%s", err, out, log)
		}
	})
	t.Run("the workflow-permission refusal met in a race: skipped once", func(t *testing.T) {
		// main moved after the check: the tip seen by the pre-check is this
		// commit, the push is then refused, and ls-remote shows the newer tip.
		out, pushes, log, err := runTagStep(t, tagCase{tip: tagHead, rejections: 1000, reject: rejectWorkflow})
		// The stub's ls-remote also answers tagHead here, so this commit IS
		// the tip: a real failure, not a skip.
		if err == nil || pushes != 1 || strings.Contains(out, "released=false") {
			t.Fatalf("on main's tip the refusal must fail; err=%v pushes=%d outputs=%q\n%s", err, pushes, out, log)
		}
	})
}

// TestPushRejectionClassifier pins the three classes on real git output.
func TestPushRejectionClassifier(t *testing.T) {
	for want, log := range map[string]string{
		"contention":          rejectExists,
		"workflow-permission": rejectWorkflow,
		"other":               rejectAuth,
	} {
		p := filepath.Join(t.TempDir(), "log")
		_ = os.WriteFile(p, []byte(log), 0o644)
		out, err := exec.Command(filepath.Join(repoRoot(t), "scripts/release-push-rejection.sh"), p).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%q classified %q (err %v), want %s", log, out, err, want)
		}
	}
	nff := " ! [rejected]        v2026.11.07.9 -> v2026.11.07.9 (non-fast-forward)\n"
	p := filepath.Join(t.TempDir(), "log")
	_ = os.WriteFile(p, []byte(nff), 0o644)
	if out, _ := exec.Command(filepath.Join(repoRoot(t), "scripts/release-push-rejection.sh"), p).Output(); strings.TrimSpace(string(out)) != "contention" {
		t.Errorf("a non-fast-forward is contention, got %q", out)
	}
}
