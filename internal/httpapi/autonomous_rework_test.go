package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

type patrolRecordStageStore struct {
	recordStageStore
}

func (s *patrolRecordStageStore) ArmApprovalResume(context.Context, string, string) (bool, error) {
	return true, nil
}

func TestAutonomousStage_FloorBlockedSessionDenyAndDynamicRemote(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{
			"execute_plan",
			"create_record",
			"delete_resource",
			"remote_mutate",
			"remote_nosa",
		},
		CriticalToolNoSessionApproval: []string{
			"execute_plan",
			"create_record",
			"delete_resource",
			"remote_nosa",
		},
		CriticalToolAutonomousBlocked: []string{
			"delete_resource",
		},
	})

	st := &patrolRecordStageStore{}
	sink := &eventSink{}
	reg := NewSessionApprovalRegistry()
	reg.RegisterForUser("c1", "alice@example.com", "mcp_beta_create_record", SessionApprovalPolicy{
		Pattern: "",
		Mode:    "deny",
	})

	destructiveTrue := true
	a := &approvalStager{
		ctx:              context.Background(),
		store:            st,
		conversationID:   "c1",
		userEmail:        "alice@example.com",
		sink:             sink,
		sessionRegistry:  reg,
		convAutoApprove:  true,
		convApprovalMode: store.ConversationApprovalAutonomous,
		liveApprovalMode: func(context.Context) string { return store.ConversationApprovalAutonomous },
		mcpCatalog: []mcp.ServerTool{
			{ServerName: "user_remote", Tool: mcp.Tool{Name: "remote_mutate"}},
			{ServerName: "user_remote", Tool: mcp.Tool{Name: "remote_nosa"}},
			{ServerName: "user_remote", Tool: mcp.Tool{Name: "remote_destroy", Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructiveTrue}}},
		},
		dynamicRemoteServers: map[string]bool{
			"user_remote": true,
		},
	}

	// 1. Default-allow critical_tool_no_session_approval tool -> RecordedSentinel.
	got, err := a.Stage("mcp_alpha_execute_plan", "call_1", `{"plan_id":"p1"}`)
	if err != nil || got != agentcore.RecordedSentinel {
		t.Fatalf("execute_plan = (%q, %v), want RecordedSentinel", got, err)
	}

	// 2. Explicit session deny overrides Autonomous mode -> PreDeniedSentinel.
	got, err = a.Stage("mcp_beta_create_record", "call_deny", `{"name":"x"}`)
	if err != nil || got != agentcore.PreDeniedSentinel {
		t.Fatalf("session-denied tool = (%q, %v), want PreDeniedSentinel", got, err)
	}

	// 3. Bundle critical_tool_autonomous_blocked stages a pending card.
	got, err = a.Stage("mcp_gamma_delete_resource", "call_blk", `{"slug":"home"}`)
	if err != nil || got == agentcore.RecordedSentinel || got == agentcore.PreApprovedSentinel {
		t.Fatalf("blocked delete_resource = (%q, %v), want pending card ID", got, err)
	}

	// 4. Engine Hard-Block Floor tools stage a pending card even in Autonomous mode.
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{"m365 send-mail", "mcp_ms365_send-mail", `{"to":"ext@example.com"}`},
		{"m365 graph-batch", "mcp_ms365_graph-batch", `{"requests":[]}`},
		{"m365 execute-tool", "mcp_ms365_execute-tool", `{"tool":"send-mail"}`},
		{"email template save", "mcp_email_save_email_template", `{}`},
		{"email template delete", "mcp_email_delete_email_template", `{}`},
		{"schedule_task", "schedule_task", `{"name":"job"}`},
		{"manage_tasks pause", "manage_tasks", `{"action":"pause","task_id":"t1"}`},
		{"chat_patrol create", "chat_patrol", `{"action":"create","prompt":"hi","cron":"0 * * * *"}`},
		{"chat_patrol update", "chat_patrol", `{"action":"update","schedule_id":"s1","prompt":"hi"}`},
		{"connector_studio connect", "connector_studio", `{"action":"connect_mcp","url":"https://x.test"}`},
		{"fast_io share", "mcp_fast_io_share", `{"action":"create"}`},
		{"fast_io storage delete", "mcp_fast_io_storage", `{"action":"delete","path":"/x"}`},
		{"bash git -C push", "bash", `{"command":"git -C /workspace/repo push origin main"}`},
		{"bash if git push", "bash", `{"command":"if git push origin main; then :; fi"}`},
	} {
		id, err := a.Stage(tc.tool, "call_"+tc.name, tc.args)
		if err != nil {
			t.Fatalf("%s: Stage error: %v", tc.name, err)
		}
		if id == agentcore.RecordedSentinel || id == agentcore.PreApprovedSentinel {
			t.Errorf("%s (%s) bypassed floor in Autonomous mode: got %q", tc.name, tc.tool, id)
		}
	}

	// 5. Non-git-push risky bash (e.g. apt-get install) stays lightweight PreApprovedSentinel.
	got, err = a.Stage("bash", "call_apt", `{"command":"apt-get install -y jq"}`)
	if err != nil || got != agentcore.PreApprovedSentinel {
		t.Fatalf("non-push bash = (%q, %v), want PreApprovedSentinel", got, err)
	}

	// 6. User-connected dynamic remote MCP server keeps Auto-mode gating:
	//    - non-no_session_approval tool pre-approves (PreApprovedSentinel),
	//    - no_session_approval / destructive tool stages a blocking card!
	got, err = a.Stage("mcp_user_remote_remote_mutate", "call_rem_ok", `{}`)
	if err != nil || got != agentcore.PreApprovedSentinel {
		t.Fatalf("dynamic remote session-allowed tool = (%q, %v), want PreApprovedSentinel", got, err)
	}
	got, err = a.Stage("mcp_user_remote_remote_nosa", "call_rem_nosa", `{}`)
	if err != nil || got == agentcore.RecordedSentinel || got == agentcore.PreApprovedSentinel {
		t.Fatalf("dynamic remote no_session_approval tool = (%q, %v), want pending card ID", got, err)
	}
	got, err = a.Stage("mcp_user_remote_remote_destroy", "call_rem_dest", `{}`)
	if err != nil || got == agentcore.RecordedSentinel || got == agentcore.PreApprovedSentinel {
		t.Fatalf("dynamic remote destructiveHint tool = (%q, %v), want pending card ID", got, err)
	}
}

func TestIsSelfAddressedOwnerEmail_InteractiveAndPatrolWithContentFile(t *testing.T) {
	wsDir := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsDir)
	convID := "conv_email_self_test"
	convDir := tools.WorkspaceDirForConversation(convID)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(convDir) })
	if err := os.WriteFile(filepath.Join(convDir, "digest.html"), []byte("<p>Daily owner digest</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "report.csv"), []byte("deal,spend\nd1,100\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, isPatrol := range []bool{false, true} {
		st := &patrolRecordStageStore{}
		sink := &eventSink{}
		inputKind := ""
		if isPatrol {
			inputKind = agent.InputKindChatPatrol
		}
		a := &approvalStager{
			ctx:              context.Background(),
			store:            st,
			conversationID:   convID,
			userEmail:        "Alice@Example.com",
			sink:             sink,
			inputKind:        inputKind,
			convApprovalMode: store.ConversationApprovalAutonomous,
			liveApprovalMode: func(context.Context) string { return store.ConversationApprovalAutonomous },
		}

		// Self-addressed with content_file + attachments succeeds in BOTH interactive and patrol Autonomous turns.
		selfArgs := `{
			"to": ["alice@example.com"],
			"subject": "Morning Digest",
			"content_file": "digest.html",
			"attachments": [{"filename": "report.csv", "path": "report.csv"}]
		}`
		got, err := a.Stage("mcp_ses_outbound_send_email", "call_self", selfArgs)
		if err != nil || got != agentcore.RecordedSentinel {
			t.Fatalf("isPatrol=%v self email with content_file+attachments = (%q, %v), want RecordedSentinel", isPatrol, got, err)
		}

		// Self-addressed template email via to_email also succeeds.
		tplArgs := `{"to_email": "ALICE@example.com", "template_id": "daily-summary", "variables": {"k": "v"}}`
		got, err = a.Stage("mcp_ses_outbound_send_template_email", "call_tpl", tplArgs)
		if err != nil || got != agentcore.RecordedSentinel {
			t.Fatalf("isPatrol=%v self template email = (%q, %v), want RecordedSentinel", isPatrol, got, err)
		}

		// Rejections: external recipient, mixed recipients, cc, bcc, reply_to, custom from, duplicate keys, unexpected key.
		rejectCases := []struct {
			name string
			tool string
			args string
		}{
			{"external to", "mcp_ses_outbound_send_email", `{"to":["bob@example.com"],"subject":"Hi","content":"<p>x</p>"}`},
			{"mixed self and external", "mcp_ses_outbound_send_email", `{"to":["alice@example.com","bob@example.com"],"subject":"Hi","content":"<p>x</p>"}`},
			{"with cc", "mcp_ses_outbound_send_email", `{"to":["alice@example.com"],"cc":["bob@example.com"],"subject":"Hi","content":"<p>x</p>"}`},
			{"with bcc", "mcp_ses_outbound_send_email", `{"to":["alice@example.com"],"bcc":["bob@example.com"],"subject":"Hi","content":"<p>x</p>"}`},
			{"with reply_to", "mcp_ses_outbound_send_email", `{"to":["alice@example.com"],"reply_to":"bob@example.com","subject":"Hi","content":"<p>x</p>"}`},
			{"with custom from", "mcp_ses_outbound_send_email", `{"to":["alice@example.com"],"from":"spoof@example.com","subject":"Hi","content":"<p>x</p>"}`},
			{"duplicate case-folded to key", "mcp_ses_outbound_send_email", `{"to":["bob@example.com"],"TO":["alice@example.com"],"subject":"Hi","content":"<p>x</p>"}`},
			{"unexpected personalizations key", "mcp_ses_outbound_send_email", `{"to":["alice@example.com"],"personalizations":[{"to":"bob@example.com"}],"subject":"Hi","content":"<p>x</p>"}`},
		}
		for _, rc := range rejectCases {
			id, err := a.Stage(rc.tool, "call_"+rc.name, rc.args)
			if err != nil {
				t.Fatalf("isPatrol=%v %s: unexpected error: %v", isPatrol, rc.name, err)
			}
			if id == agentcore.RecordedSentinel || id == agentcore.PreApprovedSentinel {
				t.Errorf("isPatrol=%v %s bypassed approval card: got %q", isPatrol, rc.name, id)
			}
		}
	}
}

func TestEnableMCPConnectors_AutonomousInlineAndSessionDeny(t *testing.T) {
	t.Parallel()

	st := newEnableMCPTestStore()
	conv, err := st.CreateConversation(context.Background(), "alice@example.com", "Connectors", "victoria", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	conv.ApprovalMode = store.ConversationApprovalAutonomous
	conv.AutoApprove = true

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "alpha_mcp",
				DisplayName: "Alpha",
				ToolCount:   4,
				Tools:       []string{"alpha_list_records"},
			},
		},
	}
	srv := newDefaultChatServer(t, engine, st)
	defer srv.StopBackground()
	buf := newTurnBuffer("turn-1", "")
	reg := NewSessionApprovalRegistry()
	stager := &approvalStager{
		ctx:              context.Background(),
		store:            st,
		conversationID:   conv.ID,
		userEmail:        "alice@example.com",
		sink:             buf,
		sessionRegistry:  reg,
		convApprovalMode: store.ConversationApprovalAutonomous,
		liveApprovalMode: func(context.Context) string { return store.ConversationApprovalAutonomous },
	}

	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager)
	res, err := handler(context.Background(), "call_conn_1", tools.EnableMCPConnectorsParams{
		Connectors: []string{"alpha_mcp"},
		Reason:     "Need Alpha reporting",
	})
	if err != nil {
		t.Fatalf("Autonomous enable_mcp_connectors error: %v", err)
	}
	if res.Status != "enabled" || !res.AutoApproved || len(res.Connectors) != 1 || res.Connectors[0] != "alpha_mcp" {
		t.Fatalf("unexpected enable_mcp_connectors result: %+v", res)
	}

	// Verify explicit session deny blocks enable_mcp_connectors even in Autonomous mode.
	reg.RegisterForUser(conv.ID, "alice@example.com", tools.EnableMCPConnectorsToolName, SessionApprovalPolicy{
		Pattern: "",
		Mode:    "deny",
	})
	engine.catalog = append(engine.catalog, agent.OptionalServerInfo{
		Name:        "beta_mcp",
		DisplayName: "Beta",
		ToolCount:   2,
		Tools:       []string{"beta_list"},
	})
	_, err = handler(context.Background(), "call_conn_2", tools.EnableMCPConnectorsParams{
		Connectors: []string{"beta_mcp"},
		Reason:     "Blocked by session policy",
	})
	if err == nil || !strings.Contains(err.Error(), "APPROVAL_DENIED") {
		t.Fatalf("expected session deny error on enable_mcp_connectors, got %v", err)
	}
}

func TestAutonomousRiskAcceptanceGate_AllEndpoints(t *testing.T) {
	t.Parallel()

	st := newScheduleFakeStore()
	conv, err := st.CreateConversation(context.Background(), "alice@example.com", "Gate test", "default", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	srv.chatSchedules = &fakeChatScheduleEngine{st: st, srv: srv}
	srv.cfg = &config.Config{}
	defer srv.StopBackground()
	h := srv.Routes()

	doJSON := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(asUser(req.Context(), "alice@example.com"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. Before acceptance, GET /server-config reports autonomous_accepted = false.
	cfgRec := doJSON(http.MethodGet, "/server-config", "")
	if cfgRec.Code != http.StatusOK || !strings.Contains(cfgRec.Body.String(), `"autonomous_accepted":false`) {
		t.Fatalf("GET /server-config = %d %s", cfgRec.Code, cfgRec.Body.String())
	}

	// 2. Setting approval_mode = "autonomous" returns 409 autonomous_acceptance_required across all 4 surfaces.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"auto-approve set", http.MethodPost, "/conversations/" + conv.ID + "/auto-approve", `{"approval_mode":"autonomous","auto_approve":true}`},
		{"conversation create", http.MethodPost, "/conversations", `{"title":"New","approval_mode":"autonomous","auto_approve":true}`},
		{"chat turn post", http.MethodPost, "/chat", `{"message":"hi","approval_mode":"autonomous","auto_approve":true}`},
		{"schedule create", http.MethodPost, "/conversations/" + conv.ID + "/schedules", `{"title":"Patrol","prompt":"check","cron":"0 * * * *","approval_mode":"autonomous","auto_approve":true}`},
	} {
		rec := doJSON(tc.method, tc.path, tc.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"autonomous_acceptance_required"`) {
			t.Fatalf("%s before accept: got %d %s, want 409 autonomous_acceptance_required", tc.name, rec.Code, rec.Body.String())
		}
	}

	// 3. POST /chat/autonomous-accept records acceptance of CurrentAutonomousPolicyVersion.
	accRec := doJSON(http.MethodPost, "/chat/autonomous-accept", `{}`)
	if accRec.Code != http.StatusOK || !strings.Contains(accRec.Body.String(), `"autonomous_accepted":true`) {
		t.Fatalf("POST /chat/autonomous-accept = %d %s", accRec.Code, accRec.Body.String())
	}

	// 4. After acceptance, switching conversation and creating Autonomous conversations/schedules succeed.
	setRec := doJSON(http.MethodPost, "/conversations/"+conv.ID+"/auto-approve", `{"approval_mode":"autonomous","auto_approve":true}`)
	if setRec.Code != http.StatusOK && setRec.Code != http.StatusNoContent {
		t.Fatalf("POST /auto-approve after accept = %d %s", setRec.Code, setRec.Body.String())
	}
	createRec := doJSON(http.MethodPost, "/conversations", `{"title":"Auton Chat","approval_mode":"autonomous","auto_approve":true}`)
	if createRec.Code != http.StatusOK && createRec.Code != http.StatusCreated {
		t.Fatalf("POST /conversations after accept = %d %s", createRec.Code, createRec.Body.String())
	}
	schedRec := doJSON(http.MethodPost, "/conversations/"+conv.ID+"/schedules", `{"title":"Patrol","prompt":"check","cron":"0 * * * *","approval_mode":"autonomous","auto_approve":true}`)
	if schedRec.Code != http.StatusOK && schedRec.Code != http.StatusCreated {
		t.Fatalf("POST /schedules after accept = %d %s", schedRec.Code, schedRec.Body.String())
	}
}

func TestUIAutoSubmit_ArmPauseCancelAndSweeper(t *testing.T) {
	t.Parallel()

	st := newScheduleFakeStore()
	ctx := context.Background()
	_ = st.AcceptAutonomousPolicy(ctx, "alice@example.com", store.CurrentAutonomousPolicyVersion)

	conv, err := st.CreateConversation(ctx, "alice@example.com", "AutoSubmit Chat", "default", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetApprovalMode(ctx, "alice@example.com", conv.ID, store.ConversationApprovalAutonomous)
	updatedConv, _ := st.Get(ctx, "alice@example.com", conv.ID)

	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	srv.cfg = &config.Config{}
	defer srv.StopBackground()
	h := srv.Routes()

	buf := newTurnBuffer("turn-auto", "")
	stager := &approvalStager{
		ctx:              ctx,
		store:            st,
		conversationID:   conv.ID,
		userEmail:        "alice@example.com",
		sink:             buf,
		convApprovalMode: store.ConversationApprovalAutonomous,
		liveApprovalMode: func(context.Context) string { return store.ConversationApprovalAutonomous },
	}

	eligibleSpec := []byte(`{
		"title": "Confirm Deal Clone",
		"components": [
			{"type": "text_input", "id": "deal_name", "label": "Name", "required": true, "value": "Cloned Deal"}
		],
		"actions": [
			{"id": "clone", "label": "Clone deal"}
		]
	}`)

	// 1. InputKindChatPatrol NEVER arms a ui_autosubmits row.
	patrolHook := srv.newShowUIDisplayedHandler(updatedConv, "alice@example.com", agent.InputKindChatPatrol, "", buf, stager, nil)
	patrolHook(ctx, "call_patrol_ui", eligibleSpec)
	if active, _ := st.ListActiveUIAutoSubmits(ctx, conv.ID); len(active) != 0 {
		t.Fatalf("expected 0 active autosubmits on patrol turn, got %v", active)
	}

	// 2. Owner interactive Autonomous turn arms a pending 10s row.
	ownerHook := srv.newShowUIDisplayedHandler(updatedConv, "alice@example.com", "", "Plan deal for Acme", buf, stager, nil)
	ownerHook(ctx, "call_ui_1", eligibleSpec)
	active, _ := st.ListActiveUIAutoSubmits(ctx, conv.ID)
	if len(active) != 1 || active[0].CallID != "call_ui_1" || active[0].Status != store.UIAutoSubmitStatusPending {
		t.Fatalf("expected pending call_ui_1 autosubmit, got %v", active)
	}

	// 2b. A display-only show_ui card (no submit actions and replaces == "") must NOT cancel call_ui_1!
	displayOnlySpec := []byte(`{
		"title": "Summary Metrics",
		"components": [
			{"type": "text", "content": "Estimated reach: 1.2M"}
		],
		"actions": [
			{"id": "copy_txt", "label": "Copy", "kind": "copy", "message": "1.2M"}
		]
	}`)
	ownerHook(ctx, "call_display_only", displayOnlySpec)
	active, _ = st.ListActiveUIAutoSubmits(ctx, conv.ID)
	if len(active) != 1 || active[0].CallID != "call_ui_1" {
		t.Fatalf("display-only card cancelled call_ui_1 countdown: %v", active)
	}

	// 3. A replacement card naming `replaces: "call_ui_1"` cancels call_ui_1 and arms call_ui_2.
	replaceSpec := []byte(`{
		"title": "Confirm Deal Clone v2",
		"replaces": "call_ui_1",
		"components": [
			{"type": "text_input", "id": "deal_name", "label": "Name", "required": true, "value": "Cloned Deal v2"}
		],
		"actions": [
			{"id": "clone", "label": "Clone deal"}
		]
	}`)
	ownerHook(ctx, "call_ui_2", replaceSpec)
	active, _ = st.ListActiveUIAutoSubmits(ctx, conv.ID)
	if len(active) != 1 || active[0].CallID != "call_ui_2" {
		t.Fatalf("expected only call_ui_2 active after replaces, got %v", active)
	}

	// 4. POST /conversations/{id}/autosubmit/pause transitions call_ui_2 to paused.
	pauseReq := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/autosubmit/pause", bytes.NewBufferString(`{"call_id":"call_ui_2"}`))
	pauseReq.Header.Set("Content-Type", "application/json")
	pauseReq = pauseReq.WithContext(asUser(pauseReq.Context(), "alice@example.com"))
	pauseRec := httptest.NewRecorder()
	h.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusOK || !strings.Contains(pauseRec.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause endpoint = %d %s", pauseRec.Code, pauseRec.Body.String())
	}

	// 5. Arm call_ui_3 with an already-due deadline and run SweepDueUIAutoSubmits:
	//    verifies single-firer enqueue with auto=1 marker and per-row UUID client_input_id.
	pastMs := time.Now().UnixMilli() - 500
	dueRow3, err := st.CreateUIAutoSubmit(ctx, store.UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_ui_3",
		ActionID:       "clone",
		ValuesJSON:     `{"deal_name":"Cloned Deal v3"}`,
		DeadlineMs:     pastMs,
		Status:         store.UIAutoSubmitStatusPending,
		CreatedAtMs:    pastMs - 10_000,
		UpdatedAtMs:    pastMs - 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	fired, err := srv.SweepDueUIAutoSubmits(ctx)
	if err != nil || fired != 1 {
		t.Fatalf("SweepDueUIAutoSubmits = (%d, %v), want (1, nil)", fired, err)
	}
	// Second sweep is a no-op (already fired).
	firedAgain, err := srv.SweepDueUIAutoSubmits(ctx)
	if err != nil || firedAgain != 0 {
		t.Fatalf("second SweepDueUIAutoSubmits = (%d, %v), want (0, nil)", firedAgain, err)
	}
	// Pausing call_ui_3 AFTER it already fired returns status="fired" (never falsely paused!).
	latePauseReq := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/autosubmit/pause", bytes.NewBufferString(`{"call_id":"call_ui_3"}`))
	latePauseReq.Header.Set("Content-Type", "application/json")
	latePauseReq = latePauseReq.WithContext(asUser(latePauseReq.Context(), "alice@example.com"))
	latePauseRec := httptest.NewRecorder()
	h.ServeHTTP(latePauseRec, latePauseReq)
	if latePauseRec.Code != http.StatusOK || !strings.Contains(latePauseRec.Body.String(), `"status":"fired"`) {
		t.Fatalf("late pause after fire = %d %s, want status=fired", latePauseRec.Code, latePauseRec.Body.String())
	}

	queued, _ := st.ListQueuedInputs(ctx, "alice@example.com", conv.ID)
	foundAuto := false
	wantPrefix := "[UI submission] card=call_ui_3 action=clone auto=1\n"
	wantClientID := store.UIAutoSubmitClientInputPrefix + dueRow3.ID
	for _, q := range queued {
		if q.ClientInputID == wantClientID && strings.HasPrefix(q.Message, wantPrefix) {
			foundAuto = true
		}
	}
	if !foundAuto {
		t.Fatalf("expected queued auto-submit row (%s), got %+v", wantClientID, queued)
	}

	// 6. Downgrading approval_mode out of Autonomous cancels BOTH pending AND paused
	//    ui_autosubmits rows AND purges any still-queued autosubmit:* row!
	dueWhileBusy, err := st.CreateUIAutoSubmit(ctx, store.UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_ui_queued",
		ActionID:       "clone",
		ValuesJSON:     `{"deal_name":"Queued While Busy"}`,
		DeadlineMs:     pastMs,
		Status:         store.UIAutoSubmitStatusPending,
		CreatedAtMs:    pastMs - 10_000,
		UpdatedAtMs:    pastMs - 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Sweep at the store layer (representing the exact window after sweeper tx commit before drain claims it).
	if sweptBusy, serr := st.SweepAndEnqueueDueUIAutoSubmits(ctx, time.Now().UnixMilli(), 10, 20); serr != nil || len(sweptBusy) != 1 || !sweptBusy[0].Enqueued {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits = (%+v, %v), want 1 enqueued", sweptBusy, serr)
	}
	ownerHook(ctx, "call_ui_4", eligibleSpec)
	downReq := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/auto-approve", bytes.NewBufferString(`{"approval_mode":"manual","auto_approve":false}`))
	downReq.Header.Set("Content-Type", "application/json")
	downReq = downReq.WithContext(asUser(downReq.Context(), "alice@example.com"))
	downRec := httptest.NewRecorder()
	h.ServeHTTP(downRec, downReq)
	if downRec.Code != http.StatusOK && downRec.Code != http.StatusNoContent {
		t.Fatalf("downgrade = %d %s", downRec.Code, downRec.Body.String())
	}
	if activeAfterDown, _ := st.ListActiveUIAutoSubmits(ctx, conv.ID); len(activeAfterDown) != 0 {
		t.Fatalf("expected BOTH pending and paused autosubmits cancelled on downgrade, active = %+v", activeAfterDown)
	}
	stillQueuedClientID := store.UIAutoSubmitClientInputPrefix + dueWhileBusy.ID
	for _, q := range func() []store.InputQueueRow {
		rows, _ := st.ListQueuedInputs(ctx, "alice@example.com", conv.ID)
		return rows
	}() {
		if q.ClientInputID == stillQueuedClientID {
			t.Fatalf("expected still-queued autosubmit row %s cancelled on downgrade, got %+v", stillQueuedClientID, q)
		}
	}

	// 7. Chain cap (maxAutoSubmitChain = 2): 2 consecutive auto=1 turns are allowed
	//    (intake_card -> plan_table), 3rd consecutive auto=1 is refused, and a new
	//    human message resets the chain!
	verifyUIAutoSubmitChainCap(ctx, t, st, srv, conv.ID, eligibleSpec, buf, stager)
}

func verifyUIAutoSubmitChainCap(
	ctx context.Context,
	t *testing.T,
	st *scheduleFakeStore,
	srv *Server,
	convID string,
	eligibleSpec []byte,
	buf *turnBuffer,
	stager *approvalStager,
) {
	t.Helper()
	_ = st.SetApprovalMode(ctx, "alice@example.com", convID, store.ConversationApprovalAutonomous)
	updatedConv, _ := st.Get(ctx, "alice@example.com", convID)
	appendUserMsg := func(text string) {
		raw, _ := json.Marshal(agent.TextContent{Text: text})
		_, _ = st.CommitTurnHistory(ctx, convID, "t-chain", []agent.HistoryEntry{{
			Role:    "user",
			Type:    "text",
			Content: raw,
		}})
	}
	appendUserMsg("Build deal plan")
	// Simulate real RunTurn order: CommitUser ALREADY persisted auto=1 #1 before show_ui runs!
	auto1Msg1 := "[UI submission] card=c1 action=go auto=1\n```json\n{}\n```"
	appendUserMsg(auto1Msg1)
	committedTrue := func() bool { return true }
	hookStep1 := srv.newShowUIDisplayedHandler(updatedConv, "alice@example.com", "", auto1Msg1, buf, stager, committedTrue)
	hookStep1(ctx, "call_chain_2", eligibleSpec)
	if activeChain1, _ := st.ListActiveUIAutoSubmits(ctx, convID); len(activeChain1) != 1 {
		t.Fatalf("expected step 1 auto=1 turn (with CommitUser persisted) to arm step 2 card, got %v", activeChain1)
	}

	// Verify replaces also cancels an already-swept/fired queued autosubmit:* row!
	st.mu.Lock()
	for i := range st.uiAutoSubmits {
		if st.uiAutoSubmits[i].CallID == "call_chain_2" {
			st.uiAutoSubmits[i].DeadlineMs = time.Now().UnixMilli() - 10
		}
	}
	st.mu.Unlock()
	if swept, serr := st.SweepAndEnqueueDueUIAutoSubmits(ctx, time.Now().UnixMilli(), 10, 20); serr != nil || len(swept) != 1 || !swept[0].Enqueued {
		t.Fatalf("SweepAndEnqueueDueUIAutoSubmits(call_chain_2) = (%+v, %v), want 1 enqueued", swept, serr)
	}
	hasQueuedChain2 := func() bool {
		qItems, _ := st.ListQueuedInputs(ctx, "alice@example.com", convID)
		for _, qi := range qItems {
			if strings.Contains(qi.Message, "card=call_chain_2 ") {
				return true
			}
		}
		return false
	}
	if !hasQueuedChain2() {
		t.Fatalf("expected queued autosubmit input for call_chain_2 before replaces")
	}
	replaceQueuedSpec := []byte(`{
		"title": "Replaced Card",
		"replaces": "call_chain_2",
		"auto_submit": false,
		"components": [{"type": "text_input", "id": "deal_name", "label": "Name", "required": true, "value": "Updated"}],
		"actions": [{"id": "clone", "label": "Clone deal"}]
	}`)
	hookStep1(ctx, "call_chain_2_replaced", replaceQueuedSpec)
	if hasQueuedChain2() {
		t.Fatalf("expected replaces card to cancel queued autosubmit input for call_chain_2")
	}

	// Auto=1 step 2 (CommitUser persists auto=1 #2 -> depth=2 >= maxAutoSubmitChain -> REFUSED!).
	auto1Msg2 := "[UI submission] card=c2 action=go auto=1\n```json\n{}\n```"
	appendUserMsg(auto1Msg2)
	hookStep2 := srv.newShowUIDisplayedHandler(updatedConv, "alice@example.com", "", auto1Msg2, buf, stager, committedTrue)
	hookStep2(ctx, "call_chain_3_blocked", eligibleSpec)
	if activeChain2, _ := st.ListActiveUIAutoSubmits(ctx, convID); len(activeChain2) != 0 {
		t.Fatalf("expected 3rd consecutive auto=1 card to be blocked by maxAutoSubmitChain=2, got %v", activeChain2)
	}

	// Human message resets chain depth -> arms again!
	humanFollowup := "Looks good, clone for Q1 too"
	appendUserMsg(humanFollowup)
	hookAfterHuman := srv.newShowUIDisplayedHandler(updatedConv, "alice@example.com", "", humanFollowup, buf, stager, committedTrue)
	hookAfterHuman(ctx, "call_chain_reset", eligibleSpec)
	if activeReset, _ := st.ListActiveUIAutoSubmits(ctx, convID); len(activeReset) != 1 || activeReset[0].CallID != "call_chain_reset" {
		t.Fatalf("expected human message to reset chain cap, got %v", activeReset)
	}
	srv.cancelConversationAutoSubmits(ctx, convID)

	// Refuses to arm behind an already-queued human follow-up input!
	_, _, _ = st.EnqueueInput(ctx, store.InputQueueRow{
		ID:             "q-user-followup",
		ConversationID: convID,
		UserEmail:      "alice@example.com",
		ClientInputID:  "cli-user-followup-1",
		Message:        "Wait, change budget to 50k first",
		Attachments:    "[]",
		Mode:           store.InputModeQueued,
	})
	hookAfterHuman(ctx, "call_blocked_behind_queued_input", eligibleSpec)
	if activeBehindQueue, _ := st.ListActiveUIAutoSubmits(ctx, convID); len(activeBehindQueue) != 0 {
		t.Fatalf("expected autosubmit NOT to arm behind queued human follow-up, got %v", activeBehindQueue)
	}
}

func TestNewTurnStager_ClampsAutonomousWithoutPolicyAcceptance(t *testing.T) {
	t.Parallel()

	st := newScheduleFakeStore()
	ctx := context.Background()
	// Note: bob@example.com has NOT accepted CurrentAutonomousPolicyVersion!
	conv, err := st.CreateConversation(ctx, "bob@example.com", "Legacy Auton", "default", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetApprovalMode(ctx, "bob@example.com", conv.ID, store.ConversationApprovalAutonomous)
	legacyConv, _ := st.Get(ctx, "bob@example.com", conv.ID)

	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	srv.cfg = &config.Config{}
	defer srv.StopBackground()

	for _, kind := range []string{"", agent.InputKindChatPatrol} {
		buf := newTurnBuffer("t-clamp", kind)
		stager := srv.newTurnStager(ctx, legacyConv, "bob@example.com", buf)
		if stager.isAutonomous() {
			t.Fatalf("inputKind=%q: unaccepted autonomous DB mode was NOT clamped (got autonomous)", kind)
		}
		if got := stager.effectiveApprovalMode(); got != store.ConversationApprovalAuto {
			t.Fatalf("inputKind=%q: effectiveApprovalMode = %q, want %q", kind, got, store.ConversationApprovalAuto)
		}
	}

	// Once bob accepts CurrentAutonomousPolicyVersion, newTurnStager honors autonomous!
	_ = st.AcceptAutonomousPolicy(ctx, "bob@example.com", store.CurrentAutonomousPolicyVersion)
	buf := newTurnBuffer("t-accepted", "")
	stager := srv.newTurnStager(ctx, legacyConv, "bob@example.com", buf)
	if !stager.isAutonomous() {
		t.Fatal("expected stager.isAutonomous() == true after AcceptAutonomousPolicy")
	}
}

func TestConversationGet_ExposesUIAutoSubmitsAndAcceptance(t *testing.T) {
	t.Parallel()

	st := newScheduleFakeStore()
	ctx := context.Background()
	_ = st.AcceptAutonomousPolicy(ctx, "alice@example.com", store.CurrentAutonomousPolicyVersion)
	conv, err := st.CreateConversation(ctx, "alice@example.com", "Hydrate", "default", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	nowMs := time.Now().UnixMilli()
	_, _ = st.CreateUIAutoSubmit(ctx, store.UIAutoSubmit{
		ConversationID: conv.ID,
		UserEmail:      "alice@example.com",
		CallID:         "call_hydrate",
		ActionID:       "submit",
		ValuesJSON:     `{"ok":true}`,
		DeadlineMs:     nowMs + 8_000,
		Status:         store.UIAutoSubmitStatusPending,
		CreatedAtMs:    nowMs,
		UpdatedAtMs:    nowMs,
	})

	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	srv.cfg = &config.Config{}
	defer srv.StopBackground()
	req := httptest.NewRequest(http.MethodGet, "/conversations/"+conv.ID, nil)
	req = req.WithContext(asUser(req.Context(), "alice@example.com"))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /conversations/%s = %d %s", conv.ID, rec.Code, rec.Body.String())
	}
	var payload struct {
		AutonomousAccepted      bool                 `json:"autonomous_accepted"`
		AutonomousPolicyVersion int                  `json:"autonomous_policy_version"`
		UIAutoSubmits           []store.UIAutoSubmit `json:"ui_autosubmits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AutonomousAccepted || payload.AutonomousPolicyVersion != store.CurrentAutonomousPolicyVersion {
		t.Fatalf("unexpected acceptance fields: %+v", payload)
	}
	if len(payload.UIAutoSubmits) != 1 || payload.UIAutoSubmits[0].CallID != "call_hydrate" {
		t.Fatalf("unexpected ui_autosubmits: %+v", payload.UIAutoSubmits)
	}
}
