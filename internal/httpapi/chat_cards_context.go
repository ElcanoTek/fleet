// The cards a conversation is showing, read from its stored history
// (docs/DASHBOARD-VIEW.md, "Cards a turn did not draw"). Two things are built
// on it: the cards of another sender's turn that the model replay keeps
// (redactHistoryForSender), and the list of current board panels an in-chat
// patrol turn is given.
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
			// The refined card's slot moved to this key (or merged into it),
			// so its old key names no panel any more: a later card that uses
			// that key opens a new panel, placed as new.
			if replaced >= 0 && cards[replaced].panel != card.panel {
				delete(panelOwner, cards[replaced].panel)
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

// sharedCardRef names one shown card, with the spec to replay as its call's
// input.
type sharedCardRef struct {
	ordinal int
	id      string
	spec    string
}

// shownCardEntries maps the history index of each shown card's call and
// result to the card. A replay keeps these entries on turns its sender did
// not send (redactHistoryForSender). Every shown card is kept, replaced ones
// included, exactly as a sender's replay keeps all of their own cards: the
// replay must only ever grow at its end. The compaction records a turn
// persists are positions in that turn's replay (agent.replayHistory), so a
// card that dropped out of an earlier position later would make them cut in
// the wrong place. The spec is the card as drawn: for a card drawn by
// reference it replaces the call's input, because the result the reference
// named is another sender's and is not replayed.
func shownCardEntries(history []agent.HistoryEntry) map[int]sharedCardRef {
	cards := conversationShownCards(history)
	out := make(map[int]sharedCardRef, 2*len(cards))
	for n, c := range cards {
		ref := sharedCardRef{ordinal: n, id: c.id, spec: c.spec}
		out[c.callIdx], out[c.resultIdx] = ref, ref
	}
	return out
}

// replayedCardEntry is one entry of another sender's card in a replay being
// built: where it sits in the output, and which card it belongs to.
type replayedCardEntry struct {
	pos  int
	card sharedCardRef
}

// dropCollidingSharedCards removes from out another sender's card whose call
// id is also the id of a tool call the replay holds from the sender's own
// turns since the latest summary. The replay pairs calls with results by id
// alone (agent.replayHistory), so keeping both would hand one of them the
// other's result. The card stays on screen; only the model's copy is
// withheld. Its id cannot be rewritten instead, because the id is what the
// model passes back in replaces. Withholding is the safe side of the choice:
// the scan reads the entries before per-turn compaction is applied, so it can
// withhold a card whose rival call a compaction later removes, and never the
// reverse.
func dropCollidingSharedCards(out []agent.HistoryEntry, kept []replayedCardEntry) []agent.HistoryEntry {
	if len(kept) == 0 {
		return out
	}
	foreign := make(map[int]bool, len(kept)) // positions in out
	for _, k := range kept {
		foreign[k.pos] = true
	}
	// Only calls the model will actually be sent can collide: the replay
	// starts at the latest summary, so a call before it pairs with nothing.
	start := 0
	for pos := len(out) - 1; pos >= 0; pos-- {
		if out[pos].Type == "summary" {
			start = pos
			break
		}
	}
	ownCallIDs := make(map[string]bool)
	for pos := start; pos < len(out); pos++ {
		e := out[pos]
		if foreign[pos] || e.Type != "tool_call" {
			continue
		}
		var c agent.ToolCallContent
		if json.Unmarshal(e.Content, &c) == nil {
			ownCallIDs[c.ID] = true
		}
	}
	drop := make(map[int]bool)
	for _, k := range kept {
		if ownCallIDs[k.card.id] {
			drop[k.pos] = true
		}
	}
	if len(drop) == 0 {
		return out
	}
	trimmed := make([]agent.HistoryEntry, 0, len(out)-len(drop))
	for pos, e := range out {
		if !drop[pos] {
			trimmed = append(trimmed, e)
		}
	}
	return trimmed
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
