package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"charm.land/fantasy"
)

// EventSink is the callback interface the HTTP layer implements to turn
// agent streaming events into SSE frames (and, independently, into SQLite
// rows on the `messages` table).
//
// Implementations MUST be safe for concurrent calls from fantasy's callback
// goroutines, and MUST NOT block on the network — buffer or drop inside.
type EventSink interface {
	Emit(event string, payload any)
}

// HistoryEntry is the compact shape we store for every message / event in
// a conversation. SQLite rows deserialize into this and then get replayed
// into fantasy.Message on the next turn.
type HistoryEntry struct {
	Role    string          `json:"role"`    // user|assistant|tool
	Type    string          `json:"type"`    // text|reasoning|tool_call|tool_result
	Content json.RawMessage `json:"content"` // shape depends on Type
}

// entryTypeToolCall is the HistoryEntry.Type for an assistant tool call. The
// other type strings appear at most twice and stay inline; this one is shared
// by the persist path and both replay switches.
const entryTypeToolCall = "tool_call"

// TextContent for Type=text (user + assistant). Images is set on the
// user-side text entry when the user attached image files alongside their
// message; replayHistory reads those files back into fantasy.FilePart on the
// next turn so vision-capable models keep seeing them as conversation
// context. Older entries without Images replay as plain text — the field is
// optional and backward-compatible.
type TextContent struct {
	Text   string         `json:"text"`
	Images []ImageRefMeta `json:"images,omitempty"`
}

// ImageRefMeta is a pointer to an image file the user attached. Path is the
// absolute server-side path returned by /attachments and re-validated on the
// chat call; MediaType is the IANA type (e.g. image/png) we send to the
// model. We persist the path (not the bytes) because attachments are already
// on disk under the conversation's uploads token, and storing bytes inline
// would inflate the messages JSON for every reload.
type ImageRefMeta struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Name      string `json:"name,omitempty"`
}

// ImageAttachment is the per-turn input shape the HTTP layer hands to
// RunTurn. It carries enough metadata to both build the fantasy.FilePart we
// send to the model AND to persist a replayable history entry — the file
// itself stays on disk under the uploads token.
type ImageAttachment struct {
	Path      string
	MediaType string
	Name      string
}

// ReasoningContent for Type=reasoning.
type ReasoningContent struct {
	Text string `json:"text"`
}

// ToolCallContent for Type=tool_call.
type ToolCallContent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"` // raw JSON string the model emitted
}

// ToolResultContent for Type=tool_result.
type ToolResultContent struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	IsErr bool   `json:"is_err"`
}

// SummaryContent for Type=summary. Inserted by the user-initiated
// "summarize and continue" action: the model is asked to condense the
// conversation up to that point into a single structured brief, and
// the resulting text is persisted as a summary entry. From then on,
// turn construction skips every message before the summary and
// replays the summary itself as a synthetic assistant message — the
// pre-summary turns remain in the DB and on screen (collapsed in the
// UI) but never re-enter the model's context. Replace semantics: a
// new summarize call deletes any prior summary on the conversation
// before inserting, so chained drift is avoided.
type SummaryContent struct {
	Text string `json:"text"`
	// Model is the OpenRouter slug that produced this summary —
	// stored so the UI can show "summarized by X" next to the banner
	// and so future audits can reason about quality differences.
	Model string `json:"model,omitempty"`
	// PromptTokens / CompletionTokens / CostUSD let the totals chip
	// fold the summarize cost into the conversation total without a
	// special case; the summarize call is otherwise invisible to the
	// turn metrics table.
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// TurnSummaryContent for Type=turn_summary. Written once per assistant
// turn so the UI can show per-turn cost + tokens next to the message
// after a page reload.
//
// PromptTokens vs PromptTokensLastStep: PromptTokens is the SUM of
// per-step input tokens across every model call within this turn
// (load-bearing for cost — billing IS per-step input). For a tool-
// using turn with N steps, this can be many multiples of the actual
// conversation size. PromptTokensLastStep is the final step's input
// size — the right signal for "how full is the model's context
// window right now," which is what the UI's context indicator wants.
// Older conversations may omit the new field; the UI falls back to
// PromptTokens, with a defensive clamp so the indicator never shows
// the impossible "fraction > 1" that an accumulated sum can produce.
type TurnSummaryContent struct {
	CostUSD              float64 `json:"cost_usd"`
	PromptTokens         int     `json:"prompt_tokens"`
	PromptTokensLastStep int     `json:"prompt_tokens_last_step,omitempty"`
	CompletionTokens     int     `json:"completion_tokens"`
	CachedTokens         int     `json:"cached_tokens"`
	CacheCreationTokens  int     `json:"cache_creation_tokens"`
	DurationMs           int     `json:"duration_ms"`
	Cancelled            bool    `json:"cancelled,omitempty"`
	// Model is the OpenRouter slug that drove this turn (e.g.
	// "anthropic/claude-sonnet-4.6"). Stored per-turn because the
	// per-chat model override can change mid-conversation, and a silent
	// switch is the single biggest cause of "why did my cache just die"
	// confusion.
	Model string `json:"model,omitempty"`
}

// replayHistory converts stored HistoryEntry rows back into fantasy.Message
// form the agent can consume on the next turn. Adjacent assistant-text and
// tool_call entries get coalesced into a single assistant message; tool
// results become tool-role messages.
//
// Summary handling: when the entry list contains one or more `summary`
// entries (inserted by the user-initiated "summarize and continue"
// flow), the LLM context is reset at the *latest* summary marker —
// every entry before it is discarded, and the summary text is emitted
// as a synthetic assistant message. Pre-summary entries remain in the
// DB and the UI; they just stop entering the model's context.
func replayHistory(entries []HistoryEntry) ([]fantasy.Message, error) {
	// Find the index of the most recent summary entry. Anything before
	// it is supplanted by the summary itself.
	startIdx := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "summary" {
			startIdx = i
			break
		}
	}
	effective := entries[startIdx:]

	// Pass 1: pick the authoritative result for each tool-call id. A staged
	// tool's outcome (send_email / risky bash) is appended out-of-band when
	// the user clicks Send, reusing the original tool_call id — so a call can
	// end up with TWO result rows: the inline "APPROVAL_REQUIRED" placeholder
	// the agent saw during the turn, and the real outcome appended on
	// approval. Worse, if the user clicks before the turn that issued the
	// call has been persisted, the resolution row lands BEFORE its own
	// tool_call in id order. A naive replay would then emit a tool_result
	// with no preceding tool_use (or two results for one call), which every
	// provider rejects. We mark a result "out of order" when its tool_call
	// hasn't been seen yet at that point in the stream; out-of-order results
	// win over inline ones, and among same-category results the later wins
	// (covers the common case where the resolution is appended after the
	// placeholder). Pass 2 then emits exactly one result per call, paired
	// with the call, regardless of where the rows physically sit.
	chosenResult := map[string]ToolResultContent{}
	chosenOutOfOrder := map[string]bool{}
	seenCall := map[string]bool{}
	for _, e := range effective {
		switch e.Type {
		case entryTypeToolCall:
			var c ToolCallContent
			if err := json.Unmarshal(e.Content, &c); err != nil {
				return nil, err
			}
			seenCall[c.ID] = true
		case "tool_result":
			var c ToolResultContent
			if err := json.Unmarshal(e.Content, &c); err != nil {
				return nil, err
			}
			outOfOrder := !seenCall[c.ID]
			// Keep the prior choice only when it's out-of-order and this one
			// isn't; otherwise this result wins.
			if prevOOO, exists := chosenOutOfOrder[c.ID]; exists && prevOOO && !outOfOrder {
				continue
			}
			chosenResult[c.ID] = c
			chosenOutOfOrder[c.ID] = outOfOrder
		}
	}

	var out []fantasy.Message
	var pendingAssistant []fantasy.MessagePart

	flushAssistant := func() {
		if len(pendingAssistant) == 0 {
			return
		}
		out = append(out, fantasy.Message{
			Role:    fantasy.MessageRoleAssistant,
			Content: pendingAssistant,
		})
		// Immediately follow the assistant block with one tool message per
		// tool call it contains, using the result chosen in pass 1. Pairing
		// here (rather than when we hit the stored tool_result row) is what
		// makes replay robust to the out-of-order / duplicate result rows
		// described above. A call with no captured result at all (e.g. a turn
		// cut off mid-call) emits no tool message — same as before.
		for _, part := range pendingAssistant {
			tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
			if !ok {
				continue
			}
			res, ok := chosenResult[tc.ToolCallID]
			if !ok {
				continue
			}
			var output fantasy.ToolResultOutputContent
			if res.IsErr {
				output = fantasy.ToolResultOutputContentError{Error: fmt.Errorf("%s", res.Text)}
			} else {
				output = fantasy.ToolResultOutputContentText{Text: res.Text}
			}
			out = append(out, fantasy.Message{
				Role: fantasy.MessageRoleTool,
				Content: []fantasy.MessagePart{
					fantasy.ToolResultPart{ToolCallID: tc.ToolCallID, Output: output},
				},
			})
		}
		pendingAssistant = nil
	}

	for _, e := range effective {
		switch e.Type {
		case "summary":
			// Emit the summary as a single synthetic assistant message
			// so what follows is a natural user→assistant alternation.
			// Any pendingAssistant must be flushed first; in practice
			// startIdx ensures the summary is the first effective entry,
			// so this is just defensive.
			flushAssistant()
			var c SummaryContent
			if err := json.Unmarshal(e.Content, &c); err != nil {
				return nil, err
			}
			text := strings.TrimSpace(c.Text)
			if text == "" {
				// A blank summary is malformed; treat as no-op rather
				// than emit an empty assistant turn that will confuse
				// the next-turn provider call.
				continue
			}
			pendingAssistant = append(pendingAssistant, fantasy.TextPart{
				Text: "[Conversation summary so far — continuing from here]\n\n" + text,
			})
			flushAssistant()
		case "text":
			var c TextContent
			if err := json.Unmarshal(e.Content, &c); err != nil {
				return nil, err
			}
			switch e.Role {
			case "user":
				flushAssistant()
				parts := loadHistoryImageParts(c.Images)
				out = append(out, fantasy.NewUserMessage(c.Text, parts...))
			case "assistant":
				pendingAssistant = append(pendingAssistant, fantasy.TextPart{Text: c.Text})
			}
		case "reasoning", "turn_summary":
			// UI-facing only; never replayed to the model. Reasoning parts
			// are rejected by providers outside their originating step, and
			// turn_summary is just our own cost/duration metadata.
			continue
		case entryTypeToolCall:
			var c ToolCallContent
			if err := json.Unmarshal(e.Content, &c); err != nil {
				return nil, err
			}
			pendingAssistant = append(pendingAssistant, fantasy.ToolCallPart{
				ToolCallID: c.ID,
				ToolName:   c.Name,
				Input:      c.Input,
			})
		case "tool_result":
			// Close the current assistant step; flushAssistant emits the
			// paired result (chosen in pass 1). The stored result row itself
			// is not emitted here — doing so would double-emit results and
			// break on out-of-order rows. See the pass-1 comment above.
			flushAssistant()
		}
	}
	flushAssistant()
	return out, nil
}

// loadImageAttachments reads each attachment's bytes off disk and returns
// the matching fantasy.FilePart entries plus the small ImageRefMeta records
// to persist on the user-text history entry. Read failures are logged and
// dropped so a missing file (e.g. swept after TTL) never breaks the turn —
// the markdown attachment block on the user message still references the
// filename, so the conversation degrades, not crashes.
//
// Per-image cap (8 MB) and total cap (8 images) match generate_image's
// reference cap and the practical limits of Claude/Gemini vision today.
// defaultImageMediaType is the MIME fallback for attachment rows that
// carry no media type (uploads have historically been PNG-normalized).
const defaultImageMediaType = "image/png"

func loadImageAttachments(atts []ImageAttachment) ([]fantasy.FilePart, []ImageRefMeta) {
	const (
		maxImages       = 8
		maxBytesPerFile = 8 * 1024 * 1024
	)
	if len(atts) == 0 {
		return nil, nil
	}
	parts := make([]fantasy.FilePart, 0, len(atts))
	refs := make([]ImageRefMeta, 0, len(atts))
	for _, a := range atts {
		if len(parts) >= maxImages {
			log.Printf("loadImageAttachments: skipping %s (over %d cap)", a.Name, maxImages)
			continue
		}
		info, err := os.Stat(a.Path)
		if err != nil {
			log.Printf("loadImageAttachments: stat %s: %v", a.Path, err)
			continue
		}
		if info.Size() > maxBytesPerFile {
			log.Printf("loadImageAttachments: %s is %d bytes (> %d cap)", a.Path, info.Size(), maxBytesPerFile)
			continue
		}
		data, err := os.ReadFile(a.Path) // path was re-validated against uploads root
		if err != nil {
			log.Printf("loadImageAttachments: read %s: %v", a.Path, err)
			continue
		}
		mt := strings.TrimSpace(a.MediaType)
		if mt == "" {
			mt = defaultImageMediaType
		}
		parts = append(parts, fantasy.FilePart{
			Filename:  a.Name,
			Data:      data,
			MediaType: mt,
		})
		refs = append(refs, ImageRefMeta{
			Path:      a.Path,
			MediaType: mt,
			Name:      a.Name,
		})
	}
	return parts, refs
}

// loadHistoryImageParts re-reads the image files referenced in a persisted
// user message so they replay as multimodal context on subsequent turns.
// Any missing file is silently dropped — replay must never fail an entire
// turn just because an attachment got swept (TTL) since it was uploaded.
func loadHistoryImageParts(refs []ImageRefMeta) []fantasy.FilePart {
	if len(refs) == 0 {
		return nil
	}
	const maxBytesPerFile = 8 * 1024 * 1024
	parts := make([]fantasy.FilePart, 0, len(refs))
	for _, r := range refs {
		if r.Path == "" {
			continue
		}
		info, err := os.Stat(r.Path)
		if err != nil {
			log.Printf("loadHistoryImageParts: stat %s: %v (image dropped from replay)", r.Path, err)
			continue
		}
		if info.Size() > maxBytesPerFile {
			log.Printf("loadHistoryImageParts: %s is %d bytes (> %d cap)", r.Path, info.Size(), maxBytesPerFile)
			continue
		}
		data, err := os.ReadFile(r.Path) // path is from a previously validated history row
		if err != nil {
			log.Printf("loadHistoryImageParts: read %s: %v", r.Path, err)
			continue
		}
		mt := strings.TrimSpace(r.MediaType)
		if mt == "" {
			mt = defaultImageMediaType
		}
		parts = append(parts, fantasy.FilePart{
			Filename:  r.Name,
			Data:      data,
			MediaType: mt,
		})
	}
	return parts
}

func mustEntry(role, typ string, content any) HistoryEntry {
	b, err := json.Marshal(content)
	if err != nil {
		// content types are all static structs; a marshal failure would be a bug.
		panic(fmt.Sprintf("history entry marshal failed: %v", err))
	}
	return HistoryEntry{Role: role, Type: typ, Content: b}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}
