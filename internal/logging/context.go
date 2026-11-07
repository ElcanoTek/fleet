package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// The standard correlation attribute keys. Every log line that concerns a
// conversation, turn, task, run, user or HTTP request carries the matching key
// under exactly this name, so one query (`conv_id="…"`, `trace_id="…"`) finds
// every line about it regardless of which package wrote it.
const (
	KeyConvID    = "conv_id"
	KeyTurnID    = "turn_id"
	KeyTaskID    = "task_id"
	KeyRunID     = "run_id"
	KeyUserID    = "user_id"
	KeyRequestID = "request_id"
	KeyTraceID   = "trace_id"
	KeySpanID    = "span_id"
)

// ctxAttrsKey is the context key under which the correlation attributes live.
type ctxAttrsKey struct{}

// ctxAttrs is the immutable attribute list stored on a context. A With* call
// copies it, so a child context never mutates its parent's set.
type ctxAttrs []slog.Attr

// WithAttrs returns a child of ctx whose context logger (From) also carries
// attrs. An attribute whose key is already present replaces the earlier value,
// so re-attaching (e.g. the turn id of a resumed turn) never duplicates a key.
// Empty-string values are skipped: an unset id is absent, not "". trace_id and
// span_id are skipped too — they always come from the context's span, so
// WithAttrs(ctx, Attrs(other)...) copies another context's ids safely.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	prev, _ := ctx.Value(ctxAttrsKey{}).(ctxAttrs)
	next := make(ctxAttrs, len(prev), len(prev)+len(attrs))
	copy(next, prev)
	changed := false
	for _, a := range attrs {
		if a.Key == "" || (a.Value.Kind() == slog.KindString && a.Value.String() == "") {
			continue
		}
		if a.Key == KeyTraceID || a.Key == KeySpanID {
			// Always derived from the live span (Attrs), never stored: a
			// stored copy would go stale the moment a child span starts.
			continue
		}
		changed = true
		replaced := false
		for i := range next {
			if next[i].Key == a.Key {
				next[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			next = append(next, a)
		}
	}
	if !changed {
		return ctx
	}
	return context.WithValue(ctx, ctxAttrsKey{}, next)
}

// WithConversation attaches the conversation id (conv_id).
func WithConversation(ctx context.Context, convID string) context.Context {
	return WithAttrs(ctx, slog.String(KeyConvID, convID))
}

// WithTurn attaches the conversation and turn ids (conv_id, turn_id) — the
// boundary call at turn start.
func WithTurn(ctx context.Context, convID, turnID string) context.Context {
	return WithAttrs(ctx, slog.String(KeyConvID, convID), slog.String(KeyTurnID, turnID))
}

// WithTask attaches the scheduled task and run ids (task_id, run_id) — the
// boundary call when a task run starts. runID may be empty.
func WithTask(ctx context.Context, taskID, runID string) context.Context {
	return WithAttrs(ctx, slog.String(KeyTaskID, taskID), slog.String(KeyRunID, runID))
}

// WithUser attaches the acting principal (user_id): the signed-in user's
// email for a browser request, the API key's owner for an API call.
func WithUser(ctx context.Context, userID string) context.Context {
	return WithAttrs(ctx, slog.String(KeyUserID, userID))
}

// WithRequestID attaches the HTTP request id (request_id) — the value the
// tracing middleware also returns as X-Request-Id.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return WithAttrs(ctx, slog.String(KeyRequestID, requestID))
}

// Attrs returns the correlation attributes ctx carries, followed by trace_id
// and span_id when ctx holds a valid span context (a recording span, or an
// inbound traceparent this process continues). The slice is a fresh copy.
func Attrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	stored, _ := ctx.Value(ctxAttrsKey{}).(ctxAttrs)
	sc := trace.SpanContextFromContext(ctx)
	n := len(stored)
	if sc.IsValid() {
		n += 2
	}
	if n == 0 {
		return nil
	}
	out := make([]slog.Attr, 0, n)
	out = append(out, stored...)
	if sc.IsValid() {
		out = append(out,
			slog.String(KeyTraceID, sc.TraceID().String()),
			slog.String(KeySpanID, sc.SpanID().String()))
	}
	return out
}

// Value returns the string value of one correlation key on ctx ("" if absent).
// trace_id and span_id are read from the active span context.
func Value(ctx context.Context, key string) string {
	for _, a := range Attrs(ctx) {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

// From returns the process default logger carrying ctx's correlation
// attributes (see Attrs). It is the logger every call site with a context
// uses. With no attributes on ctx it is slog.Default() itself, so the call
// costs one context lookup when there is nothing to add.
//
// The default logger is read at call time, so a logger obtained before
// Configure still writes in the configured format afterwards only if it is
// fetched again — call From at the log site, do not cache it across boot.
func From(ctx context.Context) *slog.Logger {
	l := slog.Default()
	attrs := Attrs(ctx)
	if len(attrs) == 0 {
		return l
	}
	return slog.New(ctxHandler{inner: l.Handler(), attrs: attrs})
}

// ctxHandler prepends the context's correlation attributes to each record,
// skipping any key the call site passed itself — so a line that logs
// "task_id" explicitly (a handler naming the task it acted on) does not also
// carry the context's copy as a duplicate JSON key. The call site wins.
type ctxHandler struct {
	inner slog.Handler
	attrs []slog.Attr
}

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	var own map[string]struct{}
	if r.NumAttrs() > 0 {
		own = make(map[string]struct{}, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			own[a.Key] = struct{}{}
			return true
		})
	}
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range h.attrs {
		if _, dup := own[a.Key]; !dup {
			nr.AddAttrs(a)
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Bound attributes win over the context's too: drop the shadowed keys.
	kept := make([]slog.Attr, 0, len(h.attrs))
	for _, c := range h.attrs {
		shadowed := false
		for _, a := range attrs {
			if a.Key == c.Key {
				shadowed = true
				break
			}
		}
		if !shadowed {
			kept = append(kept, c)
		}
	}
	return ctxHandler{inner: h.inner.WithAttrs(attrs), attrs: kept}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	// Inside a group the context attributes would be nested under it; bind
	// them at the top level first so they keep their standard keys.
	return h.inner.WithAttrs(h.attrs).WithGroup(name)
}
