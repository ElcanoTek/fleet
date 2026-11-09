package admincli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

type fakeChatModelStore struct {
	entries   []store.ConversationModelEntry
	updated   int
	lastModel string
	lastFrom  string
}

func (f *fakeChatModelStore) ListConversationsForModelUpdate(_ context.Context, fromModel string) ([]store.ConversationModelEntry, error) {
	var out []store.ConversationModelEntry
	for _, e := range f.entries {
		if fromModel == "" || e.Model == fromModel {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeChatModelStore) BulkUpdateConversationModel(_ context.Context, model, fromModel string) (int, error) {
	f.lastModel = model
	f.lastFrom = fromModel
	return f.updated, nil
}

func TestChatSetModel_DryRunAndApply(t *testing.T) {
	st := &fakeChatModelStore{
		entries: []store.ConversationModelEntry{
			{ID: "conv-1", UserEmail: "alice@example.com", Model: "anthropic/claude-opus-5.5", Title: "Q3 report"},
			{ID: "conv-2", UserEmail: "bob@example.com", Model: "", Title: "New chat"},
		},
		updated: 2,
	}

	var outBuf, errBuf bytes.Buffer
	code := runChatSetModel(context.Background(), st, &outBuf, &errBuf, "google/gemini-4-argon", "", true, false)
	if code != 0 {
		t.Fatalf("dry-run code = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "conv-1\talice@example.com\tmodel anthropic/claude-opus-5.5 → google/gemini-4-argon") {
		t.Errorf("dry-run stdout missing conv-1 line: %s", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "conv-2\tbob@example.com\tmodel <none> → google/gemini-4-argon") {
		t.Errorf("dry-run stdout missing conv-2 line: %s", outBuf.String())
	}
	if !strings.Contains(errBuf.String(), "dry-run: 2 conversation(s) would be re-assigned") {
		t.Errorf("dry-run stderr = %s", errBuf.String())
	}
	if st.lastModel != "" {
		t.Errorf("dry-run mutated store: lastModel=%q", st.lastModel)
	}

	outBuf.Reset()
	errBuf.Reset()
	code = runChatSetModel(context.Background(), st, &outBuf, &errBuf, "google/gemini-4-argon", "anthropic/claude-opus-5.5", false, true)
	if code != 0 {
		t.Fatalf("apply code = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if st.lastModel != "google/gemini-4-argon" || st.lastFrom != "anthropic/claude-opus-5.5" {
		t.Errorf("store call = (%q, %q), want (google/gemini-4-argon, anthropic/claude-opus-5.5)", st.lastModel, st.lastFrom)
	}
	if !strings.Contains(errBuf.String(), "re-assigned model on 2 conversation(s)") {
		t.Errorf("apply stderr = %s", errBuf.String())
	}
}
