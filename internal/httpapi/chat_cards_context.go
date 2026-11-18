// The cards a conversation is showing, read from its stored history
// (docs/DASHBOARD-VIEW.md, "Cards a turn did not draw"). Two things are built
// on it: the cards of another sender's turn that the model replay keeps
// (redactHistoryForSender), and the list an in-chat patrol turn is given.
// Split out of chat.go.

package httpapi

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/genui"
)

// maxReplayedSharedCards bounds the cards of other senders' turns that one
// replay carries: the most recently shown ones. A chat a patrol has been
// redrawing for months must not grow every participant's context without
// limit; older cards are still on screen, and the model can be asked to show
// them again.
const maxReplayedSharedCards = 48

// conversationCardsNote opens the list a patrol turn is given. It is about
// updating: the reader of a shared board is hurt most by a second panel for a
// topic that already has one.
const conversationCardsNote = "[Cards in this chat: these show_ui cards are on screen for everyone in the chat (in dashboard view they are the board's panels). " +
	"Some were drawn on turns that are no longer in your context, so trust this list over your memory of the conversation. " +
	"To update one, call show_ui with the SAME panel key, or with replaces=<its card_id> when it has no key; never draw a second card for a topic that already has one. " +
	"Cards now, oldest first:\n"

// shownCard is one show_ui card the conversation displayed.
type shownCard struct {
	callIdx, resultIdx int // positions in the history it was read from
	id                 string
	title              string
	// panel is the card's dashboard panel key: its own, or the one it
	// inherited from the keyed card it replaced.
	panel string
	// spec is the card as drawn: the call's input, or for a card drawn by
	// reference (from_result) the validated spec kept on its result.
	spec string
	// current is false once a later card took its place: one that names it in
	// replaces, one with the same panel key, or one reusing its id.
	current bool
	// onBoard reports whether dashboard view draws the card as a panel
	// rather than leaving it in the chat rail.
	onBoard bool
	// slot orders board cards by when their panel first appeared.
	slot int
}

// shownCardSpec is what the derivation reads of a card's spec.
type shownCardSpec struct {
	Title    string `json:"title"`
	Panel    string `json:"panel"`
	Replaces string `json:"replaces"`
}

// conversationShownCards derives every card a conversation displayed from its
// FULL stored history, every sender's turns included, in the order shown, and
// marks the ones that are still current. The rules are the web's
// (deriveGenUiState and deriveBoard in web/src/app/chat/ui), without the
// per-browser layout: a later card with the same panel key, a card whose
// replaces names a current card, and a later card reusing an id each take the
// earlier card's place; a card is on the board when it has a panel key, when
// it replaced a card on the board, or when it is display-only.
//
// Only cards that were shown count (sharedCardEntries): a refused or
// unanswered call drew nothing. Cards are followed by occurrence, never by
// raw id alone, because a provider can reuse a tool-call id. One pass, with
// map lookups, so a long history costs time linear in its length.
func conversationShownCards(history []agent.HistoryEntry) []shownCard {
	shown := sharedCardEntries(history)
	if len(shown) == 0 {
		return nil
	}
	cards := make([]shownCard, 0, len(shown)/2)
	// call id → the shown calls still waiting for their result, oldest first
	// (the pairing sharedCardEntries made).
	type pendingCall struct {
		idx   int
		input string
	}
	pending := make(map[string][]pendingCall)
	live := make(map[string]int)       // card id → its latest occurrence
	panelOwner := make(map[string]int) // panel key → the card showing it
	slots := 0
	for i, e := range history {
		if !shown[i] {
			continue
		}
		if e.Type == "tool_call" {
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) == nil {
				pending[c.ID] = append(pending[c.ID], pendingCall{idx: i, input: c.Input})
			}
			continue
		}
		var res agent.ToolResultContent
		if json.Unmarshal(e.Content, &res) != nil || len(pending[res.ID]) == 0 {
			continue
		}
		call := pending[res.ID][0]
		pending[res.ID] = pending[res.ID][1:]

		// A card drawn by reference keeps its validated spec on the result;
		// the call's input then holds only the reference and its overrides.
		var spec, overrides shownCardSpec
		_ = json.Unmarshal([]byte(call.input), &overrides)
		card := shownCard{callIdx: call.idx, resultIdx: i, id: res.ID, spec: call.input, current: true}
		if res.Card != "" {
			card.spec = res.Card
			_ = json.Unmarshal([]byte(res.Card), &spec)
		} else {
			spec = overrides
		}
		card.title = spec.Title
		card.panel = strings.TrimSpace(cmp.Or(spec.Panel, overrides.Panel))
		replaces := strings.TrimSpace(cmp.Or(spec.Replaces, overrides.Replaces))

		// The card this one refines, only while that card is still current: a
		// card never refines itself or a version something already replaced.
		replaced := -1
		if r, ok := live[replaces]; ok && replaces != card.id && cards[r].current {
			replaced = r
		}
		if prev, ok := live[card.id]; ok {
			cards[prev].current = false
		}
		if replaced >= 0 {
			cards[replaced].current = false
		}
		n := len(cards)
		live[card.id] = n
		switch {
		case card.panel != "":
			card.onBoard = true
			if owner, ok := panelOwner[card.panel]; ok {
				cards[owner].current = false
				card.slot = cards[owner].slot
			} else if replaced >= 0 && cards[replaced].onBoard {
				// A refresh that gave the card a key: the slot moves to it.
				card.slot = cards[replaced].slot
			} else {
				card.slot = slots
				slots++
			}
		case replaced >= 0 && cards[replaced].onBoard:
			// It inherits the replaced card's slot, and its key if it had one.
			card.onBoard, card.slot, card.panel = true, cards[replaced].slot, cards[replaced].panel
		case genui.IsDisplayOnly([]byte(card.spec)):
			card.onBoard, card.slot = true, slots
			slots++
		}
		if card.panel != "" {
			panelOwner[card.panel] = n
		}
		cards = append(cards, card)
	}
	return cards
}

// currentBoardCards lists the cards dashboard view shows as panels now, in
// the order their panels first appeared, bounded to the ones updated most
// recently (a long-lived chat can hold more than a note should name).
func currentBoardCards(cards []shownCard) []dashboardBoardPanel {
	board := make([]shownCard, 0, len(cards))
	for _, c := range cards {
		if c.current && c.onBoard {
			board = append(board, c)
		}
	}
	// cards are in the order shown, so the tail is the most recently updated.
	if len(board) > maxDashboardBoardPanels {
		board = board[len(board)-maxDashboardBoardPanels:]
	}
	slices.SortStableFunc(board, func(a, b shownCard) int { return a.slot - b.slot })
	out := make([]dashboardBoardPanel, 0, len(board))
	for _, c := range board {
		out = append(out, dashboardBoardPanel{Title: c.title, CardID: c.id, Panel: c.panel})
	}
	return out
}

// replayedSharedCards returns, by history index, the entries of the cards a
// replay keeps from turns its sender did not send: the current cards, most
// recently shown first up to maxReplayedSharedCards. For a card drawn by
// reference the map also carries the spec to replay as the call's input,
// because the result it referred to is another sender's and is not replayed.
func replayedSharedCards(history []agent.HistoryEntry) map[int]string {
	cards := conversationShownCards(history)
	keep := make(map[int]string)
	kept := 0
	for i := len(cards) - 1; i >= 0 && kept < maxReplayedSharedCards; i-- {
		c := cards[i]
		if !c.current {
			continue
		}
		kept++
		keep[c.callIdx] = c.spec
		keep[c.resultIdx] = ""
	}
	return keep
}

// appendConversationCardsBlock names the conversation's current board cards
// on an in-chat patrol turn. It runs unattended, often long after the turn
// that drew a panel and after a compaction dropped that turn from its
// context, and it carries no dashboard-view note because no browser sent it.
// Other turns need no list: their replay holds the cards, other senders'
// included (redactHistoryForSender), and a dashboard-view turn brings the
// sender's own board.
//
// Nothing is added when the chat shows no board cards, so a patrol in a chat
// that never used them reads exactly as before. Titles are sanitized and the
// list bounded the same way as the dashboard-view note (dashboardBoardLines).
func appendConversationCardsBlock(injected, inputKind string, fullHistory []agent.HistoryEntry) string {
	if inputKind != agent.InputKindChatPatrol {
		return injected
	}
	lines := dashboardBoardLines(currentBoardCards(conversationShownCards(fullHistory)))
	if len(lines) == 0 {
		return injected
	}
	return injected + "\n\n" + conversationCardsNote + strings.Join(lines, "\n") + "]"
}
