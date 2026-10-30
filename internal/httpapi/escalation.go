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
	"github.com/ElcanoTek/fleet/internal/tools"
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
	stagedApproval  bool // true when the turn staged a tool approval card for the user
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
		text := strings.TrimSpace(c.Text)
		if strings.HasPrefix(text, "LOOP_GUARD") {
			t.loopGuardBlocks++
		}
		if strings.HasPrefix(text, "APPROVAL_REQUIRED") && strings.Contains(text, "approval_id=") {
			t.stagedApproval = true
		}
		// A call held for the user's approval, or a model-switch suggestion,
		// is marked as an error result but nothing failed: it is not part of
		// a failure streak, and does not break one either.
		if isControlResult(text) {
			continue
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

// controlResultPrefixes mark tool results the approval layer returns in place
// of running a call: held for the user (APPROVAL_REQUIRED, APPROVAL_BLOCKED),
// refused by the user's own session policy (APPROVAL_DENIED), or a suggestion
// card's own bookkeeping. None of them is a tool failing. (BLOCKED: policy
// refusals still count: those are the model trying something it shouldn't.)
var controlResultPrefixes = []string{"APPROVAL_", "SUGGESTION_"}

func isControlResult(text string) bool {
	for _, p := range controlResultPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
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
//   - lockdown conversations, including every chat under LockdownOnly: their
//     text does not go to another vendor;
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
	// LockdownOnly covers chats created before the operator turned it on,
	// whose own flag is still false (the same OR stagedBashLockdown applies).
	if mode == config.EscalationClassifierOff || inputKind != "" || conv.Lockdown || s.cfg.LockdownOnly ||
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
func (s *Server) escalateAfterTurn(ctx context.Context, buf *turnBuffer, conv *store.Conversation, user string, history []agent.HistoryEntry, res *TurnResult, check *struggleCheck) {
	if res == nil || res.Cancelled {
		// A stopped turn shows no card and records no struggle, but the
		// opening check was already sent (it is detached from the turn), so
		// it is still counted by its outcome: the metric is once per request.
		s.discardStruggleCheck(check)
		return
	}
	struggle := struggleFromHistory(res.NewHistory, buf.compactionCount())
	pendingActionApproval := struggle.stagedApproval && s.hasPendingActionApproval(ctx, user, conv.ID)
	var stager *approvalStager
	staged := false
	stage := func(source, reason string) bool {
		// When the turn already staged a tool approval card that is still
		// pending the user's decision, never stack a suggest_advanced_model
		// card underneath it: clicking Approve on both cards runs the turn's
		// action twice in parallel. The struggle event is still recorded below.
		if staged || pendingActionApproval {
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
	recordCtx := ctx
	record := func(source, signal string, p *float64, suggested bool) {
		err := s.store.RecordEscalationEvent(recordCtx, store.EscalationEvent{
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
		if signal, reason := struggle.strongest(); signal != "" {
			record(suggestionSourceSignal, signal, nil, stage(suggestionSourceSignal, reason))
		}
	}

	// The classifier: the check started with the turn, plus one on the last
	// message the user steered in mid-turn, if any (a correction typed while
	// the agent works is the clearest pushback there is).
	var checks []*struggleCheck
	if check != nil {
		checks = append(checks, check)
	}
	if c := s.startSteerCheck(conv, history, res.NewHistory); c != nil {
		checks = append(checks, c)
	}
	if len(checks) == 0 {
		return
	}
	// The stream stays open until this returns, and the client blocks the
	// next message while it is open. So wait a short grace for the answers
	// (normally already in: the opening check has run alongside the turn);
	// if they are late, finish in the background and record the result
	// without a card, which could no longer reach the closed stream.
	if allDone(checks, struggleCheckGrace) {
		s.classifierOutcome(checks, stage, record)
		return
	}
	// Lateness is its own counter, once per request that missed the grace;
	// each is still counted once, by its eventual outcome, in the background.
	for _, c := range checks {
		select {
		case <-c.done:
		default:
			metrics.RecordEscalationClassifierLate(c.mode)
		}
	}
	s.background.Go("escalation.classifier_late", func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentcore.StruggleCheckTimeout+5*time.Second)
		defer cancel()
		recordCtx = ctx
		s.classifierOutcome(checks, nil, record)
	})
}

// struggleCheckGrace bounds how long a finished turn's stream waits for the
// classifier before closing; past it the result is recorded without a card.
const struggleCheckGrace = 1500 * time.Millisecond

// allDone reports whether every check finished within d.
func allDone(checks []*struggleCheck, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for _, c := range checks {
		select {
		case <-c.done:
		case <-timer.C:
			return false
		}
	}
	return true
}

// classifierOutcome turns finished checks into one result: the highest
// probability among those that answered. stage is nil when a card can no
// longer be shown (the stream closed while the check was late).
func (s *Server) classifierOutcome(checks []*struggleCheck, stage func(source, reason string) bool, record func(source, signal string, p *float64, suggested bool)) {
	mode, threshold := checks[0].mode, checks[0].threshold
	// Each request is counted once by its own outcome; the card and the
	// per-user record use the highest probability among them.
	best, answered := 0.0, false
	for _, c := range checks {
		v, ok := countClassifierCheck(c)
		if !ok {
			continue
		}
		answered = true
		if v.Probability > best {
			best = v.Probability
		}
	}
	if !answered || best < threshold {
		return
	}
	suggested := false
	if stage != nil && mode == config.EscalationClassifierSuggest {
		suggested = stage(suggestionSourceClassifier, escalationClassifierReason)
	}
	record(suggestionSourceClassifier, escalationSignalFrustration, &best, suggested)
}

// discardStruggleCheck counts an already-sent check whose turn will show no
// card (stopped, or failed before producing a result): the request was made,
// so the metric still counts it once, by its outcome. nil is a no-op.
func (s *Server) discardStruggleCheck(check *struggleCheck) {
	if check == nil {
		return
	}
	s.background.Go("escalation.classifier_discarded", func() { countClassifierCheck(check) })
}

// countClassifierCheck waits for one check and counts it by its outcome,
// reporting whether it answered.
func countClassifierCheck(c *struggleCheck) (agentcore.StruggleVerdict, bool) {
	v, err := c.wait()
	switch {
	case err != nil:
		metrics.RecordEscalationClassifierCheck(c.mode, "error")
		log.Printf("escalation: classifier: %s", logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the error text.
		return v, false
	case v.Probability >= c.threshold:
		metrics.RecordEscalationClassifierCheck(c.mode, "pushback")
	default:
		metrics.RecordEscalationClassifierCheck(c.mode, "ok")
	}
	return v, true
}

// startSteerCheck classifies the last message the user steered into the
// turn while it ran (persisted as a user text entry after the turn's own
// opening message), with everything before it as context. nil when the turn
// had no steer or the classifier would not run.
func (s *Server) startSteerCheck(conv *store.Conversation, history, turn []agent.HistoryEntry) *struggleCheck {
	last := -1
	for i, h := range turn {
		if i > 0 && h.Role == "user" && h.Type == "text" {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	var tc agent.TextContent
	if err := json.Unmarshal(turn[last].Content, &tc); err != nil {
		return nil
	}
	prior := append(append([]agent.HistoryEntry{}, history...), turn[:last]...)
	return s.startStruggleCheck(conv, prior, tc.Text, "")
}

func (s *Server) hasPendingActionApproval(ctx context.Context, user, convID string) bool {
	pending, err := s.store.ListPendingApprovals(ctx, user, convID)
	if err != nil {
		return true
	}
	for _, p := range pending {
		if p.ToolName != tools.SuggestAdvancedModelToolName && p.ToolName != previewEmailToolName {
			return true
		}
	}
	return false
}
