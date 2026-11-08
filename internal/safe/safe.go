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
// Every recovered panic is emitted as a STRUCTURED log event through the
// process's configured handler (format, redactor, FLEET_LOG_FILE), with a
// direct-to-stderr JSON fallback if that handler fails, and fanned out to optional legacy and structured hooks. cmd/fleet registers the structured Sentry and
// panic-events adapters; internal/safe imports neither an SDK nor a database.
package safe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

// panicLogger emits recovered panics as structured JSON straight to stderr.
// It is the FAIL-SAFE path only: once logging.Configure has run, panic
// records go through the configured handler (SetPanicHandler) — its format,
// its redactor, its file sink — and panicLogger is used when that handler
// is unset, panics, or returns an error, so a broken log sink can never
// swallow a recovered panic.
var panicLogger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

// panicHandler is the process's configured log handler, installed by
// logging.Configure. internal/logging imports this package's setter rather
// than this package importing logging, so there is no import cycle.
var panicHandler atomic.Pointer[slog.Handler]

// SetPanicHandler routes recovered-panic records through h (nil restores the
// direct-stderr fallback).
func SetPanicHandler(h slog.Handler) {
	if h == nil {
		panicHandler.Store(nil)
		return
	}
	panicHandler.Store(&h)
}

// logPanic writes one error record through the configured handler, falling
// back to panicLogger when there is none or it fails.
func logPanic(ctx context.Context, msg string, args ...any) {
	if hp := panicHandler.Load(); hp != nil && handleSafely(ctx, *hp, msg, args) {
		return
	}
	panicLogger.Error(msg, args...)
}

func handleSafely(ctx context.Context, h slog.Handler, msg string, args []any) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	if !h.Enabled(ctx, slog.LevelError) {
		return false
	}
	r := slog.NewRecord(time.Now(), slog.LevelError, msg, 0)
	r.Add(args...)
	return h.Handle(ctx, r) == nil
}

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

var incidentSeq atomic.Uint64

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

// EmitPanic logs a recovered panic in structured form and fans out to the Sentry hook + panic-event writer. Exported so the
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
func EmitPanicWithMetadata(meta PanicMetadata, v any, stack []byte) PanicEvent {
	return EmitPanicWithMetadataContext(context.Background(), meta, v, stack)
}

// EmitPanicWithMetadataContext is EmitPanicWithMetadata for a panic recovered
// while serving a request or a run: ctx is that request's or run's context,
// and the configured handler (logging.Configure) adds its correlation —
// request_id, trace_id, span_id, conv_id, run_id … — to the event.
func EmitPanicWithMetadataContext(ctx context.Context, meta PanicMetadata, v any, _ []byte) PanicEvent {
	if meta.Location == "" {
		meta.Location = "unknown"
	}
	if meta.IncidentID == "" {
		meta.IncidentID = newIncidentID()
	}
	class := PanicClass(v)
	event := PanicEvent{PanicMetadata: meta, Class: class}
	// Only the metadata the boundary knows: an empty attribute names nothing,
	// and the handler's context correlation (task_id, run_id, conv_id …)
	// fills in what the caller did not (#2074 review).
	args := []any{"incident_id", LogSafe(meta.IncidentID), "goroutine", LogSafe(meta.Location)}
	for _, kv := range [][2]string{
		{"boundary", meta.Boundary},
		{"tool_name", meta.ToolName},
		{"tool_call_id", meta.ToolCallID},
		{"run_mode", meta.RunMode},
		{"task_id", meta.TaskID},
		{"conversation_id", meta.ConversationID},
	} {
		if kv[1] != "" {
			args = append(args, kv[0], LogSafe(kv[1]))
		}
	}
	args = append(args, "panic_class", class)
	logPanic(ctx, "panic recovered", args...)

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
			logPanic(context.Background(), "panic reporting hook panicked", "hook", name, "panic_class", PanicClass(r))
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

// RecoverContext is Recover for a goroutine that serves one turn, run or
// request: the panic record is emitted with ctx, so the configured handler adds
// that context's correlation (conv_id/turn_id, task_id/run_id …) to it. Use
// `defer safe.RecoverContext(ctx, "name", onPanic)`. recover() only stops a
// panic when called directly by the deferred function, so this repeats
// Recover's body rather than wrapping it.
func RecoverContext(ctx context.Context, name string, onPanic func(v any)) {
	if r := recover(); r != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		EmitPanicWithMetadataContext(ctx, PanicMetadata{Location: name}, r, nil)
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

// LogSafe strips CR/LF from a value before it is interpolated into a log line,
// so a value carrying a newline cannot forge a second log entry. It is the
// one copy every package uses (#1944). strings.ReplaceAll is the spelling
// CodeQL's go/log-injection query models as a sanitizer — strings.NewReplacer
// kept the same alerts open — so do not "simplify" the body.
func LogSafe(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
}
