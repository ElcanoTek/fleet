package chattui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event is one parsed SSE frame from POST /chat. Name is the `event:` field
// (conversation, turn.started, reasoning.delta, text.delta, tool.call,
// tool.result, turn.completed, …); Data is the decoded JSON `data:` object.
type Event struct {
	ID   string
	Name string
	Data map[string]any
}

// Str returns Data[key] as a string ("" when absent/non-string).
func (e Event) Str(key string) string {
	if v, ok := e.Data[key].(string); ok {
		return v
	}
	return ""
}

// Client streams turns from a running fleet server's POST /chat.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient builds a Client. The HTTP client has NO overall timeout — a turn can
// legitimately stream for minutes; cancellation is via the request context
// (Ctrl+C / a new turn), and a per-attempt dial/idle bound lives in the transport.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 0},
	}
}

// turnRequest is the subset of the server's chatRequest the TUI sends.
type turnRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
	Model          string `json:"model,omitempty"`
	Persona        string `json:"persona,omitempty"`
}

// Stream POSTs a turn and invokes onEvent for every SSE frame until the stream
// ends, the turn completes, or ctx is cancelled. It returns the (possibly new)
// conversation id observed on the `conversation` event so the caller can keep
// the thread going. A non-2xx response is returned as an error carrying the
// status + a short body excerpt (never the token).
func (c *Client) Stream(ctx context.Context, message, convID string, onEvent func(Event)) (string, error) {
	body, err := json.Marshal(turnRequest{
		Message:        message,
		ConversationID: convID,
		Model:          c.cfg.Model,
		Persona:        c.cfg.Persona,
	})
	if err != nil {
		return convID, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.ServerURL+"/chat", bytes.NewReader(body))
	if err != nil {
		return convID, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Chat-Server-Token", c.cfg.Token)
	req.Header.Set("X-User-Email", c.cfg.Email)
	req.Header.Set("X-Fleet-Client", "fleet-chat")

	resp, err := c.http.Do(req)
	if err != nil {
		return convID, fmt.Errorf("connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(excerpt))
		switch resp.StatusCode {
		case http.StatusForbidden:
			return convID, fmt.Errorf("server rejected the request (403): check FLEET_SERVER_TOKEN matches the server")
		case http.StatusUnauthorized, http.StatusBadRequest:
			return convID, fmt.Errorf("not authorized (%d) for %s: %s", resp.StatusCode, c.cfg.Email, msg)
		default:
			return convID, fmt.Errorf("server returned %d: %s", resp.StatusCode, msg)
		}
	}

	newConvID := convID
	perr := parseSSE(resp.Body, func(ev Event) {
		if ev.Name == "conversation" {
			if id := ev.Str("id"); id != "" {
				newConvID = id
			}
		}
		onEvent(ev)
	})
	if perr != nil && ctx.Err() == nil {
		return newConvID, perr
	}
	return newConvID, nil
}

// parseSSE reads a text/event-stream and calls fn for each complete frame. It
// handles multi-line `data:` (joined with "\n"), `id:`, and `event:` (default
// "message"), and ignores comments (`:`-prefixed heartbeats). A frame whose data
// is not valid JSON is delivered with a nil Data map (still useful for its Name).
func parseSSE(r io.Reader, fn func(Event)) error {
	sc := bufio.NewScanner(r)
	// Allow long frames (a big tool result or text block in one data line).
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var id, name string
	var data strings.Builder
	flush := func() {
		if name == "" && data.Len() == 0 {
			return
		}
		ev := Event{ID: id, Name: name}
		if ev.Name == "" {
			ev.Name = "message"
		}
		if data.Len() > 0 {
			var m map[string]any
			if json.Unmarshal([]byte(data.String()), &m) == nil {
				ev.Data = m
			}
		}
		fn(ev)
		id, name = "", ""
		data.Reset()
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // frame boundary
			flush()
		case strings.HasPrefix(line, ":"): // comment/heartbeat
			continue
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush() // a final frame not terminated by a blank line
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

// Ping reports whether the server's /healthz answers quickly — a fast, friendly
// preflight so `fleet chat` can say "server not reachable" instead of hanging on
// the first turn. Best-effort; never returns the token.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ServerURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fleet server not reachable at %s (is it running? `fleet status`): %w", c.cfg.ServerURL, err)
	}
	_ = resp.Body.Close()
	return nil
}
