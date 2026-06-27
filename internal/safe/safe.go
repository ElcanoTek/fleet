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
//
// Every recovered panic is emitted as a STRUCTURED JSON event (queryable by a
// log aggregator), counted in-memory (PanicCounts, for an operator probe — a
// Prometheus surface is deferred to #176), and fanned out to two optional hooks:
// SentryHook (forward-compat with #193) and PanicEventWriter (persist to a
// panic_events table). Both are registered by cmd/fleet; internal/safe imports
// neither a Sentry SDK nor a database.
package safe

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
)

// maxStackBytes bounds the stack trace included in the structured event so a
// deep panic can't blow up a log line or DB row.
const maxStackBytes = 4096

// panicLogger emits recovered panics as structured JSON to stderr.
var panicLogger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

var (
	// SentryHook, when set, is invoked for every recovered panic. Forward-compat
	// with a Sentry integration (#193): internal/safe imports no Sentry SDK, so
	// cmd/fleet registers a real hook when FLEET_SENTRY_DSN is configured.
	SentryHook func(name string, v any, stack []byte)

	// PanicEventWriter, when set, persists a recovered panic (e.g. to a
	// panic_events table) so operators can query crashes even if stdout was lost.
	// cmd/fleet registers a store-backed writer at startup. MUST be best-effort:
	// non-blocking and panic-free (a panic here would defeat the recovery).
	PanicEventWriter func(location, message string, stack []byte)
)

var (
	panicCountMu sync.Mutex
	panicCounts  = map[string]int64{}
)

// PanicCounts returns a snapshot of recovered-panic counts by goroutine location.
func PanicCounts() map[string]int64 {
	panicCountMu.Lock()
	defer panicCountMu.Unlock()
	out := make(map[string]int64, len(panicCounts))
	for k, v := range panicCounts {
		out[k] = v
	}
	return out
}

// EmitPanic logs a recovered panic in structured form, increments the location
// counter, and fans out to the Sentry hook + panic-event writer. Exported so the
// HTTP recovery middleware reuses the exact same emission. It does NOT run an
// onPanic callback — Recover owns that.
func EmitPanic(location string, v any, stack []byte) {
	msg := fmt.Sprintf("%v", v)
	s := stack
	if len(s) > maxStackBytes {
		s = s[:maxStackBytes]
	}
	panicLogger.Error("panic recovered",
		"goroutine", location,
		"message", msg,
		"stack", string(s),
	)

	panicCountMu.Lock()
	panicCounts[location]++
	panicCountMu.Unlock()

	if SentryHook != nil {
		SentryHook(location, v, stack)
	}
	if PanicEventWriter != nil {
		PanicEventWriter(location, msg, s)
	}
}

// Recover recovers a panic in the current goroutine, emitting a structured event
// (EmitPanic) then running the optional onPanic callback with the recovered
// value. Use as the first deferred call in a spawned goroutine:
// `defer safe.Recover("name", onPanic)`.
func Recover(name string, onPanic func(v any)) {
	if r := recover(); r != nil {
		EmitPanic(name, r, debug.Stack())
		if onPanic != nil {
			onPanic(r)
		}
	}
}

// Go runs fn in a new goroutine guarded by Recover. For fire-and-forget
// goroutines that need no failure bookkeeping beyond the structured event.
func Go(name string, fn func()) {
	go func() {
		defer Recover(name, nil)
		fn()
	}()
}
