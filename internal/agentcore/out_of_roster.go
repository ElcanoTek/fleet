package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/jsonrepair"

	"github.com/ElcanoTek/fleet/internal/metrics"
)

// Out-of-roster tool calls (#1744). A model sometimes calls a tool name that
// is not in this run's tool list. Fantasy answers every such call with the
// same "tool not found: X. Available tools: …" result, which is right for a
// made-up name but misleads for the two common cases, names that DO exist this
// run but are not directly callable:
//
//   - a deferred MCP tool (#506), reachable only through tool_call. The model
//     took the refusal to mean the connector was down (#1006). Such a call is
//     now rewritten into tool_call {name, arguments} and runs through the
//     bridge, under the same persona, allowlist, approval and audit gates as a
//     tool_call the model wrote itself.
//   - a tool a gate removed: a narrowed roster (#1603), the persona policy
//     (#294), an optional connector the run did not turn on, a connector's
//     allowlist, or an invalid schema. Such a call is never run; its result
//     names the gate (and, in a narrowed run, the registered stand-in).
//
// Every out-of-roster call is counted per model and reason
// (fleet_tool_call_out_of_roster_total) and reported to the Observer, so
// roster decisions can be made from data.
//
// It can never widen access. Only a name in this run's deferred registry is
// rewritten, and only into tool_call, which re-checks the name against that
// registry and dispatches through the governed tool; every other name keeps
// fantasy's refusal, with at most a better explanation.

// Out-of-roster reasons, the metric's bounded reason label.
const (
	outOfRosterDeferred = "deferred"
	outOfRosterUnknown  = "unknown"
)

// eventToolCallOutOfRoster is the Observer event for one out-of-roster call.
const eventToolCallOutOfRoster = "fleet.tool_call_out_of_roster"

// outOfRoster is a run's out-of-roster state: the current roster (replaced on
// the mid-run MCP rebuild) and the explanation for each refused call, which
// the stream sink records and explain/explainStep send the model in place of
// fantasy's "tool not found". nil-safe.
type outOfRoster struct {
	persona string
	orch    *orchestrationState // for a narrowed run's stand-in; may be nil
	gates   mcpGates

	mu     sync.Mutex
	roster toolRoster
	notes  map[noteKey]string
}

// noteKey is a refused call: its ID and its tool name. A provider may reuse a
// call ID across steps, so the ID alone would put one call's explanation on
// another tool's result: a write that ran and failed would read "was not
// called", and invite a retry.
type noteKey struct{ id, tool string }

func newOutOfRoster(persona string, orch *orchestrationState, gates mcpGates) *outOfRoster {
	return &outOfRoster{persona: persona, orch: orch, gates: gates, notes: map[noteKey]string{}}
}

// mcpGates is the run's Gate-1 (opt-in) and Gate-2 (allowlist) configuration.
// The roster records what these gates dropped from the catalog it was given,
// but out of process the MCP broker applies a server's allowlist before the
// catalog arrives (ADR-0042), so a tool the allowlist or a narrowed roster
// dropped is never seen, or recorded. gateFor applies the same rules to the
// called name instead.
type mcpGates struct {
	allow     mcpAllowlist
	exclusive bool
	optional  mcpOptionalSet
	optIn     map[string]bool
}

// gateFor is the gate that keeps the mcp_<server>_<tool> name out of the run,
// by the build loop's own Gate-1 and Gate-2 rules, or "" when neither does
// (an unknown server, or a tool the allowlist admits but the catalog lacks).
func (g mcpGates) gateFor(name string, catalogServers map[string]bool) string {
	server, tool := g.splitMCPName(name, catalogServers)
	if server == "" {
		return ""
	}
	if key := optionalServerFor(server, g.optional); key != "" && !g.optIn[key] {
		return gateOptIn
	}
	if list := g.allow.gateEntry(server, g.exclusive); (len(list) > 0 || g.exclusive) && !slices.Contains(list, tool) {
		if g.exclusive {
			return gateNarrowed
		}
		return gateAllowlist
	}
	return ""
}

// splitMCPName splits mcp_<server>_<tool> at the longest server name the run
// knows: a catalog registration name, an allowlist key or an optional server.
func (g mcpGates) splitMCPName(name string, catalogServers map[string]bool) (string, string) {
	best := ""
	consider := func(server string) {
		prefix := "mcp_" + server + "_"
		if len(server) > len(best) && len(name) > len(prefix) && strings.HasPrefix(name, prefix) {
			best = server
		}
	}
	for s := range catalogServers {
		consider(s)
	}
	for s := range g.allow {
		consider(s)
	}
	for s := range g.optional {
		consider(s)
	}
	if best == "" {
		return "", ""
	}
	return best, strings.TrimPrefix(name, "mcp_"+best+"_")
}

// setRoster installs the roster the run just built.
func (o *outOfRoster) setRoster(r toolRoster) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.roster = r
	o.mu.Unlock()
}

// note is the explanation recorded for the refused call callID to tool, if any.
func (o *outOfRoster) note(callID, tool string) (string, bool) {
	if o == nil || callID == "" {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	n, ok := o.notes[noteKey{callID, tool}]
	return n, ok
}

// snapshotNotes copies the recorded explanations; nil when there are none.
func (o *outOfRoster) snapshotNotes() map[noteKey]string {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.notes) == 0 {
		return nil
	}
	notes := make(map[noteKey]string, len(o.notes))
	for k, n := range o.notes {
		notes[k] = n
	}
	return notes
}

// repair is the run's fantasy RepairToolCallFunction for a stream on
// modelSlug. A call to a registered tool that failed validation gets
// fantasy's default JSON repair, which installing a repair function turns
// off. A call to an unregistered name is classified, counted and reported:
// a deferred tool is rewritten into tool_call; any other name is left to
// fantasy's refusal, with the gate's explanation recorded for its result.
func (o *outOfRoster) repair(modelSlug string, sink *streamSink) fantasy.RepairToolCallFunction {
	if o == nil {
		return nil
	}
	return func(_ context.Context, opts fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
		call := opts.OriginalToolCall
		if hasTool(opts.AvailableTools, call.ToolName) {
			return repairJSON(call), nil
		}
		reason, repaired, note := o.classify(call, opts.AvailableTools)
		if note != "" && call.ToolCallID != "" {
			o.mu.Lock()
			o.notes[noteKey{call.ToolCallID, call.ToolName}] = note
			o.mu.Unlock()
		}
		model := modelSlug
		if model == "" {
			model = "unknown"
		}
		metrics.RecordToolCallOutOfRoster(model, reason)
		log.Printf("out-of-roster tool call: model=%s tool=%q reason=%s rerouted=%t", model, call.ToolName, reason, repaired != nil)
		payload := map[string]any{"id": call.ToolCallID, "tool": call.ToolName, "reason": reason, "model": model}
		if repaired != nil {
			payload["rerouted_to"] = repaired.ToolName
		}
		if sink != nil {
			sink.emit(eventToolCallOutOfRoster, payload)
		}
		return repaired, nil
	}
}

// classify decides what an unregistered call is: the reason label, the
// rewritten call (deferred only) and the explanation for its result ("" keeps
// fantasy's own text).
func (o *outOfRoster) classify(call fantasy.ToolCallContent, available []fantasy.AgentTool) (string, *fantasy.ToolCallContent, string) {
	o.mu.Lock()
	deferred := o.roster.deferredMCP > 0 && slices.Contains(o.roster.mcp, call.ToolName)
	gate := o.roster.removed[call.ToolName]
	if gate == "" && !deferred && strings.HasPrefix(call.ToolName, "mcp_") {
		gate = o.gates.gateFor(call.ToolName, o.roster.servers)
	}
	o.mu.Unlock()

	if deferred {
		if !hasTool(available, toolNameToolCall) {
			// The persona denied the bridge itself (an explicit deny: the
			// bridges are exempt from a persona's allow list).
			return outOfRosterDeferred, nil, fmt.Sprintf(
				"%s is not available in this run and was not called: it is reachable only through tool_call, "+
					"which is not in your tool list.", call.ToolName)
		}
		if input, ok := deferredCallInput(call.ToolName, call.Input); ok {
			repaired := call
			repaired.ToolName = toolNameToolCall
			repaired.Input = input
			return outOfRosterDeferred, &repaired, ""
		}
		return outOfRosterDeferred, nil, fmt.Sprintf(
			"%s was not called: it is reachable only through tool_call, and its arguments were not a JSON object. "+
				"Call tool_call with {\"name\": %q, \"arguments\": {...}}.", call.ToolName, call.ToolName)
	}
	if gate == "" {
		return outOfRosterUnknown, nil, ""
	}
	return gate, nil, o.gateNote(call.ToolName, gate)
}

// gateNote explains, in the refused call's result, which gate removed tool.
func (o *outOfRoster) gateNote(tool, gate string) string {
	var why string
	switch gate {
	case gateNarrowed:
		why = "this run's tools are narrowed to the ones it requires, and " + tool + " is not one of them"
		if standIn := o.orch.narrowedStandInFor(tool); standIn != "" {
			why += "; the registered stand-in is " + standIn + ", call it instead"
		}
	case gatePersona:
		persona := "this persona"
		if o.persona != "" {
			persona = fmt.Sprintf("the %q persona", o.persona)
		}
		why = persona + "'s tool policy does not allow it"
	case gateOptIn:
		why = "it belongs to an optional connector this run did not turn on"
	case gateAllowlist:
		why = "its connector's tool allowlist does not include it"
	case gateSchema:
		why = "its input schema is not valid JSON Schema, so it is not offered to the model"
	default:
		return ""
	}
	return fmt.Sprintf("%s is not available in this run and was not called: %s.", tool, why)
}

// hasTool reports whether name is in the step's tool list.
func hasTool(tools []fantasy.AgentTool, name string) bool {
	for _, t := range tools {
		if t.Info().Name == name {
			return true
		}
	}
	return false
}

// repairJSON is fantasy's default repair for a registered tool's call, which
// a custom repair function replaces: jsonrepair over malformed input. nil when
// it changes nothing.
func repairJSON(call fantasy.ToolCallContent) *fantasy.ToolCallContent {
	repaired, err := jsonrepair.RepairJSON(call.Input)
	if err != nil || repaired == call.Input {
		return nil
	}
	call.Input = repaired
	return &call
}

// deferredCallInput is tool_call's input for a direct call to the deferred
// tool name: {"name": name, "arguments": <the call's input>}. The input's own
// bytes are kept, so the policy, approval and transcript see exactly what a
// tool_call the model wrote would carry. Empty input is no arguments;
// malformed JSON gets jsonrepair. ok is false when tool_call would refuse the
// arguments anyway (normalizeDeferredArguments: an object, null, or a
// one-object array).
func deferredCallInput(name, input string) (string, bool) {
	args := strings.TrimSpace(input)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		repaired, err := jsonrepair.RepairJSON(args)
		if err != nil || !json.Valid([]byte(repaired)) {
			return "", false
		}
		args = strings.TrimSpace(repaired)
	}
	if _, err := normalizeDeferredArguments(json.RawMessage(args)); err != nil {
		return "", false
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		return "", false
	}
	return `{"name":` + string(quoted) + `,"arguments":` + args + `}`, true
}

// explainStep is a PrepareStep that sends the model, for each refused call
// that has one, the explanation the transcript records instead of fantasy's
// "tool not found". It rewrites a copy of the step's messages, never the run's
// history, and gives the same text on every step, so the prompt-cache prefix
// is unchanged: the model first sees a refused call's result on the step
// after the call, already explained.
func (o *outOfRoster) explainStep(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	if msgs, changed := explainResults(opts.Messages, o.snapshotNotes()); changed {
		return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
	}
	return ctx, fantasy.PrepareStepResult{}, nil
}

// explain is msgs with the refused calls' explanations in place of fantasy's
// text: for a round's transcript as the run carries it on (engine.roundCarry)
// to the next round, the finalize hook, the terminal structured-output phase
// and compaction, so every later model call sees what the model was sent.
func (o *outOfRoster) explain(msgs []fantasy.Message) []fantasy.Message {
	if out, changed := explainResults(msgs, o.snapshotNotes()); changed {
		return out
	}
	return msgs
}

// explainResults replaces the error output of each tool result that has a
// note, by its call ID and the tool the preceding assistant message called,
// copying only what it changes. changed is false when nothing changed.
func explainResults(msgs []fantasy.Message, notes map[noteKey]string) ([]fantasy.Message, bool) {
	if len(notes) == 0 {
		return nil, false
	}
	var out []fantasy.Message
	called := map[string]string{} // call ID → tool, from the latest assistant message
	for i, m := range msgs {
		if m.Role == fantasy.MessageRoleAssistant {
			clear(called)
			for _, p := range m.Content {
				if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok {
					called[tc.ToolCallID] = tc.ToolName
				}
			}
			continue
		}
		if m.Role != fantasy.MessageRoleTool {
			continue
		}
		var parts []fantasy.MessagePart
		for j, p := range m.Content {
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
			if !ok {
				continue
			}
			n, ok := notes[noteKey{tr.ToolCallID, called[tr.ToolCallID]}]
			if !ok {
				continue
			}
			e, isErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output)
			if !isErr || (e.Error != nil && e.Error.Error() == n) {
				continue
			}
			if parts == nil {
				parts = slices.Clone(m.Content)
			}
			tr.Output = fantasy.ToolResultOutputContentError{Error: errors.New(n)}
			parts[j] = tr
		}
		if parts != nil {
			if out == nil {
				out = slices.Clone(msgs)
			}
			out[i].Content = parts
		}
	}
	return out, out != nil
}
