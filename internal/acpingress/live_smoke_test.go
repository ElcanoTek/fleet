//go:build live

// Optional LIVE smoke for ACP ingress: drives one real governed turn through the
// in-memory IngressAgent ↔ fake-editor harness against the REAL OpenRouter
// endpoint (no fake LLM), proving the ingress adapter streams a genuine model
// reply end to end. It is build-tagged `live` so it NEVER runs in the
// deterministic CI suite; run it manually with a real key:
//
//	OPENROUTER_API_KEY=sk-... go test -tags live ./internal/acpingress/ -run TestLiveSmoke -v
//
// Deterministic coverage is the (non-tagged) round-trip + governance tests.
package acpingress

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

func TestLiveSmoke(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("set OPENROUTER_API_KEY to run the live ingress smoke")
	}
	model := os.Getenv("FLEET_ACP_MODEL")
	if model == "" {
		model = "anthropic/claude-opus-4.8"
	}

	resolver, err := agentcore.NewModelResolver(key, agentcore.DefaultProviderHeaders)
	if err != nil {
		t.Fatalf("model resolver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	lm, err := resolver.Resolve(ctx, model)
	if err != nil {
		t.Fatalf("resolve model %q: %v", model, err)
	}

	w := setupLive(t, lm)
	resp := w.initNewPrompt(ctx, t, "Reply with exactly the word: pong")
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if strings.TrimSpace(w.editor.streamedText()) == "" {
		t.Fatalf("no streamed text from the live model")
	}
	t.Logf("live reply: %q", w.editor.streamedText())
}

// setupLive wires the harness with a real fantasy model (vs. the scripted fake).
func setupLive(t *testing.T, lm fantasy.LanguageModel) *wired {
	t.Helper()
	clientToAgentR, clientToAgentW := pipePair()
	agentToClientR, agentToClientW := pipePair()

	st := newMemStore()
	runner := &fakeRunner{}
	eng := newFakeEngine(lm)
	ia := New(eng, st, st, runner, Config{Model: lm.Model(), Principal: Principal{Email: "live@fleet.local"}})
	agentConn := acp.NewAgentSideConnection(ia, agentToClientW, clientToAgentR)
	ia.SetAgentConnection(agentConn)

	editor := newFakeEditor()
	clientConn := acp.NewClientSideConnection(editor, clientToAgentW, agentToClientR)
	t.Cleanup(func() {
		_ = clientToAgentW.Close()
		_ = agentToClientW.Close()
	})
	return &wired{agent: ia, editor: editor, client: clientConn, store: st, runner: runner}
}

var _ = agent.TurnInput{}
