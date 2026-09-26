// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMCPCatalogLintExtractsEveryEntry pins the contract the link lint relies
// on: the built-in catalog is regular enough that a `- name:` line starts each
// entry and every entry's docs_url sits on one `docs_url: "<url>"` line. The Go
// loader requires docs_url on every built-in entry, so the number of extracted
// links must equal the number of entries — a formatting change that broke the
// awk extraction would otherwise silently shrink what the nightly lint checks.
// --list needs no network and no curl.
func TestMCPCatalogLintExtractsEveryEntry(t *testing.T) {
	catalog := filepath.Join("..", "internal", "clientconfig", "builtin_remote_catalog.yaml")
	raw, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	entries := len(regexp.MustCompile(`(?m)^\s*-\s+name:`).FindAllIndex(raw, -1))
	if entries < 60 {
		t.Fatalf("catalog should be substantial, found %d entries", entries)
	}

	out, err := exec.Command("./mcp-catalog-lint.sh", "--catalog", catalog, "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("--list failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != entries {
		t.Fatalf("extracted %d docs_url links for %d entries; the awk extraction and the catalog layout drifted:\n%s", len(lines), entries, out)
	}
	for _, line := range lines {
		cols := strings.Split(line, "\t")
		if len(cols) != 3 || cols[1] != "docs_url" || !strings.HasPrefix(cols[2], "https://") || strings.ContainsAny(cols[2], `"' #`) {
			t.Errorf("malformed extraction line %q", line)
		}
	}
}

// TestMCPCatalogLintVerdicts drives the script against a local server that
// plays every verdict the script knows — a live page, a moved page, a page
// that refuses HEAD, a rate-limited page that recovers, a bot wall, a 404 and
// a host that does not resolve — spread over two hostnames (127.0.0.1 and
// localhost, both the same server) so the per-host grouping, the parallel
// workers and their concurrent result appends are exercised too.
func TestMCPCatalogLintVerdicts(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required by the link lint")
	}
	var rateHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/wall", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	mux.HandleFunc("/headno", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/rate", func(w http.ResponseWriter, _ *http.Request) {
		// The first two requests are throttled; the retry succeeds.
		if rateHits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	hostA := "http://127.0.0.1" + port
	hostB := "http://localhost" + port

	type fixture struct{ name, docs, setup string }
	fixtures := []fixture{
		{"alive", hostA + "/ok", ""},
		{"moved", hostB + "/moved", ""},
		{"headless", hostA + "/headno", ""},
		{"throttled", hostB + "/rate", ""},
		{"walled", hostA + "/wall", ""},
		{"vanished", hostB + "/gone", hostB + "/ok"},
		{"stale-setup", hostA + "/ok", hostA + "/gone"},
		{"commented", hostB + "/ok", ""},
		{"unresolvable", "http://nowhere.invalid/docs", ""},
	}
	catalog := filepath.Join(t.TempDir(), "catalog.yaml")
	var b strings.Builder
	b.WriteString("servers:\n")
	for _, e := range fixtures {
		fmt.Fprintf(&b, "  - name: %s\n    display_name: %s\n    url: \"https://example.invalid/mcp\"\n", e.name, e.name)
		if e.name == "commented" {
			fmt.Fprintf(&b, "    docs_url: \"%s\"  # moved here 2026-09, keep\n", e.docs)
		} else {
			fmt.Fprintf(&b, "    docs_url: \"%s\"\n", e.docs)
		}
		if e.setup != "" {
			fmt.Fprintf(&b, "    setup_url: '%s'\n", e.setup)
		}
		b.WriteString("    auth: open\n")
	}
	if err := os.WriteFile(catalog, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// Isolate the child from a developer's or runner's proxy: the fixtures
	// live on loopback and must not be routed through one.
	env := append(os.Environ(), "GITHUB_STEP_SUMMARY=", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	run := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("./mcp-catalog-lint.sh", append([]string{"--catalog", catalog, "--delay", "0", "--concurrency", "2"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
		return string(out), code
	}

	t.Run("docs_url only", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "links.tsv")
		out, code := run(t, "--report", report)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (dead links); output:\n%s", code, out)
		}
		for _, want := range []string{
			"checking 9 link(s) across 3 host(s)",
			"OK   200  alive  docs_url",
			"OK   200  moved  docs_url",
			"OK   200  headless  docs_url",
			"OK   200  throttled  docs_url",
			"OK   200  commented  docs_url  " + hostB + "/ok\n",
			"WARN 403  walled  docs_url  " + hostA + "/wall  — HTTP 403 (bot wall or rate limit; re-check in a browser)",
			"DEAD 404  vanished  docs_url  " + hostB + "/gone  — HTTP 404",
			"DEAD 000  unresolvable  docs_url  http://nowhere.invalid/docs  — could not resolve host (after 2 retries)",
			"links: 9  ok: 6  warn: 1  dead: 2",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "setup_url") {
			t.Errorf("setup_url was checked although only docs_url was requested:\n%s", out)
		}
		tsv, err := os.ReadFile(report)
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		if !strings.HasPrefix(string(tsv), "verdict\thttp_code\tentry\tfield\turl\tfinal_url\tnote\n") {
			t.Errorf("report header unexpected:\n%s", tsv)
		}
		// Empty columns must survive (no Retry-After, no note) and the final
		// URL after a redirect must be recorded — both were lost when the
		// internal records were tab-separated and `read` collapsed the gaps.
		for _, want := range []string{
			"DEAD\t404\tvanished\tdocs_url\t" + hostB + "/gone\t" + hostB + "/gone\tHTTP 404\n",
			"OK\t200\tmoved\tdocs_url\t" + hostB + "/moved\t" + hostB + "/ok\t\n",
		} {
			if !strings.Contains(string(tsv), want) {
				t.Errorf("report lacks %q:\n%s", want, tsv)
			}
		}
		if got := rateHits.Load(); got < 3 {
			t.Errorf("throttled page was hit %d times, want the throttled HEAD, its retry and the success", got)
		}
	})

	t.Run("setup_url too", func(t *testing.T) {
		out, code := run(t, "--fields", "docs_url,setup_url")
		if code != 1 {
			t.Fatalf("exit %d, want 1; output:\n%s", code, out)
		}
		for _, want := range []string{
			"DEAD 404  stale-setup  setup_url",
			"OK   200  vanished  setup_url",
			"links: 11  ok: 7  warn: 1  dead: 3",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("only healthy entries pass, names may carry spaces", func(t *testing.T) {
		out, code := run(t, "--only", "alive, moved , walled")
		if code != 0 {
			t.Fatalf("exit %d, want 0 (a bot wall is a warning); output:\n%s", code, out)
		}
		if !strings.Contains(out, "links: 3  ok: 2  warn: 1  dead: 0") {
			t.Errorf("unexpected summary:\n%s", out)
		}
	})

	t.Run("strict fails on a warning", func(t *testing.T) {
		out, code := run(t, "--only", "alive,walled", "--strict")
		if code != 1 || !strings.Contains(out, "--strict: 1 link(s)") {
			t.Fatalf("exit %d, want 1 under --strict; output:\n%s", code, out)
		}
	})

	t.Run("an unknown --only name is a setup error, even next to a known one", func(t *testing.T) {
		out, code := run(t, "--only", "alive,nobody")
		if code != 2 || !strings.Contains(out, "not found") || !strings.Contains(out, "nobody") {
			t.Fatalf("exit %d, want 2 naming the unknown entry; output:\n%s", code, out)
		}
	})

	t.Run("concurrency must be at least 1", func(t *testing.T) {
		out, code := run(t, "--concurrency", "0", "--only", "alive")
		if code != 2 {
			t.Fatalf("exit %d, want 2; output:\n%s", code, out)
		}
	})

	t.Run("step summary lists the non-OK links", func(t *testing.T) {
		summary := filepath.Join(t.TempDir(), "summary.md")
		cmd := exec.Command("./mcp-catalog-lint.sh", "--catalog", catalog, "--delay", "0", "--only", "alive,walled,vanished")
		cmd.Env = append(append([]string{}, env...), "GITHUB_STEP_SUMMARY="+summary)
		_, _ = cmd.CombinedOutput()
		md, err := os.ReadFile(summary)
		if err != nil {
			t.Fatalf("summary: %v", err)
		}
		if !strings.Contains(string(md), "3 checked, 1 ok, 1 warn, 1 dead") || !strings.Contains(string(md), "| DEAD | 404 | vanished | docs_url | "+hostB+"/gone | HTTP 404 |") || strings.Contains(string(md), "| OK |") {
			t.Errorf("summary content unexpected:\n%s", md)
		}
	})
}
