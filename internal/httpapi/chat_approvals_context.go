// The approval cards waiting in a conversation, as an in-chat patrol turn is
// told them (docs/CHAT-PATROLS.md, ADR-0120). Split out of chat.go.

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

const (
	// maxWaitingApprovalsListed bounds the cards named one per line; the rest
	// are counted by tool (a batch can stage one card per record, so a chat
	// can hold dozens).
	maxWaitingApprovalsListed = 12
	// maxWaitingApprovalTools bounds the tools named in that count.
	maxWaitingApprovalTools = 8
	// Text bounds, in runes, for one listed card.
	maxWaitingApprovalToolRunes    = 120
	maxWaitingApprovalSummaryRunes = 200
	maxWaitingApprovalArgRunes     = 60
)

// waitingApprovalsNote opens the list. A scheduled run is no longer held
// while a card waits (ADR-0120), so the run has to be told what is waiting:
// the staged call may be on a turn a compaction dropped from its context, a
// new card for the same tool replaces the waiting one (the stager's
// same-tool supersede), a chat that runs the tool without asking would do
// the action a second time, and a summary that reads the staged call as
// done misreports it.
//
// The supersede is what a recurring run wants for its OWN earlier card
// (today's report in place of yesterday's, not one stale card per run) and
// what it must not do to a card a person asked for and has not answered:
// nobody is there to see it go. Fleet does not record which kind of turn
// staged a card, so the note states the rule and the run applies it from
// the conversation. A tool whose cards are never superseded (schedule_task:
// supersedesEarlierCards) says so on its line, because there the "bring it up
// to date" call would add a second card for the same task instead.
// %s is when the list was read.
const waitingApprovalsNote = "[Approval cards waiting in this chat: the actions below were staged on earlier turns and are still waiting for a person to approve or decline them. None of them has run. " +
	"Do not stage or run the same action again in this run, and where you report status say they are waiting for approval, not done. " +
	"Unless its line says otherwise, a new card for one of these tools replaces the card waiting for that tool: the earlier card is rejected and never runs. " +
	"So use such a tool in this run only to bring an earlier scheduled run's own card up to date (today's report in place of yesterday's). " +
	"Never replace a card a person asked for, or one you cannot tell apart from that: leave the tool for a later run and say what is waiting behind the card. " +
	"If you do replace a card, say which one in your summary. " +
	"Waiting as of %s, oldest first:\n"

// waitingApprovalTime is how the note writes a time: UTC, to the minute.
func waitingApprovalTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// waitingDecisionCards keeps the cards that are a decision still open to a
// person at now, oldest first. An email preview is display-only (Dismiss is
// its one action) and a model suggestion is superseded when the patrol turn
// is queued, so neither is listed; nor is a card past its deadline, which
// can no longer be approved (the expiry sweep denies it).
func waitingDecisionCards(pending []store.Approval, now time.Time) []store.Approval {
	out := make([]store.Approval, 0, len(pending))
	for _, a := range pending {
		if a.Status != "" && a.Status != "pending" {
			continue
		}
		if a.ToolName == previewEmailToolName || a.ToolName == tools.SuggestAdvancedModelToolName {
			continue
		}
		if a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix() {
			continue
		}
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(x, y store.Approval) int {
		switch {
		case x.CreatedAt < y.CreatedAt:
			return -1
		case x.CreatedAt > y.CreatedAt:
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// waitingApprovalSummary is one line saying what a waiting card would do:
// the title (and subtitle) of its describer card when it has one
// (docs/APPROVAL-CARD-DESCRIBERS.md), else its top-level arguments as
// key=value pairs. Both are bounded and made safe to quote
// (dashboardText). "" when there is nothing usable, or when the secret
// redaction would alter the arguments: the line then names the tool only.
func waitingApprovalSummary(a store.Approval) string {
	if raw := approvalCardPayload(&a); raw != nil {
		var card ApprovalCard
		if json.Unmarshal(raw, &card) == nil {
			title := dashboardText(card.Title, maxWaitingApprovalSummaryRunes)
			if sub := dashboardText(card.Subtitle, maxWaitingApprovalSummaryRunes); title != "" && sub != "" {
				title = dashboardText(title+" — "+sub, maxWaitingApprovalSummaryRunes)
			}
			if title != "" {
				return title
			}
		}
	}
	if a.ArgsJSON == "" || tools.RedactionWouldAlter([]byte(a.ArgsJSON)) {
		return ""
	}
	var args map[string]any
	if json.Unmarshal([]byte(a.ArgsJSON), &args) != nil || len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		key := dashboardText(k, maxWaitingApprovalArgRunes)
		val := dashboardText(displayArgValue(args[k]), maxWaitingApprovalArgRunes)
		if key == "" || val == "" {
			continue
		}
		parts = append(parts, key+"="+val)
	}
	return dashboardText(strings.Join(parts, "; "), maxWaitingApprovalSummaryRunes)
}

// What a waiting card's line (and the count of the cards not listed) adds
// when a new call to its tool would NOT replace it (supersedesEarlierCards):
// the run must not make the call to bring the card up to date, or the chat
// ends up with two cards for one thing.
const (
	waitingApprovalStacksLine  = "; a new call to this tool does NOT replace this card, it adds another beside it: do not call it again for the same thing"
	waitingApprovalStacksCount = " (a new call does NOT replace these, it adds another: do not call it again for the same thing)"
)

// waitingApprovalStacks is note when a new card for toolName is added beside
// the waiting ones, "" when it replaces them.
func waitingApprovalStacks(toolName, note string) string {
	if supersedesEarlierCards(toolName) {
		return ""
	}
	return note
}

// waitingApprovalLines renders the list: the oldest maxWaitingApprovalsListed
// cards one per line, then the rest counted by tool.
func waitingApprovalLines(cards []store.Approval) []string {
	lines := make([]string, 0, min(len(cards), maxWaitingApprovalsListed)+1)
	rest := map[string]int{}
	restTotal := 0
	for _, a := range cards {
		tool := dashboardText(a.ToolName, maxWaitingApprovalToolRunes)
		if tool == "" {
			tool = "(unnamed tool)"
		}
		if len(lines) >= maxWaitingApprovalsListed {
			rest[tool]++
			restTotal++
			continue
		}
		line := fmt.Sprintf("%d. %s", len(lines)+1, tool)
		if summary := waitingApprovalSummary(a); summary != "" {
			line += fmt.Sprintf(": %q", summary)
		}
		expires := "does not expire"
		if a.ExpiresAt > 0 {
			expires = "expires " + waitingApprovalTime(a.ExpiresAt)
		}
		line += fmt.Sprintf(" (approval_id %s; staged %s; %s%s)",
			dashboardText(a.ID, maxDashboardCardIDLen), waitingApprovalTime(a.CreatedAt), expires, waitingApprovalStacks(a.ToolName, waitingApprovalStacksLine))
		lines = append(lines, line)
	}
	if restTotal > 0 {
		names := make([]string, 0, len(rest))
		for name := range rest {
			names = append(names, name)
		}
		// Most cards first, then by name, so the line is the same every time.
		sort.Slice(names, func(i, j int) bool {
			if rest[names[i]] != rest[names[j]] {
				return rest[names[i]] > rest[names[j]]
			}
			return names[i] < names[j]
		})
		shown := names
		if len(shown) > maxWaitingApprovalTools {
			shown = shown[:maxWaitingApprovalTools]
		}
		counts := make([]string, 0, len(shown)+1)
		for _, name := range shown {
			counts = append(counts, fmt.Sprintf("%s ×%d%s", name, rest[name], waitingApprovalStacks(name, waitingApprovalStacksCount)))
		}
		if len(names) > len(shown) {
			counts = append(counts, "other tools")
		}
		lines = append(lines, fmt.Sprintf("…and %d more waiting: %s.", restTotal, strings.Join(counts, ", ")))
	}
	return lines
}

// memoriesAndWaitingApprovals reads what startTurn needs of the sender before
// it registers a turn: their memories and, for an in-chat patrol turn, their
// pending approval cards in the conversation. A patrol turn runs whether or
// not a card is waiting on its owner (ADR-0120), so it is told which are
// (appendWaitingApprovalsBlock). Either read failing fails the launch (a
// drained row goes back to the queue and is tried again): a run that does
// not know a card is waiting can replace it or report its action as done.
func (s *Server) memoriesAndWaitingApprovals(ctx context.Context, user, convID, inputKind string) ([]store.Memory, []store.Approval, error) {
	memories, err := s.store.ListMemories(ctx, user)
	// No user, no list: the store reads an empty email as "everyone's cards".
	if err != nil || inputKind != agent.InputKindChatPatrol || strings.TrimSpace(user) == "" {
		return memories, nil, err
	}
	waiting, err := s.store.ListPendingApprovals(ctx, user, convID)
	return memories, waiting, err
}

// appendWaitingApprovalsBlock tells an in-chat patrol turn which approval
// cards are waiting on the person it runs as. pending is that person's
// pending cards in the conversation (store.ListPendingApprovals); another
// participant's cards are not in it, as they never held this person's patrol
// and a card this turn stages cannot replace them.
//
// Only a patrol turn is told. A turn a person sent has them at the keyboard
// with the cards on screen, and a resume turn starts only once the cards of
// the person it runs as are settled (store.ClaimApprovalResume).
//
// Nothing is added when no card is waiting, so a patrol in a chat without
// one reads exactly as before.
func appendWaitingApprovalsBlock(injected, inputKind string, pending []store.Approval, now time.Time) string {
	if inputKind != agent.InputKindChatPatrol {
		return injected
	}
	lines := waitingApprovalLines(waitingDecisionCards(pending, now))
	if len(lines) == 0 {
		return injected
	}
	return injected + "\n\n" + fmt.Sprintf(waitingApprovalsNote, waitingApprovalTime(now.Unix())) + strings.Join(lines, "\n") + "]"
}
