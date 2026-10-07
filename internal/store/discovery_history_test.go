package store

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// LoadDiscoveryHistory reads only what output discovery needs: user/assistant
// text and summaries (as content-free boundaries), assistant text with its
// content and user text without, newest first in pages, stopped by the
// caller's callback and cut at the byte budget in SQL — and handed back in
// ascending order.
func TestLoadDiscoveryHistory(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Q3") // user, tool_call, assistant
	more := []agent.HistoryEntry{
		{Role: "tool", Type: "tool_result", Content: []byte(`{"output":"SECRET"}`)},
		{Role: "assistant", Type: "reasoning", Content: []byte(`{"text":"hmm"}`)},
		{Role: "user", Type: "summary", Content: []byte(`{"text":"the summary text"}`)},
		{Role: "user", Type: "text", Content: []byte(`{"text":"private question"}`)},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"[a](a.csv)"}`)},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"[b](b.csv)"}`)},
	}
	if _, err := f.s.AppendHistory(f.ctx, c.ID, more); err != nil {
		t.Fatal(err)
	}
	full, err := f.s.LoadHistory(f.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}

	old := discoveryHistoryPage
	discoveryHistoryPage = 2 // exercise the paging
	t.Cleanup(func() { discoveryHistoryPage = old })

	all := func(agent.HistoryEntry) bool { return true }
	got, err := f.s.LoadDiscoveryHistory(f.ctx, c.ID, 0, 1<<20, all)
	if err != nil {
		t.Fatal(err)
	}
	shape := make([]string, 0, len(got))
	for i, e := range got {
		if i > 0 && got[i-1].ID >= e.ID {
			t.Fatalf("not ascending: %v", got)
		}
		shape = append(shape, e.Role+"/"+e.Type+"="+string(e.Content))
	}
	want := []string{
		`user/text={}`,
		`assistant/text={"text":"about 12 bps"}`,
		`user/summary_boundary={}`,
		`user/text={}`,
		`assistant/text={"text":"[a](a.csv)"}`,
		`assistant/text={"text":"[b](b.csv)"}`,
	}
	if strings.Join(shape, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(shape, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range got {
		if strings.Contains(string(e.Content), "SECRET") || strings.Contains(string(e.Content), "summary text") ||
			strings.Contains(string(e.Content), "private question") {
			t.Errorf("leaked content: %s", e.Content)
		}
	}

	// through: nothing past the bound.
	through := full[len(full)-2].ID // the [a] reply
	got, err = f.s.LoadDiscoveryHistory(f.ctx, c.ID, through, 1<<20, all)
	if err != nil {
		t.Fatal(err)
	}
	if last := got[len(got)-1]; last.ID != through {
		t.Errorf("through %d: last row %d", through, last.ID)
	}

	// The callback stops the read, keeping the row it stopped on.
	seen := 0
	got, err = f.s.LoadDiscoveryHistory(f.ctx, c.ID, 0, 1<<20, func(agent.HistoryEntry) bool {
		seen++
		return seen < 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || seen != 3 || got[len(got)-1].ID != full[len(full)-1].ID {
		t.Errorf("stopped read = %d rows (callback saw %d), want the newest 3", len(got), seen)
	}

	// The byte budget: rows are kept while the assistant-text bytes NEWER
	// than them fit, so the row that crosses it is still read (the caller's
	// own check sees it) and nothing older is.
	b := int64(len(`{"text":"[b](b.csv)"}`))
	got, err = f.s.LoadDiscoveryHistory(f.ctx, c.ID, 0, b, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(string(got[0].Content), "a.csv") {
		raw := make([]string, 0, len(got))
		for _, e := range got {
			raw = append(raw, string(e.Content))
		}
		t.Errorf("byte-cut read = %v, want the [a] and [b] replies only", raw)
	}

	// An unknown conversation reads nothing.
	if got, err := f.s.LoadDiscoveryHistory(f.ctx, "nope", 0, 1<<20, all); err != nil || len(got) != 0 {
		t.Errorf("unknown = %v, %v", got, err)
	}
}

// The branch no longer loads the transcript to authorize a teammate: the
// meta read is the same gate as the full read.
func TestGetTeamVisibleConversationMetaSameGate(t *testing.T) {
	f := newTeamFixture(t)
	c := f.sharedChat(t, "alice@x.com", f.project.ID, "Q3")
	for _, who := range []string{"alice@x.com", "bob@x.com", "dana@x.com", "carol@x.com", "nobody@x.com"} {
		full, err := f.s.GetTeamVisibleConversation(f.ctx, who, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		meta, err := f.s.GetTeamVisibleConversationMeta(f.ctx, who, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (full == nil) != (meta == nil) {
			t.Fatalf("%s: full=%v meta=%v", who, full != nil, meta != nil)
		}
		if meta != nil && (meta.ID != full.ID || meta.Title != full.Title || meta.Messages != nil) {
			t.Errorf("%s: meta = %+v", who, meta)
		}
	}
}
