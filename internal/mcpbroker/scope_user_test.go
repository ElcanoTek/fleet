package mcpbroker

import (
	"context"
	"strings"
	"testing"
)

// TestClientServer_ScopeUserCrossesToBackend pins the wire half of ADR-0087:
// an interactive scope's signed-in user reaches the credential owner intact,
// beside the selection and workspace it is substituted into.
func TestClientServer_ScopeUserCrossesToBackend(t *testing.T) {
	fake := &fakeScopedBroker{fakeBroker: &fakeBroker{}, scopeID: "scope-user-1"}
	client := loopback(t, fake)
	spec := ScopeSpec{
		Selection: []ScopeChoice{{Server: "deal_sheet"}},
		Workspace: "/srv/fleet/workspace/conv-1",
		User:      &ScopeUser{Email: "trader@example.com", Name: "Trader"},
	}
	scope, err := client.OpenScope(context.Background(), spec)
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	fake.scopeMu.Lock()
	got := fake.openSpec
	fake.scopeMu.Unlock()
	if got.User == nil || *got.User != *spec.User {
		t.Fatalf("backend user = %+v, want %+v", got.User, spec.User)
	}
}

// TestClientServer_ScopeUserRefusedOnTaskAndRemoteScopes: a scheduled scope
// (TaskID) or a remote scope that carries a signed-in user is a parent bug —
// refused before the backend sees it, so a task run is never handed a
// person's identity.
func TestClientServer_ScopeUserRefusedOnTaskAndRemoteScopes(t *testing.T) {
	user := &ScopeUser{Email: "trader@example.com", Name: "Trader"}
	tests := []struct {
		name string
		spec ScopeSpec
		want string
	}{
		{
			name: "task scope",
			spec: ScopeSpec{Selection: []ScopeChoice{{Server: "deal_sheet"}}, TaskID: "task-1", User: user},
			want: "scheduled (task) scope cannot carry a signed-in user",
		},
		{
			name: "remote scope",
			spec: ScopeSpec{Remote: &RemoteScopeSpec{UserEmail: "trader@example.com"}, User: user},
			want: "remote scope cannot include bundle scope fields",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeScopedBroker{fakeBroker: &fakeBroker{}, scopeID: "must-not-open"}
			client := loopback(t, fake)
			_, err := client.OpenScope(context.Background(), tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("OpenScope error = %v, want %q", err, tt.want)
			}
			fake.scopeMu.Lock()
			got := fake.openSpec
			fake.scopeMu.Unlock()
			if got.User != nil || got.Remote != nil || got.TaskID != "" {
				t.Fatalf("invalid scope reached backend: %+v", got)
			}
		})
	}
}
