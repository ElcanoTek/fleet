// Package safe provides panic-recovery helpers for long-lived and detached
// goroutines. fleet is single-host by design — one process runs interactive
// chat, the scheduler, and the worker pool — so an unrecovered panic in ANY
// spawned goroutine (a turn, a worker task, a scheduler tick, an SSE persister)
// terminates the whole process and every other user's work with it. Every
// goroutine that is not a child of net/http's per-request recovery must guard
// its entry point with one of these helpers.
//
// Recovery here is deliberately scoped to supervised goroutine ENTRY POINTS; it
// is not a blanket suppressor. A recovered goroutine should mark its unit failed
// (seal the turn buffer, error the task) via the onPanic callback so a panic
// surfaces as a contained failure, not a silent swallow.
package safe

import (
	"log"
	"runtime/debug"
)

// Recover recovers a panic in the current goroutine, logging the goroutine name
// and a full stack trace. Use as the first deferred call in a spawned
// goroutine: `defer safe.Recover("name", onPanic)`. The optional onPanic runs
// (with the recovered value) after logging — use it to mark the unit failed.
func Recover(name string, onPanic func(v any)) {
	if r := recover(); r != nil {
		log.Printf("panic recovered in %s: %v\n%s", name, r, debug.Stack())
		if onPanic != nil {
			onPanic(r)
		}
	}
}

// Go runs fn in a new goroutine guarded by Recover. For fire-and-forget
// goroutines that need no failure bookkeeping beyond logging.
func Go(name string, fn func()) {
	go func() {
		defer Recover(name, nil)
		fn()
	}()
}
