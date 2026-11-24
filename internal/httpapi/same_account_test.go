package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Two different accounts whose addresses strings.EqualFold calls the same
// (GREEK SMALL LETTER SIGMA and FINAL SIGMA). Each check below treats them as
// two people, and still treats one account written in other capitals (or
// stored that way by an older release) as one person.
const (
	sigmaOwner  = "σ@example.com"
	sigmaFinal  = "ς@example.com"
	sigmaCapish = "Σ@EXAMPLE.com" // the owner's address in capitals: lowercases to sigmaOwner
)

func TestMultiplayerOwnerChecksKeepFoldPairAccountsApart(t *testing.T) {
	t.Parallel()
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)

	wCreate := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations", sigmaOwner, map[string]any{"title": "Fold pair"})
	if wCreate.Code != http.StatusOK {
		t.Fatalf("create: %d %s", wCreate.Code, wCreate.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(wCreate.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}
	if w := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", sigmaOwner, map[string]any{"multiplayer": true}); w.Code != http.StatusOK {
		t.Fatalf("enable multiplayer: %d %s", w.Code, w.Body.String())
	}

	isOwner := func(user string) bool {
		t.Helper()
		w := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations/"+conv.ID+"/multiplayer", user, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET multiplayer as %q: %d %s", user, w.Code, w.Body.String())
		}
		var body struct {
			IsOwner bool `json:"is_owner"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.IsOwner
	}

	// The other account is a participant: not the owner, and refused the
	// owner-only settings.
	if isOwner(sigmaFinal) {
		t.Errorf("%q reads as the owner of %q's chat", sigmaFinal, sigmaOwner)
	}
	if w := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", sigmaFinal, map[string]any{"multiplayer": false}); w.Code != http.StatusForbidden {
		t.Errorf("%q changed the owner's multiplayer setting: %d %s", sigmaFinal, w.Code, w.Body.String())
	}

	// The owner, signed in with capitals, is still the owner.
	for _, user := range []string{sigmaOwner, sigmaCapish} {
		if !isOwner(user) {
			t.Errorf("%q does not read as the owner of their own chat", user)
		}
	}

	// A row an older release stored with capitals still names its owner.
	st.mu.Lock()
	st.convs[conv.ID].UserEmail = "Σ@Example.com"
	st.mu.Unlock()
	if !isOwner(sigmaOwner) {
		t.Errorf("%q does not own a chat stored as %q", sigmaOwner, "Σ@Example.com")
	}
	if isOwner(sigmaFinal) {
		t.Errorf("%q owns a chat stored as %q", sigmaFinal, "Σ@Example.com")
	}
}

func TestSenderAndViewerChecksKeepFoldPairAccountsApart(t *testing.T) {
	t.Parallel()

	t.Run("owner-only email recipient", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			recipient any
			want      bool
		}{
			{sigmaOwner, true},
			{sigmaCapish, true},
			{[]any{" " + sigmaOwner + " "}, true},
			{sigmaFinal, false},
			{[]any{sigmaFinal}, false},
		} {
			if got := matchesExclusiveOwnerRecipient(c.recipient, sigmaOwner); got != c.want {
				t.Errorf("recipient %v for owner %q = %v, want %v", c.recipient, sigmaOwner, got, c.want)
			}
		}
		// A look-alike of an ASCII letter is not the ASCII account.
		if matchesExclusiveOwnerRecipient("Kate@example.com", "kate@example.com") {
			t.Error("KELVIN SIGN address read as kate@example.com")
		}
		if !matchesExclusiveOwnerRecipient("Kate@Example.com", "kate@example.com") {
			t.Error("the owner's own address in capitals was not the owner")
		}
	})

	t.Run("history entry author", func(t *testing.T) {
		t.Parallel()
		tc := agent.TextContent{Text: "hi", SenderEmail: sigmaFinal}
		if historyEntryAuthoredByCaller(tc, sigmaOwner, sigmaOwner) {
			t.Errorf("%q's message read as %q's", sigmaFinal, sigmaOwner)
		}
		if !historyEntryAuthoredByCaller(tc, sigmaFinal, sigmaOwner) {
			t.Errorf("%q's message did not read as their own", sigmaFinal)
		}
		if !historyEntryAuthoredByCaller(agent.TextContent{Text: "hi", SenderEmail: "Mate@Example.com"}, "mate@example.com", sigmaOwner) {
			t.Error("a sender stored with capitals did not read as the same account")
		}
		if historyEntryAuthoredByCaller(agent.TextContent{Text: "hi"}, sigmaFinal, sigmaOwner) {
			t.Errorf("the owner's message read as %q's", sigmaFinal)
		}
	})

	t.Run("conversation event approval fields", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"id":"c1","approval_mode":"autonomous","auto_approve":true}`)
		out, ok := sanitizeConversationEvent(data, sigmaFinal, sigmaOwner)
		if !ok || strings.Contains(string(out), "approval_mode") || strings.Contains(string(out), "auto_approve") {
			t.Errorf("%q received the owner's approval fields: %s", sigmaFinal, out)
		}
		for _, viewer := range []string{sigmaOwner, sigmaCapish} {
			if out, _ := sanitizeConversationEvent(data, viewer, sigmaOwner); string(out) != string(data) {
				t.Errorf("owner viewing as %q lost their approval fields: %s", viewer, out)
			}
		}
	})

	t.Run("live events of another sender", func(t *testing.T) {
		t.Parallel()
		toolCall := []byte(`{"name":"bash","user_email":"` + sigmaOwner + `"}`)
		if out, ok := sanitizeTurnEventForViewer("tool.call", toolCall, sigmaFinal, sigmaOwner); ok {
			t.Errorf("%q received %q's tool call: %s", sigmaFinal, sigmaOwner, out)
		}
		if out, ok := sanitizeTurnEventForViewer("tool.call", toolCall, sigmaOwner, sigmaOwner); !ok || strings.Contains(string(out), "user_email") {
			t.Errorf("the sender's own tool call = %s, %v", out, ok)
		}
		legacy := []byte(`{"name":"bash","user_email":"Mate@Example.com"}`)
		if _, ok := sanitizeTurnEventForViewer("tool.call", legacy, "mate@example.com", sigmaOwner); !ok {
			t.Error("a sender tagged with capitals did not receive their own tool call")
		}

		done := []byte(`{"user_email":"` + sigmaOwner + `","cost_usd":1.5}`)
		if out, _ := sanitizeTurnEventForViewer("turn.completed", done, sigmaFinal, sigmaOwner); string(out) != "{}" {
			t.Errorf("%q received %q's turn totals: %s", sigmaFinal, sigmaOwner, out)
		}
		if out, _ := sanitizeTurnEventForViewer("turn.completed", done, sigmaOwner, sigmaOwner); !strings.Contains(string(out), "cost_usd") {
			t.Errorf("the sender lost their own turn totals: %s", out)
		}

		msg := []byte(`{"text":"hi","sender_email":"` + sigmaOwner + `","injected_context":"private"}`)
		if out, _ := sanitizeTurnEventForViewer("user.message", msg, sigmaFinal, sigmaOwner); strings.Contains(string(out), "private") {
			t.Errorf("%q received %q's injected context: %s", sigmaFinal, sigmaOwner, out)
		}
		if out, _ := sanitizeTurnEventForViewer("user.message", msg, sigmaOwner, sigmaOwner); !strings.Contains(string(out), "private") {
			t.Errorf("the sender lost their own injected context: %s", out)
		}

		queue := []byte(`{"user_email":"` + sigmaOwner + `","items":[]}`)
		if out, ok := sanitizeQueueUpdatedEvent(queue, sigmaFinal); ok {
			t.Errorf("%q received %q's queue: %s", sigmaFinal, sigmaOwner, out)
		}
		mixed := []byte(`{"items":[{"id":"a","user_email":"` + sigmaOwner + `"},{"id":"b","user_email":"` + sigmaFinal + `"}]}`)
		out, ok := sanitizeQueueUpdatedEvent(mixed, sigmaFinal)
		if !ok || strings.Contains(string(out), `"a"`) || !strings.Contains(string(out), `"b"`) {
			t.Errorf("%q's view of a shared queue = %s, %v; want only item b", sigmaFinal, out, ok)
		}
	})

	t.Run("history of another sender", func(t *testing.T) {
		t.Parallel()
		ownerMsg, _ := json.Marshal(agent.TextContent{Text: "mine", SenderEmail: sigmaOwner})
		entries := []agent.HistoryEntry{
			{ID: 1, Role: "user", Type: "text", Content: ownerMsg, InjectedContext: "private"},
			{ID: 2, Role: "assistant", Type: "summary", Content: json.RawMessage(`{"text":"summary"}`)},
		}
		for _, e := range redactHistoryForSender(entries, sigmaFinal, sigmaOwner) {
			if e.InjectedContext != "" {
				t.Errorf("replay for %q kept %q's injected context", sigmaFinal, sigmaOwner)
			}
		}
		if got := redactHistoryForSender(entries, sigmaOwner, sigmaOwner); got[0].InjectedContext != "private" {
			t.Error("the sender's own replay lost their injected context")
		}
		other := historyForParticipant(entries, sigmaFinal, sigmaOwner)
		if other[0].InjectedContext != "" || other[1].Type != agent.EntryTypeSummaryBoundary {
			t.Errorf("%q's history view = %+v; want no injected context and the summary as a boundary", sigmaFinal, other)
		}
		own := historyForParticipant(entries, sigmaOwner, sigmaOwner)
		if own[0].InjectedContext != "private" || own[1].Type != "summary" {
			t.Errorf("the owner's history view = %+v; want every field", own)
		}
	})

	t.Run("live viewers after access changes", func(t *testing.T) {
		t.Parallel()
		b := newTurnBuffer("c1", "t1")
		b.mu.Lock()
		b.subscribers[1] = make(chan bufferedEvent, 1)
		b.subViewers[1] = sigmaFinal
		b.subscribers[2] = make(chan bufferedEvent, 1)
		b.subViewers[2] = sigmaOwner
		b.mu.Unlock()
		b.detachUnauthorizedViewers(sigmaCapish, map[string]bool{})
		b.mu.Lock()
		_, otherKept := b.subscribers[1]
		_, ownerKept := b.subscribers[2]
		b.mu.Unlock()
		if otherKept {
			t.Errorf("%q stayed attached as the owner %q", sigmaFinal, sigmaOwner)
		}
		if !ownerKept {
			t.Error("the owner was detached from their own chat")
		}
	})
}

// The session approval rules are filed under the account's key: a rule one
// account set is the same account's in other capitals, and never applies to
// another account whose address only case-folds (or looks) alike.
func TestSessionApprovalRulesAreFiledByAccount(t *testing.T) {
	t.Parallel()
	if sessionKey("c1", sigmaOwner, "bash") == sessionKey("c1", sigmaFinal, "bash") {
		t.Errorf("%q and %q share session approval rules", sigmaOwner, sigmaFinal)
	}
	if sessionKey("c1", "kate@example.com", "bash") == sessionKey("c1", "\u212aate@example.com", "bash") {
		t.Error("a KELVIN SIGN address shares kate@example.com's session approval rules")
	}
	if sessionKey("c1", sigmaOwner, "bash") != sessionKey("c1", " "+sigmaCapish, "bash") {
		t.Errorf("%q in capitals does not find its own session approval rules", sigmaOwner)
	}
}
