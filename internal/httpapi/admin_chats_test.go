package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

func adminChatsFixture(t *testing.T) (*Server, *store.Store, *patrolRecordingEngine, http.Handler) {
	t.Helper()
	srv := serverFixture(t)
	eng := &patrolRecordingEngine{}
	srv.agent = eng
	srv.isMember = nil
	st := srv.concreteStore(t)
	for _, u := range []string{"admin@x", "owner@x", "member@x"} {
		seedUser(t, st, u)
	}
	setRole(t, srv, "admin@x", store.RoleAdmin, "")
	return srv, st, eng, srv.Routes()
}

func adminDo(t *testing.T, h http.Handler, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req = req.WithContext(asUser(req.Context(), user))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A message `fleet chats send` recorded on the host becomes, on the server's
// next scheduler pass, a turn that runs as the chat's owner and records the
// administrator as the sender.
func TestAdminRequest_MessageRunsAsOwnerFromAdmin(t *testing.T) {
	srv, st, eng, _ := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateAdminRequest(ctx, store.AdminRequestInput{
		Actor: store.AdminActor{Email: "robbie@ops", Via: store.AdminViaCLI}, Kind: store.AdminRequestMessage,
		ConversationID: conv.ID, Payload: "Publish the team log now.", Reason: "seed the shared log",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.KickQueuedChatScheduleTurns(ctx) // the scheduler pass
	srv.DrainTurns(ctx)

	eng.mu.Lock()
	kind, msg, user, sender := eng.lastInputKind, eng.lastUserMessage, eng.lastUserEmail, eng.lastSenderEmail
	eng.mu.Unlock()
	if kind != agent.InputKindAdminMessage || user != "owner@x" || sender != "robbie@ops" {
		t.Fatalf("turn kind=%q user=%q sender=%q; want admin_message as owner@x from robbie@ops", kind, user, sender)
	}
	if !strings.Contains(msg, "[Message from an administrator: robbie@ops]") || !strings.Contains(msg, "Publish the team log now.") {
		t.Fatalf("turn message = %q", msg)
	}
	got, err := st.GetAdminRequest(ctx, req.ID)
	if err != nil || got.State != store.AdminRequestDone || got.InputState != store.InputStateCompleted {
		t.Fatalf("request = %+v, %v", got, err)
	}
	// A second pass finds nothing to do.
	srv.KickQueuedChatScheduleTurns(ctx)
	srv.DrainTurns(ctx)
	eng.mu.Lock()
	turns := eng.turns
	eng.mu.Unlock()
	if turns != 1 {
		t.Fatalf("turns = %d, want 1", turns)
	}
}

func TestAdminChatRoutes(t *testing.T) {
	srv, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := st.CreateChatSchedule(ctx, store.ChatScheduleInput{ConversationID: conv.ID, UserEmail: "owner@x", Title: "Sweep", Prompt: "Offer a memory card.", CronExpr: "30 6 * * 1-5"})
	if err != nil {
		t.Fatal(err)
	}

	// Members are refused every admin chat route.
	for _, path := range []string{"/admin/chats", "/admin/chats/" + conv.ID, "/admin/patrols/" + sc.ID, "/admin/chat-actions"} {
		if rec := adminDo(t, h, "member@x", http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("member GET %s = %d", path, rec.Code)
		}
	}

	rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats?owner=owner@x", "")
	var list struct {
		Chats []store.AdminChat `json:"chats"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Chats) != 1 || list.Chats[0].ActivePatrols != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	rec = adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats/"+conv.ID[:8], "")
	var detail struct {
		Chat    store.AdminChat           `json:"chat"`
		Patrols []store.AdminChatSchedule `json:"patrols"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil || detail.Chat.ID != conv.ID || len(detail.Patrols) != 1 {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}

	// Settings: a reason is required; Autonomous is refused.
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"title":"X"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch without reason = %d", rec.Code)
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"approval_mode":"autonomous","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("autonomous = %d", rec.Code)
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"title":"Workbench (team)","reason":"tidy"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPost, "/admin/chats/"+conv.ID+"/notice", `{"text":"Heads up."}`); rec.Code != http.StatusNoContent {
		t.Fatalf("notice = %d %s", rec.Code, rec.Body.String())
	}

	// A stop request is carried out at once.
	rec = adminDo(t, h, "admin@x", http.MethodPost, "/admin/chats/"+conv.ID+"/requests", `{"kind":"stop","reason":"runaway"}`)
	var reqResp struct {
		Request store.AdminRequest `json:"request"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &reqResp) != nil || reqResp.Request.State != store.AdminRequestDone {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chat-requests/"+reqResp.Request.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("request status = %d", rec.Code)
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPost, "/admin/chats/"+conv.ID+"/requests", `{"kind":"delete","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d", rec.Code)
	}

	rec = adminDo(t, h, "admin@x", http.MethodGet, "/admin/chat-actions", "")
	var acts struct {
		Actions []store.AdminAction `json:"actions"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &acts) != nil {
		t.Fatalf("actions = %d %s", rec.Code, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, a := range acts.Actions {
		if a.Actor != "admin@x" || a.Via != store.AdminViaWeb {
			t.Fatalf("action recorded as %s/%s", a.Actor, a.Via)
		}
		seen[a.Action] = true
	}
	for _, want := range []string{"update", "notice", "request_stop"} {
		if !seen[want] {
			t.Fatalf("actions %v missing %q", seen, want)
		}
	}

	rec = adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats/"+conv.ID+"/history?tail=10", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Heads up.") {
		t.Fatalf("history = %d %s", rec.Code, rec.Body.String())
	}
	_ = srv
}

func TestAdminPatrolRoutes(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := st.CreateChatSchedule(ctx, store.ChatScheduleInput{ConversationID: conv.ID, UserEmail: "owner@x", Title: "Sweep", Prompt: "Offer a memory card.", CronExpr: "30 6 * * 1-5"})
	if err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	if rec = adminDo(t, h, "member@x", http.MethodPatch, "/admin/patrols/"+sc.ID, `{"status":"paused","reason":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member patch = %d", rec.Code)
	}
	// Patrol prompt: compare-and-swap on the sha; status pause.
	rec = adminDo(t, h, "admin@x", http.MethodPatch, "/admin/patrols/"+sc.ID, `{"prompt":"Append to the team log.","expect_sha256":"`+store.PromptSHA256("other")+`","reason":"log moved"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale prompt edit = %d %s", rec.Code, rec.Body.String())
	}
	rec = adminDo(t, h, "admin@x", http.MethodPatch, "/admin/patrols/"+sc.ID, `{"prompt":"Append to the team log.","expect_sha256":"`+store.PromptSHA256(sc.Prompt)+`","reason":"log moved"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Append to the team log.") {
		t.Fatalf("prompt edit = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/patrols/"+sc.ID, `{"status":"paused","reason":"holiday"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body.String())
	}

	actions, err := st.ListAdminActions(ctx, conv.ID, 10)
	if err != nil || len(actions) != 2 || actions[0].Action != "patrol_pause" || actions[1].Action != "patrol_prompt" || actions[1].Actor != "admin@x" {
		t.Fatalf("actions = %+v, %v", actions, err)
	}
}

// Archiving from the admin page goes through the owner's archive path as a
// claimed request, and is recorded and noted.
func TestAdminChatArchive(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Old work", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"archived":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("archive without reason = %d", rec.Code)
	}
	rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"archived":true,"title":"Old work (done)","reason":"client finished"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"archived":true`) || !strings.Contains(rec.Body.String(), "Old work (done)") {
		t.Fatalf("archive = %d %s", rec.Code, rec.Body.String())
	}
	acts, _ := st.ListAdminActions(ctx, conv.ID, 10)
	kinds := map[string]bool{}
	for _, a := range acts {
		kinds[a.Action] = true
	}
	if !kinds["update"] || !kinds["request_archive"] {
		t.Fatalf("actions = %+v", acts)
	}
}

func TestUnattendedTurnKinds(t *testing.T) {
	for kind, want := range map[string]bool{
		agent.InputKindChatPatrol:     true,
		agent.InputKindAdminMessage:   true,
		agent.InputKindApprovalResume: false,
		"":                            false,
	} {
		if got := unattendedTurn(kind); got != want {
			t.Errorf("unattendedTurn(%q) = %t, want %t", kind, got, want)
		}
	}
}
