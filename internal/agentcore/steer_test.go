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

// steerAssistant / countSteerTexts are shared helpers for the #1125 dedupe
// tests below.
func steerAssistant(text string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}}
}

func countSteerTexts(messages []fantasy.Message, text string) int {
	count := 0
	for _, m := range messages {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text == text {
				count++
			}
		}
	}
	return count
}

// TestSteeringStep_ShiftedHistoryDoesNotDoubleInject pins the #1125 dedupe
// fix: a shortened rebuild can move an already-present injected message MORE
// than one slot from its recorded position (while staying at/after the
// injection floor, where injected steers live), and the old pos/pos-1-only
// probe then re-inserted the same text as a duplicate user message in the
// provider input.
func TestSteeringStep_ShiftedHistoryDoesNotDoubleInject(t *testing.T) {
	source := &fakeSteerSource{}
	state := &steerState{}
	step := steeringStep(source, state, nil)

	// First invocation fixes the injection floor at 2 (the seed history);
	// nothing is queued yet, so nothing is accepted.
	base := []fantasy.Message{fantasy.NewUserMessage("ask"), steerAssistant("working")}
	if _, _, err := step(context.Background(), steerOpts(base)); err != nil {
		t.Fatal(err)
	}

	// The steer arrives mid-stream and is accepted at the tail of a GROWN
	// entry slice: recorded pos = 5.
	source.mu.Lock()
	source.pending = append(source.pending, SteerMessage{ID: "in-1", Text: "steered guidance"})
	source.mu.Unlock()
	grown := append(append([]fantasy.Message{}, base...), steerAssistant("a1"), steerAssistant("a2"), steerAssistant("a3"))
	if _, _, err := step(context.Background(), steerOpts(grown)); err != nil {
		t.Fatal(err)
	}

	// A shortened rebuild kept the injected message but moved it THREE slots
	// below its recorded position, still at/after the floor — exactly the
	// shape the positional probe alone misses.
	shifted := []fantasy.Message{
		base[0],
		base[1],
		fantasy.NewUserMessage("steered guidance"),
		steerAssistant("tail-1"),
	}
	_, res, err := step(context.Background(), steerOpts(shifted))
	if err != nil {
		t.Fatal(err)
	}
	final := shifted
	if res.Messages != nil {
		final = res.Messages
	}
	if got := countSteerTexts(final, "steered guidance"); got != 1 {
		t.Fatalf("steer message appears %d times after a >1-slot shift, want exactly 1 (no duplicate injection)", got)
	}
}

// TestSteeringStep_PriorHistoryByteCollisionStillInjects pins the injection
// floor: a steer whose text byte-equals a PRIOR-history user message (the
// user's last turn was "continue"; they steer "continue" mid-run) must still
// be re-injected on every step. Without the floor, the content scan claims
// the historical message and permanently suppresses the steer — the model
// never sees it again after the accepting step.
func TestSteeringStep_PriorHistoryByteCollisionStillInjects(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{{ID: "in-1", Text: "continue"}}}
	state := &steerState{}
	step := steeringStep(source, state, nil)

	// Seed history already carries a byte-identical user message.
	base := []fantasy.Message{fantasy.NewUserMessage("continue"), steerAssistant("done so far")}
	if _, _, err := step(context.Background(), steerOpts(base)); err != nil {
		t.Fatal(err)
	}

	// Next step: fantasy rebuilt the entry without the injection; the only
	// "continue" present is the prior-history one, BELOW the floor. It must
	// not satisfy the dedupe.
	rebuilt := append(append([]fantasy.Message{}, base...), steerAssistant("more work"))
	_, res, err := step(context.Background(), steerOpts(rebuilt))
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages == nil {
		t.Fatal("steer suppressed by a prior-history byte collision: expected re-injection")
	}
	if got := countSteerTexts(res.Messages, "continue"); got != 2 {
		t.Fatalf("%d copies of the text present, want 2 (the prior-history message AND the injected steer)", got)
	}
}

// TestSteeringStep_IdenticalTextSteersEachKeepTheirCopy guards the claim
// tracking inside the content-scan fallback: two accepted steers with
// byte-identical text must each account for their own copy — one present copy
// satisfies only one of them, so the other is restored rather than deduped
// away, and two present copies are never tripled.
func TestSteeringStep_IdenticalTextSteersEachKeepTheirCopy(t *testing.T) {
	source := &fakeSteerSource{pending: []SteerMessage{
		{ID: "in-1", Text: "keep going"},
		{ID: "in-2", Text: "keep going"},
	}}
	state := &steerState{}
	step := steeringStep(source, state, nil)

	countSteers := func(messages []fantasy.Message) int {
		count := 0
		for _, m := range messages {
			if m.Role != fantasy.MessageRoleUser {
				continue
			}
			for _, part := range m.Content {
				if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && p.Text == "keep going" {
					count++
				}
			}
		}
		return count
	}

	base := []fantasy.Message{fantasy.NewUserMessage("ask")}
	// Step 1 accepts in-1; step 2 re-applies in-1 and accepts in-2.
	if _, _, err := step(context.Background(), steerOpts(base)); err != nil {
		t.Fatal(err)
	}
	_, res2, err := step(context.Background(), steerOpts(base))
	if err != nil {
		t.Fatal(err)
	}
	if got := countSteers(res2.Messages); got != 2 {
		t.Fatalf("after accepting two identical steers, %d copies present, want 2", got)
	}

	// A rebuilt slice already carrying BOTH copies gains nothing.
	_, res3, err := step(context.Background(), steerOpts(res2.Messages))
	if err != nil {
		t.Fatal(err)
	}
	if res3.Messages != nil {
		if got := countSteers(res3.Messages); got != 2 {
			t.Fatalf("both copies present, re-application produced %d copies, want 2", got)
		}
	}

	// A rebuilt slice carrying only ONE copy gets the second restored — the
	// single copy satisfies one accepted steer, not both.
	oneCopy := []fantasy.Message{
		fantasy.NewUserMessage("ask"),
		fantasy.NewUserMessage("keep going"),
	}
	_, res4, err := step(context.Background(), steerOpts(oneCopy))
	if err != nil {
		t.Fatal(err)
	}
	if res4.Messages == nil {
		t.Fatal("expected the missing second copy to be re-applied")
	}
	if got := countSteers(res4.Messages); got != 2 {
		t.Fatalf("one copy present, re-application produced %d copies, want 2", got)
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
