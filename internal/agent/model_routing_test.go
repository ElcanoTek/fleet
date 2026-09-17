package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

func TestModelDiscoveryTracksOnlySuccessfulProviderSwaps(t *testing.T) {
	m := &Manager{config: &config.Config{}}
	base := []agentcore.ProviderConfig{{Name: "direct", Type: agentcore.ProviderTypeOpenAI, APIKey: "test-key", Models: []string{"gpt-4o"}}}
	if err := m.SetLLMProviders(base); err != nil {
		t.Fatal(err)
	}
	// This manager has no sandbox pool or broker. An unavailable model must
	// still produce an actionable routing error before touching either one.
	if _, err := m.RunTurn(context.Background(), TurnInput{Model: "google/gemini-3.8-flash"}, nil); err == nil || !strings.Contains(err.Error(), "choose a workspace model") {
		t.Fatalf("turn preflight = %v", err)
	}
	broken := []agentcore.ProviderConfig{{Name: "broken", Type: "unsupported"}}
	if err := m.SetLLMProviders(broken); err == nil {
		t.Fatal("invalid provider applied")
	}
	if got := m.ModelProviders(); len(got) != 1 || got[0].Name != "direct" {
		t.Fatalf("failed swap changed discovery: %+v", got)
	}
	merged := agentcore.MergeLLMProviders(base, []agentcore.ProviderConfig{{Name: "direct", Type: agentcore.ProviderTypeOpenAI, APIKey: "test-replacement", Models: []string{"gpt-4o-mini"}}}, "")
	if err := m.SetLLMProviders(merged); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckModelRoute("gpt-4o"); err == nil {
		t.Fatal("old model still advertised after overlay")
	}
	if _, err := m.CheckModelRoute("gpt-4o-mini"); err != nil {
		t.Fatal(err)
	}
}
