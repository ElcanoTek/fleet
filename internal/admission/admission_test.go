package admission

import (
	"context"
	"testing"
	"time"
)

// freeSlot is a never-firing done channel (context.Background().Done() is nil, so
// its select case is disabled). Use it for acquires that should succeed because a
// slot is known to be free — NOT a pre-closed channel, which would race the slot
// case (select picks randomly among ready cases).
func freeSlot() <-chan struct{} { return context.Background().Done() }

func TestNewClampsArgs(t *testing.T) {
	cases := []struct{ total, reserved, wantTotal, wantSched int }{
		{8, 2, 8, 6},
		{4, 1, 4, 3},
		{1, 5, 1, 1},  // reserved clamped to total-1=0 → sched=1
		{0, 0, 1, 1},  // total floored to 1
		{8, -3, 8, 8}, // negative reserved → 0
		{8, 99, 8, 1}, // reserved clamped to 7 → sched=1
	}
	for _, c := range cases {
		l := New(c.total, c.reserved)
		if l.Total() != c.wantTotal || l.SchedulableSlots() != c.wantSched {
			t.Errorf("New(%d,%d): total=%d sched=%d, want total=%d sched=%d",
				c.total, c.reserved, l.Total(), l.SchedulableSlots(), c.wantTotal, c.wantSched)
		}
	}
}

// TestScheduledBoundedAndReserveProtectsChat is the core invariant: scheduled
// tasks can occupy at most total-reserved slots, leaving `reserved` always
// available to interactive chat even when the scheduler is saturated.
func TestScheduledBoundedAndReserveProtectsChat(t *testing.T) {
	const total, reserved = 5, 2 // scheduled may hold at most 3
	l := New(total, reserved)

	// Fill scheduled to its cap.
	var schedReleases []func()
	for i := 0; i < total-reserved; i++ {
		rel, ok := l.TryAcquireScheduled()
		if !ok {
			t.Fatalf("scheduled acquire %d should succeed (cap %d)", i, total-reserved)
		}
		schedReleases = append(schedReleases, rel)
	}
	// One more scheduled must be refused — scheduler is at its sub-cap.
	if _, ok := l.TryAcquireScheduled(); ok {
		t.Fatal("scheduled acquire past total-reserved should be refused")
	}
	if l.InFlight() != total-reserved {
		t.Fatalf("in-flight = %d, want %d", l.InFlight(), total-reserved)
	}

	// Interactive can still claim the reserved slots even though scheduled is maxed.
	var chatReleases []func()
	for i := 0; i < reserved; i++ {
		rel, ok := l.AcquireInteractive(freeSlot()) // slot is free; must not block
		if !ok {
			t.Fatalf("interactive acquire %d should succeed against the reserve", i)
		}
		chatReleases = append(chatReleases, rel)
	}
	if l.InFlight() != total {
		t.Fatalf("in-flight = %d, want full %d", l.InFlight(), total)
	}

	// Now the box is genuinely full — the next interactive turn gets a fast "no".
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok := l.AcquireInteractive(ctx.Done()); ok {
		t.Fatal("interactive acquire on a full box should fail")
	}

	// Releasing a chat slot lets a waiting interactive turn in.
	chatReleases[0]()
	rel, ok := l.AcquireInteractive(freeSlot())
	if !ok {
		t.Fatal("interactive acquire should succeed after a release")
	}
	rel()

	for _, r := range append(schedReleases, chatReleases[1:]...) {
		r()
	}
	if l.InFlight() != 0 {
		t.Fatalf("after releasing all, in-flight = %d, want 0", l.InFlight())
	}
}

func TestAcquireInteractiveBlocksThenSucceeds(t *testing.T) {
	l := New(1, 0)
	rel, ok := l.AcquireInteractive(freeSlot())
	if !ok {
		t.Fatal("first interactive acquire should succeed")
	}
	// Pool full: a waiter should block until the holder releases.
	got := make(chan bool, 1)
	go func() {
		r, ok := l.AcquireInteractive(context.Background().Done())
		if ok {
			r()
		}
		got <- ok
	}()
	select {
	case <-got:
		t.Fatal("interactive acquire should still be blocked while the slot is held")
	case <-time.After(30 * time.Millisecond):
	}
	rel() // free the slot
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("waiter should have acquired after release")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not unblock after release")
	}
}

func TestDoubleReleaseIsSafe(t *testing.T) {
	l := New(2, 0)
	rel, _ := l.AcquireInteractive(freeSlot())
	rel()
	rel() // must not drain a second slot
	if l.InFlight() != 0 {
		t.Fatalf("in-flight = %d after double-release, want 0", l.InFlight())
	}
	// Both slots remain usable.
	for i := 0; i < 2; i++ {
		if _, ok := l.AcquireInteractive(freeSlot()); !ok {
			t.Fatalf("slot %d should be available after a safe double-release", i)
		}
	}
}
