// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// .github/codeql-accepted-findings.json waives specific CodeQL findings from the
// blocking gate in .github/workflows/codeql.yml. A waiver register is only worth
// anything if it cannot rot, so these tests are the anti-rot controls an auditor
// would ask for:
//
//   - every entry names a real file, so a waiver cannot outlive the code it was
//     written about (a renamed or deleted file silently widens coverage loss —
//     the gate would stop matching, but nobody would notice the dead entry);
//   - every entry carries a substantive reason, so "why is this accepted?" is
//     answerable from the repo rather than from a PR conversation;
//   - no duplicate (rule, file) pairs, so there is exactly one reviewed reason
//     per waiver rather than two that can disagree;
//   - the rule ids look like CodeQL rule ids, so a typo fails here instead of
//     silently never matching (a waiver that matches nothing is indistinguishable
//     from a waiver that works, until the day it was supposed to fire).

type acceptedFinding struct {
	Rule   string `json:"rule"`
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type acceptedRegister struct {
	Accepted []acceptedFinding `json:"accepted"`
}

func loadRegister(t *testing.T) (acceptedRegister, string) {
	t.Helper()
	root := repoRoot(t)
	path := filepath.Join(root, ".github", "codeql-accepted-findings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var reg acceptedRegister
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return reg, root
}

// The register is consumed by jq in the workflow, which fails the build if the
// file is missing but cannot judge whether an entry still makes sense.
func TestCodeQLRegisterEntriesAreWellFormed(t *testing.T) {
	reg, _ := loadRegister(t)
	if len(reg.Accepted) == 0 {
		// Not an error in principle — an empty register means nothing is waived,
		// which is the goal state. Say so rather than asserting a count that
		// would have to be edited every time a finding is genuinely fixed.
		t.Log("register is empty: no CodeQL findings are currently waived")
		return
	}
	for _, e := range reg.Accepted {
		if strings.TrimSpace(e.Rule) == "" {
			t.Errorf("entry with file %q has no rule", e.File)
			continue
		}
		// CodeQL rule ids are "<lang>/<name>" — e.g. go/request-forgery.
		if !strings.Contains(e.Rule, "/") || strings.ContainsAny(e.Rule, " \t") {
			t.Errorf("rule %q does not look like a CodeQL rule id (want <lang>/<name>)", e.Rule)
		}
		if strings.TrimSpace(e.File) == "" {
			t.Errorf("entry for rule %q has no file", e.Rule)
		}
		// A reason has to actually say something. The gate cannot check this and
		// a reviewer skimming a diff might not either.
		if len(strings.TrimSpace(e.Reason)) < 80 {
			t.Errorf("rule %q file %q: reason is too short to be a justification (%d chars) — say why the finding cannot be exploited HERE",
				e.Rule, e.File, len(strings.TrimSpace(e.Reason)))
		}
		if strings.Contains(strings.ToLower(e.Reason), "false positive") &&
			len(strings.TrimSpace(e.Reason)) < 160 {
			t.Errorf("rule %q file %q: %q is an assertion, not a justification",
				e.Rule, e.File, e.Reason)
		}
	}
}

// A waiver that names a file which no longer exists matches nothing, so the gate
// would silently start blocking (or, worse, the finding moved to a file that is
// NOT waived and nobody connected the two). Either way the entry is stale.
func TestCodeQLRegisterFilesExist(t *testing.T) {
	reg, root := loadRegister(t)
	for _, e := range reg.Accepted {
		if strings.TrimSpace(e.File) == "" {
			continue
		}
		// The register stores repo-relative, forward-slash SARIF URIs.
		p := filepath.Join(root, filepath.FromSlash(e.File))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("rule %q waives %q, which does not exist — remove the entry or repoint it: %v",
				e.Rule, e.File, err)
		}
	}
}

// Two entries for the same (rule, file) means two reasons that can drift apart,
// and the gate would honor whichever it saw first.
func TestCodeQLRegisterHasNoDuplicates(t *testing.T) {
	reg, _ := loadRegister(t)
	seen := make(map[string]bool, len(reg.Accepted))
	for _, e := range reg.Accepted {
		key := e.Rule + " " + e.File
		if seen[key] {
			t.Errorf("duplicate register entry for %q", key)
		}
		seen[key] = true
	}
}

// The workflow reads the register by an exact path. If that path moves, the gate
// fails closed (it refuses to run without the file) — but only at CI time, on
// whatever PR happens to move it. Assert the coupling here instead.
func TestCodeQLWorkflowReferencesTheRegister(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "codeql.yml"))
	if err != nil {
		t.Fatalf("read codeql.yml: %v", err)
	}
	const want = ".github/codeql-accepted-findings.json"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("codeql.yml no longer references %s — the gate and the register have been decoupled", want)
	}
}
