// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A CI job that dies downloading a tool, before any test runs, is a red build
// that says nothing about the change under test. The go-race job did exactly
// that on a `curl: (55) OpenSSL SSL_read: unexpected eof` fetching the PGDG
// apt key. So every curl in a workflow carries curl's own bounded retry
// (`--retry N --retry-all-errors --connect-timeout S`, keeping `-f` so an HTTP
// error is still an error), and every apt-get that touches the network carries
// `-o Acquire::Retries=N`. Retrying never weakens an integrity check: the
// sha256sum / signature step after each fetch is untouched, and a mismatch
// fails every attempt.
//
// The other fetchers (go mod download, go install, playwright install, the
// sandbox image build) have no retry flag of their own and go through
// scripts/retry.sh; pip and npm use theirs (`--retries`, `--fetch-retries`).
// Those are reviewed in place rather than pinned here.

var (
	// A curl invocation as a command, not the word in prose: at the start of a
	// logical line or after a shell separator, optionally under sudo.
	workflowCurlRe = regexp.MustCompile(`(?:^|[\s;&|(]|\$\()(?:sudo\s+)?curl\s`)
	// `command -v curl` and friends name curl without running it.
	curlLookupRe = regexp.MustCompile(`\b(?:command\s+-v|which|type)\s+curl\b`)
	// apt-get subcommands that fetch.
	workflowAptRe = regexp.MustCompile(`\bapt-get\b[^;&|]*?\b(update|install|upgrade|dist-upgrade|source|download|build-dep)\b`)
)

func isCurlInvocation(line string) bool {
	return workflowCurlRe.MatchString(curlLookupRe.ReplaceAllString(line, ""))
}

// workflowLogicalLines strips whole-line comments and joins backslash
// continuations, so a curl whose flags sit on the next line is one line.
func workflowLogicalLines(raw string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func TestWorkflowNetworkFetchesRetry(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	curls, apts := 0, 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range workflowLogicalLines(string(raw)) {
			if isCurlInvocation(line) {
				curls++
				for _, want := range []string{"--retry ", "--retry-all-errors", "--connect-timeout "} {
					if !strings.Contains(line, want) {
						t.Errorf("%s: curl without %s — a transient network failure fails the job: %q", e.Name(), strings.TrimSpace(want), line)
					}
				}
				if !regexp.MustCompile(`\scurl\s+-[A-Za-z]*f`).MatchString(" "+line) && !strings.Contains(line, "--fail") {
					t.Errorf("%s: curl without -f — an HTTP error page would be saved as the download: %q", e.Name(), line)
				}
			}
			for _, m := range workflowAptRe.FindAllString(line, -1) {
				apts++
				if !strings.Contains(m, "Acquire::Retries=") {
					t.Errorf("%s: apt-get without -o Acquire::Retries=N — one dropped mirror connection fails the job: %q", e.Name(), line)
				}
			}
		}
	}
	// Guard against the regexes silently matching nothing (a format change
	// would otherwise turn this into a vacuous pass).
	if curls == 0 || apts == 0 {
		t.Fatalf("found %d curl and %d apt-get invocations across the workflows — expected both; the matcher is broken", curls, apts)
	}
	t.Logf("checked %d curl and %d apt-get invocations", curls, apts)
}

func TestWorkflowLogicalLinesJoinsContinuations(t *testing.T) {
	got := workflowLogicalLines("  # curl in a comment\n  curl -fsSL \\\n    --retry 5 x\n  echo done\n")
	if len(got) != 2 || !strings.Contains(got[0], "curl -fsSL") || !strings.Contains(got[0], "--retry 5") {
		t.Fatalf("logical lines = %q", got)
	}
	if isCurlInvocation("command -v curl >/dev/null") {
		t.Fatal("`command -v curl` is not a fetch")
	}
	if !isCurlInvocation(`latest="$(curl -fsSL x)"`) || !isCurlInvocation("sudo curl -fsSL x") {
		t.Fatal("curl invocations not matched")
	}
	if m := workflowAptRe.FindString("sudo apt-get -o Acquire::Retries=5 install -y podman"); !strings.Contains(m, "Acquire::Retries=") {
		t.Fatalf("apt match %q lost the option", m)
	}
}

// scripts/retry.sh: retries a failing command up to the bound, returns the
// command's own status, and stops at the first success.
func TestRetryScript(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "retry.sh")
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")

	// Fails twice, then succeeds.
	flaky := `n=$(cat "$1" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$1"; [ "$n" -ge 3 ]`
	out, err := exec.Command("bash", script, "--attempts", "3", "--delay", "0", "--", "bash", "-c", flaky, "_", counter).CombinedOutput()
	if err != nil {
		t.Fatalf("flaky command should succeed on attempt 3: %v\n%s", err, out)
	}
	if n, _ := os.ReadFile(counter); strings.TrimSpace(string(n)) != "3" {
		t.Fatalf("ran %q times, want 3", strings.TrimSpace(string(n)))
	}

	// Always fails with 7: three attempts, exit 7, an ::error:: naming it.
	_ = os.Remove(counter)
	cmd := exec.Command("bash", script, "--attempts", "3", "--delay", "0", "--what", "the thing", "--", "bash", "-c", `n=$(cat "$1" 2>/dev/null || echo 0); echo $((n+1)) > "$1"; exit 7`, "_", counter)
	out, err = cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("want exit 7, got %v\n%s", err, out)
	}
	if n, _ := os.ReadFile(counter); strings.TrimSpace(string(n)) != "3" {
		t.Fatalf("ran %q times, want 3", strings.TrimSpace(string(n)))
	}
	if !strings.Contains(string(out), "::error::the thing failed 3 time(s)") {
		t.Fatalf("missing ::error:: annotation:\n%s", out)
	}

	// Bad usage is exit 2 and runs nothing.
	if err := exec.Command("bash", script, "--attempts", "0", "--", "true").Run(); err == nil {
		t.Fatal("--attempts 0 accepted")
	}
	if err := exec.Command("bash", script, "--attempts", "2").Run(); err == nil {
		t.Fatal("missing command accepted")
	}
}
