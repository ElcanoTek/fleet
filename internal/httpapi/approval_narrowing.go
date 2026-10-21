// Approval narrowing: approve some of the records one staged call covers.
//
// A bundle can declare the ONE top-level list argument of a critical tool a
// person may narrow on its card (agent_policy.critical_tool_narrowable_args,
// e.g. execute_plan: {arg: deal_ids}). The describer's card rows carry a
// `key` naming a staged value; the person unticks rows and approves the rest.
// The approve POST then names the ticked values (`approved_values`), and
// fleet approves the call with that argument replaced by them and every other
// argument exactly as staged.
//
// The rules, all enforced here and not trusted to the client:
//   - the tool must declare the argument, and its staged value must be a list
//     of 2 to maxNarrowableValues distinct non-empty strings;
//   - the approved values must be a non-empty subset of the staged values,
//     each named once. Fleet keeps them in staged order, so a narrowing can
//     only leave values out: never add, repeat or reorder one;
//   - narrowing goes with an approval decided once (scope once), never with a
//     decline or a session policy;
//   - the narrowing is written in the claim itself (ClaimApprovalNarrowed), so
//     an approved row and what was approved can never disagree, and execution
//     re-checks that record against the staged call and refuses to run on a
//     mismatch rather than run the whole staged list.
//
// The server is told which values were staged (`_meta["fleet/narrowed"]`), so
// it can check its other arguments (a summary written for the staged list,
// say) against what the person saw. See docs/APPROVAL-NARROWING.md, ADR-0089.

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// maxNarrowableValues bounds a narrowable list: a card shows at most
// approvalCardMaxItems rows, so a longer list could never be reviewed row by
// row anyway.
const maxNarrowableValues = approvalCardMaxItems

// maxApprovalBody bounds the single-card approve POST: up to
// maxNarrowableValues approved values of maxNarrowableValueRunes characters,
// each character at most 12 bytes once JSON-escaped (a surrogate pair of
// six-byte escapes): about 1.2 MiB, plus the small fixed fields. The route
// is exempt from the global 1 MiB JSON cap; handleApproval applies this one.
const maxApprovalBody = 4 << 20

// maxNarrowableValueRunes bounds one narrowable value: the card row that
// names it carries it as its `key`, whose limit is the same.
const maxNarrowableValueRunes = approvalCardMaxTitle

// narrowedRequestMetaKey is the `_meta` entry fleet adds to a narrowed
// approved call: {"arg": <name>, "staged": [<every staged value, in order>]}.
const narrowedRequestMetaKey = mcp.RequestMetaPrefix + "narrowed"

// approvalNarrowing is the record of a narrowed approval
// (store.Approval.NarrowedJSON): the argument, every value it was staged with,
// and the subset the person approved, in staged order.
type approvalNarrowing struct {
	Arg      string   `json:"arg"`
	Staged   []string `json:"staged"`
	Approved []string `json:"approved"`
}

// narrowableArgAtStaging is the argument a person may narrow on a card being
// staged now: the policy's critical_tool_narrowable_args entry for toolName,
// when the staged call passes stagedNarrowable with it, else "". It is frozen
// on the row (store.Approval.NarrowableArg) and read back from there by the
// card, every reload and the decision, never from the boot policy again: a
// restart with another entry cannot narrow an argument the person did not
// review.
func narrowableArgAtStaging(toolName, argsJSON string) string {
	arg, _, ok := stagedNarrowable(toolName, argsJSON, agentcore.NarrowableArgFor(toolName))
	if !ok {
		return ""
	}
	return arg
}

// stagedNarrowable reports whether arg (the argument frozen on the card at
// staging) can be narrowed on toolName's card, and its staged values. ok is
// false when arg is empty, or the staged value is not a list of
// 2..maxNarrowableValues distinct non-empty strings: such a card approves its
// call whole, as before.
func stagedNarrowable(toolName, argsJSON, arg string) (_ string, values []string, ok bool) {
	// An outbound email send (send_email, send_template_email) is never
	// narrowable, whatever the policy says: one approval covers the message
	// as written.
	// A tool of a synthetic server (inline http_tools, a2a peers) is never
	// narrowable either: its transport sends the arguments only, so the
	// endpoint would never see `_meta["fleet/narrowed"]` and could not check
	// its other arguments against the staged list.
	if !strings.HasPrefix(toolName, "mcp_") || handlerOnlyApproval(toolName) || tools.IsEmailToolName(toolName) ||
		agentcore.IsOutboundEmailTool(toolName) || onSyntheticTransport(toolName) {
		return "", nil, false
	}
	if arg == "" || len(argsJSON) > frozenArgsMaxBytes {
		return "", nil, false
	}
	var args map[string]any
	if decodeJSONNumbers([]byte(argsJSON), &args) != nil || args == nil {
		return "", nil, false
	}
	values, ok = narrowableValues(args[arg])
	if !ok || len(values) < 2 {
		return "", nil, false
	}
	return arg, values, true
}

// onSyntheticTransport reports whether toolName is a tool of one of fleet's
// synthetic servers: inline http_tools (mcp.HTTPToolServerName) or a2a peers
// (mcp.A2AToolServerName). Their transports forward a call's arguments only:
// no request `_meta` reaches the endpoint and Notify is a no-op, so nothing
// that relies on either (a narrowing's staged list, a Stop's cancellation)
// can be honoured there.
func onSyntheticTransport(toolName string) bool {
	for _, server := range []string{mcp.HTTPToolServerName, mcp.A2AToolServerName} {
		if strings.HasPrefix(toolName, "mcp_"+server+"_") {
			return true
		}
	}
	return false
}

// narrowableValues reads a staged list argument: distinct non-empty strings
// of at most maxNarrowableValueRunes characters (a card row's key, which must
// equal the value to be selectable, is bounded the same), at most
// maxNarrowableValues of them.
func narrowableValues(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 || len(list) > maxNarrowableValues {
		return nil, false
	}
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, isString := e.(string)
		if !isString || s == "" || seen[s] || utf8.RuneCountInString(s) > maxNarrowableValueRunes {
			return nil, false
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, true
}

// narrowApproval checks the values a person approved against a's staged call
// and returns the narrowing to record, or nil when they approved every staged
// value (the call runs whole, exactly as an un-narrowed approval). shownArg is
// the argument the card showed (its `narrowable.arg`), echoed by the client:
// the decision is bound to it, so a policy that changed since the card was
// shown (a restart with another critical_tool_narrowable_args entry) can
// never narrow an argument the person did not review. Any error is the
// client's: the request is refused and nothing is claimed.
func narrowApproval(a *store.Approval, approved []string, shownArg string) (*approvalNarrowing, error) {
	arg, staged, ok := stagedNarrowable(a.ToolName, a.ArgsJSON, a.NarrowableArg)
	if !ok {
		return nil, errors.New("this action cannot be narrowed: approve or decline it whole")
	}
	if shownArg == "" {
		return nil, errors.New("narrowed_arg is missing: send the argument the card narrows with the approved values")
	}
	if shownArg != arg {
		return nil, fmt.Errorf("this card now narrows %q, not %s (fleet's policy changed after it was shown): reload it and decide again", arg, quoteClientValue(shownArg))
	}
	if len(approved) == 0 {
		return nil, fmt.Errorf("approved_values is empty: to leave every %s out, decline the action", arg)
	}
	// A subset names at most every staged value once: anything longer is
	// refused before a map is sized by the client's count (the body may hold
	// hundreds of thousands of short repeats).
	if len(approved) > len(staged) {
		return nil, fmt.Errorf("approved_values names %d values, more than the %d staged %s", len(approved), len(staged), arg)
	}
	inStaged := make(map[string]bool, len(staged))
	for _, v := range staged {
		inStaged[v] = true
	}
	picked := make(map[string]bool, len(approved))
	for i, v := range approved {
		// No staged value is longer than maxNarrowableValueRunes, so a longer
		// one is refused by its position before it could be quoted back: the
		// body may carry a single multi-megabyte value.
		if len(v) > maxNarrowableValueRunes*utf8.UTFMax || utf8.RuneCountInString(v) > maxNarrowableValueRunes {
			return nil, fmt.Errorf("approved_values item %d is longer than %d characters, so it is not one of the staged %s", i+1, maxNarrowableValueRunes, arg)
		}
		if !inStaged[v] {
			return nil, fmt.Errorf("approved_values names %s, which is not one of the staged %s", quoteClientValue(v), arg)
		}
		if picked[v] {
			return nil, fmt.Errorf("approved_values names %s more than once", quoteClientValue(v))
		}
		picked[v] = true
	}
	if len(picked) == len(staged) {
		return nil, nil
	}
	n := &approvalNarrowing{Arg: arg, Staged: staged, Approved: make([]string, 0, len(picked))}
	for _, v := range staged {
		if picked[v] {
			n.Approved = append(n.Approved, v)
		}
	}
	// The line that tells the model exactly which values were approved is
	// never shortened (the model must never read a left-out value as
	// approved), and the result must stay within the operator's output
	// limit. With a limit near the minimum, a long scattered selection cannot
	// have both, so it is refused here, before anything is claimed.
	if len(narrowingNote(n))+approvalStopTrailerReserve(a.ToolName) > agentcore.ApprovalTrailerRoom() {
		return nil, fmt.Errorf("this selection is too scattered to record exactly within this deployment's tool output limit: select fewer separate ranges of %s, or approve or decline the action whole", arg)
	}
	return n, nil
}

// maxQuotedClientRunes bounds how much of one client-sent string an error
// reply quotes back.
const maxQuotedClientRunes = 80

// quoteClientValue quotes a client-sent string (an approval id, an approved
// value, an argument name) for an error reply, cut to maxQuotedClientRunes
// characters: a request may carry one value of megabytes, and an error must
// never echo it whole. A cut value is marked with an ellipsis after the
// closing quote, so it cannot be read as the value itself.
func quoteClientValue(s string) string {
	if len(s) <= maxQuotedClientRunes {
		return fmt.Sprintf("%q", s)
	}
	n := 0
	for i := range s {
		if n == maxQuotedClientRunes {
			return fmt.Sprintf("%q…", s[:i])
		}
		n++
	}
	return fmt.Sprintf("%q", s)
}

// requestedNarrowing checks a request's approved_values (nil when it names
// none, or names every staged value): only with an approval decided once, and
// only a narrowing narrowApproval accepts. An error is the client's.
func requestedNarrowing(a *store.Approval, req approvalRequest) (*approvalNarrowing, error) {
	if !req.ApprovedValues.present {
		return nil, nil
	}
	if req.ApprovedValues.values == nil {
		// An explicit null is refused, never read as "approve everything": a
		// client that meant to send a subset must not run the whole list.
		return nil, errors.New("approved_values is null: send the approved values, or leave the field out to approve the whole action")
	}
	if !req.Approved {
		return nil, errors.New("approved_values goes with an approval; to decline, send approved false without it")
	}
	if scope := strings.TrimSpace(req.Scope); scope != "" && scope != "once" {
		return nil, errors.New("a narrowed approval covers this call only; use scope once")
	}
	return narrowApproval(a, req.ApprovedValues.values, req.NarrowedArg)
}

// claimApproved claims a pending card as approved and executing: the plain
// claim, or, for a narrowed approval, the claim that writes the narrowing in
// the same UPDATE, so the row can never be approved without what the person
// approved. On a won narrowed claim it sets a.NarrowedJSON (execution reads
// it) and logs the audit line.
func (s *Server) claimApproved(ctx context.Context, user, convID string, a *store.Approval, n *approvalNarrowing) (bool, error) {
	if n == nil {
		return s.store.ClaimApproval(ctx, user, a.ID, "approved", approvalExecutingSentinel)
	}
	record, err := json.Marshal(n)
	if err != nil {
		return false, err
	}
	claimed, err := s.store.ClaimApprovalNarrowed(ctx, user, a.ID, approvalExecutingSentinel, string(record))
	if err == nil && claimed {
		a.NarrowedJSON = string(record)
		// Counts and staged positions only: the values are model-authored and
		// may carry anything, so they never reach the log. The exact record is
		// on the row (narrowed_json).
		//nolint:gosec // G706: every interpolated value is rendered with %q or %d, which escapes any CR/LF.
		log.Printf("audit: %q approved %d of %d %q of approval %q (%q) in conversation %q; left out staged positions %q",
			user, len(n.Approved), len(n.Staged), n.Arg, a.ID, a.ToolName, convID, n.leftOutPositions())
	}
	return claimed, err
}

// approvedValuesField is the approve request's `approved_values`, with its
// presence kept apart from its value: absent approves the call whole, while
// an explicit null is refused (Go would otherwise decode null and absent to
// the same nil slice).
type approvedValuesField struct {
	present bool
	values  []string // nil for an explicit null
}

// UnmarshalJSON records that the field was sent, and its values.
func (f *approvedValuesField) UnmarshalJSON(b []byte) error {
	f.present = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		f.values = nil
		return nil
	}
	var values []string
	if err := json.Unmarshal(b, &values); err != nil {
		return err
	}
	if values == nil {
		values = []string{}
	}
	f.values = values
	return nil
}

// groupApprovedValues is a group decision's narrowing for one card as the
// single-card field: absent when the card is not in `narrow`.
// checkGroupNarrowing has already refused a null entry.
func groupApprovedValues(narrow map[string][]string, id string) approvedValuesField {
	values, ok := narrow[id]
	if !ok {
		return approvedValuesField{}
	}
	if values == nil {
		values = []string{}
	}
	return approvedValuesField{present: true, values: values}
}

// parseApprovalNarrowing decodes a stored narrowing record strictly.
func parseApprovalNarrowing(raw string) (*approvalNarrowing, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var n approvalNarrowing
	if err := dec.Decode(&n); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	if n.Arg == "" || len(n.Approved) == 0 || len(n.Approved) >= len(n.Staged) {
		return nil, errors.New("not a narrowing")
	}
	return &n, nil
}

// approvalExecutionArgs is the argument map an approved call runs with: the
// staged arguments, with the narrowed argument replaced by the approved
// values when the card was approved narrowed. The record is checked against
// the staged call itself (the argument holds exactly the recorded staged
// values, and the approved ones are a non-empty subset in staged order): a
// record that does not match refuses the run. Running the whole staged list
// instead would run values the person left out.
func approvalExecutionArgs(a *store.Approval) (map[string]any, *approvalNarrowing, error) {
	var args map[string]any
	if err := decodeJSONNumbers([]byte(a.ArgsJSON), &args); err != nil {
		return nil, nil, fmt.Errorf("parse args: %w", err)
	}
	if a.NarrowedJSON == "" {
		return args, nil, nil
	}
	refuse := func(why string) (map[string]any, *approvalNarrowing, error) {
		return nil, nil, fmt.Errorf("the approval was narrowed, but its record %s; nothing was run", why)
	}
	n, err := parseApprovalNarrowing(a.NarrowedJSON)
	if err != nil {
		return refuse("could not be read")
	}
	staged, ok := narrowableValues(args[n.Arg])
	if !ok || len(staged) != len(n.Staged) {
		return refuse("does not match the staged call")
	}
	for i := range staged {
		if staged[i] != n.Staged[i] {
			return refuse("does not match the staged call")
		}
	}
	next := 0
	approved := make([]any, 0, len(n.Approved))
	for _, v := range n.Approved {
		for next < len(staged) && staged[next] != v {
			next++
		}
		if next == len(staged) {
			return refuse("approves a value that was not staged, or out of order")
		}
		approved = append(approved, v)
		next++
	}
	args[n.Arg] = approved
	return args, n, nil
}

// narrowingRequestMeta is the `_meta` entry a narrowed call carries, so its
// server can check its other arguments against the staged list.
func narrowingRequestMeta(n *approvalNarrowing) map[string]any {
	return map[string]any{narrowedRequestMetaKey: map[string]any{
		"arg":    n.Arg,
		"staged": append([]string(nil), n.Staged...),
	}}
}

// leftOutPositions is the 1-based staged positions the person did not
// approve, as ranges ("2, 5-7"): what the audit line names instead of values.
func (n *approvalNarrowing) leftOutPositions() string {
	picked := make(map[string]bool, len(n.Approved))
	for _, v := range n.Approved {
		picked[v] = true
	}
	var pos []int
	for i, v := range n.Staged {
		if !picked[v] {
			pos = append(pos, i+1)
		}
	}
	return store.PositionRanges(pos)
}

// narrowingNote is fleet's line under a narrowed call's result, in the
// history the model reads (store.NarrowingNote, shared with the restart
// recovery): what the person approved and what they left out.
func narrowingNote(n *approvalNarrowing) string {
	return store.NarrowingNote(n.Arg, n.Staged, n.Approved)
}

// approvalTrailer joins fleet's labelled lines under an approved call's
// result: the narrowing line (when narrowed is set) and then others. Never
// shortened: narrowApproval refuses, at decision, a narrowing whose exact line
// would not fit agentcore.ApprovalTrailerRoom, so the tool text and the lines
// together stay within the configured output limit.
func approvalTrailer(narrowed *approvalNarrowing, others ...string) string {
	parts := make([]string, 0, 1+len(others))
	if narrowed != nil {
		parts = append(parts, narrowingNote(narrowed))
	}
	return strings.Join(append(parts, others...), "\n\n")
}

// withApprovalNarrowable adds a pending card's `narrowable` field: the
// argument frozen on the row at staging and its staged values. Absent when the
// card approves its call whole, so every other payload is unchanged.
func withApprovalNarrowable(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil || a.Status != "pending" {
		return payload
	}
	arg, values, ok := stagedNarrowable(a.ToolName, a.ArgsJSON, a.NarrowableArg)
	if !ok {
		return payload
	}
	// critical_tool_narrowable_args: the describer card's rows that name a
	// staged value get a checkbox (docs/APPROVAL-NARROWING.md).
	payload["narrowable"] = map[string]any{"arg": arg, "values": values}
	return payload
}

// withApprovalNarrowed adds a settled card's `narrowed` record (what the
// person approved, what they left out) when it was approved narrowed. Absent
// for every other row, so its payload is unchanged.
func withApprovalNarrowed(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil || a.NarrowedJSON == "" {
		return payload
	}
	n, err := parseApprovalNarrowing(a.NarrowedJSON)
	if err != nil {
		return payload
	}
	payload["narrowed"] = n
	return payload
}
