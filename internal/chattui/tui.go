package chattui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// approvalResolvedMsg reports the outcome of a /approve //deny POST back into
// the model so the transcript can commit what happened.
type approvalResolvedMsg struct {
	card         pendingApproval
	decision     ApprovalDecision
	conversation string
	tool         string
	approved     bool
	status       string // server-reported resolution ("approved" / "rejected")
	resultText   string // the staged tool's outcome text on approve
	model        string // suggest_advanced_model pins this slug server-side
	err          error
}

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
	toolLines []string        // rendered per-tool one-liners this turn
	toolNames []string        // raw tool names, index-aligned with toolLines
	toolIDs   []string
	// approvalLines renders each staged card this turn. Kept OUT of toolLines:
	// tool.result rewrites the LAST tool line on completion, and a staged
	// tool's result arrives AFTER its card — an approval line in toolLines
	// would be clobbered into a bogus "✗ failed" (observed live).
	approvalLines []string
	reasoning     strings.Builder // raw reasoning this turn (shown only when showReasoning)
	cancel        context.CancelFunc
	frame         int

	showReasoning bool
	lastUser      string // for /retry
	statusErr     string // last error line (cleared on next send)

	// Staged critical tools awaiting the human's call. Outlives the turn that
	// staged them (the card is settled after turn.completed), so this is NOT
	// reset per-turn like toolLines.
	pending      []pendingApproval
	reviewBusy   bool
	cardPolicies map[string]map[string][]ApprovalDecision
}

// pendingApproval is one staged approval card as the TUI tracks it: the id the
// resolve endpoint needs, the tool name, and a one-line human summary.
type pendingApproval struct {
	settled        bool // selected one-shot replay; POST can only echo its recorded outcome
	id             string
	tool           string
	summary        string
	details        map[string]any
	patternArgs    map[string]string
	frozenArgs     map[string]any
	frozenComplete bool
	frozenPresent  bool
	edits          *ScheduleEdits
	expiresAt      int64
	executing      bool
	autoBlocked    bool // automatic resolve already failed; reset on reload/retry
}

func newModel(cfg Config) *model {
	ti := textinput.New()
	ti.Prompt = "› "
	st := ti.Styles()
	st.Focused.Prompt = styleAccent
	st.Blurred.Prompt = styleAccent
	ti.SetStyles(st)
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
	m.history = append(m.history, styleDim.Render("Type a message and press Enter. Slash commands: /new /retry /model <slug> /reasoning /approvals /approve /deny /clear /quit."))
	return m
}

// Init requests the terminal background color THROUGH bubbletea (which owns
// stdin and parses the OSC 11 reply correctly). glamour must never auto-query
// the terminal itself mid-session: its raw query races bubbletea's input
// parser, and the stray reply bytes end up typed into the composer — or wedge
// input entirely. (Observed live; the demo recordings caught it.)
func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, tea.RequestBackgroundColor, approvalTick()}
	if m.convID != "" {
		cmds = append(cmds, m.loadApprovals(m.convID))
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case approvalTickMsg:
		if m.expireApprovals(time.Now()) {
			cmds = append(cmds, m.autoResolveCard())
		}
		cmds = append(cmds, approvalTick())
	case approvalsLoadedMsg:
		m.reviewBusy = false
		if msg.err != nil {
			m.reviewNote("Could not load approvals: " + msg.err.Error())
			break
		}
		if m.convID != msg.conversation {
			m.lastUser = ""
		}
		m.convID = msg.conversation
		m.pending = msg.pending
		m.expireApprovals(time.Now())
		m.showApprovals()
		cmds = append(cmds, m.autoResolveCard())
	case tea.BackgroundColorMsg:
		// Resolve glamour's style from the terminal's actual background —
		// via bubbletea's handshake, never glamour's own mid-session query.
		if msg.IsDark() {
			m.md.forceStyle = "dark"
		} else {
			m.md.forceStyle = "light"
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		// Reserve rows: header(1) + rule(1) + status(1) + boxed input(3) + padding(1).
		bodyH := msg.Height - 7
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
		cmds = append(cmds, m.autoResolveCard())

	case approvalResolvedMsg:
		m.finishApproval(msg)
		m.refresh()
		m.vp.GotoBottom()
		cmds = append(cmds, m.autoResolveCard())

	case tea.KeyPressMsg:
		if cmd, consumed := m.onKey(msg); consumed {
			return m, cmd
		}
	}

	// Forward to the sub-components. Keys go ONLY to the textinput: the
	// viewport's default keymap binds plain letters (h/j/k/l, u/d, …) to
	// scrolling, so forwarding typed keys there silently pans the transcript
	// under the user's message (observed as a horizontally-clipped viewport in
	// the demo recording). Scrolling is explicit: PgUp/PgDn in onKey, and
	// non-key events (mouse wheel) still reach the viewport below.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	if _, isKey := msg.(tea.KeyPressMsg); !isKey {
		m.vp, cmd = m.vp.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// onKey returns (cmd, consumed). When consumed is true the key was fully handled
// (enter/ctrl+c/scroll) and must NOT also fall through to the textinput/viewport.
func (m *model) onKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		if m.streaming && m.cancel != nil {
			m.cancel()
			return nil, true
		}
		return nil, false
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
	if text == "" || m.streaming || m.reviewBusy {
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
		// Pending cards belong to the conversation being abandoned; the TUI
		// resolves by convID, so keeping them would target a thread we're no
		// longer on. They stay pending server-side (settle them in the web UI).
		m.pending = m.pending[:0]
		m.history = append(m.history, styleDim.Render("— new conversation — pending cards stay on the previous thread; /approvals reviews this conversation"))
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
			fallback := "server default"
			if m.convID != "" {
				fallback = "stored conversation model"
			}
			m.history = append(m.history, styleDim.Render("usage: /model <slug>  (current: "+orDefault(m.client.turnModel(m.convID), fallback)+")"))
		} else {
			m.client.cfg.Model = fields[1]
			delete(m.client.conversationModels, m.convID)
			m.history = append(m.history, styleDim.Render("— model set to "+fields[1]+" (applies to the next turn) —"))
		}
		m.refresh()
		m.vp.GotoBottom()
		return nil
	case "/approve", "/deny":
		return m.decideApproval(approvalCommandFields(text), fields[0] == "/approve")
	case "/edit":
		return m.editApproval(text)
	case "/resume":
		if len(fields) != 2 {
			return m.reviewNote("usage: /resume <conversation-id>")
		}
		return m.loadApprovals(fields[1])
	case "/approvals":
		if len(fields) == 2 && fields[1] == "reload" && m.convID != "" {
			return m.loadApprovals(m.convID)
		}
		return m.showApprovals()
	case "/retry":
		if m.lastUser == "" {
			m.history = append(m.history, styleDim.Render("— nothing to retry —"))
			m.refresh()
			return nil
		}
		m.resetApprovalAutoBlocked()
		return m.sendTurn(m.lastUser)
	case "/help":
		m.history = append(m.history, strings.Join([]string{
			styleAccent.Render("commands"),
			styleTool.Render("  /new       ") + styleDim.Render("start a fresh conversation"),
			styleTool.Render("  /retry     ") + styleDim.Render("resend your last message"),
			styleTool.Render("  /model <s> ") + styleDim.Render("switch model for the next turn"),
			styleTool.Render("  /reasoning ") + styleDim.Render("toggle live reasoning display"),
			styleTool.Render("  /approve   ") + styleDim.Render("run the oldest pending approval card"),
			styleTool.Render("  /deny      ") + styleDim.Render("refuse the oldest pending approval card"),
			"  /approvals [reload]  review full cards / refresh server state",
			"  /edit {\"name\":\"...\",\"prompt\":\"...\",\"cron\":\"...\"}  edit oldest scheduled-task card",
			"  /approve|/deny [id] [session|pattern arg=glob]  scope this conversation; glob matches raw handler string fields (name/prompt/cron/run_at), not summary aliases",
			"  /resume <id>  switch conversation and load its pending cards",
			styleTool.Render("  /clear     ") + styleDim.Render("clear the transcript"),
			styleTool.Render("  /quit      ") + styleDim.Render("exit (Ctrl+D too) · Esc/Ctrl+C cancels a running turn"),
		}, "\n"))
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
	m.history = append(m.history, stylePillUser.Render("you")+"\n"+text)
	m.streaming = true
	m.assistant.Reset()
	m.reasoning.Reset()
	m.toolLines = m.toolLines[:0]
	m.toolNames = m.toolNames[:0]
	m.toolIDs = m.toolIDs[:0]
	m.approvalLines = m.approvalLines[:0]
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
	case "text.replace":
		m.assistant.Reset()
		m.assistant.WriteString(ev.Str("text"))
	case "reasoning.delta":
		m.reasoning.WriteString(ev.Str("text"))
	case "tool.call":
		name := ev.Str("name")
		if name == "" {
			name = "tool"
		}
		m.toolNames = append(m.toolNames, name)
		m.toolIDs = append(m.toolIDs, ev.Str("id"))
		m.toolLines = append(m.toolLines, styleTool.Render("⏺ "+name)+styleDim.Render(" running…"))
	case "tool.result":
		// Mark the most recent tool line as done (best-effort: calls resolve in
		// order on this stream) with a colored outcome glyph.
		if n := m.toolResultIndex(ev) + 1; n > 0 {
			name := "tool"
			if len(m.toolNames) >= n {
				name = m.toolNames[n-1]
			}
			// A staged critical tool resolves its call with an is_err
			// APPROVAL_REQUIRED sentinel — that is a PAUSE for human review,
			// not a failure, and rendering ✗ would tell the user the action
			// died when it is actually waiting on them.
			if strings.HasPrefix(ev.Str("text"), "APPROVAL_REQUIRED:") {
				m.toolLines[n-1] = styleTool.Render("⏸ "+name) + styleDim.Render(" awaiting approval")
			} else if isErr, _ := ev.Data["is_err"].(bool); isErr {
				m.toolLines[n-1] = styleToolErr.Render("✗ "+name) + styleDim.Render(" failed")
			} else {
				m.toolLines[n-1] = styleToolOK.Render("✓ "+name) + styleDim.Render(" done")
			}
		}
	case "tool.approval_required":
		// A critical tool staged its card. Track it (it outlives this turn) and
		// show a transcript line with the human-decision commands — the web
		// renders a card here; the terminal renders text.
		tool := orDefault(ev.Str("tool"), "tool")
		ap := pendingApproval{
			id:          ev.Str("approval_id"),
			tool:        tool,
			summary:     approvalSummaryLine(tool, ev.Data["summary"]),
			patternArgs: parsePatternArgs(ev.Data["pattern_args"]),
		}
		ap.details, _ = ev.Data["summary"].(map[string]any)
		ap.frozenArgs, ap.frozenComplete, ap.frozenPresent = parseFrozenArgs(ev.Data["frozen_args"])
		if expiry, ok := ev.Data["expires_at"].(float64); ok {
			ap.expiresAt = int64(expiry)
		}
		m.pending = append(m.pending, ap)
		line := styleTool.Render("⚠ " + tool + " needs approval")
		if ap.summary != "" {
			line += styleDim.Render(" — " + ap.summary)
		}
		line += styleDim.Render("  (/approve · /deny)")
		m.approvalLines = append(m.approvalLines, line)
		m.approvalLines = append(m.approvalLines, frozenApprovalReview(ap))
	case "tool.approval_superseded":
		// The agent re-staged the same tool; the server voided the older card.
		// Drop it so /approve can never settle a dead approval.
		tool := ev.Str("tool")
		kept := m.pending[:0]
		for _, ap := range m.pending {
			if ap.tool != tool || ap.executing {
				kept = append(kept, ap)
			}
		}
		m.pending = kept
	}
}

// resolveNextApproval settles the OLDEST pending card (FIFO matches the
// transcript's top-to-bottom reading order) and returns a command that POSTs
// the decision and reports back via approvalResolvedMsg. Approving runs the
// staged tool server-side, which can take seconds — hence async, never a
// blocking call on the UI goroutine.
// finishApproval commits an approval resolution to the transcript: the staged
// tool's own outcome text on approve (task id, send confirmation — the same
// text the web card shows), a short refusal line on deny, or the error.
func (m *model) finishApproval(msg approvalResolvedMsg) {
	m.reviewBusy = false
	// Session scope records the user's consent, independently of whether the
	// authorized action succeeded. A transport error with no authoritative
	// status must not install a policy.
	consentRecorded := (msg.approved && msg.status == "approved") || (!msg.approved && msg.status == "rejected")
	if consentRecorded && handlerOnlyCard(msg.tool) && msg.decision.Scope != "" {
		if m.cardPolicies == nil {
			m.cardPolicies = map[string]map[string][]ApprovalDecision{}
		}
		if m.cardPolicies[msg.conversation] == nil {
			m.cardPolicies[msg.conversation] = map[string][]ApprovalDecision{}
		}
		d := msg.decision
		d.Edits = nil
		m.cardPolicies[msg.conversation][msg.tool] = append(m.cardPolicies[msg.conversation][msg.tool], d)
	}
	if msg.err != nil {
		if msg.status == "" && msg.card.id != "" {
			card := msg.card
			var running approvalRunningError
			if errors.As(msg.err, &running) {
				card.executing = true
			}
			card.autoBlocked = true
			m.pending = append([]pendingApproval{card}, m.pending...)
		}
		m.statusErr = sanitizeTerminalText(msg.err.Error())
		m.history = append(m.history, styleErr.Render("error: ")+sanitizeTerminalText(msg.err.Error()))
		return
	}
	if !msg.approved {
		m.history = append(m.history, styleToolErr.Render("✗ "+msg.tool)+styleDim.Render(" denied"))
		return
	}
	if slug := strings.TrimSpace(msg.model); slug != "" && m.client != nil {
		if m.client.conversationModels == nil {
			m.client.conversationModels = make(map[string]string)
		}
		m.client.conversationModels[m.convID] = slug
	}
	block := styleToolOK.Render("✓ " + msg.tool + " approved")
	if t := strings.TrimSpace(msg.resultText); t != "" {
		block += "\n" + styleDim.Render(sanitizeTerminalText(t))
	}
	m.history = append(m.history, block)
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
	block.WriteString(stylePillAgent.Render("agent"))
	if len(m.toolLines) > 0 {
		block.WriteString("\n" + strings.Join(m.toolLines, "\n"))
	}
	if len(m.approvalLines) > 0 {
		block.WriteString("\n" + strings.Join(m.approvalLines, "\n"))
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
	m.toolNames = m.toolNames[:0]
	m.toolIDs = m.toolIDs[:0]
	m.approvalLines = m.approvalLines[:0]
}

// refresh rebuilds the viewport content from history + the in-flight turn.
func (m *model) refresh() {
	blocks := append([]string{}, m.history...)
	if m.streaming {
		var live strings.Builder
		live.WriteString(stylePillAgent.Render("agent") + " " + styleAccent.Render(spinnerFrames[m.frame%len(spinnerFrames)]) + styleDim.Render(" working…"))
		if m.showReasoning {
			if r := strings.TrimSpace(m.reasoning.String()); r != "" {
				live.WriteString("\n" + styleDim.Render(indent(r, "  │ ")))
			}
		}
		if len(m.toolLines) > 0 {
			live.WriteString("\n" + strings.Join(m.toolLines, "\n"))
		}
		if len(m.approvalLines) > 0 {
			live.WriteString("\n" + strings.Join(m.approvalLines, "\n"))
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

func (m *model) View() tea.View { return altView(m.render()) }

// render builds the full ANSI frame (header · rule · viewport · status · boxed
// input). Split out from View so the deterministic screenshot/gif generators
// (#487, #540) can capture the exact frame the alt-screen shows, without a
// Program or terminal.
func (m *model) render() string {
	conv := "new conversation"
	if m.convID != "" {
		conv = "conv " + shortID(m.convID)
	}
	right := conv
	if mdl := strings.TrimSpace(m.client.displayModel(m.convID)); mdl != "" {
		right = mdl + " · " + conv
	}
	header := barLine(m.width, styleHeader.Render("⚓ fleet chat"), styleDim.Render(right))

	status := styleDim.Render("ready")
	switch {
	case m.streaming:
		status = styleAccent.Render(spinnerFrames[m.frame%len(spinnerFrames)]+" streaming") + styleDim.Render(" — Esc/Ctrl+C to cancel")
	case m.reviewBusy:
		status = styleTool.Render("resolving approval / loading conversation…")
	case len(m.pending) > 0:
		n := strconv.Itoa(len(m.pending))
		status = styleTool.Render("⚠ "+n+" approval"+pluralS(len(m.pending))+" pending") + styleDim.Render(" — /approve · /deny")
		for _, a := range m.pending {
			if a.executing {
				status = styleTool.Render("⚠ "+n+" approval card"+pluralS(len(m.pending))+" (includes running actions)") + styleDim.Render(" — /approvals")
				break
			}
		}
	case m.statusErr != "":
		status = styleErr.Render("⚠ " + m.statusErr)
	}
	statusBar := barLine(m.width, status, styleDim.Render("PgUp/PgDn scroll · /help"))

	box := styleInputBox
	if m.streaming {
		box = styleInputBoxBusy
	}
	inputW := m.width - 6 // box borders(2) + padding(2) + prompt glyph(2)
	if inputW < 20 {
		inputW = 20
	}
	m.input.SetWidth(inputW)
	inputBox := box.Width(maxInt(m.width-2, 20)).Render(m.input.View())

	return strings.Join([]string{
		header,
		rule(m.width),
		m.vp.View(),
		statusBar,
		inputBox,
	}, "\n")
}

// barLine lays out left + right segments on one row, padding the middle to the
// full width (falling back to a single space when the terminal is too narrow).
func barLine(width int, left, right string) string {
	gap := width - lipglossWidth(left) - lipglossWidth(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── small helpers ──

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
