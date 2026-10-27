package scheduledrun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// strangerID is a user with an email and no hosted connections; any other id
// that is not the owner resolves to no email (a deleted user).
var strangerID = uuid.New()

// selectionCheckRunner is a remote-capable runner with one bundle server
// ("bundle") whose owner (owner@example.com) has connected "browserbase" on
// its default seat and "github" on a labelled seat.
func selectionCheckRunner(t *testing.T, ownerID uuid.UUID) *Runner {
	t.Helper()
	return &Runner{
		mcpServerInventory: func() map[string]TaskMCPServerInfo {
			return map[string]TaskMCPServerInfo{"bundle": {Optional: true}}
		},
		ownerEmail: func(_ context.Context, id uuid.UUID) (string, error) {
			switch id {
			case ownerID:
				return "owner@example.com", nil
			case strangerID:
				return "stranger@example.com", nil
			}
			return "", nil
		},
		openRemoteMCPOverlay: func(context.Context, string, map[string]bool, agent.RemoteMCPSelection) (*agent.RemoteMCPOverlay, error) {
			t.Fatal("the create-time check must not open an overlay (it would mint tokens and dial)")
			return nil, nil
		},
		hostedConnections: func(_ context.Context, email string) ([]agent.RemoteMCPConn, error) {
			if email != "owner@example.com" {
				return nil, nil
			}
			return []agent.RemoteMCPConn{
				{ID: "c1", Name: "browserbase", Default: true},
				{ID: "c2", Name: "github", Account: "work"},
			}, nil
		},
	}
}

func TestCheckTaskMCPSelection(t *testing.T) {
	ownerID := uuid.New()
	stranger := strangerID
	deleted := uuid.New()
	sel := func(choices ...models.MCPChoice) models.MCPSelection { return choices }

	cases := []struct {
		name    string
		task    *models.Task
		wantErr string // "" = accepted
	}{
		{name: "no selection", task: &models.Task{CreatedBy: &ownerID}},
		{name: "bundle server", task: &models.Task{CreatedBy: &ownerID, MCPSelection: sel(models.MCPChoice{Server: "bundle"})}},
		{name: "owner's hosted connection", task: &models.Task{CreatedBy: &ownerID, MCPSelection: sel(models.MCPChoice{Server: "browserbase"})}},
		{name: "owner's labelled seat", task: &models.Task{CreatedBy: &ownerID, MCPSelection: sel(models.MCPChoice{Server: "github", Account: "work"})}},
		// A pin to a seat that is not connected under a connection the owner
		// has: the run mounts nothing for it and says so in a notice — it is
		// not an unknown server, so a create must not refuse it either.
		{name: "owner's connection, unconnected seat", task: &models.Task{CreatedBy: &ownerID, MCPSelection: sel(models.MCPChoice{Server: "browserbase", Account: "other"})}},
		{
			name:    "ownerless copy pinning a hosted connection (prod 2026-10-26)",
			task:    &models.Task{MCPSelection: sel(models.MCPChoice{Server: "browserbase"})},
			wantErr: "mcp selection references server(s) [browserbase] which are neither in the active bundle catalog nor among the task owner's hosted connections",
		},
		{
			name:    "owner without that connection",
			task:    &models.Task{CreatedBy: &stranger, MCPSelection: sel(models.MCPChoice{Server: "browserbase"})},
			wantErr: "[browserbase] which are neither in the active bundle catalog",
		},
		{
			name:    "owner that no longer resolves to an email",
			task:    &models.Task{CreatedBy: &deleted, MCPSelection: sel(models.MCPChoice{Server: "browserbase"})},
			wantErr: "[browserbase]",
		},
		{
			name:    "misspelled server",
			task:    &models.Task{CreatedBy: &ownerID, MCPSelection: sel(models.MCPChoice{Server: "bundle"}, models.MCPChoice{Server: "browserbsae"})},
			wantErr: "[browserbsae]",
		},
		// A deny-all task binds no server at all, so an unknown pin is moot —
		// unless the task has no owner, where the run refuses before it ever
		// looks at the allowlist (buildTaskRemoteOverlay's guard order).
		{name: "deny-all with an owner", task: &models.Task{CreatedBy: &stranger, CredentialAllowlist: models.CredentialAllowlist{}, MCPSelection: sel(models.MCPChoice{Server: "browserbase"})}},
		{
			name:    "deny-all without an owner",
			task:    &models.Task{CredentialAllowlist: models.CredentialAllowlist{}, MCPSelection: sel(models.MCPChoice{Server: "browserbase"})},
			wantErr: "[browserbase]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := selectionCheckRunner(t, ownerID).CheckTaskMCPSelection(context.Background(), c.task)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}

// TestCheckTaskMCPSelection_MatchesTheRun pins that the create-time check is
// the run's refusal, word for word: for the incident shape (an ownerless task
// pinning a hosted connection) and for an owner who lacks the connection, the
// error a run gets from buildTaskRemoteOverlay is the error a create gets.
func TestCheckTaskMCPSelection_MatchesTheRun(t *testing.T) {
	ownerID := uuid.New()
	stranger := strangerID
	for name, owner := range map[string]*uuid.UUID{"ownerless": nil, "owner lacks it": &stranger} {
		t.Run(name, func(t *testing.T) {
			task := &models.Task{ID: uuid.New(), CreatedBy: owner, MCPSelection: models.MCPSelection{{Server: "browserbase"}}}
			r := selectionCheckRunner(t, ownerID)
			checkErr := r.CheckTaskMCPSelection(context.Background(), task)
			// The run builds the owner's overlay; an owner with no connections
			// gets an empty one.
			r.openRemoteMCPOverlay = func(context.Context, string, map[string]bool, agent.RemoteMCPSelection) (*agent.RemoteMCPOverlay, error) {
				return &agent.RemoteMCPOverlay{Servers: map[string]bool{}}, nil
			}
			_, runErr := r.buildTaskRemoteOverlay(context.Background(), task, nil)
			if checkErr == nil || runErr == nil || checkErr.Error() != runErr.Error() {
				t.Fatalf("create-time check = %v\nrun = %v\nwant the same refusal", checkErr, runErr)
			}
		})
	}
}

// TestCheckTaskMCPSelection_InfrastructureErrorsDoNotRefuse: only a definite
// refusal is returned. A failed owner lookup lets the run proceed without its
// hosted connections, and a failed connection listing is the run's to report,
// so neither may turn a create into a 400.
func TestCheckTaskMCPSelection_InfrastructureErrorsDoNotRefuse(t *testing.T) {
	ownerID := uuid.New()
	task := &models.Task{CreatedBy: &ownerID, MCPSelection: models.MCPSelection{{Server: "unknown-to-everyone"}}}

	r := selectionCheckRunner(t, ownerID)
	r.ownerEmail = func(context.Context, uuid.UUID) (string, error) { return "", errors.New("db down") }
	if err := r.CheckTaskMCPSelection(context.Background(), task); err != nil {
		t.Fatalf("owner lookup failure refused the create: %v", err)
	}

	r = selectionCheckRunner(t, ownerID)
	r.hostedConnections = func(context.Context, string) ([]agent.RemoteMCPConn, error) {
		return nil, errors.New("chat store down")
	}
	if err := r.CheckTaskMCPSelection(context.Background(), task); err != nil {
		t.Fatalf("connection listing failure refused the create: %v", err)
	}

	r = selectionCheckRunner(t, ownerID)
	r.hostedConnections = nil // nothing to list with: cannot know, so let the run decide
	if err := r.CheckTaskMCPSelection(context.Background(), task); err != nil {
		t.Fatalf("no lister refused the create: %v", err)
	}
}

// TestCheckTaskMCPSelection_NoHostedConnections: on a deployment without hosted
// connections every name goes to the bundle binder, which refuses an unknown
// server — so the create refuses with the binder's message.
func TestCheckTaskMCPSelection_NoHostedConnections(t *testing.T) {
	ownerID := uuid.New()
	r := selectionCheckRunner(t, ownerID)
	r.openRemoteMCPOverlay = nil
	r.hostedConnections = nil

	ok := &models.Task{CreatedBy: &ownerID, MCPSelection: models.MCPSelection{{Server: "bundle", Account: "client_a"}}}
	if err := r.CheckTaskMCPSelection(context.Background(), ok); err != nil {
		t.Fatalf("bundle server refused: %v", err)
	}
	bad := &models.Task{CreatedBy: &ownerID, MCPSelection: models.MCPSelection{{Server: "browserbase"}}}
	err := r.CheckTaskMCPSelection(context.Background(), bad)
	if err == nil || !strings.Contains(err.Error(), `server "browserbase" which is not in the active catalog`) {
		t.Fatalf("err = %v, want the binder's unknown-server refusal", err)
	}
}
