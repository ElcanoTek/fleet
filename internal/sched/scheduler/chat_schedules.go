package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Chat schedules (#2049 M5, ADR-0104): in-chat patrols and timers are fired by
// this scheduler's chat schedule loop (runChatLoop), a goroutine of its own
// beside the task loop. Each fire is claim (lease) → delivery (the
// chat server enqueues the turn, idempotently) → settle (run count, next slot,
// run history, lease released). See db/chat_schedules.go for the lease rules.

// ChatScheduleDeliverer is the chat server's side of a fire. The scheduler
// cannot import the chat store; cmd wires the chat HTTP server in.
type ChatScheduleDeliverer interface {
	// DeliverChatScheduleFire enqueues the fire's turn into its conversation
	// (or says why not). It must be idempotent for a given fire: a fire
	// whose lease expired before it settled is delivered again.
	DeliverChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire) (models.ChatScheduleDelivery, error)
	// ChatScheduleFireSettled runs after a fire settled: push the queue and
	// patrol updates to open clients and start the conversation's drain.
	ChatScheduleFireSettled(ctx context.Context, fire models.ChatScheduleFire, d models.ChatScheduleDelivery)
	// KickQueuedChatScheduleTurns starts the drain of conversations holding
	// a queued patrol turn (one left queued by a restart, say).
	KickQueuedChatScheduleTurns(ctx context.Context)
}

// chatScheduleLease bounds one fire's claim → settle. Delivery is one short
// chat transaction, so the bound is generous; it matters only when the
// process dies mid-fire, and then it is how long the slot waits to be fired
// again.
const chatScheduleLease = 2 * time.Minute

// chatScheduleWakeHorizon is the least a wake-up request may be ahead of now
// and still be kept (see chatWakeHorizon).
const chatScheduleWakeHorizon = 5 * time.Minute

// minChatScheduleWake keeps a burst of wake requests from spinning the loop.
const minChatScheduleWake = 50 * time.Millisecond

// SetChatScheduleDeliverer wires the chat server in. Call before Start. Nil
// (the default) leaves chat schedules unfired, e.g. in scheduler-only tests.
func (s *Scheduler) SetChatScheduleDeliverer(d ChatScheduleDeliverer) {
	s.chatDeliverer = d
}

// WakeChatSchedulesAt asks the loop to run a chat schedule pass at t, instead
// of waiting for the next tick: a timer created for 30 seconds from now fires
// at 30 seconds. Non-blocking and never lost: requests coalesce into the
// earliest one pending (chatWakeAt), and a capacity-1 signal tells the loop
// to take it, so however many arrive while the loop is busy in a pass, the
// earliest survives and memory stays bounded. A request further out than the
// wake horizon (the larger of chatScheduleWakeHorizon and the tick interval)
// is dropped: some tick runs before it is due and re-arms it then.
func (s *Scheduler) WakeChatSchedulesAt(t time.Time) {
	if s == nil || t.IsZero() || t.Sub(s.now()) > s.chatWakeHorizon() {
		return
	}
	s.chatWakeMu.Lock()
	if s.chatWakeAt.IsZero() || t.Before(s.chatWakeAt) {
		s.chatWakeAt = t
	}
	s.chatWakeMu.Unlock()
	select {
	case s.chatWakeSig <- struct{}{}:
	default: // a signal is already pending; the loop will read chatWakeAt
	}
}

// takeChatWake returns and clears the earliest pending wake request (zero
// when there is none).
func (s *Scheduler) takeChatWake() time.Time {
	s.chatWakeMu.Lock()
	defer s.chatWakeMu.Unlock()
	t := s.chatWakeAt
	s.chatWakeAt = time.Time{}
	return t
}

// chatWakeHorizon: every tick re-arms the wake-up for a schedule due before
// the next tick, so any request within one tick interval must be kept.
func (s *Scheduler) chatWakeHorizon() time.Duration {
	if s.tickInterval > chatScheduleWakeHorizon {
		return s.tickInterval
	}
	return chatScheduleWakeHorizon
}

// chatScheduleFireTimeout bounds one fire's delivery and settle when it runs
// on a context detached from its caller (Run now).
const chatScheduleFireTimeout = 30 * time.Second

// RunChatScheduleNow fires one chat schedule immediately ("Run now"), whether
// or not it is due, through the same claim → delivery → settle path as the
// tick. It returns the delivery: queued (InputID is the new turn) or gone.
// A schedule with a fire in flight, or whose patrol turn is still queued or
// running, answers ErrChatScheduleFiring: of two Run nows racing, one runs.
//
// Only the claim runs on ctx. Once claimed, the fire is the scheduler's: its
// delivery and settle run on a context detached from the caller and bounded
// by chatScheduleFireTimeout, so an HTTP client that disconnects cannot leave
// a delivered turn unsettled with its lease held. (Were the process to die
// there instead, the recovery pass re-drives the same fire by its id.)
func (s *Scheduler) RunChatScheduleNow(ctx context.Context, scheduleID string) (models.ChatScheduleDelivery, error) {
	if s.chatDeliverer == nil {
		return models.ChatScheduleDelivery{}, errNoChatDeliverer
	}
	now := s.now()
	fire, err := s.storage.DB().ClaimChatScheduleNow(ctx, scheduleID, now, chatScheduleLease)
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	fireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatScheduleFireTimeout)
	defer cancel()
	d, err := s.fireChatSchedule(fireCtx, fire)
	// As a worker does: ask for a pass, which arms the wake-up for a fire
	// this kept in flight (deferred, or a failed delivery's backoff) — its
	// retry then runs on time instead of at the next tick.
	s.WakeChatSchedulesAt(time.Now())
	if err != nil {
		return d, err
	}
	switch d.Outcome {
	case models.ChatScheduleCoalesced:
		// A patrol turn of this schedule is still queued or running: of two
		// Run nows racing, exactly one runs and the other is told so.
		return d, models.ErrChatScheduleFiring
	case models.ChatScheduleRejected:
		// Refused for good (the fire has ended): say why, never run it later.
		if d.Reason == "queue_full" {
			return d, models.ErrChatScheduleQueueFull
		}
		return d, fmt.Errorf("%w: %s", models.ErrChatScheduleNotRunnable, d.Reason)
	case models.ChatScheduleQueued, models.ChatScheduleBusy, models.ChatScheduleGone,
		models.ChatScheduleStale, models.ChatScheduleDeferred, models.ChatScheduleSuperseded:
	}
	return d, nil
}

type chatSchedError string

func (e chatSchedError) Error() string { return string(e) }

const errNoChatDeliverer = chatSchedError("chat schedules are not wired to a chat server")

// chatScheduleWorkers is how many fires are delivered at once. A pass claims
// no more than the free workers, so a fire is never claimed (and leased)
// before a worker can deliver it well inside its lease.
const chatScheduleWorkers = 4

// chatScheduleWriteTimeout bounds a write that records a fire's result
// (settle, failure backoff) or publishes it. It runs on its own context —
// never the fire's, which may have expired on a slow delivery — detached from
// Stop so a result that came in is recorded rather than lost at shutdown.
const chatScheduleWriteTimeout = 10 * time.Second

// now is the scheduler's clock (a test may substitute one).
func (s *Scheduler) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// writeCtx is a fresh bounded context for recording a result.
func (s *Scheduler) writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), chatScheduleWriteTimeout)
}

// runChatLoop is the chat schedule loop (ADR-0104), its own goroutine beside
// the task loop so nothing on the task path can delay a chat fire: a pass at
// start (a schedule due across a restart fires now), one every tick
// interval, and one at each wake-up — armed by the passes for the next
// schedule or fire retry due before the next tick, by every finished fire
// (to claim what is still due), and by the chat server when a schedule is
// created or edited. A pass only claims: deliveries run on a bounded pool of
// workers, so the loop is back in its select within one claim statement and
// a wake-up is never held behind a slow delivery. Ends on Stop.
func (s *Scheduler) runChatLoop() {
	defer close(s.chatLoopDone)
	defer safe.Recover("scheduler.chatLoop", nil)
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	var wake chatWakeTimer
	defer wake.stop()
	wake.request(time.Now())
	for {
		select {
		case <-ticker.C:
			s.runChatSchedules()
		case <-s.chatWakeSig:
			if t := s.takeChatWake(); !t.IsZero() {
				wake.request(t)
			}
		case <-wake.c():
			wake.fired()
			s.runChatSchedules()
		case <-s.stop:
			return
		}
	}
}

// runChatSchedules is one chat schedule pass (see chatPass), with a panic
// boundary.
func (s *Scheduler) runChatSchedules() {
	defer safe.Recover("scheduler.chat-schedules", nil)
	s.chatPass()
}

// chatScheduleErrorRetry is how soon a pass that failed on a storage error is
// tried again.
const chatScheduleErrorRetry = 5 * time.Second

// chatPassTimeout bounds a pass's claim statements. sweepTimeout unless a
// test shortens it.
func (s *Scheduler) chatPassTimeout() time.Duration {
	if s.chatPassTimeoutOverride > 0 {
		return s.chatPassTimeoutOverride
	}
	return sweepTimeout
}

// chatPass claims up to the free workers' worth of fires — re-driving fires
// whose lease expired first, then due schedules — and hands each to a worker,
// then kicks queued patrol turns and arms the wake-up for the next work due
// before the next tick. It returns how many fires it dispatched.
//
// Continuing a backlog needs no deadline or cap here: every finished fire
// asks for another pass, which claims more while any is due. That cannot
// spin: a claimed row stays out of the claims (leased, then settled, or kept
// for a backoff or a retry) until something durable changes it, so a pass
// that finds nothing claimable dispatches nothing and asks for nothing.
func (s *Scheduler) chatPass() int {
	if s.chatDeliverer == nil || s.ctx.Err() != nil {
		return 0
	}
	s.chatPasses.Add(1)
	dispatched := 0
	func() {
		ctx, cancel := context.WithTimeout(s.ctx, s.chatPassTimeout())
		defer cancel()
		claim := func(name string, get func(limit int) ([]models.ChatScheduleFire, error)) bool {
			free := cap(s.chatSlots) - len(s.chatSlots)
			if free <= 0 {
				return false
			}
			fs, err := get(free)
			if err != nil {
				if s.ctx.Err() == nil {
					logging.From(ctx).Error("scheduler: chat schedules: claim failed", "claim", name, "err", err, "retry_in", chatScheduleErrorRetry)
					s.WakeChatSchedulesAt(time.Now().Add(chatScheduleErrorRetry))
				}
				return false
			}
			for _, f := range fs {
				s.dispatchChatFire(f)
				dispatched++
			}
			return true
		}
		now := s.now()
		if claim("recover", func(limit int) ([]models.ChatScheduleFire, error) {
			return s.storage.DB().RecoverChatScheduleFires(ctx, now, chatScheduleLease, limit)
		}) {
			claim("claim", func(limit int) ([]models.ChatScheduleFire, error) {
				return s.storage.DB().ClaimDueChatSchedules(ctx, now, chatScheduleLease, limit, nil)
			})
		}
	}()
	// Bookkeeping on a context of its own: the claims' may have run out.
	ctx, cancel := s.storeCtx()
	defer cancel()
	s.chatDeliverer.KickQueuedChatScheduleTurns(ctx)
	s.armNextChatScheduleWake(ctx)
	return dispatched
}

// dispatchChatFire hands one claimed fire to a worker (taking a slot, which
// the caller made sure is free). The worker delivers it under its own
// bounded context, records the result under another, and asks for the next
// pass.
func (s *Scheduler) dispatchChatFire(f models.ChatScheduleFire) {
	s.chatSlots <- struct{}{}
	s.chatWorkers.Add(1)
	go func() {
		defer s.chatWorkers.Done()
		// Ask for the next pass only once this worker's slot is free again
		// (defers run last-first), so that pass can claim into it.
		defer func() { s.WakeChatSchedulesAt(time.Now()) }()
		defer func() { <-s.chatSlots }()
		defer safe.RecoverContext(logging.WithConversation(s.ctx, f.ConversationID), "scheduler.chat-schedule-fire", nil)
		timeout := chatScheduleFireTimeout
		if s.chatFireTimeoutOverride > 0 {
			timeout = s.chatFireTimeoutOverride
		}
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		if _, err := s.fireChatSchedule(ctx, f); err != nil {
			logging.From(logging.WithConversation(ctx, f.ConversationID)).Error("scheduler: chat schedule fire failed", "schedule_id", safe.LogSafe(f.ScheduleID), "err", err)
		}
	}()
}

// fireChatSchedule delivers one claimed fire and settles it. Only the
// delivery runs on ctx; the result is recorded and published under a fresh
// bounded context (writeCtx), so a delivery that used up ctx still leaves its
// settle — or its failure and backoff — on record. A delivery error keeps the
// fire in flight with its lease pushed out by a backoff (the recovery pass
// retries it then, by the same idempotency key).
//
// The settle is timed when the delivery has finished (s.now() after it), not
// when the fire was claimed: the next slot is computed from that time, so a
// delivery that crosses a slot boundary moves the schedule to a future slot
// rather than one already past (which would fire again at once), and the run
// history's fired_at is when the turn was actually queued.
func (s *Scheduler) fireChatSchedule(ctx context.Context, fire models.ChatScheduleFire) (models.ChatScheduleDelivery, error) {
	var (
		d    models.ChatScheduleDelivery
		derr error
	)
	// A fire claimed under a definition edited since (def_version moved) is
	// superseded before anything is delivered. The deliverer repeats this
	// check inside its delivery transaction (the authoritative one: an edit
	// can land between the two); once a turn is delivered, an edit no longer
	// supersedes it — the settle counts the run.
	if live, err := s.storage.DB().ChatScheduleDefVersion(ctx, fire.ScheduleID); err == nil && live != fire.DefVersion {
		d = models.ChatScheduleDelivery{Outcome: models.ChatScheduleSuperseded}
	} else {
		d, derr = s.deliverChatSchedule(ctx, fire)
	}
	now := s.now()
	wctx, cancel := s.writeCtx()
	defer cancel()
	if derr != nil {
		if _, backoff, err := s.storage.DB().FailChatScheduleFire(wctx, fire); err == nil && backoff > 0 {
			return d, fmt.Errorf("%w (retrying in %s)", derr, backoff)
		}
		return d, derr
	}
	settled, err := s.storage.DB().SettleChatScheduleFire(wctx, fire, d, now)
	if err != nil {
		// The lease expires and the fire is retried; the delivery (if it
		// happened) is idempotent.
		return d, err
	}
	if !settled {
		logging.From(logging.WithConversation(wctx, fire.ConversationID)).Info("scheduler: chat schedule fire ended before settle (re-claimed or revoked)", "schedule_id", safe.LogSafe(fire.ScheduleID))
		return d, nil
	}
	switch d.Outcome {
	case models.ChatScheduleStale, models.ChatScheduleDeferred, models.ChatScheduleRejected, models.ChatScheduleSuperseded:
		// Nothing changed that a client could see.
	case models.ChatScheduleQueued:
		logging.From(logging.WithConversation(wctx, fire.ConversationID)).Info("audit: chat schedule queued", "schedule_id", safe.LogSafe(fire.ScheduleID), "run", fire.RunNumber, "input_id", safe.LogSafe(d.InputID))
		s.chatDeliverer.ChatScheduleFireSettled(wctx, fire, d)
	case models.ChatScheduleCoalesced, models.ChatScheduleBusy, models.ChatScheduleGone:
		s.chatDeliverer.ChatScheduleFireSettled(wctx, fire, d)
	}
	return d, nil
}

// deliverChatSchedule calls the deliverer with a panic boundary: a panic in
// the chat side fails this fire (its lease is released), not the tick.
func (s *Scheduler) deliverChatSchedule(ctx context.Context, fire models.ChatScheduleFire) (d models.ChatScheduleDelivery, err error) {
	defer safe.RecoverContext(ctx, "scheduler.chat-schedule-deliver", func(any) {
		err = chatSchedError("chat schedule delivery panicked")
	})
	return s.chatDeliverer.DeliverChatScheduleFire(ctx, fire)
}

// armNextChatScheduleWake schedules a pass for the next work due before the
// next tick. A lookup that fails is logged and retried in
// chatScheduleErrorRetry — never silently dropped, which would leave a due
// schedule to the next tick.
func (s *Scheduler) armNextChatScheduleWake(ctx context.Context) {
	lookup := s.nextChatDueAt
	if lookup == nil {
		lookup = s.storage.DB().NextChatScheduleDueAt
	}
	now := s.now()
	next, ok, err := lookup(ctx, now.Unix())
	if err != nil {
		if s.ctx.Err() == nil {
			logging.From(ctx).Error("scheduler: chat schedules: next due time lookup failed", "err", err, "retry_in", chatScheduleErrorRetry)
			s.WakeChatSchedulesAt(time.Now().Add(chatScheduleErrorRetry))
		}
		return
	}
	if ok {
		if at := time.Unix(next, 0); at.Sub(now) < s.tickInterval {
			s.WakeChatSchedulesAt(at)
		}
	}
	// Work already due that this pass could not claim — a row another
	// transaction held locked (SKIP LOCKED passed over it), or a full worker
	// pool — is not in the lookup above (it only sees future times). Retry
	// it after chatScheduleErrorRetry rather than leave it to the next tick;
	// the interval bounds it, so a row locked for long cannot spin the loop.
	overdue, err := s.storage.DB().OverdueChatScheduleWork(ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			logging.From(ctx).Error("scheduler: chat schedules: overdue check failed", "err", err, "retry_in", chatScheduleErrorRetry)
			s.WakeChatSchedulesAt(time.Now().Add(chatScheduleErrorRetry))
		}
		return
	}
	if overdue {
		s.WakeChatSchedulesAt(time.Now().Add(chatScheduleErrorRetry))
	}
}

// runChatScheduleCleanup prunes old chat schedule run history on the daily
// maintenance sweep, with the task runs' retention policy.
func (s *Scheduler) runChatScheduleCleanup() {
	s.sweepWithin(dailySweepTimeout, "chat schedule run cleanup", func(ctx context.Context) (int, error) {
		return s.storage.DB().CleanupOldChatScheduleRuns(ctx, s.retentionDays, s.keepPerTask)
	}, func(n int) {
		logging.From(s.ctx).Info("scheduler: pruned old chat schedule runs", "runs", n, "retention_days", s.retentionDays, "keep_per_schedule", s.keepPerTask)
	})
}

// chatWakeTimer tracks the earliest requested chat schedule wake-up for
// runChatLoop.
type chatWakeTimer struct {
	timer *time.Timer
	at    time.Time
}

// request arms the wake-up for t, or pulls an armed one earlier.
func (w *chatWakeTimer) request(t time.Time) {
	if !w.at.IsZero() && !t.Before(w.at) {
		return
	}
	d := time.Until(t)
	if d < minChatScheduleWake {
		d = minChatScheduleWake
	}
	if w.timer == nil {
		w.timer = time.NewTimer(d)
	} else {
		if !w.timer.Stop() {
			select {
			case <-w.timer.C:
			default:
			}
		}
		w.timer.Reset(d)
	}
	w.at = t
}

// fired clears the armed wake-up after its channel delivered.
func (w *chatWakeTimer) fired() { w.at = time.Time{} }

// c is the channel to select on (nil — never ready — when nothing is armed).
func (w *chatWakeTimer) c() <-chan time.Time {
	if w.timer == nil || w.at.IsZero() {
		return nil
	}
	return w.timer.C
}

func (w *chatWakeTimer) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}
