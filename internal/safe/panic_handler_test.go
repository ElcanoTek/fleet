package safe

import (
	"bytes"
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
