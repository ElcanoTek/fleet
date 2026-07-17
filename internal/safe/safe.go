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
// Prometheus surface is deferred to #176), and fanned out to optional legacy
// and structured hooks. cmd/fleet registers the structured Sentry and
// panic-events adapters; internal/safe imports neither an SDK nor a database.
package safe

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// panicLogger emits recovered panics as structured JSON to stderr.
var panicLogger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

var (
	// SentryHook, when set, is invoked for every recovered panic. The value passed
	// to it is the sanitized panic class and stack is always nil; raw recovered
	// values and stacks never cross a telemetry seam. Forward-compat
	// with a Sentry integration (#193): internal/safe imports no Sentry SDK, so
	// cmd/fleet registers a real hook when FLEET_SENTRY_DSN is configured.
	SentryHook func(name string, v any, stack []byte)

	// PanicEventHook receives the complete structured panic event, including an
	// opaque incident ID and any run/tool attribution supplied by the recovery
	// boundary. Its second argument is the sanitized class, never the recovered
	// value. cmd/fleet wires this to Sentry. It is separate from the legacy
	// SentryHook so existing embedders keep source compatibility while richer
	// recovery points can attach identity without encoding it into a location
	// string.
	PanicEventHook func(event PanicEvent, v any)

	// PanicEventWriter, when set, persists a recovered panic (e.g. to a
	// panic_events table) so operators can query crashes even if stdout was lost.
	// message is the sanitized class and stack is always nil.
	// cmd/fleet registers a store-backed writer at startup. MUST be best-effort:
	// non-blocking and panic-free (a panic here would defeat the recovery).
	PanicEventWriter func(location, message string, stack []byte)

	// StructuredPanicEventWriter persists the complete event. cmd/fleet wires it
	// to the panic_events ledger. The hook MUST be best-effort;
	// EmitPanicWithMetadata nevertheless contains a hook panic so reporting can
	// never defeat recovery.
	StructuredPanicEventWriter func(event PanicEvent)
)

var (
	panicCountMu sync.Mutex
	panicStats   = map[string]PanicStat{}
	incidentSeq  atomic.Uint64
)

// PanicMetadata is safe, non-secret attribution attached by a recovery
// boundary. Identifiers are opaque database IDs already present in Fleet; tool
// arguments and output are deliberately absent.
type PanicMetadata struct {
	IncidentID     string
	Location       string
	Boundary       string
	ToolName       string
	ToolCallID     string
	RunMode        string
	TaskID         string
	ConversationID string
}

// PanicEvent is the secret-safe structured record fanned out to logging, Sentry,
// and the panic_events ledger. Class is derived only from the recovered value's
// Go kind (for example "string" or "error"); the value and stack are discarded
// before any telemetry hook runs.
type PanicEvent struct {
	PanicMetadata
	Class string
}

// PanicStat is the low-cardinality counter and most recent attribution for one
// recovery location. Counts remain keyed only by location so call/task IDs do
// not create an unbounded metrics cardinality surface.
type PanicStat struct {
	Count int64
	Last  PanicMetadata
}

// PanicCounts returns a snapshot of recovered-panic counts by goroutine location.
func PanicCounts() map[string]int64 {
	panicCountMu.Lock()
	defer panicCountMu.Unlock()
	out := make(map[string]int64, len(panicStats))
	for k, v := range panicStats {
		out[k] = v.Count
	}
	return out
}

// PanicStats returns a snapshot of counts plus the last structured attribution
// observed at each location. Unlike PanicCounts it is intended for diagnostics,
// not a metrics label set.
func PanicStats() map[string]PanicStat {
	panicCountMu.Lock()
	defer panicCountMu.Unlock()
	out := make(map[string]PanicStat, len(panicStats))
	for k, v := range panicStats {
		out[k] = v
	}
	return out
}

// EmitPanic logs a recovered panic in structured form, increments the location
// counter, and fans out to the Sentry hook + panic-event writer. Exported so the
// HTTP recovery middleware reuses the exact same emission. It does NOT run an
// onPanic callback — Recover owns that.
func EmitPanic(location string, v any, stack []byte) {
	EmitPanicWithMetadata(PanicMetadata{Location: location}, v, stack)
}

// EmitPanicWithMetadata emits a recovered panic and returns the exact event
// handed to downstream hooks. An opaque incident ID is allocated when the
// caller did not already provide one; returning it lets an in-band error carry
// the same reference. stack is accepted for source compatibility but is
// deliberately discarded together with the recovered value.
func EmitPanicWithMetadata(meta PanicMetadata, v any, _ []byte) PanicEvent {
	if meta.Location == "" {
		meta.Location = "unknown"
	}
	if meta.IncidentID == "" {
		meta.IncidentID = newIncidentID()
	}
	class := PanicClass(v)
	event := PanicEvent{PanicMetadata: meta, Class: class}
	panicLogger.Error("panic recovered",
		"incident_id", meta.IncidentID,
		"goroutine", meta.Location,
		"boundary", meta.Boundary,
		"tool_name", meta.ToolName,
		"tool_call_id", meta.ToolCallID,
		"run_mode", meta.RunMode,
		"task_id", meta.TaskID,
		"conversation_id", meta.ConversationID,
		"panic_class", class,
	)

	panicCountMu.Lock()
	stat := panicStats[meta.Location]
	stat.Count++
	stat.Last = meta
	panicStats[meta.Location] = stat
	panicCountMu.Unlock()

	if SentryHook != nil {
		callReportingHook("SentryHook", func() { SentryHook(meta.Location, class, nil) })
	}
	if PanicEventHook != nil {
		callReportingHook("PanicEventHook", func() { PanicEventHook(event, class) })
	}
	if PanicEventWriter != nil {
		callReportingHook("PanicEventWriter", func() { PanicEventWriter(meta.Location, class, nil) })
	}
	if StructuredPanicEventWriter != nil {
		callReportingHook("StructuredPanicEventWriter", func() { StructuredPanicEventWriter(event) })
	}
	return event
}

// PanicClass returns a bounded, value-free classification suitable for logs and
// telemetry. It never calls Error or String on the recovered value: those
// methods may themselves panic or contain credentials copied into the panic.
func PanicClass(v any) string {
	if v == nil {
		return "nil"
	}
	if _, ok := v.(error); ok {
		return "error"
	}
	return reflect.TypeOf(v).Kind().String()
}

func newIncidentID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Recovery reporting must not fail just because the host entropy source is
		// unavailable. Hash time + a process-local sequence into an opaque fallback;
		// uniqueness, not cryptographic authentication, is the requirement here.
		var seed [16]byte
		binary.BigEndian.PutUint64(seed[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(seed[8:], incidentSeq.Add(1))
		sum := sha256.Sum256(seed[:])
		copy(b[:], sum[:len(b)])
	}
	return "inc_" + hex.EncodeToString(b[:])
}

func callReportingHook(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			panicLogger.Error("panic reporting hook panicked", "hook", name, "panic_class", PanicClass(r))
		}
	}()
	fn()
}

// Recover recovers a panic in the current goroutine, emitting a structured event
// (EmitPanic) then running the optional onPanic callback with the recovered
// value. Use as the first deferred call in a spawned goroutine:
// `defer safe.Recover("name", onPanic)`.
func Recover(name string, onPanic func(v any)) {
	if r := recover(); r != nil {
		EmitPanic(name, r, nil)
		if onPanic != nil {
			onPanic(r)
		}
	}
}

// Go runs fn in a new goroutine guarded by Recover. For fire-and-forget
// goroutines that need no failure bookkeeping beyond the structured event.
func Go(name string, fn func()) {
	_ = goWithDone(name, fn)
}

// goWithDone is Go's joinable form for package tests. Keeping it private
// preserves the fire-and-forget production contract while allowing the race
// suite to prove recovery completed before restoring process-global hooks.
func goWithDone(name string, fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer Recover(name, nil)
		fn()
	}()
	return done
}
