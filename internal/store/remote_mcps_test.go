package store

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/secretbox"
)

func newTestStoreWithCipher(t testing.TB) *Store {
	t.Helper()
	s := newTestStore(t)
	key := make([]byte, secretbox.KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("key: %v", err)
	}
	c, err := secretbox.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetTokenCipher(c)
	return s
}

func sampleServerInput(email string) RemoteMCPServerInput {
	return RemoteMCPServerInput{
		UserEmail:             email,
		Name:                  "acme",
		URL:                   "https://mcp.acme.com",
		Transport:             RemoteMCPTransportStreamableHTTP,
		Issuer:                "https://auth.acme.com",
		AuthorizationEndpoint: "https://auth.acme.com/authorize",
		TokenEndpoint:         "https://auth.acme.com/token",
		Scopes:                "mcp:read mcp:write",
		ClientID:              "client-1",
		ClientSecret:          "shh-secret",
		RegistrationToken:     "reg-tok",
	}
}

func TestRemoteMCPCreateListGetDelete(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const email = "Brad@Elcano.com"

	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(email))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if srv.Status != RemoteMCPStatusLoginRequired {
		t.Errorf("status = %q", srv.Status)
	}
	if srv.UserEmail != "brad@elcano.com" {
		t.Errorf("email not normalized: %q", srv.UserEmail)
	}

	// Duplicate name rejected.
	if _, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(email)); err == nil {
		t.Error("expected duplicate (user,name) to fail")
	}

	list, err := s.ListRemoteMCPServers(ctx, "brad@elcano.com")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d servers, err %v", len(list), err)
	}

	// Another user cannot see it.
	other, _ := s.ListRemoteMCPServers(ctx, "someone@else.com")
	if len(other) != 0 {
		t.Errorf("cross-user leak: %d servers", len(other))
	}
	if _, err := s.GetRemoteMCPServer(ctx, "someone@else.com", srv.ID); !errors.Is(err, ErrRemoteMCPNotFound) {
		t.Errorf("cross-user Get err = %v", err)
	}

	// Secrets decrypt back.
	secret, regTok, err := s.LoadServerSecrets(ctx, srv)
	if err != nil {
		t.Fatalf("LoadServerSecrets: %v", err)
	}
	if secret != "shh-secret" || regTok != "reg-tok" {
		t.Errorf("secrets = %q,%q", secret, regTok)
	}

	if err := s.DeleteRemoteMCPServer(ctx, "brad@elcano.com", srv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteRemoteMCPServer(ctx, "brad@elcano.com", srv.ID); !errors.Is(err, ErrRemoteMCPNotFound) {
		t.Errorf("second delete err = %v", err)
	}
}

func TestRemoteMCPTokenLifecycle(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	future := time.Now().Add(time.Hour).Unix()
	if err := s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: future}); err != nil {
		t.Fatalf("store tokens: %v", err)
	}
	srv, _ = s.GetRemoteMCPServer(ctx, "u@x.com", srv.ID)
	if srv.Status != RemoteMCPStatusConnected {
		t.Errorf("status after store = %q", srv.Status)
	}

	// Fresh token → no refresh.
	called := false
	at, err := s.EnsureFreshToken(ctx, srv, 300, func(context.Context, RemoteMCPTokens) (RefreshResult, error) {
		called = true
		return RefreshResult{}, nil
	})
	if err != nil || at != "at-1" {
		t.Fatalf("EnsureFreshToken(fresh) = %q, %v", at, err)
	}
	if called {
		t.Error("refresh called for a fresh token")
	}

	// Near-expiry → refresh, rotation persisted.
	if err := s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(60 * time.Second).Unix()}); err != nil {
		t.Fatalf("store near-expiry: %v", err)
	}
	at, err = s.EnsureFreshToken(ctx, srv, 300, func(_ context.Context, cur RemoteMCPTokens) (RefreshResult, error) {
		if cur.RefreshToken != "rt-1" {
			t.Errorf("refreshFn got refresh token %q", cur.RefreshToken)
		}
		return RefreshResult{Tokens: RemoteMCPTokens{AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, nil
	})
	if err != nil || at != "at-2" {
		t.Fatalf("EnsureFreshToken(refresh) = %q, %v", at, err)
	}
	// The rotated refresh token must be what a subsequent refresh sees. Re-store a
	// near-expiry pair carrying the rotated rt-2 so the next EnsureFreshToken refreshes.
	if err := s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: time.Now().Add(30 * time.Second).Unix()}); err != nil {
		t.Fatalf("re-store rotated tokens: %v", err)
	}
	_, _ = s.EnsureFreshToken(ctx, srv, 300, func(_ context.Context, cur RemoteMCPTokens) (RefreshResult, error) {
		if cur.RefreshToken != "rt-2" {
			t.Errorf("after rotation, refreshFn got %q, want rt-2", cur.RefreshToken)
		}
		return RefreshResult{Tokens: RemoteMCPTokens{AccessToken: "at-3", RefreshToken: "rt-3", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, nil
	})
}

func TestEnsureFreshTokenNeedsReauth(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	_ = s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(10 * time.Second).Unix()})

	_, err := s.EnsureFreshToken(ctx, srv, 300, func(context.Context, RemoteMCPTokens) (RefreshResult, error) {
		return RefreshResult{NeedReauth: true}, nil
	})
	if !errors.Is(err, ErrRemoteMCPNeedsReauth) {
		t.Fatalf("err = %v, want ErrRemoteMCPNeedsReauth", err)
	}
	srv, _ = s.GetRemoteMCPServer(ctx, "u@x.com", srv.ID)
	if srv.Status != RemoteMCPStatusNeedsReauth {
		t.Errorf("status = %q, want needs_reauth", srv.Status)
	}
	// An unclassified refresh keeps the generic wording.
	if srv.StatusDetail != "authorization expired — reconnect required" {
		t.Errorf("detail = %q, want the generic fallback", srv.StatusDetail)
	}
}

// A RefreshFunc that classified the failure supplies its own user-facing reason,
// so the connections UI names the real cause instead of claiming expiry for a
// client-registration problem.
func TestEnsureFreshTokenNeedsReauthCarriesDetail(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	_ = s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(10 * time.Second).Unix()})

	const detail = "the authorization server no longer recognizes this client — reconnect required"
	_, err := s.EnsureFreshToken(ctx, srv, 300, func(context.Context, RemoteMCPTokens) (RefreshResult, error) {
		return RefreshResult{NeedReauth: true, ReauthDetail: detail}, nil
	})
	if !errors.Is(err, ErrRemoteMCPNeedsReauth) {
		t.Fatalf("err = %v, want ErrRemoteMCPNeedsReauth", err)
	}
	srv, _ = s.GetRemoteMCPServer(ctx, "u@x.com", srv.ID)
	if srv.Status != RemoteMCPStatusNeedsReauth {
		t.Errorf("status = %q, want needs_reauth", srv.Status)
	}
	if srv.StatusDetail != detail {
		t.Errorf("detail = %q, want %q", srv.StatusDetail, detail)
	}
}

func TestEnsureFreshTokenNoExpiryUsable(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	// A long-lived access token the AS issued with NO expires_in and NO refresh
	// token. It must be returned as-is, never forced into needs_reauth or rotated.
	if err := s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{AccessToken: "long-lived", ExpiresAt: 0}); err != nil {
		t.Fatalf("store: %v", err)
	}
	called := false
	at, err := s.EnsureFreshToken(ctx, srv, 300, func(context.Context, RemoteMCPTokens) (RefreshResult, error) {
		called = true
		return RefreshResult{}, nil
	})
	if err != nil || at != "long-lived" {
		t.Fatalf("no-expiry token = %q, %v (want usable)", at, err)
	}
	if called {
		t.Error("a no-expiry token must not trigger a refresh")
	}
}

func TestEnsureFreshTokenNeverAuthorized(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	_, err := s.EnsureFreshToken(ctx, srv, 300, func(context.Context, RemoteMCPTokens) (RefreshResult, error) {
		t.Fatal("refresh should not be called when no token row exists")
		return RefreshResult{}, nil
	})
	if !errors.Is(err, ErrRemoteMCPNeedsReauth) {
		t.Fatalf("err = %v, want ErrRemoteMCPNeedsReauth", err)
	}
}

func TestOAuthFlowSingleUse(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))

	if err := s.BeginOAuthFlow(ctx, "state-1", srv.ID, "u@x.com", "verifier-1", 10*time.Minute); err != nil {
		t.Fatalf("begin flow: %v", err)
	}
	got, err := s.ConsumeOAuthFlow(ctx, "state-1")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got.CodeVerifier != "verifier-1" || got.ServerID != srv.ID || got.UserEmail != "u@x.com" {
		t.Errorf("consumed = %+v", got)
	}
	// Replay must fail (single-use).
	if _, err := s.ConsumeOAuthFlow(ctx, "state-1"); !errors.Is(err, ErrOAuthFlowNotFound) {
		t.Errorf("replay err = %v, want ErrOAuthFlowNotFound", err)
	}
}

func TestOAuthFlowExpiry(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	srv, _ := s.CreateRemoteMCPServer(ctx, sampleServerInput("u@x.com"))
	// Negative TTL → already expired.
	if err := s.BeginOAuthFlow(ctx, "state-exp", srv.ID, "u@x.com", "v", -time.Minute); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ConsumeOAuthFlow(ctx, "state-exp"); !errors.Is(err, ErrOAuthFlowNotFound) {
		t.Errorf("expired consume err = %v", err)
	}
	if _, err := s.SweepExpiredOAuthFlows(ctx); err != nil {
		t.Errorf("sweep: %v", err)
	}
}

func TestRemoteMCPNoCipherFailsClosed(t *testing.T) {
	s := newTestStore(t) // no cipher set
	if s.RemoteMCPEncryptionEnabled() {
		t.Fatal("expected encryption disabled without a cipher")
	}
	_, err := s.CreateRemoteMCPServer(context.Background(), sampleServerInput("u@x.com"))
	if !errors.Is(err, secretbox.ErrNoCipher) {
		t.Fatalf("create without cipher err = %v, want ErrNoCipher", err)
	}
}

// Sign out (#owner request): ClearOAuthTokens drops the token row and marks
// needs_reauth, but the registration — including the sealed client secret a
// manual OAuth app registration depends on — must survive so Reconnect works
// without re-entering credentials.
func TestClearOAuthTokensKeepsRegistration(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()
	const email = "user@elcano.com"

	srv, err := s.CreateRemoteMCPServer(ctx, sampleServerInput(email))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.StoreOAuthTokens(ctx, srv, RemoteMCPTokens{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("store tokens: %v", err)
	}

	if err := s.ClearOAuthTokens(ctx, email, srv.ID); err != nil {
		t.Fatalf("ClearOAuthTokens: %v", err)
	}

	got, err := s.GetRemoteMCPServer(ctx, email, srv.ID)
	if err != nil {
		t.Fatalf("get after sign out: %v", err)
	}
	if got.Status != RemoteMCPStatusNeedsReauth {
		t.Errorf("status = %q, want %q", got.Status, RemoteMCPStatusNeedsReauth)
	}
	if _, terr := s.GetOAuthTokens(ctx, got); !errors.Is(terr, ErrRemoteMCPNeedsReauth) {
		t.Errorf("tokens after sign out: err = %v, want ErrRemoteMCPNeedsReauth", terr)
	}
	// The sealed client secret survives — Reconnect must not re-prompt for it.
	secret, _, serr := s.LoadServerSecrets(ctx, got)
	if serr != nil || secret != "shh-secret" {
		t.Errorf("client secret after sign out = %q, %v; want preserved", secret, serr)
	}

	// Unknown id / wrong owner both report not-found.
	if err := s.ClearOAuthTokens(ctx, "other@elcano.com", srv.ID); !errors.Is(err, ErrRemoteMCPNotFound) {
		t.Errorf("wrong owner: err = %v, want ErrRemoteMCPNotFound", err)
	}
}
