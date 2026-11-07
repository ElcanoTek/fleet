package logging

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

// Startup buffering. fleet serve can only Configure logging once it has read
// its configuration — the env file, the bundle — and those steps (and the
// tracing and Sentry init right after them) already log. Lines written before
// Configure would otherwise go through Go's default handler: plaintext, never
// level-gated, and never in FLEET_LOG_FILE. BufferStartup holds them in
// memory instead; Configure replays them through the configured handler,
// which applies the format, the level and the file sink as if they had been
// written after it. FlushStartup writes them to stderr (JSON, info) when the
// process ends before Configure ran, so a boot failure is never silent.

// maxStartupRecords bounds the buffer; a boot that logs more than this before
// Configure keeps the first ones and counts the rest.
const maxStartupRecords = 2000

type startupBuffer struct {
	mu      sync.Mutex
	records []bufferedRecord
	dropped int
}

type bufferedRecord struct {
	ctx context.Context
	rec slog.Record
}

var (
	startupMu sync.Mutex
	startup   *startupBuffer
)

// BufferStartup makes slog's default logger hold records in memory until
// Configure (or FlushStartup) replays them. Call it first thing in a command
// that configures logging later.
func BufferStartup() {
	startupMu.Lock()
	defer startupMu.Unlock()
	if startup != nil {
		return
	}
	startup = &startupBuffer{}
	slog.SetDefault(slog.New(&bufferHandler{buf: startup}))
}

// FlushStartup writes any still-buffered startup records to stderr as JSON
// at info level, makes that handler the default, and stops buffering. It is a
// no-op once Configure has run. Defer it right after BufferStartup so an
// early return (a config error) still shows what was logged; Fatal calls it
// too.
func FlushStartup() {
	h := newJSONHandler(os.Stderr, slog.LevelInfo)
	if replayStartup(h) {
		slog.SetDefault(slog.New(h))
	}
}

// replayStartup hands every buffered record to h (subject to h's level) and
// stops buffering. It reports whether a buffer was active.
func replayStartup(h slog.Handler) bool {
	startupMu.Lock()
	buf := startup
	startup = nil
	startupMu.Unlock()
	if buf == nil {
		return false
	}
	buf.mu.Lock()
	records, dropped := buf.records, buf.dropped
	buf.records = nil
	buf.mu.Unlock()
	for _, br := range records {
		if h.Enabled(br.ctx, br.rec.Level) {
			_ = h.Handle(br.ctx, br.rec)
		}
	}
	if dropped > 0 {
		r := slog.NewRecord(time.Now(), slog.LevelWarn, "logging: startup log buffer overflowed; lines dropped", 0)
		r.AddAttrs(slog.Int("dropped", dropped))
		_ = h.Handle(context.Background(), r)
	}
	return true
}

// bufferHandler records every record (the level is decided at replay). A
// handler made by WithAttrs/WithGroup links to its parent with that one
// operation, so Handle can rebuild the exact attribute nesting.
type bufferHandler struct {
	buf    *startupBuffer
	parent *bufferHandler
	attrs  []slog.Attr // WithAttrs step
	group  string      // WithGroup step
}

func (h *bufferHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *bufferHandler) Handle(ctx context.Context, r slog.Record) error {
	var cur []slog.Attr
	r.Attrs(func(a slog.Attr) bool { cur = append(cur, a); return true })
	for n := h; n != nil; n = n.parent {
		switch {
		case n.group != "":
			cur = []slog.Attr{slog.Group(n.group, attrsToAny(cur)...)}
		case len(n.attrs) > 0:
			cur = append(slices.Clone(n.attrs), cur...)
		}
	}
	rec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	rec.AddAttrs(cur...)
	h.buf.mu.Lock()
	defer h.buf.mu.Unlock()
	if len(h.buf.records) >= maxStartupRecords {
		h.buf.dropped++
		return nil
	}
	h.buf.records = append(h.buf.records, bufferedRecord{ctx: context.WithoutCancel(ctx), rec: rec})
	return nil
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

func (h *bufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &bufferHandler{buf: h.buf, parent: h, attrs: slices.Clone(attrs)}
}

func (h *bufferHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &bufferHandler{buf: h.buf, parent: h, group: name}
}
