package chattui

import (
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
)

// The fleet palette — kept close to the sibling middle-manager TUI / crush so the
// terminal experience feels of-a-piece.
var (
	cAccent = lipgloss.Color("#36E2E2") // cyan — brand / prompts
	cUser   = lipgloss.Color("#9D7CFF") // violet — the human's turns
	cAgent  = lipgloss.Color("#3DF5A0") // green — the agent's turns
	cTool   = lipgloss.Color("#FFC857") // amber — tool calls
	cErr    = lipgloss.Color("#FF5C72") // red — errors
	cDim    = lipgloss.Color("#6C7086") // muted — reasoning / hints
	cInk    = lipgloss.Color("#16161E") // near-black — text on colored pills

	styleTool   = lipgloss.NewStyle().Foreground(cTool)
	styleErr    = lipgloss.NewStyle().Foreground(cErr).Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(cDim)
	styleAccent = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	styleHeader = lipgloss.NewStyle().Foreground(cAccent).Bold(true)

	// Speaker pills: high-contrast chips so turns scan at a glance even in a
	// long transcript. Ink-on-color mirrors the web app's badge treatment.
	stylePillUser  = lipgloss.NewStyle().Background(cUser).Foreground(cInk).Bold(true).Padding(0, 1)
	stylePillAgent = lipgloss.NewStyle().Background(cAgent).Foreground(cInk).Bold(true).Padding(0, 1)

	// Tool-call outcome glyphs (replacing the old "…"/"ok"/"error" words).
	styleToolOK  = lipgloss.NewStyle().Foreground(cAgent)
	styleToolErr = lipgloss.NewStyle().Foreground(cErr)

	// The composer: a rounded, accent-bordered box that dims while a turn is
	// streaming (input is queued behind the running turn, and the border color
	// says so without words).
	styleInputBox     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
	styleInputBoxBusy = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim).Padding(0, 1)

	// Thin rule under the header, over the status row.
	styleRule = lipgloss.NewStyle().Foreground(lipgloss.Color("#2A2A37"))
)

// rule renders a full-width horizontal hairline.
func rule(width int) string {
	if width < 1 {
		width = 1
	}
	return styleRule.Render(strings.Repeat("─", width))
}

// markdownRenderer lazily builds (and caches per width) a glamour renderer. glamour
// turns the agent's reply into an ANSI string we drop straight into the viewport;
// it carries its OWN lipgloss internally, so it composes fine with our v2 styles.
type markdownRenderer struct {
	mu    sync.Mutex
	width int
	r     *glamour.TermRenderer

	// forceStyle overrides glamour's terminal auto-detection. Empty = auto (the
	// production default: dark/light/notty by the real terminal). The screenshot
	// generator (#487) sets "dark" so the captured frame shows glamour's styled
	// output instead of the no-TTY plain fallback that a piped `go test` gets.
	forceStyle string
}

func (m *markdownRenderer) render(md string, width int) string {
	md = strings.TrimRight(md, "\n")
	if md == "" {
		return ""
	}
	if width < 20 {
		width = 20
	}
	m.mu.Lock()
	if m.r == nil || m.width != width {
		styleOpt := glamour.WithAutoStyle()
		if m.forceStyle != "" {
			styleOpt = glamour.WithStandardStyle(m.forceStyle)
		}
		r, err := glamour.NewTermRenderer(styleOpt, glamour.WithWordWrap(width))
		if err != nil {
			m.mu.Unlock()
			return md // fall back to raw markdown — never lose the content
		}
		m.r, m.width = r, width
	}
	r := m.r
	m.mu.Unlock()

	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return strings.TrimRight(out, "\n")
}
