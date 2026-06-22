package acpingress

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// pipePair returns one half of a duplex io.Pipe.
func pipePair() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// discardSink is an EventSink that drops everything (the web-path parity run
// streams to nothing; only the spy observer matters there).
type discardSink struct{}

func (discardSink) Emit(string, any) {}

// waitFor polls cond until true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// wired holds an in-memory IngressAgent ↔ fakeEditor pair connected over an
// io.Pipe duplex, plus the shared fakes, so each test drives the real agent the
// way an editor would.
type wired struct {
	agent  *IngressAgent
	editor *fakeEditor
	client *acp.ClientSideConnection
	store  *memStore
	runner *fakeRunner
	spy    *recordingObserver
	engine *fakeEngine
}

// setup wires the agent + editor over io.Pipe with the given scripted model and
// editor permission policy. Closing happens via t.Cleanup.
func setup(t *testing.T, model fantasy.LanguageModel, cfg Config) *wired {
	return setupOn(t, model, cfg, newMemStore())
}

// setupOn is setup over an EXISTING memStore — used by the resume tests to wire a
// SECOND IngressAgent (a fresh connection + empty in-mem session map) against the
// SAME persisted store, simulating a `fleet acp` restart.
func setupOn(t *testing.T, model fantasy.LanguageModel, cfg Config, st *memStore) *wired {
	t.Helper()
	clientToAgentR, clientToAgentW := pipePair()
	agentToClientR, agentToClientW := pipePair()

	runner := &fakeRunner{}
	spy := &recordingObserver{}
	eng := newFakeEngine(model)
	eng.spyObs = spy

	ia := New(eng, st, st, runner, cfg)
	agentConn := acp.NewAgentSideConnection(ia, agentToClientW, clientToAgentR)
	ia.SetAgentConnection(agentConn)

	editor := newFakeEditor()
	clientConn := acp.NewClientSideConnection(editor, clientToAgentW, agentToClientR)

	t.Cleanup(func() {
		_ = clientToAgentW.Close()
		_ = agentToClientW.Close()
	})

	return &wired{agent: ia, editor: editor, client: clientConn, store: st, runner: runner, spy: spy, engine: eng}
}

// initNewPrompt runs the standard initialize → new-session → prompt sequence and
// returns the prompt response.
func (w *wired) initNewPrompt(ctx context.Context, t *testing.T, userText string) acp.PromptResponse {
	t.Helper()
	if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	resp, err := w.client.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(userText)},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	return resp
}

func baseCfg() Config {
	return Config{Model: "scripted-model", Principal: Principal{Email: "op@fleet.local"}}
}

// TestRoundTrip: initialize → new-session → prompt streams session/update text +
// returns end_turn. This is the core ingress adapter proof.
func TestRoundTrip(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("hello from fleet over ACP")}}
	w := setup(t, model, baseCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp := w.initNewPrompt(ctx, t, "hi")

	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if got := w.editor.streamedText(); !strings.Contains(got, "hello from fleet over ACP") {
		t.Fatalf("streamed text = %q, want it to contain the model reply", got)
	}
	// A conversation was created + the turn persisted (audit/history fidelity).
	if len(w.store.convs) != 1 {
		t.Fatalf("conversations created = %d, want 1", len(w.store.convs))
	}
	for id, hist := range w.store.history {
		if len(hist) == 0 {
			t.Fatalf("conversation %s has empty persisted history", id)
		}
	}
}

// TestLockdownThreadsThroughTurn: when the ingress Config is locked down, the
// bound conversation is created lockdown=true AND every turn's TurnInput carries
// Lockdown=true — so a LockdownOnly server (which ORs into Config.Lockdown in
// cmd/fleet/acp.go) seals the per-turn sandbox the moment an editor connects.
// Guards the governance hole from issue #30.
func TestLockdownThreadsThroughTurn(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("sealed reply")}}
	cfg := baseCfg()
	cfg.Lockdown = true
	w := setup(t, model, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if resp := w.initNewPrompt(ctx, t, "hi"); resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	// The bound conversation was created locked down (CreateConversation lockdown arg).
	if len(w.store.convs) != 1 {
		t.Fatalf("conversations created = %d, want 1", len(w.store.convs))
	}
	for id, c := range w.store.convs {
		if !c.Lockdown {
			t.Fatalf("conversation %s created with lockdown=false, want true", id)
		}
	}
	// The turn ran with Lockdown set, so Manager.takeTurnSandbox forces the sealed
	// no-network sandbox.
	if !w.engine.lastTurnInput().Lockdown {
		t.Fatal("TurnInput.Lockdown = false, want true — ingress turn would run with network egress")
	}
}

// TestNoLockdownByDefault: the default ingress Config leaves lockdown off, so the
// conversation + turn are unlocked (the opt-in default; a LockdownOnly server or
// FLEET_ACP_LOCKDOWN flips it on via cmd/fleet/acp.go, exercised at that layer).
func TestNoLockdownByDefault(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("open reply")}}
	w := setup(t, model, baseCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if resp := w.initNewPrompt(ctx, t, "hi"); resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	for id, c := range w.store.convs {
		if c.Lockdown {
			t.Fatalf("conversation %s created with lockdown=true, want false by default", id)
		}
	}
	if w.engine.lastTurnInput().Lockdown {
		t.Fatal("TurnInput.Lockdown = true, want false by default")
	}
}

// TestGovernedToolCall: a prompt that triggers bash → it executes in the host
// sandbox via the REAL governed loop, a tool_call update streams to the editor
// (begin + completed), and the SAME observer events the web path emits are
// produced (tool.call + tool.result).
func TestGovernedToolCall(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("call-1", "echo hello-ingress"),
		textRound("done"),
	}}
	w := setup(t, model, baseCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp := w.initNewPrompt(ctx, t, "run echo")

	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	// The tool_call streamed to the editor and was marked completed.
	starts := w.editor.toolStarts()
	if len(starts) != 1 || starts[0] != "call-1" {
		t.Fatalf("tool_call starts = %v, want [call-1]", starts)
	}
	if status := w.editor.toolStatus("call-1"); status != string(acp.ToolCallStatusCompleted) {
		t.Fatalf("tool_call final status = %q, want completed", status)
	}

	// The SAME observer event vocabulary the web path emits is present.
	ev := w.spy.events()
	assertContains(t, ev, "tool.call")
	assertContains(t, ev, "tool.result")
	assertContains(t, ev, "text.delta")

	// The bash output reached the loop (host sandbox executed it): the run
	// produced a tool.result for the call.
	if !hasToolResult(w.spy, "call-1") {
		t.Fatalf("no tool.result observed for call-1; events=%v", ev)
	}
}

// TestPermissionApprove: a critical-tool turn (risky bash) → IngressAgent sends
// request_permission to the editor; on APPROVE the staged tool executes and the
// turn honors the decision. Asserts the approve path end-to-end.
func TestPermissionApprove(t *testing.T) {
	// `git push` trips the interactive risky-bash gate (classifyRiskyBash), so the
	// turn stages a critical-tool approval — the SAME gate the web path uses.
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("push-1", "git push origin main"),
		textRound("pushed"),
	}}
	w := setup(t, model, baseCfg())
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return allowResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp := w.initNewPrompt(ctx, t, "clean tmp")

	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if w.editor.permissionCount() != 1 {
		t.Fatalf("permission requests = %d, want exactly 1 (no approve-all)", w.editor.permissionCount())
	}
	// The outbound request offered exactly two options decided on their own
	// merits — allow_once + reject_once, NO allow_always "approve all".
	if req, ok := w.editor.lastPermReq(); !ok {
		t.Fatalf("no permission request captured")
	} else {
		if len(req.Options) != 2 {
			t.Fatalf("permission options = %d, want 2 (allow/reject, no approve-all)", len(req.Options))
		}
		for _, o := range req.Options {
			if o.Kind == acp.PermissionOptionKindAllowAlways || o.Kind == acp.PermissionOptionKindRejectAlways {
				t.Fatalf("permission offered an *_always option (%s) — approve-all is forbidden", o.Kind)
			}
		}
	}
	// Approve path: the approval row was claimed approved and the staged tool ran.
	appr := w.store.approvalByTool("bash")
	if appr == nil {
		t.Fatalf("no bash approval staged")
	}
	if appr.Status != "approved" {
		t.Fatalf("approval status = %q, want approved", appr.Status)
	}
	if ran := w.runner.executed(); len(ran) != 1 || ran[0] != "bash" {
		t.Fatalf("staged tools executed = %v, want [bash]", ran)
	}
	// The editor's tool-call card was flipped to COMPLETED by the post-approval
	// resolution (regression guard: the in-loop APPROVAL_REQUIRED block streamed a
	// FAILED status; a successful approve+execute must correct it to completed).
	if status := w.editor.toolStatus("push-1"); status != string(acp.ToolCallStatusCompleted) {
		t.Fatalf("tool_call final status = %q, want completed after approve+execute", status)
	}
}

// TestPermissionDenyOnReject: same critical-tool turn but the editor REJECTS →
// default-deny: the approval is claimed rejected and the staged tool does NOT
// run.
func TestPermissionDenyOnReject(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("push-1", "git push origin main"),
		textRound("ok then"),
	}}
	w := setup(t, model, baseCfg())
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return rejectResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp := w.initNewPrompt(ctx, t, "clean tmp")

	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if w.editor.permissionCount() != 1 {
		t.Fatalf("permission requests = %d, want 1", w.editor.permissionCount())
	}
	appr := w.store.approvalByTool("bash")
	if appr == nil || appr.Status != "rejected" {
		t.Fatalf("approval = %+v, want status rejected", appr)
	}
	if ran := w.runner.executed(); len(ran) != 0 {
		t.Fatalf("staged tools executed = %v, want none (denied)", ran)
	}
}

// TestPermissionDenyOnTimeout: the editor blocks past the approver's timeout →
// default-DENY (no hang, no allow). Uses a tiny PermissionTimeout so the test is
// fast.
func TestPermissionDenyOnTimeout(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("push-1", "git push origin main"),
		textRound("ok"),
	}}
	cfg := baseCfg()
	cfg.PermissionTimeout = 150 * time.Millisecond
	w := setup(t, model, cfg)
	// The editor never answers — it blocks until ITS ctx is cancelled, which
	// happens when the agent's permission ctx hits the timeout and the RPC is
	// abandoned. Returning a cancelled outcome here would also be a deny; the
	// point is the agent does not hang and defaults to deny.
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		<-time.After(5 * time.Second)
		return allowResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	resp := w.initNewPrompt(ctx, t, "clean tmp")
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("turn took %v — the permission request appears to have hung instead of default-denying", elapsed)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	appr := w.store.approvalByTool("bash")
	if appr == nil || appr.Status != "rejected" {
		t.Fatalf("approval = %+v, want status rejected (default-deny on timeout)", appr)
	}
	if ran := w.runner.executed(); len(ran) != 0 {
		t.Fatalf("staged tools executed = %v, want none (timed out → deny)", ran)
	}
}

// TestPreviewEmailAutoDismiss: preview_email is a DISPLAY-ONLY card the in-loop
// gate tells the agent needs NO approval (Dismiss-only). Ingress must therefore
// NOT pop an Allow/Reject request_permission for it — it auto-resolves. Asserts
// no permission request was sent and the preview was dismissed (not denied).
func TestPreviewEmailAutoDismiss(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		toolRound("pv-1", "preview_email", `{"to_email":"a@b.com","subject":"hi","content":"<p>hi</p>"}`),
		textRound("here is your draft"),
	}}
	w := setup(t, model, baseCfg())
	// If this fires, the test fails — preview_email must not ask the human.
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		t.Errorf("preview_email triggered a request_permission; it is display-only")
		return rejectResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp := w.initNewPrompt(ctx, t, "draft an email")

	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if w.editor.permissionCount() != 0 {
		t.Fatalf("permission requests = %d, want 0 (preview_email is display-only)", w.editor.permissionCount())
	}
	// The preview was auto-resolved as approved (a dismissal), and the staged
	// runner returned the dismissal text — no real send.
	appr := w.store.approvalByTool("preview_email")
	if appr == nil || appr.Status != "approved" {
		t.Fatalf("preview approval = %+v, want status approved (auto-dismiss)", appr)
	}
}

// TestCancelDuringPermission: a session Cancel while a critical-tool approval is
// waiting for the human → the in-flight request_permission default-DENIES (the
// turn ctx propagates to the permission wait), and the staged tool does not run.
func TestCancelDuringPermission(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		bashRound("push-1", "git push origin main"),
		textRound("done"),
	}}
	w := setup(t, model, baseCfg())

	asked := make(chan struct{})
	var once sync.Once
	// The editor receives the request, signals it, then blocks forever — only the
	// turn ctx cancel (via the agent's permission wait) can unblock it → deny.
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		once.Do(func() { close(asked) })
		<-time.After(30 * time.Second)
		return allowResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	done := make(chan acp.PromptResponse, 1)
	go func() {
		resp, _ := w.client.Prompt(ctx, acp.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("push it")},
		})
		done <- resp
	}()

	// Once the human is being asked, cancel the session — the in-flight permission
	// wait must default-deny rather than hang.
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatalf("permission was never requested")
	}
	if err := w.client.Cancel(ctx, acp.CancelNotification{SessionId: sess.SessionId}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("prompt hung after cancel — the permission wait did not honor the turn ctx")
	}
	if ran := w.runner.executed(); len(ran) != 0 {
		t.Fatalf("staged tools executed = %v, want none (cancelled during approval → deny)", ran)
	}
	appr := w.store.approvalByTool("bash")
	if appr == nil || appr.Status != "rejected" {
		t.Fatalf("approval = %+v, want status rejected (cancel during approval → default-deny)", appr)
	}
}

// TestCancellation: cancel mid-turn → StopReasonCancelled + the partial
// transcript is persisted. The model blocks on the first round until Cancel
// fires the session's run ctx.
func TestCancellation(t *testing.T) {
	// release is never closed: the only way the model's stream unblocks is the
	// session cancel firing turnCtx (see the waitFor + Cancel below).
	model := &blockingModel{release: make(chan struct{})}
	w := setup(t, model, baseCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// Fire the prompt in the background; cancel once the model is mid-stream.
	type result struct {
		resp acp.PromptResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, perr := w.client.Prompt(ctx, acp.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("long task")},
		})
		done <- result{resp, perr}
	}()

	// Wait until the model has emitted its first chunk (the editor saw text),
	// then cancel the session. We deliberately do NOT close(released): the cancel
	// is the ONLY unblock, so the model exits via <-ctx.Done() and the turn
	// cancels deterministically (no race between release + cancel).
	waitFor(t, 5*time.Second, func() bool { return w.editor.streamedText() != "" })
	if err := w.client.Cancel(ctx, acp.CancelNotification{SessionId: sess.SessionId}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("prompt returned error: %v", r.err)
	}
	if r.resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", r.resp.StopReason)
	}
	// Partial transcript persisted: the conversation history is non-empty
	// (at least the user message + the partial assistant text).
	persisted := 0
	for _, h := range w.store.history {
		persisted += len(h)
	}
	if persisted == 0 {
		t.Fatalf("no partial transcript persisted on cancel")
	}
}

// TestParity_ObserverSequence: an ingress turn produces the SAME ordered
// observer event sequence a web interactive turn produces for an identical
// scripted run. We assert the ingress turn's observed events match what
// agent.RunInteractiveTurn emits directly (the web path's loop), proving ingress
// governs through the identical loop — only the I/O surface differs.
func TestParity_ObserverSequence(t *testing.T) {
	script := func() *scriptedModel {
		return &scriptedModel{rounds: [][]fantasy.StreamPart{
			bashRound("p1", "echo parity"),
			textRound("parity done"),
		}}
	}

	// 1. Ingress turn: capture the observer events via the spy.
	w := setup(t, script(), baseCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w.initNewPrompt(ctx, t, "echo parity")
	ingressEvents := nonStreamingEvents(w.spy.events())

	// 2. Web path: drive agent.RunInteractiveTurn directly with the SAME script
	//    + a host sandbox, capturing its observer events.
	webObs := &recordingObserver{}
	eng := newFakeEngine(script())
	eng.spyObs = webObs
	// Reuse the engine's RunTurn (it IS agent.RunInteractiveTurn under the hood —
	// the same call Manager.RunTurn makes).
	if _, err := eng.RunTurn(ctx, agent.TurnInput{UserMessage: "echo parity"}, discardSink{}); err != nil {
		t.Fatalf("web-path run: %v", err)
	}
	webEvents := nonStreamingEvents(webObs.events())

	if strings.Join(ingressEvents, ",") != strings.Join(webEvents, ",") {
		t.Fatalf("observer event sequence mismatch:\n ingress=%v\n web    =%v", ingressEvents, webEvents)
	}
}

// blockingModel streams one chunk then blocks until release is closed (or ctx is
// cancelled), to exercise mid-turn cancellation.
type blockingModel struct{ release chan struct{} }

func (m *blockingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: "ok"}}, FinishReason: fantasy.FinishReasonStop}, nil
}
func (m *blockingModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial..."}) {
			return
		}
		select {
		case <-m.release:
			// Released without cancellation: finish cleanly.
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		case <-ctx.Done():
			// Cancelled mid-stream: surface a stream error carrying the ctx error.
			// agentcore.Run sees ctx.Err() != nil and returns the partial transcript
			// with Cancelled=true (exactly the production cancel contract).
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		}
	}, nil
}
func (m *blockingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.Canceled
}
func (m *blockingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.Canceled
}
func (m *blockingModel) Provider() string { return "blocking" }
func (m *blockingModel) Model() string    { return "scripted-model" }

// ── small helpers ──

func assertContains(t *testing.T, haystack []string, want string) {
	t.Helper()
	for _, h := range haystack {
		if h == want {
			return
		}
	}
	t.Fatalf("event %q not found in %v", want, haystack)
}

func hasToolResult(o *recordingObserver, id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, e := range o.raw {
		if e.kind == "tool.result" {
			if got, _ := e.payload["id"].(string); got == id {
				return true
			}
		}
	}
	return false
}

// nonStreamingEvents drops per-chunk text/reasoning deltas (whose COUNT can vary
// with chunking) and keeps the structural event sequence the parity assertion
// cares about (tool.call / tool.result / etc.), collapsing repeats.
func nonStreamingEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if e == "text.delta" || e == "reasoning.delta" || e == "reasoning.start" || e == "reasoning.end" {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == e {
			continue
		}
		out = append(out, e)
	}
	return out
}

var _ = agentcore.ModeInteractive
