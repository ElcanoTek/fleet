package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

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

// chatScheduleBatch is how many due schedules one pass claims.
const chatScheduleBatch = 25

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
	if s == nil || t.IsZero() || time.Until(t) > s.chatWakeHorizon() {
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
	now := time.Now()
	fire, err := s.storage.DB().ClaimChatScheduleNow(ctx, scheduleID, now, chatScheduleLease)
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	fireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatScheduleFireTimeout)
	defer cancel()
	d, err := s.fireChatSchedule(fireCtx, fire, now)
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
		models.ChatScheduleStale, models.ChatScheduleDeferred:
	}
	return d, nil
}

type chatSchedError string

func (e chatSchedError) Error() string { return string(e) }

const errNoChatDeliverer = chatSchedError("chat schedules are not wired to a chat server")

// maxChatScheduleFiresPerSweep is a backstop on one sweep's drain; the sweep
// deadline (sweepTimeout) is the real bound. A backlog larger than this —
// thousands of schedules due at once — finishes on the next pass, which the
// wake-up arms right away.
const maxChatScheduleFiresPerSweep = 10000

// runChatLoop is the chat schedule loop (ADR-0104), its own goroutine beside
// the task loop so nothing on the task path can delay a chat fire: a pass at
// start (a schedule due across a restart fires now), one every tick
// interval, and one at each wake-up — armed by the passes themselves for the
// next schedule or fire retry due before the next tick, and by the chat
// server when a schedule is created or edited. Ends on Stop.
func (s *Scheduler) runChatLoop() {
	defer close(s.chatLoopDone)
	defer safe.Recover("scheduler.chatLoop", nil)
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	var wake chatWakeTimer
	defer wake.stop()
	wake.request(time.Now())
	pass := func() {
		defer safe.Recover("scheduler.chat-schedules", nil)
		s.runChatSchedules()
	}
	for {
		select {
		case <-ticker.C:
			pass()
		case <-s.chatWakeSig:
			if t := s.takeChatWake(); !t.IsZero() {
				wake.request(t)
			}
		case <-wake.c():
			wake.fired()
			pass()
		case <-s.stop:
			return
		}
	}
}

// runChatSchedules is one chat schedule pass: re-drive fires whose lease expired,
// then claim and fire due schedules a batch at a time until none is left (or
// the sweep's deadline), then start the drain of any patrol turn left queued
// and arm the wake-up for the next schedule due before the next tick.
func (s *Scheduler) runChatSchedules() {
	if s.chatDeliverer == nil {
		return
	}
	var left chatBacklog
	s.sweepWithin(s.chatPassTimeout(), "chat schedules", func(ctx context.Context) (int, error) {
		fired, l, err := s.drainChatSchedules(ctx)
		left = l
		return fired, err
	}, nil)
	if s.ctx.Err() != nil {
		return // stopping
	}
	// The bookkeeping after a pass gets a context of its own: the pass's may
	// have run out, and with it this would silently do nothing.
	ctx, cancel := s.storeCtx()
	defer cancel()
	s.chatDeliverer.KickQueuedChatScheduleTurns(ctx)
	s.armNextChatScheduleWake(ctx)
	switch left {
	case chatBacklogLeft:
		// Stopped early with work still due (the pass's deadline or its
		// fire cap): carry on right away rather than at the next tick.
		s.WakeChatSchedulesAt(time.Now())
	case chatBacklogAfterError:
		// A storage error: retry soon, without spinning on it.
		s.WakeChatSchedulesAt(time.Now().Add(chatScheduleErrorRetry))
	case chatBacklogNone:
	}
}

// chatBacklog says whether a pass left due work behind.
type chatBacklog int

const (
	chatBacklogNone       chatBacklog = iota // drained, or deferred (retried by its fires' leases)
	chatBacklogLeft                          // stopped at its deadline or fire cap
	chatBacklogAfterError                    // stopped on a storage error
)

// chatScheduleErrorRetry is how soon a pass that failed on a storage error is
// tried again.
const chatScheduleErrorRetry = 5 * time.Second

// chatPassTimeout bounds one chat schedule pass (claims; each fire has its
// own bound). sweepTimeout unless a test shortens it.
func (s *Scheduler) chatPassTimeout() time.Duration {
	if s.chatPassTimeoutOverride > 0 {
		return s.chatPassTimeoutOverride
	}
	return sweepTimeout
}

// drainChatSchedules fires every recoverable and due schedule. Every claimed
// row is excluded from the rest of the pass (attempted), so a batch of
// schedules whose deliveries fail cannot be claimed again and again ahead of
// the healthy rows behind it; a failed fire also keeps its lease for a
// backoff, so the next passes skip it too.
//
// ctx (the pass's deadline) bounds only the claims: a claimed fire is always
// delivered and settled, each under a fresh bounded context of its own, so a
// pass that runs out of time never strands fires it already leased. It
// reports whether it left due work behind.
func (s *Scheduler) drainChatSchedules(ctx context.Context) (int, chatBacklog, error) {
	fired := 0
	attempted := []string{}
	fire := func(fs []models.ChatScheduleFire, now time.Time) (deferred bool) {
		for _, f := range fs {
			attempted = append(attempted, f.ScheduleID)
			fireCtx, cancel := context.WithTimeout(s.ctx, chatScheduleFireTimeout)
			d, err := s.fireChatSchedule(fireCtx, f, now)
			cancel()
			if err != nil {
				log.Printf("scheduler: chat schedule %s fire: %v", safe.LogSafe(f.ScheduleID), err)
				continue
			}
			switch d.Outcome {
			case models.ChatScheduleDeferred:
				deferred = true
			case models.ChatScheduleStale, models.ChatScheduleRejected:
			default:
				fired++
			}
		}
		return deferred
	}

	stoppedEarly := func() (int, chatBacklog, error) {
		left := chatBacklogLeft
		if stopping := s.ctx.Err() != nil; stopping {
			left = chatBacklogNone
		}
		return fired, left, nil
	}
	recovered := false
	for !recovered {
		if len(attempted) >= maxChatScheduleFiresPerSweep || ctx.Err() != nil {
			return stoppedEarly()
		}
		now := time.Now()
		fs, err := s.storage.DB().RecoverChatScheduleFires(ctx, now, chatScheduleLease, chatScheduleBatch)
		if err != nil {
			if ctx.Err() != nil {
				return stoppedEarly()
			}
			return fired, chatBacklogAfterError, err
		}
		if fire(fs, now) {
			return fired, chatBacklogNone, nil // the chat server is not taking turns now
		}
		recovered = len(fs) < chatScheduleBatch
	}
	for {
		if len(attempted) >= maxChatScheduleFiresPerSweep || ctx.Err() != nil {
			return stoppedEarly()
		}
		now := time.Now()
		fs, err := s.storage.DB().ClaimDueChatSchedules(ctx, now, chatScheduleLease, chatScheduleBatch, attempted)
		if err != nil {
			if ctx.Err() != nil {
				return stoppedEarly()
			}
			return fired, chatBacklogAfterError, err
		}
		if fire(fs, now) {
			return fired, chatBacklogNone, nil
		}
		if len(fs) < chatScheduleBatch {
			return fired, chatBacklogNone, nil
		}
	}
}

// fireChatSchedule delivers one claimed fire and settles it. A delivery error
// keeps the fire in flight with its lease pushed out by a backoff (the
// recovery pass retries it then, by the same idempotency key).
func (s *Scheduler) fireChatSchedule(ctx context.Context, fire models.ChatScheduleFire, now time.Time) (models.ChatScheduleDelivery, error) {
	d, derr := s.deliverChatSchedule(ctx, fire)
	if derr != nil {
		if _, backoff, err := s.storage.DB().FailChatScheduleFire(ctx, fire); err == nil && backoff > 0 {
			return d, fmt.Errorf("%w (retrying in %s)", derr, backoff)
		}
		return d, derr
	}
	settled, err := s.storage.DB().SettleChatScheduleFire(ctx, fire, d, now)
	if err != nil {
		// The lease expires and the fire is retried; the delivery (if it
		// happened) is idempotent.
		return d, err
	}
	if !settled {
		log.Printf("scheduler: chat schedule %s: fire ended before settle (re-claimed or revoked)", safe.LogSafe(fire.ScheduleID))
		return d, nil
	}
	switch d.Outcome {
	case models.ChatScheduleStale, models.ChatScheduleDeferred, models.ChatScheduleRejected:
		// Nothing changed that a client could see.
	case models.ChatScheduleQueued:
		log.Printf("audit: chat schedule %q (run #%d) queued for conversation %q as input %q",
			fire.ScheduleID, fire.RunNumber, fire.ConversationID, d.InputID)
		s.chatDeliverer.ChatScheduleFireSettled(ctx, fire, d)
	case models.ChatScheduleCoalesced, models.ChatScheduleBusy, models.ChatScheduleGone:
		s.chatDeliverer.ChatScheduleFireSettled(ctx, fire, d)
	}
	return d, nil
}

// deliverChatSchedule calls the deliverer with a panic boundary: a panic in
// the chat side fails this fire (its lease is released), not the tick.
func (s *Scheduler) deliverChatSchedule(ctx context.Context, fire models.ChatScheduleFire) (d models.ChatScheduleDelivery, err error) {
	defer safe.Recover("scheduler.chat-schedule-deliver", func(any) {
		err = chatSchedError("chat schedule delivery panicked")
	})
	return s.chatDeliverer.DeliverChatScheduleFire(ctx, fire)
}

// armNextChatScheduleWake schedules a pass for the next active schedule due
// before the next tick.
func (s *Scheduler) armNextChatScheduleWake(ctx context.Context) {
	next, ok, err := s.storage.DB().NextChatScheduleDueAt(ctx, time.Now().Unix())
	if err != nil || !ok {
		return
	}
	at := time.Unix(next, 0)
	if time.Until(at) < s.tickInterval {
		s.WakeChatSchedulesAt(at)
	}
}

// runChatScheduleCleanup prunes old chat schedule run history on the daily
// maintenance sweep, with the task runs' retention policy.
func (s *Scheduler) runChatScheduleCleanup() {
	s.sweepWithin(dailySweepTimeout, "chat schedule run cleanup", func(ctx context.Context) (int, error) {
		return s.storage.DB().CleanupOldChatScheduleRuns(ctx, s.retentionDays, s.keepPerTask)
	}, func(n int) {
		log.Printf("scheduler: pruned %d old chat schedule run(s) (retention=%dd, keep=%d/schedule)", n, s.retentionDays, s.keepPerTask)
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
