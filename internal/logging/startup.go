package logging

import (
	"context"
	"log"
	"log/slog"
	"slices"
	"strings"
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
	// sealed is set once the buffer has been replayed: a record that still
	// arrives (a goroutine holding the buffering logger or the stdlib bridge
	// across Configure) goes straight to the replay's handlers, never into a
	// buffer nobody reads again.
	sealed *replayTarget
}

// replayTarget is where a replayed (or late) record goes: an ordinary record
// to h, subject to its level; a bridged stdlib line to bridge, ungated.
type replayTarget struct{ h, bridge slog.Handler }

func (t *replayTarget) handle(br bufferedRecord) {
	switch {
	case br.bridged:
		_ = t.bridge.Handle(br.ctx, br.rec)
	case t.h.Enabled(br.ctx, br.rec.Level):
		_ = t.h.Handle(br.ctx, br.rec)
	}
}

type bufferedRecord struct {
	ctx context.Context
	rec slog.Record
	// bridged marks a line a third-party library wrote through the stdlib
	// log package: replayed through the ungated bridge handler, as it would
	// have been after Configure, not gated by FLEET_LOG_LEVEL as an info.
	bridged bool
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
	// After slog.SetDefault, so the stdlib log package writes here rather
	// than into the buffer as an ordinary info record.
	log.SetFlags(0)
	log.SetOutput(startupLogBridge{buf: startup})
}

// startupLogBridge buffers a stdlib log line before Configure, tagged as
// bridged (bufferedRecord.bridged).
type startupLogBridge struct{ buf *startupBuffer }

func (b startupLogBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", " "), "\n", " ")
	b.buf.add(bufferedRecord{ctx: context.Background(), rec: slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0), bridged: true})
	return len(p), nil
}

// add appends one record, counting it as dropped past maxStartupRecords —
// or, once the buffer is sealed, hands it to the replay's handlers.
func (b *startupBuffer) add(r bufferedRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed != nil {
		b.sealed.handle(r)
		return
	}
	if len(b.records) >= maxStartupRecords {
		b.dropped++
		return
	}
	b.records = append(b.records, r)
}

// FlushStartup writes any still-buffered startup records to stderr as JSON
// at info level, makes that handler the default, and stops buffering. It is a
// no-op once Configure has run. Defer it right after BufferStartup so an
// early return (a config error) still shows what was logged; Fatal calls it
// too.
func FlushStartup() {
	h := emergencyHandler()
	if replayStartup(h, newBridgeHandler("json", lockedStderr)) {
		slog.SetDefault(slog.New(h))
	}
}

// emergencyHandler is the handler for output before Configure (FlushStartup,
// Fatal): redacting JSON on the locked stderr at info.
func emergencyHandler() slog.Handler {
	return newJSONHandler(lockedStderr, slog.LevelInfo)
}

// replayStartup hands every buffered record to h (subject to h's level) —
// a bridged stdlib log line to bridge instead, ungated, as after Configure —
// and stops buffering. It reports whether a buffer was active.
//
// It seeds the redactor with the process's secret-named env values first:
// the buffered records are redacted only now, as h handles them, and by now
// the env file has been applied — whichever path replays (Configure, the
// FlushStartup emergency flush, Fatal), no record goes out before the
// redactor knows the process's own secrets.
func replayStartup(h, bridge slog.Handler) bool {
	seedEnvLiterals()
	startupMu.Lock()
	buf := startup
	startup = nil
	startupMu.Unlock()
	if buf == nil {
		return false
	}
	// Replay under the buffer's lock and seal it before letting go: a record
	// added concurrently (a stdlib log.Printf racing Configure's switch of
	// log.SetOutput) waits, then goes straight to h or bridge after the
	// replayed ones — never into the buffer after its last read (#2074
	// review).
	target := &replayTarget{h: h, bridge: bridge}
	buf.mu.Lock()
	records, dropped := buf.records, buf.dropped
	buf.records = nil
	for _, br := range records {
		target.handle(br)
	}
	buf.sealed = target
	buf.mu.Unlock()
	if dropped > 0 && h.Enabled(context.Background(), slog.LevelWarn) {
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
	// A record marked ungated (ungatedAttr: a subprocess's stderr line) is
	// buffered as a bridged line: replayed through the ungated bridge, which
	// adds the marker back, not gated by the configured level.
	bridged := false
	cur = slices.DeleteFunc(cur, func(a slog.Attr) bool {
		if isUngatedAttr(a) {
			bridged = true
			return true
		}
		return false
	})
	rec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	rec.AddAttrs(cur...)
	h.buf.add(bufferedRecord{ctx: context.WithoutCancel(ctx), rec: rec, bridged: bridged})
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
