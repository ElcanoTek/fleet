package apikeys

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two Managers over one file stand in for the server and the `fleet sched
// apikey` CLI: a change the second makes to a key the first already knows —
// revoke, rotate, delete — takes effect on the first's very next request,
// with no restart.
func twoManagers(t *testing.T) (server, cli *Manager) {
	t.Helper()
	dir := t.TempDir()
	path, audit := filepath.Join(dir, "api_keys.json"), filepath.Join(dir, "audit.jsonl")
	var err error
	if server, err = NewManager(path, audit); err != nil {
		t.Fatal(err)
	}
	if cli, err = NewManager(path, audit); err != nil {
		t.Fatal(err)
	}
	return server, cli
}

func TestExternalRevokeTakesEffectAtOnce(t *testing.T) {
	for _, kt := range []KeyType{KeyTypeUser, KeyTypeTask, KeyTypeAdmin} {
		server, cli := twoManagers(t)
		var (
			key *APIKey
			raw string
			err error
		)
		if kt == KeyTypeUser {
			key, raw, err = server.CreateUserKey("k", "u@x.com", 0, nil, "")
		} else {
			key, raw, err = server.CreateTypedKey("k", kt, nil, 0, nil, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		if ok, _, _ := server.ValidateKey(raw, nil, nil, nil); !ok {
			t.Fatalf("%s: fresh key rejected", kt)
		}
		if err := cli.RevokeKey(key.KeyID); err != nil {
			t.Fatal(err)
		}
		if ok, _, reason := server.ValidateKey(raw, nil, nil, nil); ok {
			t.Fatalf("%s: a key revoked by another process still authenticates", kt)
		} else if reason != "API key is disabled" {
			t.Errorf("%s: reason %q", kt, reason)
		}
		if _, _, ok := server.LookupKeyType(raw); ok {
			t.Errorf("%s: LookupKeyType still resolves the revoked key", kt)
		}
		if _, _, ok := server.LookupKeyMeta(raw); ok {
			t.Errorf("%s: LookupKeyMeta still resolves the revoked key", kt)
		}
	}
}

func TestExternalRotateTakesEffectAtOnce(t *testing.T) {
	server, cli := twoManagers(t)
	key, oldRaw, err := server.CreateUserKey("k", "u@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := server.ValidateKey(oldRaw, nil, nil, nil); !ok {
		t.Fatal("fresh key rejected")
	}
	_, newRaw, err := cli.RotateKey(key.KeyID, 0) // no grace: the old key ends now
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // past the zero-hour grace instant
	if ok, _, _ := server.ValidateKey(oldRaw, nil, nil, nil); ok {
		t.Error("the rotated-out key still authenticates")
	}
	ok, got, _ := server.ValidateKey(newRaw, nil, nil, nil)
	if !ok || got.UserEmail != "u@x.com" {
		t.Error("the rotated-in key is rejected")
	}
}

func TestExternalDeleteAndEditTakeEffect(t *testing.T) {
	server, cli := twoManagers(t)
	key, raw, err := server.CreateTypedKey("k", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ceiling := 3
	if err := cli.SetMaxPriority(key.KeyID, &ceiling); err != nil {
		t.Fatal(err)
	}
	if got := server.GetKey(key.KeyID); got == nil || got.MaxPriority == nil || *got.MaxPriority != 3 {
		t.Fatalf("an edit by another process was not seen: %+v", got)
	}
	if err := cli.DeleteKey(key.KeyID); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := server.ValidateKey(raw, nil, nil, nil); ok || server.GetKey(key.KeyID) != nil {
		t.Fatal("a key deleted by another process still exists")
	}
}

// A write of ours never drops a key another process added meanwhile: every
// mutation syncs first.
func TestOwnWritesKeepExternalKeys(t *testing.T) {
	server, cli := twoManagers(t)
	ext, extRaw, err := cli.CreateTypedKey("ext", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.CreateTypedKey("own", KeyTypeTask, nil, 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewManager(server.storagePath, server.auditLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.GetKey(ext.KeyID) == nil {
		t.Fatal("the server's write dropped a key the CLI had added")
	}
	if ok, _, _ := server.ValidateKey(extRaw, nil, nil, nil); !ok {
		t.Fatal("the CLI's key does not authenticate on the server")
	}
}

// An unreadable or torn file leaves the known keys in place.
func TestBrokenFileKeepsState(t *testing.T) {
	server, _ := twoManagers(t)
	_, raw, err := server.CreateTypedKey("k", KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.storagePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := server.ValidateKey(raw, nil, nil, nil); !ok {
		t.Fatal("a torn file wiped the known keys")
	}
}

// KeyType.AllowsSurface is the one table of which key types authenticate on
// which surface; ValidateKeyOn and the Lookup*On helpers apply it before any
// rate-limit token is charged.
func TestKeyTypeSurfaces(t *testing.T) {
	want := map[KeyType][]Surface{
		KeyTypeAdmin:    {SurfaceOrchestrator, SurfaceA2A},
		KeyTypeTask:     {SurfaceOrchestrator, SurfaceA2A},
		KeyTypeReadonly: {SurfaceOrchestrator, SurfaceA2A},
		KeyTypeLegacy:   {SurfaceOrchestrator, SurfaceA2A},
		KeyTypeWebhook:  {SurfaceOrchestrator, SurfaceA2A, SurfaceTrigger},
		KeyTypeUser:     {SurfaceChat},
	}
	server, _ := twoManagers(t)
	for kt, allowed := range want {
		var raw string
		var err error
		switch kt {
		case KeyTypeUser:
			_, raw, err = server.CreateUserKey("k", "u@x.com", 0, nil, "")
		case KeyTypeLegacy:
			role := "client"
			_, raw, err = server.CreateKey("k", nil, &role, 0, nil, "")
		default:
			_, raw, err = server.CreateTypedKey("k", kt, []string{"s"}, 0, nil, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range Surfaces {
			exp := false
			for _, a := range allowed {
				exp = exp || a == s
			}
			if got := kt.AllowsSurface(s); got != exp {
				t.Errorf("%q.AllowsSurface(%s) = %v, want %v", kt, s, got, exp)
			}
			ok, _, reason := server.ValidateKeyOn(s, raw, nil, nil, nil)
			if ok != exp || (!exp && reason != ReasonWrongSurface) {
				t.Errorf("ValidateKeyOn(%s, %q key) = %v %q, want %v", s, kt, ok, reason, exp)
			}
			if _, _, ok := server.LookupKeyTypeOn(s, raw); ok != exp {
				t.Errorf("LookupKeyTypeOn(%s, %q key) = %v, want %v", s, kt, ok, exp)
			}
			if _, _, ok := server.LookupKeyMetaOn(s, raw); ok != exp {
				t.Errorf("LookupKeyMetaOn(%s, %q key) = %v, want %v", s, kt, ok, exp)
			}
		}
	}
}

// A user key takes no per-key rate limit (see CreateUserKey).
func TestUserKeyRefusesARateLimit(t *testing.T) {
	server, _ := twoManagers(t)
	if _, _, err := server.CreateUserKey("k", "u@x.com", 1, nil, ""); err == nil {
		t.Fatal("a user key accepted a per-key rate limit")
	}
}
