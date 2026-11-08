package safe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

type explodingWriter struct{}

func (explodingWriter) Write([]byte) (int, error) { panic("sink exploded") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// The configured handler gets recovered panics; when it panics or fails, the
// record still goes out through the direct-stderr fallback logger.
func TestPanicHandlerWithFallback(t *testing.T) {
	prevLogger := panicLogger
	t.Cleanup(func() { panicLogger = prevLogger; SetPanicHandler(nil) })

	var configured bytes.Buffer
	SetPanicHandler(slog.NewTextHandler(&configured, nil))
	var fallback bytes.Buffer
	panicLogger = slog.New(slog.NewJSONHandler(&fallback, nil))
	func() {
		defer Recover("unit.configured", nil)
		panic("x")
	}()
	if !strings.Contains(configured.String(), `msg="panic recovered"`) || fallback.Len() != 0 {
		t.Fatalf("configured=%q fallback=%q", configured.String(), fallback.String())
	}

	for name, w := range map[string]interface{ Write([]byte) (int, error) }{"panicking": explodingWriter{}, "erroring": failingWriter{}} {
		fallback.Reset()
		SetPanicHandler(slog.NewTextHandler(w, nil))
		func() {
			defer Recover("unit.fallback", nil)
			panic("x")
		}()
		if !strings.Contains(fallback.String(), `"msg":"panic recovered"`) {
			t.Fatalf("%s handler: fallback got %q", name, fallback.String())
		}
	}
}

type ctxKey struct{}

// ctxRecordingHandler records the context each record was handled with.
type ctxRecordingHandler struct{ got *[]any }

func (h ctxRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h ctxRecordingHandler) Handle(ctx context.Context, _ slog.Record) error {
	*h.got = append(*h.got, ctx.Value(ctxKey{}))
	return nil
}
func (h ctxRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h ctxRecordingHandler) WithGroup(string) slog.Handler      { return h }

// RecoverContext emits the panic record with the goroutine's context, so the
// configured handler can add that turn's or run's correlation (#2052), and
// still runs onPanic with the recovered value.
func TestRecoverContextEmitsWithItsContext(t *testing.T) {
	t.Cleanup(func() { SetPanicHandler(nil) })
	var got []any
	SetPanicHandler(ctxRecordingHandler{got: &got})

	var recovered any
	ctx := context.WithValue(context.Background(), ctxKey{}, "turn-42")
	func() {
		defer RecoverContext(ctx, "unit.recover-context", func(v any) { recovered = v })
		panic("boom")
	}()
	if recovered != "boom" {
		t.Fatalf("onPanic got %v, want boom", recovered)
	}
	if len(got) != 1 || got[0] != "turn-42" {
		t.Fatalf("panic record handled with context values %v, want [turn-42]", got)
	}

	got = nil
	func() {
		//nolint:staticcheck // SA1012: a nil context must not stop recovery
		defer RecoverContext(nil, "unit.recover-nil-context", nil)
		panic("boom")
	}()
	if len(got) != 1 {
		t.Fatalf("nil context: %d records, want 1", len(got))
	}
}
