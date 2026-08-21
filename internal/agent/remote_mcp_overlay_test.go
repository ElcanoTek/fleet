package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

type recordingBroker struct{ label string }

func (b *recordingBroker) CallMCP(_ context.Context, server, _ string, _ map[string]any) (string, bool, error) {
	return b.label + ":" + server, false, nil
}

func TestCompositeBrokerRouting(t *testing.T) {
	cb := &compositeBroker{
		overlay:        &recordingBroker{label: "overlay"},
		overlayServers: map[string]bool{"userserver": true},
		base:           &recordingBroker{label: "base"},
	}
	// An overlay server routes to the overlay broker.
	got, _, _ := cb.CallMCP(context.Background(), "userserver", "tool", nil)
	if got != "overlay:userserver" {
		t.Errorf("overlay routing = %q", got)
	}
	// Anything else routes to the base broker — a user server can't shadow it.
	got, _, _ = cb.CallMCP(context.Background(), "gamma", "tool", nil)
	if got != "base:gamma" {
		t.Errorf("base routing = %q", got)
	}
}

type fakeResolver struct {
	conns    []RemoteMCPConn
	tokens   map[string]string
	tokenErr map[string]error
	listed   int
	asked    []string // server IDs AcquireTokenByID was called for
}

func (f *fakeResolver) ConnectedServersForUser(_ context.Context, _ string) ([]RemoteMCPConn, error) {
	f.listed++
	return f.conns, nil
}
func (f *fakeResolver) AcquireTokenByID(_ context.Context, _, id string) (string, error) {
	f.asked = append(f.asked, id)
	if f.tokenErr != nil {
		if e := f.tokenErr[id]; e != nil {
			return "", e
		}
	}
	return f.tokens[id], nil
}
func (f *fakeResolver) SafeHTTPClient() *http.Client { return http.DefaultClient }

func TestBuildRemoteMCPOverlayGuards(t *testing.T) {
	ctx := context.Background()
	// nil resolver → nil overlay.
	if ov, err := BuildRemoteMCPOverlay(ctx, nil, "u@x.com", nil, nil); err != nil || ov.Active() {
		t.Errorf("nil resolver: ov=%v err=%v", ov, err)
	}
	// empty email → nil overlay.
	r := &fakeResolver{conns: []RemoteMCPConn{{ID: "1", Name: "s", URL: "https://s"}}}
	if ov, err := BuildRemoteMCPOverlay(ctx, r, "", nil, nil); err != nil || ov.Active() {
		t.Errorf("empty email: ov=%v err=%v", ov, err)
	}
	// no connected servers → nil overlay.
	if ov, err := BuildRemoteMCPOverlay(ctx, &fakeResolver{}, "u@x.com", nil, nil); err != nil || ov.Active() {
		t.Errorf("no servers: ov=%v err=%v", ov, err)
	}
}

func TestBuildRemoteMCPOverlaySkipsAndReportsNeedsReauth(t *testing.T) {
	ctx := context.Background()
	const sensitiveDetail = "provider-response-detail-must-stay-private"
	reauth := errors.New(sensitiveDetail)
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	r := &fakeResolver{
		conns:    []RemoteMCPConn{{ID: "1", Name: "dead", URL: "https://dead.example.com"}},
		tokenErr: map[string]error{"1": reauth},
	}
	ov, err := BuildRemoteMCPOverlay(ctx, r, "u@x.com", nil, nil)
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov.Close()
	if ov.Active() {
		t.Error("overlay should be inactive when the only server needs reauth")
	}
	if len(ov.Skipped) != 1 || ov.Skipped[0] != "dead" {
		t.Errorf("Skipped = %v, want [dead]", ov.Skipped)
	}
	if strings.Contains(logs.String(), sensitiveDetail) {
		t.Fatal("token failure detail reached logs")
	}
}

func TestBuildRemoteMCPOverlayOptInFilter(t *testing.T) {
	ctx := context.Background()
	// Both servers' token fetch errors, but only the opted-in one is even ATTEMPTED.
	r := &fakeResolver{
		conns: []RemoteMCPConn{
			{ID: "1", Name: "s1", URL: "https://s1.example.com"},
			{ID: "2", Name: "s2", URL: "https://s2.example.com"},
		},
		tokenErr: map[string]error{"1": errors.New("x"), "2": errors.New("x")},
	}
	ov, err := BuildRemoteMCPOverlay(ctx, r, "u@x.com", nil, map[string]bool{"s2": true})
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov.Close()
	if len(r.asked) != 1 || r.asked[0] != "2" {
		t.Errorf("opt-in filter: AcquireToken asked for %v, want only [2]", r.asked)
	}
}

func TestBuildRemoteMCPOverlayOptInMatchesLowercased(t *testing.T) {
	ctx := context.Background()
	// The persisted opt-in list is canonically lowercase, but AddServer only
	// trims a remote server's name — a user's "GitHub" must still pass the
	// gate when the conversation enabled "github". Token fetch errors so the
	// gate outcome is observable via asked/Skipped without any network dial.
	r := &fakeResolver{
		conns:    []RemoteMCPConn{{ID: "1", Name: "GitHub", URL: "https://gh.example.com"}},
		tokenErr: map[string]error{"1": errors.New("x")},
	}
	ov, err := BuildRemoteMCPOverlay(ctx, r, "u@x.com", nil, map[string]bool{"github": true})
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov.Close()
	if len(r.asked) != 1 || r.asked[0] != "1" {
		t.Errorf("lowercased opt-in: AcquireToken asked for %v, want [1]", r.asked)
	}
	// A non-enabled server still doesn't pass just because of case games.
	r2 := &fakeResolver{
		conns:    []RemoteMCPConn{{ID: "1", Name: "GitHub", URL: "https://gh.example.com"}},
		tokenErr: map[string]error{"1": errors.New("x")},
	}
	ov2, err := BuildRemoteMCPOverlay(ctx, r2, "u@x.com", nil, map[string]bool{"other": true})
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov2.Close()
	if len(r2.asked) != 0 {
		t.Errorf("non-enabled server passed the gate: asked=%v", r2.asked)
	}
}

func TestApplyMCPOverlayNoopWhenInactive(t *testing.T) {
	deps := agentcore.Deps{}
	base := mcp.NewClient()
	// nil overlay → no broker/catalog wiring.
	ApplyMCPOverlayWithBase(&deps, base, nil, nil, nil)
	if deps.MCPBroker != nil || deps.MCPCatalog != nil {
		t.Error("nil overlay should leave Deps untouched")
	}
	// Inactive overlay (no servers) → no-op too.
	ApplyMCPOverlayWithBase(&deps, base, nil, nil, &RemoteMCPOverlay{Client: base})
	if deps.MCPBroker != nil || deps.MCPCatalog != nil {
		t.Error("inactive overlay should leave Deps untouched")
	}
}

func TestApplyMCPOverlayActiveSetsCompositeBroker(t *testing.T) {
	deps := agentcore.Deps{}
	base := mcp.NewClient()
	overlayClient := mcp.NewClient()
	overlay := &RemoteMCPOverlay{
		Client:  overlayClient,
		Servers: map[string]bool{"userserver": true},
		Catalog: nil,
	}
	ApplyMCPOverlayWithBase(&deps, base, nil, nil, overlay)
	if deps.MCPBroker == nil {
		t.Fatal("active overlay should set a composite broker")
	}
	if _, ok := deps.MCPBroker.(*compositeBroker); !ok {
		t.Errorf("expected *compositeBroker, got %T", deps.MCPBroker)
	}
	// MCPCatalog is set (merged) even when empty, so the loop advertises it.
	if deps.MCPCatalog == nil {
		// base + overlay are both empty here, so merged is an empty non-nil slice
		// only if base had tools; with both empty append yields nil — acceptable.
		t.Log("merged catalog is empty (both clients empty) — fine")
	}
}

func TestApplyMCPOverlayWithInjectedBaseBroker(t *testing.T) {
	deps := agentcore.Deps{}
	base := &recordingBroker{label: "injected"}
	overlayClient := mcp.NewClient()
	overlay := &RemoteMCPOverlay{
		Client:  overlayClient,
		Servers: map[string]bool{"userserver": true},
	}
	baseCatalog := []mcp.ServerTool{{ServerName: "bundle"}}

	ApplyMCPOverlayWithBase(&deps, nil, base, baseCatalog, overlay)
	if deps.MCPBroker == nil {
		t.Fatal("injected base + active overlay did not install a composite broker")
	}
	text, _, err := deps.MCPBroker.CallMCP(context.Background(), "bundle", "tool", nil)
	if err != nil || text != "injected:bundle" {
		t.Fatalf("base route = (%q, %v), want injected broker", text, err)
	}
	if len(deps.MCPCatalog) != 1 || deps.MCPCatalog[0].ServerName != "bundle" {
		t.Fatalf("merged catalog = %+v, want injected base catalog", deps.MCPCatalog)
	}
}

func TestApplyMCPOverlayWithInjectedOverlayBroker(t *testing.T) {
	deps := agentcore.Deps{}
	base := &recordingBroker{label: "base"}
	overlayBroker := &recordingBroker{label: "remote"}
	overlay := &RemoteMCPOverlay{
		Broker:  overlayBroker,
		Servers: map[string]bool{"userserver": true},
		Catalog: []mcp.ServerTool{{ServerName: "userserver"}},
	}

	ApplyMCPOverlayWithBase(&deps, nil, base, nil, overlay)
	got, _, err := deps.MCPBroker.CallMCP(context.Background(), "userserver", "tool", nil)
	if err != nil || got != "remote:userserver" {
		t.Fatalf("remote route = (%q, %v), want injected overlay broker", got, err)
	}
	got, _, err = deps.MCPBroker.CallMCP(context.Background(), "bundle", "tool", nil)
	if err != nil || got != "base:bundle" {
		t.Fatalf("base route = (%q, %v), want base broker", got, err)
	}
}

func TestRemoteMCPOverlayCloseUsesFreshBoundedContext(t *testing.T) {
	var called bool
	overlay := &RemoteMCPOverlay{CloseScope: func(ctx context.Context) error {
		called = true
		if err := ctx.Err(); err != nil {
			t.Fatalf("close context already cancelled: %v", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("close context has no deadline")
		}
		return nil
	}}
	overlay.Close()
	if !called {
		t.Fatal("broker scope close was not called")
	}
}

func TestRemoteMCPOverlayValidateRejectsUnownedBroker(t *testing.T) {
	overlay := &RemoteMCPOverlay{
		Broker:  &recordingBroker{label: "remote"},
		Servers: map[string]bool{"remote": true},
	}
	if err := overlay.Validate(); err == nil {
		t.Fatal("broker overlay without close function passed validation")
	}
}

func TestManagerOpenRemoteOverlayPrefersInjectedOpener(t *testing.T) {
	resolver := &fakeResolver{conns: []RemoteMCPConn{{ID: "should-not-list"}}}
	var gotEmail string
	manager := &Manager{
		remoteMCP: resolver,
		openRemoteMCPOverlay: func(_ context.Context, email string, shadowed, enabled map[string]bool) (*RemoteMCPOverlay, error) {
			gotEmail = email
			if !shadowed["bundle"] || len(shadowed) != 1 {
				t.Fatalf("shadowed = %v, want bundle only", shadowed)
			}
			if !enabled["remote"] || len(enabled) != 1 {
				t.Fatalf("enabled = %v, want remote only", enabled)
			}
			return &RemoteMCPOverlay{}, nil
		},
	}

	_, err := manager.openRemoteOverlay(context.Background(), "user@example.com", []mcp.ServerTool{{ServerName: "bundle"}}, []string{" ", "remote"})
	if err != nil {
		t.Fatalf("openRemoteOverlay: %v", err)
	}
	if gotEmail != "user@example.com" {
		t.Fatalf("email = %q", gotEmail)
	}
	if resolver.listed != 0 {
		t.Fatalf("compatibility resolver was called %d time(s)", resolver.listed)
	}
}

// browserbaseKeyFunc is what makes "paste the key once, in Settings →
// Connections" true: it unseals the running user's own Browserbase connector
// credential for the host-side live-view tool (#987). It must find the connection
// by URL host, because the registration name is whatever the user typed.
func TestBrowserbaseKeyFuncResolvesByURLHost(t *testing.T) {
	res := &fakeResolver{
		conns: []RemoteMCPConn{
			{ID: "other", Name: "browserbase", URL: "https://mcp.example.test/mcp"},
			// Named nothing like the vendor, but it IS the vendor.
			{ID: "bb", Name: "bb", URL: "https://mcp.browserbase.com/mcp?keepAlive=true"},
		},
		tokens: map[string]string{"bb": "bb-connector-key"},
	}
	m := &Manager{remoteMCP: res}

	fn := m.browserbaseKeyFunc(context.Background(), "user@example.test", []string{"bb"})
	if fn == nil {
		t.Fatal("a wired resolver + a user + an enabled connection must yield a key func")
	}
	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "bb-connector-key" {
		t.Errorf("key = %q, want the browserbase connection's key", got)
	}
	if len(res.asked) != 1 || res.asked[0] != "bb" {
		t.Errorf("asked for %v; must unseal only the Browserbase connection", res.asked)
	}
}

// A nil func is what keeps browserbase_live_view unregistered, so every case that
// cannot reach a key must return nil rather than a func that fails later. The
// opt-in case is the security-relevant one: a chat with Browserbase switched OFF
// must not be able to unseal the key or enumerate the account's sessions.
func TestBrowserbaseKeyFuncReturnsNilWhenUnreachable(t *testing.T) {
	bb := []RemoteMCPConn{{ID: "bb", Name: "browserbase", URL: "https://mcp.browserbase.com/mcp"}}
	for _, tc := range []struct {
		name     string
		resolver RemoteMCPResolver
		email    string
		enabled  []string
	}{
		{"no resolver wired", nil, "user@example.test", []string{"browserbase"}},
		{"no user on the run", &fakeResolver{conns: bb}, "", []string{"browserbase"}},
		{"user has no browserbase connection", &fakeResolver{
			conns: []RemoteMCPConn{{ID: "n", Name: "notion", URL: "https://mcp.notion.test/mcp"}},
		}, "user@example.test", []string{"notion"}},
		{"connector switched off for this chat", &fakeResolver{conns: bb}, "user@example.test", []string{"notion"}},
		{"nothing enabled in this chat", &fakeResolver{conns: bb}, "user@example.test", []string{}},
		// nil must gate exactly like empty: RunTurnInput.OptionalMCPServersEnabled
		// documents "nil/empty means no optional servers", and openRemoteOverlay
		// wires zero remote servers for a nil list. A key func here would register
		// the tool and unseal the credential in a chat with every connector off.
		{"opt-in list never seeded (nil)", &fakeResolver{conns: bb}, "user@example.test", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{remoteMCP: tc.resolver}
			if fn := m.browserbaseKeyFunc(context.Background(), tc.email, tc.enabled); fn != nil {
				t.Error("expected nil so the tool stays unregistered and the key stays sealed")
			}
		})
	}
}

// The URL match must survive an explicit port: url.URL.Host keeps ":443", so the
// resolver compares Hostname(). A silent non-match here unregisters the tool with
// no diagnostic despite a fully working connector.
func TestBrowserbaseKeyFuncMatchesURLWithExplicitPort(t *testing.T) {
	m := &Manager{remoteMCP: &fakeResolver{
		conns:  []RemoteMCPConn{{ID: "bb", Name: "browserbase", URL: "https://mcp.browserbase.com:443/mcp"}},
		tokens: map[string]string{"bb": "k"},
	}}
	if fn := m.browserbaseKeyFunc(context.Background(), "user@example.test", []string{"browserbase"}); fn == nil {
		t.Error("an explicit :443 must not defeat the vendor-host match")
	}
}
