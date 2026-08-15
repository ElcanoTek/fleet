package apikeys

import (
	"strings"
	"testing"
)

// TestSecondRotationRevokesOriginalKey pins the #567 fix: rotating a key twice
// (H0 → H1 → H2) must actually revoke H0. Before the fix, RotateKey overwrote
// PreviousKeyHash without deleting the outgoing previous hash from the
// keyHashIndex, and ValidateKey's grace-expiry cleanup only fires for the
// CURRENT PreviousKeyHash — so the original (possibly leaked) key kept
// authenticating indefinitely, until a process restart happened to rebuild the
// index. Only the most recent predecessor may remain valid, within its grace.
func TestSecondRotationRevokesOriginalKey(t *testing.T) {
	tmpDir := t.TempDir()
	manager, err := NewManager(tmpDir+"/keys.json", tmpDir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	key, raw0, err := manager.CreateKey("double-rotate", nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("Failed to create key: %v", err)
	}

	// First rotation: H0 -> H1, H0 enters its grace window.
	_, raw1, err := manager.RotateKey(key.KeyID, 1)
	if err != nil {
		t.Fatalf("First rotation failed: %v", err)
	}
	// Second rotation, before H0's grace elapses: H1 -> H2.
	_, raw2, err := manager.RotateKey(key.KeyID, 1)
	if err != nil {
		t.Fatalf("Second rotation failed: %v", err)
	}

	// The original key must be dead — this is the leaked-key revocation case.
	if valid, _, msg := manager.ValidateKey(raw0, nil, nil, nil); valid {
		t.Error("original key still authenticates after two rotations — rotation did not revoke it")
	} else if !strings.Contains(msg, "Invalid API key") {
		t.Errorf("original key rejection = %q, want %q", msg, "Invalid API key")
	}

	// The most recent predecessor is still inside its grace window: valid.
	if valid, _, msg := manager.ValidateKey(raw1, nil, nil, nil); !valid {
		t.Errorf("previous key within grace should validate, got %q", msg)
	}

	// The current key is valid.
	if valid, _, msg := manager.ValidateKey(raw2, nil, nil, nil); !valid {
		t.Errorf("current key should validate, got %q", msg)
	}

	// And the revocation survives a restart: a fresh manager over the same
	// store must reject H0 too (load reindexes only current + previous).
	reloaded, err := NewManager(tmpDir+"/keys.json", tmpDir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("Failed to reload manager: %v", err)
	}
	if valid, _, _ := reloaded.ValidateKey(raw0, nil, nil, nil); valid {
		t.Error("original key authenticates after restart — persisted state kept a stale hash")
	}
	if valid, _, msg := reloaded.ValidateKey(raw2, nil, nil, nil); !valid {
		t.Errorf("current key should validate after restart, got %q", msg)
	}
}
