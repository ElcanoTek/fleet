package agentcore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Start-of-run create reconciliation (#717, ported from the v1 engine).
//
// The cutlass-family bundle MCP servers write a cross-run create ledger
// (creates.jsonl) into the workspace directory fleet hands them via the
// reserved ${FLEET_WORKSPACE} manifest-env token (see mcp_workspace.go). The
// ledger's fail-closed half records — BEFORE a non-idempotent create POST
// goes out — that a create was SUBMITTED; a definite outcome (confirmed
// created, confirmed partial, or confirmed not-created) resolves the marker
// with a later record for the same (ssp, deal_name) key. If the process dies
// mid-POST, no resolving record ever follows, and the record MAY exist
// server-side.
//
// Fleet's half of the contract: at scheduled-run start, replay any unresolved
// markers into the task prompt so a retried/resumed run verifies whether the
// creates landed BEFORE issuing any new create — otherwise a mid-run failure
// can double-create live records. The injected block is byte-compatible with
// the v1 engine's so bundle protocols that reference its wording still match.
// It is appended to the TASK (user-portion) prompt, never the cached system
// prefix, per docs/PROMPT-CACHE-CONTRACT.md.

// createLedgerFilename is the ledger file the bundle servers append to inside
// the resolved MCP workspace dir. The name is part of the bundle wire
// contract (run_ledger's CREATES_FILENAME).
const createLedgerFilename = "creates.jsonl"

// createLedgerRecord is one JSONL line of the bundle servers' create ledger.
// Field names are the ledger wire contract; "ssp" and "deal_name" are the
// record's composite key (fleet treats both as opaque strings).
type createLedgerRecord struct {
	SSP            string `json:"ssp"`
	DealName       string `json:"deal_name"`
	Submitted      bool   `json:"submitted"`
	SubmitResolved bool   `json:"submit_resolved"`
	Success        bool   `json:"success"`
	Partial        bool   `json:"partial"`
}

// AugmentTaskWithCreateReconciliation turns unresolved pre-POST markers from a
// prior process into a mandatory start-of-run sweep appended to the task
// prompt. This closes the SIGTERM/crash window before the model reaches any
// new create call without exposing payloads (the ledger stores only the
// key + outcome flags, never credentials or full payloads). workdir is the
// run's resolved MCP workspace dir; "" (no workspace-armed server) and any
// read/parse miss return the task unchanged — the ledger is advisory here,
// the fail-closed guard lives server-side in the bundle.
func AugmentTaskWithCreateReconciliation(task, workdir string) string {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return task
	}
	// #nosec G304 -- workdir is the fleet-managed MCP workspace dir
	// (mcp_workspace.go); the fixed basename cannot escape it and the file is
	// parsed as untrusted JSON.
	raw, err := os.ReadFile(filepath.Join(workdir, createLedgerFilename))
	if err != nil {
		return task
	}
	unresolved := make(map[string]createLedgerRecord)
	for _, line := range strings.Split(string(raw), "\n") {
		var record createLedgerRecord
		// Torn lines from a crashed writer are tolerated and skipped, matching
		// the ledger's own read semantics.
		if json.Unmarshal([]byte(line), &record) != nil || record.SSP == "" || record.DealName == "" {
			continue
		}
		key := record.SSP + "\x00" + record.DealName
		if record.Submitted {
			unresolved[key] = record
		} else if record.SubmitResolved || record.Success || record.Partial {
			delete(unresolved, key)
		}
	}
	if len(unresolved) == 0 {
		return task
	}
	items := make([]string, 0, len(unresolved))
	for _, record := range unresolved {
		items = append(items, fmt.Sprintf("- SSP=%s deal=%q", record.SSP, record.DealName))
	}
	sort.Strings(items)
	// Byte-compatible with the v1 engine's injected block — bundle protocols
	// reference this wording; do not rephrase it here.
	return task + "\n\nCRITICAL START-OF-RUN CREATE RECONCILIATION (resume safety):\n" +
		"The prior process stopped after submitting these creates but before recording a definite outcome:\n" +
		strings.Join(items, "\n") + "\n" +
		"MUST reconcile every entry with the SSP's read/list/search tools before ANY new create. " +
		"If the SSP cannot prove absence, fail closed and report the paused item; NEVER blindly recreate it."
}
