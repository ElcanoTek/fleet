package runner

// outcome.go — recording a run's outcome so that a failed write never makes
// the scheduler run it again.
//
// A run ends in exactly one lease-fenced write: the terminal success or
// error, the retry requeue, the dead-letter, or the ask/wake park. Each of
// those used to be a single attempt. A Postgres restart, failover or
// connection reset at that instant left the row `running`; the worker
// returned, its active entry went away and with it the lease renewals; five
// minutes later lease recovery re-queued the task and the whole run happened
// again — its emails and MCP writes included, prefixed "[retry] attempt 2" —
// or, with max_retries=0, it was dead-lettered as a crash loop with no
// notification. A wall-clock timeout or a self-audit abort, which are never
// retried by design, re-ran by the same route. scheduleNextRecurrence names
// this exact outcome as the thing its post-commit spawn exists to avoid.
//
// recordOutcome closes it in three parts:
//
//   - It retries a transient failure (storage.IsTransientDBError) with
//     bounded backoff for as long as the run keeps its lease. It runs inside
//     executeTask, so the run's active entry — and so its lease renewal —
//     stays in place until the write lands, and the claim's token stays in
//     heldTokens, so this process's lease recovery skips the row even when
//     the outage outlasts the lease (Pool.HeldLeaseTokens). A database outage
//     costs a delay, not a second run.
//   - The write is idempotent through finished_run_id (migration 0213):
//     every outcome write stamps the claim token in its own statement, and a
//     fenced refusal (or a COMMIT whose result was lost) is followed by a
//     read-back. This run's token on a row it no longer leases, in the status
//     it was writing, means an earlier attempt landed; the outcome counts as
//     recorded and its side effects fire once.
//   - Before the first attempt it stamps finished_run_id under the lease
//     (MarkRunFinished). If the write is still failing when shutdown ends
//     the retries, the row stays `running` with that stamp, and lease
//     recovery dead-letters it for review instead of running it again
//     (db.RecoverExpiredLeases). The stamp needs the database too; a run
//     whose stamp could not land either is recovered like a crash. A write
//     that asks for the task to run again (outcomeWrite.rerun) is not
//     stamped first, and clears an earlier stamp of the same run:
//     recovering it like a crash is what it asked for.
//
// A write the database refuses for a reason a retry cannot fix (a constraint,
// a contract violation) is not retried: the caller records a fallback outcome
// — the existing dead-letter→error and requeue→error fallbacks, the
// structured-output persistence class, or for a free-form success a terminal
// error that says the success could not be recorded.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

const (
	// defaultOutcomeAttemptTimeout bounds one attempt of an outcome write (and
	// the finished stamp and the read-back), so a connection that stopped
	// answering cannot hold the retry loop forever.
	defaultOutcomeAttemptTimeout = 15 * time.Second
	// defaultOutcomeRetryBase and defaultOutcomeRetryMax shape the backoff
	// between attempts: doubling from the base up to the cap. The cap stays
	// well under the lease renewal interval, so once the database answers
	// again the write lands within seconds, not at the next renewal.
	defaultOutcomeRetryBase = 250 * time.Millisecond
	defaultOutcomeRetryMax  = 15 * time.Second
	// defaultOutcomeShutdownWindow is how long outcome writes keep trying
	// after the pool force-cancels its runs (the drain grace expired, or
	// ForceCancel). One window for the whole pool, however many writes are
	// pending, so shutdown stays bounded and inside the supervisor's stop
	// timeout (the deploy unit allows 30s past the drain grace).
	defaultOutcomeShutdownWindow = 10 * time.Second
)

// outcomeWrite is one lease-fenced write that records how a run ended.
type outcomeWrite struct {
	// what names the outcome in log lines ("success", "wall-clock timeout").
	what string
	// status is the status the row holds once this write has landed: what
	// the read-back must find, with this run's token in finished_run_id and
	// the lease released, to count an earlier attempt as landed.
	status models.TaskStatus
	// apply performs the write once. It returns storage.ErrTaskLeaseNotHeld
	// when the lease fence refused it (a park's "did not apply" included).
	apply func(ctx context.Context) (*models.Task, error)
	// rerun marks a write that sends the task back to run again (the retry
	// requeue, the connector-outage re-run). recordOutcome does not stamp the
	// run finished before it, and clears a stamp an earlier write of this run
	// left (a refused structured success retried through retry_on): if the
	// write never lands, lease recovery re-queues the task like a crash,
	// which is what the run asked for, instead of dead-lettering it. If that
	// clear fails, the stamp stays and recovery dead-letters the run. The
	// requeue statement stamps the token itself, so the read-back still
	// recognises a requeue that landed.
	rerun bool
}

// outcomeResult is how recordOutcome ended.
type outcomeResult int

const (
	// outcomeLanded: the outcome is on the row — this attempt's write, or an
	// earlier attempt whose result was lost in transit (found by read-back).
	outcomeLanded outcomeResult = iota
	// outcomeLeaseLost: the row no longer belongs to this claim (an operator
	// cancel; another server's lease recovery after an outage longer than the
	// lease), or the fence refused the write and the row could not be read
	// back, so nothing proves the outcome is this run's. Either way the row's
	// outcome is not this run's to claim: no fallback write, no side effects.
	outcomeLeaseLost
	// outcomeRefused: the write failed in a way trying again cannot fix (a
	// constraint, a contract violation, a status the guarded write does not
	// leave from). The claim still holds the row; the caller may record a
	// fallback outcome.
	outcomeRefused
	// outcomeUnwritten: the write was still failing transiently when
	// shutdown ended the retries. The row stays `running`; its finished stamp
	// (when that landed) makes lease recovery dead-letter it rather than run
	// it again.
	outcomeUnwritten
)

func (r outcomeResult) String() string {
	switch r {
	case outcomeLanded:
		return "landed"
	case outcomeLeaseLost:
		return "lease_lost"
	case outcomeRefused:
		return "refused"
	case outcomeUnwritten:
		return "unwritten"
	}
	return "unknown"
}

// recordOutcome stamps the run as finished and applies w until it lands, the
// lease is genuinely gone, the database refuses it for good, or shutdown ends
// the retries (see the file comment). It returns the row the write (or the
// read-back) produced when it landed.
func (p *Pool) recordOutcome(ctx context.Context, taskID, token uuid.UUID, w outcomeWrite) (*models.Task, outcomeResult, error) {
	lg := logging.From(ctx)
	marked := false
	if w.rerun {
		p.clearRunFinished(ctx, taskID, token)
	} else {
		marked = p.markRunFinished(ctx, taskID, token)
	}
	for attempt := 1; ; attempt++ {
		actx, cancel := p.outcomeAttemptContext(ctx)
		landed, err := w.apply(actx)
		cancel()
		if p.afterOutcomeAttempt != nil {
			err = p.afterOutcomeAttempt(w.what, err)
		}
		if err == nil {
			if attempt > 1 {
				lg.Info("runner: outcome recorded after retrying", "task_id", taskID, "outcome", w.what, "attempts", attempt)
			}
			return landed, outcomeLanded, nil
		}

		retry := storage.IsTransientDBError(err)
		// A fenced refusal, or a COMMIT whose result never arrived, may be an
		// earlier attempt (or this one) having landed: read the row back.
		if errors.Is(err, storage.ErrTaskLeaseNotHeld) || errors.Is(err, storage.ErrTaskCommitOutcomeUnknown) {
			row, state, rerr := p.outcomeReadBack(ctx, taskID, token, w.status)
			switch {
			case rerr != nil && storage.IsTransientDBError(rerr):
				// Unknown until the database answers again.
				retry = true
				err = fmt.Errorf("%w (read-back: %w)", err, rerr)
			case rerr != nil && errors.Is(err, storage.ErrTaskLeaseNotHeld):
				// The fence refused the write and the row cannot be read:
				// nothing proves the outcome is this run's, so fail closed.
				return nil, outcomeLeaseLost, fmt.Errorf("%w (read-back: %w)", err, rerr)
			case rerr != nil:
				// A COMMIT of unknown result and an unreadable row: the
				// write's own cause decides, as below. A transient cause
				// retries, and the next attempt either lands or meets the
				// fence.
				err = fmt.Errorf("%w (read-back: %w)", err, rerr)
			case state == readBackApplied:
				lg.Info("runner: outcome found already recorded on read-back", "task_id", taskID, "outcome", w.what, "attempts", attempt)
				return row, outcomeLanded, nil
			case state == readBackOtherOwner:
				return nil, outcomeLeaseLost, err
			case errors.Is(err, storage.ErrTaskLeaseNotHeld):
				// Still this claim's row, yet the guarded write did not
				// apply: the row is not in a status that write leaves from.
				return nil, outcomeRefused, err
			}
			// A COMMIT that did not land on a row this claim still holds:
			// its cause decides, like any other failure.
		}
		if !retry {
			return nil, outcomeRefused, err
		}

		delay, ok := p.outcomeRetryWait(attempt)
		if !ok {
			// Shutdown ended the retries. One last read-back: the last
			// attempt may have landed with its result lost.
			if row, state, rerr := p.outcomeReadBack(ctx, taskID, token, w.status); rerr == nil && state == readBackApplied {
				return row, outcomeLanded, nil
			}
			switch {
			case marked:
				lg.Error("runner: outcome still not recorded at shutdown; the run is marked finished, so lease recovery will dead-letter it for review rather than run it again", "task_id", taskID, "outcome", w.what, "attempts", attempt, "err", err)
			case w.rerun:
				lg.Error("runner: re-run request still not recorded at shutdown; lease recovery will re-queue the task like a crashed run (unless its finished stamp could not be cleared)", "task_id", taskID, "outcome", w.what, "attempts", attempt, "err", err)
			default:
				lg.Error("runner: outcome still not recorded at shutdown, and the run could not be marked finished; lease recovery will treat it like a crashed run (unless an earlier write of this run already stamped it)", "task_id", taskID, "outcome", w.what, "attempts", attempt, "err", err)
			}
			return nil, outcomeUnwritten, err
		}
		lg.Warn("runner: outcome write failed; retrying while this run keeps its lease", "task_id", taskID, "outcome", w.what, "attempt", attempt, "retry_in", delay, "err", err)
		p.waitOutcomeRetry(delay)
	}
}

// readBackState is what the read-back after an ambiguous attempt found.
type readBackState int

const (
	// readBackApplied: this run's outcome is on the row.
	readBackApplied readBackState = iota
	// readBackStillOurs: this claim still leases the row; nothing landed.
	readBackStillOurs
	// readBackOtherOwner: the row moved on without this run's outcome.
	readBackOtherOwner
)

// outcomeReadBack reads the row to settle an ambiguous attempt. The write
// stamps finished_run_id with the claim token in the same statement that
// releases the lease, so "this run's token, lease released, the status the
// write leaves" is that write and nothing else: lease recovery clears the
// stamp when it dead-letters, and an operator cancel or a later claim leaves
// a different status or token.
func (p *Pool) outcomeReadBack(ctx context.Context, taskID, token uuid.UUID, status models.TaskStatus) (*models.Task, readBackState, error) {
	rctx, cancel := p.outcomeAttemptContext(ctx)
	defer cancel()
	row, err := p.store.GetTaskWithContext(rctx, taskID)
	if p.afterReadBack != nil {
		err = p.afterReadBack(err)
	}
	if err != nil {
		return nil, readBackOtherOwner, err
	}
	if row == nil {
		return nil, readBackOtherOwner, nil
	}
	tok := token.String()
	if row.LeaseOwner != nil && *row.LeaseOwner == tok {
		return row, readBackStillOurs, nil
	}
	if row.FinishedRunID != nil && *row.FinishedRunID == tok && row.Status == status {
		return row, readBackApplied, nil
	}
	return row, readBackOtherOwner, nil
}

// markRunFinished stamps finished_run_id with this run's token under its
// lease, before the outcome write (see db.MarkRunFinished), and reports
// whether the stamp landed. Best effort: the outcome write stamps the token
// too, so a missed stamp only matters if the outcome then never lands.
func (p *Pool) markRunFinished(ctx context.Context, taskID, token uuid.UUID) bool {
	mctx, cancel := p.outcomeAttemptContext(ctx)
	defer cancel()
	ok, err := p.store.MarkRunFinished(mctx, taskID, token)
	if err != nil {
		logging.From(ctx).Warn("runner: could not mark the run finished before recording its outcome", "task_id", taskID, "err", err)
	}
	return ok
}

// clearRunFinished removes this run's own finished stamp before a write that
// asks for the task to run again (see outcomeWrite.rerun). Best effort: if it
// fails, the stamp stays, and a requeue that then never lands is
// dead-lettered by lease recovery instead of re-queued.
func (p *Pool) clearRunFinished(ctx context.Context, taskID, token uuid.UUID) {
	cctx, cancel := p.outcomeAttemptContext(ctx)
	defer cancel()
	if _, err := p.store.ClearRunFinished(cctx, taskID, token); err != nil {
		logging.From(ctx).Warn("runner: could not clear the run's finished mark before asking to run it again", "task_id", taskID, "err", err)
	}
}

// outcomeAttemptContext bounds one database call of the outcome path. It
// keeps ctx's values (the run's log correlation and trace) but not its
// cancellation: the run context is often already cancelled (interrupted,
// wall timeout, a park) when its outcome is written. After a force-cancel the
// bound shrinks to what is left of the shutdown window.
func (p *Pool) outcomeAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := orDefault(p.outcomeAttemptTimeout, defaultOutcomeAttemptTimeout)
	if deadline, forced := p.outcomeShutdownDeadline(); forced {
		if left := time.Until(deadline); left < timeout {
			timeout = max(left, time.Second)
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// outcomeRetryWait returns how long to wait before the next attempt, and
// false once the shutdown window has closed.
func (p *Pool) outcomeRetryWait(attempt int) (time.Duration, bool) {
	delay := orDefault(p.outcomeRetryMax, defaultOutcomeRetryMax)
	if attempt < 30 {
		if d := orDefault(p.outcomeRetryBase, defaultOutcomeRetryBase) << (attempt - 1); d > 0 && d < delay {
			delay = d
		}
	}
	if deadline, forced := p.outcomeShutdownDeadline(); forced {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, false
		}
		delay = min(delay, left)
	}
	return delay, true
}

// waitOutcomeRetry sleeps d, waking early when the pool force-cancels so the
// next attempt runs under the shutdown window.
func (p *Pool) waitOutcomeRetry(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	if _, forced := p.outcomeShutdownDeadline(); forced {
		<-t.C
		return
	}
	select {
	case <-t.C:
	case <-p.forceStop:
	}
}

// outcomeShutdownDeadline reports when outcome retries stop: the shutdown
// window after the pool force-cancelled its runs. forced is false until then.
func (p *Pool) outcomeShutdownDeadline() (deadline time.Time, forced bool) {
	at := p.forceStopAt.Load()
	if at == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, at).Add(orDefault(p.outcomeShutdownWindow, defaultOutcomeShutdownWindow)), true
}

// orDefault keeps a Pool built without NewPool (a test's struct literal) on
// the production retry shape instead of zero timeouts and zero backoff.
func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// signalForceStop records that the pool force-cancelled its runs and wakes
// outcome retries waiting on a backoff. Idempotent.
func (p *Pool) signalForceStop() {
	p.forceStopOnce.Do(func() {
		p.forceStopAt.Store(time.Now().UnixNano())
		if p.forceStop != nil {
			close(p.forceStop)
		}
	})
}

// recordTerminalError records a terminal error outcome (msg on the row)
// through recordOutcome and reports whether it landed — the gate for the
// failure notification (#580).
func (p *Pool) recordTerminalError(ctx context.Context, taskID, token uuid.UUID, what, msg string) bool {
	_, result, err := p.recordOutcome(ctx, taskID, token, outcomeWrite{
		what:   what,
		status: models.TaskStatusError,
		apply: func(actx context.Context) (*models.Task, error) {
			return p.reportStatus(actx, taskID, token, models.TaskStatusError, msg)
		},
	})
	if result != outcomeLanded {
		logging.From(ctx).Error("runner: failed to record the outcome for task", "task_id", taskID, "outcome", what, "result", result.String(), "err", err)
	}
	return result == outcomeLanded
}

// parkWrite adapts a guarded park writer's (applied, error) to apply's
// contract: a park that did not apply is the fence refusing it, and the
// read-back tells a lost lease from a row the park cannot leave from.
func parkWrite(park func(ctx context.Context) (bool, error)) func(ctx context.Context) (*models.Task, error) {
	return func(ctx context.Context) (*models.Task, error) {
		ok, err := park(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, storage.ErrTaskLeaseNotHeld
		}
		return nil, nil
	}
}
