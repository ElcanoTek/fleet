package agentcore

// #785 steering-seam tests: injection only at the step boundary, durable
// Acknowledge gating, exactly-once sink recording, stable re-application,
// and nil-source no-op.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

type fakeSteerSource struct {
	mu      sync.Mutex
	pending []SteerMessage
	ackErr  error
	acked   []string
}

func (f *fakeSteerSource) Poll() (SteerMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return SteerMessage{}, false
	}
	msg := f.pending[0]
	f.pending = f.pending[1:]
	return msg, true
}

func (f *fakeSteerSource) Acknowledge(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, id)
	return nil
}

func steerOpts(messages []fantasy.Message) fantasy.PrepareStepFunctionOptions {
	return fantasy.PrepareStepFunctionOptions{Messages: messages}
}

func lastUserText(t *testing.T, messages []fantasy.Message) string {
	t.Helper()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range messages[i].Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				return p.Text
			}
		}
	}
	return ""
}

func TestSteeringStep_InjectsAfterAcknowledge(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{{ID: "in-1", Text: "also check the logs"}}}
	state := &steerState{}
	sink := newStreamSink(nil)
	step := steeringStep(source, state, sink)

	base := []fantasy.Message{fantasy.NewUserMessage("original ask")}
	_, res, err := step(context.Background(), steerOpts(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || lastUserText(t, res.Messages) != "also check the logs" {
		t.Fatalf("steer not appended: %+v", res.Messages)
	}
	if len(source.acked) != 1 || source.acked[0] != "in-1" {
		t.Fatalf("acknowledge not durable-first: %+v", source.acked)
	}
	// The sink recorded the injected user entry exactly once.
	entries, _ := sink.snapshot()
	var userEntries int
	for _, e := range entries {
		if e.Type == "user_text" && e.SteerID == "in-1" {
			userEntries++
		}
	}
	if userEntries != 1 {
		t.Fatalf("user_text entries = %d, want 1", userEntries)
	}
}

func TestSteeringStep_AcknowledgeFailureRefusesInjection(t *testing.T) {
	source := &fakeSteerSource{
		pending: []SteerMessage{{ID: "in-1", Text: "removed meanwhile"}},
		ackErr:  errors.New("no longer queued"),
	}
	state := &steerState{}
	sink := newStreamSink(nil)
	step := steeringStep(source, state, sink)

	base := []fantasy.Message{fantasy.NewUserMessage("original ask")}
	_, res, err := step(context.Background(), steerOpts(base))
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != nil {
		t.Fatalf("refused injection must not modify messages: %+v", res.Messages)
	}
	entries, _ := sink.snapshot()
	for _, e := range entries {
		if e.Type == "user_text" {
			t.Fatal("refused injection must not record an entry")
		}
	}
}

func TestSteeringStep_ReappliesAtStablePositionAcrossSteps(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{{ID: "in-1", Text: "steered guidance"}}}
	state := &steerState{}
	step := steeringStep(source, state, nil)

	// Step 1: accept + append at the tail.
	base := []fantasy.Message{fantasy.NewUserMessage("ask")}
	_, res1, err := step(context.Background(), steerOpts(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(res1.Messages) != 2 {
		t.Fatalf("step 1 messages = %d", len(res1.Messages))
	}

	// Step 2: fantasy rebuilds the slice (prompt + response messages); the
	// accepted message must be re-applied at its recorded position, once.
	rebuilt := []fantasy.Message{
		base[0],
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "working"}}},
	}
	_, res2, err := step(context.Background(), steerOpts(rebuilt))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range res2.Messages {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text == "steered guidance" {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("steer message appears %d times after re-application, want exactly 1", count)
	}

	// Step 3: a slice that ALREADY contains the injection must not double it.
	_, res3, err := step(context.Background(), steerOpts(res2.Messages))
	if err != nil {
		t.Fatal(err)
	}
	if res3.Messages != nil {
		joined := 0
		for _, m := range res3.Messages {
			for _, part := range m.Content {
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.Contains(p.Text, "steered guidance") {
					joined++
				}
			}
		}
		if joined != 1 {
			t.Fatalf("double injection on already-present slice: %d", joined)
		}
	}
}

func TestSteeringStep_NilSourceIsNil(t *testing.T) {
	if steeringStep(nil, &steerState{}, nil) != nil {
		t.Fatal("nil source must produce a nil step (chain skips it)")
	}
}

func TestSteeringStep_RollbackReRecordsTranscriptEntry(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{{ID: "in-1", Text: "steered guidance"}}}
	state := &steerState{}
	sink := newStreamSink(nil)
	step := steeringStep(source, state, sink)

	base := []fantasy.Message{fantasy.NewUserMessage("ask")}
	if _, _, err := step(context.Background(), steerOpts(base)); err != nil {
		t.Fatal(err)
	}

	// A resilience re-drive rolled the sink back past the injection point:
	// the user_text entry is gone, but the model WILL see the steered text
	// again on the retried attempt (re-application). The transcript entry
	// must come back with it — a committed transcript missing text the model
	// acted on would be a lie.
	sink.rollbackTo(sinkMark{})

	if _, _, err := step(context.Background(), steerOpts(base)); err != nil {
		t.Fatal(err)
	}
	entries, _ := sink.snapshot()
	count := 0
	for _, e := range entries {
		if e.Type == "user_text" && e.SteerID == "in-1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("user_text entries after rollback + re-apply = %d, want exactly 1", count)
	}
}

func TestSteeringStep_ReapplyNeverSplitsToolExchange(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{{ID: "in-1", Text: "steered guidance"}}}
	state := &steerState{}
	step := steeringStep(source, state, nil)

	// Accept at position 1 (after one message).
	if _, _, err := step(context.Background(), steerOpts([]fantasy.Message{fantasy.NewUserMessage("ask")})); err != nil {
		t.Fatal(err)
	}

	// Rebuilt slice where position 1 would land BETWEEN a tool call and its
	// results — the re-application must advance past the tool messages.
	rebuilt := []fantasy.Message{
		fantasy.NewUserMessage("ask"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "res"}},
		}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "thinking"}}},
	}
	_, res, err := step(context.Background(), steerOpts(rebuilt))
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages == nil {
		t.Fatal("expected re-application")
	}
	// The injected user message must not sit at index 1 (before the tool
	// result) — that would break call/result adjacency.
	if res.Messages[1].Role == fantasy.MessageRoleUser {
		t.Fatalf("steer injected between tool call and result: %+v", res.Messages)
	}
}
