package acpingress

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"
)

// TestInitializeAdvertisesLoadSession: the agent now advertises loadSession +
// resume so editors know they can reconnect to a prior conversation.
func TestInitializeAdvertisesLoadSession(t *testing.T) {
	w := setup(t, &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("ok")}}, baseCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !resp.AgentCapabilities.LoadSession {
		t.Error("LoadSession capability must be advertised true")
	}
	if resp.AgentCapabilities.SessionCapabilities.Resume == nil {
		t.Error("Resume capability must be advertised")
	}
}

// newSessionID drives initialize→new-session and returns the SessionId.
func (w *wired) newSession(ctx context.Context, t *testing.T) acp.SessionId {
	t.Helper()
	if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	return sess.SessionId
}

// TestSessionIdIsConversationId: the SessionId is the durable conversation ID.
func TestSessionIdIsConversationId(t *testing.T) {
	w := setup(t, &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("ok")}}, baseCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid := w.newSession(ctx, t)
	if len(w.store.convs) != 1 {
		t.Fatalf("conversations = %d, want 1", len(w.store.convs))
	}
	if _, ok := w.store.convs[string(sid)]; !ok {
		t.Fatalf("SessionId %q is not a conversation id (%v)", sid, keysOf(w.store.convs))
	}
}

// TestResumeAcrossRestart: a second IngressAgent over the SAME store (simulating a
// `fleet acp` restart with an empty in-mem session map) can LoadSession by the
// prior SessionId, replays the persisted transcript to the editor (including a
// tool card with exactly one start + one terminal status), and a follow-up Prompt
// continues the same conversation instead of erroring "session not found".
func TestResumeAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Turn 1 on the first agent: a bash tool call then a final reply, so history
	// holds a user msg + a tool_call/tool_result pair + assistant text.
	model1 := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("call-1", "echo hi"),
		textRound("done with the work"),
	}}
	w1 := setup(t, model1, baseCfg())
	sid := w1.newSession(ctx, t)
	if _, err := w1.client.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("do it")}}); err != nil {
		t.Fatalf("turn 1 prompt: %v", err)
	}

	// "Restart": a brand-new agent + editor over the SAME store; empty in-mem sessions.
	model2 := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("second turn reply")}}
	w2 := setupOn(t, model2, baseCfg(), w1.store)
	if _, err := w2.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("w2 initialize: %v", err)
	}
	if _, err := w2.client.LoadSession(ctx, acp.LoadSessionRequest{SessionId: sid, Cwd: "/workspace", McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("LoadSession across restart: %v", err)
	}

	// The prior assistant text was replayed to the reconnecting editor.
	if got := w2.editor.streamedText(); !strings.Contains(got, "done with the work") {
		t.Errorf("replay missing prior assistant text; got %q", got)
	}
	// The tool card replayed exactly once with a terminal status (no double-render).
	if starts := w2.editor.toolStarts(); len(starts) != 1 || starts[0] != "call-1" {
		t.Errorf("replayed tool starts = %v, want [call-1]", starts)
	}
	if st := w2.editor.toolStatus("call-1"); st != string(acp.ToolCallStatusCompleted) {
		t.Errorf("replayed tool status = %q, want completed", st)
	}

	// A follow-up Prompt on the resumed session works (no cold-start "not found").
	resp, err := w2.client.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("continue")}})
	if err != nil {
		t.Fatalf("post-resume prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("post-resume stop = %q, want end_turn", resp.StopReason)
	}
}

// TestResumeSessionNoReplay: ResumeSession rebinds WITHOUT replaying history (the
// spec contract — only loadSession replays), and a follow-up Prompt still works.
func TestResumeSessionNoReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w1 := setup(t, &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("first reply")}}, baseCfg())
	sid := w1.newSession(ctx, t)
	if _, err := w1.client.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}

	w2 := setupOn(t, &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("resumed reply")}}, baseCfg(), w1.store)
	if _, err := w2.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := w2.client.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: sid, Cwd: "/workspace"}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if got := w2.editor.streamedText(); got != "" {
		t.Errorf("ResumeSession must NOT replay history; editor got %q", got)
	}
	if _, err := w2.client.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("more")}}); err != nil {
		t.Fatalf("post-resume prompt: %v", err)
	}
}

// TestLoadUnknownSession: loading a bogus id is an error, not a silent empty success.
func TestLoadUnknownSession(t *testing.T) {
	w := setup(t, &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("ok")}}, baseCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := w.client.LoadSession(ctx, acp.LoadSessionRequest{SessionId: acp.SessionId("does-not-exist"), Cwd: "/workspace", McpServers: []acp.McpServer{}}); err == nil {
		t.Fatal("LoadSession on an unknown id must return an error")
	}
}

func keysOf[V any](m map[string]V) []string { // small helper for the failure message
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
