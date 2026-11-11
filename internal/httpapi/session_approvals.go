package httpapi

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// SessionApprovalPolicy is a per-conversation pre-decision a user set during the
// current session — "approve/deny all <tool>" — so subsequent matching calls
// skip the approval card (#300).
type SessionApprovalPolicy struct {
	Mode      string // "approve" | "deny"
	Pattern   string // "" = all calls; "argName=glob" = scoped to a matching argument
	CreatedAt time.Time
}

// SessionApprovalRegistry is a process-level, in-memory index of per-conversation
// pre-approvals. Intentionally NOT persisted: pre-approvals are a UX convenience
// for the current session, not a durable authorization grant — they are lost on
// restart by design (a durable bypass belongs in the client-config allow-list).
type SessionApprovalRegistry struct {
	mu      sync.Mutex
	entries map[string][]SessionApprovalPolicy // convID + "\x00" + toolName → policies
}

// NewSessionApprovalRegistry builds an empty registry.
func NewSessionApprovalRegistry() *SessionApprovalRegistry {
	return &SessionApprovalRegistry{entries: map[string][]SessionApprovalPolicy{}}
}

func sessionKey(convID, userEmail, toolName string) string {
	return convID + "\x00" + strings.ToLower(strings.TrimSpace(userEmail)) + "\x00" + toolName
}

// Register records a pre-decision for (convID, toolName). Nil-safe.
func (r *SessionApprovalRegistry) Register(convID, toolName string, p SessionApprovalPolicy) {
	r.RegisterForUser(convID, "", toolName, p)
}

// RegisterForUser records a pre-decision scoped to (convID, userEmail, toolName)
// so one multiplayer participant's session decision never governs another
// participant's tool calls. Nil-safe.
func (r *SessionApprovalRegistry) RegisterForUser(convID, userEmail, toolName string, p SessionApprovalPolicy) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sessionKey(convID, userEmail, toolName)
	r.entries[k] = append(r.entries[k], p)
}

// Forget drops every pre-decision recorded for convID. Called when the
// conversation is deleted: the registry lives for the whole process and, with
// nothing ever removing entries, every "approve/deny all" click accumulated
// until restart. Nil-safe.
func (r *SessionApprovalRegistry) Forget(convID string) {
	if r == nil {
		return
	}
	prefix := convID + "\x00"
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.entries {
		if strings.HasPrefix(k, prefix) {
			delete(r.entries, k)
		}
	}
}

// ForgetUnauthorizedUsers drops every pre-decision recorded for convID whose
// userEmail is neither the conversation owner nor in allowed. Nil-safe.
func (r *SessionApprovalRegistry) ForgetUnauthorizedUsers(convID, ownerEmail string, allowed map[string]bool) {
	if r == nil || convID == "" {
		return
	}
	prefix := convID + "\x00"
	ownerKey := strings.ToLower(strings.TrimSpace(ownerEmail))
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.entries {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		idx := strings.IndexByte(rest, '\x00')
		if idx < 0 {
			continue
		}
		userKey := rest[:idx]
		if userKey == "" || userKey == ownerKey || allowed[userKey] {
			continue
		}
		delete(r.entries, k)
	}
}

// ForgetUser drops every pre-decision recorded for userEmail across all
// conversations. Nil-safe.
func (r *SessionApprovalRegistry) ForgetUser(userEmail string) {
	if r == nil {
		return
	}
	userKey := strings.ToLower(strings.TrimSpace(userEmail))
	if userKey == "" {
		return
	}
	needle := "\x00" + userKey + "\x00"
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.entries {
		if strings.Contains(k, needle) {
			delete(r.entries, k)
		}
	}
}

// Match returns the first policy covering (convID, toolName, rawInput), or
// ok=false when none applies. A "" pattern matches every call; "argName=glob"
// matches when the call's argName argument matches glob (filepath.Match), the
// name read ignoring case (policyMatches). DENY policies are evaluated before
// APPROVE so an explicit pre-denial always wins.
func (r *SessionApprovalRegistry) Match(convID, toolName, rawInput string) (SessionApprovalPolicy, bool) {
	return r.MatchForUser(convID, "", toolName, rawInput)
}

// MatchForUser returns the first policy covering (convID, userEmail, toolName, rawInput),
// falling back to user-agnostic policies registered with userEmail="" in unit tests.
func (r *SessionApprovalRegistry) MatchForUser(convID, userEmail, toolName, rawInput string) (SessionApprovalPolicy, bool) {
	if r == nil {
		return SessionApprovalPolicy{}, false
	}
	r.mu.Lock()
	policies := append([]SessionApprovalPolicy(nil), r.entries[sessionKey(convID, userEmail, toolName)]...)
	if strings.TrimSpace(userEmail) != "" {
		policies = append(policies, r.entries[sessionKey(convID, "", toolName)]...)
	}
	r.mu.Unlock()
	if len(policies) == 0 {
		return SessionApprovalPolicy{}, false
	}
	in := parseCallInput(rawInput)
	// Deny wins: scan denies first, then approves.
	for _, want := range [...]string{"deny", "approve"} {
		for _, p := range policies {
			if p.Mode == want && policyMatches(p, in, toolName) {
				return p, true
			}
		}
	}
	return SessionApprovalPolicy{}, false
}

// callArg is one top-level argument of a tool call, as written.
type callArg struct {
	name  string
	value any
}

// callInput is a tool call's top-level arguments in the order written, every
// duplicate kept (a map decode keeps only the last of an exact name). object
// is false when the input is not a valid JSON object. Such a call has no
// argument a pattern could name (a tool's decode refuses it or, for null,
// reads none), so only an all-calls policy covers it.
type callInput struct {
	object bool
	args   []callArg
}

// parseCallInput reads rawInput's top-level arguments. Numbers stay
// json.Number, so a number no float64 holds (1e400), which a tool's struct
// decode skips in a field it does not have, does not hide the other
// arguments.
func parseCallInput(rawInput string) callInput {
	if !json.Valid([]byte(rawInput)) {
		return callInput{}
	}
	dec := json.NewDecoder(strings.NewReader(rawInput))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return callInput{}
	}
	in := callInput{object: true}
	for dec.More() {
		tok, err := dec.Token()
		name, isName := tok.(string)
		if err != nil || !isName {
			return callInput{}
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return callInput{}
		}
		in.args = append(in.args, callArg{name: name, value: v})
	}
	return in
}

// policyMatches reports whether a policy applies to a call.
//
// A pattern names an argument the way the tools read it. A Go tool decodes
// its arguments with encoding/json, which matches a name ignoring case (with
// Unicode case folding, as strings.EqualFold does) and keeps the last
// spelling; a tool that reads names exactly, such as an MCP server, reads
// its own one of them. So the pattern is checked against every spelling of
// the name in the call: a deny matches when any of them matches, an approve
// only when all of them do. Whichever spelling the tool reads, a deny that
// names it applies, and an approve covers only a value it was given for.
//
// For the browser tools, whose actions have aliases ("live" runs
// "start_session"), an action pattern also matches the call's canonical
// action, and an alias in the pattern stands for its canonical action, so a
// session deny of start_session catches every spelling of it.
func policyMatches(p SessionApprovalPolicy, in callInput, toolName string) bool {
	if p.Pattern == "" {
		return true
	}
	eq := strings.IndexByte(p.Pattern, '=')
	if eq <= 0 {
		return false
	}
	argName, glob := p.Pattern[:eq], p.Pattern[eq+1:]
	values := patternValues(in, argName, toolName)
	if len(values) == 0 {
		return false
	}
	deny := p.Mode == "deny"
	for _, v := range values {
		matched := valueMatches(v, argName, glob, toolName)
		if deny && matched {
			return true
		}
		if !deny && !matched {
			return false
		}
	}
	return !deny
}

// patternValues are the values of every top-level argument whose name equals
// argName ignoring case, in the order written. A browser tool reads its
// action into a string field, which a null leaves as it is and which stays
// "" when nothing sets it (playwright_browser's default, run): for the
// action, a null is skipped, and a call with no other spelling counts as "".
func patternValues(in callInput, argName, toolName string) []any {
	if !in.object {
		return nil
	}
	browserAction := strings.EqualFold(argName, "action") && browserActionNormalizer(toolName) != nil
	var values []any
	for _, a := range in.args {
		if !strings.EqualFold(a.name, argName) || (browserAction && a.value == nil) {
			continue
		}
		values = append(values, a.value)
	}
	if browserAction && len(values) == 0 {
		return []any{""}
	}
	return values
}

// foldName is name with each rune replaced by the smallest rune of its case
// folding orbit, so two names are equal ignoring case (strings.EqualFold)
// exactly when their foldNames are equal. encoding/json folds names the same
// way.
func foldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		for {
			f := unicode.SimpleFold(r)
			if f <= r {
				r = f
				break
			}
			r = f
		}
		b.WriteRune(r)
	}
	return b.String()
}

// valueMatches reports whether one spelling's value matches glob. Only a
// string can: any other value fails an approve and never matches a deny.
func valueMatches(v any, argName, glob, toolName string) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	if matched, err := filepath.Match(glob, s); err == nil && matched {
		return true
	}
	if !strings.EqualFold(argName, "action") {
		return false
	}
	normalize := browserActionNormalizer(toolName)
	if normalize == nil {
		return false
	}
	canonical := normalize(s)
	if canonical == "" {
		return false
	}
	if c := normalize(glob); c != "" {
		glob = c
	}
	matched, err := filepath.Match(glob, canonical)
	return err == nil && matched
}

// browserActionNormalizer returns the action normalizer of a browser tool,
// or nil for any other tool.
func browserActionNormalizer(toolName string) func(string) string {
	switch toolName {
	case tools.PlaywrightBrowserToolName:
		return tools.NormalizePlaywrightBrowserAction
	case tools.ConnectorStudioToolName:
		return tools.NormalizeConnectorStudioAction
	}
	return nil
}
