package dbtrace_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ElcanoTek/fleet/internal/dbtrace"
)

// Every transaction through a dbtrace pool is one db.tx span under the
// caller's span, named after the function that opened it and ended by its
// commit or rollback — with no query text on it.
func TestTransactionsAreSpans(t *testing.T) {
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL unset")
	}
	prev := otel.GetTracerProvider()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })

	db, err := dbtrace.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, parent := otel.Tracer("test").Start(context.Background(), "store.op")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT 'QUERY-TEXT-1'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	parent.End()

	var outcomes []string
	for _, s := range sr.Ended() {
		if !strings.HasPrefix(s.Name(), "db.tx") {
			continue
		}
		if !strings.Contains(s.Name(), "TestTransactionsAreSpans") {
			t.Errorf("span name %q should name the caller", s.Name())
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("db.tx is not a child of the caller's span")
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "QUERY-TEXT") {
				t.Errorf("attribute %s carries query text", kv.Key)
			}
			if kv.Key == "fleet.db.tx.outcome" {
				outcomes = append(outcomes, kv.Value.AsString())
			}
		}
	}
	if strings.Join(outcomes, ",") != "commit,rollback" {
		t.Errorf("outcomes = %v, want commit then rollback", outcomes)
	}
}
