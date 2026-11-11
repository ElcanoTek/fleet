package httpapi

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
// matches when the call's args[argName] matches glob (filepath.Match). DENY
// policies are evaluated before APPROVE so an explicit pre-denial always wins.
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
	var args map[string]any
	_ = json.Unmarshal([]byte(rawInput), &args)
	if args != nil && browserActionNormalizer(toolName) != nil {
		if action, ok := browserCallAction(rawInput); ok {
			args["action"] = action
		}
	}
	// Deny wins: scan denies first, then approves.
	for _, want := range [...]string{"deny", "approve"} {
		for _, p := range policies {
			if p.Mode == want && policyMatches(p, args, toolName) {
				return p, true
			}
		}
	}
	return SessionApprovalPolicy{}, false
}

// policyMatches reports whether a policy applies to the given parsed args.
// For the browser tools, whose actions have aliases ("live" runs
// "start_session"), an action pattern also matches the call's canonical
// action, and an alias in the pattern stands for its canonical action, so a
// session deny of start_session catches every spelling of it.
func policyMatches(p SessionApprovalPolicy, args map[string]any, toolName string) bool {
	if p.Pattern == "" {
		return true
	}
	eq := strings.IndexByte(p.Pattern, '=')
	if eq <= 0 {
		return false
	}
	argName, glob := p.Pattern[:eq], p.Pattern[eq+1:]
	val, ok := args[argName]
	if !ok {
		return false
	}
	s, ok := val.(string)
	if !ok {
		return false
	}
	if matched, err := filepath.Match(glob, s); err == nil && matched {
		return true
	}
	if argName != "action" {
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

// browserCallAction is a browser tool call's action as the tool reads it:
// decoded into a struct field, so any key case matches and the last
// duplicate wins, and a missing or null action is "" (playwright_browser's
// default, run). ok is false when the input does not decode, and the tool
// refuses the call too.
func browserCallAction(rawInput string) (string, bool) {
	var p struct {
		Action *string `json:"action"`
	}
	if json.Unmarshal([]byte(rawInput), &p) != nil {
		return "", false
	}
	if p.Action == nil {
		return "", true
	}
	return *p.Action, true
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
