// Package dbtrace opens fleet's Postgres pools with one span per database
// transaction (#2052, docs/TELEMETRY.md).
//
// The two stores (internal/store, the chat DB; internal/sched/db, the
// scheduler DB) each run dozens of transactions through *sql.DB.BeginTx.
// Rather than wrap every call site, the transaction SEAM is the driver: Open
// builds the pool on pgx's database/sql driver with each connection wrapped so
// BeginTx opens a db.tx span and the transaction's Commit or Rollback ends it.
// Every existing `tx, err := s.db.BeginTx(ctx, nil)` is therefore traced, under
// the caller's span, with no change at the call site.
//
// Ids and sizes only, like every fleet span: the database system, the
// transaction's isolation level and read-only flag, how it ended
// (commit/rollback), and the Go function that opened it. Never a query, a
// parameter or a row.
//
// Disabled tracing costs one no-op span start per transaction; the caller
// lookup runs only for a recording span.
package dbtrace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ElcanoTek/fleet/internal/telemetry"
)

const (
	attrDBSystem   = attribute.Key("db.system.name")
	attrDBOutcome  = attribute.Key("fleet.db.tx.outcome")
	attrDBIsoLevel = attribute.Key("fleet.db.tx.isolation")
	attrDBReadOnly = attribute.Key("fleet.db.tx.read_only")
	attrCodeFunc   = attribute.Key("code.function.name")
)

// Open opens a database/sql pool on pgx's driver for dsn, with transaction
// tracing. Like sql.Open it does not connect; the caller pings.
func Open(dsn string) (*sql.DB, error) {
	connector, err := stdlib.GetDefaultDriver().(driver.DriverContext).OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(tracedConnector{Connector: connector}), nil
}

// tracedConnector wraps each new connection.
type tracedConnector struct{ driver.Connector }

func (c tracedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pc, ok := conn.(*stdlib.Conn)
	if !ok {
		return conn, nil
	}
	return &tracedConn{Conn: pc}, nil
}

// tracedConn embeds pgx's *stdlib.Conn, so every optional driver interface it
// implements (ExecerContext, QueryerContext, Pinger, SessionResetter,
// Validator, NamedValueChecker, …) is promoted unchanged; only BeginTx is
// intercepted.
type tracedConn struct{ *stdlib.Conn }

func (c *tracedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	ctx, span := telemetry.Start(ctx, "db.tx", trace.WithSpanKind(trace.SpanKindClient))
	if span.IsRecording() {
		fn := callerFunction()
		if fn != "" {
			span.SetName("db.tx " + shortFunc(fn))
		}
		span.SetAttributes(
			attrDBSystem.String("postgresql"),
			attrDBIsoLevel.String(fmt.Sprint(sql.IsolationLevel(opts.Isolation))),
			attrDBReadOnly.Bool(opts.ReadOnly),
			attrCodeFunc.String(fn),
		)
	}
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		telemetry.End(span, err)
		return nil, err
	}
	return &tracedTx{Tx: tx, span: span}, nil
}

// tracedTx ends the transaction's span when it commits or rolls back.
type tracedTx struct {
	driver.Tx
	span trace.Span
}

func (t *tracedTx) Commit() error {
	err := t.Tx.Commit()
	t.end("commit", err)
	return err
}

func (t *tracedTx) Rollback() error {
	err := t.Tx.Rollback()
	t.end("rollback", err)
	return err
}

func (t *tracedTx) end(outcome string, err error) {
	if t.span.IsRecording() {
		t.span.SetAttributes(attrDBOutcome.String(outcome))
	}
	telemetry.End(t.span, err)
}

// callerFunction is the first function on the stack outside database/sql and
// this package — the store method that called BeginTx.
func callerFunction() string {
	var pcs [24]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function != "" &&
			!strings.HasPrefix(f.Function, "database/sql.") &&
			!strings.HasPrefix(f.Function, "github.com/ElcanoTek/fleet/internal/dbtrace.") {
			return f.Function
		}
		if !more {
			return ""
		}
	}
}

// shortFunc trims a function's import path: ".../internal/store.(*Store).Claim"
// → "store.(*Store).Claim".
func shortFunc(fn string) string {
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		return fn[i+1:]
	}
	return fn
}
