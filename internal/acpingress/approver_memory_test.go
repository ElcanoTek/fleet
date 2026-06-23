package acpingress

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"
)

// TestProposeMemoryOverIngress_Allow: a propose_memory tool call over ingress
// stages a memory proposal and resolves it over request_permission; on Allow the
// proposal is accepted (source flips to "chat") — proving propose_memory is wired
// over ingress (issue #40), not silently unavailable.
func TestProposeMemoryOverIngress_Allow(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		toolRound("mem-call", "propose_memory", `{"content":"the user prefers metric units"}`),
		textRound("noted"),
	}}
	w := setup(t, model, baseCfg())
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return allowResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if resp := w.initNewPrompt(ctx, t, "remember my preference"); resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	// Exactly one memory proposal was staged and then ACCEPTED on Allow.
	if len(w.store.memories) != 1 {
		t.Fatalf("memory proposals staged = %d, want 1", len(w.store.memories))
	}
	for id, m := range w.store.memories {
		if m.Content != "the user prefers metric units" {
			t.Errorf("memory %s content = %q", id, m.Content)
		}
		if m.Source != "chat" {
			t.Errorf("memory %s source = %q, want chat (accepted on Allow)", id, m.Source)
		}
	}
	// The human was asked (no silent accept).
	if w.editor.permissionCount() != 1 {
		t.Errorf("permission requests = %d, want 1", w.editor.permissionCount())
	}
}

// TestProposeMemoryOverIngress_Deny: on Reject (default-DENY behavior), the
// proposal is left PENDING (source stays "proposed") — mirroring the web path,
// which never deletes a proposal on deny.
func TestProposeMemoryOverIngress_Deny(t *testing.T) {
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{
		toolRound("mem-call", "propose_memory", `{"content":"do not save this"}`),
		textRound("ok"),
	}}
	w := setup(t, model, baseCfg())
	w.editor.decide = func(p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		return rejectResp(p), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if resp := w.initNewPrompt(ctx, t, "maybe remember this"); resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	if len(w.store.memories) != 1 {
		t.Fatalf("memory proposals staged = %d, want 1", len(w.store.memories))
	}
	for id, m := range w.store.memories {
		if m.Source != "proposed" {
			t.Errorf("memory %s source = %q, want proposed (left pending on deny)", id, m.Source)
		}
	}
}
