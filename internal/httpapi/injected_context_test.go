package httpapi

// The turn's injected blocks are now accumulated from an EMPTY base and joined
// to the user's text only for the provider call. That refactor has to be
// byte-neutral for the model: the composed message must equal what the old
// chain (each appender concatenating onto the message itself) produced, or
// every replayed turn reads as a new prompt prefix
// (docs/PROMPT-CACHE-CONTRACT.md) and the model's view of a turn changes for
// no reason.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sharedfiles"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestInjectedContextComposesToTheOldBytes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "report.csv"), []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	images := []chatAttachment{{Name: "shot.png", Size: 2048}}
	others := []chatAttachment{{Name: "spend.csv", Path: filepath.Join(workspace, "attachments", "spend.csv"), Size: 1234}}
	library := sharedfiles.PromptBlock([]store.SharedFile{{Name: "prices.csv", SizeBytes: 4096}})

	for _, message := range []string{
		"what is the CPM by channel?",
		"trailing newline\n",
		"trailing blank lines\n\n\n",
		"", // an attachment-only turn
	} {
		// The chain as it was: every block appended onto the user's message.
		old := appendAttachmentsBlock(message, images, others)
		old = appendWorkspaceInventoryBlock(old, workspace)
		old = strings.TrimRight(old, "\n") + "\n\n" + library

		// The chain as it is: the same appenders from an empty base, joined
		// once at the end.
		injected := appendAttachmentsBlock("", images, others)
		injected = appendWorkspaceInventoryBlock(injected, workspace)
		injected = strings.TrimRight(injected, "\n") + "\n\n" + library

		if got := agent.ComposeUserMessage(message, injected); got != old {
			t.Errorf("message %q composed differently:\n--- new ---\n%s\n--- old ---\n%s", message, got, old)
		}
		// And the injected half must be recognizable to the legacy strip, so a
		// row written today and a row written last month branch the same way.
		if _, _, stripped := agent.StripLegacyInjectedContext(agent.ComposeUserMessage(message, injected)); !stripped {
			t.Errorf("message %q: composed text is not recognizable as carrying injected blocks", message)
		}
	}
}

// The attachment block advertises the STAGED path and describes the workspace
// lifetime on both backends — the uploads area is not a place a sandbox can
// read from any more (ADR-0058).
func TestAttachmentBlockAdvertisesTheStagedPath(t *testing.T) {
	staged := "/var/lib/fleet/workspace/conv-a/attachments/spend.csv"
	got := appendAttachmentsBlock("", nil, []chatAttachment{{Name: "spend.csv", Path: staged, Size: 1234}})
	if !strings.Contains(got, staged) {
		t.Errorf("staged path missing from the block:\n%s", got)
	}
	if !strings.Contains(got, "conversation's workspace") {
		t.Errorf("block does not describe the workspace lifetime:\n%s", got)
	}
	if strings.Contains(got, "temporary uploads area") {
		t.Errorf("block still describes the uploads area:\n%s", got)
	}
}

// historyForClient is the only surface that publishes injected context, and it
// publishes it as its OWN field — never merged back into the user's text.
func TestHistoryForClientExposesInjectedContextSeparately(t *testing.T) {
	entries := []agent.HistoryEntry{
		{ID: 7, Role: "user", Type: "text", Content: []byte(`{"text":"hi"}`), InjectedContext: "\n\n---\n**User attached files:**\n"},
		{ID: 8, Role: "assistant", Type: "text", Content: []byte(`{"text":"hello"}`)},
	}
	out := historyForClient(entries)
	if len(out) != 2 {
		t.Fatalf("entries = %d, want 2", len(out))
	}
	if out[0].InjectedContext != entries[0].InjectedContext {
		t.Errorf("injected context = %q, want it carried through", out[0].InjectedContext)
	}
	if string(out[0].Content) != `{"text":"hi"}` {
		t.Errorf("content = %s; the user's text must stay exactly what they typed", out[0].Content)
	}
	if out[1].InjectedContext != "" {
		t.Errorf("assistant entry carries injected context: %q", out[1].InjectedContext)
	}
	// Empty stays absent on the wire (omitempty), so the common entry does not
	// grow a null field.
	if len(historyForClient(nil)) != 0 {
		t.Error("nil history must project to an empty slice, not a nil one")
	}
}
