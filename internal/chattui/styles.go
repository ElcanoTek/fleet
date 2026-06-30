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

	styleUserLabel  = lipgloss.NewStyle().Foreground(cUser).Bold(true)
	styleAgentLabel = lipgloss.NewStyle().Foreground(cAgent).Bold(true)
	styleTool       = lipgloss.NewStyle().Foreground(cTool)
	styleErr        = lipgloss.NewStyle().Foreground(cErr).Bold(true)
	styleDim        = lipgloss.NewStyle().Foreground(cDim)
	styleAccent     = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	styleHeader     = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
)

// markdownRenderer lazily builds (and caches per width) a glamour renderer. glamour
// turns the agent's reply into an ANSI string we drop straight into the viewport;
// it carries its OWN lipgloss internally, so it composes fine with our v2 styles.
type markdownRenderer struct {
	mu    sync.Mutex
	width int
	r     *glamour.TermRenderer
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
		r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
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
