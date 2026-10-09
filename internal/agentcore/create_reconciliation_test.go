package agentcore

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeLedger(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, createLedgerFilename), []byte(content), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func TestAugmentTaskWithCreateReconciliation(t *testing.T) {
	const task = "Create this week's PG records."
	const header = "CRITICAL START-OF-RUN CREATE RECONCILIATION (resume safety):"

	cases := []struct {
		name       string
		ledger     string // "" = no ledger file written
		noWorkdir  bool
		wantBlock  bool
		wantLines  []string
		rejectText []string
	}{
		{
			name:      "no workdir is a no-op",
			noWorkdir: true,
		},
		{
			name: "no ledger file is a no-op",
		},
		{
			name: "unresolved pre-POST marker injects the sweep",
			ledger: `{"ssp":"openx","deal_name":"Deal_A","success":false,"submitted":true,"ts":1.0}
`,
			wantBlock: true,
			wantLines: []string{`- SSP=openx deal="Deal_A"`},
		},
		{
			name: "later confirmed create resolves the marker",
			ledger: `{"ssp":"openx","deal_name":"Deal_A","success":false,"submitted":true,"ts":1.0}
{"ssp":"openx","deal_name":"Deal_A","success":true,"ts":2.0,"deal_id":"OX-1"}
`,
		},
		{
			name: "later partial create resolves the marker",
			ledger: `{"ssp":"openx","deal_name":"Deal_A","success":false,"submitted":true,"ts":1.0}
{"ssp":"openx","deal_name":"Deal_A","success":false,"partial":true,"ts":2.0}
`,
		},
		{
			name: "explicit submit_resolved releases the marker",
			ledger: `{"ssp":"openx","deal_name":"Deal_A","success":false,"submitted":true,"ts":1.0}
{"ssp":"openx","deal_name":"Deal_A","success":false,"submit_resolved":true,"ts":2.0}
`,
		},
		{
			name: "only the unresolved sibling is listed, sorted, torn lines tolerated",
			ledger: `{"ssp":"pubmatic","deal_name":"Deal_C","success":false,"submitted":true,"ts":1.0}
{"ssp":"openx","deal_name":"Deal_B","success":false,"submitted":true,"ts":1.5}
{"ssp":"openx","deal_name":"Deal_A","success":false,"submitted":true,"ts":2.0}
{"ssp":"openx","deal_name":"Deal_A","success":true,"ts":3.0}
{"ssp":"openx","deal_na
`,
			wantBlock:  true,
			wantLines:  []string{`- SSP=openx deal="Deal_B"` + "\n" + `- SSP=pubmatic deal="Deal_C"`},
			rejectText: []string{`deal="Deal_A"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := ""
			if !tc.noWorkdir {
				workdir = t.TempDir()
				if tc.ledger != "" {
					writeLedger(t, workdir, tc.ledger)
				}
			}
			got := AugmentTaskWithCreateReconciliation(task, workdir)
			if !tc.wantBlock {
				if got != task {
					t.Fatalf("expected task unchanged, got:\n%s", got)
				}
				return
			}
			if !strings.HasPrefix(got, task) {
				t.Fatalf("augmented prompt must keep the task first (prompt-cache task portion), got:\n%s", got)
			}
			if !strings.Contains(got, header) {
				t.Fatalf("missing reconciliation header, got:\n%s", got)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in:\n%s", want, got)
				}
			}
			for _, reject := range tc.rejectText {
				if strings.Contains(got, reject) {
					t.Fatalf("resolved entry %q leaked into:\n%s", reject, got)
				}
			}
			if !strings.Contains(got, "NEVER blindly recreate it.") {
				t.Fatalf("missing fail-closed instruction tail, got:\n%s", got)
			}
		})
	}
}

// TestCreateReconciliationRefusesSpecialLedger pins Codex P1 on #1709: the
// ledger lives in the sandbox-writable mount, so a prior attempt can leave a
// FIFO (which would block a plain read forever, pinning a scheduler worker) or
// a symlink to a host file in its place. Neither is read; the task is
// returned unchanged, promptly.
func TestCreateReconciliationRefusesSpecialLedger(t *testing.T) {
	const task = "Create this week's PG records."
	unresolved := `{"ssp":"PubMatic","deal_name":"X","submitted":true}` + "\n"

	t.Run("fifo", func(t *testing.T) {
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, createLedgerFilename), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan string, 1)
		go func() { done <- AugmentTaskWithCreateReconciliation(task, dir) }()
		select {
		case got := <-done:
			if got != task {
				t.Fatalf("FIFO ledger must leave the task unchanged, got %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reading a FIFO ledger blocked")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "host.jsonl")
		if err := os.WriteFile(target, []byte(unresolved), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, createLedgerFilename)); err != nil {
			t.Fatal(err)
		}
		if got := AugmentTaskWithCreateReconciliation(task, dir); got != task {
			t.Fatalf("symlinked ledger must not be followed, got %q", got)
		}
	})

	t.Run("regular file still read", func(t *testing.T) {
		dir := t.TempDir()
		writeLedger(t, dir, unresolved)
		if got := AugmentTaskWithCreateReconciliation(task, dir); !strings.Contains(got, `deal="X"`) {
			t.Fatalf("regular ledger must still be reconciled, got %q", got)
		}
	})
}
