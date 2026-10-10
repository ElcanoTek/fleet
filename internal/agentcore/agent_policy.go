package agentcore

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// AgentPolicy carries the client-bundle-configurable tool-behavior lists:
// which MCP tools are safe to dispatch in parallel, which tool-name suffixes are
// "critical" (require audit gating before execution), and the substitute-suffix
// map (which committed suffix may be discharged by which executed suffix).
//
// These lists are client-specific (e.g. ad-tech DSP deal-creation/execution
// tools); fleet itself ships NONE of them. The only critical suffixes fleet
// guarantees unconditionally are the generic outbound-email tools (see
// baseCriticalToolSuffixes); everything else is supplied by the client bundle
// via ConfigureAgentPolicy.
type AgentPolicy struct {
	// ParallelSafeTools are the fully-prefixed MCP tool names (mcp_<server>_<tool>)
	// safe to dispatch concurrently within a single assistant turn.
	ParallelSafeTools []string
	// CriticalToolSuffixes are the bare tool-name suffixes that require an audit
	// before execution (matched by suffix so "create_deal" matches a tool named
	// "<server>_create_deal"). The base suffixes are always merged in.
	CriticalToolSuffixes []string
	// CriticalToolSubstitutes maps a committed-tool suffix to the substitute
	// suffixes that may discharge its commitment (e.g. a high-level
	// execute_deal_from_prompt_inputs discharged by a lower-level create_deal).
	CriticalToolSubstitutes map[string][]string
	// CriticalToolTimeouts maps a bare tool-name suffix to a per-tool approval
	// default-deny window in seconds (#225). Matched by suffix exactly like
	// CriticalToolSuffixes ("send_email" matches "<server>_send_email"); the
	// longest matching suffix wins. It is the highest-priority layer of the
	// approval-timeout resolution chain (per-tool > per-conversation > global
	// FLEET_APPROVAL_TIMEOUT_SECONDS > hardcoded default). Empty = no per-tool
	// overrides, so every tool falls through to the per-conversation/global value.
	CriticalToolTimeouts map[string]int
	// CriticalToolModes maps a bare tool-name suffix to its approval MODE
	// (#1153): ApprovalModeApprove (default) or ApprovalModeNotify. Matched by
	// suffix exactly like CriticalToolSuffixes, longest match wins. Empty = every
	// critical tool blocks on a card, which is the behavior that existed before.
	CriticalToolModes map[string]string
	// CriticalToolUndoHints maps the same suffixes to a one-line, bundle-authored
	// statement of how to reverse the action, rendered on a notify record card.
	// Fleet does not know any client's undo verb and must not invent one.
	CriticalToolUndoHints map[string]string
	// CriticalToolAliases declares equivalence classes of critical suffixes
	// that are the SAME action under different names (#1604) — e.g. an inline
	// write and its staged-upload twin. Each key and the suffixes listed under
	// it form one class; entries that share a suffix merge. Unlike
	// CriticalToolSubstitutes (a one-way "this other action may stand in for
	// the committed one"), an alias is symmetric: a commitment declared on any
	// member is authorized and discharged by a call of any other member on the
	// same server/variant. Members must be critical suffixes; see
	// buildCriticalAliasClasses. Empty = exact-name binding only, as before.
	CriticalToolAliases map[string][]string
	// EmailLastToolSuffixes are critical suffixes whose run reports by ONE
	// summary email sent as its LAST outward step — a batch of record writes
	// followed by the sheet that lists every outcome (see audit_email_last.go).
	// Once a run attempts or declares one: send_email is refused while a
	// commitment on one is unsettled, every such call is refused after the
	// summary email has gone out, and an abort still owes (and is allowed) the
	// single failure-summary email. Members must be critical, non-email
	// suffixes. Empty = none of this applies, as before.
	EmailLastToolSuffixes []string
	// SettleableCreateToolSuffixes are the record-CREATE suffixes whose
	// UNBOUND commitment is settled by a definitive failure (the tool ran and
	// reported success=false with no ambiguous-outcome marker): it no longer
	// holds the summary email back, and no longer blocks finish once that
	// email has gone out. Each member is implicitly an email-last suffix too.
	// Empty = a failed create keeps its commitment open, as before.
	SettleableCreateToolSuffixes []string
	// BatchSecondsPerDeal maps a manifest MCP server name to its per-record
	// budget, in seconds, for a deal_ids batch call (the bundle's
	// mcp_servers[].batch_seconds_per_deal). A registered named-account
	// variant <server>_<account> resolves to its base server's entry through
	// the one server-name keying rule (longestServerKey). Unlisted servers get
	// batchToolCallTimeoutPerDeal. Non-positive values are ignored.
	BatchSecondsPerDeal map[string]int
	// ApprovedCallTimeoutSeconds maps a manifest MCP server name to its budget,
	// in seconds, for one call a person approved on a chat approval card (the
	// bundle's mcp_servers[].approved_call_timeout_seconds). Declaring it opts
	// the server into ApprovedCallBudget's scaling; an unlisted server keeps
	// DefaultApprovedCallBudget. Variants resolve to their base server's entry
	// the same way as BatchSecondsPerDeal. Non-positive values are ignored.
	ApprovedCallTimeoutSeconds map[string]int
	// CriticalToolNoSessionApproval lists critical suffixes whose approval
	// cards never offer "apply my choice to all calls in this chat" (#300):
	// every call needs its own decision. Matched by suffix exactly like
	// CriticalToolSuffixes. The web hides the checkbox, the approve POST
	// refuses a non-"once" scope, and Stage ignores any session policy for a
	// matching tool. Members must be critical suffixes; see
	// NoSessionApprovalProblems. Empty = every card keeps apply-all, as before.
	CriticalToolNoSessionApproval []string
	// CriticalToolCardDescribers maps a critical suffix to a read-only
	// "describer" tool suffix on the same server. When a matching call is
	// staged for approval, the stager calls the describer with the same
	// arguments (bounded, no retries) and renders its structured result as a
	// readable card, falling back to the generic arguments card on any
	// failure. The describer must be parallel-safe and must NOT be critical:
	// it runs outside the audit gate, so an entry whose describer is critical
	// is dropped (see CardDescriberProblems). Empty = every card is generic.
	CriticalToolCardDescribers map[string]string
	// CriticalToolResume lists critical suffixes whose approval cards, once
	// they reach a terminal outcome (approved with its result recorded,
	// declined, or timed out), start one new agent turn in the conversation
	// so the agent can verify the outcome and carry on without the user
	// typing (docs/RESUME-AFTER-APPROVAL.md). Matched by suffix exactly like
	// CriticalToolSuffixes. Members must be critical suffixes; see
	// ResumeAfterApprovalProblems. Empty = no turn ever starts on its own, as
	// before.
	CriticalToolResume []string
	// CriticalToolResumeMaxPerHour caps the automatic resumes one conversation
	// may start in any rolling hour. 0 = DefaultResumeMaxPerHour; the
	// accepted range is 1..MaxResumeMaxPerHour.
	CriticalToolResumeMaxPerHour int
}

// Approval modes a bundle may declare per critical tool (#1153).
const (
	// ApprovalModeApprove blocks the call on a card until a human decides. The
	// default, and what every critical tool did before modes existed.
	ApprovalModeApprove = "approve"
	// ApprovalModeNotify executes the call immediately and posts a card
	// RECORDING what happened. Only legitimate when undoing the action is cheap
	// and complete — that is the entire argument for it, and the bundle has to
	// back it up with an undo hint the card can show.
	ApprovalModeNotify = "notify"
)

// baseCriticalToolSuffixes are ALWAYS critical regardless of the configured
// bundle — generic destructive / external-effect tools fleet ships behavior for.
// These are deliberately client-agnostic (outbound email).
var baseCriticalToolSuffixes = []string{
	sendEmailToolSuffix, // "send_email"
	"send_template_email",
}

var (
	policyMu sync.RWMutex

	// activeParallelSafe is the set of fully-prefixed MCP tool names safe to run
	// concurrently. Empty by default (generic fleet runs nothing in parallel
	// until a bundle opts tools in).
	activeParallelSafe = map[string]bool{}

	// activeCriticalSuffixes is the ordered list of critical tool-name suffixes.
	// Defaults to the base (generic) suffixes only. Order is not load-bearing for
	// correctness: matchCriticalSuffix selects the longest match by length, and
	// isCriticalTool tests membership, not order.
	activeCriticalSuffixes = append([]string(nil), baseCriticalToolSuffixes...)

	// activeCriticalSubstitutes maps committed suffix -> allowed executed
	// substitutes. Empty by default.
	activeCriticalSubstitutes = map[string][]string{}

	// activeCriticalTimeouts maps a critical-tool suffix -> per-tool approval
	// default-deny window in seconds (#225). Empty by default (no per-tool
	// overrides); ApprovalTimeoutForTool returns 0 then, and callers fall back
	// to the per-conversation / global timeout.
	activeCriticalTimeouts = map[string]int{}

	// activeCriticalModes / activeCriticalUndoHints back the per-tool approval
	// mode (#1153). Empty by default: every critical tool blocks on a card.
	activeCriticalModes     = map[string]string{}
	activeCriticalUndoHints = map[string]string{}

	// activeCriticalAliasClass maps each aliased critical suffix to its alias
	// class key (#1604). Empty by default: a suffix with no entry is aliased to
	// nothing, and every lookup falls back to the suffix itself.
	activeCriticalAliasClass = map[string]string{}

	// activeEmailLastSuffixes / activeSettleableCreateSuffixes back the
	// email-last batch rules (audit_email_last.go). Empty by default: no
	// suffix is email-last and no failure settles anything.
	activeEmailLastSuffixes        = map[string]bool{}
	activeSettleableCreateSuffixes = map[string]bool{}
	// activeBatchPerDeal maps a manifest server name to its declared per-record
	// deal_ids batch budget. Empty by default: every server gets
	// batchToolCallTimeoutPerDeal.
	activeBatchPerDeal = map[string]time.Duration{}
	// activeApprovedCallTimeout maps a manifest server name to its declared
	// approved-card call budget. Empty by default: every approved call gets
	// DefaultApprovedCallBudget.
	activeApprovedCallTimeout = map[string]time.Duration{}
	// activeNoSessionApproval is the set of critical suffixes whose cards take
	// one decision per call (no apply-all). Empty by default.
	activeNoSessionApproval = map[string]bool{}
	// activeCardDescribers maps a critical suffix to its describer suffix.
	// Empty by default: no card is described.
	activeCardDescribers = map[string]string{}
	// activeResume is the set of critical suffixes whose resolved cards start
	// a resume turn, and activeResumeMaxPerHour its per-conversation cap.
	// Empty by default: no turn starts without user input.
	activeResume           = map[string]bool{}
	activeResumeMaxPerHour = DefaultResumeMaxPerHour
)

// nonReversibleSuffixes can never be declared `notify`, whatever a bundle says.
// The whole case for executing without asking is "we can always roll it back",
// and for outbound email that is simply false — there is no undo, and the
// approval card IS the review step. A bundle that tries is logged and pinned
// back to `approve` rather than quietly honored.
var nonReversibleSuffixes = map[string]bool{
	sendEmailToolSuffix:   true,
	"send_template_email": true,
}

// ConfigureAgentPolicy installs the client bundle's tool-behavior policy. Call
// once at startup (cmd/fleet) before any turn runs. The base critical suffixes
// are always merged in (deduped, base-first). Safe to call with a zero
// AgentPolicy, which yields the generic defaults (no parallel tools, only the
// base critical email suffixes, no substitutes). Idempotent: each call fully
// replaces the previously installed policy.
func ConfigureAgentPolicy(p AgentPolicy) {
	policyMu.Lock()
	defer policyMu.Unlock()

	parallel := make(map[string]bool, len(p.ParallelSafeTools))
	for _, t := range p.ParallelSafeTools {
		if t != "" {
			parallel[t] = true
		}
	}
	activeParallelSafe = parallel

	seen := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	critical := make([]string, 0, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		if s != "" && !seen[s] {
			seen[s] = true
			critical = append(critical, s)
		}
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" && !seen[s] {
			seen[s] = true
			critical = append(critical, s)
		}
	}
	activeCriticalSuffixes = critical
	classes, aliasProblems := buildCriticalAliasClasses(p.CriticalToolAliases, seen)
	for _, problem := range aliasProblems {
		log.Printf("agent_policy: %s", problem)
	}
	activeCriticalAliasClass = classes
	emailLast, settleable, emailLastProblems := buildEmailLastSets(p, seen)
	for _, problem := range emailLastProblems {
		log.Printf("agent_policy: %s", problem)
	}

	subs := make(map[string][]string, len(p.CriticalToolSubstitutes))
	for k, v := range p.CriticalToolSubstitutes {
		subs[k] = append([]string(nil), v...)
	}
	activeCriticalSubstitutes = subs

	closeEmailLastOverEquivalents(emailLast, classes, subs, seen)
	activeEmailLastSuffixes, activeSettleableCreateSuffixes = emailLast, settleable

	timeouts := make(map[string]int, len(p.CriticalToolTimeouts))
	for k, v := range p.CriticalToolTimeouts {
		if k != "" && v > 0 {
			timeouts[k] = v
		}
	}
	activeCriticalTimeouts = timeouts

	modes := make(map[string]string, len(p.CriticalToolModes))
	for k, v := range p.CriticalToolModes {
		k = strings.TrimSpace(k)
		v = strings.ToLower(strings.TrimSpace(v))
		if k == "" {
			continue
		}
		if v != ApprovalModeNotify && v != ApprovalModeApprove {
			log.Printf("agent_policy: ignoring unknown approval mode %q for %q (want %q or %q)", v, k, ApprovalModeApprove, ApprovalModeNotify)
			continue
		}
		if v == ApprovalModeNotify && nonReversibleSuffixes[k] {
			log.Printf("agent_policy: refusing mode %q for %q — a sent message cannot be undone, so its approval card is the review step; pinned to %q", ApprovalModeNotify, k, ApprovalModeApprove)
			continue
		}
		modes[k] = v
	}
	activeCriticalModes = modes

	hints := make(map[string]string, len(p.CriticalToolUndoHints))
	for k, v := range p.CriticalToolUndoHints {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			hints[k] = v
		}
	}
	activeCriticalUndoHints = hints

	perDeal := make(map[string]time.Duration, len(p.BatchSecondsPerDeal))
	for server, secs := range p.BatchSecondsPerDeal {
		if server = strings.TrimSpace(server); server != "" && secs > 0 {
			perDeal[server] = time.Duration(secs) * time.Second
		}
	}
	activeBatchPerDeal = perDeal

	approved := make(map[string]time.Duration, len(p.ApprovedCallTimeoutSeconds))
	for server, secs := range p.ApprovedCallTimeoutSeconds {
		if server = strings.TrimSpace(server); server != "" && secs > 0 {
			approved[server] = time.Duration(secs) * time.Second
		}
	}
	activeApprovedCallTimeout = approved

	noSession, noSessionProblems := buildNoSessionApprovalSet(p.CriticalToolNoSessionApproval, seen)
	for _, problem := range noSessionProblems {
		log.Printf("agent_policy: %s", problem)
	}
	activeNoSessionApproval = noSession

	describers, describerProblems := buildCardDescribers(p.CriticalToolCardDescribers, critical, parallel)
	for _, problem := range describerProblems {
		log.Printf("agent_policy: %s", problem)
	}
	activeCardDescribers = describers

	resume, resumeMax, resumeProblems := buildResumeSet(p, seen)
	for _, problem := range resumeProblems {
		log.Printf("agent_policy: %s", problem)
	}
	activeResume, activeResumeMaxPerHour = resume, resumeMax
}

// suffixMatches reports whether a tool (or a suffix standing for one) is
// selected by suffix under the critical_tools rule: equal, or ending in
// "_<suffix>".
func suffixMatches(name, suffix string) bool {
	return name == suffix || strings.HasSuffix(name, "_"+suffix)
}

// buildCardDescribers resolves critical_tool_card_describers. An entry is
// kept only when its key is a critical suffix (no card is ever staged for any
// other tool), and its describer is not critical under the merged suffix list
// (a describer runs outside the audit gate, so a critical one would be a write
// with no approval) and matches at least one parallel_safe_tools entry (the
// bundle's declaration that the tool is a read). Every dropped entry is a
// problem for the caller to log (boot) or report (validate-config).
func buildCardDescribers(describers map[string]string, critical []string, parallel map[string]bool) (map[string]string, []string) {
	out := make(map[string]string, len(describers))
	var problems []string
	keys := make([]string, 0, len(describers))
	for k := range describers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	isCritical := func(name string) bool {
		for _, s := range critical {
			if s != "" && suffixMatches(name, s) {
				return true
			}
		}
		return false
	}
	for _, k := range keys {
		key, desc := strings.TrimSpace(k), strings.TrimSpace(describers[k])
		switch {
		case key == "" || desc == "":
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_card_describers entry %q: both the critical suffix and the describer suffix are required", k))
			continue
		case !containsString(critical, key):
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_card_describers entry %q: it is not in critical_tools, so no approval card is ever staged for it", key))
			continue
		case isCritical(desc):
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_card_describers entry %q: describer %q is a critical tool, and a describer runs without approval", key, desc))
			continue
		}
		safe := false
		for name := range parallel {
			if suffixMatches(name, desc) {
				safe = true
				break
			}
		}
		if !safe {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_card_describers entry %q: describer %q is not in parallel_safe_tools, which is how a bundle declares a read-only tool", key, desc))
			continue
		}
		out[key] = desc
	}
	return out, problems
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// CardDescriberProblems is the preflight face of buildCardDescribers, for
// `fleet validate-config`: the entries ConfigureAgentPolicy would drop.
func CardDescriberProblems(p AgentPolicy) []string {
	critical := append([]string(nil), baseCriticalToolSuffixes...)
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical = append(critical, s)
		}
	}
	parallel := make(map[string]bool, len(p.ParallelSafeTools))
	for _, t := range p.ParallelSafeTools {
		if t != "" {
			parallel[t] = true
		}
	}
	_, problems := buildCardDescribers(p.CriticalToolCardDescribers, critical, parallel)
	return problems
}

// CardDescriberFor returns the describer suffix declared for toolName (the
// longest matching critical suffix wins, like ApprovalModeForTool), or ""
// when none is declared.
func CardDescriberFor(toolName string) string {
	policyMu.RLock()
	defer policyMu.RUnlock()
	best, bestLen := "", -1
	for suffix, desc := range activeCardDescribers {
		if suffixMatches(toolName, suffix) && len(suffix) > bestLen {
			best, bestLen = desc, len(suffix)
		}
	}
	return best
}

// IsParallelSafeTool reports whether a fully-prefixed MCP tool name is listed
// in the bundle's parallel_safe_tools. Exported for the approval stager, which
// re-checks a resolved describer before calling it.
func IsParallelSafeTool(name string) bool { return isParallelSafeTool(name) }

// buildNoSessionApprovalSet resolves critical_tool_no_session_approval into a
// suffix set. A member that is not a critical suffix is reported: no card is
// ever staged for a tool the audit gate does not see, so such an entry is
// inert, and most likely a typo that leaves the intended tool with apply-all.
// It is still installed — refusing a session scope is the safe direction, and
// a later critical_tools fix then takes effect without touching this list.
func buildNoSessionApprovalSet(members []string, critical map[string]bool) (map[string]bool, []string) {
	set := make(map[string]bool, len(members))
	var problems []string
	for _, s := range members {
		s = strings.TrimSpace(s)
		if s == "" || set[s] {
			continue
		}
		if !critical[s] {
			problems = append(problems, fmt.Sprintf("critical_tool_no_session_approval member %q is not in critical_tools, so no approval card is ever staged for it and the entry does nothing", s))
		}
		set[s] = true
	}
	return set, problems
}

// NoSessionApprovalProblems is the preflight face of
// buildNoSessionApprovalSet, for `fleet validate-config`: members of
// p.CriticalToolNoSessionApproval that are not critical suffixes (the same
// merged list the gate uses).
func NoSessionApprovalProblems(p AgentPolicy) []string {
	critical := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		critical[s] = true
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical[s] = true
		}
	}
	_, problems := buildNoSessionApprovalSet(p.CriticalToolNoSessionApproval, critical)
	return problems
}

// SessionApprovalAllowed reports whether a card for toolName may offer, and
// honor, a decision that applies to every later call of the tool in the
// conversation (#300). False when the bundle lists a matching suffix in
// critical_tool_no_session_approval; matching mirrors isCriticalTool (the
// tool name equals the suffix or ends with "_<suffix>").
func SessionApprovalAllowed(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for suffix := range activeNoSessionApproval {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			return false
		}
	}
	return true
}

// batchPerDealFor returns the per-record deal_ids batch budget for a
// REGISTERED server name: the bundle-declared value of the server (or, for a
// named-account variant without its own entry, of its base server), else
// batchToolCallTimeoutPerDeal.
func batchPerDealFor(registered string) time.Duration {
	policyMu.RLock()
	defer policyMu.RUnlock()
	if key, ok := longestServerKey(registered, activeBatchPerDeal, true, nil); ok {
		return activeBatchPerDeal[key]
	}
	return batchToolCallTimeoutPerDeal
}

// approvedCallTimeoutFor returns the bundle-declared approved-card call budget
// for a REGISTERED server name (the server's own entry, or, for a
// named-account variant without one, its base server's), and whether one is
// declared at all.
func approvedCallTimeoutFor(registered string) (time.Duration, bool) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	if key, ok := longestServerKey(registered, activeApprovedCallTimeout, true, nil); ok {
		return activeApprovedCallTimeout[key], true
	}
	return 0, false
}

// CriticalToolAliasProblems reports what ConfigureAgentPolicy would ignore in
// p.CriticalToolAliases: members that are not critical suffixes (checked
// against the SAME merged list the gate uses — base suffixes plus
// p.CriticalToolSuffixes) and entries left with fewer than two members. It is
// the preflight face of the boot-time validation: `fleet validate-config`
// reports these, because at boot a problem is one log line and a typo'd member
// silently leaves the exact wrong-variant wedge the alias was meant to end.
func CriticalToolAliasProblems(p AgentPolicy) []string {
	critical := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		critical[s] = true
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical[s] = true
		}
	}
	_, problems := buildCriticalAliasClasses(p.CriticalToolAliases, critical)
	return problems
}

// EmailLastPolicyProblems reports what ConfigureAgentPolicy would ignore in
// p.EmailLastToolSuffixes / p.SettleableCreateToolSuffixes: a member that is
// not a critical suffix (the same merged list the gate uses) or is an email
// suffix. The preflight face of buildEmailLastSets, like
// CriticalToolAliasProblems: at boot each is one log line, and a misspelled
// member silently leaves that tool outside the email-last rules.
func EmailLastPolicyProblems(p AgentPolicy) []string {
	critical := make(map[string]bool, len(baseCriticalToolSuffixes)+len(p.CriticalToolSuffixes))
	for _, s := range baseCriticalToolSuffixes {
		critical[s] = true
	}
	for _, s := range p.CriticalToolSuffixes {
		if s != "" {
			critical[s] = true
		}
	}
	_, _, problems := buildEmailLastSets(p, critical)
	return problems
}

// buildEmailLastSets resolves the bundle's email_last_tools and
// settleable_create_tools into suffix sets. Every member must be a critical
// suffix (the gate never sees any other tool, so a settle or an email-last
// rule on one would be inert or a typo) and must not be an email suffix (the
// summary email cannot wait on itself). Settleable members join the email-last
// set: settling exists only to release the summary email. Dropped members are
// returned as problems for the caller to log (boot) or report (preflight).
func buildEmailLastSets(p AgentPolicy, critical map[string]bool) (emailLast, settleable map[string]bool, problems []string) {
	emailLast, settleable = map[string]bool{}, map[string]bool{}
	admit := func(field, s string) bool {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			return false
		case !critical[s]:
			problems = append(problems, fmt.Sprintf("ignoring %s member %q — it is not in critical_tools, so the audit gate never sees that tool", field, s))
			return false
		case isSummaryEmailTool(s):
			problems = append(problems, fmt.Sprintf("ignoring %s member %q — an email tool cannot be held behind the summary email", field, s))
			return false
		}
		return true
	}
	for _, s := range p.EmailLastToolSuffixes {
		if admit("email_last_tools", s) {
			emailLast[strings.TrimSpace(s)] = true
		}
	}
	for _, s := range p.SettleableCreateToolSuffixes {
		if admit("settleable_create_tools", s) {
			s = strings.TrimSpace(s)
			settleable[s], emailLast[s] = true, true
		}
	}
	return emailLast, settleable, problems
}

// closeEmailLastOverEquivalents widens the email-last set to every critical
// suffix that can carry the same logical write as a member: its
// critical_tool_aliases twins (either direction) and the
// critical_tool_substitutes targets listed under it — exactly the executed
// names typedCommitment.nameMatches lets authorize or discharge a commitment
// on a member. Without this, a write after the summary email could run under
// the twin's name, and a commitment declared on the twin would not hold the
// email back. Iterates to a fixpoint (a substitute's own alias class joins
// too); email suffixes and non-critical names never join. Settleable
// membership is NOT widened: a settle only releases the email, so leaving an
// equivalent unsettleable is the safe direction.
func closeEmailLastOverEquivalents(emailLast map[string]bool, aliasClass map[string]string,
	subs map[string][]string, critical map[string]bool) {
	join := func(s, via string) bool {
		s = strings.TrimSpace(s)
		if s == "" || emailLast[s] || !critical[s] || isSummaryEmailTool(s) {
			return false
		}
		emailLast[s] = true
		log.Printf("agent_policy: %q is email-last as an equivalent of %q (critical_tool_aliases / critical_tool_substitutes)", s, via)
		return true
	}
	for changed := true; changed; {
		changed = false
		for member := range emailLast {
			if class, ok := aliasClass[member]; ok {
				for s, c := range aliasClass {
					if c == class && join(s, member) {
						changed = true
					}
				}
			}
			for _, s := range subs[member] {
				if join(s, member) {
					changed = true
				}
			}
		}
	}
}

// buildCriticalAliasClasses turns the bundle's critical_tool_aliases into
// equivalence classes (#1604): an entry's key and the suffixes listed under it
// are one class, and entries sharing a suffix merge, so the relation is
// symmetric and transitive whichever way the manifest spells it. Every member
// must be a critical suffix: an alias of a tool the audit gate never sees would
// be a discharge path around the gate, and a misspelled one would be silently
// inert. Such members are dropped, and an entry left with fewer than two
// members is dropped whole; each is returned as a problem for the caller to
// log (boot) or report (validate-config). Returns suffix -> class key, the
// class's lexicographically smallest member, so the key does not depend on the
// order the YAML map decoded in.
func buildCriticalAliasClasses(aliases map[string][]string, critical map[string]bool) (map[string]string, []string) {
	var problems []string
	parent := map[string]string{}
	find := func(s string) string {
		for parent[s] != s {
			parent[s] = parent[parent[s]]
			s = parent[s]
		}
		return s
	}
	keys := make([]string, 0, len(aliases))
	for k := range aliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var members []string
		inEntry := map[string]bool{}
		for _, s := range append([]string{k}, aliases[k]...) {
			s = strings.TrimSpace(s)
			if s == "" || inEntry[s] {
				continue
			}
			inEntry[s] = true
			if !critical[s] {
				problems = append(problems, fmt.Sprintf("ignoring critical_tool_aliases member %q (entry %q) — it is not in critical_tools, so the audit gate never sees that tool and it may not discharge a declared commitment", s, k))
				continue
			}
			members = append(members, s)
		}
		if len(members) < 2 {
			problems = append(problems, fmt.Sprintf("ignoring critical_tool_aliases entry %q — fewer than two critical suffixes remain in it", k))
			continue
		}
		for _, m := range members {
			if _, ok := parent[m]; !ok {
				parent[m] = m
			}
		}
		for _, m := range members[1:] {
			ra, rb := find(members[0]), find(m)
			if rb < ra {
				ra, rb = rb, ra
			}
			parent[rb] = ra // the smaller root wins, so a class's key is its smallest member
		}
	}
	classes := make(map[string]string, len(parent))
	for s := range parent {
		classes[s] = find(s)
	}
	return classes, problems
}

// ApprovalModeForTool returns the bundle-declared approval mode for toolName and
// the one-line undo hint to render alongside it (#1153). Matching mirrors
// ApprovalTimeoutForTool — longest matching suffix wins — and an undeclared tool
// gets ApprovalModeApprove, so adding modes changes nothing for any bundle that
// does not use them.
func ApprovalModeForTool(toolName string) (mode, undoHint string) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	mode = ApprovalModeApprove
	bestLen := -1
	for suffix, m := range activeCriticalModes {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				mode = m
			}
		}
	}
	hintLen := -1
	for suffix, h := range activeCriticalUndoHints {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > hintLen {
				hintLen = len(suffix)
				undoHint = h
			}
		}
	}
	return mode, undoHint
}

// ApprovalTimeoutForTool returns the per-tool approval default-deny window (in
// seconds) configured for toolName via the bundle's
// agent_policy.critical_tool_timeouts, or 0 if none applies (#225). Matching
// mirrors isCriticalTool — a suffix matches when the tool name equals it or ends
// with "_<suffix>" — and the LONGEST matching suffix wins so a specific
// "execute_deal" pins over a generic "deal". 0 tells the caller to fall back to
// the per-conversation / global timeout.
func ApprovalTimeoutForTool(toolName string) int {
	policyMu.RLock()
	defer policyMu.RUnlock()
	bestLen := -1
	best := 0
	for suffix, secs := range activeCriticalTimeouts {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				best = secs
			}
		}
	}
	return best
}

// isParallelSafeTool reports whether the fully-prefixed MCP tool name is safe to
// dispatch concurrently under the active policy.
func isParallelSafeTool(name string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	return activeParallelSafe[name]
}
