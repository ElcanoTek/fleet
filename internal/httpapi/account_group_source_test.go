package httpapi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestParseSourcedAccountGroups(t *testing.T) {
	t.Parallel()
	got, err := parseSourcedAccountGroups(`{"groups": [
		{"name": "The Weather Company", "updated": "2026-11-01", "accounts": [
			{"server": "ix", "account": "TWC"}, {"server": "not_on_this_box", "account": "twc"}]},
		{"name": "", "accounts": [{"server": "ix"}]},
		{"id": "arena", "name": "The Arena Group", "accounts": [{"server": "ix", "account": "arena"}]},
		{"id": "arena", "name": "Arena again", "accounts": [{"server": "ix", "account": "arena"}]},
		{"name": "Empty", "accounts": []}
	], "generated_at": 1}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []clientconfig.AccountGroup{
		// A server this box does not offer is kept: the chip reports it.
		{ID: "the-weather-company", Name: "The Weather Company", Accounts: []clientconfig.AccountGroupMember{
			{Server: "ix", Account: "twc"}, {Server: "not_on_this_box", Account: "twc"},
		}},
		{ID: "arena", Name: "The Arena Group", Accounts: []clientconfig.AccountGroupMember{{Server: "ix", Account: "arena"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %#v, want %#v", got, want)
	}

	// No groups is a valid answer: a source may have nothing to list.
	if got, err := parseSourcedAccountGroups(`{"groups": []}`); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %#v, %v", got, err)
	}
	for name, text := range map[string]string{
		"not JSON":        `clients: twc`,
		"no groups key":   `{"clients": []}`,
		"groups is null":  `{"groups": null}`,
		"every one bad":   `{"groups": [{"name": ""}, {"name": "A"}]}`,
		"the wrong shape": `{"groups": {"twc": {}}}`,
		"too large":       `{"groups": [], "pad": "` + strings.Repeat("x", accountGroupSourceMaxBytes) + `"}`,
	} {
		if _, err := parseSourcedAccountGroups(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMergedAccountGroups(t *testing.T) {
	t.Parallel()
	g := func(id string) clientconfig.AccountGroup {
		return clientconfig.AccountGroup{ID: id, Name: id, Accounts: []clientconfig.AccountGroupMember{{Server: "ix"}}}
	}
	fixed := []clientconfig.AccountGroup{g("house"), g("twc")}
	got := mergedAccountGroups(fixed, []clientconfig.AccountGroup{g("twc"), g("arena")})
	ids := make([]string, 0, len(got))
	for _, x := range got {
		ids = append(ids, x.ID)
	}
	// Fixed groups first, and a fixed group wins its id.
	if !reflect.DeepEqual(ids, []string{"house", "twc", "arena"}) {
		t.Fatalf("merged ids = %v", ids)
	}
	if len(fixed) != 2 {
		t.Fatal("merging changed the fixed list")
	}
	if got := mergedAccountGroups(fixed, nil); !reflect.DeepEqual(got, fixed) {
		t.Fatalf("no sourced groups changed the list: %v", got)
	}
}

// sourceBroker answers the source tool and counts the calls.
type sourceBroker struct {
	mu     sync.Mutex
	calls  []string
	answer string
	err    error
	gate   chan struct{}
	// panicOnce makes the next call panic, as a faulty dependency would.
	panicOnce bool
}

func (b *sourceBroker) CallMCP(ctx context.Context, server, tool string, _ map[string]any) (string, bool, error) {
	b.mu.Lock()
	b.calls = append(b.calls, server+"/"+tool)
	answer, err, gate := b.answer, b.err, b.gate
	boom := b.panicOnce
	b.panicOnce = false
	b.mu.Unlock()
	if boom {
		panic("source broker fault")
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	return answer, false, err
}

func (b *sourceBroker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *sourceBroker) set(answer string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer, b.err = answer, err
}

type sourceEngine struct {
	*fakeTurnEngine
	broker *sourceBroker
	// catalog is what the source server published; nil = the tool, unmarked.
	catalog []mcp.ServerTool
}

func (e *sourceEngine) MCPBroker() agentcore.MCPBroker { return e.broker }
func (e *sourceEngine) MCPCatalog() []mcp.ServerTool {
	if e.catalog != nil {
		return e.catalog
	}
	return []mcp.ServerTool{{ServerName: "deals", Tool: mcp.Tool{Name: "list_account_groups"}}}
}
func (e *sourceEngine) MCPServerCatalog() []agent.OptionalServerInfo {
	return []agent.OptionalServerInfo{{Name: "ix", Accounts: []string{"twc"}, Tools: []string{"get_deal"}, ToolCount: 1}}
}

func sourcePolicy(t *testing.T, parallelSafe ...string) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{ParallelSafeTools: parallelSafe})
}

func waitForRefresh(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.accountGroupSource.mu.Lock()
		running := s.accountGroupSource.refreshing
		s.accountGroupSource.mu.Unlock()
		if running == nil {
			return
		}
		<-running
	}
	t.Fatal("refresh did not finish")
}

func groupIDs(groups []clientconfig.AccountGroup) []string {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	return ids
}

const twoGroups = `{"groups": [
	{"name": "The Weather Company", "accounts": [{"server": "ix", "account": "twc"}]},
	{"name": "The Arena Group", "accounts": [{"server": "ix", "account": "arena"}]}]}`

func TestSourcedAccountGroupsCachesAndRefreshes(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	broker := &sourceBroker{answer: twoGroups}
	s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}}
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}
	ctx := context.Background()

	// The first read waits for the first answer.
	if got := groupIDs(s.sourcedAccountGroups(ctx, src)); !reflect.DeepEqual(got, []string{"the-weather-company", "the-arena-group"}) {
		t.Fatalf("first read = %v", got)
	}
	if broker.callCount() != 1 || broker.calls[0] != "deals/list_account_groups" {
		t.Fatalf("calls = %v", broker.calls)
	}
	// A fresh answer is served from the cache.
	s.sourcedAccountGroups(ctx, src)
	if broker.callCount() != 1 {
		t.Fatalf("a fresh answer was refetched: %d calls", broker.callCount())
	}

	// Stale: the next read is served the old answer at once and refreshes
	// behind it.
	broker.set(`{"groups": [{"name": "Only One", "accounts": [{"server": "ix"}]}]}`, nil)
	s.accountGroupSource.mu.Lock()
	s.accountGroupSource.nextRefresh = time.Now().Add(-time.Second)
	s.accountGroupSource.mu.Unlock()
	if got := groupIDs(s.sourcedAccountGroups(ctx, src)); len(got) != 2 {
		t.Fatalf("a stale read should serve the last answer, got %v", got)
	}
	waitForRefresh(t, s)
	if got := groupIDs(s.sourcedAccountGroups(ctx, src)); !reflect.DeepEqual(got, []string{"only-one"}) {
		t.Fatalf("after refresh = %v", got)
	}

	// A failed refresh keeps the last good answer and is retried sooner than
	// a good one.
	broker.set("", errors.New("upstream down"))
	s.accountGroupSource.mu.Lock()
	s.accountGroupSource.nextRefresh = time.Now().Add(-time.Second)
	s.accountGroupSource.mu.Unlock()
	s.sourcedAccountGroups(ctx, src)
	waitForRefresh(t, s)
	if got := groupIDs(s.sourcedAccountGroups(ctx, src)); !reflect.DeepEqual(got, []string{"only-one"}) {
		t.Fatalf("a failed refresh dropped the last answer: %v", got)
	}
	s.accountGroupSource.mu.Lock()
	retryIn := time.Until(s.accountGroupSource.nextRefresh)
	s.accountGroupSource.mu.Unlock()
	if retryIn > accountGroupSourceRetry || retryIn <= 0 {
		t.Fatalf("retry in %v, want within %v", retryIn, accountGroupSourceRetry)
	}

	// No source: nothing, and no call.
	before := broker.callCount()
	if got := s.sourcedAccountGroups(ctx, nil); got != nil || broker.callCount() != before {
		t.Fatalf("no source: %v, %d calls", got, broker.callCount()-before)
	}
}

// fleet calls the source with no approval and no hook run behind it, so it
// calls only a tool the bundle declared a read (ADR-0119).
func TestSourcedAccountGroupsCallsOnlyADeclaredRead(t *testing.T) {
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}
	cases := map[string]agentcore.AgentPolicy{
		"not in parallel_safe_tools": {},
		"critical":                   {ParallelSafeTools: []string{"mcp_deals_list_account_groups"}, CriticalToolSuffixes: []string{"list_account_groups"}},
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
			agentcore.ConfigureAgentPolicy(policy)
			broker := &sourceBroker{answer: twoGroups}
			s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}}
			if got := s.sourcedAccountGroups(context.Background(), src); len(got) != 0 {
				t.Fatalf("groups = %v, want none", got)
			}
			if broker.callCount() != 0 {
				t.Fatalf("the tool was called %d times", broker.callCount())
			}
		})
	}
}

// The server's own discovery decides too: a tool it marks critical is
// approval-gated in every turn, so it is never called as a host read even
// when the bundle lists it as parallel-safe, and a tool it does not export is
// not called at all.
func TestSourcedAccountGroupsRespectsTheServersOwnCatalog(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}
	cases := map[string][]mcp.ServerTool{
		"marked critical by the server": {{ServerName: "deals", Tool: mcp.Tool{Name: "list_account_groups", Critical: true}}},
		"not exported":                  {{ServerName: "deals", Tool: mcp.Tool{Name: "something_else"}}},
		"exported by another server":    {{ServerName: "other", Tool: mcp.Tool{Name: "list_account_groups"}}},
	}
	for name, catalog := range cases {
		t.Run(name, func(t *testing.T) {
			broker := &sourceBroker{answer: twoGroups}
			s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker, catalog: catalog}}
			if got := s.sourcedAccountGroups(context.Background(), src); len(got) != 0 {
				t.Fatalf("groups = %v, want none", got)
			}
			if broker.callCount() != 0 {
				t.Fatalf("the tool was called %d times", broker.callCount())
			}
		})
	}
}

// A slow source does not hold the picker: the first read gives up waiting and
// later reads are served whatever has arrived.
func TestSourcedAccountGroupsDoesNotBlockOnASlowSource(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	broker := &sourceBroker{answer: twoGroups, gate: make(chan struct{})}
	s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}}
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}

	// A caller that has gone away does not wait at all.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if got := s.sourcedAccountGroups(gone, src); len(got) != 0 {
		t.Fatalf("groups before the source answered: %v", got)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("a cancelled read waited %v", waited)
	}
	// Only one read waits for the first answer. A source that takes longer
	// than that must not hold every reader that arrives while its first
	// refresh is still running: this one has a live request and still
	// returns at once, with nothing yet.
	start = time.Now()
	if got := s.sourcedAccountGroups(context.Background(), src); len(got) != 0 {
		t.Fatalf("groups before the source answered: %v", got)
	}
	if waited := time.Since(start); waited > accountGroupSourceFirstWait/2 {
		t.Fatalf("a second read waited %v for the first refresh", waited)
	}
	// The refresh the first read started outlives its caller.
	close(broker.gate)
	waitForRefresh(t, s)
	if got := s.sourcedAccountGroups(context.Background(), src); len(got) != 2 {
		t.Fatalf("after the source answered: %v", got)
	}
	if broker.callCount() != 1 {
		t.Fatalf("calls = %d, want the one refresh", broker.callCount())
	}
}

// A dependency that panics during a refresh (the tracker recovers it) must not
// leave the refresh marked as running: that would stop every later refresh.
func TestSourcedAccountGroupsRecoversFromAPanickingRefresh(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}
	broker := &sourceBroker{answer: twoGroups, panicOnce: true}
	s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}}
	ctx := context.Background()

	if got := s.sourcedAccountGroups(ctx, src); len(got) != 0 {
		t.Fatalf("groups from a refresh that panicked: %v", got)
	}
	waitForRefresh(t, s)
	s.accountGroupSource.mu.Lock()
	running, retryIn := s.accountGroupSource.refreshing, time.Until(s.accountGroupSource.nextRefresh)
	s.accountGroupSource.nextRefresh = time.Now().Add(-time.Second)
	s.accountGroupSource.mu.Unlock()
	if running != nil {
		t.Fatal("the refresh is still marked as running after it panicked")
	}
	if retryIn <= 0 || retryIn > accountGroupSourceRetry {
		t.Fatalf("retry in %v, want within %v", retryIn, accountGroupSourceRetry)
	}
	// The next refresh runs and its answer is served.
	s.sourcedAccountGroups(ctx, src)
	waitForRefresh(t, s)
	if got := s.sourcedAccountGroups(ctx, src); len(got) != 2 {
		t.Fatalf("after the retry: %v", got)
	}
}

// A refresh is server background work: shutdown waits for one that is using
// an MCP scope, and a read after shutdown began starts none.
func TestSourcedAccountGroupsRefreshIsTrackedBackgroundWork(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	src := &clientconfig.AccountGroupsSource{Server: "deals", Tool: "list_account_groups", RefreshSeconds: 300}

	broker := &sourceBroker{answer: twoGroups, gate: make(chan struct{})}
	s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	s.sourcedAccountGroups(gone, src)
	stopped := make(chan struct{})
	go func() {
		s.StopBackground()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("StopBackground returned while a refresh was still calling the source")
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.gate)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopBackground did not return after the refresh finished")
	}
	if broker.callCount() != 1 {
		t.Fatalf("calls = %d, want the one refresh", broker.callCount())
	}

	// After shutdown began: no call, no refresh left marked as running, and
	// the read does not wait for one.
	late := &sourceBroker{answer: twoGroups}
	s2 := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: late}}
	s2.StopBackground()
	start := time.Now()
	if got := s2.sourcedAccountGroups(context.Background(), src); len(got) != 0 {
		t.Fatalf("groups after shutdown: %v", got)
	}
	if waited := time.Since(start); waited > accountGroupSourceFirstWait/2 {
		t.Fatalf("a read after shutdown waited %v", waited)
	}
	if late.callCount() != 0 {
		t.Fatalf("the source was called %d times after shutdown", late.callCount())
	}
	s2.accountGroupSource.mu.Lock()
	running := s2.accountGroupSource.refreshing
	s2.accountGroupSource.mu.Unlock()
	if running != nil {
		t.Fatal("a refresh that never started is still marked as running")
	}
}

// The catalog response lists the manifest's groups and the source's, each
// member checked against this box.
func TestCatalogMergesSourcedAccountGroups(t *testing.T) {
	sourcePolicy(t, "mcp_deals_list_account_groups")
	bundle, err := clientconfig.Load(writeSourceBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	broker := &sourceBroker{answer: `{"groups": [
		{"name": "The Weather Company", "accounts": [{"server": "ix", "account": "twc"}, {"server": "ox", "account": "twc"}]},
		{"id": "house", "name": "Not the house", "accounts": [{"server": "ix", "account": "twc"}]}]}`}
	s := &Server{store: newFakeChatStore(), agent: &sourceEngine{fakeTurnEngine: &fakeTurnEngine{}, broker: broker}, clientConfig: bundle}

	body := s.withAccountGroups(context.Background(), map[string]any{}, []map[string]any{{"name": "ix"}}, nil)
	view, _ := body["account_groups"].(*accountGroupsView)
	if view == nil {
		t.Fatalf("no account_groups in %v", body)
	}
	want := &accountGroupsView{Label: "Clients", Groups: []accountGroupView{
		{ID: "house", Name: "House", Accounts: []accountGroupMemberView{{Server: "ix", Account: "", Status: accountGroupMemberOK}}},
		{ID: "the-weather-company", Name: "The Weather Company", Accounts: []accountGroupMemberView{
			{Server: "ix", Account: "twc", Status: accountGroupMemberOK},
			{Server: "ox", Account: "twc", Status: accountGroupMemberConnectorMissing},
		}},
	}}
	if !reflect.DeepEqual(view, want) {
		t.Fatalf("account_groups = %+v, want %+v", view, want)
	}
}
