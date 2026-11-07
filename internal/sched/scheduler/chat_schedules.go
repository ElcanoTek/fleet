package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Chat schedules (#2049 M5, ADR-0104): in-chat patrols and timers are fired by
// this scheduler's one tick loop. Each fire is claim (lease) → delivery (the
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

// chatScheduleWakeHorizon: a wake-up further out than this is left to the
// regular tick, which re-arms the wake-up as the time approaches.
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
// at 30 seconds. Non-blocking; a request the loop cannot take now is dropped
// and the tick covers it.
func (s *Scheduler) WakeChatSchedulesAt(t time.Time) {
	if s == nil || t.IsZero() || time.Until(t) > chatScheduleWakeHorizon {
		return
	}
	select {
	case s.chatWake <- t:
	default:
	}
}

// RunChatScheduleNow fires one chat schedule immediately ("Run now"), whether
// or not it is due, through the same claim → delivery → settle path as the
// tick. It returns the delivery: queued (InputID is the new turn), coalesced
// (InputID is the patrol turn already in flight), or gone.
func (s *Scheduler) RunChatScheduleNow(ctx context.Context, scheduleID string) (models.ChatScheduleDelivery, error) {
	if s.chatDeliverer == nil {
		return models.ChatScheduleDelivery{}, errNoChatDeliverer
	}
	now := time.Now()
	fire, err := s.storage.DB().ClaimChatScheduleNow(ctx, scheduleID, now, chatScheduleLease)
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	return s.fireChatSchedule(ctx, fire, now)
}

type chatSchedError string

func (e chatSchedError) Error() string { return string(e) }

const errNoChatDeliverer = chatSchedError("chat schedules are not wired to a chat server")

// runChatSchedules is the tick phase: claim every due schedule (a batch at a
// time), fire each, then re-drive any patrol turn left queued, and arm the
// wake-up for the next schedule due before the next tick.
func (s *Scheduler) runChatSchedules() {
	if s.chatDeliverer == nil {
		return
	}
	s.sweep("chat schedules", func(ctx context.Context) (int, error) {
		fired := 0
		for pass := 0; pass < 4; pass++ {
			now := time.Now()
			fires, err := s.storage.DB().ClaimDueChatSchedules(ctx, now, chatScheduleLease, chatScheduleBatch)
			if err != nil {
				return fired, err
			}
			advanced := 0
			for _, f := range fires {
				d, err := s.fireChatSchedule(ctx, f, now)
				if err != nil {
					log.Printf("scheduler: chat schedule %s fire: %v", safe.LogSafe(f.ScheduleID), err)
					continue
				}
				if d.Outcome != models.ChatScheduleDeferred && d.Outcome != models.ChatScheduleStale {
					advanced++
				}
			}
			fired += advanced
			// A short batch is everything that was due; a batch that moved
			// nothing (all deferred) would only be claimed again.
			if len(fires) < chatScheduleBatch || advanced == 0 {
				break
			}
		}
		s.chatDeliverer.KickQueuedChatScheduleTurns(ctx)
		s.armNextChatScheduleWake(ctx)
		return fired, nil
	}, nil)
}

// fireChatSchedule delivers one claimed fire and settles it. A delivery error
// releases the lease untouched (the fire is retried on a later pass).
func (s *Scheduler) fireChatSchedule(ctx context.Context, fire models.ChatScheduleFire, now time.Time) (models.ChatScheduleDelivery, error) {
	d, derr := s.deliverChatSchedule(ctx, fire)
	if derr != nil {
		d = models.ChatScheduleDelivery{Outcome: models.ChatScheduleDeferred}
	}
	settled, err := s.storage.DB().SettleChatScheduleFire(ctx, fire, d, now)
	if err != nil {
		// The lease expires and the fire is retried; the delivery (if it
		// happened) is idempotent.
		return d, err
	}
	if derr != nil {
		return d, derr
	}
	if !settled {
		log.Printf("scheduler: chat schedule %s: lease lost before settle (a newer claim owns the row)", safe.LogSafe(fire.ScheduleID))
		return d, nil
	}
	switch d.Outcome {
	case models.ChatScheduleStale, models.ChatScheduleDeferred:
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
// runLoop.
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
