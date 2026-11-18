package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// POST /conversations/{id}/transfer: the owner (or an admin) hands a chat to
// another person. The store test pins what moves; this pins who may ask, what
// each refusal answers, and the reconciliation that follows.
func TestConversationTransferRoute(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())

	st := newScheduleFakeStore()
	st.users["gone@x.com"] = &store.User{Email: "gone@x.com", Enabled: false}
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	defer srv.StopBackground()
	h := srv.Routes()
	ctx := context.Background()

	const owner, next, mate = "jeanne@elcanotek.com", "client@reklaim.com", "mate@elcanotek.com"
	conv, err := st.CreateConversation(ctx, owner, "Opti workbench", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := st.SetConversationMultiplayer(ctx, owner, conv.ID, true, []string{mate}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	skills := filepath.Join(tools.WorkspaceDirForConversation(conv.ID), userSkillsRoot, "mine")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}

	post := func(reqCtx context.Context, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/transfer", bytes.NewBufferString(body))
		req = req.WithContext(reqCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A participant is not the owner: the same 404 as a chat that is not there.
	if rec := post(asUser(ctx, mate), `{"to_email":"`+next+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("participant transfer = %d, want 404", rec.Code)
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"to themselves":    {`{"to_email":"` + owner + `"}`, http.StatusBadRequest},
		"no target":        {`{}`, http.StatusBadRequest},
		"disabled account": {`{"to_email":"gone@x.com"}`, http.StatusBadRequest},
	} {
		if rec := post(asUser(ctx, owner), tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if c, _ := st.Get(ctx, owner, conv.ID); c == nil {
		t.Fatal("a refused transfer moved the chat")
	}

	assertTransferNeedsAnIdleChat(ctx, t, srv, st, conv.ID, owner, next, mate, post)

	// A message the owner submitted just before the transfer was admitted
	// while they still owned the chat, and has not registered its turn yet.
	stale, _ := st.Get(ctx, owner, conv.ID)
	admitted := &steerMailbox{store: st, user: owner, convID: conv.ID}
	refused := func(int, error) { t.Error("the owner's own submission was refused before any transfer") }
	if !srv.revalidateMultiplayerTurnAccess(ctx, owner, stale, admitted, nil, nil, refused) || !admitted.ownerChecked {
		t.Fatal("the owner's submission was not admitted with its ownership epoch")
	}
	// The new owner uploaded a file while they were a participant.
	stash, err := srv.privateWorkspaceStashDir(conv.ID)
	if err != nil {
		t.Fatalf("stash dir: %v", err)
	}
	theirs := filepath.Join(stash, "participants", participantDigest(next), uploadsDir)
	if err := os.MkdirAll(theirs, 0o755); err != nil {
		t.Fatalf("mkdir participant stash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(theirs, "brief.csv"), []byte("a,b\n"), 0o600); err != nil {
		t.Fatalf("write participant attachment: %v", err)
	}

	// A process that died during a participant's turn left the workspace
	// marked as swapped; the transfer puts it back before it moves anything.
	interrupted := filepath.Join(stash, "active-participant")
	if err := os.WriteFile(interrupted, []byte(mate), 0o600); err != nil {
		t.Fatalf("write the interrupted-swap marker: %v", err)
	}

	// The cleanup after the commit does not depend on the request: a client
	// that has gone away by then still gets the previous audience reconciled.
	gone, hangUp := context.WithCancel(asUser(ctx, owner))
	hangUp()
	rec := post(gone, `{"to_email":"`+next+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transfer = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OwnerEmail    string `json:"owner_email"`
		PreviousOwner string `json:"previous_owner"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.OwnerEmail != next || out.PreviousOwner != owner {
		t.Errorf("response = %s (err %v)", rec.Body.String(), err)
	}
	if c, _ := st.Get(ctx, next, conv.ID); c == nil || c.Multiplayer || c.TeamVisible {
		t.Errorf("the chat is not the new owner's private chat: %+v", c)
	}
	if _, err := os.Stat(filepath.Dir(skills)); !os.IsNotExist(err) {
		t.Errorf("the previous owner's skills are still in the workspace (err %v)", err)
	}
	// The submission admitted before the transfer cannot start a turn now,
	// and a new one from the previous owner is turned away.
	_, stopLate := context.WithCancel(ctx)
	defer stopLate()
	if _, _, _, ok, swept := srv.registerTurnGated(conv.ID, stopLate, admitted, nil, ""); ok || !swept {
		t.Errorf("a turn admitted before the transfer registered after it (ok=%v swept=%v)", ok, swept)
	}
	lateStatus := 0
	late := &steerMailbox{store: st, user: owner, convID: conv.ID}
	if srv.revalidateMultiplayerTurnAccess(ctx, owner, stale, late, nil, nil, func(code int, _ error) { lateStatus = code }) || lateStatus != http.StatusNotFound {
		t.Errorf("the previous owner's later submission was admitted (status %d), want a 404", lateStatus)
	}
	// The new owner's own submission is admitted and registers.
	fresh, _ := st.Get(ctx, next, conv.ID)
	mine := &steerMailbox{store: st, user: next, convID: conv.ID}
	if !srv.revalidateMultiplayerTurnAccess(ctx, next, fresh, mine, nil, nil, func(int, error) {}) {
		t.Error("the new owner's submission was refused")
	}
	if _, err := os.Stat(interrupted); !os.IsNotExist(err) {
		t.Errorf("the interrupted participant swap was not recovered before the transfer (err %v)", err)
	}
	// Their participant attachment is now in the workspace, where an
	// owner's attachments live.
	if _, err := os.Stat(filepath.Join(tools.WorkspaceDirForConversation(conv.ID), uploadsDir, "brief.csv")); err != nil {
		t.Errorf("the new owner's participant attachment was not moved into the workspace: %v", err)
	}
	// The previous owner no longer owns it, so asking again is a 404.
	if rec := post(asUser(ctx, owner), `{"to_email":"`+mate+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("previous owner's second transfer = %d, want 404", rec.Code)
	}
	// An admin may move any chat.
	if rec := post(asUserRole(ctx, "boss@elcanotek.com", store.RoleAdmin), `{"to_email":"`+mate+`"}`); rec.Code != http.StatusOK {
		t.Errorf("admin transfer = %d: %s", rec.Code, rec.Body.String())
	}
	if c, _ := st.Get(ctx, mate, conv.ID); c == nil {
		t.Error("the admin's transfer did not move the chat")
	}
}

// assertTransferNeedsAnIdleChat: a chat changes hands only when no turn is
// running in it and nothing holds its workspace, and while it does, no turn
// can register for it.
func assertTransferNeedsAnIdleChat(ctx context.Context, t *testing.T, srv *Server, st *scheduleFakeStore, convID, owner, next, mate string, post func(context.Context, string) *httptest.ResponseRecorder) {
	t.Helper()
	// While a turn is running the chat cannot change hands: the turn has the
	// workspace's private subtrees mid-swap.
	_, stopTurn := context.WithCancel(ctx)
	srv.inflightMu.Lock()
	srv.inflight[convID] = inflightEntry{buf: newTurnBuffer("t-running", ""), cancel: stopTurn}
	srv.inflightMu.Unlock()
	if rec := post(asUser(ctx, owner), `{"to_email":"`+next+`"}`); rec.Code != http.StatusConflict {
		t.Errorf("transfer during a running turn = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if c, _ := st.Get(ctx, owner, convID); c == nil {
		t.Fatal("a transfer refused for a running turn moved the chat")
	}
	srv.inflightMu.Lock()
	delete(srv.inflight, convID)
	srv.inflightMu.Unlock()
	stopTurn()

	// Nor while the workspace is still held: by a turn that has ended but
	// not yet put the workspace back, or by an approval being executed.
	transferWorkspaceWait = 50 * time.Millisecond
	t.Cleanup(func() { transferWorkspaceWait = 10 * time.Second })
	releaseWorkspace := srv.workspaceLocks.lock(convID)
	if rec := post(asUser(ctx, owner), `{"to_email":"`+next+`"}`); rec.Code != http.StatusConflict {
		t.Errorf("transfer while the workspace is held = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	releaseWorkspace()
	// And while a transfer is under way, no turn can register for the chat.
	endTransfer, idle := srv.beginTransfer(convID)
	if !idle {
		t.Fatal("an idle chat could not begin a transfer")
	}
	if _, again := srv.beginTransfer(convID); again {
		t.Error("a second transfer of the same chat began while the first was under way")
	}
	_, stopGated := context.WithCancel(ctx)
	if _, _, _, ok, swept := srv.registerTurnGated(convID, stopGated, &steerMailbox{store: st, user: mate, convID: convID}, nil, ""); ok || !swept {
		t.Errorf("a turn registered while the chat was changing hands (ok=%v swept=%v)", ok, swept)
	}
	stopGated()
	if !srv.transferInProgress(convID) {
		t.Error("a chat changing hands does not report a transfer in progress (decideApproval reads this)")
	}
	endTransfer()
	if srv.transferInProgress(convID) {
		t.Error("a finished transfer still reports as in progress")
	}

	// An approved action being executed is not idle either, and a refused
	// transfer leaves no mark behind.
	srv.executingApprovals.register(ctx, convID, owner, "ap-running")
	if _, idle := srv.beginTransfer(convID); idle {
		t.Error("a transfer began while an approved action was being executed")
	}
	if srv.transferInProgress(convID) {
		t.Error("a transfer refused for a running approval left the chat marked as changing hands")
	}
	srv.executingApprovals.unregister("ap-running")
}
