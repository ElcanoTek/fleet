package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Notifier is the runner's narrow view of the outbound completion notifier
// (#208). internal/notify.Notifier satisfies it. The runner depends on this
// interface rather than the concrete type so the wiring is injectable in tests
// (a fake captures the events) and so a deployment with notifications OFF can
// pass nil (or a disabled notifier) and the fire path becomes a cheap no-op.
type Notifier interface {
	// Notify delivers a terminal completion event. It must be safe to call from a
	// detached goroutine and must never block the caller on the runner's behalf —
	// the runner fires it with `go` and only logs the returned error, so a
	// notification failure NEVER affects task status.
	Notify(ctx context.Context, ev notify.Event) error
}

// notifyTerminal fires an outbound notification for a task that reached a
// terminal status, off-thread. It is a no-op when no notifier is wired (nil) so
// the default — no notify config — changes nothing. Errors are logged inside
// notify.Notify (and by the caller via the returned error) and never propagate
// to the task's status or the pool's bookkeeping.
//
// It must NOT be called while holding p.mu (it spawns a goroutine that does its
// own I/O); the call sites in executeTask are after the terminal status write,
// outside any lock.
func (p *Pool) notifyTerminal(task *models.Task, status notify.Status, session *models.LogSession, dur time.Duration) {
	if p.notifier == nil {
		return
	}
	ev := p.buildEvent(task, status, session, dur)
	go func() {
		// A bound on the whole fan-out independent of any per-attempt timeout, so a
		// pathological retry loop cannot leak a goroutine forever. notify applies
		// its own per-attempt timeout + bounded retry within this budget.
		ctx, cancel := context.WithTimeout(context.Background(), notifyFanoutBudget)
		defer cancel()
		// safe.Recover is not used here: notify.Notify does no panicky work, and a
		// panic in a detached notify goroutine must not be silently swallowed in a
		// way that hides a bug. Keep it simple — the runner's own recover guards the
		// task goroutine, not this one.
		_ = p.notifier.Notify(ctx, ev)
	}()
}

// notifyFanoutBudget caps the lifetime of one detached notify goroutine. Generous
// relative to notify's own per-attempt timeout + small retry count so a normal
// retry sequence completes, but finite so a stuck send is eventually abandoned.
const notifyFanoutBudget = 90 * time.Second

// buildEvent constructs the secret-free notify.Event from a finished run. It
// pulls the cost from the run's LogSession (nil-safe), truncates the prompt to a
// short display name, and builds the absolute log URL when a public base is
// configured. No credentials and no raw task internals beyond the truncated name
// cross into the event.
// notifyTaskName is the short, secret-free display label (first 60 chars of the
// prompt) shared by terminal and progress (#510) notifications.
func notifyTaskName(prompt string) string {
	const maxName = 60
	if len(prompt) > maxName {
		return prompt[:maxName] + "…"
	}
	return prompt
}

func (p *Pool) buildEvent(task *models.Task, status notify.Status, session *models.LogSession, dur time.Duration) notify.Event {
	name := notifyTaskName(task.Prompt)
	var cost float64
	if session != nil {
		cost = session.Cost
	}
	logURL := ""
	if p.publicURLBase != "" {
		logURL = p.publicURLBase + "/orchestrator/tasks/" + task.ID.String()
	}
	return notify.Event{
		TaskID:          task.ID.String(),
		Name:            name,
		Status:          status,
		CostUSD:         fmt.Sprintf("%.4f", cost),
		DurationSeconds: int(dur.Seconds()),
		LogURL:          logURL,
	}
}
