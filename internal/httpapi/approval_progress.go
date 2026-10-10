package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Approval progress (docs/APPROVAL-PROGRESS.md).
//
// For a tool the bundle lists in agent_policy.critical_tool_progress, an
// approved card's MCP call is made with a progress sink (mcp.WithProgress):
// the request carries _meta.progressToken, and every notifications/progress
// the server sends for it crosses the broker as an intermediate frame and
// lands here. The latest update is written to the approval row
// (approvals.progress_json) at most once per approvalProgressWriteEvery, and
// only while the row is still executing. The executing card reads it from the
// one-card approval GET it polls, so it survives a reload. When the call
// finishes, the conversation owner gets a browser push (internal/webpush),
// titled with the card's plain-words title when the bundle declared a
// describer.

// approvalProgressWriteEvery bounds how often one call's progress is written.
// Var, not const: tests shorten it.
var approvalProgressWriteEvery = time.Second

// approvalProgressWriteTimeout bounds one progress write.
const approvalProgressWriteTimeout = 5 * time.Second

// approvalProgressMessageMax bounds the stored message, in runes.
const approvalProgressMessageMax = 200

// approvalProgress is the stored and served progress of an executing call.
type approvalProgress struct {
	Progress float64 `json:"progress"`
	// Total is 0 when the server does not know the amount of work.
	Total   float64 `json:"total,omitempty"`
	Message string  `json:"message,omitempty"`
	// UpdatedAt is when fleet recorded the update, unix seconds.
	UpdatedAt int64 `json:"updated_at"`
}

// progressMessage makes a server's progress message safe to show: control and
// bidirectional-formatting characters become spaces (they could reorder what
// the person reads), it is cut to approvalProgressMessageMax runes, and a
// message the secret redaction would change is dropped whole.
func progressMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > approvalProgressMessageMax {
		s = string([]rune(s)[:approvalProgressMessageMax]) + "…"
	}
	if s == "" {
		return s
	}
	if raw, err := json.Marshal(s); err != nil || tools.RedactionWouldAlter(raw) {
		return ""
	}
	return s
}

// canonicalApprovalProgress validates a progress value for storage or the
// wire: finite, non-negative numbers and a safe message. ok is false for
// anything else, which is then not shown.
func canonicalApprovalProgress(p approvalProgress) (approvalProgress, bool) {
	for _, f := range []float64{p.Progress, p.Total} {
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return approvalProgress{}, false
		}
	}
	p.Message = progressMessage(p.Message)
	return p, true
}

// approvalProgressPayload is the progress a client receives for an executing
// approval row, or nil. The stored JSON is re-validated on read.
func approvalProgressPayload(a *store.Approval) *approvalProgress {
	if a == nil || a.ProgressJSON == "" {
		return nil
	}
	var p approvalProgress
	if json.Unmarshal([]byte(a.ProgressJSON), &p) != nil {
		return nil
	}
	p, ok := canonicalApprovalProgress(p)
	if !ok {
		return nil
	}
	return &p
}

// withApprovalProgress adds what an executing card of an opted-in tool needs:
// progress_updates (the card polls for progress and its outcome) and the
// latest progress when there is one. Nothing is added for any other row, so
// every other payload is unchanged.
func (s *Server) withApprovalProgress(payload map[string]any, a *store.Approval) map[string]any {
	if a == nil || payload["executing"] != true || !approvalReportsProgress(a.ToolName) {
		return payload
	}
	payload["progress_updates"] = true
	if p := approvalProgressPayload(a); p != nil {
		payload["progress"] = p
	}
	return payload
}

// approvalReportsProgress reports whether an approved call of toolName asks
// for progress under the running policy (handler-only cards never run an MCP
// call).
func approvalReportsProgress(toolName string) bool {
	return agentcore.ApprovalProgress(toolName) && !handlerOnlyApproval(toolName)
}

// startApprovalProgress attaches a progress sink to an approved call's context
// and returns the stop that ends it. The sink never blocks the transport it
// runs on: it keeps only the latest update and wakes a writer goroutine, which
// writes at most once per approvalProgressWriteEvery. stop waits for that
// writer, so no write happens after the call's outcome is recorded.
func (s *Server) startApprovalProgress(ctx context.Context, approval *store.Approval) (context.Context, func()) {
	var (
		mu     sync.Mutex
		latest *mcp.ProgressUpdate
		wake   = make(chan struct{}, 1)
		done   = make(chan struct{})
		wg     sync.WaitGroup
		once   sync.Once
	)
	sink := func(u mcp.ProgressUpdate) {
		mu.Lock()
		latest = &u
		mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	user, id := approval.UserEmail, approval.ID
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-wake:
			}
			mu.Lock()
			u := latest
			latest = nil
			mu.Unlock()
			if u != nil {
				s.writeApprovalProgress(user, id, *u)
			}
			select {
			case <-done:
				return
			case <-time.After(approvalProgressWriteEvery):
			}
		}
	})
	stop := func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
	return mcp.WithProgress(ctx, sink), stop
}

// writeApprovalProgress stores one update on a still-executing row. Best
// effort: a failure only means the card shows an older update.
func (s *Server) writeApprovalProgress(user, approvalID string, u mcp.ProgressUpdate) {
	p, ok := canonicalApprovalProgress(approvalProgress{
		Progress: u.Progress, Total: u.Total, Message: u.Message, UpdatedAt: time.Now().Unix(),
	})
	if !ok {
		return
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), approvalProgressWriteTimeout)
	defer cancel()
	if _, err := s.store.SetApprovalProgress(ctx, user, approvalID, string(raw)); err != nil {
		log.Printf("approval progress: store %s: %s", logSafe(approvalID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the server-minted id and the error text.
	}
}

// approvalPushLabel names an approval in a notification: the describer's
// plain-words card title when there is a valid card, else the tool name. The
// arguments and the result never go in a push.
func approvalPushLabel(a *store.Approval) string {
	if raw := approvalCardPayload(a); raw != nil {
		var c struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(raw, &c) == nil && strings.TrimSpace(c.Title) != "" {
			return c.Title
		}
	}
	return a.ToolName
}

// notifyApprovalFinished sends the owner a "done" or "not applied" push once
// an opted-in approved call has recorded its outcome. Fire-and-forget on the
// server's background tracker, like the approval-needed push.
func (s *Server) notifyApprovalFinished(user, convID string, approval *store.Approval, failed bool) {
	if !approvalReportsProgress(approval.ToolName) || !s.push.ApprovalPushEnabled() {
		return
	}
	push, label := s.push, approvalPushLabel(approval)
	s.background.Go("httpapi.approval_finished_push", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := push.NotifyApprovalFinished(ctx, user, convID, label, failed); err != nil {
			log.Printf("push: approval finished (conv=%s): %s", logSafe(convID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
		}
	})
}

// notifyResumeSkipped tells the owner that the automatic continue did not
// start (the hourly cap, or a full queue), so the task waits for them.
func (s *Server) notifyResumeSkipped(owner, convID string) {
	if owner == "" || !s.push.ApprovalPushEnabled() {
		return
	}
	push := s.push
	s.background.Go("httpapi.resume_skipped_push", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := push.NotifyResumeSkipped(ctx, owner, convID); err != nil {
			log.Printf("push: resume skipped (conv=%s): %s", logSafe(convID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
		}
	})
}
