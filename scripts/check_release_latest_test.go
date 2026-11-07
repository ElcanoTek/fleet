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

// TestNewerPublishedReleases — Latest is decided by successfully PUBLISHED
// releases (non-draft, with SHA256SUMS), never by the mere existence of a
// newer tag whose build failed or is still running.
func TestNewerPublishedReleases(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := repoRoot(t)
	for _, c := range []struct {
		name   string
		tag    string            // the release being published
		listed []string          // gh release list (non-draft)
		state  map[string]string // gh release view → complete?
		listOK bool
		want   string
		wantOK bool
	}{
		// .2 was tagged but its build failed: no release, so .1 is newest.
		{"newer tag never published (build failed)", "v2026.11.07.1", []string{"v2026.11.07.1"}, nil, true, "", true},
		{"newer release published", "v2026.11.07.1", []string{"v2026.11.07.2", "v2026.11.07.1"}, map[string]string{"v2026.11.07.2": "true"}, true, "v2026.11.07.2", true},
		{"newer release incomplete (no SHA256SUMS)", "v2026.11.07.1", []string{"v2026.11.07.2"}, map[string]string{"v2026.11.07.2": "false"}, true, "", true},
		{"only older releases, numeric order", "v2026.11.07.10", []string{"v2026.11.07.9", "v2026.11.06.12"}, nil, true, "", true},
		{"a .10 is newer than .9", "v2026.11.07.9", []string{"v2026.11.07.10"}, map[string]string{"v2026.11.07.10": "true"}, true, "v2026.11.07.10", true},
		{"listing fails", "v2026.11.07.1", nil, nil, false, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin, state := t.TempDir(), t.TempDir()
			for tag, v := range c.state {
				_ = os.WriteFile(filepath.Join(state, tag), []byte(v), 0o644)
			}
			list := "exit 1"
			if c.listOK {
				list = "printf '%s\\n' " + strings.Join(c.listed, " ") + "; exit 0"
			}
			gh := "#!/bin/sh\ncase \"$1 $2\" in\n" +
				"  'release list') " + list + " ;;\n" +
				"  'release view') [ -f \"" + state + "/$3\" ] || exit 1; cat \"" + state + "/$3\"; echo; exit 0 ;;\n" +
				"esac\nexit 1\n"
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(root, "scripts/release-newer-published.sh"), c.tag)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.Output()
			if (err == nil) != c.wantOK {
				t.Fatalf("ok = %v, want %v (%s)", err == nil, c.wantOK, out)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Errorf("newer published = %q, want %q", got, c.want)
			}
		})
	}

	// release.yml decides the mark with it, before AND after the create, and
	// no longer from the newest tag.
	raw, _ := releaseWorkflow(t)
	if strings.Count(raw, "scripts/release-newer-published.sh \"${TAG}\"") != 2 {
		t.Error("release.yml must decide Latest from published releases both before and after `gh release create`")
	}
	if strings.Contains(raw, "newest=\"$(git tag --list | scripts/version.sh filter | tail -n 1)\"") {
		t.Error("release.yml still decides Latest from the newest TAG")
	}
}

// TestTagAllocationOutlastsContention runs the tag job's allocation step with
// stub git and version.sh: K concurrent runs taking ordinals first must not
// leave this certified commit untagged, however many K is below the bound.
func TestTagAllocationOutlastsContention(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	var run string
	for _, s := range jobs["tag"].Steps {
		if strings.Contains(s.Run, `git push origin "refs/tags/${tag}"`) {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatal("no tag-allocation step in the tag job")
	}
	for _, c := range []struct {
		name       string
		rejections int
		max        string
		wantTag    string
	}{
		{"uncontended", 0, "40", "v2026.11.07.1"},
		{"five runs got there first", 5, "40", "v2026.11.07.6"},
		{"twelve runs got there first", 12, "40", "v2026.11.07.13"},
		{"a push that never succeeds stops at the bound", 1000, "6", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			count := filepath.Join(dir, "pushes")
			_ = os.WriteFile(count, []byte("0"), 0o644)
			write := func(p, body string) {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Each rejected push is another run taking that ordinal.
			write(filepath.Join(bin, "git"), "#!/bin/sh\nif [ \"$1\" = push ]; then n=$(cat "+count+"); echo $((n+1)) > "+count+
				"; [ \"$n\" -ge "+strconv.Itoa(c.rejections)+" ] && exit 0; exit 1; fi\nexit 0\n")
			write(filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n")
			write(filepath.Join(dir, "scripts", "version.sh"), "#!/bin/sh\ncase \"$1\" in\n  next) echo v2026.11.07.$(( $(cat "+count+") + 1 )) ;;\n  released-at) exit 0 ;;\nesac\n")
			out := filepath.Join(dir, "output")
			cmd := exec.Command("bash", "-c", run)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GITHUB_OUTPUT="+out,
				"HEAD_SHA=1111111111111111111111111111111111111111", "RUN_URL=https://example.invalid/run", "TAG_MAX_ATTEMPTS="+c.max)
			log, err := cmd.CombinedOutput()
			got, _ := os.ReadFile(out)
			if c.wantTag == "" {
				if err == nil {
					t.Fatalf("a push that never succeeds reported success\n%s", log)
				}
				if !strings.Contains(string(log), "after 6 attempts") {
					t.Errorf("want the bound reached\n%s", log)
				}
				return
			}
			if err != nil || !strings.Contains(string(got), "tag="+c.wantTag+"\n") {
				t.Fatalf("want tag=%s, err=%v, outputs=%q\n%s", c.wantTag, err, got, log)
			}
		})
	}
}
