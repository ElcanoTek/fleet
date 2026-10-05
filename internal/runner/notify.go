package runner

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/truncate"
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
		p.deliver(ctx, task, ev)
	}()
}

// deliver sends one event from a detached notify goroutine.
func (p *Pool) deliver(ctx context.Context, task *models.Task, ev notify.Event) {
	// Resolve the owner email HERE, off-thread, so the terminal path never
	// waits on the users lookup. Empty = no push audience (#292); the
	// deployment-wide email/webhook channels ignore the field.
	ev.Audience = p.ownerEmail(ctx, task)
	// safe.Recover is not used here: notify.Notify does no panicky work, and a
	// panic in a detached notify goroutine must not be silently swallowed in a
	// way that hides a bug. Keep it simple — the runner's own recover guards the
	// task goroutine, not this one.
	_ = p.notifier.Notify(ctx, ev)
}

// blockedStreakAlert is how many consecutive blocked occurrences of one
// recurring lineage raise the blocked-streak alert (notifyBlockedSuccess).
const blockedStreakAlert = 3

// notifyBlockedSuccess notifies for a success whose declared completion
// clause recorded a blocked outcome (Task.RunOutcome "blocked"). An ordinary
// blocked run is a success event whose message says "Blocked: <detail>". The
// run that makes it EXACTLY blockedStreakAlert blocked occurrences in a row of
// a recurring lineage is sent as a failure event instead — a dashboard that
// has stopped updating is something its owner must hear about even when they
// only subscribe to failures — once per streak. Nothing parks: the schedule
// keeps running, and the next run that publishes ends the streak. The streak
// lookup runs off-thread with the rest of the fan-out.
func (p *Pool) notifyBlockedSuccess(task *models.Task, session *models.LogSession, dur time.Duration) {
	if p.notifier == nil {
		return
	}
	detail := ""
	if task.RunOutcomeDetail != nil {
		detail = strings.Join(strings.Fields(*task.RunOutcomeDetail), " ")
	}
	ev := p.buildEvent(task, notify.StatusSuccess, session, dur)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyFanoutBudget)
		defer cancel()
		message := "Blocked: " + detail
		if task.Recurrence != "" && p.store != nil {
			streak, err := p.store.BlockedStreak(ctx, task.ID, blockedStreakAlert+1)
			if err != nil {
				log.Printf("runner: blocked-streak lookup for task %s failed: %v", task.ID, err)
			} else if streak == blockedStreakAlert {
				ev.Status = notify.StatusFailure
				message = fmt.Sprintf("Blocked %d runs in a row — the schedule keeps running, but none of them published: %s", streak, detail)
			}
		}
		ev.Message = truncate.Clamp(strings.TrimSpace(message), maxScheduleStoppedMessage, "…")
		p.deliver(ctx, task, ev)
	}()
}

// ownerEmail resolves the task creator's email — the per-user push audience
// (#292). The sched username IS the chat email for the elcano-auth tier, the
// same assumption cmd/fleet's ownerEmailResolver rests on. Best-effort: an
// anonymous task (nil CreatedBy), a store-less unit fixture, or a lookup
// failure yields "" and the event simply carries no push audience.
func (p *Pool) ownerEmail(ctx context.Context, task *models.Task) string {
	if task.CreatedBy == nil || p.store == nil {
		return ""
	}
	m, err := p.store.GetUsersByIDsWithContext(ctx, []uuid.UUID{*task.CreatedBy})
	if err != nil {
		log.Printf("runner: notify owner lookup for task %s failed: %v", task.ID, err)
		return ""
	}
	return m[*task.CreatedBy]
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
	// The name lands in an email Subject header and the webhook body: a
	// multi-line prompt must become one line first, or the subject carries a
	// header injection and the label wraps mid-word.
	name := strings.Join(strings.Fields(prompt), " ")
	// Rune-boundary clamp (#595): a byte slice can cut a multi-byte rune in
	// half, sending invalid UTF-8 into email subjects and the webhook JSON
	// template.
	return truncate.Clamp(name, maxName, "…")
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
		Message:         scheduleStoppedMessage(task),
	}
}

// maxScheduleStoppedMessage bounds the park reason a notification carries: the
// reason quotes a prompt-derived identifier (already clamped by the validator),
// and the email body is no place for an unbounded line.
const maxScheduleStoppedMessage = 600

// scheduleStoppedMessage is the notification line for a dead-letter that parked
// its recurring chain (ADR-0070, ADR-0073): "Schedule stopped: <reason>", so
// the owner learns the schedule will not fire again and why — for a malformed
// EXECUTION REQUIREMENTS line that is the validator's message, naming the
// identifier. "" for every other event. sendToDeadLetter sets the reason from
// the storage write; nothing else on the runner's copy carries one.
func scheduleStoppedMessage(task *models.Task) string {
	if task.RecurrenceParkedReason == nil || strings.TrimSpace(*task.RecurrenceParkedReason) == "" {
		return ""
	}
	line := strings.Join(strings.Fields(*task.RecurrenceParkedReason), " ")
	return truncate.Clamp("Schedule stopped: "+line, maxScheduleStoppedMessage, "…")
}
