// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestPinsUpdateStaysRedAfterAPartialUpdate runs pins-update.yml's PR step
// with stub git and gh: when update-pins.sh failed for one pin but moved
// others, the PR (or issue) is still opened — and the run still fails.
func TestPinsUpdateStaysRedAfterAPartialUpdate(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), ".github/workflows/pins-update.yml")), &wf); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "gh pr create") {
				run = s.Run
			}
		}
	}
	if run == "" {
		t.Fatal("pins-update.yml has no step that opens the PR")
	}
	for _, c := range []struct {
		name, outcome, prExit string
		changed, wantOK       bool
	}{
		{"all verified, PR opened", "success", "0", true, true},
		{"one failed, PR opened", "failure", "0", true, false},
		{"one failed, issue instead of PR", "failure", "1", true, false},
		{"all verified, issue instead of PR", "success", "1", true, true},
		{"nothing moved, one failed", "failure", "0", false, false},
		{"nothing moved", "success", "0", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			log := filepath.Join(t.TempDir(), "calls")
			diff := "0"
			if c.changed {
				diff = "1"
			}
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> \""+log+"\"\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("git", `[ "$1" = diff ] && exit `+diff+"\nexit 0\n")
			stub("gh", `[ "$1 $2" = "pr create" ] && exit `+c.prExit+"\nexit 0\n")
			cmd := exec.Command("bash", "-c", run)
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PINS_OUTCOME="+c.outcome,
				"CHANGES=placeholder 1.0 -> 1.1", "GH_TOKEN=placeholder")
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.wantOK {
				t.Errorf("ok = %v, want %v\n%s", err == nil, c.wantOK, out)
			}
			calls, _ := os.ReadFile(log)
			if c.changed && !strings.Contains(string(calls), "gh pr create") {
				t.Errorf("the changes were not proposed:\n%s", calls)
			}
		})
	}
}
