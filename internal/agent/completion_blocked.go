package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// CompletionBlockedWhen is the task's completion.blocked_when rule (EXECUTION
// REQUIREMENTS, docs/CONDITIONAL-TASK-COMPLETION.md) as the driver resolved it
// against the run's roster. A run that completes through the completion
// predicate, in which no completion tool OTHER than Tools succeeded, and
// whose last successful execution of one of Tools passed the top-level string
// argument Argument with a value in In, finishes successfully with run
// outcome "blocked": it correctly decided not to publish, and the Operations
// Center shows that instead of plain green. Fleet assigns no meaning to the
// names or the values beyond that match.
type CompletionBlockedWhen struct {
	// Tools are the full roster names (mcp_<server>_<tool> or native) the
	// declared tool resolved to.
	Tools []string
	// Argument is the top-level argument whose string value is matched.
	Argument string
	// In is the set of values that mark the run blocked (exact match).
	In []string
	// DetailArgument optionally names a top-level string argument of the same
	// call whose text explains the outcome; "" for none.
	DetailArgument string
}

// completionBlockedRule is CompletionBlockedWhen as sets.
type completionBlockedRule struct {
	tools          map[string]bool
	argument       string
	in             map[string]bool
	detailArgument string
}

func newCompletionBlockedRule(opt *CompletionBlockedWhen) *completionBlockedRule {
	if opt == nil || len(opt.Tools) == 0 || opt.Argument == "" || len(opt.In) == 0 {
		return nil
	}
	rule := &completionBlockedRule{
		tools:          make(map[string]bool, len(opt.Tools)),
		argument:       opt.Argument,
		in:             make(map[string]bool, len(opt.In)),
		detailArgument: opt.DetailArgument,
	}
	for _, name := range opt.Tools {
		rule.tools[name] = true
	}
	for _, v := range opt.In {
		rule.in[v] = true
	}
	return rule
}

// maxBlockedDetailRunes bounds the explanation quoted from the call.
const maxBlockedDetailRunes = 300

// messageTypeCompletionBlocked marks the [completion_blocked] breadcrumb a run
// leaves when its declared blocked rule matched (descriptive session-log
// metadata, like completion_predicate).
const messageTypeCompletionBlocked = "completion_blocked"

// evaluateBlockedOutcome applies the task's blocked rule to a run the
// completion predicate just completed, recording the breadcrumb and the
// detail persistBlockedOutcome hands to the runner once the run has
// succeeded. A run that published — another completion tool succeeded — or
// whose last recording call carried a non-blocking value is an ordinary
// success.
func (p *scheduledPolicy) evaluateBlockedOutcome() {
	p.blockedDetail = ""
	if p.agent == nil || p.agent.completionBlocked == nil {
		return
	}
	detail, blocked := p.agent.completionBlocked.match(p.agent.logSession, p.agent.completionAnySucceeded)
	if !blocked {
		return
	}
	p.blockedDetail = detail
	log.Printf("Completion blocked rule matched (%s); the run finishes as blocked", detail)
	t := messageTypeCompletionBlocked
	p.agent.logSession.AddMessageWithMetadata(roleUser, fmt.Sprintf(
		"[completion_blocked] %s — the task's EXECUTION REQUIREMENTS completion clause declares this a blocked outcome, so the run finishes as Blocked rather than plain success",
		detail), nil, nil, &t, nil, nil, "")
}

// persistBlockedOutcome records the blocked run outcome on the session for
// the runner (models.LogSession.RunOutcome). Called only once the run has
// returned success, like persistVerifierWarning.
func (p *scheduledPolicy) persistBlockedOutcome() {
	if p == nil || p.blockedDetail == "" || p.agent == nil {
		return
	}
	p.agent.logSession.SetRunOutcome(runOutcomeBlocked, p.blockedDetail)
}

// runOutcomeBlocked mirrors models.RunOutcomeBlocked (this package does not
// import the scheduler models).
const runOutcomeBlocked = "blocked"

// match reports whether the run's tool records satisfy the rule, and the
// detail: "<argument>=<value>", plus ": <text>" from DetailArgument.
func (r *completionBlockedRule) match(session *LogSession, completion map[string]bool) (string, bool) {
	for _, rec := range buildToolExecSummary(session) {
		if rec.Succeeded && completion[rec.Name] && !r.tools[rec.Name] {
			return "", false
		}
	}
	args, ok := lastSuccessfulCallArguments(session, r.tools)
	if !ok {
		return "", false
	}
	value, _ := args[r.argument].(string)
	if !r.in[value] {
		return "", false
	}
	detail := r.argument + "=" + value
	if r.detailArgument != "" {
		if text, _ := args[r.detailArgument].(string); strings.TrimSpace(text) != "" {
			detail += ": " + truncateRunes(collapseWhitespace(text), maxBlockedDetailRunes)
		}
	}
	return agentcore.RedactSecrets(detail), true
}

// lastSuccessfulCallArguments returns the top-level arguments of the last
// successful execution of any tool in names, read from the session's raw
// tool calls (a tool_call bridge call is unwrapped to the connector tool it
// invoked) with the same success classification buildToolExecSummary uses.
func lastSuccessfulCallArguments(session *LogSession, names map[string]bool) (map[string]any, bool) {
	if session == nil {
		return nil, false
	}
	type call struct{ name, args string }
	pending := make(map[string]call)
	var last map[string]any
	found := false
	for _, msg := range session.SnapshotMessages() {
		for _, tc := range msg.ToolCalls {
			name, args, _ := unwrapToolCallBridge(tc.Name, tc.Arguments)
			pending[tc.ID] = call{name: name, args: args}
		}
		if msg.Role != roleTool || msg.ToolCallID == nil {
			continue
		}
		c, ok := pending[*msg.ToolCallID]
		if !ok {
			continue
		}
		delete(pending, *msg.ToolCallID)
		if !names[c.name] || msg.IsError || toolResultLooksFailed(msg.Content) {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(c.args), &args); err != nil {
			args = nil
		}
		last, found = args, true
	}
	return last, found
}
