package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The account-groups source (docs/ACCOUNT-GROUPS.md, ADR-0119): a tool the
// bundle names that lists account groups at run time, for a deployment whose
// groups live in another system. fleet calls it on its own, with no chat turn
// behind the call, so the call is held to the host-read rules
// (agentcore.HostReadRefusal) and its answer is display data only: it is
// validated against the fixed group schema, cached, and sent to the picker.
// It never reaches a model.

const (
	// accountGroupSourceCallTimeout bounds one call to the source, as a
	// context deadline and as the per-call budget the broker child applies.
	accountGroupSourceCallTimeout = 15 * time.Second
	// accountGroupSourceFirstWait is how long the first catalog read after
	// boot waits for the first answer, so the picker opens with its chips.
	// After that a read never waits: it is served the last answer while a
	// refresh runs behind it.
	accountGroupSourceFirstWait = 2 * time.Second
	// accountGroupSourceRetry is how soon a failed refresh is tried again.
	accountGroupSourceRetry = 60 * time.Second
	// accountGroupSourceMaxBytes bounds the answer fleet will parse.
	accountGroupSourceMaxBytes = 1 << 20
)

// accountGroupSource caches the last good answer of the bundle's source.
type accountGroupSource struct {
	mu sync.Mutex
	// groups is the last good answer; nil until the first one.
	groups []clientconfig.AccountGroup
	// waited reports whether a read has already waited for the first answer.
	// Exactly one does: the wait is for the picker that opens right after
	// boot, and a source that takes longer than that must not hold every
	// reader that arrives while its first refresh is still running.
	waited bool
	// nextRefresh is when the cached answer (or the last failure) goes stale.
	nextRefresh time.Time
	// refreshing is non-nil while a refresh runs; it is closed when it ends.
	refreshing chan struct{}
}

// sourcedAccountGroups returns the groups the bundle's source last listed,
// starting a refresh behind the caller when the answer is stale. nil when the
// bundle names no source, or it has never answered.
func (s *Server) sourcedAccountGroups(ctx context.Context, src *clientconfig.AccountGroupsSource) []clientconfig.AccountGroup {
	if src == nil || s.agent == nil {
		return nil
	}
	c := &s.accountGroupSource
	c.mu.Lock()
	var wait chan struct{}
	if c.refreshing == nil && !time.Now().Before(c.nextRefresh) {
		c.refreshing = make(chan struct{})
		done := c.refreshing
		source := *src
		// Detached from the request: the answer is for every later read too,
		// and the reader that started it may be gone before it lands.
		refreshCtx := context.WithoutCancel(ctx)
		safe.Go("httpapi.account-groups.refresh", func() {
			defer close(done)
			s.refreshAccountGroups(refreshCtx, source)
		})
	}
	if !c.waited {
		c.waited = true
		wait = c.refreshing
	}
	c.mu.Unlock()

	if wait != nil {
		timer := time.NewTimer(accountGroupSourceFirstWait)
		defer timer.Stop()
		select {
		case <-wait:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clientconfig.AccountGroup(nil), c.groups...)
}

// refreshAccountGroups asks the source once and stores the outcome. A failure
// keeps the last good answer: a source that is down for a while must not
// empty every user's picker.
func (s *Server) refreshAccountGroups(ctx context.Context, src clientconfig.AccountGroupsSource) {
	groups, err := s.fetchAccountGroups(ctx, src)
	c := &s.accountGroupSource
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = nil
	if err != nil {
		c.nextRefresh = time.Now().Add(accountGroupSourceRetry)
		logging.From(ctx).Error("account groups: source not read; keeping the last answer",
			"server", safe.LogSafe(src.Server), "tool", safe.LogSafe(src.Tool), "kept_groups", len(c.groups), "err", redactedForLog(err))
		return
	}
	c.groups = groups
	c.nextRefresh = time.Now().Add(time.Duration(src.RefreshSeconds) * time.Second)
}

// fetchAccountGroups makes the one call and validates its answer.
func (s *Server) fetchAccountGroups(ctx context.Context, src clientconfig.AccountGroupsSource) ([]clientconfig.AccountGroup, error) {
	full := agentcore.MCPToolName(src.Server, src.Tool)
	// Re-checked before every call under the installed policy: this call has
	// no approval and no hook run behind it, so it is made only for a tool
	// the bundle declared a read (ADR-0119).
	if why := agentcore.HostReadRefusal(full); why != "" {
		return nil, fmt.Errorf("%s is not called: %s", full, why)
	}
	ctx, cancel := context.WithTimeout(mcp.WithCallTimeout(ctx, accountGroupSourceCallTimeout), accountGroupSourceCallTimeout)
	defer cancel()

	// The source server's default account, through the credential owner's own
	// scope gate (ADR-0042), with no user identity: groups are the
	// deployment's, not one person's.
	broker, release, err := s.accountGroupSourceBroker(ctx, src.Server)
	if err != nil {
		return nil, err
	}
	defer release()
	text, isErr, err := broker.CallMCP(ctx, src.Server, src.Tool, map[string]any{})
	switch {
	case err != nil:
		return nil, fmt.Errorf("call %s: %w", full, err)
	case isErr:
		return nil, fmt.Errorf("%s reported an error", full)
	}
	return parseSourcedAccountGroups(text)
}

// accountGroupSourceBroker opens a scope holding only the source server, or
// falls back to the engine's shared broker where there is no scope opener
// (the transitional local-client mode), as approval execution does.
func (s *Server) accountGroupSourceBroker(ctx context.Context, server string) (agentcore.MCPBroker, func(), error) {
	scope, err := s.agent.OpenApprovalMCPScope(ctx, agentcore.MCPSelection{{Server: server}}, "", agentcore.MCPUserIdentity{})
	if err != nil {
		return nil, nil, fmt.Errorf("open MCP scope for %s: %w", server, err)
	}
	if scope == nil {
		broker := s.agent.MCPBroker()
		if broker == nil {
			return nil, nil, errors.New("MCP broker not initialized")
		}
		return broker, func() {}, nil
	}
	release := func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), approvalScopeCloseTimeout)
		defer cancel()
		if err := scope.Close(closeCtx); err != nil {
			logging.From(ctx).Error("account groups: close MCP scope failed", "err", err)
		}
	}
	return scope.Broker, release, nil
}

// parseSourcedAccountGroups validates a source's answer:
//
//	{"groups": [{"id": "...", "name": "...", "accounts": [{"server": "...", "account": "..."}]}]}
//
// Unknown keys are ignored, so a bundle can carry its own extras. The groups
// are held to the manifest's rules (clientconfig.NormalizeAccountGroups), one
// group at a time: a group that breaks a rule is left out and the rest are
// kept, because one malformed record in another system must not take every
// client's chip away. A member's server is not checked here: a connector this
// box does not offer is reported on the chip (connector_missing), which is
// where the person who can fix it will see it. An answer the secret redaction
// would alter is refused whole.
func parseSourcedAccountGroups(text string) ([]clientconfig.AccountGroup, error) {
	text = strings.TrimSpace(text)
	if len(text) > accountGroupSourceMaxBytes {
		return nil, fmt.Errorf("answer is %d bytes; at most %d are read", len(text), accountGroupSourceMaxBytes)
	}
	var payload struct {
		Groups *[]clientconfig.AccountGroup `json:"groups"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("answer is not the expected JSON object: %w", err)
	}
	if payload.Groups == nil {
		return nil, errors.New(`answer has no "groups" list`)
	}
	if tools.RedactionWouldAlter([]byte(text)) {
		return nil, errors.New("answer carries a secret-like value; refused")
	}
	listed := *payload.Groups
	if len(listed) > clientconfig.MaxAccountGroups {
		listed = listed[:clientconfig.MaxAccountGroups]
	}
	out := make([]clientconfig.AccountGroup, 0, len(listed))
	seen := make(map[string]bool, len(listed))
	for _, g := range listed {
		normalized, err := clientconfig.NormalizeAccountGroups("groups", []clientconfig.AccountGroup{g}, nil)
		if err != nil || len(normalized) != 1 || seen[normalized[0].ID] {
			continue
		}
		seen[normalized[0].ID] = true
		out = append(out, normalized[0])
	}
	if len(out) == 0 && len(listed) > 0 {
		return nil, fmt.Errorf("none of the %d listed groups is valid", len(listed))
	}
	return out, nil
}

// mergedAccountGroups is the bundle's fixed groups followed by the source's.
// A sourced group whose id a fixed group already has is left out: the
// manifest is the reviewed list and wins.
func mergedAccountGroups(fixed, sourced []clientconfig.AccountGroup) []clientconfig.AccountGroup {
	if len(sourced) == 0 {
		return fixed
	}
	out := append([]clientconfig.AccountGroup(nil), fixed...)
	seen := make(map[string]bool, len(fixed))
	for _, g := range fixed {
		seen[g.ID] = true
	}
	for _, g := range sourced {
		if seen[g.ID] || len(out) >= clientconfig.MaxAccountGroups {
			continue
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	return out
}
