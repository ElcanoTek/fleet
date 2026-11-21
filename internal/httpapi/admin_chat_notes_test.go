package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// convNotePayload is what the pinned-note tests read from GET
// /conversations/{id}.
type convNotePayload struct {
	AdminNote *store.AdminNote  `json:"admin_note"`
	History   []json.RawMessage `json:"history"`
}

// pinNote pins a note as admin@x and returns it.
func pinNote(t *testing.T, h http.Handler, convID, text string) store.AdminNote {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	rec := adminDo(t, h, "admin@x", http.MethodPut, "/admin/chats/"+convID+"/note", string(body))
	var set struct {
		Note store.AdminNote `json:"note"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &set) != nil || set.Note.Author != "admin@x" || set.Note.Via != store.AdminViaWeb {
		t.Fatalf("pin = %d %s", rec.Code, rec.Body.String())
	}
	return set.Note
}

// Only an administrator can pin, replace or remove a note or read the
// overview; the administrator's view of the chat carries the note.
func TestAdminNoteRoutes_AdminOnly(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Weekly report", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	notePath := "/admin/chats/" + conv.ID + "/note"

	// Members cannot pin, replace or remove a note, or read the overview.
	for _, c := range [][2]string{{http.MethodPut, notePath}, {http.MethodDelete, notePath}, {http.MethodGet, "/admin/chat-overview"}} {
		if rec := adminDo(t, h, "member@x", c[0], c[1], `{"text":"hi"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s = %d", c[0], c[1], rec.Code)
		}
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPut, notePath, `{"text":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty note = %d %s", rec.Code, rec.Body.String())
	}

	pinNote(t, h, conv.ID, "I fixed the patrol. Reply here if it fails again.")

	// The administrator's view of the chat carries the note.
	rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats/"+conv.ID, "")
	var detail struct {
		Chat         store.AdminChat          `json:"chat"`
		Note         *store.AdminNote         `json:"note"`
		PendingCards []store.AdminPendingCard `json:"pending_cards"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil || detail.Note == nil || !detail.Chat.HasNote || detail.PendingCards == nil {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
}

// The chat's people are shown the note with the chat and acknowledge it;
// nobody else can; and it is no part of the chat's history.
func TestAdminNote_ShownToTheChatAndAcknowledged(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Weekly report", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	note := pinNote(t, h, conv.ID, "I fixed the patrol. Reply here if it fails again.")

	// The owner is shown it with the chat...
	rec := adminDo(t, h, "owner@x", http.MethodGet, "/conversations/"+conv.ID, "")
	var got convNotePayload
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.AdminNote == nil || got.AdminNote.Text != note.Text {
		t.Fatalf("owner's chat = %d %s", rec.Code, rec.Body.String())
	}
	// ...and it is no part of the history the model's context is built from.
	for _, e := range got.History {
		if strings.Contains(string(e), "I fixed the patrol") {
			t.Fatalf("the note is in the chat's history: %s", e)
		}
	}
	hist, err := st.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range hist {
		if strings.Contains(string(e.Content), "I fixed the patrol") {
			t.Fatalf("the note is in the stored history: %s", e.Content)
		}
	}

	// An open, idle chat re-reads the note on its own route; a stranger cannot.
	rec = adminDo(t, h, "owner@x", http.MethodGet, "/conversations/"+conv.ID+"/admin-note", "")
	var reread struct {
		AdminNote *store.AdminNote `json:"admin_note"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reread) != nil || reread.AdminNote == nil || reread.AdminNote.Text != note.Text {
		t.Fatalf("owner re-read = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "member@x", http.MethodGet, "/conversations/"+conv.ID+"/admin-note", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger re-read = %d %s", rec.Code, rec.Body.String())
	}

	// A draft made from an earlier version of the note is refused.
	stale := `{"text":"Stale draft.","expect_version":` + jsonInt(note.Version-1) + `}`
	if rec := adminDo(t, h, "admin@x", http.MethodPut, "/admin/chats/"+conv.ID+"/note", stale); rec.Code != http.StatusConflict {
		t.Fatalf("stale replace = %d %s", rec.Code, rec.Body.String())
	}

	// Someone who cannot open the chat cannot acknowledge its note.
	ack := `{"version":` + jsonInt(note.Version) + `}`
	if rec := adminDo(t, h, "member@x", http.MethodPost, "/conversations/"+conv.ID+"/admin-note", ack); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger ack = %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := st.GetAdminNote(ctx, conv.ID); n == nil || n.SeenAt != nil {
		t.Fatalf("note after a refused ack = %+v", n)
	}

	// The owner presses "Got it".
	rec = adminDo(t, h, "owner@x", http.MethodPost, "/conversations/"+conv.ID+"/admin-note", ack)
	var acked struct {
		AdminNote *store.AdminNote `json:"admin_note"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &acked) != nil || acked.AdminNote == nil || acked.AdminNote.SeenAt == nil || acked.AdminNote.SeenBy != "owner@x" {
		t.Fatalf("owner ack = %d %s", rec.Code, rec.Body.String())
	}
}

// The listing filters on a pinned note and the overview counts it; removing
// the note takes it away from the owner's chat; both changes are recorded.
func TestAdminNote_ListedCountedAndRemoved(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Weekly report", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	notePath := "/admin/chats/" + conv.ID + "/note"
	note := pinNote(t, h, conv.ID, "Welcome.")
	if _, err := st.AckAdminNote(ctx, conv.ID, "owner@x", note.Version); err != nil {
		t.Fatal(err)
	}

	rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats?state=noted", "")
	var list struct {
		Chats []store.AdminChat `json:"chats"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Chats) != 1 || !list.Chats[0].NoteSeen {
		t.Fatalf("noted list = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats?state=nonsense", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d", rec.Code)
	}
	rec = adminDo(t, h, "admin@x", http.MethodGet, "/admin/chat-overview", "")
	var ov struct {
		Overview store.AdminChatOverview `json:"overview"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ov) != nil || ov.Overview.Chats != 1 || ov.Overview.Noted != 1 || len(ov.Overview.People) != 1 || ov.Overview.People[0].Email != "owner@x" {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}

	// An unpin confirmed against an earlier version removes nothing.
	if rec := adminDo(t, h, "admin@x", http.MethodDelete, notePath+"?expect_version="+jsonInt(note.Version-1), ""); rec.Code != http.StatusConflict {
		t.Fatalf("stale unpin = %d %s", rec.Code, rec.Body.String())
	}
	// Removing it takes it away from the owner's chat.
	if rec := adminDo(t, h, "admin@x", http.MethodDelete, notePath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unpin = %d %s", rec.Code, rec.Body.String())
	}
	rec = adminDo(t, h, "owner@x", http.MethodGet, "/conversations/"+conv.ID, "")
	var got convNotePayload
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.AdminNote != nil {
		t.Fatalf("owner's chat after unpin = %d, note %+v", rec.Code, got.AdminNote)
	}
	actions, err := st.ListAdminActions(ctx, conv.ID, 10)
	if err != nil || len(actions) != 2 || actions[0].Action != "note_clear" || actions[1].Action != "note_set" || actions[1].Actor != "admin@x" {
		t.Fatalf("recorded = %+v, %v", actions, err)
	}
}

// The transcript pages back with ?before= and bounds ?max_chars=.
func TestAdminChatHistoryPaging(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Long", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three"} {
		if err := st.AdminAppendNotice(ctx, store.AdminActor{Email: "admin@x", Via: store.AdminViaWeb}, conv.ID, text); err != nil {
			t.Fatal(err)
		}
	}
	read := func(query string) []store.AdminTranscriptEntry {
		t.Helper()
		rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats/"+conv.ID+"/history"+query, "")
		var body struct {
			Entries []store.AdminTranscriptEntry `json:"entries"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("history%s = %d %s", query, rec.Code, rec.Body.String())
		}
		return body.Entries
	}
	latest := read("?tail=2")
	if len(latest) != 2 || latest[1].Text != "three" {
		t.Fatalf("latest = %+v", latest)
	}
	older := read("?tail=2&before=" + jsonInt(latest[0].ID))
	if len(older) != 1 || older[0].Text != "one" {
		t.Fatalf("older = %+v", older)
	}
	if cut := read("?tail=1&max_chars=2"); len(cut) != 1 || !cut[0].Truncated {
		t.Fatalf("max_chars = %+v", cut)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
