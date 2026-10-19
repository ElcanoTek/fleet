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

// noteKey is a refused call: its ID, its tool name, and the text fantasy
// answered it with (its ValidationError). A provider may reuse a call ID,
// within a run and across turns, and the history the model is sent includes
// earlier turns replayed from the session log. So the ID and tool alone would
// put one call's explanation on another call's result: a write that ran and
// failed would read "was not called", and invite a retry. Only fantasy's
// refusal of the call carries that exact text.
type noteKey struct{ id, tool, refused string }

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

// gateFor is the gate the build loop's own Gate-1 and Gate-2 rules would
// apply to the mcp_<server>_<tool> name, and the connector it names (the
// optional key for gateOptIn), or "" when neither applies. It goes by the name
// alone, so it cannot tell a dropped tool from a made-up one on the same
// connector: callers say only what is true of both.
func (g mcpGates) gateFor(name string, catalogServers map[string]bool) (string, string) {
	server, tool := g.splitMCPName(name, catalogServers)
	if server == "" {
		return "", ""
	}
	if key := optionalServerFor(server, g.optional); key != "" && !g.optIn[key] {
		return gateOptIn, key
	}
	if list := g.allow.gateEntry(server, g.exclusive); (len(list) > 0 || g.exclusive) && !slices.Contains(list, tool) {
		if g.exclusive {
			return gateNarrowed, server
		}
		return gateAllowlist, server
	}
	return "", ""
}

// splitMCPName splits mcp_<server>_<tool> at the longest server name the run
// knows: a catalog registration name or an optional server, and in a
// narrowed run an allowlist key too (a server none of whose tools the run
// requires has only its deny-all entry). Outside a narrowed run an allowlist
// key alone is no evidence: its server may simply not be in this run.
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
	if g.exclusive {
		for s := range g.allow {
			consider(s)
		}
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

// note is the explanation recorded for the refused call callID to tool whose
// result fantasy gave as refused, if any.
func (o *outOfRoster) note(callID, tool, refused string) (string, bool) {
	if o == nil || callID == "" {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	n, ok := o.notes[noteKey{callID, tool, refused}]
	return n, ok
}

// refusalText is the text of fantasy's result for a call it refused with err.
func refusalText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
		got := o.classify(call, opts.AvailableTools, opts.ValidationError)
		reason, repaired := got.reason, got.repaired
		if got.note != "" && call.ToolCallID != "" {
			o.mu.Lock()
			o.notes[noteKey{call.ToolCallID, call.ToolName, refusalText(opts.ValidationError)}] = got.note
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
		if got.inferred != "" {
			payload["inferred_gate"] = got.inferred
		}
		if sink != nil {
			sink.emit(eventToolCallOutOfRoster, payload)
		}
		return repaired, nil
	}
}

// outOfRosterOutcome is what classify decided about an unregistered call.
type outOfRosterOutcome struct {
	reason   string                   // the metric's reason label
	repaired *fantasy.ToolCallContent // the rewritten call (a deferred tool)
	note     string                   // the result's text; "" keeps fantasy's
	inferred string                   // a gate gateFor inferred from the name
}

// classify decides what an unregistered call is. A gate the roster recorded
// removed a tool that exists, so the result says so outright and the metric
// counts it under that gate. A gate inferred from the name alone (gateFor)
// fits a made-up name on that connector just as well, so the result keeps
// fantasy's text, with its list of tools to correct a typo from, and adds
// only what is true either way; the metric counts it as unknown.
func (o *outOfRoster) classify(call fantasy.ToolCallContent, available []fantasy.AgentTool, validation error) outOfRosterOutcome {
	o.mu.Lock()
	deferred := o.roster.deferredMCP > 0 && slices.Contains(o.roster.mcp, call.ToolName)
	gate := o.roster.removed[call.ToolName]
	var inferred, server string
	if gate == "" && !deferred && strings.HasPrefix(call.ToolName, "mcp_") {
		inferred, server = o.gates.gateFor(call.ToolName, o.roster.servers)
	}
	o.mu.Unlock()

	switch {
	case deferred && !hasTool(available, toolNameToolCall):
		// The persona denied the bridge itself (an explicit deny: the
		// bridges are exempt from a persona's allow list).
		return outOfRosterOutcome{reason: outOfRosterDeferred, note: fmt.Sprintf(
			"%s is not available in this run and was not called: it is reachable only through tool_call, "+
				"which is not in your tool list.", call.ToolName)}
	case deferred:
		if input, ok := deferredCallInput(call.ToolName, call.Input); ok {
			repaired := call
			repaired.ToolName = toolNameToolCall
			repaired.Input = input
			return outOfRosterOutcome{reason: outOfRosterDeferred, repaired: &repaired}
		}
		// The note does not describe the arguments: the replayed call shows
		// them normalized (toolCallInputSanitizeStep), often as {}.
		return outOfRosterOutcome{reason: outOfRosterDeferred, note: fmt.Sprintf(
			"%s was not called: it is reachable only through tool_call. "+
				"Call tool_call with {\"name\": %q, \"arguments\": {...}}, with the arguments as a JSON object.", call.ToolName, call.ToolName)}
	case gate != "":
		return outOfRosterOutcome{reason: gate, note: o.gateNote(call.ToolName, gate)}
	case inferred != "":
		notFound := "tool not found: " + call.ToolName + "."
		if validation != nil {
			notFound = validation.Error()
		}
		return outOfRosterOutcome{reason: outOfRosterUnknown, inferred: inferred,
			note: strings.TrimSuffix(notFound, ".") + ". " + o.inferredNote(call.ToolName, inferred, server)}
	}
	return outOfRosterOutcome{reason: outOfRosterUnknown}
}

// inferredNote is the sentence a gate inferred from tool's name adds to
// fantasy's result: true whether or not a tool by that name exists.
func (o *outOfRoster) inferredNote(tool, gate, server string) string {
	switch gate {
	case gateNarrowed:
		note := "This run's tools are narrowed to the ones it requires, and that name is not one of them."
		if standIn := o.orch.narrowedStandInFor(tool); standIn != "" {
			note += " The registered stand-in for it is " + standIn + "; call that instead."
		}
		return note
	case gateOptIn:
		return "That name is on the optional " + server + " connector, which this run did not turn on."
	default:
		return "The " + server + " connector's tool allowlist does not include that name."
	}
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
// one-object array), or when name is not a plain tool name (plainToolName).
func deferredCallInput(name, input string) (string, bool) {
	if !plainToolName(name) {
		return "", false
	}
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
	return `{"name":"` + name + `","arguments":` + args + `}`, true
}

// plainToolName reports whether name uses only the MCP spec's tool-name
// characters (ASCII letters, digits, '_', '-' and '.'). JSON escapes none of
// them, so deferredCallInput writes such a name between quotes as it is. A
// deferred tool with any other character in its name is not rerouted; its
// result says to call tool_call.
func plainToolName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
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
// note, by its call ID, the tool the preceding assistant message called, and
// its text, which must still be fantasy's refusal, copying only what it
// changes. changed is false when nothing changed.
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
			e, isErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output)
			if !isErr {
				continue
			}
			// An explained result no longer has fantasy's text, so it is
			// never rewritten twice.
			n, ok := notes[noteKey{tr.ToolCallID, called[tr.ToolCallID], refusalText(e.Error)}]
			if !ok {
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
