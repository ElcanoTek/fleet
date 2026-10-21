package httpapi

import (
	"slices"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// The per-turn connector recommendation never suggests a connector whose
// every tool interactive_hidden_tools keeps out of chat: connecting it would
// give the chat nothing. A partly hidden connector, and one whose tools are
// not known yet, are still recommended; the catalog is not modified.
func TestChatVisibleConnectors_DropsFullyHiddenConnectorFromRecommendations(t *testing.T) {
	catalog := []agent.OptionalServerInfo{
		{Name: "openx", DisplayName: "OpenX", Description: "book OpenX deals", Tools: []string{"ox_create_deal"}},
		{Name: "xandr", DisplayName: "Xandr", Description: "book Xandr deals", Tools: []string{"create_deal", "preview_plan"}},
		{Name: "magnite", DisplayName: "Magnite", Description: "book Magnite deals"},
	}
	hidden := []string{"create_deal"}

	visible := chatVisibleConnectors(catalog, hidden)
	names := make([]string, 0, len(visible))
	for _, srv := range visible {
		names = append(names, srv.Name)
	}
	if want := []string{"xandr", "magnite"}; !slices.Equal(names, want) {
		t.Fatalf("visible = %v, want %v", names, want)
	}
	if len(catalog) != 3 || len(catalog[1].Tools) != 2 {
		t.Fatalf("the catalog was modified: %+v", catalog)
	}
	for _, rec := range recommendConnectors("book deals", visible, nil, 3) {
		if rec.Name == "openx" {
			t.Fatalf("a fully hidden connector was recommended: %+v", rec)
		}
	}
	if got := chatVisibleConnectors(catalog, nil); len(got) != 3 {
		t.Fatalf("no key: %d connectors, want 3", len(got))
	}
}
