package admincli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// `fleet sched apikey create --type user --rate-limit-per-minute N` is
// refused: a key's RateLimit is an hourly budget charged on every request,
// which would count `fleet chat`'s identity and config reads rather than
// turns. A user key's turns are limited by the chat's per-user limit.
func TestUserKeyRefusesARateLimit(t *testing.T) {
	dir := t.TempDir()
	mgr, err := apikeys.NewManager(filepath.Join(dir, "api_keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if code := createUserKey(mgr, keyStore{}, "cli", "u@x.com", 1, foundAccount("acct-1")); code == 0 {
		t.Fatal("a user key was minted with a per-key rate limit")
	}
	if n := len(mgr.ListKeys()); n != 0 {
		t.Fatalf("%d keys minted", n)
	}
	if code := createUserKey(mgr, keyStore{}, "cli", "", 0, foundAccount("acct-1")); code == 0 {
		t.Fatal("a user key was minted with no account")
	}
}

// foundAccount is an account lookup that finds every email as id.
func foundAccount(id string) apikeys.AccountLookup {
	return func(context.Context, string) (string, bool, error) { return id, true, nil }
}

// A user key is bound to the account it acts as: minting one names the
// account's id, and an address with no account is refused with what to do.
func TestUserKeyIsBoundToItsAccount(t *testing.T) {
	dir := t.TempDir()
	mgr, err := apikeys.NewManager(filepath.Join(dir, "api_keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	none := func(context.Context, string) (string, bool, error) { return "", false, nil }
	if code := createUserKey(mgr, keyStore{}, "cli", "nobody@x.com", 0, none); code == 0 {
		t.Fatal("a user key was minted for an address with no account")
	}
	if n := len(mgr.ListKeys()); n != 0 {
		t.Fatalf("%d keys minted for no account", n)
	}
	if code := createUserKey(mgr, keyStore{}, "cli", "u@x.com", 0, foundAccount("acct-1")); code != 0 {
		t.Fatalf("exit %d", code)
	}
	keys := mgr.ListKeys()
	if len(keys) != 1 || keys[0].AccountID != "acct-1" || keys[0].UserEmail != "u@x.com" {
		t.Fatalf("keys = %+v, want one bound to acct-1", keys)
	}
}
