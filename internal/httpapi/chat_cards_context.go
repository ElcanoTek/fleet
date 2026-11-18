// The cards a conversation is showing, as a note for turns whose model cannot
// see them in its own replay (docs/DASHBOARD-VIEW.md, "Cards a turn did not
// draw"): an in-chat patrol turn, and a turn in a multiplayer chat that was
// not sent from dashboard view. Split out of chat.go.

package httpapi

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// conversationCardsNote opens the note. It is deliberately about updating:
// the reader of a shared board is hurt most by a second panel for a topic
// that already has one.
const conversationCardsNote = "[Cards in this chat: these show_ui cards are on screen for everyone in the chat (in dashboard view they are the board's panels). " +
	"Some were drawn on turns you cannot see — a teammate's, a patrol's, or one from before a summary — so trust this list over your memory of the conversation. " +
	"To update one, call show_ui with the SAME panel key, or with replaces=<its card_id> when it has no key; never draw a second card for a topic that already has one. " +
	"Cards now, oldest first:\n"

// shownCardSpec is what the note needs of a shown card's spec.
type shownCardSpec struct {
	Title    string `json:"title"`
	Panel    string `json:"panel"`
	Replaces string `json:"replaces"`
}

// cardSlot is one card on screen: the newest card of a panel key, or a
// keyless card nothing has replaced.
type cardSlot struct {
	panel   string
	cardID  string
	title   string
	updated int // position of the card that last took the slot
}

// currentConversationCards derives the cards a conversation is showing from
// its FULL stored history, every sender's turns included: the same rule the
// web applies to the transcript (deriveBoard in
// web/src/app/chat/ui/dashboard/board.ts), reduced to what a note can name. A
// later card with the same panel key takes the slot over; a card whose
// replaces names a listed card takes that card's slot; a keyless form is a
// step in the conversation, not something to keep on screen, so it is not
// listed (and a card it replaces leaves the list).
//
// Only cards that were shown count (sharedCardEntries): a refused or
// unanswered call drew nothing. Titles, keys and ids of shown cards are
// already delivered to every participant, so naming them to any participant's
// turn discloses nothing new.
func currentConversationCards(history []agent.HistoryEntry) []dashboardBoardPanel {
	shown := sharedCardEntries(history)
	if len(shown) == 0 {
		return nil
	}
	var slots []cardSlot
	find := func(match func(cardSlot) bool) int {
		return slices.IndexFunc(slots, match)
	}
	inputs := make(map[string]string) // call id → the shown call's input
	for i, e := range history {
		if !shown[i] {
			continue
		}
		if e.Type == "tool_call" {
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) == nil {
				inputs[c.ID] = c.Input
			}
			continue
		}
		var res agent.ToolResultContent
		if json.Unmarshal(e.Content, &res) != nil {
			continue
		}
		// A card drawn by reference keeps its validated spec on the result;
		// the call's input then holds only the reference and its overrides.
		raw := inputs[res.ID]
		delete(inputs, res.ID)
		var spec, overrides shownCardSpec
		_ = json.Unmarshal([]byte(raw), &overrides)
		if res.Card != "" {
			_ = json.Unmarshal([]byte(res.Card), &spec)
		} else {
			spec = overrides
		}
		if spec.Panel == "" {
			spec.Panel = overrides.Panel
		}
		if spec.Replaces == "" {
			spec.Replaces = overrides.Replaces
		}
		specJSON := []byte(raw)
		if res.Card != "" {
			specJSON = []byte(res.Card)
		}
		next := cardSlot{panel: strings.TrimSpace(spec.Panel), cardID: res.ID, title: spec.Title, updated: i}
		replaced := -1
		if rep := strings.TrimSpace(spec.Replaces); rep != "" {
			replaced = find(func(s cardSlot) bool { return s.cardID == rep })
		}
		switch {
		case next.panel != "":
			keyed := find(func(s cardSlot) bool { return s.panel == next.panel })
			switch {
			case keyed >= 0:
				slots[keyed] = next
				// The refined card's old slot merges into the panel it joined.
				if replaced >= 0 && replaced != keyed {
					slots = slices.Delete(slots, replaced, replaced+1)
				}
			case replaced >= 0:
				slots[replaced] = next
			default:
				slots = append(slots, next)
			}
		case replaced >= 0 && cardHasInteractiveAction(specJSON):
			slots = slices.Delete(slots, replaced, replaced+1)
		case replaced >= 0:
			next.panel = slots[replaced].panel
			slots[replaced] = next
		case !cardHasInteractiveAction(specJSON):
			slots = append(slots, next)
		}
	}
	// A long-lived chat can hold more cards than a note should name: keep the
	// ones updated most recently, in the order they first appeared.
	if len(slots) > maxDashboardBoardPanels {
		byRecency := slices.Clone(slots)
		slices.SortFunc(byRecency, func(a, b cardSlot) int { return b.updated - a.updated })
		cutoff := byRecency[maxDashboardBoardPanels-1].updated
		slots = slices.DeleteFunc(slots, func(s cardSlot) bool { return s.updated < cutoff })
	}
	out := make([]dashboardBoardPanel, 0, len(slots))
	for _, s := range slots {
		out = append(out, dashboardBoardPanel{Title: s.title, CardID: s.cardID, Panel: s.panel})
	}
	return out
}

// appendConversationCardsBlock names the conversation's current cards on a
// turn whose replay may not hold them:
//
//   - an in-chat patrol turn. It runs unattended, often long after the turn
//     that drew a panel (and after a compaction dropped it from the replay),
//     and it carries no dashboard-view note because no browser sent it.
//   - a turn in a multiplayer chat that was not sent from dashboard view (that
//     view sends the sender's own board, which already lists every card). The
//     model replay omits another sender's working trace, cards included
//     (redactHistoryForSender), so without the list a teammate's "update the
//     pacing card" has no card to resolve to.
//
// Nothing is added when the chat shows no cards, so a chat that never used
// them reads exactly as before. Titles are sanitized and the list bounded the
// same way as the dashboard-view note (dashboardBoardLines).
func appendConversationCardsBlock(injected string, conv *store.Conversation, user, inputKind, view string, fullHistory []agent.HistoryEntry) string {
	switch {
	case inputKind == agent.InputKindChatPatrol:
	case inputKind == "" && view != chatViewDashboard && isSharedTurn(conv, user):
	default:
		return injected
	}
	lines := dashboardBoardLines(currentConversationCards(fullHistory))
	if len(lines) == 0 {
		return injected
	}
	return injected + "\n\n" + conversationCardsNote + strings.Join(lines, "\n") + "]"
}

// isSharedTurn reports whether a turn runs in a conversation more than one
// person sends to: a multiplayer chat, or any chat the sender does not own.
func isSharedTurn(conv *store.Conversation, user string) bool {
	if conv == nil {
		return false
	}
	return conv.Multiplayer || (conv.UserEmail != "" && !strings.EqualFold(conv.UserEmail, user))
}
