// The dashboard-view context a POST /chat turn carries (docs/DASHBOARD-VIEW.md):
// the sender's board, its queued encoding, and the note appended to the
// injected context. Split out of chat.go.

package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/genui"
)

// chatViewDashboard is the chatRequest.View value the web client sends while
// the sender has the chat open in dashboard view.
const chatViewDashboard = "dashboard"

// dashboardBoardPanel is one panel on the sender's board
// (docs/DASHBOARD-VIEW.md): its title, the card_id of the card it draws, and
// the card's panel key when it has one.
type dashboardBoardPanel struct {
	Title  string `json:"title"`
	CardID string `json:"card_id"`
	Panel  string `json:"panel,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
}

const (
	// maxDashboardBoardPanels bounds the panels named in one note; a board
	// with more lists the first ones and says how many were left out.
	maxDashboardBoardPanels = 24
	maxDashboardTitleRunes  = 80
	maxDashboardCardIDLen   = 256
)

// dashboardViewNote is what the agent reads on a turn sent from dashboard
// view. It points at the show_ui panel key (the tool description holds the
// details) and asks for terse prose, because the board, not the transcript,
// is where the reader is looking.
const dashboardViewNote = "[Dashboard view: the user is reading this chat as a live dashboard — a board of panels beside a narrow chat rail. " +
	"Put numbers, trends and lists they will keep watching in show_ui cards with a \"panel\" key (one topic per panel). " +
	"To update a panel, call show_ui again with the SAME panel key; for a panel that has no key (a card the user pinned), call show_ui with replaces=<its card_id> — it keeps its place on the board, and you may add a panel key then. " +
	"Never restate a panel's numbers in prose. Keep chat replies short: say what changed and name the panel."

// dashboardText makes a browser-supplied label safe to quote on one line:
// control characters (newlines included) become spaces, brackets that could
// close the note are dropped, and the result is trimmed and bounded.
func dashboardText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '[' || r == ']':
			return -1
		case r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxRunes {
		s = string(r[:maxRunes]) + "…"
	}
	return s
}

// dashboardBoardLines renders the board as one line per panel, dropping
// entries without a usable card id or panel key.
func dashboardBoardLines(board []dashboardBoardPanel) []string {
	var lines []string
	skipped := 0
	for _, p := range board {
		id := strings.TrimSpace(p.CardID)
		valid := id != "" && len(id) <= maxDashboardCardIDLen && dashboardText(id, maxDashboardCardIDLen) == id
		if p.Panel != "" && !genui.ValidPanelKey(p.Panel) {
			valid = false
		}
		if !valid {
			continue
		}
		if len(lines) >= maxDashboardBoardPanels {
			skipped++
			continue
		}
		title := dashboardText(p.Title, maxDashboardTitleRunes)
		if title == "" {
			title = "(untitled)"
		}
		how := fmt.Sprintf("no panel key — update with replaces=%q", id)
		if p.Panel != "" {
			how = fmt.Sprintf("panel %q — update with panel %q", p.Panel, p.Panel)
		}
		pinned := ""
		if p.Pinned {
			pinned = ", pinned by the user"
		}
		lines = append(lines, fmt.Sprintf("%d. %q (card_id %s%s): %s", len(lines)+1, title, id, pinned, how))
	}
	if skipped > 0 {
		lines = append(lines, fmt.Sprintf("…and %d more panel(s).", skipped))
	}
	return lines
}

// queuedViewContext is what a queued submission keeps of its view
// (chat_input_queue.view_context, migration 094).
type queuedViewContext struct {
	View  string                `json:"view"`
	Board []dashboardBoardPanel `json:"board,omitempty"`
}

// encodeQueuedViewContext keeps a dashboard-view submission's view and board
// on its queue row, so the turn that drains it carries the same note as a
// direct send. "" for every other submission. The board is trimmed to what
// the note can list.
func encodeQueuedViewContext(req chatRequest) string {
	if req.View != chatViewDashboard {
		return ""
	}
	board := req.Board
	if len(board) > maxDashboardBoardPanels+1 {
		board = board[:maxDashboardBoardPanels+1]
	}
	raw, err := json.Marshal(queuedViewContext{View: req.View, Board: board})
	if err != nil {
		return ""
	}
	return string(raw)
}

// decodeQueuedViewContext reads a queue row's view context back. Anything
// unreadable is no context: the turn runs without a dashboard note.
func decodeQueuedViewContext(raw string) (string, []dashboardBoardPanel) {
	if raw == "" {
		return "", nil
	}
	var vc queuedViewContext
	if json.Unmarshal([]byte(raw), &vc) != nil || vc.View != chatViewDashboard {
		return "", nil
	}
	return vc.View, vc.Board
}

// appendDashboardViewBlock notes a dashboard-view turn in the injected
// context, with the panels on the sender's board so "update that chart" can
// be resolved to a card. Like every injected block it is persisted with the
// user message, so a reload shows exactly what the model was told and later
// turns replay it.
func appendDashboardViewBlock(injected, view string, board []dashboardBoardPanel) string {
	if view != chatViewDashboard {
		return injected
	}
	note := dashboardViewNote
	if lines := dashboardBoardLines(board); len(lines) > 0 {
		note += " Panels on the board now, in order (\"this chart\", \"the second panel\" and the like refer to these):\n" + strings.Join(lines, "\n")
	} else {
		note += " The board is empty."
	}
	return injected + "\n\n" + note + "]"
}
