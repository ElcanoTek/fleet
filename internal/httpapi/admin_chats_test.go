package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	kind, msg, user, sender, admin := eng.lastInputKind, eng.lastUserMessage, eng.lastUserEmail, eng.lastSenderEmail, eng.lastAdminEmail
	eng.mu.Unlock()
	// The turn is the owner's (no participant sender, so the owner sees its
	// work); the administrator is recorded separately.
	if kind != agent.InputKindAdminMessage || user != "owner@x" || sender != "" || admin != "robbie@ops" {
		t.Fatalf("turn kind=%q user=%q sender=%q admin=%q; want admin_message as owner@x, admin robbie@ops", kind, user, sender, admin)
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
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, "/admin/chats/"+conv.ID, `{"approval_mode":"auto","reason":"r"}`); rec.Code != http.StatusBadRequest {
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
	if rec := adminDo(t, h, "admin@x", http.MethodPost, "/admin/chats/"+conv.ID+"/requests", `{"kind":"explode","reason":"r"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d", rec.Code)
	}
	// The store's own validation refusals answer 400 with their text; only
	// a store failure is an internal error.
	for _, tc := range []struct{ method, path, body, want string }{
		{http.MethodPatch, "/admin/chats/" + conv.ID, `{"title":"  ","reason":"r"}`, "title cannot be empty"},
		{http.MethodPatch, "/admin/chats/" + conv.ID, `{"model":"not a slug","reason":"r"}`, `"not a slug" is not a model slug`},
		{http.MethodPost, "/admin/chats/" + conv.ID + "/requests", `{"kind":"message","reason":"r"}`, "the message is empty"},
		{http.MethodPost, "/admin/chats/" + conv.ID + "/requests", `{"kind":"transfer","reason":"r"}`, "the new owner's email is required"},
	} {
		rec := adminDo(t, h, "admin@x", tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Errorf("%s %s %s = %d %q, want 400 %q", tc.method, tc.path, tc.body, rec.Code, rec.Body.String(), tc.want)
		}
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

// An administrator changes a patrol's schedule: a dry run previews the next
// runs and changes nothing; the change is compare-and-swap on def_version,
// takes a reason, is recorded and noted; members are refused.
func TestAdminPatrolScheduleRoute(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := st.CreateChatSchedule(ctx, store.ChatScheduleInput{ConversationID: conv.ID, UserEmail: "owner@x", Title: "Sweep", Prompt: "Scan.", CronExpr: "30 6 * * 1-5", Timezone: "America/New_York"})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := st.AdminGetChatSchedule(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/patrols/" + sc.ID
	v := strconv.FormatInt(cur.DefVersion, 10)
	everyDay := `{"cron":"30 6 * * *","expect_def_version":` + v + `,"reason":"weekends too"}`

	if rec := adminDo(t, h, "member@x", http.MethodPatch, path, everyDay); rec.Code != http.StatusForbidden {
		t.Fatalf("member schedule edit = %d", rec.Code)
	}
	for _, c := range []struct {
		name, body string
		code       int
	}{
		{"stale def_version", `{"cron":"30 6 * * *","expect_def_version":` + strconv.FormatInt(cur.DefVersion+1, 10) + `,"reason":"r"}`, http.StatusConflict},
		{"no def_version", `{"cron":"30 6 * * *","reason":"r"}`, http.StatusBadRequest},
		{"no reason", `{"cron":"30 6 * * *","expect_def_version":` + v + `}`, http.StatusBadRequest},
		{"invalid cron", `{"cron":"30 6 * *","expect_def_version":` + v + `,"reason":"r"}`, http.StatusBadRequest},
		{"invalid timezone", `{"timezone":"Nowhere/Land","expect_def_version":` + v + `,"reason":"r"}`, http.StatusBadRequest},
		{"too frequent", `{"cron":"* * * * *","expect_def_version":` + v + `,"reason":"r"}`, http.StatusBadRequest},
		{"schedule and prompt at once", `{"cron":"30 6 * * *","prompt":"x","expect_sha256":"` + cur.PromptSHA256 + `","expect_def_version":` + v + `,"reason":"r"}`, http.StatusBadRequest},
		{"schedule and status at once", `{"cron":"30 6 * * *","status":"paused","expect_def_version":` + v + `,"reason":"r"}`, http.StatusBadRequest},
		{"dry run without a schedule", `{"status":"paused","dry_run":true,"reason":"r"}`, http.StatusBadRequest},
	} {
		if rec := adminDo(t, h, "admin@x", http.MethodPatch, path, c.body); rec.Code != c.code {
			t.Fatalf("%s = %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.code)
		}
	}

	// A dry run: no reason needed, the next runs in the patrol's zone, nothing changed.
	rec := adminDo(t, h, "admin@x", http.MethodPatch, path, `{"cron":"30 6 * * *","expect_def_version":`+v+`,"dry_run":true}`)
	var dry struct {
		Patrol   store.AdminChatSchedule `json:"patrol"`
		Preview  store.AdminChatSchedule `json:"preview"`
		Changed  bool                    `json:"changed"`
		NextRuns []int64                 `json:"next_runs"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dry) != nil || !dry.Changed || dry.Patrol.CronExpr != "30 6 * * 1-5" ||
		dry.Preview.CronExpr != "30 6 * * *" || len(dry.NextRuns) != store.AdminCronPreviewRuns {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := st.AdminGetChatSchedule(ctx, sc.ID); got.CronExpr != sc.CronExpr || got.DefVersion != cur.DefVersion {
		t.Fatalf("a dry run changed the patrol: %+v", got)
	}

	rec = adminDo(t, h, "admin@x", http.MethodPatch, path, everyDay)
	var done struct {
		Patrol   store.AdminChatSchedule `json:"patrol"`
		NextRuns []int64                 `json:"next_runs"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &done) != nil || done.Patrol.CronExpr != "30 6 * * *" ||
		done.Patrol.DefVersion != cur.DefVersion+1 || done.Patrol.Prompt != sc.Prompt || done.Patrol.Status != store.ChatScheduleStatusActive ||
		len(done.NextRuns) == 0 || done.NextRuns[0] != done.Patrol.NextRunAt {
		t.Fatalf("schedule edit = %d %s", rec.Code, rec.Body.String())
	}
	// Sent again, the old def_version is stale.
	if rec := adminDo(t, h, "admin@x", http.MethodPatch, path, everyDay); rec.Code != http.StatusConflict {
		t.Fatalf("replayed schedule edit = %d %s", rec.Code, rec.Body.String())
	}
	actions, err := st.ListAdminActions(ctx, conv.ID, 10)
	if err != nil || len(actions) != 1 || actions[0].Action != "patrol_schedule" || actions[0].Actor != "admin@x" || actions[0].Via != store.AdminViaWeb || actions[0].Reason != "weekends too" {
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

// The owner sees everything an administrator's message did in their chat: the
// admin is recorded as admin_email, not as a participant sender, so the
// per-participant filter treats the turn as the owner's.
func TestOwnerSeesAdminMessageTurnWork(t *testing.T) {
	mk := func(role, typ string, c any) agent.HistoryEntry {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return agent.HistoryEntry{Role: role, Type: typ, Content: b}
	}
	hist := []agent.HistoryEntry{
		mk("user", "text", agent.TextContent{Text: "[Message from an administrator: admin@x]\npublish", Kind: agent.InputKindAdminMessage, AdminEmail: "admin@x"}),
		mk("assistant", "tool_call", agent.ToolCallContent{ID: "1", Name: "run_python", Input: "{}"}),
		mk("tool", "tool_result", agent.ToolResultContent{ID: "1", Name: "run_python", Text: "ok"}),
		mk("assistant", "text", agent.TextContent{Text: "Done."}),
		mk("system", agent.EntryTypeNotice, agent.NoticeContent{Kind: store.NoticeAdminUpdated, Text: "An administrator renamed the chat."}),
	}
	got := historyForParticipant(hist, "owner@x", "owner@x")
	if len(got) != len(hist) {
		t.Fatalf("the owner sees %d of %d entries of an admin-message turn", len(got), len(hist))
	}
}

// An administrator's message always runs in Manual approvals, whatever the
// chat's mode, and does not ride the owner's session approvals.
func TestAdminMessageTurnIsManual(t *testing.T) {
	a := &approvalStager{convApprovalMode: store.ConversationApprovalAutonomous, convAutoApprove: true}
	if a.effectiveApprovalMode() != store.ConversationApprovalAutonomous {
		t.Fatal("fixture: an owner turn should be autonomous")
	}
	a.inputKind = agent.InputKindAdminMessage
	if a.effectiveApprovalMode() != store.ConversationApprovalManual || a.isAutoApprove() || a.isAutonomous() {
		t.Fatalf("an admin-message turn ran in %s", a.effectiveApprovalMode())
	}
	a.liveApprovalMode = func(context.Context) string { return store.ConversationApprovalAutonomous }
	if a.effectiveApprovalMode() != store.ConversationApprovalManual {
		t.Fatal("the live approval mode overrode Manual on an admin-message turn")
	}
}

func TestAdminChatDeleteRestorePurge(t *testing.T) {
	_, st, _, h := adminChatsFixture(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "owner@x", "Kartik fork", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) *httptest.ResponseRecorder {
		return adminDo(t, h, "admin@x", http.MethodPost, "/admin/chats/"+conv.ID+"/requests", body)
	}
	if rec := post(`{"kind":"delete","reason":"forked by mistake"}`); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"state":"done"`) {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats", ""); strings.Contains(rec.Body.String(), conv.ID) {
		t.Fatal("a deleted chat is still listed")
	}
	if rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats?deleted=1", ""); !strings.Contains(rec.Body.String(), conv.ID) || !strings.Contains(rec.Body.String(), "purges_at") {
		t.Fatalf("recycle bin = %s", rec.Body.String())
	}
	if rec := post(`{"kind":"restore","reason":"wrong chat"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.AdminGetChat(ctx, conv.ID); err != nil {
		t.Fatalf("restored chat: %v", err)
	}
	if rec := post(`{"kind":"purge","reason":"disk full"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge without confirmation = %d", rec.Code)
	}
	if rec := post(`{"kind":"purge","reason":"disk full","confirm":"wrong title"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge with the wrong title = %d", rec.Code)
	}
	if rec := post(`{"kind":"purge","reason":"disk full","confirm":"Kartik fork"}`); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "deleted permanently") {
		t.Fatalf("purge = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminDo(t, h, "admin@x", http.MethodGet, "/admin/chats/"+conv.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("purged chat GET = %d", rec.Code)
	}
	if rec := adminDo(t, h, "member@x", http.MethodPost, "/admin/chats/"+conv.ID+"/requests", `{"kind":"delete","reason":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member delete = %d", rec.Code)
	}
}
