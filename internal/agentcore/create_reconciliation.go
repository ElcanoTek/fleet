package agentcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// maxCreateLedgerBytes bounds the start-of-run ledger read. Records are a few
// hundred bytes (key + flags), so this is orders of magnitude above any real
// ledger; past it the read is refused rather than buffered.
const maxCreateLedgerBytes = 32 << 20

// readCreateLedger reads the ledger defensively: it sits in the
// sandbox-writable workspace mount, so a prior attempt may have replaced it.
// O_NOFOLLOW refuses a symlink to a host file, O_NONBLOCK keeps a planted FIFO
// from blocking the open (and with it a scheduler worker), the opened handle
// must be a regular file, and the read is bounded.
func readCreateLedger(path string) ([]byte, error) {
	// #nosec G304 -- path is the fixed ledger basename inside the
	// fleet-managed MCP workspace dir (mcp_workspace.go); its content is
	// parsed as untrusted JSON.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", createLedgerFilename, info.Mode().Type())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCreateLedgerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCreateLedgerBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", createLedgerFilename, maxCreateLedgerBytes)
	}
	return raw, nil
}

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
	raw, err := readCreateLedger(filepath.Join(workdir, createLedgerFilename))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("create reconciliation: ledger in %s not read: %v", workdir, err)
		}
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
