package apikeys

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCLIMintedKeyVisibleWithoutRestart pins the operational contract behind
// `fleet sched apikey create`: a key appended to api_keys.json by a SEPARATE
// process (here: a second Manager on the same file) authenticates against an
// already-running Manager without a restart — the lookup miss triggers a
// staleness check + reload. Found live: a freshly minted fleet_task_ key got
// 401s until the server restarted.
func TestCLIMintedKeyVisibleWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")

	server, err := NewManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: garbage never validates and must not spin the file.
	if _, _, ok := server.LookupKeyMeta("fleet_task_garbage"); ok {
		t.Fatal("garbage key validated")
	}

	// A second Manager = the CLI process minting a key into the same file.
	cli, err := NewManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	// mtime granularity can be 1s on some filesystems; make the write land
	// strictly after the server's load time.
	time.Sleep(1100 * time.Millisecond)
	_, raw, err := cli.CreateTypedKey("ci-bot", KeyTypeTask, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, ok := server.LookupKeyMeta(raw); !ok {
		t.Error("LookupKeyMeta: CLI-minted key invisible to the running manager")
	}
	if kt, _, ok := server.LookupKeyType(raw); !ok || kt != KeyTypeTask {
		t.Errorf("LookupKeyType: ok=%v type=%q", ok, kt)
	}
	if valid, key, msg := server.ValidateKey(raw, nil, nil, nil, nil); !valid {
		t.Errorf("ValidateKey: %s", msg)
	} else if key.Name != "ci-bot" {
		t.Errorf("key name %q", key.Name)
	}

	// A revoked-in-memory key must NOT be resurrected by a later refresh.
	if err := server.RevokeKey(func() string {
		id, _, _ := server.LookupKeyMeta(raw)
		return id
	}()); err != nil {
		t.Fatal(err)
	}
	if valid, _, _ := server.ValidateKey(raw, nil, nil, nil, nil); valid {
		t.Error("revoked key still validates")
	}

	// An unchanged file on a miss = no reload storm (mtime check only).
	if _, _, ok := server.LookupKeyMeta("fleet_task_alsogarbage"); ok {
		t.Fatal("garbage validated after refresh")
	}
}
