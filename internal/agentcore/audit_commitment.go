package agentcore

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
)

// Typed critical-action commitment binding (#715) + payload-level failure
// detection (#716), ported from the v1 engine (cutlass orchestration.go,
// hardened there after real incidents; fleet #715).
//
// The audit token is NOT a bearer token. A confirm_audit that supplies the
// typed `critical_actions` field binds each approval to the FULL
// server-qualified tool name (mcp_<server>[_<variant>]_<tool>) and, when the
// entry provides them, to specific record ids (`deal_id` / `deal_ids`) and a
// value-set digest (`values_digest`). While such a commitment ledger is
// active, a critical call that matches no outstanding commitment is blocked,
// and a successful call can only discharge the commitment it actually
// matches — one approval can never ride to (or be silently consumed by) a
// same-suffix tool on a DIFFERENT server, a different client variant, or a
// different record. The JSON field names (deal_id/deal_ids/values_digest and
// the call-side values_sha256) are the wire contract the client bundles'
// protocols and MCP servers already emit; fleet keeps them verbatim even
// though the engine treats them as opaque record identifiers.
//
// Legacy free-text declarations (`critical_actions_being_unblocked`) carry no
// server identity and no record ids, so they keep the pre-existing
// suffix-scoped semantics as a FALLBACK for untyped audits only — see
// registerCommittedActions in audit.go.

// typedCommitment is one commitment registered from a typed critical_actions
// entry. It binds the audit's approval to the FULL tool name — server and
// client-variant prefix included — and, optionally, to specific record id(s).
type typedCommitment struct {
	// tool is the FULL declared tool name (e.g. "mcp_myserver_create_record").
	// Always server-qualified: registerCommittedActionsTyped drops bare or
	// unrecognized declarations fail-closed, so a typed commitment never
	// wildcards across servers sharing a suffix.
	tool string
	// suffix is the resolved critical-tool suffix; always non-empty.
	suffix string
	// dealID is the optional single-record binding from the entry's deal_id
	// field, normalized. When set, only a call whose deal_id (or a sibling
	// record-id key, see callDealID) equals it may ride and discharge this
	// commitment.
	dealID string
	// dealIDs is the batch binding from the entry's deal_ids field (one
	// commitment unit per id). nil for single-action commitments.
	dealIDs map[string]bool
	// digest is the batch values_digest binding, lowercased ("" = none).
	digest string
	// discharged tracks which bound record ids have already discharged, so
	// piecewise or re-run execution cannot double-discharge one record while a
	// sibling approved record silently stays undone.
	discharged map[string]bool
	// remaining is the outstanding count: 1 for single actions,
	// len(dealIDs) for batch entries.
	remaining int
}

// nameMatches reports whether an executed toolName satisfies this
// commitment's tool binding: the exact declared full name, or a
// policy-approved substitute (criticalToolSubstitutes) on the SAME
// server/variant. Cross-server matching is refused: an approval for one
// server never matches another server's call even though both names end in
// the same critical suffix, and a base-server approval never matches a
// client-variant call.
func (c *typedCommitment) nameMatches(toolName string) bool {
	execSuffix := criticalSuffixFor(toolName)
	if execSuffix == "" {
		return false
	}
	if toolName == c.tool {
		return true
	}
	return substituteSatisfies(c.suffix, execSuffix) && sameToolServer(c.tool, toolName)
}

// allowsDeal reports whether this commitment's record binding covers a
// SINGLE-record call targeting dealID ("" = the call names no record). Fails
// closed: a record-bound commitment never covers a call whose record id is
// absent or different, and a batch-bound commitment covers only its approved,
// not-yet-discharged ids.
func (c *typedCommitment) allowsDeal(dealID string) bool {
	switch {
	case len(c.dealIDs) > 0:
		return dealID != "" && c.dealIDs[dealID] && !c.discharged[dealID]
	case c.dealID != "":
		return dealID != "" && dealID == c.dealID
	default:
		return true
	}
}

// hasDealBinding reports whether this commitment is bound to specific
// record(s) — a single deal_id or a deal_ids batch. Unbound commitments
// (creation tools with no record id) return false and are never superseded on
// re-audit: they carry no identity to match, and multi-record creation
// legitimately registers several unbound same-tool commitments in one audit.
func (c *typedCommitment) hasDealBinding() bool {
	return len(c.dealIDs) > 0 || c.dealID != ""
}

// sameDealSet reports whether two commitments target the identical record
// binding — the same batch id set, or the same single deal_id. Used to scope
// re-audit superseding strictly to same-family commitments so a re-audit can
// never silently retire an UNRELATED outstanding obligation.
func (c *typedCommitment) sameDealSet(other *typedCommitment) bool {
	if len(c.dealIDs) > 0 || len(other.dealIDs) > 0 {
		if len(c.dealIDs) != len(other.dealIDs) {
			return false
		}
		for id := range c.dealIDs {
			if !other.dealIDs[id] {
				return false
			}
		}
		return true
	}
	return c.dealID == other.dealID
}

// describe renders the commitment for enforcement messages and logs.
func (c *typedCommitment) describe() string {
	switch {
	case len(c.dealIDs) > 0:
		ids := make([]string, 0, len(c.dealIDs))
		for id := range c.dealIDs {
			if !c.discharged[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return fmt.Sprintf("%s (records %s)", c.tool, strings.Join(ids, ","))
	case c.dealID != "":
		return fmt.Sprintf("%s (record %s)", c.tool, c.dealID)
	default:
		return fmt.Sprintf("%s x%d", c.tool, c.remaining)
	}
}

// criticalSuffixFor returns the critical-tool suffix a real tool name matches
// (exact name, or "_"+suffix tail), or "" if none. Unlike matchCriticalSuffix
// (a substring scan of a free-text declaration), this is the precise suffix
// for an actual tool name and is what typed registration and batch binding key
// on. The LONGEST matching suffix wins — the bundle-supplied suffix list has
// no guaranteed order, and a specific name (e.g. create_prepared_deal) must
// pin over a shorter one it happens to end with (create_deal).
func criticalSuffixFor(toolName string) string {
	best := ""
	policyMu.RLock()
	defer policyMu.RUnlock()
	for _, suffix := range activeCriticalSuffixes {
		if toolName != suffix && !strings.HasSuffix(toolName, "_"+suffix) {
			continue
		}
		if len(suffix) > len(best) {
			best = suffix
		}
	}
	return best
}

// toolServerPrefix returns everything before toolName's critical suffix — the
// mcp_<server>[_<variant>]… portion that identifies WHICH MCP server (and
// client variant) the tool belongs to. "" when toolName has no critical suffix
// or IS the bare suffix (no server identity to compare).
func toolServerPrefix(toolName string) string {
	suffix := criticalSuffixFor(toolName)
	if suffix == "" || toolName == suffix {
		return ""
	}
	return strings.TrimSuffix(toolName, "_"+suffix)
}

// sameToolServer reports whether two full tool names live on the SAME MCP
// server/client-variant (identical prefixes before their critical suffixes).
// False when either carries no prefix — with no server identity we cannot
// prove they match, and callers fail closed.
func sameToolServer(a, b string) bool {
	pa, pb := toolServerPrefix(a), toolServerPrefix(b)
	return pa != "" && pa == pb
}

// unmarshalArgs decodes a tool call's JSON arguments with UseNumber so numeric
// record ids keep their exact digits. Plain json.Unmarshal decodes every
// number to float64, which silently rounds integers above 2^53 — a large
// numeric id would then fail to match its string binding and block a
// legitimate call. json.Number preserves the literal text.
func unmarshalArgs(rawInput string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(rawInput))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	return args, nil
}

// callRecordIDKeys are the argument keys a critical call's single-record
// identifier may arrive under. This is the wire contract of the cutlass-family
// bundle servers (each server names its primary identifier differently);
// deal_id is tried first, so a tool carrying several binds on it.
var callRecordIDKeys = []string{"deal_id", "internal_deal_id", "curated_id", "curated_deal_id", "rtd_id"}

// callDealID extracts the single record id a tool call targets, normalized;
// "" when the call names no record — or the args don't parse, which
// record-bound matching treats as ambiguous and fails closed on.
func callDealID(rawInput string) string {
	args, err := unmarshalArgs(rawInput)
	if err != nil {
		return ""
	}
	for _, key := range callRecordIDKeys {
		if v, ok := args[key]; ok && v != nil {
			if id := normalizeDealID(v); id != "" {
				return id
			}
		}
	}
	return ""
}

// batchDealIDs extracts the record ids a server-side batch call targets.
// Returns (normalized ids, true) when the raw tool input carries a non-empty
// "deal_ids" array; otherwise (nil, false) — a single-record call is not a
// batch and skips batch binding entirely.
func batchDealIDs(rawInput string) ([]string, bool) {
	args, err := unmarshalArgs(rawInput)
	if err != nil {
		return nil, false
	}
	list, ok := args["deal_ids"].([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, v := range list {
		ids = append(ids, normalizeDealID(v))
	}
	return ids, true
}

// normalizeDealID renders a record id to the canonical string used for
// approval matching. With UseNumber decoding (unmarshalArgs), JSON numbers
// arrive as json.Number and render via their exact literal text so large
// integer ids keep full precision; float64 is still handled for any legacy
// caller. An integral value renders without a decimal point so the call's
// 529786 and the audit's "529786" compare equal.
func normalizeDealID(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		// Fold an integral value to its canonical integer form so the three
		// wire representations of one id all match a single string binding:
		// exact large int "9007199254740993" (Int64 preserves full precision),
		// a float-formatted integral literal "529786.0", and the plain
		// "529786" all normalize identically. A genuinely fractional id (never
		// expected) falls through to its literal text.
		s := strings.TrimSpace(x.String())
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := x.Float64(); err == nil && f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return s
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", x))
	}
}

// valuesDigestArg returns the lowercased values_sha256 a tool call carries, or "".
func valuesDigestArg(rawInput string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return ""
	}
	if s, ok := args["values_sha256"].(string); ok {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return ""
}

// mcpReportedFailure reports whether an MCP tool result PAYLOAD signals
// failure (#716) — a JSON object with a top-level "success": false (the
// cutlass-family MCP convention) or, absent a "success" field, a non-empty
// top-level "error". A tool can "run" with no transport error yet report a
// failure this way (e.g. an HTTP 400 surfaced as {"success": false, ...});
// the enforcement layer must not treat that as a discharged commitment.
// Tools that don't follow the convention (non-JSON, no "success" field and no
// "error", or success:true) are NOT failures, so accounting is unchanged for
// them. The whole payload is parsed (no size cap): a size cap here would fail
// OPEN — a large failure payload would read as "not a failure" and wrongly
// discharge a commitment.
func mcpReportedFailure(resultText string) bool {
	s := strings.TrimSpace(resultText)
	if s == "" || s[0] != '{' {
		return false
	}
	var probe struct {
		Success *bool `json:"success"`
		// RawMessage (not string): the "error" field may be a string OR an
		// object (e.g. {"message": "HTTP 400"}); a string typing would fail
		// the whole unmarshal and mask a success:false payload.
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return false
	}
	if probe.Success != nil {
		return !*probe.Success
	}
	// No explicit "success" field: a top-level non-empty "error" is the
	// convention for a failed call (e.g. an email server returning
	// {"error": "..."} with no success key). Treat it as a failure so it does
	// not wrongly discharge a critical-action commitment or bypass the retry
	// budget.
	e := strings.TrimSpace(string(probe.Error))
	return e != "" && e != "null" && e != `""`
}

// dealOutcome is one record's result parsed from a batch tool's RESULT.
type dealOutcome struct {
	dealID  string
	success bool
}

// parseDealOutcomes extracts per-record outcomes from a batch-shaped tool
// RESULT — a JSON object carrying `results: [{deal_id, success}, ...]` (the
// shape the bundle servers' batch tools return). Returns (outcomes, true)
// only when the shape is present and every entry has both fields; otherwise
// (nil, false) so the caller falls back to the single-call discharge path
// used by tools that don't follow this convention.
func parseDealOutcomes(resultText string) ([]dealOutcome, bool) {
	s := strings.TrimSpace(resultText)
	if s == "" || s[0] != '{' || len(s) > 8*1024*1024 {
		return nil, false
	}
	var probe struct {
		Results []struct {
			DealID  any   `json:"deal_id"`
			Success *bool `json:"success"`
		} `json:"results"`
	}
	// UseNumber so a large numeric deal_id in results[] keeps full precision
	// and matches the string binding it was approved under; normalizeDealID
	// handles the resulting json.Number.
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil || len(probe.Results) == 0 {
		return nil, false
	}
	out := make([]dealOutcome, 0, len(probe.Results))
	for _, r := range probe.Results {
		if r.Success == nil || r.DealID == nil {
			return nil, false
		}
		out = append(out, dealOutcome{dealID: normalizeDealID(r.DealID), success: *r.Success})
	}
	return out, true
}

// resetBatchApprovals clears the batch record-id and values-digest approval
// state. Called on the first registering entry of every ACCEPTED audit
// envelope: batch approvals are envelope-scoped, never cumulative — but a
// rejected/zero-registering audit must leave them intact (see the lazy-reset
// comments in registerCommittedActionsTyped). Callers must hold o.mu.
func (o *orchestrationState) resetBatchApprovals() {
	o.approvedDealIDs = make(map[string]map[string]bool)
	o.approvedDigest = make(map[string]string)
}

// registerCommittedActionsTyped records commitments from the typed
// critical_actions field, honoring per-entry deal_ids (batch) and
// values_digest (binding). A batch entry commits its suffix once PER record id
// and records the approved id set + digest, so the matching batch call is
// bound to exactly those records (see checkBatchBinding). A non-batch entry
// registers a single commitment.
//
// Each entry is recorded in the typedCommitments ledger keyed by the FULL
// declared tool name (and the optional deal_id single-record binding), so
// execution and discharge are bound to the exact server/variant/record the
// audit approved — not just the bare suffix (#715).
//
// FAIL CLOSED: a typed entry MUST name a full server-qualified critical tool.
// A bare suffix (e.g. "create_deal") or fuzzy text is DROPPED — it carries no
// server identity, so honoring it would silently wildcard the binding across
// every server sharing that suffix. The bundle protocols always emit full
// tool names, so a bare/fuzzy typed entry is a malformed or injected
// declaration.
//
// Returns the number of commitment units registered (0 means the whole typed
// declaration was unusable — the caller MUST refuse to grant the audit token).
// Callers must hold o.mu.
func (o *orchestrationState) registerCommittedActionsTyped(actions []criticalActionStruct) int {
	if o.committedCriticalActions == nil {
		o.committedCriticalActions = make(map[string]int)
	}
	// A fresh audit envelope REPLACES the batch-approval state: only records
	// declared in THIS envelope are batch-approved, so a superseded prior
	// envelope's ids cannot ride a new envelope's digest. The reset is LAZY —
	// it fires on the first entry that actually registers, NOT at entry: a
	// typed audit that resolves to ZERO commitments is REJECTED by the caller
	// and must be a true no-op, or it would wipe the ACTIVE envelope's
	// approvals (auditConfirmed stays true, the prior commitment stays
	// outstanding) and then false-block the legitimately-approved in-flight
	// batch.
	//
	// Snapshot the commitments that existed BEFORE this audit so re-audit
	// superseding (below) only retires PRIOR-envelope commitments — never one
	// registered in this same call. This is what lets multi-record creation
	// register several unbound same-tool commitments at once without them
	// cannibalising each other.
	preExisting := len(o.typedCommitments)
	registered := 0
	for _, a := range actions {
		tool := strings.TrimSpace(a.Tool)
		if tool == "" {
			continue
		}
		// Only a full server-qualified tool name is accepted in the typed
		// field. criticalSuffixFor returns non-empty AND tool != suffix
		// exactly when tool carries an mcp_<server>[_<variant>]_ prefix ahead
		// of the suffix. A bare suffix (tool == suffix) or an unrecognized
		// name is dropped fail-closed.
		suffix := criticalSuffixFor(tool)
		if suffix == "" || tool == suffix {
			log.Printf("WARNING: dropping typed critical_action %q — not a full server-qualified MCP tool name "+
				"(a bare suffix would wildcard across every server sharing it). Copy the literal tool name "+
				"verbatim, e.g. \"mcp_myserver_create_record\". See protocols/self-audit.md.", tool)
			continue
		}
		// First entry that will register → this audit is ACCEPTED, so wipe the
		// prior envelope's batch approvals exactly once (lazy reset; a
		// rejected/zero-registering audit never reaches here).
		if registered == 0 {
			o.resetBatchApprovals()
		}
		// Fresh audit envelope for this suffix → clear any per-record
		// discharge ledger left over from a prior batch on the same suffix.
		delete(o.dischargedDeals, suffix)
		n := len(a.DealIDs)
		if n == 0 {
			n = 1
		}
		o.committedCriticalActions[suffix] += n
		registered += n
		if len(a.DealIDs) > 0 {
			if o.approvedDealIDs[suffix] == nil {
				o.approvedDealIDs[suffix] = make(map[string]bool)
			}
			for _, id := range a.DealIDs {
				o.approvedDealIDs[suffix][strings.TrimSpace(id)] = true
			}
			if a.ValuesDigest != "" {
				o.approvedDigest[suffix] = strings.ToLower(strings.TrimSpace(a.ValuesDigest))
			}
		}
		tc := &typedCommitment{
			tool:      tool, // always full server-qualified (fail-closed above)
			suffix:    suffix,
			remaining: n,
		}
		if len(a.DealIDs) > 0 {
			tc.dealIDs = make(map[string]bool, len(a.DealIDs))
			tc.discharged = make(map[string]bool, len(a.DealIDs))
			for _, id := range a.DealIDs {
				tc.dealIDs[strings.TrimSpace(id)] = true
			}
			tc.digest = strings.ToLower(strings.TrimSpace(a.ValuesDigest))
		} else if id := strings.TrimSpace(a.DealID); id != "" {
			tc.dealID = id
		}
		// Supersede any OUTSTANDING prior-envelope commitment with the SAME
		// full tool name AND SAME record-set. A re-audit that corrects the
		// values_digest (same tool + same records, different digest) registers
		// a FRESH commitment; without retiring the stale one they double-count
		// and the legit batch call discharges the stale commitment, leaving
		// the fresh one outstanding forever → the finished task wedges on a
		// phantom obligation with no path but abort. Retiring the stale
		// commitment transfers the obligation to the fresh one (resetting
		// discharge progress — correct, the audit re-approved) and removes its
		// outstanding units from the suffix count. Scoped strictly to same
		// tool + same record-set (record-bound only) so a re-audit can NEVER
		// silently retire an unrelated obligation and escape finish
		// enforcement.
		if tc.hasDealBinding() {
			for i := 0; i < preExisting; i++ {
				old := o.typedCommitments[i]
				if old.remaining <= 0 || old.tool != tc.tool || !old.hasDealBinding() || !old.sameDealSet(tc) {
					continue
				}
				if o.committedCriticalActions[old.suffix] >= old.remaining {
					o.committedCriticalActions[old.suffix] -= old.remaining
				} else {
					o.committedCriticalActions[old.suffix] = 0
				}
				log.Printf("Enforcement: superseded stale commitment %q on re-audit (same tool+record-set); "+
					"obligation transferred to the fresh approval", old.describe())
				old.remaining = 0
			}
		}
		o.typedCommitments = append(o.typedCommitments, tc)
		log.Printf("Enforcement: registered committed critical action %q (from %q); %d outstanding",
			suffix, tool, o.committedCriticalActions[suffix])
	}
	if len(actions) > 0 && registered == 0 {
		log.Printf("WARNING: confirm_audit supplied %d typed critical_actions but NONE resolved to a full "+
			"server-qualified critical tool — the audit will be REFUSED (fail closed). Use the literal MCP tool "+
			"name in each entry's `tool` field. See protocols/self-audit.md.", len(actions))
	}
	return registered
}

// markTypedExecuted discharges the best-matching outstanding typed commitment
// for an executed critical call, refusing cross-server / cross-variant /
// cross-record discharge. Returns true when a typed commitment was discharged.
// Callers must hold o.mu.
//
// Selection prefers the MOST specific match so a flexible commitment stays
// available for the work that actually needs it: exact-name + record-bound
// first, then exact-name unbound, then record-bound substitute matches, then
// the rest (same-server substitutes). Tie-break: on equal rank, prefer a
// commitment whose values_digest matches the executing call's callDigest,
// then the freshest (latest-registered) commitment — re-audit superseding
// already retires a stale same-family commitment at registration, so a live
// tie should not arise; this keeps discharge correct even if one ever did.
func (o *orchestrationState) markTypedExecuted(toolName, dealID, callDigest string) bool {
	var chosen *typedCommitment
	chosenIdx, best := -1, 0
	chosenDigestMatch := false
	for i, c := range o.typedCommitments {
		if c.remaining <= 0 || !c.nameMatches(toolName) || !c.allowsDeal(dealID) {
			continue
		}
		exact := c.tool == toolName
		bound := c.hasDealBinding()
		rank := 1
		switch {
		case exact && bound:
			rank = 4
		case exact:
			rank = 3
		case bound:
			rank = 2
		}
		digestMatch := c.digest != "" && c.digest == callDigest
		better := false
		switch {
		case rank != best:
			better = rank > best
		case digestMatch != chosenDigestMatch:
			better = digestMatch // digest-matching commitment wins the tie
		default:
			better = i > chosenIdx // else the freshest (latest) wins
		}
		if chosen == nil || better {
			best, chosen, chosenIdx, chosenDigestMatch = rank, c, i, digestMatch
		}
	}
	if chosen == nil {
		return false
	}
	if len(chosen.dealIDs) > 0 && dealID != "" {
		if chosen.discharged == nil {
			chosen.discharged = make(map[string]bool)
		}
		chosen.discharged[dealID] = true
	}
	chosen.remaining--
	if o.committedCriticalActions[chosen.suffix] > 0 {
		o.committedCriticalActions[chosen.suffix]--
	}
	log.Printf("Enforcement: typed commitment %q discharged via %q (record %q; %d left on this commitment, "+
		"%d outstanding on suffix %q)", chosen.describe(), toolName, dealID, chosen.remaining,
		o.committedCriticalActions[chosen.suffix], chosen.suffix)
	return true
}

// legacyHeadroomFor returns how many outstanding commitments for suffix are
// NOT owned by typed commitments — i.e. the portion registered through the
// legacy free-text path that suffix-level matching may still authorize or
// discharge. Callers must hold o.mu.
func (o *orchestrationState) legacyHeadroomFor(suffix string) int {
	n := o.committedCriticalActions[suffix]
	for _, c := range o.typedCommitments {
		if c.suffix == suffix && c.remaining > 0 {
			n -= c.remaining
		}
	}
	return n
}

// legacySuffixAuthorized reports whether suffix-level (free-text) commitments
// have headroom for a call with this suffix — directly, or as an approved
// substitute of an outstanding legacy commitment. Headroom excludes the
// portion owned by typed commitments, so a wrong-server call that failed
// typed matching can never ride (or later discharge) a typed commitment's
// count. Callers must hold o.mu.
func (o *orchestrationState) legacySuffixAuthorized(execSuffix string) bool {
	if execSuffix == "" {
		return false
	}
	if o.legacyHeadroomFor(execSuffix) > 0 {
		return true
	}
	for suffix := range o.committedCriticalActions {
		if substituteSatisfies(suffix, execSuffix) && o.legacyHeadroomFor(suffix) > 0 {
			return true
		}
	}
	return false
}

// outstandingCommitmentSummary renders every outstanding commitment for
// enforcement messages, sorted for determinism. Callers must hold o.mu.
func (o *orchestrationState) outstandingCommitmentSummary() []string {
	var out []string
	for _, c := range o.typedCommitments {
		if c.remaining > 0 {
			out = append(out, c.describe())
		}
	}
	for suffix := range o.committedCriticalActions {
		if headroom := o.legacyHeadroomFor(suffix); headroom > 0 {
			out = append(out, fmt.Sprintf("%s x%d", suffix, headroom))
		}
	}
	sort.Strings(out)
	return out
}

// checkBatchBinding gates a critical call carrying deal_ids (a server-side
// batch): it is allowed only over the records the audit approved — and, when
// the audit declared a value-set digest, only with that exact values_sha256.
// This is what stops one audit approval from silently authorizing a batch
// over records (or a value list) the audit never saw. Single-record calls
// carry no deal_ids and skip this entirely. Callers must hold o.mu and have
// verified auditConfirmed (the audit gate already blocked unaudited critical
// calls).
func (o *orchestrationState) checkBatchBinding(toolName, rawInput string) (bool, string) {
	dealIDs, isBatch := batchDealIDs(rawInput)
	if !isBatch {
		return false, ""
	}
	suffix := criticalSuffixFor(toolName)
	approved := o.approvedDealIDs[suffix]
	for _, id := range dealIDs {
		if !approved[id] {
			log.Printf("Enforcement: Blocking batch %s — record %q not in the approved set", toolName, id)
			return true, fmt.Sprintf("BLOCKED: batch '%s' targets record %q, which is not in the "+
				"confirm_audit-approved set for this tool. Re-run confirm_audit declaring every "+
				"deal_id in a typed critical_actions entry, or narrow the batch to the approved records.",
				toolName, id)
		}
	}
	if want := o.approvedDigest[suffix]; want != "" {
		if got := valuesDigestArg(rawInput); got != want {
			log.Printf("Enforcement: Blocking batch %s — values_sha256 %q != approved %q", toolName, got, want)
			return true, fmt.Sprintf("BLOCKED: batch '%s' values_sha256 does not match the "+
				"audit-approved digest. The approved value list differs from the one being applied — "+
				"re-audit with the correct values_digest.", toolName)
		}
	}
	return false, ""
}

// commitmentAuthorizes reports whether an outstanding commitment covers a
// critical call, failing closed when none does. Typed commitments require the
// executed tool's FULL name to match (or a same-server substitute) plus
// record-binding compatibility; legacy free-text commitments carry only a
// bare suffix — no server identity, no record ids — so they authorize by
// suffix (and approved substitutes) exactly as before, but never a
// server-side batch: batch approval requires typed deal_ids. Callers must
// hold o.mu.
func (o *orchestrationState) commitmentAuthorizes(toolName, rawInput string) (bool, string) {
	batchIDs, isBatch := batchDealIDs(rawInput)
	singleID := callDealID(rawInput)
	digest := valuesDigestArg(rawInput)

	for _, c := range o.typedCommitments {
		if c.remaining <= 0 || !c.nameMatches(toolName) {
			continue
		}
		if isBatch {
			if len(c.dealIDs) == 0 {
				continue // a batch call needs a batch-bound commitment
			}
			covered := true
			for _, id := range batchIDs {
				if !c.dealIDs[id] {
					covered = false
					break
				}
			}
			if !covered || (c.digest != "" && digest != c.digest) {
				continue
			}
			return true, ""
		}
		// A digest-bound BATCH commitment may ONLY be discharged by a
		// whole-batch call whose values_sha256 matches the approved digest.
		// A single-record call carries no batch digest, so authorizing it here
		// (via id membership alone) would let an approved record be mutated
		// with UN-verified values. Fail closed and steer the agent to the
		// batch call. This is deliberately conservative — no partial-digest
		// verification is attempted.
		if len(c.dealIDs) > 0 && c.digest != "" {
			if singleID != "" && c.dealIDs[singleID] && !c.discharged[singleID] {
				log.Printf("Enforcement: Blocking single-record %s (record %q) — record is bound to a "+
					"values_digest batch commitment; only the whole-batch call (deal_ids + matching "+
					"values_sha256) may discharge it", toolName, singleID)
				return false, fmt.Sprintf("BLOCKED: '%s' targeting record %q is part of a values_digest-bound "+
					"batch commitment. A single-record call cannot prove the approved values, so it is refused. "+
					"Discharge it by issuing the whole-batch call carrying deal_ids and the matching values_sha256.",
					toolName, singleID)
			}
			continue
		}
		if c.allowsDeal(singleID) {
			return true, ""
		}
	}

	if !isBatch && o.legacySuffixAuthorized(criticalSuffixFor(toolName)) {
		return true, ""
	}

	target := ""
	switch {
	case isBatch:
		target = fmt.Sprintf(" (deal_ids %v)", batchIDs)
	case singleID != "":
		target = fmt.Sprintf(" (record %s)", singleID)
	}
	log.Printf("Enforcement: Blocking %s%s — no outstanding audited commitment matches (#715)",
		toolName, target)
	return false, fmt.Sprintf("BLOCKED: '%s'%s matches no outstanding audited commitment. The confirm_audit "+
		"approval is bound to the exact tool name (server and client-variant prefix included) and its declared "+
		"record id(s) — a call on a different MCP server, a different client variant, or a different record "+
		"cannot ride this audit. Outstanding: %s. Execute the committed action(s) exactly as declared; if this "+
		"call is genuinely required, re-run confirm_audit declaring it in typed critical_actions (tool + deal_id), "+
		"or abort via confirm_audit(success=false, user_visible_summary=...).",
		toolName, target, strings.Join(o.outstandingCommitmentSummary(), "; "))
}
