package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/genui"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

const autonomousAcceptanceRequiredError = "autonomous_acceptance_required"

// maxAutoSubmitChain caps how many consecutive auto=1 UI submissions can run
// without an intervening human-authored message (2 allows e.g. intake_card ->
// plan_table while blocking a 3rd consecutive auto-submit loop).
const maxAutoSubmitChain = 2

func (s *Server) hasAcceptedAutonomous(ctx context.Context, userEmail string) bool {
	if s == nil || s.store == nil || strings.TrimSpace(userEmail) == "" {
		return false
	}
	_, ok, err := s.store.GetAutonomousAcceptance(ctx, userEmail)
	if err != nil {
		return false
	}
	return ok
}

// effectiveOwnerApprovalMode returns conv's effective approval mode for its
// owner, clamping "autonomous" down to "auto" if the owner has not accepted
// CurrentAutonomousPolicyVersion.
func (s *Server) effectiveOwnerApprovalMode(ctx context.Context, ownerEmail string, conv *store.Conversation) string {
	if conv == nil {
		return store.ConversationApprovalManual
	}
	mode := conv.EffectiveApprovalMode()
	if mode == store.ConversationApprovalAutonomous && s != nil && s.store != nil {
		if !s.hasAcceptedAutonomous(ctx, ownerEmail) {
			return store.ConversationApprovalAuto
		}
	}
	return mode
}

// requireAutonomousAcceptance enforces that userEmail has accepted
// store.CurrentAutonomousPolicyVersion before setting approval_mode =
// "autonomous". Writes 409 Conflict {"error": "autonomous_acceptance_required"}
// and returns false when acceptance is missing.
func (s *Server) requireAutonomousAcceptance(w http.ResponseWriter, r *http.Request, userEmail string) bool {
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return false
	}
	_, accepted, err := s.store.GetAutonomousAcceptance(r.Context(), userEmail)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	if !accepted {
		writeJSONStatus(w, r, http.StatusConflict, map[string]any{
			"error":                     autonomousAcceptanceRequiredError,
			"autonomous_policy_version": store.CurrentAutonomousPolicyVersion,
		})
		return false
	}
	return true
}

// handleAutonomousAccept serves POST /chat/autonomous-accept (proxied from
// POST /api/chat/autonomous-accept), recording the caller's acceptance of
// CurrentAutonomousPolicyVersion.
func (s *Server) handleAutonomousAccept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return
	}
	if err := s.store.AcceptAutonomousPolicy(r.Context(), user, store.CurrentAutonomousPolicyVersion); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{
		"autonomous_accepted":       true,
		"autonomous_policy_version": store.CurrentAutonomousPolicyVersion,
	})
}

func uiAutoSubmitEventPayload(row store.UIAutoSubmit) map[string]any {
	row = row.WithRemainingMs(time.Now().UnixMilli())
	return map[string]any{
		"id":              row.ID,
		"conversation_id": row.ConversationID,
		"call_id":         row.CallID,
		"action_id":       row.ActionID,
		"deadline_ms":     row.DeadlineMs,
		"remaining_ms":    row.RemainingMs,
		"status":          row.Status,
	}
}

func (s *Server) emitUIAutoSubmit(buf *turnBuffer, convID string, row store.UIAutoSubmit) {
	s.notePublish("ui.autosubmit")
	payload := uiAutoSubmitEventPayload(row)
	if buf != nil {
		buf.Emit("ui.autosubmit", payload)
		return
	}
	if entry, ok := s.getInflight(convID); ok && entry.buf != nil {
		entry.buf.Emit("ui.autosubmit", payload)
	}
}

func (s *Server) cancelConversationAutoSubmits(ctx context.Context, convID string) {
	if s == nil || s.store == nil || strings.TrimSpace(convID) == "" {
		return
	}
	rows, cancelledQueued, err := s.store.CancelActiveUIAutoSubmits(ctx, convID, "")
	if err != nil {
		logging.From(ctx).Warn("autosubmit cancel failed", "conv_id", safe.LogSafe(convID), "err", err)
		return
	}
	ownerEmail := ""
	for _, r := range rows {
		if ownerEmail == "" && r.UserEmail != "" {
			ownerEmail = r.UserEmail
		}
		s.emitUIAutoSubmit(nil, convID, r)
	}
	if cancelledQueued > 0 {
		s.emitQueueUpdate(ctx, ownerEmail, convID)
	}
}

func isAutoSubmitUserMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[UI submission] ") {
		return false
	}
	header, _, _ := strings.Cut(trimmed, "\n")
	for _, tok := range strings.Fields(header) {
		if tok == "auto=1" {
			return true
		}
	}
	return false
}

func isSyntheticFleetContinuationMessage(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "[fleet]")
}

func (s *Server) consecutiveAutoSubmitChainDepth(
	ctx context.Context,
	convID, currentUserInput string,
	userCommitted func() bool,
) int {
	var userTexts []string
	if s != nil && s.store != nil && strings.TrimSpace(convID) != "" {
		if hist, err := s.store.LoadHistory(ctx, convID); err == nil {
			for _, h := range hist {
				if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
					continue
				}
				var tc agent.TextContent
				if json.Unmarshal(h.Content, &tc) == nil && strings.TrimSpace(tc.Text) != "" {
					userTexts = append(userTexts, tc.Text)
				}
			}
		}
	}
	alreadyPersisted := userCommitted != nil && userCommitted()
	if !alreadyPersisted && strings.TrimSpace(currentUserInput) != "" {
		userTexts = append(userTexts, currentUserInput)
	}

	depth := 0
	for i := len(userTexts) - 1; i >= 0; i-- {
		txt := userTexts[i]
		if isAutoSubmitUserMessage(txt) {
			depth++
			continue
		}
		if isSyntheticFleetContinuationMessage(txt) {
			continue
		}
		break
	}
	return depth
}

func cardHasInteractiveAction(specJSON []byte) bool {
	var parsed struct {
		Panel   string `json:"panel"`
		Actions []struct {
			Kind string `json:"kind"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(specJSON, &parsed); err != nil {
		return false
	}
	if strings.TrimSpace(parsed.Panel) != "" {
		return false
	}
	for _, a := range parsed.Actions {
		k := strings.ToLower(strings.TrimSpace(a.Kind))
		if k == "" || k == "submit" || k == "message" {
			return true
		}
	}
	return false
}

// newShowUIDisplayedHandler returns the per-turn hook called when show_ui
// validates and displays a card. Display-only cards (no submit/message actions
// and replaces == "") leave any earlier interactive card's countdown intact;
// replacement or interactive cards supersede earlier active/queued countdowns,
// and eligible prefilled cards in owner-initiated non-patrol Autonomous turns
// arm a 10-second server-owned countdown row (up to maxAutoSubmitChain
// consecutive auto=1 turns).
func (s *Server) newShowUIDisplayedHandler(
	conv *store.Conversation,
	user, inputKind, currentUserInput string,
	buf *turnBuffer,
	turnStager *approvalStager,
	userCommitted func() bool,
) tools.ShowUIDisplayedHandler {
	return func(ctx context.Context, callID string, specJSON []byte) {
		if s.store == nil || conv == nil || strings.TrimSpace(conv.ID) == "" {
			return
		}
		ownerEmail := convOwnerEmail(conv, user)
		var meta struct {
			Replaces string `json:"replaces"`
		}
		_ = json.Unmarshal(specJSON, &meta)
		if rep := strings.TrimSpace(meta.Replaces); rep != "" {
			if cancelled, cancelledQueued, cerr := s.store.CancelActiveUIAutoSubmits(ctx, conv.ID, rep); cerr == nil {
				for _, r := range cancelled {
					s.emitUIAutoSubmit(buf, conv.ID, r)
				}
				if cancelledQueued > 0 {
					s.emitQueueUpdate(ctx, ownerEmail, conv.ID)
				}
			}
		}

		interactive := cardHasInteractiveAction(specJSON)
		actionID, valuesJSON, eligible := genui.CollectInitialSubmit(specJSON)
		if interactive || eligible {
			// Supersede any earlier active or still-queued auto-submit in this
			// conversation when a new interactive card is shown. Display-only
			// cards (e.g. a metrics summary without submit actions and
			// replaces == "") leave an earlier interactive card's pending
			// countdown untouched.
			if cancelled, cancelledQueued, cerr := s.store.CancelActiveUIAutoSubmits(ctx, conv.ID, ""); cerr == nil {
				for _, r := range cancelled {
					if r.CallID != callID {
						s.emitUIAutoSubmit(buf, conv.ID, r)
					}
				}
				if cancelledQueued > 0 {
					s.emitQueueUpdate(ctx, ownerEmail, conv.ID)
				}
			}
		}

		if inputKind == agent.InputKindChatPatrol {
			return
		}
		if !strings.EqualFold(ownerEmail, user) {
			return
		}
		if turnStager == nil || !turnStager.isAutonomous() {
			return
		}
		if strings.TrimSpace(callID) == "" || !eligible {
			return
		}
		if s.consecutiveAutoSubmitChainDepth(ctx, conv.ID, currentUserInput, userCommitted) >= maxAutoSubmitChain {
			return
		}

		nowMs := time.Now().UnixMilli()
		stored, err := s.store.CreateUIAutoSubmit(ctx, store.UIAutoSubmit{
			ConversationID: conv.ID,
			UserEmail:      ownerEmail,
			CallID:         callID,
			ActionID:       actionID,
			ValuesJSON:     string(valuesJSON),
			DeadlineMs:     nowMs + store.UIAutoSubmitDelayMs,
			Status:         store.UIAutoSubmitStatusPending,
			CreatedAtMs:    nowMs,
			UpdatedAtMs:    nowMs,
		})
		if err != nil {
			logging.From(ctx).Warn("autosubmit arm failed", "conv_id", safe.LogSafe(conv.ID), "call_id", safe.LogSafe(callID), "err", err)
			return
		}
		s.emitUIAutoSubmit(buf, conv.ID, stored)
	}
}

// handleConversationAutoSubmitPause serves POST /conversations/{id}/autosubmit/pause.
// Any conversation participant (owner or Multiplayer participant) may pause a
// pending countdown on the conversation.
func (s *Server) handleConversationAutoSubmitPause(w http.ResponseWriter, r *http.Request, _ string, id string, _ *store.Conversation) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		CallID string `json:"call_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && err.Error() != "EOF" {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
	}
	rows, err := s.store.PauseUIAutoSubmits(r.Context(), id, strings.TrimSpace(req.CallID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nowMs := time.Now().UnixMilli()
	for i := range rows {
		rows[i] = rows[i].WithRemainingMs(nowMs)
		if rows[i].Status == store.UIAutoSubmitStatusPaused {
			s.emitUIAutoSubmit(nil, id, rows[i])
		}
	}
	if rows == nil {
		rows = []store.UIAutoSubmit{}
	}
	writeJSON(w, r, map[string]any{
		"ui_autosubmits": rows,
	})
}

// SweepDueUIAutoSubmits claims due pending rows from chat.ui_autosubmits and
// enqueues valid submissions atomically inside the store transaction, emitting
// ui.autosubmit status "fired" ONLY when the queue row was actually inserted.
func (s *Server) SweepDueUIAutoSubmits(ctx context.Context) (int, error) {
	if s == nil || s.store == nil || s.shuttingDown.Load() || UpdateDrainActive() {
		return 0, nil
	}
	nowMs := time.Now().UnixMilli()
	swept, err := s.store.SweepAndEnqueueDueUIAutoSubmits(ctx, nowMs, 64, maxPendingInputs)
	if err != nil {
		return 0, err
	}
	fired := 0
	for _, item := range swept {
		s.emitUIAutoSubmit(nil, item.Row.ConversationID, item.Row)
		if item.Enqueued {
			fired++
			s.emitQueueUpdate(ctx, item.Row.UserEmail, item.Row.ConversationID)
			s.maybeDrainQueue(item.Row.ConversationID)
		}
	}
	return fired, nil
}
