package store

// Branching must not copy the PARENT's server-injected context (ADR-0058).
//
// Red/green: a chat turn's user message used to be stored as one blob — what
// the user typed plus the attachment manifest with its absolute paths. A
// teammate branching a team-shared chat therefore got a copy that named the
// OWNER'S upload, and the fork's run_python opened that path and read the raw
// rows. The brancher had attached nothing.
//
// Two mechanisms are pinned here: the injected suffix now lives in its own
// column and is simply not selected by the copy, and a legacy row that still
// embeds the blocks in its text is stripped by marker.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// The exact block the chat server injects for a non-image attachment, with the
// path shape the QA finding reported.
const testInjectedAttachmentBlock = "\n\n---\n**User attached files:**\n" +
	"- `fleet-team-share-test.csv` (1.2 KB, /var/lib/fleet/data/attachments/uploads/fq6xyz/fleet-team-share-test.csv)\n" +
	"\nThese files are saved in this conversation's workspace (the `attachments/` subdirectory).\n"

const testInjectedLibraryBlock = "\n\n---\n**Shared file library** (files your administrator published to every conversation):\n" +
	"- `shared/prices.csv` (4.0 KB)\n"

// userEntryWithInjected is the modern (post-056) shape: the user's own text in
// content, the server's suffix beside it.
func userEntryWithInjected(t *testing.T, text, injected string) agent.HistoryEntry {
	t.Helper()
	raw, err := json.Marshal(agent.TextContent{Text: text})
	if err != nil {
		t.Fatalf("marshal text content: %v", err)
	}
	return agent.HistoryEntry{Role: "user", Type: "text", Content: raw, InjectedContext: injected}
}

// TestBranchDropsTheParentsInjectedContext: the fork carries the user's words
// and none of the turn's injected context — no attachment path, no library
// listing — on the owner's own branch as well as a teammate's. One rule: no
// copy of a message carries a path into a conversation that does not own it.
func TestBranchDropsTheParentsInjectedContext(t *testing.T) {
	f := newTeamFixture(t)
	parent := f.sharedChat(t, "alice@x.com", f.project.ID, "Weekly Channel Spend and CPM")

	// The turn the finding describes: a question plus an attached CSV, stored
	// the way CommitUserMessage stores it in production.
	if _, err := f.s.CommitUserMessage(f.ctx, parent.ID, "turn-att",
		userEntryWithInjected(t, "what is the CPM by channel?",
			testInjectedAttachmentBlock+testInjectedLibraryBlock)); err != nil {
		t.Fatalf("CommitUserMessage: %v", err)
	}
	full, err := f.s.LoadHistory(f.ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := full[len(full)-1]
	if last.InjectedContext == "" {
		t.Fatal("the parent's own row must carry the injected context (the model still sees it)")
	}

	for _, brancher := range []string{"bob@x.com", "alice@x.com"} {
		branch, err := f.s.BranchConversation(f.ctx, brancher, parent.ID, last.ID, "fork")
		if err != nil {
			t.Fatalf("branch as %s: %v", brancher, err)
		}
		copied, err := f.s.LoadHistory(f.ctx, branch.ID)
		if err != nil {
			t.Fatal(err)
		}
		var sawQuestion bool
		for _, m := range copied {
			if m.InjectedContext != "" {
				t.Errorf("%s's branch copied injected context: %q", brancher, m.InjectedContext)
			}
			body := string(m.Content)
			for _, leak := range []string{"attachments/uploads", "User attached files", "Shared file library"} {
				if strings.Contains(body, leak) {
					t.Errorf("%s's branch copied %q in: %s", brancher, leak, body)
				}
			}
			if strings.Contains(body, "what is the CPM by channel?") {
				sawQuestion = true
			}
		}
		if !sawQuestion {
			t.Errorf("%s's branch lost the user's actual question", brancher)
		}
	}
}

// TestBranchStripsLegacyInlineInjectedContext: rows written BEFORE migration
// 056 embedded the blocks in content.text, so leaving the new column
// unselected is not enough for them. The marker strip is the second layer, and
// it keeps the user's own words.
func TestBranchStripsLegacyInlineInjectedContext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	conv, err := s.CreateConversation(ctx, owner, "Legacy", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	legacy, err := json.Marshal(agent.TextContent{
		Text: "what marker is in the last row?" + testInjectedAttachmentBlock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: legacy},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"MARKER-ZEBRA-7741"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	ids := messageIDs(t, s, conv.ID)

	branch, err := s.BranchConversation(ctx, owner, conv.ID, ids[len(ids)-1], "fork")
	if err != nil {
		t.Fatalf("BranchConversation: %v", err)
	}
	copied, err := s.LoadHistory(ctx, branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 2 {
		t.Fatalf("copied %d entries, want 2", len(copied))
	}
	var tc agent.TextContent
	if err := json.Unmarshal(copied[0].Content, &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Text != "what marker is in the last row?" {
		t.Errorf("copied user text = %q; want the typed question with the injected block cut off", tc.Text)
	}
	if strings.Contains(string(copied[0].Content), "attachments/uploads") {
		t.Errorf("legacy inline path survived the copy: %s", copied[0].Content)
	}
}

// TestBranchEmitsNoEmptyEntriesForFilteredContent (finding #11's data half):
// where the copy is not allowed to carry a message's content, it must write
// NOTHING — not a row with empty text, which renders as an empty bubble in the
// branched transcript.
func TestBranchEmitsNoEmptyEntriesForFilteredContent(t *testing.T) {
	f := newTeamFixture(t)
	parent := f.sharedChat(t, "alice@x.com", f.project.ID, "Charts")

	// Two messages that leave nothing behind once filtered: an image-only turn
	// (the teammate copy strips images, which were all it held) and a legacy
	// attachment-only turn (nothing typed, just the injected block).
	legacy, err := json.Marshal(agent.TextContent{Text: testInjectedAttachmentBlock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AppendHistory(f.ctx, parent.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: []byte(`{"text":"  ","images":[{"path":"/w/alice/secret.png","name":"secret.png"}]}`)},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"that is the Q3 chart"}`)},
		{Role: "user", Type: "text", Content: legacy},
		{Role: "assistant", Type: "text", Content: []byte(`{"text":"read it"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	full, err := f.s.LoadHistory(f.ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := full[len(full)-1]

	branch, err := f.s.BranchConversation(f.ctx, "bob@x.com", parent.ID, last.ID, "fork")
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	copied, err := f.s.LoadHistory(f.ctx, branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range copied {
		var tc agent.TextContent
		if err := json.Unmarshal(m.Content, &tc); err != nil {
			continue
		}
		if strings.TrimSpace(tc.Text) == "" {
			t.Errorf("branch wrote an empty %s entry (placeholder for filtered content): %s", m.Role, m.Content)
		}
		if strings.Contains(string(m.Content), "/w/alice/") {
			t.Errorf("branch copied a path into the owner's workspace: %s", m.Content)
		}
	}
	// The prose either side of the dropped rows still made it.
	if len(copied) == 0 {
		t.Fatal("the branch must still carry the transcript")
	}
}

// The strip is a suffix cut, never an interior rewrite, and it does not fire
// on text a user merely typed that resembles a header.
func TestStripLegacyInjectedTextIsConservative(t *testing.T) {
	entry := func(text string) *agent.HistoryEntry {
		raw, err := json.Marshal(agent.TextContent{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return &agent.HistoryEntry{Role: "user", Type: "text", Content: raw}
	}
	textOf := func(e *agent.HistoryEntry) string {
		var tc agent.TextContent
		if err := json.Unmarshal(e.Content, &tc); err != nil {
			t.Fatal(err)
		}
		return tc.Text
	}

	// A bare mention of a header, without the injected separator, is the
	// user's own prose and survives untouched.
	typed := entry("remind me what **User attached files:** means")
	if emptied, err := stripLegacyInjectedText(typed); err != nil || emptied {
		t.Fatalf("stripLegacyInjectedText = (%v, %v), want (false, nil)", emptied, err)
	}
	if got := textOf(typed); got != "remind me what **User attached files:** means" {
		t.Errorf("typed prose was rewritten: %q", got)
	}

	// A real block is cut, and an entry that held nothing else reports emptied.
	only := entry(testInjectedLibraryBlock)
	emptied, err := stripLegacyInjectedText(only)
	if err != nil || !emptied {
		t.Fatalf("block-only entry: (%v, %v), want (true, nil)", emptied, err)
	}
	if got := textOf(only); strings.TrimSpace(got) != "" {
		t.Errorf("block-only entry text = %q, want empty", got)
	}

	// Assistant and tool entries are never touched.
	assistant := &agent.HistoryEntry{Role: "assistant", Type: "text",
		Content: []byte(`{"text":"` + "here" + `"}`)}
	if emptied, err := stripLegacyInjectedText(assistant); err != nil || emptied {
		t.Fatalf("assistant entry: (%v, %v), want (false, nil)", emptied, err)
	}
}
