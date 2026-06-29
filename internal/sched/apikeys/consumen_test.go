// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package apikeys

import (
	"os"
	"testing"
)

// TestConsumeN verifies the multi-token rate-limit charge (#227): a batch of N
// tasks costs N tokens total (ValidateKey charges 1, ConsumeN charges N-1).
func TestConsumeN(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "apikeys_consumen")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := NewManager(tmpDir+"/keys.json", tmpDir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	// No per-key cap (RateLimit=0) → ConsumeN is a no-op success.
	key, raw, err := mgr.CreateKey("nocap", nil, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if !mgr.ConsumeN(key.KeyID, 50) {
		t.Fatal("uncapped key ConsumeN returned false")
	}

	// Capped key (RateLimit=5): ValidateKey charges 1, then ConsumeN(4) reaches
	// exactly 5. A further ConsumeN(1) must fail (would exceed).
	capped, rawCapped, err := mgr.CreateKey("capped", nil, nil, nil, 5, nil, "")
	if err != nil {
		t.Fatalf("create capped key: %v", err)
	}
	if _, _, msg := mgr.ValidateKey(rawCapped, nil, nil, nil, nil); !mgr.ConsumeN(capped.KeyID, 4) {
		t.Fatalf("ConsumeN(4) after 1 ValidateKey should succeed (1+4=5==cap): %s", msg)
	}
	if mgr.ConsumeN(capped.KeyID, 1) {
		t.Fatal("ConsumeN(1) over the cap should fail")
	}
	_ = raw

	// Unknown keyID and n<=0 are no-op successes (admin/cookie callers).
	if !mgr.ConsumeN("key_nope", 5) {
		t.Fatal("unknown keyID ConsumeN should succeed (no-op)")
	}
	if !mgr.ConsumeN(capped.KeyID, 0) {
		t.Fatal("ConsumeN(0) should succeed (no-op)")
	}
	if !mgr.ConsumeN(capped.KeyID, -1) {
		t.Fatal("ConsumeN(-1) should succeed (no-op)")
	}
}

// TestConsumeN_RejectsOverBudget verifies a single ConsumeN call that would
// exceed the remaining budget is rejected WITHOUT mutating the counter, so a
// retried smaller charge can still succeed.
func TestConsumeN_RejectsOverBudget(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "apikeys_consumen_over")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := NewManager(tmpDir+"/keys.json", tmpDir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	capped, raw, err := mgr.CreateKey("capped", nil, nil, nil, 5, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// ValidateKey charges 1 → remaining = 4. ConsumeN(5) would reach 6 > 5 →
	// rejected, and the counter must stay at 1 so a subsequent ConsumeN(4) fits.
	if ok, _, _ := mgr.ValidateKey(raw, nil, nil, nil, nil); !ok {
		t.Fatal("setup ValidateKey failed")
	}
	if mgr.ConsumeN(capped.KeyID, 5) {
		t.Fatal("ConsumeN(5) over budget should be rejected")
	}
	if !mgr.ConsumeN(capped.KeyID, 4) {
		t.Fatal("ConsumeN(4) should now succeed (1+4=5==cap)")
	}
}
