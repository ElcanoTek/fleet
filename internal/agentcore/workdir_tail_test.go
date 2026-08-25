package agentcore

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestWorkingDirTurnSuffix_NamesTheDirAndTheMCPRule(t *testing.T) {
	got := WorkingDirTurnSuffix("/var/lib/fleet/workspace/runs/abc")
	for _, want := range []string{
		"## Working directory (this run)",
		"    /var/lib/fleet/workspace/runs/abc",
		"output_dir",
		"ABSOLUTE",
		"download_url",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("suffix missing %q:\n%s", want, got)
		}
	}
}

func TestAppendWorkingDirMessage_AppendsOneUserMessage(t *testing.T) {
	msgs := []fantasy.Message{fantasy.NewUserMessage("task")}
	out := appendWorkingDirMessage(msgs, "/tmp/wt")
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[1].Role != fantasy.MessageRoleUser {
		t.Fatalf("role=%v, want user", out[1].Role)
	}
}
