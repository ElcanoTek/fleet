package httpmw_test

import (
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/otelsetup"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// A handler panic recovered inside the tracing middleware — where both
// routers now place their recoverer — is reported with the request's
// request_id, trace_id and span_id (#2074 review: the recoverer ran outside
// otelsetup.Middleware and emitted under a background context).
func TestRecoveredPanicCarriesTheRequestCorrelation(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	prevSlog := slog.Default()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(t.Context())
		slog.SetDefault(prevSlog)
		safe.SetPanicHandler(nil)
		log.SetOutput(os.Stderr)
	})
	path := t.TempDir() + "/fleet.log"
	closer, err := logging.Configure(logging.Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	h := httpmw.Recoverer(otelsetup.Middleware(httpmw.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	reqID := w.Header().Get("X-Request-Id")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "panic recovered" {
			if event != nil {
				t.Fatalf("the panic was reported twice:\n%s", data)
			}
			event = m
		}
	}
	if event == nil {
		t.Fatalf("no panic event:\n%s", data)
	}
	if reqID == "" || event["request_id"] != reqID || event["trace_id"] == nil || event["trace_id"] == "" || event["span_id"] == nil {
		t.Fatalf("panic event lacks the request correlation (X-Request-Id %q): %v", reqID, event)
	}
}
