// Promote a chat into a recurring task (#455).
//
// A user who produced a useful one-off analysis in chat can "promote" it into a
// recurring scheduled task. This endpoint synthesizes a clean, self-contained
// task prompt + a suggested cadence from the conversation (a host-side model
// call), then STAGES it as a schedule_task approval — reusing the exact #239
// approval card + creation path (runStagedScheduleTask → WithTaskScheduler →
// EnqueueTask). Nothing is created until the user approves the card.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// promoteCaptureSink records the events a one-shot Stage() emits so the handler
// can return the exact tool.approval_required payload (approval_id / tool /
// summary / expires_at) the SSE stream would carry — the UI renders it with the
// same ScheduleTaskCard, no separate shape.
type promoteCaptureSink struct {
	approval map[string]any
}

func (c *promoteCaptureSink) Emit(event string, data any) {
	if event == "tool.approval_required" {
		if m, ok := data.(map[string]any); ok {
			c.approval = m
		}
	}
}

// promoteFallbackCron is used when the synthesizer's suggested cron doesn't
// parse — a recurring task needs a valid cadence, and the user reviews it on the
// card before approving. 9am daily is an innocuous default.
const promoteFallbackCron = "0 9 * * *"

// handlePromoteToTask backs POST /conversations/{id}/promote-to-task. It loads
// the conversation the caller owns, synthesizes a recurring-task proposal from
// its transcript, and stages a schedule_task approval. Returns the approval-card
// payload + the synthesizer's rationale. 404 if the conversation isn't
// owned/known, 422 if it has no promotable content, 502 if synthesis fails.
func (s *Server) handlePromoteToTask(w http.ResponseWriter, r *http.Request, convID, user string) {
	ctx := r.Context()

	// Ownership: Get is user-scoped, so a foreign/unknown id yields nil → 404.
	conv, err := s.store.Get(ctx, user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	history, err := s.store.LoadHistory(ctx, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	transcript := transcriptFromHistory(history)
	if strings.TrimSpace(transcript) == "" {
		http.Error(w, "conversation has no content to promote", http.StatusUnprocessableEntity)
		return
	}

	proposal, err := s.agent.SuggestRecurringTask(ctx, transcript, nil)
	if err != nil || proposal == nil {
		http.Error(w, "could not synthesize a recurring task from this conversation; try again", http.StatusBadGateway)
		return
	}

	// A recurring task needs a valid cron. If the model's suggestion doesn't
	// parse, fall back to a sane default — the user still reviews/edits on the card.
	cronExpr := strings.TrimSpace(proposal.Cron)
	if _, perr := cron.ParseStandard(cronExpr); perr != nil {
		cronExpr = promoteFallbackCron
	}

	params := tools.ScheduleTaskParams{
		Name:   proposal.Name,
		Prompt: proposal.Prompt,
		Cron:   cronExpr,
		Tags:   []string{"source-chat"},
	}
	rawInput, err := json.Marshal(params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Stage the proposal as a schedule_task approval, reusing the same
	// persistence + summary + card the #239 gate produces. A capturing sink grabs
	// the emitted card payload; a generous 1h window since this is a deliberate,
	// user-initiated action they may take a moment to review.
	sink := &promoteCaptureSink{}
	stager := &approvalStager{
		ctx:                  ctx,
		store:                s.store,
		conversationID:       convID,
		userEmail:            user,
		sink:                 sink,
		globalTimeoutSeconds: promoteApprovalTimeoutSeconds,
	}
	if _, err := stager.Stage("schedule_task", "", string(rawInput)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"approval":  sink.approval,
		"rationale": proposal.Rationale,
	})
}

// promoteApprovalTimeoutSeconds is the default-deny window for a promote card:
// generous (1h) because the user explicitly initiated it and reviews a
// synthesized prompt before approving.
const promoteApprovalTimeoutSeconds = 3600

// transcriptFromHistory renders the user/assistant TEXT turns of a conversation
// into a plain "User:/Assistant:" transcript for the synthesizer, dropping
// tool_call/tool_result/reasoning/summary entries (noise for prompt synthesis).
func transcriptFromHistory(history []agent.HistoryEntry) string {
	var b strings.Builder
	for _, e := range history {
		if e.Type != "text" || (e.Role != "user" && e.Role != "assistant") {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(e.Content, &tc); err != nil {
			continue
		}
		text := strings.TrimSpace(tc.Text)
		if text == "" {
			continue
		}
		label := "User"
		if e.Role == "assistant" {
			label = "Assistant"
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}
