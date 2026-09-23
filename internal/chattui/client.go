package chattui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event is one parsed SSE frame from POST /chat. Name is the `event:` field
// (conversation, turn.started, reasoning.delta, text.delta, text.replace,
// tool.call, tool.result, turn.completed, …); Data is the decoded JSON
// `data:` object.
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

	// defaultModel is the workspace default slug adopted from GET /client-config
	// when the operator passed no --model. It is sent ONLY on turns that start a
	// new conversation (empty convID): resuming an existing thread must leave the
	// conversation's stored model untouched (an empty request model means "no
	// opinion, keep what's stored" server-side).
	defaultModel             string
	conversationModels       map[string]string
	serverModelConversations map[string]bool
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

// AdoptDefaultModel records the workspace default slug (see defaultModel).
func (c *Client) AdoptDefaultModel(slug string) { c.defaultModel = strings.TrimSpace(slug) }

// EffectiveModel reports what the next NEW conversation would run on: the
// explicit --model//model when set, else the adopted workspace default.
func (c *Client) EffectiveModel() string {
	if strings.TrimSpace(c.cfg.Model) != "" {
		return c.cfg.Model
	}
	return c.defaultModel
}

// turnModel sends explicit operator overrides, never cached server selections.
// Accepting a suggestion hands this conversation back to its stored pin until
// /model explicitly overrides it again. Workspace defaults apply to new threads.
func (c *Client) turnModel(convID string) string {
	if convID != "" && c.serverModelConversations[convID] {
		return ""
	}
	if strings.TrimSpace(c.cfg.Model) != "" {
		return c.cfg.Model
	}
	if strings.TrimSpace(convID) == "" {
		return c.defaultModel
	}
	return ""
}

func (c *Client) displayModel(convID string) string {
	if model := c.conversationModels[convID]; convID != "" && model != "" {
		return model
	}
	return c.turnModel(convID)
}

// setAuthHeaders applies the shared-secret + identity headers every chattui
// request carries. The token is a header, never a URL/query value, so it cannot
// land in access logs.
func (c *Client) setAuthHeaders(req *http.Request) {
	req.Header.Set("X-Chat-Server-Token", c.cfg.Token)
	req.Header.Set("X-User-Email", c.cfg.Email)
	req.Header.Set("X-Fleet-Client", orDefault(c.cfg.ClientName, "fleet-chat"))
}

// turnRequest is the subset of the server's chatRequest the TUI sends.
type turnRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
	Model          string `json:"model,omitempty"`
	Persona        string `json:"persona,omitempty"`
	// InputID is the server's idempotency key (#785): a re-POST of the same
	// id is answered with the input already accepted instead of a new one.
	InputID string `json:"input_id,omitempty"`
}

// QueuedError is POST /chat's queue acknowledgement: the conversation already
// had a running turn (typically started from another surface), so the server
// durably QUEUED this message to run after it (#785) instead of streaming a
// turn. It is not a failure — the message will run — but this call has no
// stream to follow.
type QueuedError struct {
	ConversationID string
	InputID        string
	Position       int
	// Mode and State are the input row's: a replay of an input_id the
	// server already accepted reports where that input is now — still
	// queued, running, completed, or cancelled (nothing ran). Mode "direct"
	// is a submission that started its turn directly (not a queue item).
	Mode  string
	State string
}

// Replayed reports whether this acknowledgement is for an input the server had
// already accepted under the same key, rather than one it just queued.
func (e *QueuedError) Replayed() bool {
	return e.Mode == "direct" || (e.State != "" && e.State != "queued")
}

func (e *QueuedError) Error() string {
	switch e.State {
	case "running", "injected":
		return "this message is already running (it was accepted earlier)"
	case "completed":
		return "this message already ran (it was accepted earlier)"
	case "cancelled":
		return "this message was accepted earlier but did not run"
	}
	return fmt.Sprintf("a turn is already running in this conversation, so the message was queued (position %d) and will run after it", e.Position)
}

// StatusError is a non-2xx answer to POST /chat. Code lets a caller tell an
// auth failure (401/403) from any other refusal without matching on text; the
// message never carries the token.
type StatusError struct {
	Code int
	msg  string
}

func (e *StatusError) Error() string { return e.msg }

// Stream POSTs a turn and invokes onEvent for every SSE frame until the stream
// ends, the turn completes, or ctx is cancelled. It returns the (possibly new)
// conversation id observed on the `conversation` event so the caller can keep
// the thread going. A non-2xx response is returned as an error carrying the
// status + a short body excerpt (never the token).
func (c *Client) Stream(ctx context.Context, message, convID string, onEvent func(Event)) (string, error) {
	return c.StreamInput(ctx, message, convID, "", onEvent)
}

// StreamInput is Stream with an idempotency key (inputID, "" = none): a
// retried POST carrying the same id cannot start or queue the message twice.
func (c *Client) StreamInput(ctx context.Context, message, convID, inputID string, onEvent func(Event)) (string, error) {
	body, err := json.Marshal(turnRequest{
		Message:        message,
		ConversationID: convID,
		Model:          c.turnModel(convID),
		Persona:        c.cfg.Persona,
		InputID:        inputID,
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
	c.setAuthHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return convID, fmt.Errorf("connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()

	// A queue acknowledgement is JSON, not a stream: 202 for a newly queued
	// message, 200 for an idempotent replay of one already accepted.
	if resp.StatusCode == http.StatusAccepted ||
		(resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")) {
		var ack struct {
			Queued         bool   `json:"queued"`
			ConversationID string `json:"conversation_id"`
			Input          struct {
				ID       string `json:"id"`
				Position int    `json:"position"`
				Mode     string `json:"mode"`
				State    string `json:"state"`
			} `json:"input"`
		}
		derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ack)
		if derr == nil && ack.Queued {
			id := orDefault(ack.ConversationID, convID)
			return id, &QueuedError{ConversationID: id, InputID: ack.Input.ID, Position: ack.Input.Position, Mode: ack.Input.Mode, State: ack.Input.State}
		}
		// An unreadable acknowledgement (the connection closed mid-body) is not
		// a refusal: fleet may well have queued the message. Report it as a
		// transport failure — an unknown outcome — never as a definite status.
		return convID, fmt.Errorf("server accepted the request (%d) but its acknowledgement was unreadable: %v", resp.StatusCode, orDefault(errString(derr), "not a queue acknowledgement"))
	}
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(excerpt))
		switch resp.StatusCode {
		case http.StatusForbidden:
			return convID, &StatusError{Code: resp.StatusCode, msg: "server rejected the request (403): check FLEET_SERVER_TOKEN matches the server"}
		case http.StatusUnauthorized, http.StatusBadRequest:
			return convID, &StatusError{Code: resp.StatusCode, msg: fmt.Sprintf("not authorized (%d) for %s: %s", resp.StatusCode, c.cfg.Email, msg)}
		default:
			return convID, &StatusError{Code: resp.StatusCode, msg: fmt.Sprintf("server returned %d: %s", resp.StatusCode, msg)}
		}
	}

	newConvID := convID
	// The server names a new conversation on the response headers before any
	// frame (#1591), so a stream that dies before the `conversation` frame
	// still leaves the caller holding the id of the turn it started. Surface it
	// as a `conversation` event too, so callers that track the id from events
	// (the TUI, `fleet acp`) learn it at the same moment.
	if hdr := strings.TrimSpace(resp.Header.Get("X-Fleet-Conversation-Id")); hdr != "" && convID == "" {
		newConvID = hdr
		onEvent(Event{Name: "conversation", Data: map[string]any{"id": hdr}})
	}
	// The turn is named on the headers too; surfaced as a synthetic
	// `turn.identified` event (renderers ignore it) for callers that must
	// address this exact turn later, such as a targeted Stop.
	if hdr := strings.TrimSpace(resp.Header.Get("X-Fleet-Turn-Id")); hdr != "" {
		onEvent(Event{Name: "turn.identified", Data: map[string]any{"turn_id": hdr}})
	}
	terminalSeen := false
	var terminalErr error
	perr := parseSSE(resp.Body, func(ev Event) {
		if ev.Name == "conversation" {
			if id := ev.Str("id"); id != "" {
				newConvID = id
			}
		}
		switch ev.Name {
		case "turn.completed":
			terminalSeen = true
		case "turn.cancelled":
			terminalSeen = true
			terminalErr = context.Canceled
		case "turn.error":
			terminalSeen = true
			terminalErr = fmt.Errorf("turn failed: %s", orDefault(ev.Str("message"), "the server reported an error"))
		case "turn.model_required":
			terminalSeen = true
			terminalErr = fmt.Errorf("turn requires another model: %s", orDefault(ev.Str("message"), "select a different model and retry"))
		}
		onEvent(ev)
	})
	// A terminal event is authoritative: turn.completed is emitted only after
	// the canonical history commit, so a transport close racing just afterward
	// must not downgrade that committed outcome to cancellation or read failure.
	if terminalErr != nil {
		return newConvID, terminalErr
	}
	if terminalSeen {
		return newConvID, nil
	}
	if err := ctx.Err(); err != nil {
		return newConvID, err
	}
	if perr != nil {
		return newConvID, perr
	}
	return newConvID, fmt.Errorf("stream ended before a terminal turn event")
}

// attachFrozenArgsRaw replaces frozen_args with the exact JSON bytes so
// json.Number decoding can preserve integers above 2^53. Other event fields
// keep the default float64 mapping.
func attachFrozenArgsRaw(m map[string]any, raw []byte) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	fa, ok := envelope["frozen_args"]
	if !ok || len(fa) == 0 || string(fa) == "null" {
		return
	}
	m["frozen_args"] = fa
}

// parseSSE reads a text/event-stream and calls fn for each complete frame. It
// handles multi-line `data:` (joined with "\n"), `id:`, and `event:` (default
// "message"), and ignores comments (`:`-prefixed heartbeats). A frame whose data
// is not valid JSON is delivered with a nil Data map (still useful for its Name).
func parseSSE(r io.Reader, fn func(Event)) error {
	sc := bufio.NewScanner(r)
	// Allow long frames (a big tool result or text block in one data line).
	// A 1 MiB frozen object, its summary and handler-only pattern arguments can
	// each expand sixfold under JSON escaping. Budget all three plus metadata.
	sc.Buffer(make([]byte, 0, 64*1024), 24*1024*1024)

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
			raw := []byte(data.String())
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				attachFrozenArgsRaw(m, raw)
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

// DefaultModel fetches the workspace's advertised default model slug from
// GET /client-config — the same endpoint the web model picker reads — so a
// `fleet chat` with no --model lands on the same model a new web chat would
// instead of dying on the server's "frontend must send a model" rejection
// (#provider-aware model selection makes an empty slug a hard error).
func (c *Client) DefaultModel(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ServerURL+"/client-config", nil)
	if err != nil {
		return "", err
	}
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client-config returned %d", resp.StatusCode)
	}
	var body struct {
		Models struct {
			DefaultModel string `json:"default_model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode client-config: %w", err)
	}
	return strings.TrimSpace(body.Models.DefaultModel), nil
}

// ResolveApproval approves (or denies) a staged approval card: POST
// /conversations/{convID}/approvals/{approvalID} with {"approved": bool} — the
// exact call the web approval card's Send/Cancel buttons make. On approve the
// server runs the staged tool and returns its outcome; the returned strings are
// the resolution status ("approved"/"rejected") and the tool's result text.
func (c *Client) ResolveApproval(ctx context.Context, convID, approvalID string, approved bool) (string, string, error) {
	status, result, _, err := c.ResolveApprovalWithOptions(ctx, convID, approvalID, ApprovalDecision{Approved: approved})
	return status, result, err
}

// ApprovalDecision uses the existing governed web approval endpoint.
type ApprovalDecision struct {
	Approved bool           `json:"approved"`
	Scope    string         `json:"scope,omitempty"`
	Pattern  string         `json:"pattern,omitempty"`
	Edits    *ScheduleEdits `json:"edits,omitempty"`
}

type ScheduleEdits struct {
	Name   *string `json:"name,omitempty"`
	Prompt *string `json:"prompt,omitempty"`
	Cron   *string `json:"cron,omitempty"`
}

type approvalRunningError struct{}

func (approvalRunningError) Error() string {
	return "approval execution is still running; retry to retrieve its outcome"
}

func (c *Client) ResolveApprovalWithOptions(ctx context.Context, convID, approvalID string, decision ApprovalDecision) (string, string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	body, err := json.Marshal(decision)
	if err != nil {
		return "", "", "", err
	}
	url := c.cfg.ServerURL + "/conversations/" + url.PathEscape(convID) + "/approvals/" + url.PathEscape(approvalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", "", fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	var out struct {
		Status           string `json:"status"`
		ResultText       string `json:"result_text"`
		Model            string `json:"model"`
		IsErr            bool   `json:"is_err"`
		Executing        bool   `json:"executing"`
		ExecutionUnknown bool   `json:"execution_unknown"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return "", "", "", fmt.Errorf("decode approval response: %w", err)
	}
	if out.Executing {
		// Consent has been claimed, but the detached server action has not
		// finished. Retain the card for an idempotent status retry, not success.
		return "", out.ResultText, "", approvalRunningError{}
	}
	if out.ExecutionUnknown {
		return out.Status, out.ResultText, "", fmt.Errorf("approval was recorded, but its execution outcome is unavailable")
	}
	if out.IsErr || (decision.Approved && out.Status != "approved") || (!decision.Approved && out.Status != "rejected") {
		return out.Status, out.ResultText, out.Model, fmt.Errorf("approval resolved as %q: %s", out.Status, out.ResultText)
	}
	return out.Status, out.ResultText, out.Model, nil
}

// Cancel stops the conversation's in-flight turn server-side: POST
// /conversations/{convID}/cancel with scope "turn" — the web Stop button's
// call, narrowed so follow-ups already queued on the conversation still run.
// turnID ("" = whichever turn is running) targets one turn: the server cancels
// it only while it is the running turn, so a Stop for a turn that already
// ended can never hit a successor.
// Aborting the Stream context alone does not stop the turn: the server
// deliberately detaches a turn from its HTTP request so a dropped connection
// cannot kill work mid-flight. The caller's ctx may already be cancelled, so
// Cancel uses its own short deadline rather than inheriting it.
func (c *Client) Cancel(convID, turnID string) error {
	if strings.TrimSpace(convID) == "" {
		return nil // no conversation yet → nothing is running server-side
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := c.cfg.ServerURL + "/conversations/" + url.PathEscape(convID) + "/cancel"
	payload := []byte(`{"scope":"turn"}`)
	if id := strings.TrimSpace(turnID); id != "" {
		payload, _ = json.Marshal(map[string]string{"scope": "turn", "turn_id": id})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("cancel returned %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	return nil
}

// RemoveQueued withdraws a message fleet queued (DELETE
// /conversations/{convID}/queue/{inputID}) — the web queue chip's remove
// call. A 409 means it already started running, and is returned as an error.
func (c *Client) RemoveQueued(convID, inputID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := c.cfg.ServerURL + "/conversations/" + url.PathEscape(convID) + "/queue/" + url.PathEscape(inputID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("remove queued input returned %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("fleet server health check returned %d", resp.StatusCode)
	}
	return nil
}
