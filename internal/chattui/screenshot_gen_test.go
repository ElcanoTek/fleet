package chattui

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestGenerateTUIScreenshot renders a representative `fleet chat` TUI frame and
// writes its raw ANSI to the file named by FLEET_TUI_SCREENSHOT_ANSI (#487).
// It is a GENERATOR, not an assertion: it is skipped in a normal `go test` run
// and only fires when the env var is set (scripts/generate-tui-screenshot.sh
// sets it, then pipes the ANSI through `freeze` to a PNG). It reaches no server
// — the model renders purely from in-memory state — so the committed screenshot
// carries no real conversation data.
func TestGenerateTUIScreenshot(t *testing.T) {
	out := os.Getenv("FLEET_TUI_SCREENSHOT_ANSI")
	if out == "" {
		t.Skip("FLEET_TUI_SCREENSHOT_ANSI not set — screenshot generator, not run in the normal suite")
	}

	m := newModel(Config{
		ServerURL: "http://fleet.internal:8080",
		Email:     "you@example.com",
		Model:     "anthropic/claude-opus-4-8",
	})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 92, Height: 26})
	m = updated.(*model)
	// Force glamour's dark style: a piped `go test` isn't a TTY, so auto-style
	// would fall back to plain notty and the reply would lose its bold/color.
	m.md.forceStyle = "dark"

	// Drive one completed exchange through the real event path so the frame
	// matches what a live turn produces: a committed user line, a tool call, and
	// a glamour-rendered assistant reply.
	const ask = "How many scheduled tasks failed in the last 24h?"
	m.lastUser = ask
	m.history = append(m.history, stylePillUser.Render("you")+"\n"+ask)
	m.streaming = true
	m.applyEvent(Event{Name: "conversation", Data: map[string]any{"id": "a1b2c3d4e5f6"}})
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "bash"}})
	m.applyEvent(Event{Name: "text.delta", Data: map[string]any{"text": "**3** scheduled tasks failed in the last 24h:\n\n" +
		"- `nightly-etl` — exit 1 (upstream API returned 503)\n" +
		"- `db-backup` — timed out after 30m\n" +
		"- `weekly-report` — template not found\n\n" +
		"The first two look transient. Want me to retry them and re-run the report once the template is restored?"}})
	m.finishTurn(turnDoneMsg{convID: "a1b2c3d4e5f6"})
	m.refresh()

	if err := os.WriteFile(out, []byte(m.render()), 0o644); err != nil {
		t.Fatalf("write TUI screenshot ANSI to %s: %v", out, err)
	}
	t.Logf("wrote TUI screenshot ANSI (%d bytes) to %s", len(m.render()), out)
}
