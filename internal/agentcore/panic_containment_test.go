package agentcore

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/guardrail"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/piiredact"
	"github.com/ElcanoTek/fleet/internal/safe"
)

type panicTestInput struct{}

type passPolicy struct{}

func (passPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (passPolicy) RecordToolResult(string, string, string, bool)        {}
func (passPolicy) CanFinish(int) (bool, []string)                       { return true, nil }

type recordedPolicyResult struct {
	name      string
	input     string
	result    string
	succeeded bool
}

type recordingPanicPolicy struct {
	mu      sync.Mutex
	records []recordedPolicyResult
}

func (*recordingPanicPolicy) BeforeToolCall(string, string, string) (bool, string) {
	return false, ""
}
func (p *recordingPanicPolicy) RecordToolResult(name, input, result string, succeeded bool) {
	p.mu.Lock()
	p.records = append(p.records, recordedPolicyResult{name: name, input: input, result: result, succeeded: succeeded})
	p.mu.Unlock()
}
func (*recordingPanicPolicy) CanFinish(int) (bool, []string) { return true, nil }

func (p *recordingPanicPolicy) recordsFor(name string) []recordedPolicyResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []recordedPolicyResult
	for _, record := range p.records {
		if record.name == name {
			out = append(out, record)
		}
	}
	return out
}

func newPanickingTool(name, panicValue string) fantasy.AgentTool {
	fn := func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		panic(panicValue)
	}
	return fantasy.NewAgentTool(name, "panics for containment tests", fn)
}

type panicEventCollector struct {
	mu     sync.Mutex
	events []safe.PanicEvent
}

func capturePanicEvents(t *testing.T) *panicEventCollector {
	t.Helper()
	c := &panicEventCollector{}
	previous := safe.PanicEventHook
	safe.PanicEventHook = func(event safe.PanicEvent, _ any) {
		c.mu.Lock()
		c.events = append(c.events, event)
		c.mu.Unlock()
	}
	t.Cleanup(func() { safe.PanicEventHook = previous })
	return c
}

func (c *panicEventCollector) snapshot() []safe.PanicEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]safe.PanicEvent(nil), c.events...)
}

var incidentPattern = regexp.MustCompile(`inc_[0-9a-f]{32}`)

func assertOpaquePanicResponse(t *testing.T, response fantasy.ToolResponse, rawPanic string) string {
	t.Helper()
	if !response.IsError {
		t.Fatalf("panic response IsError=false: %+v", response)
	}
	incident := incidentPattern.FindString(response.Content)
	if incident == "" {
		t.Fatalf("panic response has no incident ID: %q", response.Content)
	}
	if strings.Contains(response.Content, rawPanic) || strings.Contains(response.Content, "goroutine") {
		t.Fatalf("model-visible response leaked panic/stack: %q", response.Content)
	}
	if !strings.Contains(response.Metadata, incident) || !strings.Contains(response.Metadata, `"possibly_committed":true`) {
		t.Fatalf("response metadata %q does not conservatively mark incident %s", response.Metadata, incident)
	}
	return incident
}

type panickingBroker struct{ value string }

func (b panickingBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	panic(b.value)
}

type textBroker struct{ text string }

func (b textBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return b.text, false, nil
}

type errorBroker struct{ err error }

func (b errorBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return "", false, b.err
}

type panickingError struct{}

func (panickingError) Error() string { panic("error method raw panic") }

func TestUniversalPanicBoundary_CoversEveryToolRegistrationRoute(t *testing.T) {
	const rawPanic = "route panic must remain operator-only"
	attribution := panicAttribution{
		runMode:        ModeScheduled.String(),
		taskID:         "task-route",
		conversationID: "conversation-route",
	}

	tests := []struct {
		name         string
		buildAndFind func(t *testing.T, policy Policy) (fantasy.AgentTool, fantasy.ToolCall, string)
		wantTool     string
	}{
		{
			name: "native",
			buildAndFind: func(t *testing.T, policy Policy) (fantasy.AgentTool, fantasy.ToolCall, string) {
				tools, err := buildFantasyTools(
					[]fantasy.AgentTool{newPanickingTool("native_panic", rawPanic)},
					nil, nil, nil, policy, nil, nil,
					toolBuildConfig{panicAttribution: attribution},
				)
				if err != nil {
					t.Fatal(err)
				}
				return findTool(tools, "native_panic"), fantasy.ToolCall{ID: "call-native", Input: "{}"}, rawPanic
			},
			wantTool: "native_panic",
		},
		{
			name: "loader",
			buildAndFind: func(t *testing.T, policy Policy) (fantasy.AgentTool, fantasy.ToolCall, string) {
				cfg := toolBuildConfig{
					loaderTools:      []fantasy.AgentTool{newPanickingTool("loader_panic", rawPanic)},
					panicAttribution: attribution,
				}
				tools, err := buildFantasyTools(nil, nil, nil, nil, policy, nil, nil, cfg)
				if err != nil {
					t.Fatal(err)
				}
				return findTool(tools, "loader_panic"), fantasy.ToolCall{ID: "call-loader", Input: "{}"}, rawPanic
			},
			wantTool: "loader_panic",
		},
		{
			name: "direct MCP",
			buildAndFind: func(t *testing.T, policy Policy) (fantasy.AgentTool, fantasy.ToolCall, string) {
				catalog := []mcp.ServerTool{{ServerName: "svc", Tool: mcp.Tool{Name: "explode", Description: "explode"}}}
				tools, err := buildFantasyTools(nil, catalog, panickingBroker{value: rawPanic}, nil, policy, nil, nil,
					toolBuildConfig{panicAttribution: attribution})
				if err != nil {
					t.Fatal(err)
				}
				return findTool(tools, "mcp_svc_explode"), fantasy.ToolCall{ID: "call-direct-mcp", Input: "{}"}, rawPanic
			},
			wantTool: "mcp_svc_explode",
		},
		{
			name: "deferred MCP",
			buildAndFind: func(t *testing.T, policy Policy) (fantasy.AgentTool, fantasy.ToolCall, string) {
				SetToolDisclosureThreshold(1)
				t.Cleanup(func() { SetToolDisclosureThreshold(0) })
				catalog := []mcp.ServerTool{
					{ServerName: "svc", Tool: mcp.Tool{Name: "explode", Description: "explode"}},
					{ServerName: "svc", Tool: mcp.Tool{Name: "other", Description: "other"}},
				}
				tools, err := buildFantasyTools(nil, catalog, panickingBroker{value: rawPanic}, nil, policy, nil, nil,
					toolBuildConfig{panicAttribution: attribution})
				if err != nil {
					t.Fatal(err)
				}
				call := fantasy.ToolCall{
					ID:    "call-deferred-mcp",
					Input: `{"name":"mcp_svc_explode","arguments":{}}`,
				}
				return findTool(tools, toolNameToolCall), call, rawPanic
			},
			wantTool: "mcp_svc_explode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := capturePanicEvents(t)
			policy := &recordingPanicPolicy{}
			tool, call, secret := test.buildAndFind(t, policy)
			response, err := tool.Run(context.Background(), call)
			if err != nil {
				t.Fatalf("contained panic returned Go error: %v", err)
			}
			incident := assertOpaquePanicResponse(t, response, secret)
			events := collector.snapshot()
			if len(events) != 1 {
				t.Fatalf("panic events = %d, want exactly one: %+v", len(events), events)
			}
			event := events[0]
			if event.IncidentID != incident || event.ToolName != test.wantTool || event.ToolCallID != call.ID ||
				event.RunMode != attribution.runMode || event.TaskID != attribution.taskID ||
				event.ConversationID != attribution.conversationID {
				t.Fatalf("panic event attribution = %+v", event.PanicMetadata)
			}
			records := policy.recordsFor(test.wantTool)
			if len(records) != 1 {
				t.Fatalf("failed policy records for logical tool %q = %d, want exactly one: %+v", test.wantTool, len(records), records)
			}
			if records[0].succeeded || records[0].input == "" || !strings.Contains(records[0].result, incident) ||
				strings.Contains(records[0].result, secret) {
				t.Fatalf("logical failed-accounting record = %+v", records[0])
			}
		})
	}
}

func TestDeferredMCPRedactorPanicProducesSingleLogicalIncident(t *testing.T) {
	SetToolDisclosureThreshold(1)
	SetPIIRedactor(panickingPIIRedactor{})
	t.Cleanup(func() {
		SetToolDisclosureThreshold(0)
		SetPIIRedactor(nil)
	})
	collector := capturePanicEvents(t)
	policy := &recordingPanicPolicy{}
	catalog := []mcp.ServerTool{
		{ServerName: "svc", Tool: mcp.Tool{Name: "explode", Description: "explode"}},
		{ServerName: "svc", Tool: mcp.Tool{Name: "other", Description: "other"}},
	}
	tools, err := buildFantasyTools(nil, catalog, textBroker{text: "connector output"}, nil, policy, nil, nil,
		toolBuildConfig{panicAttribution: panicAttribution{runMode: "interactive", conversationID: "conv-deferred-redactor"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := findTool(tools, toolNameToolCall).Run(context.Background(), fantasy.ToolCall{
		ID:    "call-deferred-redactor",
		Input: `{"name":"mcp_svc_explode","arguments":{}}`,
	})
	if err != nil {
		t.Fatalf("contained deferred redactor panic returned Go error: %v", err)
	}
	incident := assertOpaquePanicResponse(t, response, "redactor raw panic")
	events := collector.snapshot()
	if len(events) != 1 || events[0].IncidentID != incident ||
		events[0].Boundary != panicPhaseOutputRedact || events[0].ToolName != "mcp_svc_explode" {
		t.Fatalf("deferred redactor events = %+v, want one logical-tool incident", events)
	}
	logicalRecords := policy.recordsFor("mcp_svc_explode")
	if len(logicalRecords) != 1 || logicalRecords[0].succeeded || !strings.Contains(logicalRecords[0].result, incident) {
		t.Fatalf("logical failed-accounting records = %+v", logicalRecords)
	}
}

func TestGovernedToolErrorsAreNormalizedBeforeFantasy(t *testing.T) {
	const secret = "Authorization: Bearer secret-native-error-token-123456789"
	policy := &recordingPanicPolicy{}
	native := fantasy.NewAgentTool("native_error", "native error",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, errors.New(secret)
		})
	tools, err := buildFantasyTools([]fantasy.AgentTool{native}, nil, nil, nil, policy, nil, nil,
		toolBuildConfig{panicAttribution: panicAttribution{runMode: "interactive"}})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := findTool(tools, "native_error").Run(context.Background(), fantasy.ToolCall{ID: "native-error", Input: "{}"})
	if runErr == nil || strings.Contains(runErr.Error(), secret) || !strings.Contains(runErr.Error(), "[REDACTED]") {
		t.Fatalf("normalized native error = %q", runErr)
	}
	records := policy.recordsFor("native_error")
	if len(records) != 1 || records[0].succeeded || strings.Contains(records[0].result, secret) ||
		!strings.Contains(records[0].result, "[REDACTED]") {
		t.Fatalf("native error policy records = %+v", records)
	}

	catalog := []mcp.ServerTool{{ServerName: "svc", Tool: mcp.Tool{Name: "failure", Description: "failure"}}}
	tools, err = buildFantasyTools(nil, catalog, errorBroker{err: errors.New(secret)}, nil, passPolicy{}, nil, nil,
		toolBuildConfig{panicAttribution: panicAttribution{runMode: "interactive"}})
	if err != nil {
		t.Fatal(err)
	}
	response, runErr := findTool(tools, "mcp_svc_failure").Run(context.Background(), fantasy.ToolCall{ID: "mcp-error", Input: "{}"})
	if runErr != nil || !response.IsError || strings.Contains(response.Content, secret) ||
		!strings.Contains(response.Content, "[REDACTED]") {
		t.Fatalf("governed MCP transport error: response=%+v err=%v", response, runErr)
	}
}

func TestMCPTransportErrorMethodPanicBecomesIncident(t *testing.T) {
	collector := capturePanicEvents(t)
	catalog := []mcp.ServerTool{{ServerName: "svc", Tool: mcp.Tool{Name: "panic_error", Description: "panic error"}}}
	tools, err := buildFantasyTools(nil, catalog, errorBroker{err: panickingError{}}, nil, passPolicy{}, nil, nil,
		toolBuildConfig{panicAttribution: panicAttribution{runMode: "scheduled", taskID: "task-mcp-error"}})
	if err != nil {
		t.Fatal(err)
	}
	response, runErr := findTool(tools, "mcp_svc_panic_error").Run(context.Background(), fantasy.ToolCall{
		ID: "mcp-error-panic", Input: "{}",
	})
	if runErr != nil {
		t.Fatalf("contained MCP Error panic returned Go error: %v", runErr)
	}
	assertOpaquePanicResponse(t, response, "error method raw panic")
	if strings.Contains(response.Content, "%!v(PANIC=") {
		t.Fatalf("fmt swallowed MCP Error panic into model text: %q", response.Content)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != panicPhaseOutputRedact ||
		events[0].ToolName != "mcp_svc_panic_error" || events[0].ToolCallID != "mcp-error-panic" ||
		events[0].TaskID != "task-mcp-error" {
		t.Fatalf("MCP Error panic events = %+v", events)
	}
}

func TestToolErrorMethodPanicBecomesPairedIncident(t *testing.T) {
	collector := capturePanicEvents(t)
	tool := fantasy.NewAgentTool("error_method_panic", "error method panic",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, panickingError{}
		})
	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:      CanonicalEnvPrefix,
		ConversationID: "conv-error-method",
		NativeTools:    []fantasy.AgentTool{tool},
	}, Deps{
		Input:  stubInput{system: "sys", user: "call it", label: "error-method"},
		Policy: passPolicy{},
		Model:  scriptedToolModel([]scriptedToolCall{{id: "call-error-method", name: "error_method_panic"}}),
	})
	if err != nil {
		t.Fatalf("Run failed after contained Error panic: %v", err)
	}
	results := assertTranscriptPairing(t, res.Entries, map[string]bool{"call-error-method": true})
	result := results["call-error-method"]
	if !result.IsErr || !incidentPattern.MatchString(result.Text) || strings.Contains(result.Text, "error method raw panic") {
		t.Fatalf("Error panic result is not safe/opaque: %+v", result)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != panicPhaseOutputRedact ||
		events[0].ToolName != "error_method_panic" || events[0].ToolCallID != "call-error-method" {
		t.Fatalf("Error panic events = %+v", events)
	}
}

func TestSafeToolResultTextPreservesPairWhenErrorMethodPanics(t *testing.T) {
	collector := capturePanicEvents(t)
	text, isErr := safeToolResultText(fantasy.ToolResultContent{
		ToolCallID: "flatten-call",
		ToolName:   "flatten-tool",
		Result:     fantasy.ToolResultOutputContentError{Error: panickingError{}},
	}, panicAttribution{runMode: "scheduled", taskID: "task-flatten"})
	if !isErr || !incidentPattern.MatchString(text) || strings.Contains(text, "error method raw panic") {
		t.Fatalf("flattened result = (%q, %v)", text, isErr)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != panicPhaseResultFlatten ||
		events[0].ToolName != "flatten-tool" || events[0].ToolCallID != "flatten-call" ||
		events[0].TaskID != "task-flatten" {
		t.Fatalf("flatten panic events = %+v", events)
	}
}

func TestStreamSinkSecretBackstopRedactsFantasyErrors(t *testing.T) {
	const secret = "Authorization: Bearer secret-fantasy-error-token-123456789"
	sink := newStreamSink(nil)
	sink.onToolResult("fantasy-error", "unknown_tool", secret, true)
	entries, _ := sink.snapshot()
	if len(entries) != 1 || strings.Contains(entries[0].Text, secret) || !strings.Contains(entries[0].Text, "[REDACTED]") {
		t.Fatalf("sink backstop entries = %+v", entries)
	}
}

type phasePanicPolicy struct {
	before      bool
	record      bool
	recordCalls atomic.Int32
}

func (p *phasePanicPolicy) BeforeToolCall(string, string, string) (bool, string) {
	if p.before {
		panic("before policy raw panic")
	}
	return false, ""
}

func (p *phasePanicPolicy) RecordToolResult(string, string, string, bool) {
	p.recordCalls.Add(1)
	if p.record {
		panic("record policy raw panic")
	}
}

func (*phasePanicPolicy) CanFinish(int) (bool, []string) { return true, nil }

type panickingPIIRedactor struct{}

func (panickingPIIRedactor) Redact(string) piiredact.Result { panic("redactor raw panic") }
func (panickingPIIRedactor) Mode() piiredact.Mode           { return piiredact.ModeRedact }

type panickingGuardrailDetector struct{}

func (panickingGuardrailDetector) Check(context.Context, string, string, string) (guardrail.Verdict, error) {
	panic("guardrail raw panic")
}

func TestUniversalPanicBoundary_AttributesPolicyAndOutputPhases(t *testing.T) {
	tests := []struct {
		name      string
		policy    Policy
		setup     func(t *testing.T)
		wantPhase string
		wantRuns  int32
	}{
		{
			name:      "BeforeToolCall",
			policy:    &phasePanicPolicy{before: true},
			wantPhase: panicPhasePolicyBefore,
		},
		{
			name:      "RecordToolResult",
			policy:    &phasePanicPolicy{record: true},
			wantPhase: panicPhasePolicyRecord,
			wantRuns:  1,
		},
		{
			name:   "redaction",
			policy: passPolicy{},
			setup: func(t *testing.T) {
				SetPIIRedactor(panickingPIIRedactor{})
				t.Cleanup(func() { SetPIIRedactor(nil) })
			},
			wantPhase: panicPhaseOutputRedact,
			wantRuns:  1,
		},
		{
			name:   "guardrail",
			policy: passPolicy{},
			setup: func(t *testing.T) {
				SetGuardrail(true, true, "block", "test", panickingGuardrailDetector{})
				t.Cleanup(func() { SetGuardrail(false, false, "", "", nil) })
			},
			wantPhase: panicPhaseOutputGuardrail,
			wantRuns:  1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.setup != nil {
				test.setup(t)
			}
			collector := capturePanicEvents(t)
			var runs atomic.Int32
			inner := fantasy.NewAgentTool("phase_tool", "phase tool",
				func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					runs.Add(1)
					return fantasy.NewTextResponse("output"), nil
				})
			tools, err := buildFantasyTools([]fantasy.AgentTool{inner}, nil, nil, nil, test.policy, nil, nil,
				toolBuildConfig{panicAttribution: panicAttribution{runMode: "interactive", conversationID: "conv-phase"}})
			if err != nil {
				t.Fatal(err)
			}
			response, err := findTool(tools, "phase_tool").Run(context.Background(), fantasy.ToolCall{ID: "phase-call", Input: "{}"})
			if err != nil {
				t.Fatalf("contained phase panic returned Go error: %v", err)
			}
			assertOpaquePanicResponse(t, response, "raw panic")
			if got := runs.Load(); got != test.wantRuns {
				t.Fatalf("inner tool ran %d times, want %d", got, test.wantRuns)
			}
			if p, ok := test.policy.(*phasePanicPolicy); ok && p.recordCalls.Load() != 1 {
				t.Fatalf("RecordToolResult invoked %d times after %s panic, want exactly one attempt", p.recordCalls.Load(), test.wantPhase)
			}
			events := collector.snapshot()
			if len(events) != 1 || events[0].Boundary != test.wantPhase || events[0].ToolName != "phase_tool" {
				t.Fatalf("phase events = %+v, want boundary %q", events, test.wantPhase)
			}
		})
	}
}

type panicOnEventObserver struct {
	event string
	calls atomic.Int32
}

func (o *panicOnEventObserver) Observe(event string, _ map[string]any) {
	o.calls.Add(1)
	if event == o.event {
		panic("observer raw panic")
	}
}

func TestObserverPanic_IsDisabledAndReturnedAsOrdinaryRunError(t *testing.T) {
	collector := capturePanicEvents(t)
	observer := &panicOnEventObserver{event: "tool.call"}
	var toolSettled atomic.Bool
	tool := fantasy.NewAgentTool("observer_tool", "observer tool",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			toolSettled.Store(true)
			return fantasy.NewTextResponse("settled"), nil
		})
	model := scriptedToolModel([]scriptedToolCall{{id: "observer-call", name: "observer_tool"}})

	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:      CanonicalEnvPrefix,
		ConversationID: "conv-observer",
		NativeTools:    []fantasy.AgentTool{tool},
	}, Deps{
		Input:    stubInput{system: "sys", user: "use tool", label: "observer"},
		Observer: observer,
		Policy:   passPolicy{},
		Model:    model,
	})
	if err == nil || !errors.Is(err, ErrRunBoundaryPanic) {
		t.Fatalf("Run error = %v, want ordinary ErrRunBoundaryPanic", err)
	}
	if strings.Contains(err.Error(), "observer raw panic") {
		t.Fatalf("run error leaked raw observer panic: %v", err)
	}
	if !toolSettled.Load() {
		t.Fatal("Run returned before Fantasy's tool goroutine settled")
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != "observer.tool.call" ||
		events[0].ToolName != "observer_tool" || events[0].ToolCallID != "observer-call" ||
		events[0].ConversationID != "conv-observer" {
		t.Fatalf("observer panic attribution = %+v", events)
	}
	if observer.calls.Load() != 1 {
		t.Fatalf("failed observer invoked %d times, want disabled after first panic", observer.calls.Load())
	}
}

func TestObserverPanic_AttributesPayloadToolField(t *testing.T) {
	collector := capturePanicEvents(t)
	observer := containObserver(
		&panicOnEventObserver{event: "persona_tool_blocked"},
		panicAttribution{runMode: ModeInteractive.String(), conversationID: "conv-tool-field"},
	)

	observer.Observe("persona_tool_blocked", map[string]any{
		"tool": "mcp_calendar_create_event",
		"id":   "blocked-call",
	})

	if err := observer.Err(); err == nil || !errors.Is(err, ErrRunBoundaryPanic) {
		t.Fatalf("observer error = %v, want ErrRunBoundaryPanic", err)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].ToolName != "mcp_calendar_create_event" ||
		events[0].ToolCallID != "blocked-call" || events[0].Boundary != "observer.persona_tool_blocked" {
		t.Fatalf("observer tool-field attribution = %+v", events)
	}
}

type scriptedToolCall struct {
	id   string
	name string
}

func scriptedToolModel(calls []scriptedToolCall) *mockModel {
	var streams atomic.Int32
	return &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		stream := streams.Add(1)
		return func(yield func(fantasy.StreamPart) bool) {
			if stream == 1 {
				for _, call := range calls {
					if !yield(fantasy.StreamPart{
						Type:          fantasy.StreamPartTypeToolCall,
						ID:            call.id,
						ToolCallName:  call.name,
						ToolCallInput: "{}",
					}) {
						return
					}
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "process still alive"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
}

func assertTranscriptPairing(t *testing.T, entries []RunEntry, expected map[string]bool) map[string]RunEntry {
	t.Helper()
	calls := make(map[string]int)
	results := make(map[string]RunEntry)
	for _, entry := range entries {
		switch entry.Type {
		case "tool_call":
			calls[entry.ToolCallID]++
		case "tool_result":
			if _, duplicate := results[entry.ToolCallID]; duplicate {
				t.Fatalf("duplicate tool result for %s: %+v", entry.ToolCallID, entries)
			}
			results[entry.ToolCallID] = entry
		}
	}
	for id := range expected {
		if calls[id] != 1 {
			t.Fatalf("tool call %s count = %d, want 1: %+v", id, calls[id], entries)
		}
		if _, ok := results[id]; !ok {
			t.Fatalf("tool call %s has no paired result: %+v", id, entries)
		}
	}
	return results
}

func TestFantasyStream_SequentialToolPanicProducesOnePairedResult(t *testing.T) {
	collector := capturePanicEvents(t)
	tool := newPanickingTool("sequential_panic", "sequential raw panic")
	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:      CanonicalEnvPrefix,
		ConversationID: "conv-sequential",
		NativeTools:    []fantasy.AgentTool{tool},
	}, Deps{
		Input:  stubInput{system: "sys", user: "call it", label: "sequential"},
		Policy: passPolicy{},
		Model:  scriptedToolModel([]scriptedToolCall{{id: "call-sequential", name: "sequential_panic"}}),
	})
	if err != nil {
		t.Fatalf("Run failed after contained tool panic: %v", err)
	}
	if res.FinalText != "process still alive" {
		t.Fatalf("final text = %q, process did not continue", res.FinalText)
	}
	results := assertTranscriptPairing(t, res.Entries, map[string]bool{"call-sequential": true})
	result := results["call-sequential"]
	if !result.IsErr || !incidentPattern.MatchString(result.Text) || strings.Contains(result.Text, "sequential raw panic") {
		t.Fatalf("persisted panic result is not safe/opaque: %+v", result)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].ToolCallID != "call-sequential" {
		t.Fatalf("panic events = %+v", events)
	}
}

func TestFantasyStream_RedactorPanicStillProducesOnePairedResult(t *testing.T) {
	collector := capturePanicEvents(t)
	SetPIIRedactor(panickingPIIRedactor{})
	t.Cleanup(func() { SetPIIRedactor(nil) })
	tool := fantasy.NewAgentTool("redactor_panic", "redactor panic",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("untrusted output"), nil
		})
	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:      CanonicalEnvPrefix,
		ConversationID: "conv-redactor-panic",
		NativeTools:    []fantasy.AgentTool{tool},
	}, Deps{
		Input:  stubInput{system: "sys", user: "call it", label: "redactor"},
		Policy: passPolicy{},
		Model:  scriptedToolModel([]scriptedToolCall{{id: "call-redactor", name: "redactor_panic"}}),
	})
	if err != nil {
		t.Fatalf("Run failed after contained redactor panic: %v", err)
	}
	results := assertTranscriptPairing(t, res.Entries, map[string]bool{"call-redactor": true})
	result := results["call-redactor"]
	if !result.IsErr || !incidentPattern.MatchString(result.Text) || strings.Contains(result.Text, "redactor raw panic") {
		t.Fatalf("redactor panic result is not safe/opaque: %+v", result)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != panicPhaseOutputRedact || events[0].ToolCallID != "call-redactor" {
		t.Fatalf("redactor panic events = %+v", events)
	}
}

func TestFantasyStream_ParallelPanicSettlesWithSuccessfulSibling(t *testing.T) {
	collector := capturePanicEvents(t)
	gate := make(chan struct{})
	var started atomic.Int32
	arrive := func() {
		if started.Add(1) == 2 {
			close(gate)
		}
		<-gate
	}
	panicTool := fantasy.NewParallelAgentTool("parallel_panic", "parallel panic",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			arrive()
			panic("parallel raw panic")
		})
	var siblingRuns atomic.Int32
	sibling := fantasy.NewParallelAgentTool("parallel_success", "parallel success",
		func(context.Context, panicTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			arrive()
			siblingRuns.Add(1)
			return fantasy.NewTextResponse("sibling succeeded"), nil
		})

	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		TaskID:      "task-parallel",
		NativeTools: []fantasy.AgentTool{panicTool, sibling},
	}, Deps{
		Input:  stubInput{system: "sys", user: "parallel", label: "parallel"},
		Policy: passPolicy{},
		Model: scriptedToolModel([]scriptedToolCall{
			{id: "call-panic", name: "parallel_panic"},
			{id: "call-success", name: "parallel_success"},
		}),
	})
	if err != nil {
		t.Fatalf("parallel Run failed: %v", err)
	}
	if started.Load() != 2 || siblingRuns.Load() != 1 {
		t.Fatalf("parallel tools did not settle deterministically: started=%d sibling=%d", started.Load(), siblingRuns.Load())
	}
	results := assertTranscriptPairing(t, res.Entries, map[string]bool{"call-panic": true, "call-success": true})
	if !results["call-panic"].IsErr || strings.Contains(results["call-panic"].Text, "parallel raw panic") {
		t.Fatalf("panic result = %+v", results["call-panic"])
	}
	if results["call-success"].IsErr || results["call-success"].Text != "sibling succeeded" {
		t.Fatalf("successful sibling result = %+v", results["call-success"])
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].TaskID != "task-parallel" || events[0].ToolCallID != "call-panic" {
		t.Fatalf("parallel panic events = %+v", events)
	}
}

func TestFantasyStream_PanickedCallSuppressesInRoundProviderRecovery(t *testing.T) {
	t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "0")
	var streams atomic.Int32
	model := &mockModel{streamFunc: func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		switch streams.Add(1) {
		case 1:
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "committed-call", ToolCallName: "committed_panic", ToolCallInput: "{}"})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			}, nil
		default:
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New(midStream504)})
			}, nil
		}
	}}

	_, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{newPanickingTool("committed_panic", "committed raw panic")},
	}, Deps{
		Input:  stubInput{system: "sys", user: "call", label: "commit"},
		Policy: passPolicy{},
		Model:  model,
	})
	if err == nil || !errors.Is(err, ErrCommittedSideEffects) {
		t.Fatalf("Run error = %v, want ErrCommittedSideEffects", err)
	}
	if streams.Load() != 2 {
		t.Fatalf("model stream count = %d, want 2 (tool round + one failed continuation, no re-drive)", streams.Load())
	}
}

type canFinishPanicPolicy struct{ passPolicy }

func (canFinishPanicPolicy) CanFinish(int) (bool, []string) { panic("finish raw panic") }

func TestPolicyCanFinishPanic_IsContainedAsRunError(t *testing.T) {
	collector := capturePanicEvents(t)
	_, err := Run(context.Background(), ModeScheduled, RunConfig{
		EnvPrefix: CanonicalEnvPrefix,
		TaskID:    "task-finish",
	}, Deps{
		Input:  stubInput{system: "sys", user: "finish", label: "finish"},
		Policy: canFinishPanicPolicy{},
		Model:  &mockModel{streamFunc: streamStop()},
	})
	if err == nil || !errors.Is(err, ErrRunBoundaryPanic) || strings.Contains(err.Error(), "finish raw panic") {
		t.Fatalf("CanFinish panic error = %v", err)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Boundary != panicPhasePolicyFinish || events[0].TaskID != "task-finish" {
		t.Fatalf("CanFinish panic events = %+v", events)
	}
}

type synchronousPanicTool struct{ phase string }

func (t synchronousPanicTool) Info() fantasy.ToolInfo {
	if t.phase == "info" {
		panic("info raw panic")
	}
	return fantasy.ToolInfo{Name: "sync_panic_tool", Description: "synchronous panic test"}
}

func (t synchronousPanicTool) ProviderOptions() fantasy.ProviderOptions {
	if t.phase == "provider_options" {
		panic("provider options raw panic")
	}
	return nil
}

func (synchronousPanicTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (synchronousPanicTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("ok"), nil
}

func TestRunBackstop_ContainsSynchronousToolMethodsAndFinalize(t *testing.T) {
	tests := []struct {
		name     string
		tool     fantasy.AgentTool
		finalize FinalizeHook
		secret   string
	}{
		{name: "Info", tool: synchronousPanicTool{phase: "info"}, secret: "info raw panic"},
		{name: "ProviderOptions", tool: synchronousPanicTool{phase: "provider_options"}, secret: "provider options raw panic"},
		{
			name: "Finalize",
			finalize: func(context.Context, FinalizeInput) (string, error) {
				panic("finalize raw panic")
			},
			secret: "finalize raw panic",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := capturePanicEvents(t)
			cfg := RunConfig{EnvPrefix: CanonicalEnvPrefix, ConversationID: "conv-run-backstop"}
			if test.tool != nil {
				cfg.NativeTools = []fantasy.AgentTool{test.tool}
			}
			_, err := Run(context.Background(), ModeInteractive, cfg, Deps{
				Input:    stubInput{system: "sys", user: "work", label: "backstop"},
				Policy:   passPolicy{},
				Model:    &mockModel{streamFunc: streamStop()},
				Finalize: test.finalize,
			})
			if err == nil || !errors.Is(err, ErrRunBoundaryPanic) || strings.Contains(err.Error(), test.secret) {
				t.Fatalf("Run error = %v, want opaque ErrRunBoundaryPanic", err)
			}
			events := collector.snapshot()
			if len(events) != 1 || events[0].Location != panicLocationRun ||
				events[0].Boundary != panicPhaseRunSynchronous ||
				events[0].ConversationID != "conv-run-backstop" {
				t.Fatalf("run-backstop events = %+v", events)
			}
		})
	}
}
