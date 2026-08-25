package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Seats (#988): one connection name may carry several logins. A run mounts
// exactly one per name — the pinned one, else the default — and never
// substitutes another account for a pin it cannot honor.

func seatFixture() []RemoteMCPConn {
	return []RemoteMCPConn{
		{ID: "gh-work", Name: "github", Account: "work", Default: true, URL: "https://gh/work"},
		{ID: "gh-personal", Name: "github", Account: "personal", URL: "https://gh/personal"},
		{ID: "gamma-legacy", Name: "gamma", Account: "", Default: true, URL: "https://gamma"},
		{ID: "gamma-team", Name: "gamma", Account: "team", URL: "https://gamma"},
	}
}

func chosenIDs(conns []RemoteMCPConn) []string {
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.ID)
	}
	return out
}

func TestSelectRemoteSeatsDefaultsAndPins(t *testing.T) {
	conns := seatFixture()

	// Scheduled default: every name on its default seat, no pins.
	chosen, missing := selectRemoteSeats(conns, RemoteMCPAllConnected)
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"gh-work", "gamma-legacy"}) || len(missing) != 0 {
		t.Fatalf("all-connected = %v missing %v", got, missing)
	}

	// A pin selects the labeled seat; "" (non-Exact) is still "the default".
	chosen, missing = selectRemoteSeats(conns, RemoteMCPSelection{Accounts: map[string]string{"github": "personal", "gamma": ""}})
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"gh-personal", "gamma-legacy"}) || len(missing) != 0 {
		t.Fatalf("pinned = %v missing %v", got, missing)
	}

	// Interactive filter: only opted-in names, lowercased match tolerated.
	chosen, _ = selectRemoteSeats([]RemoteMCPConn{{ID: "1", Name: "GitHub", Default: true}}, RemoteMCPEnabledOnly([]string{"github"}, nil))
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("lowercased opt-in = %v", got)
	}
	chosen, _ = selectRemoteSeats(conns, RemoteMCPEnabledOnly([]string{"gamma"}, map[string]string{"gamma": "team"}))
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"gamma-team"}) {
		t.Fatalf("filtered pin = %v", got)
	}
}

func TestSelectRemoteSeatsNeverSubstitutesForAMissingPin(t *testing.T) {
	conns := seatFixture()
	chosen, missing := selectRemoteSeats(conns, RemoteMCPSelection{Accounts: map[string]string{"github": "school"}})
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"gamma-legacy"}) {
		t.Fatalf("a missing pin must drop the NAME, not fall back: chosen %v", got)
	}
	if !reflect.DeepEqual(missing, []string{"github_school"}) {
		t.Fatalf("missing = %v, want the registered name of the pin", missing)
	}
}

func TestSelectRemoteSeatsExactPinsTheUnlabeledSeat(t *testing.T) {
	conns := []RemoteMCPConn{
		{ID: "legacy", Name: "gamma", Account: ""},
		{ID: "team", Name: "gamma", Account: "team", Default: true},
	}
	// Non-exact "": the default (team) — what a picker's "Default" means.
	chosen, _ := selectRemoteSeats(conns, RemoteMCPSelection{Accounts: map[string]string{"gamma": ""}})
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"team"}) {
		t.Fatalf("non-exact empty = %v, want the default seat", got)
	}
	// Exact "": the unlabeled seat itself — what approval re-execution
	// recorded, even though the default has since moved to "team".
	chosen, missing := selectRemoteSeats(conns, RemoteMCPSelection{Filter: true, Enabled: map[string]bool{"gamma": true}, Accounts: map[string]string{"gamma": ""}, Exact: true})
	if got := chosenIDs(chosen); !reflect.DeepEqual(got, []string{"legacy"}) || len(missing) != 0 {
		t.Fatalf("exact empty = %v missing %v, want the unlabeled seat", got, missing)
	}
	// Exact with a label nobody holds is missing, not defaulted.
	_, missing = selectRemoteSeats(conns, RemoteMCPSelection{Filter: true, Enabled: map[string]bool{"gamma": true}, Accounts: map[string]string{"gamma": "gone"}, Exact: true})
	if !reflect.DeepEqual(missing, []string{"gamma_gone"}) {
		t.Fatalf("exact missing = %v", missing)
	}
}

func TestDefaultRemoteSeatFallsBackToUnlabeledThenFirst(t *testing.T) {
	// No Default flag anywhere (e.g. a grantee sees only shared, non-default
	// seats): the unlabeled seat wins, else the first.
	seats := []RemoteMCPConn{{ID: "b", Account: "b"}, {ID: "plain", Account: ""}}
	if got := defaultRemoteSeat(seats).ID; got != "plain" {
		t.Fatalf("default = %s, want the unlabeled seat", got)
	}
	seats = []RemoteMCPConn{{ID: "b", Account: "b"}, {ID: "c", Account: "c"}}
	if got := defaultRemoteSeat(seats).ID; got != "b" {
		t.Fatalf("default = %s, want the first seat", got)
	}
}

func TestGroupRemoteMCPSeats(t *testing.T) {
	groups := GroupRemoteMCPSeats(append(seatFixture(), RemoteMCPConn{ID: "s", Name: "shared", Account: "x", Default: true, Owner: "o@x.com"}))
	if len(groups) != 3 {
		t.Fatalf("groups = %+v", groups)
	}
	gh, gamma, shared := groups[0], groups[1], groups[2]
	if gh.Name != "github" || !reflect.DeepEqual(gh.Accounts, []string{"personal", "work"}) || gh.DefaultAccount != "work" || gh.URL != "https://gh/work" || gh.Owner != "" {
		t.Fatalf("github group = %+v", gh)
	}
	// The unlabeled seat is not a pickable label; it is reachable as default.
	if gamma.Name != "gamma" || !reflect.DeepEqual(gamma.Accounts, []string{"team"}) || gamma.DefaultAccount != "" {
		t.Fatalf("gamma group = %+v", gamma)
	}
	if shared.Owner != "o@x.com" || shared.DefaultAccount != "x" {
		t.Fatalf("shared group = %+v", shared)
	}
	if got := GroupRemoteMCPSeats(nil); len(got) != 0 {
		t.Fatalf("nil conns grouped to %+v", got)
	}
}

// BuildRemoteMCPOverlay must register a labeled seat under the bundle seat
// formula (name_account), record the seat for approval staging, and report a
// pinned-but-unconnected seat as skipped WITHOUT touching another seat's token.
func TestBuildRemoteMCPOverlaySeatRegistrationAndMissingPin(t *testing.T) {
	ctx := context.Background()
	r := &fakeResolver{
		conns: []RemoteMCPConn{
			{ID: "work", Name: "github", Account: "work", Default: true, URL: "https://gh.example.com"},
			{ID: "personal", Name: "github", Account: "personal", URL: "https://gh.example.com"},
		},
		// Token fetch fails so nothing dials; the seat that was ATTEMPTED is
		// observable through asked/Skipped.
		tokenErr: map[string]error{"work": errors.New("x"), "personal": errors.New("x")},
	}
	ov, err := BuildRemoteMCPOverlay(ctx, r, "u@x.com", nil, RemoteMCPEnabledOnly([]string{"github"}, map[string]string{"github": "personal"}))
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov.Close()
	if !reflect.DeepEqual(r.asked, []string{"personal"}) {
		t.Fatalf("asked = %v, want only the pinned seat's token", r.asked)
	}
	if !reflect.DeepEqual(ov.Skipped, []string{"github_personal"}) {
		t.Fatalf("Skipped = %v, want the registered seat name", ov.Skipped)
	}

	r2 := &fakeResolver{conns: r.conns, tokenErr: map[string]error{"work": errors.New("x")}}
	ov2, err := BuildRemoteMCPOverlay(ctx, r2, "u@x.com", nil, RemoteMCPSelection{Accounts: map[string]string{"github": "school"}})
	if err != nil {
		t.Fatalf("BuildRemoteMCPOverlay: %v", err)
	}
	defer ov2.Close()
	if len(r2.asked) != 0 {
		t.Fatalf("a missing pin minted a token for another seat: asked=%v", r2.asked)
	}
	if !reflect.DeepEqual(ov2.Skipped, []string{"github_school"}) {
		t.Fatalf("Skipped = %v", ov2.Skipped)
	}
}

func TestRemoteMCPOverlaySeatSelectionAndComposeWith(t *testing.T) {
	ov := &RemoteMCPOverlay{
		Broker:     &recordingBroker{},
		Servers:    map[string]bool{"github_work": true, "gamma": true},
		Seats:      map[string]agentcore.MCPChoice{"github_work": {Server: "github", Account: "work"}, "gamma": {Server: "gamma"}},
		Catalog:    []mcp.ServerTool{{ServerName: "github_work", Tool: mcp.Tool{Name: "search"}}},
		CloseScope: func(context.Context) error { return nil },
	}
	if got := ov.SeatSelection(); !reflect.DeepEqual(got, agentcore.MCPSelection{{Server: "gamma"}, {Server: "github", Account: "work"}}) {
		t.Fatalf("SeatSelection = %+v", got)
	}
	base := &recordingBroker{}
	broker, catalog := ov.ComposeWith(base, []mcp.ServerTool{{ServerName: "bundle", Tool: mcp.Tool{Name: "t"}}})
	if len(catalog) != 2 || catalog[0].ServerName != "bundle" || catalog[1].ServerName != "github_work" {
		t.Fatalf("composed catalog = %+v", catalog)
	}
	if _, ok := broker.(*compositeBroker); !ok {
		t.Fatalf("composed broker = %T, want compositeBroker", broker)
	}
	// Inactive overlay: base passes through untouched.
	var none *RemoteMCPOverlay
	if b, c := none.ComposeWith(base, nil); b != base || c != nil {
		t.Fatalf("inactive ComposeWith changed the base: %v %v", b, c)
	}
	if none.SeatSelection() != nil {
		t.Fatal("nil overlay produced a seat selection")
	}
}
