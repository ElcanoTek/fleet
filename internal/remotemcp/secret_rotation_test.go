// #1274: the secret observer's SCOPE/ROTATION contract, and the control-plane
// acquisition sites (#1124 covered only the data-plane refresh path).
//
// Every credential-shaped string here is an obviously fake placeholder; the
// fake authorization server's tokens ("at-init", "rt-rotated", …) are the
// existing test fixtures. Nothing in this package may carry a real credential.
package remotemcp

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// secretCall is one observer invocation, recorded for assertion.
type secretCall struct {
	scope   string
	rotated bool
	secrets []string
}

type secretRecorder struct {
	mu    sync.Mutex
	calls []secretCall
}

func (r *secretRecorder) observe(scope string, rotated bool, secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, secretCall{scope: scope, rotated: rotated, secrets: slices.Clone(secrets)})
}

func (r *secretRecorder) snapshot() []secretCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// callsWith returns every recorded call that carried value.
func callsWith(calls []secretCall, value string) []secretCall {
	var out []secretCall
	for _, c := range calls {
		if slices.Contains(c.secrets, value) {
			out = append(out, c)
		}
	}
	return out
}

// TestServiceSecretObserverScopesTokenRotations pins the contract the bounded
// redactor depends on (#1274): every OAuth credential is offered under the
// SERVER ROW's scope, a successful mint/rotation is reported with rotated=true
// carrying the row's COMPLETE live set (so the client secret survives the
// generation swap), and the pre-request registrations that merely put a
// credential in play are joins, not rotations. Get the completeness wrong and
// the redactor would start a grace clock on a live secret.
func TestServiceSecretObserverScopesTokenRotations(t *testing.T) {
	fs := newFakeStore()
	srv := oauthTestServer(t, "rotate")
	svc := newTestService(t, fs, srv)
	ctx := context.Background()
	fs.clientSecret = "placeholder-client-secret-fixture"

	rec := &secretRecorder{}
	svc.SetSecretObserver(rec.observe)

	server, _, err := svc.AddServer(ctx, AddServerInput{Email: "u@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if _, err := svc.Authorize(ctx, "u@x.com", server.ID); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	var state string
	for k := range fs.flows {
		state = k
	}
	const code = "placeholder-authorization-code"
	if _, err := svc.Complete(ctx, "u@x.com", state, code); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // let the 1s access token go stale
	server, _ = svc.store.GetRemoteMCPServer(ctx, "u@x.com", server.ID)
	if bearer, err := svc.AcquireToken(ctx, server); err != nil || bearer != "at-refreshed" {
		t.Fatalf("AcquireToken = %q, %v; want at-refreshed", bearer, err)
	}

	calls := rec.snapshot()
	scope := "remotemcp:" + server.ID

	// Every OAuth credential is offered under this row's scope — never
	// unscoped (which would retain it forever) and never another row's.
	for _, value := range []string{fs.clientSecret, code, "rt-init", "at-refreshed", "rt-rotated"} {
		got := callsWith(calls, value)
		if len(got) == 0 {
			t.Errorf("observer never saw %q", value)
			continue
		}
		for _, c := range got {
			if c.scope != scope {
				t.Errorf("value %q registered under scope %q, want %q", value, c.scope, scope)
			}
		}
	}

	// Two mints happened: the code exchange and the refresh. Each must report
	// the row's whole live set.
	var rotations []secretCall
	for _, c := range calls {
		if c.rotated {
			rotations = append(rotations, c)
		}
	}
	if len(rotations) != 2 {
		t.Fatalf("rotated registrations = %d, want 2 (code exchange + refresh); calls: %+v", len(rotations), calls)
	}
	for i, want := range [][]string{
		{fs.clientSecret, "at-init", "rt-init"},
		{fs.clientSecret, "at-refreshed", "rt-rotated"},
	} {
		for _, v := range want {
			if !slices.Contains(rotations[i].secrets, v) {
				t.Errorf("rotation %d omitted the live secret %q — the redactor would retire it while it is still in use (got %v)", i, v, rotations[i].secrets)
			}
		}
		if rotations[i].scope != scope {
			t.Errorf("rotation %d scope = %q, want %q", i, rotations[i].scope, scope)
		}
	}

	// The stored refresh token about to ride the token request is registered on
	// its own as a JOIN: it stays live until the rotation that consumes it
	// succeeds, so this call must not itself open a generation.
	preRequest := false
	for _, c := range calls {
		if slices.Equal(c.secrets, []string{"rt-init"}) {
			preRequest = true
			if c.rotated {
				t.Error("the pre-request registration of the stored refresh token must be a join, not a rotation")
			}
		}
	}
	if !preRequest {
		t.Error("the stored refresh token was never registered before it rode the token request")
	}
	// The spent authorization code is only ever a join, so the successful
	// exchange supersedes it and it retires after the grace window.
	for _, c := range callsWith(calls, code) {
		if c.rotated {
			t.Error("the authorization code must never be part of a rotation's live set")
		}
	}
}

// TestServiceControlPlaneAPIKeyAcquisitionsAreObserved covers the other half of
// #1274's coverage gap: the add-time probe and the key-rotation probe both send
// a user-supplied key upstream and relay the vendor's failure text, so the key
// must reach the observer BEFORE the probe runs. api_keys do not rotate on a
// clock, so they are registered unscoped (permanent).
func TestServiceControlPlaneAPIKeyAcquisitionsAreObserved(t *testing.T) {
	fs := newFakeStore()
	sawDiscovery := false
	const goodKey = "placeholder-vendor-api-key-one"
	const rotatedKey = "placeholder-vendor-api-key-two"
	const badKey = "placeholder-vendor-api-key-wrong"
	srv := mcpProbeServer(t, 3, "X-API-Key", map[string]bool{goodKey: true, rotatedKey: true}, &sawDiscovery)
	defer srv.Close()
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	rec := &secretRecorder{}
	svc.SetSecretObserver(rec.observe)

	// A REJECTED key is the important case: its probe error is wrapped and
	// relayed to the caller, so it has to be registered even though nothing is
	// stored.
	if _, _, err := svc.AddServer(ctx, AddServerInput{
		Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key",
		APIKey: badKey, APIKeyHeader: "X-API-Key",
	}); err == nil {
		t.Fatal("AddServer(api_key) accepted a key the vendor rejects")
	}
	server, _, err := svc.AddServer(ctx, AddServerInput{
		Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key",
		APIKey: goodKey, APIKeyHeader: "X-API-Key",
	})
	if err != nil {
		t.Fatalf("AddServer(api_key): %v", err)
	}
	if _, err := svc.SetAPIKey(ctx, "u@x.com", server.ID, rotatedKey); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}

	calls := rec.snapshot()
	for _, key := range []string{badKey, goodKey, rotatedKey} {
		got := callsWith(calls, key)
		if len(got) == 0 {
			t.Errorf("observer never saw the api_key %q — its probe error path is uncovered", key)
			continue
		}
		for _, c := range got {
			if c.scope != "" || c.rotated {
				t.Errorf("api_key %q registered as scope=%q rotated=%v, want an unscoped permanent literal", key, c.scope, c.rotated)
			}
		}
	}
}
