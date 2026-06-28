package agentcore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEntrypointConformance guards the "governance is one core" invariant
// (AGENTS.md): every entrypoint that runs an agent turn must drive the single
// governed loop, agentcore.Run — never fork a second, weaker path. Issue #49's
// Note asks for this slice explicitly.
//
// It is a source-level guard on purpose: the alternative (invoking each
// entrypoint and observing that Run executed) needs a model, a sandbox, and a
// DB per transport. Asserting the call site exists is cheap, always-on, and
// fails loudly if someone adds a turn path that bypasses the core — at which
// point the right fix is to route it through agentcore.Run, then update this
// list (re-affirming the invariant), not to delete the check.
//
// The web/SSE path reaches Run through agent.Manager.RunTurn → RunInteractiveTurn
// (interactive.go); the scheduled-native path through agent.Agent.Execute
// (scheduled.go). These are the only two agent-turn entrypoints — fleet runs one
// native in-process loop and nothing else.
func TestEntrypointConformance(t *testing.T) {
	root := repoRoot(t)
	// file → a human label for the entrypoint it hosts.
	governed := map[string]string{
		"internal/agent/interactive.go": "web/SSE interactive turn (Manager.RunTurn → RunInteractiveTurn)",
		"internal/agent/scheduled.go":   "scheduled-native task (Agent.Execute)",
	}
	for rel, label := range governed {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read entrypoint %s (%s): %v", rel, label, err)
		}
		if !strings.Contains(string(src), "agentcore.Run(") {
			t.Errorf("%s (%s) no longer calls agentcore.Run — the one-governed-core invariant requires every turn entrypoint to drive it", rel, label)
		}
	}
}

// repoRoot resolves the repository root from this test file's location
// (internal/agentcore/…), so the source-level checks don't depend on the working
// directory `go test` happens to run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <root>/internal/agentcore/entrypoint_conformance_test.go
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}
