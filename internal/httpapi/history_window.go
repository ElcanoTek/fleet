package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The windowed read of GET /conversations/{id} (docs/CHAT-HISTORY-WINDOW.md).
//
// Opt-in: a request that names none of `limit`, `before` and `from` gets the
// whole history, exactly as before the window existed.
//
//	?limit=N            the newest N entries, widened back to the opening
//	                    message of the turn the N-th falls in
//	?limit=N&before=ID  the same, of the entries older than ID (paging back)
//	?from=ID            everything from the turn that holds entry ID onward
//	                    (re-reading a window the client already holds)
//
// A windowed response adds `history_page`:
//
//	{"has_more": true, "next_before": 1234,
//	 "earlier": {"cards": [...], "totals": {...}, "summary_id": 77}}
//
// next_before is the id the window starts at: pass it as `before` for the
// next older page, or as `from` to re-read this window. `earlier` (present
// while has_more) is what the entries older than the window contribute to
// views of the whole conversation, so a client that holds only a window still
// draws them right: the show_ui cards a dashboard panel is built from, the
// user messages that answered a card, the cost and token totals of the turns
// not loaded, and where a compaction summary sits.

const (
	// maxHistoryWindowLimit bounds `limit`. A larger value is clamped, not
	// refused: the caller asked for "a lot", and gets the most a page holds.
	maxHistoryWindowLimit = 500
	// maxEarlierCardBytes bounds the card entries of `earlier`. Past it the
	// oldest cards that something later replaced are left out (see earlierOf).
	maxEarlierCardBytes = 512 << 10
)

// historyWindowRequest is a parsed windowed read.
type historyWindowRequest struct {
	limit  int
	before int64
	from   int64
}

// historyWindowError is a refused window parameter; its text is the 400 body.
type historyWindowError string

func (e historyWindowError) Error() string { return string(e) }

// parseHistoryWindow reads the window parameters. windowed is false when the
// request names none of them (an empty value counts as absent), which is the
// whole-history read every caller made before the window existed.
func parseHistoryWindow(q url.Values) (req historyWindowRequest, windowed bool, err error) {
	limit, before, from := q.Get("limit"), q.Get("before"), q.Get("from")
	if limit == "" && before == "" && from == "" {
		return req, false, nil
	}
	positive := func(name, raw string) (int64, error) {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 1 {
			return 0, historyWindowError(name + " must be a positive integer")
		}
		return n, nil
	}
	if from != "" {
		if limit != "" || before != "" {
			return req, false, historyWindowError("from cannot be combined with limit or before")
		}
		if req.from, err = positive("from", from); err != nil {
			return req, false, err
		}
		return req, true, nil
	}
	if limit == "" {
		return req, false, historyWindowError("before requires limit")
	}
	n, err := positive("limit", limit)
	if err != nil {
		return req, false, err
	}
	if n > maxHistoryWindowLimit {
		n = maxHistoryWindowLimit
	}
	req.limit = int(n)
	if before != "" {
		if req.before, err = positive("before", before); err != nil {
			return req, false, err
		}
	}
	return req, true, nil
}

// historyPage is the `history_page` object of a windowed response.
type historyPage struct {
	HasMore bool `json:"has_more"`
	// NextBefore is the id the window starts at; 0 (omitted) for an empty one.
	NextBefore int64           `json:"next_before,omitempty"`
	Earlier    *historyEarlier `json:"earlier,omitempty"`
}

// historyEarlier is what the entries older than a window contribute to views
// of the whole conversation, projected for the viewer like the window itself.
type historyEarlier struct {
	// Cards are history entries, oldest first: each show_ui call that drew a
	// card with its result, and the user messages that answered a card.
	Cards []historyEntryResponse `json:"cards"`
	// CardsTrimmed is set when older, since-replaced cards were left out to
	// keep the response bounded. Every card nothing has replaced is present.
	CardsTrimmed bool          `json:"cards_trimmed,omitempty"`
	Totals       historyTotals `json:"totals"`
	// SummaryID is the id of the compaction summary among the older entries,
	// for a viewer who is shown it: cards before it are retired.
	SummaryID int64 `json:"summary_id,omitempty"`
}

// historyTotals sums the turn summaries of the turns not loaded, with the
// rules the web's totals chip applies to the ones that are (conversationTotals
// in web/src/app/chat/ui/history.ts): a turn counts when it recorded any cost,
// tokens or duration, and cached tokens are capped at the turn's prompt tokens.
type historyTotals struct {
	CostUSD      float64 `json:"cost_usd"`
	PromptTokens int     `json:"prompt_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	Turns        int     `json:"turns"`
}

// historyWindowStore is what a windowed read needs of the store.
type historyWindowStore interface {
	LoadHistory(ctx context.Context, convID string) ([]agent.HistoryEntry, error)
	LoadHistoryTail(ctx context.Context, convID string, limit int, beforeID int64) (store.HistoryWindow, error)
	LoadHistoryFrom(ctx context.Context, convID string, fromID int64) (store.HistoryWindow, error)
	LoadHistoryOutline(ctx context.Context, convID string, beforeID int64) ([]agent.HistoryEntry, error)
}

// loadHistoryPage reads the window req names and projects it for viewer. The
// entries are the ones historyForParticipant would return for the same ids out
// of the whole history: the window starts where a turn opens, so the
// projection has nothing to carry in — except in the one case
// windowNeedsWholeHistory finds, which is answered from the whole history.
func loadHistoryPage(ctx context.Context, st historyWindowStore, convID, viewer, owner string, req historyWindowRequest) ([]historyEntryResponse, *historyPage, error) {
	var (
		w   store.HistoryWindow
		err error
	)
	if req.from > 0 {
		w, err = st.LoadHistoryFrom(ctx, convID, req.from)
	} else {
		w, err = st.LoadHistoryTail(ctx, convID, req.limit, req.before)
	}
	if err != nil {
		return nil, nil, err
	}
	var entries []historyEntryResponse
	if w.HasMore && viewer != "" && windowNeedsWholeHistory(w.Entries) {
		full, err := st.LoadHistory(ctx, convID)
		if err != nil {
			return nil, nil, err
		}
		entries = make([]historyEntryResponse, 0, len(w.Entries))
		for _, e := range historyForParticipant(full, viewer, owner) {
			if e.ID >= w.StartID && (req.before == 0 || e.ID < req.before) {
				entries = append(entries, e)
			}
		}
	} else {
		entries = historyForParticipant(w.Entries, viewer, owner)
	}
	page := &historyPage{HasMore: w.HasMore, NextBefore: w.StartID}
	if w.HasMore {
		outline, err := st.LoadHistoryOutline(ctx, convID, w.StartID)
		if err != nil {
			return nil, nil, err
		}
		page.Earlier = earlierOf(outline, viewer, owner)
	}
	return entries, page, nil
}

// windowNeedsWholeHistory reports whether projecting a window on its own
// could differ from projecting the whole history: it holds a tool result that
// does not name its sender and whose call is not earlier in the window. Such a
// result (an approval settled turns after its call, written by startup
// recovery) belongs to the sender of the turn that made the call, and that
// turn is older than the window. It is rare, and the answer then comes from
// the whole history rather than from a guess at whose result it is.
func windowNeedsWholeHistory(entries []agent.HistoryEntry) bool {
	calls := make(map[string]bool)
	for _, e := range entries {
		switch e.Type {
		case "tool_call":
			var c agent.ToolCallContent
			if json.Unmarshal(e.Content, &c) == nil && strings.TrimSpace(c.ID) != "" {
				calls[strings.TrimSpace(c.ID)] = true
			}
		case "tool_result":
			var c agent.ToolResultContent
			if json.Unmarshal(e.Content, &c) != nil {
				continue
			}
			if strings.TrimSpace(c.SenderEmail) == "" && !calls[strings.TrimSpace(c.ID)] {
				return true
			}
		}
	}
	return false
}

// The markers a card's Submit and quick-reply buttons write at the start of
// the user message they send (web/src/app/chat/ui/genui/model.ts).
const (
	uiSubmissionPrefix = "[UI submission] "
	uiReplyPrefix      = "[UI reply] "
)

// earlierOf builds `earlier` from a conversation's outline
// (store.LoadHistoryOutline) for viewer. The outline is projected by the same
// historyForParticipant as a transcript, so it shows this viewer nothing a
// whole-history read would not: a shown card reaches every participant, an
// answer is the message its sender wrote, and another sender's turn summary
// keeps no cost.
func earlierOf(outline []agent.HistoryEntry, viewer, owner string) *historyEarlier {
	out := &historyEarlier{Cards: []historyEntryResponse{}}
	shown := sharedCardEntries(outline)
	isCard := make(map[int64]bool, len(shown))
	for i := range shown {
		isCard[outline[i].ID] = true
	}
	// The web keeps one summary per assistant message, the last written, and
	// a user message or a compaction summary ends that message.
	var pending *agent.TurnSummaryContent
	flush := func() {
		ts := pending
		pending = nil
		if ts == nil || (ts.CostUSD <= 0 && ts.PromptTokens <= 0 && ts.CompletionTokens <= 0 && ts.DurationMs <= 0) {
			return
		}
		out.Totals.CostUSD += ts.CostUSD
		out.Totals.PromptTokens += ts.PromptTokens
		out.Totals.CachedTokens += min(ts.CachedTokens, ts.PromptTokens)
		out.Totals.Turns++
	}
	for _, e := range historyForParticipant(outline, viewer, owner) {
		switch {
		case e.Role == "user" && e.Type == "text":
			flush()
			var c agent.TextContent
			if json.Unmarshal(e.Content, &c) == nil &&
				(strings.HasPrefix(c.Text, uiSubmissionPrefix) || strings.HasPrefix(c.Text, uiReplyPrefix)) {
				out.Cards = append(out.Cards, e)
			}
		case e.Type == "summary":
			flush()
			if out.SummaryID == 0 {
				out.SummaryID = e.ID
			}
		case e.Type == agent.EntryTypeSummaryBoundary:
			flush()
		case e.Type == "turn_summary":
			var ts agent.TurnSummaryContent
			if json.Unmarshal(e.Content, &ts) == nil {
				pending = &ts
			}
		case isCard[e.ID]:
			out.Cards = append(out.Cards, e)
		}
	}
	flush()
	out.Cards, out.CardsTrimmed = trimEarlierCards(out.Cards, outline)
	return out
}

// trimEarlierCards keeps the card entries of `earlier` within
// maxEarlierCardBytes. A patrol that redraws a panel every few minutes leaves
// thousands of cards behind, all but the newest replaced; past the bound the
// oldest replaced cards are dropped (call and result together) until the rest
// fits. A card nothing has replaced is never dropped, so every panel and every
// card still awaiting an answer is drawn; what a trimmed board loses is the
// version count of a panel whose early versions were left out.
func trimEarlierCards(cards []historyEntryResponse, outline []agent.HistoryEntry) ([]historyEntryResponse, bool) {
	size := 0
	for _, e := range cards {
		size += len(e.Content)
	}
	if size <= maxEarlierCardBytes {
		return cards, false
	}
	drop := make(map[int64]bool)
	for _, c := range conversationShownCards(outline) {
		if size <= maxEarlierCardBytes {
			break
		}
		if c.current {
			continue
		}
		for _, idx := range []int{c.callIdx, c.resultIdx} {
			drop[outline[idx].ID] = true
			size -= len(outline[idx].Content)
		}
	}
	if len(drop) == 0 {
		return cards, false
	}
	kept := make([]historyEntryResponse, 0, len(cards))
	for _, e := range cards {
		if !drop[e.ID] {
			kept = append(kept, e)
		}
	}
	return kept, true
}
