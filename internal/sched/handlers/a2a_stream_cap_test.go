// Copyright (c) 2026 ElcanoTek

package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// One credential's streams are capped below the process-wide ceiling, so
// another credential can still open one.
func TestA2AStreamCounter_PerPrincipalCap(t *testing.T) {
	c := newA2AStreamCounter()
	keyA := a2aStreamKey(principal{apiKey: &apikeys.APIKey{KeyID: "a"}})
	keyB := a2aStreamKey(principal{apiKey: &apikeys.APIKey{KeyID: "b"}})
	for i := 0; i < a2aMaxConcurrentStreamsPerPrincipal; i++ {
		if !c.acquire(keyA) {
			t.Fatalf("stream %d for key A refused early", i+1)
		}
	}
	if c.acquire(keyA) {
		t.Fatal("key A exceeded its per-credential stream cap")
	}
	if !c.acquire(keyB) {
		t.Fatal("key B was refused while key A held its cap")
	}
	c.release(keyA)
	if !c.acquire(keyA) {
		t.Fatal("a released slot was not reusable")
	}
	// The admin key is not counted per credential.
	admin := a2aStreamKey(principal{isAdmin: true})
	for i := 0; i < a2aMaxConcurrentStreamsPerPrincipal+1; i++ {
		if !c.acquire(admin) {
			t.Fatal("admin key held to the per-credential cap")
		}
	}
}

// A reservation past the per-credential cap is refused with a JSON-RPC
// error before any SSE bytes, and gives back the process-wide slot.
func TestA2AReserveStream_RefusesPastPerPrincipalCap(t *testing.T) {
	h := New(Config{DefaultTaskModel: "test/model"}, nil, nil)
	p := principal{apiKey: &apikeys.APIKey{KeyID: "a"}}
	for i := 0; i < a2aMaxConcurrentStreamsPerPrincipal; i++ {
		if !h.a2aStreamsBy.acquire(a2aStreamKey(p)) {
			t.Fatal("setup acquire refused")
		}
	}
	w := httptest.NewRecorder()
	if slot := h.a2aReserveStream(w, p, json.RawMessage(`1`)); slot != nil {
		t.Fatal("reservation granted past the per-credential cap")
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatal("a refused stream must not start SSE")
	}
	if !strings.Contains(w.Body.String(), "concurrent A2A streams") {
		t.Fatalf("body = %s, want the per-credential refusal", w.Body.String())
	}
	if h.a2aStreams != 0 {
		t.Fatalf("process-wide count = %d after a refusal, want 0", h.a2aStreams)
	}
}

// A granted reservation counts against both ceilings until released, and a
// second release (the caller's defer after the stream's own) is a no-op.
func TestA2AReserveStream_ReleaseIsIdempotent(t *testing.T) {
	h := New(Config{DefaultTaskModel: "test/model"}, nil, nil)
	p := principal{apiKey: &apikeys.APIKey{KeyID: "a"}}
	slot := h.a2aReserveStream(httptest.NewRecorder(), p, json.RawMessage(`1`))
	if slot == nil {
		t.Fatal("reservation refused under both ceilings")
	}
	if h.a2aStreams != 1 || h.a2aStreamsBy.byPrincipal[a2aStreamKey(p)] != 1 {
		t.Fatalf("counts after reserve = %d / %d, want 1 / 1", h.a2aStreams, h.a2aStreamsBy.byPrincipal[a2aStreamKey(p)])
	}
	slot.release()
	slot.release()
	if h.a2aStreams != 0 || len(h.a2aStreamsBy.byPrincipal) != 0 {
		t.Fatalf("counts after release = %d / %v, want 0 / empty", h.a2aStreams, h.a2aStreamsBy.byPrincipal)
	}
}
