package agentcore

// The policy classifies a call by its tool's own payload, not by the text the
// model sees after fleet's notes and post_tool_use fragments are joined to it.
// A fragment appended to {"status_code":400} used to leave nothing for the
// classifiers to parse, so the send counted as delivered: a refused send
// blocked its safe retry, and a 5xx send was reported as sent instead of
// uncertain.

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

const hookFragment = "formatter: clean"

// withPostToolHook configures one post_tool_use hook that annotates every
// result, and returns its engine.
func withPostToolHook(t *testing.T) *hookEngine {
	t.Helper()
	return engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"` + hookFragment + `"}`}, nil,
		LifecycleHook{ID: "fmt", Event: HookPostToolUse, Matcher: "*", Command: "c"})
}

// sendThroughMCP runs one send_email through mcpTool with the hook engine,
// answering with result (succeeded at the transport), and returns what the
// model saw.
func sendThroughMCP(t *testing.T, p Policy, hooks *hookEngine, input, result string) fantasy.ToolResponse {
	t.Helper()
	tool := &mcpTool{serverName: "ses", tool: mcp.Tool{Name: "send_email"}, broker: &recordingBroker{text: result}, policy: p, hooks: hooks}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c-1", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEmailWithPostHook_ClassifiesTheProviderPayload(t *testing.T) {
	cases := []struct {
		name    string
		result  string
		wantMsg string // "" means the identical retry goes out
	}{
		{"provider refused (400)", `{"status_code":400,"error":"invalid recipient"}`, ""},
		{"provider failed (500)", `{"status_code":500,"error":"internal error"}`, maybeSentText},
		{"provider accepted (200)", `{"status_code":200}`, alreadySentText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hooks := withPostToolHook(t)
			p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
			resp := sendThroughMCP(t, p, hooks, inflightEmailInput, tc.result)
			if !strings.Contains(resp.Content, hookFragment) {
				t.Fatalf("the model did not see the hook fragment: %q", resp.Content)
			}
			blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-2", inflightEmailInput)
			switch {
			case tc.wantMsg == "" && blocked:
				t.Fatalf("identical retry refused: %q", msg)
			case tc.wantMsg != "" && (!blocked || !strings.Contains(msg, tc.wantMsg)):
				t.Fatalf("identical retry: blocked=%v msg=%q, want a refusal containing %q", blocked, msg, tc.wantMsg)
			}
		})
	}
}

// fleet's own date-window note is joined the same way, and is kept out of the
// policy record the same way.
func TestEmailWithDateWindowNote_ClassifiesTheProviderPayload(t *testing.T) {
	p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
	input := `{"to_email":"ops@example.com","subject":"weekly report","content":"<p>numbers</p>","date_to":"2020-01-31"}`
	resp := sendThroughMCP(t, p, nil, input, `{"status_code":400,"error":"invalid recipient"}`)
	if !strings.Contains(resp.Content, "[fleet date-window]") {
		t.Fatalf("the model did not see the date-window note: %q", resp.Content)
	}
	if blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-2", input); blocked {
		t.Fatalf("identical retry after a refused send refused: %q", msg)
	}
}

// A scheduled run's accounting reads the payload too: a send accepted under a
// post_tool_use hook discharges its commitment and counts as the run's send.
func TestScheduledEmailWithPostHook_DischargesOnTheProviderPayload(t *testing.T) {
	hooks := withPostToolHook(t)
	sp := NewScheduledPolicy(&LogSession{}, 0, 0)
	o := sp.orch
	o.registerCommittedActions([]string{"send_email: weekly report to ops"})
	o.auditConfirmed = true
	sendThroughMCP(t, sp, hooks, inflightEmailInput, `{"status_code":202}`)

	o.mu.Lock()
	owed, sent := o.unexecutedCommitments(), o.sendEmailSuccessCount
	o.mu.Unlock()
	if len(owed) != 0 || sent != 1 {
		t.Fatalf("after an accepted send: owed=%v sent=%d, want the commitment discharged and one send counted", owed, sent)
	}
	blocked, msg := sp.BeforeToolCall(inflightEmailTool, "c-2", inflightEmailInput)
	if !blocked || !strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
		t.Fatalf("identical resend: blocked=%v msg=%q, want the already-sent suppression", blocked, msg)
	}
}

// A failed call records its bounded bytes, still without the annotations.
func TestPolicyRecordText(t *testing.T) {
	governed := fantasy.NewTextErrorResponse(`{"status_code":500}`)
	annotated := governed
	annotated.Content = appendHookContext(governed.Content, "[hook:fmt] x")
	if got := policyRecordText(governed, annotated, annotated, true, maxToolOutputBytes()); got != governed.Content {
		t.Errorf("failed call with a whole annotated text recorded %q, want the payload", got)
	}
	// The annotations pushed the model-visible text past the limit, but the
	// payload alone passes it whole: the payload is recorded.
	envelope := annotated
	envelope.Content = "[truncated envelope]"
	if got := policyRecordText(governed, annotated, envelope, true, maxToolOutputBytes()); got != governed.Content {
		t.Errorf("failed call whose payload fits on its own recorded %q, want the payload", got)
	}
	// The payload alone is past the limit: the envelope is all there is.
	huge := fantasy.NewTextErrorResponse(strings.Repeat("upstream failure detail ", HardMaxToolOutputBytes/24+1))
	hugeAnnotated := huge
	hugeAnnotated.Content = appendHookContext(huge.Content, "[hook:fmt] x")
	if got := policyRecordText(huge, hugeAnnotated, envelope, true, maxToolOutputBytes()); got != envelope.Content {
		t.Errorf("failed call whose payload is past the limit recorded %d bytes, want the bounded envelope", len(got))
	}
	// The answer follows the limit the response was bounded with, not the
	// live one: lowering the live limit afterwards changes nothing.
	fits := fantasy.NewTextErrorResponse(`{"status_code":500,"detail":"` + strings.Repeat("x", 2*MinMaxToolOutputBytes) + `"}`)
	fitsAnnotated := fits
	fitsAnnotated.Content = appendHookContext(fits.Content, "[hook:fmt] x")
	boundedWith := maxToolOutputBytes()
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(MinMaxToolOutputBytes)
	if got := policyRecordText(fits, fitsAnnotated, envelope, true, boundedWith); got != fits.Content {
		t.Errorf("a limit lowered after bounding changed the record to %d bytes, want the payload", len(got))
	}
	if got := policyRecordText(fits, fitsAnnotated, envelope, true, maxToolOutputBytes()); got != envelope.Content {
		t.Errorf("under the lowered limit the payload does not fit: recorded %d bytes, want the envelope", len(got))
	}
	SetMaxToolOutputBytes(-1)
	if got := policyRecordText(governed, governed, governed, true, maxToolOutputBytes()); got != governed.Content {
		t.Errorf("failed call with no annotation recorded %q", got)
	}
	ok := fantasy.NewTextResponse(`{"status_code":200}`)
	okAnnotated := ok
	okAnnotated.Content = appendHookContext(ok.Content, "[hook:fmt] x")
	if got := policyRecordText(ok, okAnnotated, okAnnotated, false, maxToolOutputBytes()); got != ok.Content {
		t.Errorf("successful call recorded %q, want the payload", got)
	}
}

// Under a small output limit a long hook fragment can push a failed send past
// the model-visible limit although its payload fits: the model then gets a
// truncation envelope, but the policy still classifies the payload, so a 502
// stays "may have been sent" and the identical retry is refused.
func TestEmailWithPostHook_LongFragmentUnderSmallLimit(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(MinMaxToolOutputBytes)
	fragment := strings.TrimSpace(strings.Repeat("lint ok ", 100)) // ~800 bytes
	hooks := engineWith(t, &scriptExecutor{out: `{"decision":"continue","additional_context":"` + fragment + `"}`}, nil,
		LifecycleHook{ID: "fmt", Event: HookPostToolUse, Matcher: "*", Command: "c"})
	payload := `{"status_code":502,"error":"bad gateway","detail":"` + strings.TrimSpace(strings.Repeat("upstream did not answer in time ", 11)) + `"}`
	if len(payload) >= MinMaxToolOutputBytes || len(payload)+len(fragment) <= MinMaxToolOutputBytes {
		t.Fatalf("setup: payload %d bytes, fragment %d bytes; need the payload to fit alone and not with the fragment", len(payload), len(fragment))
	}
	p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
	tool := &mcpTool{serverName: "ses", tool: mcp.Tool{Name: "send_email"}, broker: &recordingBroker{text: payload, isErr: true}, policy: p, hooks: hooks}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c-1", Input: inflightEmailInput})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Content, fragment) {
		t.Fatalf("setup: the model-visible text was not cut (%d bytes)", len(resp.Content))
	}
	blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-2", inflightEmailInput)
	if !blocked || !strings.Contains(msg, maybeSentText) {
		t.Fatalf("identical retry after a 502: blocked=%v msg=%q, want the may-have-been-sent refusal", blocked, msg)
	}
}

// The session-log readers recognise fleet's notes by their opening tag
// (StartsWithFleetNote), so every note fleet joins to a result must open with
// a listed one.
func TestFleetNotesOpenWithAListedTag(t *testing.T) {
	frag := withPostToolHook(t).postToolUse(context.Background(), "bash", "c-1", `{}`, "ok", false)
	notes := dateWindowNotes("mcp_gmail_search_emails", `{"query":"from:a@example.com","date_to":"2020-01-31"}`, `{"matches_found":0}`, runtimeNow())
	lines := append([]string{frag}, strings.Split(notes, "\n")...)
	if len(lines) < 3 {
		t.Fatalf("setup: want a hook fragment and both date-window notes, got %q", lines)
	}
	for _, line := range lines {
		if !StartsWithFleetNote(line) {
			t.Errorf("fleet note %q opens with no listed tag", line)
		}
	}
	for _, other := range []string{"[tool error] write failed", "[1, 2]", "hook: x", ""} {
		if StartsWithFleetNote(other) {
			t.Errorf("%q read as a fleet note", other)
		}
	}
}
