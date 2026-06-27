package httpapi

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

func sessionKey(convID, toolName string) string { return convID + "\x00" + toolName }

// Register records a pre-decision for (convID, toolName). Nil-safe.
func (r *SessionApprovalRegistry) Register(convID, toolName string, p SessionApprovalPolicy) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sessionKey(convID, toolName)
	r.entries[k] = append(r.entries[k], p)
}

// Match returns the first policy covering (convID, toolName, rawInput), or
// ok=false when none applies. A "" pattern matches every call; "argName=glob"
// matches when the call's args[argName] matches glob (filepath.Match). DENY
// policies are evaluated before APPROVE so an explicit pre-denial always wins.
func (r *SessionApprovalRegistry) Match(convID, toolName, rawInput string) (SessionApprovalPolicy, bool) {
	if r == nil {
		return SessionApprovalPolicy{}, false
	}
	r.mu.Lock()
	policies := append([]SessionApprovalPolicy(nil), r.entries[sessionKey(convID, toolName)]...)
	r.mu.Unlock()
	if len(policies) == 0 {
		return SessionApprovalPolicy{}, false
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(rawInput), &args)
	// Deny wins: scan denies first, then approves.
	for _, want := range [...]string{"deny", "approve"} {
		for _, p := range policies {
			if p.Mode == want && policyMatches(p, args) {
				return p, true
			}
		}
	}
	return SessionApprovalPolicy{}, false
}

// policyMatches reports whether a policy applies to the given parsed args.
func policyMatches(p SessionApprovalPolicy, args map[string]any) bool {
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
	matched, err := filepath.Match(glob, s)
	return err == nil && matched
}
