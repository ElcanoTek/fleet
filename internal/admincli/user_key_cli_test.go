package admincli

import (
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
	if code := createUserKey(mgr, keyStore{}, "cli", "u@x.com", 1); code == 0 {
		t.Fatal("a user key was minted with a per-key rate limit")
	}
	if n := len(mgr.ListKeys()); n != 0 {
		t.Fatalf("%d keys minted", n)
	}
	if code := createUserKey(mgr, keyStore{}, "cli", "", 0); code == 0 {
		t.Fatal("a user key was minted with no account")
	}
}
