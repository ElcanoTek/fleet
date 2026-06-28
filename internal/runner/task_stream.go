package runner

// Live SSE log streaming for in-progress scheduled tasks (#200).
//
// This is the scheduled-task analogue of internal/httpapi's turnBuffer: a
// per-task, in-memory event buffer that the HTTP layer's GET /tasks/{id}/stream
// handler can Attach to and tail while a task runs, with Last-Event-ID replay so
// a dropped browser EventSource reconnects without losing events. It deliberately
// mirrors turnBuffer's Emit/Attach/Finish (sans the Postgres persister — the
// authoritative captain's-log is still written to storage by submitLog at task
// completion exactly as before) so both code paths behave identically for the UI.
//
// The buffer satisfies agentcore.Observer, so the scheduled driver tees the run's
// real event stream into it via agentcore.WithStreamObserver — the SAME events the
// captain's-log writer consumes. No new event bus is invented and the interactive
// chat SSE is untouched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// taskStreamRetainTTL is how long a finished task's buffer stays in the registry
// after Finish — long enough for a client that dropped mid-run (or that only
// connects right as the task ends) to attach and replay the complete stream. It
// mirrors the chat path's bufferRetainTTL intent at the 2-minute window the issue
// specifies; the orchestrator falls back to the persisted log after this window.
const taskStreamRetainTTL = 2 * time.Minute

// taskStreamMaxBytes caps a single task's in-memory event buffer (sliding window;
// oldest events evicted past the cap) so a chatty long run can't grow unbounded.
// Matches the chat default (5 MiB) — generous for a run log, bounded for safety.
const taskStreamMaxBytes = 5 << 20

// taskStreamHeartbeat pings an idle subscriber's socket so a long, quiet scheduled
// run (30+ minutes between events) doesn't trip an intermediary's idle timeout.
const taskStreamHeartbeat = 15 * time.Second

// taskStreamSubBuffer is the per-subscriber channel depth. A subscriber that
// can't keep up is evicted and reattaches from its Last-Event-ID (see Emit).
const taskStreamSubBuffer = 256

// taskStreamEvent is one already-serialized SSE frame. Data is the marshalled
// JSON payload so fan-out doesn't reserialize per subscriber (mirrors
// httpapi.bufferedEvent).
type taskStreamEvent struct {
	ID   uint64
	Name string
	Data []byte
}

// taskStreamBuffer is the single source of truth for one running task's event
// stream. Emit appends + fans out under a lock; Attach atomically snapshots the
// replay slice and registers a live subscription under the same lock so no event
// is missed between replay and live; Finish seals it (Emit becomes a no-op and all
// subscribers see EOF). It is the scheduled analogue of httpapi.turnBuffer.
type taskStreamBuffer struct {
	mu          sync.Mutex
	events      []taskStreamEvent
	subscribers map[uint64]chan taskStreamEvent
	nextSubID   uint64
	closed      bool

	// maxBytes caps the cumulative size of buffered event Data (sliding window).
	// totalBytes tracks the running sum so eviction is O(evicted).
	maxBytes   int
	totalBytes int
}

// newTaskStreamBuffer builds an empty, open buffer.
func newTaskStreamBuffer() *taskStreamBuffer {
	return &taskStreamBuffer{
		subscribers: make(map[uint64]chan taskStreamEvent),
		maxBytes:    taskStreamMaxBytes,
	}
}

// Emit assigns a monotonic id, appends to the log, and non-blocking-sends to every
// live subscriber. A subscriber whose channel is full is evicted (it reattaches
// with Last-Event-ID and replays). Must NOT block — it runs on the agent's event
// path. After Finish it is a no-op.
func (b *taskStreamBuffer) Emit(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"marshal_error":%q}`, err.Error()))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	// IDs are monotonic across the run even after eviction: derive the next id from
	// the last event (not len(events), which shrinks when the window slides).
	var id uint64 = 1
	if n := len(b.events); n > 0 {
		id = b.events[n-1].ID + 1
	}
	ev := taskStreamEvent{ID: id, Name: event, Data: data}
	b.events = append(b.events, ev)
	b.totalBytes += len(data)

	// Sliding-window eviction: drop the oldest events once over the byte cap, always
	// keeping at least the newest so a reconnecting client still gets something.
	for b.maxBytes > 0 && b.totalBytes > b.maxBytes && len(b.events) > 1 {
		b.totalBytes -= len(b.events[0].Data)
		b.events = b.events[1:]
	}

	for subID, ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(b.subscribers, subID)
		}
	}
}

// Observe makes the buffer an agentcore.Observer so the scheduled run tees its
// real event stream here. It maps the run's internal event names onto the stable
// SSE event types the orchestrator UI tails (#200):
//
//   - text.delta  → agent_message  (an assistant output chunk)
//   - tool.call   → tool_call      (a tool invocation: name + raw JSON input)
//   - tool.result → tool_result    (a tool result: name + output + error flag)
//
// Reasoning / enforcement / context-pressure events are intentionally not
// forwarded to the live stream — they are loop internals, not run-log output the
// operator tails (the captain's-log writer still records the ones it persists).
// The lifecycle status events are emitted by the worker pool directly.
func (b *taskStreamBuffer) Observe(eventType string, payload map[string]any) {
	switch eventType {
	case "text.delta":
		text, _ := payload["text"].(string)
		if text == "" {
			return
		}
		b.Emit("agent_message", map[string]any{
			"type":    "agent_message",
			"role":    "assistant",
			"content": text,
		})
	case "tool.call":
		b.Emit("tool_call", map[string]any{
			"type":    "tool_call",
			"call_id": payload["id"],
			"name":    payload["name"],
			"input":   payload["input"],
		})
	case "tool.result":
		isErr, _ := payload["is_err"].(bool)
		b.Emit("tool_result", map[string]any{
			"type":    "tool_result",
			"call_id": payload["id"],
			"name":    payload["name"],
			"output":  payload["text"],
			"error":   isErr,
		})
	}
}

// Finish seals the buffer: Emit becomes a no-op and every live subscriber channel
// is closed so its Attach goroutine sees EOF. Safe to call more than once; it
// reports true only on the call that actually sealed an open buffer, so the
// registry schedules the retain-window cleanup exactly once.
func (b *taskStreamBuffer) Finish() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.closed = true
	for subID, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, subID)
	}
	return true
}

// Attach writes every event with ID > lastEventID to w in SSE framing, then
// streams subsequent events live until the buffer is sealed or ctx is cancelled
// (client disconnect). It sets the SSE headers + status line itself, so it must be
// called before any other write to w. Mirrors httpapi.turnBuffer.Attach.
func (b *taskStreamBuffer) Attach(ctx context.Context, lastEventID uint64, w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Atomically grab the replay slice and register a live subscription under the
	// same lock the publisher uses, so no event is both replayed AND delivered live
	// (nor missed between them).
	b.mu.Lock()
	var replay []taskStreamEvent
	for _, e := range b.events {
		if e.ID > lastEventID {
			replay = append(replay, e)
		}
	}
	var ch chan taskStreamEvent
	var subID uint64
	if !b.closed {
		b.nextSubID++
		subID = b.nextSubID
		ch = make(chan taskStreamEvent, taskStreamSubBuffer)
		b.subscribers[subID] = ch
	}
	b.mu.Unlock()

	for _, e := range replay {
		if err := writeTaskStreamFrame(w, flusher, e); err != nil {
			b.unsubscribe(subID)
			return err
		}
	}

	// Buffer already sealed when we attached: replay is all there is.
	if ch == nil {
		return nil
	}

	var hb *time.Ticker
	var heartbeatC <-chan time.Time
	if taskStreamHeartbeat > 0 {
		hb = time.NewTicker(taskStreamHeartbeat)
		defer hb.Stop()
		heartbeatC = hb.C
	}

	for {
		select {
		case <-ctx.Done():
			b.unsubscribe(subID)
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				// Sealed by Finish — our subscription was closed.
				return nil
			}
			if err := writeTaskStreamFrame(w, flusher, ev); err != nil {
				b.unsubscribe(subID)
				return err
			}
			if hb != nil {
				hb.Reset(taskStreamHeartbeat)
			}
		case <-heartbeatC:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				b.unsubscribe(subID)
				return err
			}
			flusher.Flush()
		}
	}
}

func (b *taskStreamBuffer) unsubscribe(id uint64) {
	if id == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
}

// writeTaskStreamFrame formats one event as an SSE frame and flushes. A write
// error (client disconnect) propagates so Attach can unsubscribe.
func writeTaskStreamFrame(w http.ResponseWriter, flusher http.Flusher, e taskStreamEvent) error {
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Name, string(e.Data)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// TaskStreamRegistry holds the live per-task stream buffers keyed by task UUID. A
// process has one (built by NewPool); the HTTP handler reaches it through an
// injected lookup so it can Attach a client to a running task. It is the
// scheduled analogue of httpapi.Server.inflight.
type TaskStreamRegistry struct {
	mu      sync.Mutex
	streams map[uuid.UUID]*taskStreamBuffer
}

// newTaskStreamRegistry builds an empty registry.
func newTaskStreamRegistry() *TaskStreamRegistry {
	return &TaskStreamRegistry{streams: make(map[uuid.UUID]*taskStreamBuffer)}
}

// register installs a fresh buffer for a task that is about to run and returns it.
func (r *TaskStreamRegistry) register(taskID uuid.UUID) *taskStreamBuffer {
	buf := newTaskStreamBuffer()
	r.mu.Lock()
	r.streams[taskID] = buf
	r.mu.Unlock()
	return buf
}

// release seals the buffer and schedules its removal after the retain window, so a
// late-joining client can still replay the finished run before falling back to the
// persisted log. Idempotent: only the call that actually sealed the buffer arms
// the cleanup timer, so a defer + explicit call don't double-schedule.
func (r *TaskStreamRegistry) release(taskID uuid.UUID, buf *taskStreamBuffer) {
	if !buf.Finish() {
		return
	}
	time.AfterFunc(taskStreamRetainTTL, func() {
		r.mu.Lock()
		// Delete only if still the SAME buffer — a re-run of the task ID after the
		// window would have registered a new one we must not evict.
		if cur, ok := r.streams[taskID]; ok && cur == buf {
			delete(r.streams, taskID)
		}
		r.mu.Unlock()
	})
}

// Lookup returns the live (or recently-finished, still-retained) stream buffer for
// a task, or false when none exists. The HTTP handler uses it to decide between an
// SSE attach (live) and a one-shot replay of the persisted log (absent).
func (r *TaskStreamRegistry) Lookup(taskID uuid.UUID) (TaskStream, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf, ok := r.streams[taskID]
	if !ok {
		return nil, false
	}
	return buf, true
}

// TaskStream is the narrow read surface the HTTP handler needs from a buffer: it
// attaches a client to the live SSE stream with Last-Event-ID replay. Kept minimal
// so the handler doesn't depend on the buffer's internals.
type TaskStream interface {
	Attach(ctx context.Context, lastEventID uint64, w http.ResponseWriter) error
}

// Compile-time assertions: a buffer is both an Observer (the run tees into it) and
// a TaskStream (the handler attaches to it).
var (
	_ agentcore.Observer = (*taskStreamBuffer)(nil)
	_ TaskStream         = (*taskStreamBuffer)(nil)
)
