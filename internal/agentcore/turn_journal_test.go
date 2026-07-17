package agentcore

// #798 seam tests: the intent barrier fails closed, the journaled outcome is
// byte-identical to the governed model-visible result (the #793 boundary
// output), and a nil journal changes nothing.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

type recordingJournal struct {
	mu        sync.Mutex
	intents   map[string]string // callID -> input
	outcomes  map[string]string // callID -> governed text
	intentErr error
}

func newRecordingJournal() *recordingJournal {
	return &recordingJournal{intents: map[string]string{}, outcomes: map[string]string{}}
}

func (j *recordingJournal) ToolIntent(_ context.Context, callID, _, inputJSON string) error {
	if j.intentErr != nil {
		return j.intentErr
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.intents[callID] = inputJSON
	return nil
}

func (j *recordingJournal) ToolOutcome(_ context.Context, callID, _, governedText string, _ bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.outcomes[callID] = governedText
	return nil
}

func TestTurnJournal_OutcomeMatchesModelVisibleBytes(t *testing.T) {
	journal := newRecordingJournal()
	secret := "export OPENAI_API_KEY=sk-test-fixture-not-real-123456\nresult row"
	tool := fantasy.NewAgentTool("emitter", "d",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(secret), nil
		})
	policy := &gatePolicy{}
	guarded := &policyGuardedTool{inner: tool, policy: policy, journal: journal}

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if journal.intents["c1"] != `{}` {
		t.Fatalf("intent not journaled before dispatch: %q", journal.intents["c1"])
	}
	// The journaled outcome is the exact bytes the model + policy received —
	// governed (secret redacted) and bounded, never the raw tool output.
	if journal.outcomes["c1"] != resp.Content {
		t.Fatalf("journaled outcome diverges from model-visible bytes:\n journal: %q\n model:   %q",
			journal.outcomes["c1"], resp.Content)
	}
	if strings.Contains(journal.outcomes["c1"], "sk-test-fixture-not-real-123456") {
		t.Fatal("raw secret reached the journal — governance must run first")
	}
	if !policy.recorded || policy.recordText != resp.Content {
		t.Fatal("policy audit and journal must see identical bytes")
	}
}

func TestTurnJournal_IntentFailureBlocksDispatch(t *testing.T) {
	journal := newRecordingJournal()
	journal.intentErr = errors.New("db down")
	var executed bool
	tool := fantasy.NewAgentTool("side_effect", "d",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			executed = true
			return fantasy.NewTextResponse("ran"), nil
		})
	guarded := &policyGuardedTool{inner: tool, journal: journal}

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("tool executed despite a failed intent write — the barrier must fail closed")
	}
	if !resp.IsError || !strings.Contains(resp.Content, "not executed") {
		t.Fatalf("expected a refusal response, got: %q", resp.Content)
	}
}

func TestTurnJournal_NilJournalIsNoOp(t *testing.T) {
	tool := fantasy.NewAgentTool("plain", "d",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		})
	guarded := &policyGuardedTool{inner: tool}
	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("nil journal must not affect the run: %v %q", err, resp.Content)
	}
}

func TestTurnJournal_MCPToolJournalsUnderRealName(t *testing.T) {
	journal := newRecordingJournal()
	tool := newTestMCPTool(&recordingBroker{text: "mcp says hi"}, nil)
	tool.journal = journal
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c9", Input: `{"q":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if journal.intents["c9"] != `{"q":1}` {
		t.Fatalf("mcp intent missing: %+v", journal.intents)
	}
	if journal.outcomes["c9"] != resp.Content {
		t.Fatalf("mcp journaled outcome diverges from model-visible bytes")
	}
}
