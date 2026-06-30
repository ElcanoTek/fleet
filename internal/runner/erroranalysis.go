package runner

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// errorAnalysisBudget bounds one detached diagnosis goroutine, independent of any
// per-call timeout the analyzer applies internally, so a stuck analysis can never
// leak a goroutine forever (mirrors notifyFanoutBudget).
const errorAnalysisBudget = 60 * time.Second

// errorAnalysisTailMessages caps how many trailing session-log messages feed the
// diagnosis prompt — enough to capture what the agent attempted near the failure
// without shipping the whole transcript to the cheap model.
const errorAnalysisTailMessages = 12

// errorAnalysisTailMaxChars caps the rendered tail size (the most-recent context
// is the most diagnostic, so we keep the tail when over budget).
const errorAnalysisTailMaxChars = 6000

// maybeAnalyzeFailure fires the post-failure LLM diagnosis (#317) off-thread for a
// task that failed TERMINALLY. It is a no-op when no analyzer is wired (the
// default) so a deployment without analysis configured is byte-for-byte
// unchanged. Mirrors notifyTerminal exactly: a detached, time-bounded goroutine
// whose errors are logged and NEVER touch task status or the pool's bookkeeping.
//
// The validated diagnosis is persisted via the lease-FREE SetTaskErrorAnalysis —
// the lease is already released by the time the task reached its terminal
// transition, and the diagnosis is a benign annotation on an already-terminal row
// (it touches neither status nor lease). It must NOT be called while holding p.mu.
func (p *Pool) maybeAnalyzeFailure(task *models.Task, session *models.LogSession, runErr error) {
	if p.errorAnalyzer == nil || runErr == nil {
		return
	}
	// Snapshot the primitives the analyzer needs BEFORE detaching, so the
	// goroutine never races a caller that reuses task/session.
	taskID := task.ID
	prompt := task.Prompt
	errMsg := runErr.Error()
	tail := sessionTail(session, errorAnalysisTailMessages, errorAnalysisTailMaxChars)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), errorAnalysisBudget)
		defer cancel()
		analysis, err := p.errorAnalyzer.AnalyzeTaskFailure(ctx, prompt, errMsg, tail)
		if err != nil {
			log.Printf("runner: error analysis for task %s failed: %v", taskID, err)
			return
		}
		if len(analysis) == 0 {
			return
		}
		// Detached background context for the write: the analysis is worth
		// persisting even if the per-analysis deadline above has elapsed.
		if err := p.store.SetTaskErrorAnalysis(context.Background(), taskID, analysis); err != nil {
			log.Printf("runner: failed to persist error analysis for task %s: %v", taskID, err)
		}
	}()
}

// sessionTail renders the last n messages of a session log into a compact
// role-prefixed transcript for the diagnosis prompt, capped at maxChars and kept
// to a rune boundary (so the cheap model is fed valid UTF-8). Keeps the TAIL when
// over budget. Returns "" for a nil/empty session.
func sessionTail(session *models.LogSession, n, maxChars int) string {
	if session == nil || len(session.Messages) == 0 {
		return ""
	}
	start := len(session.Messages) - n
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for _, m := range session.Messages[start:] {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	s := b.String()
	if r := []rune(s); len(r) > maxChars {
		s = string(r[len(r)-maxChars:])
	}
	return s
}
