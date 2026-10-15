package httpapi

// Chat escalation from the host side (docs/CHAT-ESCALATION.md).
//
// The chat model can already ask the user to switch to the advanced model
// (suggest_advanced_model). That relies on the everyday model noticing its own
// trouble, which is the judgment it is weakest at. This file adds two host-side
// sources that stage the same card through the same gate (stageSuggestion):
//
//   - free end-of-turn signals read from the turn itself: a tool-error streak,
//     a loop-guard block, a forced context compaction, a very long turn;
//   - an optional decision-model check (Jev by default) on the user's latest
//     message, started when the turn starts so it never delays the reply.
//
// Both run after the turn's reply is complete and stage at most one card per
// turn. Every signal and every classifier "yes" is also recorded per user in
// escalation_events, for the admin Users page, whether or not a card was
// shown. Nothing here blocks or alters a turn: a failure of any part only
// means no card.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/store"
)

const (
	// escalationToolErrorStreak is the run of consecutive failed tool calls
	// that counts as struggling. One or two failures are routine (a typo'd
	// path, a retry); three in a row means the model is not finding its way.
	escalationToolErrorStreak = 3
	// escalationLongTurnToolCalls is the number of tool calls in one turn
	// that counts as struggling: well past what a routine answer takes.
	escalationLongTurnToolCalls = 25

	escalationSignalFrustration = "frustration"
	escalationSignalToolErrors  = "tool_errors"
	escalationSignalLoopGuard   = "loop_guard"
	escalationSignalCompaction  = "compaction"
	escalationSignalLongTurn    = "long_turn"

	escalationClassifierReason = "Your last message suggests my previous answer missed what you needed. The advanced model may handle this better."
)

// turnStruggle is what the turn's own history and stream say about how it went.
type turnStruggle struct {
	toolCalls       int
	toolErrorStreak int // the longest run of consecutive failed tool calls
	loopGuardBlocks int
	compactions     int
}

// struggleFromHistory reads the turn's tool results in order. A result whose
// text starts with LOOP_GUARD is the repeat-call guard refusing a call.
func struggleFromHistory(history []agent.HistoryEntry, compactions int) turnStruggle {
	t := turnStruggle{compactions: compactions}
	streak := 0
	for _, h := range history {
		if h.Role != "tool" || h.Type != "tool_result" {
			continue
		}
		var c agent.ToolResultContent
		if err := json.Unmarshal(h.Content, &c); err != nil {
			continue
		}
		t.toolCalls++
		if strings.HasPrefix(strings.TrimSpace(c.Text), "LOOP_GUARD") {
			t.loopGuardBlocks++
		}
		if c.IsErr {
			streak++
			if streak > t.toolErrorStreak {
				t.toolErrorStreak = streak
			}
		} else {
			streak = 0
		}
	}
	return t
}

// strongest names the most telling signal that fired, with the reason the
// card shows, or "" when the turn went fine.
func (t turnStruggle) strongest() (signal, reason string) {
	switch {
	case t.loopGuardBlocks > 0:
		return escalationSignalLoopGuard, "I kept repeating the same step without making progress. The advanced model may find a better approach."
	case t.toolErrorStreak >= escalationToolErrorStreak:
		return escalationSignalToolErrors, fmt.Sprintf("%d tool calls in a row failed on this request. The advanced model may get further.", t.toolErrorStreak)
	case t.compactions > 0:
		return escalationSignalCompaction, "This conversation outgrew my working memory and had to be summarized. The advanced model handles long, detailed work better."
	case t.toolCalls >= escalationLongTurnToolCalls:
		return escalationSignalLongTurn, fmt.Sprintf("This request took %d tool steps. The advanced model may reach the answer more directly.", t.toolCalls)
	}
	return "", ""
}

// struggleCheck is one classifier call running alongside the turn.
type struggleCheck struct {
	mode      string
	threshold float64
	done      chan struct{}
	verdict   agentcore.StruggleVerdict
	err       error
}

// wait returns the classifier's answer; it is bounded by the call's own
// timeout (agentcore.StruggleCheckTimeout).
func (c *struggleCheck) wait() (agentcore.StruggleVerdict, error) {
	<-c.done
	return c.verdict, c.err
}

// checkStruggle is the classifier call, a seam for tests.
var checkStruggle = agentcore.CheckStruggle

// startStruggleCheck starts the decision-model check on the user's message
// when the classifier is on, or returns nil. It skips:
//   - a turn fleet started itself (a resume after an approval): nobody spoke;
//   - the first message of a conversation: there is no answer to push back on;
//   - lockdown conversations: their text does not go to another vendor;
//   - deployments with no OpenRouter key.
//
// It runs on every model, including the advanced one, so the per-user
// frustration count is complete; only the card is limited to the everyday
// model (by stageSuggestion's gate).
func (s *Server) startStruggleCheck(conv *store.Conversation, history []agent.HistoryEntry, userInput, inputKind string) *struggleCheck {
	if s.cfg == nil {
		return nil
	}
	mode, model, pct := s.cfg.LiveEscalationClassifier()
	if mode == config.EscalationClassifierOff || inputKind != "" || conv.Lockdown ||
		strings.TrimSpace(s.cfg.OpenRouterAPIKey) == "" || strings.TrimSpace(userInput) == "" {
		return nil
	}
	prior := transcriptFromHistory(history)
	if prior == "" {
		return nil
	}
	conversation := prior + "\n\nUser: " + strings.TrimSpace(userInput)
	c := &struggleCheck{mode: mode, threshold: float64(pct) / 100, done: make(chan struct{})}
	apiKey := s.cfg.OpenRouterAPIKey
	go func() {
		defer close(c.done)
		// Detached from the turn: a Stop must not leave a half-recorded check,
		// and the call is short and bounded.
		c.verdict, c.err = checkStruggle(context.Background(), apiKey, model, conversation)
	}()
	return c
}

// escalateAfterTurn reads the finished turn's signals and the classifier's
// answer, records each struggle per user, and stages at most one card.
func (s *Server) escalateAfterTurn(ctx context.Context, buf *turnBuffer, conv *store.Conversation, user string, res *TurnResult, check *struggleCheck) {
	if res == nil || res.Cancelled {
		return
	}
	var stager *approvalStager
	staged := false
	stage := func(source, reason string) bool {
		if staged {
			return false
		}
		if stager == nil {
			stager = s.newTurnStager(ctx, conv, user, buf)
		}
		ok, err := stager.StageHostSuggestion(source, reason)
		if err != nil {
			log.Printf("escalation: stage %s suggestion (conv=%s): %s", source, logSafe(conv.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
		}
		staged = ok
		return ok
	}
	record := func(source, signal string, p *float64, suggested bool) {
		err := s.store.RecordEscalationEvent(ctx, store.EscalationEvent{
			ConversationID: conv.ID,
			TurnID:         buf.turnID,
			UserEmail:      user,
			CreatedAt:      time.Now().Unix(),
			Source:         source,
			Signal:         signal,
			Probability:    p,
			Model:          res.Model,
			Suggested:      suggested,
		})
		if err != nil {
			log.Printf("escalation: record event (conv=%s): %s", logSafe(conv.ID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
		}
	}

	// Signals first: they describe this turn, and are free.
	if s.cfg != nil && s.cfg.LiveEscalationSignalsEnabled() {
		if signal, reason := struggleFromHistory(res.NewHistory, buf.compactionCount()).strongest(); signal != "" {
			record(suggestionSourceSignal, signal, nil, stage(suggestionSourceSignal, reason))
		}
	}

	if check == nil {
		return
	}
	verdict, err := check.wait()
	if err != nil {
		metrics.RecordEscalationClassifierCheck(check.mode, "error")
		log.Printf("escalation: classifier (conv=%s): %s", logSafe(conv.ID), logSafe(err.Error()))
		return
	}
	if verdict.Probability < check.threshold {
		metrics.RecordEscalationClassifierCheck(check.mode, "ok")
		return
	}
	metrics.RecordEscalationClassifierCheck(check.mode, "pushback")
	suggested := false
	if check.mode == config.EscalationClassifierSuggest {
		suggested = stage(suggestionSourceClassifier, escalationClassifierReason)
	}
	p := verdict.Probability
	record(suggestionSourceClassifier, escalationSignalFrustration, &p, suggested)
}
