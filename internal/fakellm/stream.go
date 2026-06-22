package fakellm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── chat-completions wire types (response/SSE side) ──
//
// Field names mirror exactly what charmbracelet/openai-go decodes (see
// chatcompletion.go ChatCompletionChunk*): object, created, model, choices[],
// choices[].index, choices[].finish_reason, choices[].delta.{role,content,
// tool_calls[]}, tool_calls[].{index,id,type,function.{name,arguments}}, and
// the trailing usage object.

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []deltaToolCall `json:"tool_calls,omitempty"`
}

type deltaToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function deltaToolCallFn `json:"function"`
}

type deltaToolCallFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

const chunkObject = "chat.completion.chunk"

func ptr(s string) *string { return &s }

// writeSSE serializes one chunk as a `data: {json}\n\n` SSE event and flushes.
func writeSSE(w http.ResponseWriter, c chunk) {
	b, _ := json.Marshal(c)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	flush(w)
}

// streamText emits an assistant text turn: a role-priming chunk, the content
// split into a few deltas (so the stream resembles token-by-token delivery),
// a terminal chunk with finish_reason:"stop", a usage-only chunk, then [DONE].
func (s *Server) streamText(w http.ResponseWriter, r *http.Request, model, text string, delay time.Duration) {
	id := "fake-" + nonce()
	startSSE(w)

	// Role priming (matches real providers' first delta).
	writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Role: "assistant"}}}})

	for _, piece := range splitForStream(text) {
		if r.Context().Err() != nil {
			return
		}
		if delay > 0 {
			sleepCtx(r, delay)
		}
		writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
			Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Content: piece}}}})
	}

	// Terminal choice: a NON-EMPTY finish_reason is mandatory or fantasy treats
	// the stream as truncated and retries.
	writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: ptr("stop")}}})

	writeUsage(w, id, model)
	writeDone(w)
}

// streamToolCalls emits an assistant turn that calls one or more tools and
// finishes with finish_reason:"tool_calls". Each call is sent in a single chunk
// carrying the id, name and complete JSON arguments — the SDK's accumulator
// handles whole-in-one-chunk calls.
func (s *Server) streamToolCalls(w http.ResponseWriter, r *http.Request, model string, calls []ToolCall, delay time.Duration) {
	id := "fake-" + nonce()
	startSSE(w)

	writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Role: "assistant"}}}})

	for i, c := range calls {
		if r.Context().Err() != nil {
			return
		}
		if delay > 0 {
			sleepCtx(r, delay)
		}
		callID := c.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", nonce(), i)
		}
		args := c.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
			Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{
				ToolCalls: []deltaToolCall{{
					Index:    i,
					ID:       callID,
					Type:     "function",
					Function: deltaToolCallFn{Name: c.Name, Arguments: args},
				}},
			}}}})
	}

	writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: ptr("tool_calls")}}})

	writeUsage(w, id, model)
	writeDone(w)
}

func writeUsage(w http.ResponseWriter, id, model string) {
	// A usage-only chunk (empty choices) after include_usage; harmless if the
	// client ignores it, and keeps cost accounting clean.
	writeSSE(w, chunk{ID: id, Object: chunkObject, Created: now(), Model: model,
		Choices: []chunkChoice{}, Usage: &usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
}

func writeDone(w http.ResponseWriter) {
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush(w)
}

func startSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func now() int64 { return time.Now().Unix() }

// splitForStream chops text into small word-ish chunks so the SSE looks like
// real streaming. Always returns at least one element.
func splitForStream(s string) []string {
	if s == "" {
		return []string{""}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	out := make([]string, 0, len(words))
	for i, wd := range words {
		if i == 0 {
			out = append(out, wd)
		} else {
			out = append(out, " "+wd)
		}
	}
	return out
}

// sleepCtx sleeps for d or until the request context is cancelled.
func sleepCtx(r *http.Request, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
}

// ── non-streaming JSON responses (stream:false) ──
//
// fleet's interactive chat turns use streaming, but the agent's Generate path
// and the title/summary helpers issue non-streaming requests. These mirror the
// chat.completion (not .chunk) shape the SDK decodes.

type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usage              `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	ToolCalls []msgToolCall `json:"tool_calls,omitempty"`
}

type msgToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function deltaToolCallFn `json:"function"`
}

func writeJSON(w http.ResponseWriter, c completion) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c)
}

func (s *Server) jsonText(w http.ResponseWriter, model, text string) {
	writeJSON(w, completion{
		ID: "fake-" + nonce(), Object: "chat.completion", Created: now(), Model: model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      completionMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
		Usage: usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
}

func (s *Server) jsonToolCalls(w http.ResponseWriter, model string, calls []ToolCall) {
	tcs := make([]msgToolCall, 0, len(calls))
	for i, c := range calls {
		callID := c.ID
		if callID == "" {
			callID = fmt.Sprintf("call_%s_%d", nonce(), i)
		}
		args := c.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		tcs = append(tcs, msgToolCall{
			ID: callID, Type: "function",
			Function: deltaToolCallFn{Name: c.Name, Arguments: args},
		})
	}
	writeJSON(w, completion{
		ID: "fake-" + nonce(), Object: "chat.completion", Created: now(), Model: model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      completionMessage{Role: "assistant", ToolCalls: tcs},
			FinishReason: "tool_calls",
		}},
		Usage: usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
}

// ── /api/v1/models ──

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	ids := append([]string(nil), s.models...)
	s.mu.RUnlock()

	type entry struct {
		ID            string `json:"id"`
		ContextLength int    `json:"context_length"`
	}
	out := struct {
		Data []entry `json:"data"`
	}{}
	for _, id := range ids {
		out.Data = append(out.Data, entry{ID: id, ContextLength: 200000})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
