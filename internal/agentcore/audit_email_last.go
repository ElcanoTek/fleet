package agentcore

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
)

// Email-last batch rules (ported from the v1 engine: cutlass #749.3 abort
// notification, a4b76d83 email-last invariant, #1087 settled-failed creates
// and re-audit restate).
//
// A batch of record writes reports by ONE summary email sent as its LAST
// outward step: the email is the record of every outcome, and the per-run
// email cap means a post-email retry leaves the reader a stale report no later
// email can correct. The bundle names the tools this applies to
// (agent_policy.email_last_tools / settleable_create_tools — fleet knows none
// of them), and once a run declares or attempts one:
//
//   - send_email is refused while any commitment on an email-last tool is
//     UNSETTLED (checkSummaryEmailOrder);
//   - every email-last call is refused after the summary email has gone out
//     (checkEmailLastOrder);
//   - a create whose latest attempt failed DEFINITIVELY is settled: it goes on
//     the sheet as failed, so it no longer holds the email back, and — after
//     the email — no longer blocks finish (settleCreateFailure). An AMBIGUOUS
//     failure (the record may exist) never settles; and
//   - an abort (confirm_audit success=false) still owes the reader its single
//     failure-summary email: send_email stays callable for exactly that one
//     send, and finish is refused (bounded) until it is sent.
//
// Field case (v1, 2026-10-05): a 3-record batch booked two, the third failed
// with a permission error no argument change could fix, the commitment stayed
// open, and the only exit was an abort — which then blocked send_email as
// "requires audit first" and let finish pass with no email at all. The reader
// got silence for a batch that was two-thirds live, and the results callback
// that follows the email never ran.

// maxAbortNotifyNudges caps how many times finish is refused after an abort to
// demand the failure-summary email. Three refusals give the model ample rounds
// to build and send it; past that the run ends with the abort recorded, so a
// deterministically-broken email path cannot burn the enforcement-round
// budget.
const maxAbortNotifyNudges = 3

// templateEmailToolSuffix is the other base outbound-email suffix
// (baseCriticalToolSuffixes). isEmailTool matches only send_email.
const templateEmailToolSuffix = "send_template_email"

// isSummaryEmailTool reports whether toolName is an outbound email that can be
// a batch's summary email: send_email or send_template_email. Both are held
// back while email-last work is unsettled, and a successful one of either
// marks the summary as sent.
func isSummaryEmailTool(toolName string) bool {
	return isEmailTool(toolName) || toolName == templateEmailToolSuffix ||
		strings.HasSuffix(toolName, "_"+templateEmailToolSuffix)
}

func isEmailLastSuffix(suffix string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return suffix != "" && activeEmailLastSuffixes[suffix]
}

func isSettleableCreateSuffix(suffix string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return suffix != "" && activeSettleableCreateSuffixes[suffix]
}

// ── outcome classification ──

// ambiguousCreateOutcomeMarkers are fragments a FAILED create's result carries
// when its outcome is UNKNOWN — the record MAY exist live. The first three are
// the cutlass-family bundle servers' blocker codes (*_ambiguous_transport /
// *_outcome_unknown after a transport death post-send; deal_already_created for
// an exception raised after the create POST succeeded). The rest are fleet's
// own MCP transport texts (internal/mcp/client.go): a dead or desynced stdio
// transport, and the restart path's "outcome is UNKNOWN". Those normally
// arrive as a failed call (never settled anyway); matching them in a payload
// too is belt and braces. Matched anywhere, case-insensitively — a false match
// only keeps the email gate closed, the safe direction.
var ambiguousCreateOutcomeMarkers = []string{
	"ambiguous_transport", "outcome_unknown", "deal_already_created",
	"request not delivered", "transport marked dead", "outcome is unknown",
}

// ambiguousCreateStateRe matches result fields that say the record MAY (or
// does) exist after a failed create: `write_state: "unknown"` (5xx or no
// response), and `written: true` / `deal_created: true` (it was written;
// repair it, never re-create it).
var ambiguousCreateStateRe = regexp.MustCompile(
	`(?i)"write_state"\s*:\s*"unknown"|"written"\s*:\s*true|"deal_created"\s*:\s*true`)

// createCallFailedRe matches the bundle servers' generic `<ssp>_create_call_failed`
// code, reported for ANY failed create POST — a 4xx the server answered
// (definitive) or a 5xx / exception after the request was sent (the record MAY
// exist). The v1 engine listed four servers by name; fleet knows no server, so
// it matches the code shape and treats every such failure as ambiguous unless
// the result proves a rejection (createDefinitiveRejectionRe). That can hold a
// server's answered non-4xx failure open where v1 settled it — the safe
// direction.
var createCallFailedRe = regexp.MustCompile(`(?i)[a-z0-9]_create_call_failed`)

// createDefinitiveRejectionRe matches evidence that a create the server
// handled was REJECTED (nothing written): an HTTP 4xx status in the servers'
// message / field shapes, or a coded pre-send / in-body rejection.
var createDefinitiveRejectionRe = regexp.MustCompile(
	`(?i)\bhttp[ /]*4\d\d\b|"status(?:_code)?"\s*:\s*4\d\d\b|client error '4\d\d|` +
		`_create_rejected|_missing_deal_id|_args_invalid|_precondition_failed`)

// ambiguousCreateOutcome reports whether a FAILED create's result leaves the
// record's existence unknown (or known to exist), so it must not settle.
func ambiguousCreateOutcome(resultText string) bool {
	lower := strings.ToLower(resultText)
	for _, m := range ambiguousCreateOutcomeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	if ambiguousCreateStateRe.MatchString(resultText) {
		return true
	}
	if createCallFailedRe.MatchString(resultText) {
		return !createDefinitiveRejectionRe.MatchString(resultText)
	}
	return false
}

// ── deal identity ──

// dealNameKeys are the argument keys a create-family call names its record by
// (the cutlass-family wire contract): `name`, `deal_name` and `display_name`.
var dealNameKeys = []string{"name", "deal_name", "display_name"}

// dealNameContainers are the nested objects a raw create carries its body in,
// searched after the top level, in order: `payload`, `payload.deal`, `deal`,
// and a clone's `overrides`.
var dealNameContainers = [][]string{{"payload"}, {"payload", "deal"}, {"deal"}, {"overrides"}}

// preparedDealIDKey is the argument (a create from a prepared handle) and
// result (the prepare step) key of a two-step create's handle.
const preparedDealIDKey = "prepared_deal_id"

// normalizeDealName case-folds and collapses whitespace.
func normalizeDealName(v string) string {
	return strings.Join(strings.Fields(strings.ToLower(v)), " ")
}

// asArgMap returns v as a JSON object: a decoded object, or a string holding
// one (models sometimes pass a nested payload JSON-encoded).
func asArgMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(t)), &m) == nil && m != nil {
			return m, true
		}
	}
	return nil, false
}

// callDealName returns the normalized record name a call carries, top level
// first, then the containers in dealNameContainers. "" when none does.
func callDealName(args map[string]any) string {
	scopes := []map[string]any{args}
	for _, path := range dealNameContainers {
		cur, ok := args, true
		for _, key := range path {
			if cur, ok = asArgMap(cur[key]); !ok {
				break
			}
		}
		if ok {
			scopes = append(scopes, cur)
		}
	}
	for _, scope := range scopes {
		for _, key := range dealNameKeys {
			if v, ok := scope[key].(string); ok {
				if name := normalizeDealName(v); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// recordPreparedDeal remembers which record a successful prepare step built:
// a call that named a record and whose JSON result returned a prepared_deal_id
// handle. The later create from that handle carries only the handle, so this
// is how attemptDealName ties it back to the record. Callers must hold o.mu.
func (o *orchestrationState) recordPreparedDeal(rawInput, resultText string, succeeded bool) {
	if !succeeded || !strings.Contains(resultText, preparedDealIDKey) {
		return
	}
	args, err := unmarshalArgs(rawInput)
	if err != nil {
		return
	}
	name := callDealName(args)
	if name == "" {
		return
	}
	var res map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(resultText)), &res) != nil {
		return
	}
	id, _ := res[preparedDealIDKey].(string)
	if id = strings.TrimSpace(id); id == "" {
		return
	}
	if o.preparedDealNames == nil {
		o.preparedDealNames = make(map[string]string)
	}
	o.preparedDealNames[id] = name
}

// attemptDealName is the normalized record NAME a create-family call carries
// (top level or nested, or its prepared handle's prepare-step name); "" when
// none is readable. Callers must hold o.mu.
func (o *orchestrationState) attemptDealName(rawInput string) string {
	args, err := unmarshalArgs(rawInput)
	if err != nil {
		return ""
	}
	if name := callDealName(args); name != "" {
		return name
	}
	if id, ok := args[preparedDealIDKey].(string); ok {
		return o.preparedDealNames[strings.TrimSpace(id)]
	}
	return ""
}

// attemptIdentity names the record a create-family call targets — its name
// (attemptDealName), else its record id, normalized. Ties a create's later
// attempts (a corrected retry, an ambiguous re-run) to the commitment its
// earlier failure settled. "" when the call names none. Callers must hold
// o.mu.
func (o *orchestrationState) attemptIdentity(rawInput string) string {
	if name := o.attemptDealName(rawInput); name != "" {
		return name
	}
	return strings.ToLower(callDealID(rawInput))
}

// namedCreateRefusal is commitmentAuthorizes' BLOCKED text for a create whose
// record name matches none of the open NAMED units it could ride. dealName ""
// means the call named no readable record at all.
func namedCreateRefusal(toolName, dealName string, names []string) string {
	sort.Strings(names)
	if dealName == "" {
		log.Printf("Enforcement: Blocking %s — it names no readable record, and the audit binds this tool's open "+
			"create units to record names %v (create_deal_name_not_declared)", toolName, names)
		return fmt.Sprintf("BLOCKED [create_deal_name_not_declared]: '%s' names no record this gate can read — no "+
			"name / deal_name / display_name (top level or in payload / deal / overrides), and no prepared_deal_id "+
			"returned by a successful prepare step in this run — but this audit's open %s units are bound to these "+
			"deal_name values: %s. Pass the record's declared name on the call (or create from the handle its "+
			"prepare step returned); never create a declared record through a call that cannot show its name.", toolName, criticalSuffixFor(toolName), strings.Join(names, "; "))
	}
	log.Printf("Enforcement: Blocking %s creating record %q — the audit binds this tool's open create units to "+
		"other record names %v (create_deal_name_not_declared)", toolName, dealName, names)
	return fmt.Sprintf("BLOCKED [create_deal_name_not_declared]: '%s' would create %q, but this audit's open %s "+
		"units are bound to these deal_name values: %s. Create each record under its declared name exactly (case "+
		"and spacing are ignored). If this record must be created under a new or corrected name, re-run "+
		"confirm_audit declaring it (typed critical_actions entry with deal_name) first — never rename a record "+
		"on a retry.", toolName, dealName, criticalSuffixFor(toolName), strings.Join(names, "; "))
}

// ── settled-failed creates ──

// createCommitmentsFor returns the outstanding UNBOUND settleable-create
// commitments an executed toolName could discharge, in registration order.
// Callers must hold o.mu.
func (o *orchestrationState) createCommitmentsFor(toolName string) []*typedCommitment {
	var out []*typedCommitment
	for _, c := range o.typedCommitments {
		if c.remaining > 0 && !c.hasDealBinding() && isSettleableCreateSuffix(c.suffix) && c.nameMatches(toolName) {
			out = append(out, c)
		}
	}
	return out
}

// declaredDealName reports whether identity is the record name of a create
// unit (open or done) that toolName could discharge. Callers must hold o.mu.
func (o *orchestrationState) declaredDealName(toolName, identity string) bool {
	if identity == "" {
		return false
	}
	for _, c := range o.typedCommitments {
		if c.dealName == identity && c.nameMatches(toolName) {
			return true
		}
	}
	return false
}

// splitCreateCommitments resolves a create attempt against the outstanding
// unbound create commitments for toolName: named is the unit declared with
// this attempt's record name, if any; otherwise unnamed holds the count-based
// candidates. A named unit never stands for an attempt under another name, and
// a declared name never consumes an unnamed sibling (a re-run of a booked
// record must not). Callers must hold o.mu.
func (o *orchestrationState) splitCreateCommitments(toolName, identity string) (named *typedCommitment, unnamed []*typedCommitment) {
	if o.declaredDealName(toolName, identity) {
		for _, c := range o.createCommitmentsFor(toolName) {
			if c.dealName == identity {
				return c, nil
			}
		}
		return nil, nil
	}
	for _, c := range o.createCommitmentsFor(toolName) {
		if c.dealName == "" {
			unnamed = append(unnamed, c)
		}
	}
	return nil, unnamed
}

// noteCreateFailure applies a FAILED settleable-create attempt to the
// settled-failed bookkeeping: a definitive failure (the tool ran and reported
// failure, with no ambiguous marker) settles one unit; anything else — an
// ambiguous marker, a transport error, an is-error result, where the tool may
// have written before it died — leaves (or puts back) the record's unit as
// unsettled. Callers must hold o.mu.
func (o *orchestrationState) noteCreateFailure(toolName, rawInput, resultText, argsHash string, succeeded bool) {
	if !isSettleableCreateSuffix(criticalSuffixFor(toolName)) {
		return
	}
	identity := o.attemptIdentity(rawInput)
	if succeeded && mcpReportedFailure(resultText) && !ambiguousCreateOutcome(resultText) {
		o.settleCreateFailure(toolName, argsHash, identity)
		return
	}
	o.unsettleCreateFailure(toolName, identity)
}

// settleCreateFailure records a DEFINITIVE create failure: the record's own
// settled-failed unit (same identity) stays settled, else the first unsettled
// matching unit becomes settled-failed. A failure with nothing left to settle
// (more attempts than declared records) changes nothing. Callers must hold
// o.mu.
func (o *orchestrationState) settleCreateFailure(toolName, argsHash, identity string) {
	o.retirePendingCreate(toolName, argsHash)
	named, cands := o.splitCreateCommitments(toolName, identity)
	if named != nil {
		if !named.settledFailed {
			named.settledFailed, named.failedIdentity = true, identity
			log.Printf("Enforcement: create commitment %q settled-failed by a definitive failure; it no longer "+
				"holds the summary email back", named.describe())
		}
		return
	}
	if identity != "" {
		for _, c := range cands {
			if c.settledFailed && c.failedIdentity == identity {
				return
			}
		}
	}
	for _, c := range cands {
		if !c.settledFailed {
			c.settledFailed, c.failedIdentity = true, identity
			log.Printf("Enforcement: create commitment %q settled-failed by a definitive failure (record %q); it "+
				"no longer holds the summary email back", c.describe(), identity)
			return
		}
	}
	if identity != "" {
		log.Printf("Enforcement: definitive create failure for record %q matches no open unit it may settle — "+
			"it settles nothing", identity)
	}
}

// retirePendingCreate drops the pre-audit pending entry a definitive create
// failure acted on — the exact (tool, args) entry, else the oldest record-less
// entry of the same tool or one it stands in for (sameOrStandInTool: a
// same-server alias twin or approved substitute, which is how the failure
// settled that tool's unit) — so a create blocked before the audit and then
// attempted after it cannot hold the summary email back (markPendingCriticalDone
// covers the success side). Not recorded as completed. Callers must hold o.mu.
func (o *orchestrationState) retirePendingCreate(toolName, argsHash string) {
	idx := -1
	for i, p := range o.pendingCriticalActions {
		if !sameOrStandInTool(p.toolName, toolName) {
			continue
		}
		if p.toolName == toolName && p.argsHash == argsHash {
			idx = i
			break
		}
		if idx < 0 && p.record == "" {
			idx = i
		}
	}
	if idx >= 0 {
		o.pendingCriticalActions = append(o.pendingCriticalActions[:idx], o.pendingCriticalActions[idx+1:]...)
		log.Printf("Enforcement: retired pre-audit pending %s — the create was since attempted and failed definitively", toolName)
	}
}

// unsettleCreateFailure records an AMBIGUOUS (or transport-level) create
// outcome: the record MAY exist, so its unit must hold the email back again.
// The record's own settled-failed unit (same identity) is cleared; failing an
// identity match, and when no unsettled unit remains to stand for this
// attempt, the first settled-failed one is cleared — an ambiguous outcome
// always leaves at least one unit unsettled. Callers must hold o.mu.
func (o *orchestrationState) unsettleCreateFailure(toolName, identity string) {
	unsettle := func(c *typedCommitment) {
		if c.settledFailed {
			c.settledFailed, c.failedIdentity = false, ""
			log.Printf("Enforcement: create commitment %q is UNSETTLED again — the latest attempt (record %q) had "+
				"an ambiguous outcome; it holds the summary email back until reconciled", c.describe(), identity)
		}
	}
	named, cands := o.splitCreateCommitments(toolName, identity)
	if named != nil {
		unsettle(named)
		return
	}
	if identity != "" {
		for _, c := range cands {
			if c.settledFailed && c.failedIdentity == identity {
				unsettle(c)
				return
			}
		}
	}
	for _, c := range cands {
		if !c.settledFailed {
			return
		}
	}
	if len(cands) > 0 {
		unsettle(cands[0])
	}
}

// settledFailedSuffixes returns one suffix entry per outstanding
// settled-failed unit, sorted (the shape of unexecutedCommitments). Callers
// must hold o.mu.
func (o *orchestrationState) settledFailedSuffixes() []string {
	var out []string
	for _, c := range o.typedCommitments {
		if c.remaining > 0 && c.settledFailed {
			for i := 0; i < c.remaining; i++ {
				out = append(out, c.suffix)
			}
		}
	}
	sort.Strings(out)
	return out
}

// subtractEach removes one occurrence from list for each entry of remove.
func subtractEach(list, remove []string) []string {
	if len(remove) == 0 {
		return list
	}
	drop := make(map[string]int, len(remove))
	for _, r := range remove {
		drop[r]++
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if drop[v] > 0 {
			drop[v]--
			continue
		}
		out = append(out, v)
	}
	return out
}

// ── re-audit restate ──

// batchRestateLimits opens a fresh batch when the ledger is exhausted (or the
// batch maps are unset) and returns, per FULL tool name, how many of this
// envelope's UNNAMED unbound create entries restate units the batch already
// holds (see restateUnboundEntry). Callers must hold o.mu.
func (o *orchestrationState) batchRestateLimits(actions []criticalActionStruct) map[string]int {
	if o.unboundBatchUnits == nil || o.namedBatchUnits == nil || o.allCommitmentsExhausted() {
		o.unboundBatchUnits = make(map[string]int)
		o.namedBatchUnits = make(map[string]map[string]bool)
	}
	// Unnamed entries restate against the units the batch held BEFORE this
	// envelope. An envelope that names any record restates its unnamed
	// entries against the batch's prior UNNAMED units only — it names what it
	// names.
	limits := make(map[string]int, len(o.unboundBatchUnits))
	envelopeNamed := envelopeHasDealNames(actions)
	for tool, n := range o.unboundBatchUnits {
		if envelopeNamed {
			n -= len(o.namedBatchUnits[tool])
		}
		limits[tool] = n
	}
	return limits
}

// unboundCreateEntry reports whether a typed entry is an unbound (no real
// record id) entry for a settleable create tool.
func unboundCreateEntry(a criticalActionStruct, suffix string) bool {
	return len(declaredDealIDs(a.DealIDs)) == 0 && declaredDealID(a.DealID) == "" && isSettleableCreateSuffix(suffix)
}

// envelopeHasDealNames reports whether any typed entry is a NAMED create unit.
func envelopeHasDealNames(actions []criticalActionStruct) bool {
	for _, a := range actions {
		if normalizeDealName(a.DealName) != "" && unboundCreateEntry(a, criticalSuffixFor(strings.TrimSpace(a.Tool))) {
			return true
		}
	}
	return false
}

// restateUnboundEntry applies the re-audit restate rule to one typed entry
// (and marks the run as email-last work when the entry's tool is one), and
// returns the entry's effective record name ("" unless it is a named create
// unit) and whether the entry RESTATES a unit this batch already holds (so it
// registers nothing). Only unbound settleable-create entries restate; every
// other entry returns ("", false) and keeps the ordinary registration and
// supersede rules. An entry that will register is counted into the batch
// here. Callers must hold o.mu.
//
// Why (v1 9-record run, 2026-10-05): six creates booked, three failed, and the
// model re-declared the whole batch to "settle". Unbound creates carry no
// identity a re-audit could match, so each re-declaration either stacked
// (v1: 10 → 14 → 15 outstanding) or, under fleet's unbound supersede, retired
// the open units and registered nine fresh ones — six phantom obligations to
// re-create records that were already live. The batch therefore counts its
// units per FULL tool name: a re-audit's first that-many unnamed entries for
// the tool restate them, and only entries BEYOND that total are new work. A
// NAMED entry (deal_name) restates by name: same tool + same name is the same
// unit. Settled-failed state survives a restate, so a re-audit never forces a
// retry. The one under-count — a re-audit declaring only what is left PLUS a
// new record — fails safe: the extra create is refused as matching no
// outstanding commitment until the ledger empties.
func (o *orchestrationState) restateUnboundEntry(a criticalActionStruct, tool, suffix string,
	unnamedLimit, declaredUnbound map[string]int) (dealName string, restate bool) {
	// Declaring email-last work makes the run email-last work, restated or
	// not (abortEmailAllowed, abortNotifyFinishNudge).
	if isEmailLastSuffix(suffix) {
		o.emailLastAttempted = true
	}
	unbound := unboundCreateEntry(a, suffix)
	dealName = normalizeDealName(a.DealName)
	if dealName != "" && !unbound {
		log.Printf("Enforcement: ignoring deal_name %q on %q — deal_name binds only an unbound create entry "+
			"of a settleable_create_tools tool", a.DealName, tool)
		dealName = ""
	}
	switch {
	case !unbound:
		return "", false
	case dealName != "":
		if o.namedBatchUnits[tool][dealName] {
			log.Printf("Enforcement: re-audit restates named create commitment %q (record name %q) — no new "+
				"obligation registered", tool, dealName)
			return dealName, true
		}
		if o.namedBatchUnits[tool] == nil {
			o.namedBatchUnits[tool] = make(map[string]bool)
		}
		o.namedBatchUnits[tool][dealName] = true
	default:
		declaredUnbound[tool]++
		if declaredUnbound[tool] <= unnamedLimit[tool] {
			log.Printf("Enforcement: re-audit restates unbound create commitment %q (%d of %d this batch already "+
				"holds) — no new obligation registered", tool, declaredUnbound[tool], unnamedLimit[tool])
			return "", true
		}
	}
	o.unboundBatchUnits[tool]++
	return dealName, false
}

// ── email-last gates ──

// checkEmailLastOrder runs before the audit gate for every critical call. An
// email-last call after the summary email is refused (no write may follow the
// record of the batch's outcomes); otherwise it marks the run as email-last
// work, which obligates the failure-summary email on a later abort. Callers
// must hold o.mu.
func (o *orchestrationState) checkEmailLastOrder(toolName string) (bool, string) {
	if !isEmailLastSuffix(criticalSuffixFor(toolName)) {
		return false, ""
	}
	if o.summaryEmailSent {
		log.Printf("Enforcement: Blocking %s — the batch's summary email has already been sent", toolName)
		return true, fmt.Sprintf("BLOCKED: '%s' — the batch's summary email has already been sent, and that email "+
			"must be the batch's LAST outward step. No create, retry or update may follow it in this run: the "+
			"email already records each item's final outcome. Report any remaining failure in your final summary "+
			"and finish; a retry belongs in a new run.", toolName)
	}
	o.emailLastAttempted = true
	return false, ""
}

// abortEmailAllowed reports whether toolName is the single failure-summary
// email an aborted email-last run may still send with no live audit: the abort
// left auditConfirmed=false, which would otherwise block it as "requires audit
// first" and turn a failed batch into silence. Only the email family, only
// until the first summary email has gone out; the send cap and duplicate
// guard still apply, and it grants nothing a typed audit of the email could
// not. Callers must hold o.mu.
func (o *orchestrationState) abortEmailAllowed(toolName string) bool {
	return o.auditTerminalFailure && o.emailLastAttempted && !o.summaryEmailSent && isEmailTool(toolName)
}

// unsettledEmailLastWork names every email-last obligation that must settle
// before the summary email: outstanding typed commitments that are not
// settled-failed, legacy (suffix-scoped) headroom, and calls blocked before
// the audit and never retried. Callers must hold o.mu.
func (o *orchestrationState) unsettledEmailLastWork() []string {
	var out []string
	for _, c := range o.typedCommitments {
		if c.remaining > 0 && !c.settledFailed && isEmailLastSuffix(c.suffix) {
			out = append(out, c.describe())
		}
	}
	for suffix := range o.committedCriticalActions {
		if headroom := o.legacyHeadroomFor(suffix); headroom > 0 && isEmailLastSuffix(suffix) {
			out = append(out, fmt.Sprintf("%s x%d", suffix, headroom))
		}
	}
	for _, p := range o.pendingCriticalActions {
		if isEmailLastSuffix(criticalSuffixFor(p.toolName)) {
			out = append(out, p.toolName+" (blocked before the audit, never retried)")
		}
	}
	sort.Strings(out)
	return out
}

// checkSummaryEmailOrder refuses send_email while email-last work is
// unsettled, so the summary email always reports FINAL outcomes (v1 ELC07255,
// 2026-07-20: the email reported a failure at :51, finish enforcement pushed a
// corrected retry that succeeded at :53, and the one-email cap left the reader
// the stale report). The all-failed path cannot deadlock: the abort allowance
// still admits the single failure-summary email. Callers must hold o.mu.
func (o *orchestrationState) checkSummaryEmailOrder(toolName string) (bool, string) {
	if !isSummaryEmailTool(toolName) {
		return false, ""
	}
	missing := o.unsettledEmailLastWork()
	if len(missing) == 0 {
		return false, ""
	}
	log.Printf("Enforcement: Blocking %s — email-last work is still unsettled: %v", toolName, missing)
	return true, fmt.Sprintf("BLOCKED: '%s' — these declared actions are still unsettled: %s. The batch's summary "+
		"email must be its LAST step so it reports FINAL outcomes (a retry after the email cannot correct it). "+
		"Settle each one first: execute it now (retrying with corrected arguments resets the retry budget). A "+
		"create whose latest attempt FAILED definitively is already settled — it goes in the email as failed — and "+
		"is not listed here; a create with an AMBIGUOUS outcome (*_ambiguous_transport / *_outcome_unknown, or a "+
		"transport error) stays listed because the record may exist: reconcile it with the server's list/search "+
		"tool before any re-create. If an action cannot be settled, call confirm_audit(success=false, "+
		"user_visible_summary=...) to abort; an aborted run may still send its single failure-summary email.",
		toolName, strings.Join(missing, "; "))
}

// checkPreBatchDuplicate refuses to treat a duplicate email as "already
// done" when that delivery PRE-dates the run's email-last work: while no
// summary email has been recorded, every stored fingerprint was sent before
// the work began (a successful send after it sets summaryEmailSent), so the
// reader got that message before the outcomes existed. The summary email
// stays owed and must be sent fresh. Callers must hold o.mu.
func (o *orchestrationState) checkPreBatchDuplicate(toolName string) (bool, string) {
	if !isSummaryEmailTool(toolName) || !o.emailLastAttempted || o.summaryEmailSent {
		return false, ""
	}
	log.Printf("Enforcement: Blocking %s — an identical payload was sent before this run's batch work; it is not the summary email", toolName)
	return true, fmt.Sprintf("BLOCKED: '%s' — an identical payload was sent before this run's batch work began, so it "+
		"cannot stand as the batch's summary email and does not discharge it. Send the summary email now, built "+
		"from the batch's final outcomes (each item and its result); it is necessarily a different message from "+
		"the earlier one.", toolName)
}

// noteTemplateEmailResult marks the summary email as sent when a
// send_template_email was DELIVERED after email-last work began — the caller
// passes the same check send_email's accounting uses (a clean transport and
// sendEmailSucceeded's provider 202), so a status failure such as
// {"status_code":500} over a clean transport never records a summary the
// reader did not get. send_email goes through recordToolResult's send
// accounting instead. Callers must hold o.mu.
func (o *orchestrationState) noteTemplateEmailResult(toolName string, succeeded bool) {
	if succeeded && !isEmailTool(toolName) && isSummaryEmailTool(toolName) {
		o.noteSummaryEmailSent()
	}
}

// noteSummaryEmailSent marks the summary email as sent when a send_email
// succeeds after email-last work began. Callers must hold o.mu.
func (o *orchestrationState) noteSummaryEmailSent() {
	if o.emailLastAttempted && !o.summaryEmailSent {
		o.summaryEmailSent = true
		log.Printf("Enforcement: the batch's summary email has been sent — further email-last calls are refused")
	}
}

// abortNotifyFinishNudge is checkFinishEnforcement's answer for an aborted
// run: refuse finish (at most maxAbortNotifyNudges times) while an email-last
// run's failure-summary email is still owed. The abort stays the run's
// verdict either way. Callers must hold o.mu.
func (o *orchestrationState) abortNotifyFinishNudge() (bool, []string) {
	if !o.emailLastAttempted || o.summaryEmailSent {
		return true, nil
	}
	if o.abortNotifyNudges >= maxAbortNotifyNudges {
		log.Printf("Enforcement: allowing finish after a terminal abort WITHOUT the failure-summary email — %d nudges exhausted",
			maxAbortNotifyNudges)
		return true, nil
	}
	o.abortNotifyNudges++
	log.Printf("Enforcement: terminal abort on an email-last run with NO summary email sent — rejecting finish (nudge %d/%d)",
		o.abortNotifyNudges, maxAbortNotifyNudges)
	return false, []string{fmt.Sprintf(
		"The abort is recorded, but this run attempted batch work and its reader has received NOTHING. Even a "+
			"fully-failed batch MUST produce its record: send the ONE summary email listing every item's outcome "+
			"(failed items marked failed) — send_email is unlocked for this single failure notification, with no "+
			"new audit. Then finish. (Reminder %d/%d; after that the run ends without a notification.)",
		o.abortNotifyNudges, maxAbortNotifyNudges)}
}

// finishOwed returns what finish enforcement still demands: the outstanding
// suffix units and their typed/legacy descriptions. Once the summary email has
// gone out, settled-failed creates are done — the email recorded them as
// failed, and no write may follow it. Callers must hold o.mu.
func (o *orchestrationState) finishOwed() (missing, outstanding []string) {
	missing = o.unexecutedCommitments()
	outstanding = o.outstandingCommitmentSummary()
	if !o.summaryEmailSent {
		return missing, outstanding
	}
	missing = subtractEach(missing, o.settledFailedSuffixes())
	var settled []string
	for _, c := range o.typedCommitments {
		if c.remaining > 0 && c.settledFailed {
			settled = append(settled, c.describe())
		}
	}
	return missing, subtractEach(outstanding, settled)
}

// settledFailedNote is the confirm_audit trailer clause naming outstanding
// units that already failed definitively, so the model does not read them as
// work to redo. "" when there are none. Callers must hold o.mu.
func (o *orchestrationState) settledFailedNote() string {
	var failed []string
	for _, c := range o.typedCommitments {
		if c.remaining > 0 && c.settledFailed {
			failed = append(failed, c.describe())
		}
	}
	if len(failed) == 0 {
		return ""
	}
	sort.Strings(failed)
	return fmt.Sprintf(" Of these, %s already failed definitively — settled for the summary email (a corrected "+
		"retry is optional; never blindly retry an ambiguous outcome).", strings.Join(failed, ", "))
}
