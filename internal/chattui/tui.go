package chattui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// altView wraps the rendered frame in an alt-screen tea.View (bubbletea v2 has no
// WithAltScreen program option). Alt-screen gives a fixed terminal-sized buffer
// with per-cell diffing, so a tall transcript repaints cleanly instead of
// scrolling/flickering the real terminal.
func altView(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// ── async messages pushed into the model from the SSE goroutine ──

type sseMsg struct{ ev Event } // one streamed frame
type turnDoneMsg struct {
	convID string
	err    error
}                            // turn finished (or stream ended)
type spinnerTickMsg struct{} // animate the "working" indicator

func spinnerTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// model is the fleet chat TUI. Pointer receiver: Update mutates and returns the
// same pointer (matching the sibling repo's bubbletea v2 usage).
type model struct {
	client *Client
	prog   *tea.Program // set before Run so the SSE goroutine can Send frames
	md     markdownRenderer

	vp    viewport.Model
	input textinput.Model
	width int

	convID  string
	history []string // committed transcript blocks (already glamour-rendered)

	// In-flight turn state.
	streaming bool
	assistant strings.Builder // raw assistant text accumulated this turn
	toolLines []string        // "▸ tool …" one-liners this turn
	reasoning strings.Builder // raw reasoning this turn (shown only when showReasoning)
	cancel    context.CancelFunc
	frame     int

	showReasoning bool
	lastUser      string // for /retry
	statusErr     string // last error line (cleared on next send)
}

func newModel(cfg Config) *model {
	ti := textinput.New()
	ti.Placeholder = "Message the agent  (Enter to send · /help · Ctrl+C to cancel/quit)"
	ti.Focus()
	ti.CharLimit = 0
	vp := viewport.New()
	m := &model{client: NewClient(cfg), input: ti, vp: vp}
	m.history = append(m.history, styleDim.Render(
		"fleet chat — talking to "+cfg.ServerURL+" as "+cfg.Email+
			func() string {
				if cfg.Model != "" {
					return "  (model: " + cfg.Model + ")"
				}
				return ""
			}()))
	m.history = append(m.history, styleDim.Render("Type a message and press Enter. Slash commands: /new /retry /model <slug> /reasoning /clear /quit."))
	return m
}

func (m *model) Init() tea.Cmd { return textinput.Blink }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		// Reserve rows: header(1) + blank(1) + status(1) + input(1) + padding(1).
		bodyH := msg.Height - 5
		if bodyH < 3 {
			bodyH = 3
		}
		m.vp.SetWidth(msg.Width)
		m.vp.SetHeight(bodyH)
		m.input.SetWidth(msg.Width - 2)
		m.refresh()
		m.vp.GotoBottom()

	case spinnerTickMsg:
		if m.streaming {
			m.frame++
			m.refresh()
			cmds = append(cmds, spinnerTick())
		}

	case sseMsg:
		m.applyEvent(msg.ev)
		m.refresh()
		m.vp.GotoBottom()

	case turnDoneMsg:
		m.finishTurn(msg)
		m.refresh()
		m.vp.GotoBottom()

	case tea.KeyPressMsg:
		if cmd, consumed := m.onKey(msg); consumed {
			return m, cmd
		}
	}

	// Forward to the sub-components (input cursor, viewport scroll).
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

// onKey returns (cmd, consumed). When consumed is true the key was fully handled
// (enter/ctrl+c/scroll) and must NOT also fall through to the textinput/viewport.
func (m *model) onKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "ctrl+c":
		if m.streaming && m.cancel != nil {
			m.cancel() // abort the in-flight turn; the goroutine emits turnDoneMsg
			return nil, true
		}
		return tea.Quit, true
	case "ctrl+d":
		return tea.Quit, true
	case "pgup":
		m.vp.HalfPageUp()
		return nil, true
	case "pgdown":
		m.vp.HalfPageDown()
		return nil, true
	case "enter":
		return m.onEnter(), true
	}
	return nil, false
}

func (m *model) onEnter() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.streaming {
		return nil
	}
	m.input.SetValue("")
	m.statusErr = ""

	if strings.HasPrefix(text, "/") {
		return m.runSlash(text)
	}
	return m.sendTurn(text)
}

func (m *model) runSlash(text string) tea.Cmd {
	fields := strings.Fields(text)
	switch fields[0] {
	case "/quit", "/q", "/exit":
		return tea.Quit
	case "/new":
		m.convID = ""
		m.history = append(m.history, styleDim.Render("— new conversation —"))
		m.refresh()
		m.vp.GotoBottom()
		return nil
	case "/clear":
		m.history = m.history[:0]
		m.refresh()
		return nil
	case "/reasoning":
		m.showReasoning = !m.showReasoning
		m.history = append(m.history, styleDim.Render(fmt.Sprintf("— reasoning display: %v —", m.showReasoning)))
		m.refresh()
		m.vp.GotoBottom()
		return nil
	case "/model":
		if len(fields) < 2 {
			m.history = append(m.history, styleDim.Render("usage: /model <slug>  (current: "+orDefault(m.client.cfg.Model, "server default")+")"))
		} else {
			m.client.cfg.Model = fields[1]
			m.history = append(m.history, styleDim.Render("— model set to "+fields[1]+" (applies to the next turn) —"))
		}
		m.refresh()
		m.vp.GotoBottom()
		return nil
	case "/retry":
		if m.lastUser == "" {
			m.history = append(m.history, styleDim.Render("— nothing to retry —"))
			m.refresh()
			return nil
		}
		return m.sendTurn(m.lastUser)
	case "/help":
		m.history = append(m.history, styleDim.Render(
			"commands: /new (fresh conversation) · /retry · /model <slug> · /reasoning (toggle) · /clear · /quit · PgUp/PgDn scroll · Ctrl+C cancel/quit"))
		m.refresh()
		m.vp.GotoBottom()
		return nil
	default:
		m.history = append(m.history, styleDim.Render("unknown command "+fields[0]+" — try /help"))
		m.refresh()
		m.vp.GotoBottom()
		return nil
	}
}

// sendTurn commits the user's message to the transcript and launches the SSE
// stream in a goroutine that pushes frames back via prog.Send.
func (m *model) sendTurn(text string) tea.Cmd {
	m.lastUser = text
	m.history = append(m.history, styleUserLabel.Render("you")+"\n"+text)
	m.streaming = true
	m.assistant.Reset()
	m.reasoning.Reset()
	m.toolLines = m.toolLines[:0]
	m.frame = 0

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	prog := m.prog
	client := m.client
	convID := m.convID
	go func() {
		newID, err := client.Stream(ctx, text, convID, func(ev Event) {
			prog.Send(sseMsg{ev: ev})
		})
		prog.Send(turnDoneMsg{convID: newID, err: err})
	}()

	m.refresh()
	m.vp.GotoBottom()
	return spinnerTick()
}

func (m *model) applyEvent(ev Event) {
	switch ev.Name {
	case "conversation":
		if id := ev.Str("id"); id != "" {
			m.convID = id
		}
	case "text.delta":
		m.assistant.WriteString(ev.Str("text"))
	case "reasoning.delta":
		m.reasoning.WriteString(ev.Str("text"))
	case "tool.call":
		name := ev.Str("name")
		if name == "" {
			name = "tool"
		}
		m.toolLines = append(m.toolLines, styleTool.Render("▸ "+name)+styleDim.Render("  …"))
	case "tool.result":
		// Mark the most recent matching tool line as done (best-effort: just append a tick).
		if n := len(m.toolLines); n > 0 {
			done := "ok"
			if isErr, _ := ev.Data["is_err"].(bool); isErr {
				done = "error"
			}
			m.toolLines[n-1] = strings.TrimSuffix(m.toolLines[n-1], styleDim.Render("  …")) + styleDim.Render("  "+done)
		}
	}
}

func (m *model) finishTurn(msg turnDoneMsg) {
	m.streaming = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if msg.convID != "" {
		m.convID = msg.convID
	}
	// Commit the assistant's reply (glamour-rendered) + any tool lines into history.
	var block strings.Builder
	block.WriteString(styleAgentLabel.Render("agent"))
	if len(m.toolLines) > 0 {
		block.WriteString("\n" + strings.Join(m.toolLines, "\n"))
	}
	if txt := strings.TrimSpace(m.assistant.String()); txt != "" {
		block.WriteString("\n" + m.md.render(txt, m.contentWidth()))
	} else if msg.err == nil {
		block.WriteString("\n" + styleDim.Render("(no text response)"))
	}
	m.history = append(m.history, block.String())

	if msg.err != nil {
		if ctxCancelled(msg.err) {
			m.history = append(m.history, styleDim.Render("— turn cancelled —"))
		} else {
			m.statusErr = msg.err.Error()
			m.history = append(m.history, styleErr.Render("error: ")+msg.err.Error())
		}
	}
	m.toolLines = m.toolLines[:0]
}

// refresh rebuilds the viewport content from history + the in-flight turn.
func (m *model) refresh() {
	blocks := append([]string{}, m.history...)
	if m.streaming {
		var live strings.Builder
		live.WriteString(styleAgentLabel.Render("agent") + " " + styleDim.Render(spinnerFrames[m.frame%len(spinnerFrames)]+" working…"))
		if m.showReasoning {
			if r := strings.TrimSpace(m.reasoning.String()); r != "" {
				live.WriteString("\n" + styleDim.Render(indent(r, "  │ ")))
			}
		}
		if len(m.toolLines) > 0 {
			live.WriteString("\n" + strings.Join(m.toolLines, "\n"))
		}
		if txt := m.assistant.String(); txt != "" {
			live.WriteString("\n" + txt) // raw while streaming; glamour on completion
		}
		blocks = append(blocks, live.String())
	}
	m.vp.SetContent(strings.Join(blocks, "\n\n"))
}

func (m *model) contentWidth() int {
	if m.width > 4 {
		return m.width - 2
	}
	return 78
}

func (m *model) View() tea.View {
	header := styleHeader.Render("⚓ fleet chat")
	conv := "new conversation"
	if m.convID != "" {
		conv = "conv " + shortID(m.convID)
	}
	status := styleDim.Render(conv)
	if m.streaming {
		status = styleAccent.Render(spinnerFrames[m.frame%len(spinnerFrames)] + " streaming — Ctrl+C to cancel")
	} else if m.statusErr != "" {
		status = styleErr.Render("⚠ " + m.statusErr)
	}
	body := strings.Join([]string{
		header,
		m.vp.View(),
		status,
		styleAccent.Render("› ") + m.input.View(),
	}, "\n")
	return altView(body)
}

// ── small helpers ──

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// ctxCancelled reports whether err is (or wraps) a context cancellation — i.e.
// the user hit Ctrl+C — so the UI shows "cancelled" rather than a scary error.
func ctxCancelled(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"))
}
