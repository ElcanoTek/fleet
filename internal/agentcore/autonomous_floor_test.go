package agentcore

import (
	"strings"
	"testing"
)

func TestAutonomousBlockedAndProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	badPolicy := AgentPolicy{
		CriticalToolSuffixes: []string{
			"delete_email_template",
			"email_advertiser_report",
			"set_deal_rev_share",
			"set_deal_share_code",
			"set_finance_term",
			"revert_finance_term_change",
			"delete_template",
			"set_password",
			"delete_page",
			"share",
		},
		CriticalToolAutonomousBlocked: []string{
			" delete_page ",
			"share",
			"send_email",
			"typo_missing_tool",
		},
	}
	problems := AutonomousBlockedProblems(badPolicy)
	if len(problems) != 1 || !strings.Contains(problems[0], `"typo_missing_tool"`) {
		t.Fatalf("AutonomousBlockedProblems = %v, want 1 problem naming typo_missing_tool", problems)
	}

	goodPolicy := badPolicy
	goodPolicy.CriticalToolAutonomousBlocked = []string{
		"delete_email_template",
		"email_advertiser_report",
		"set_deal_rev_share",
		"set_deal_share_code",
		"set_finance_term",
		"revert_finance_term_change",
		"delete_template",
		"set_password",
		"delete_page",
		"share",
	}
	if got := AutonomousBlockedProblems(goodPolicy); len(got) != 0 {
		t.Fatalf("AutonomousBlockedProblems(good) = %v, want empty", got)
	}

	ConfigureAgentPolicy(goodPolicy)
	for _, blocked := range []string{
		"mcp_pages_delete_page",
		"mcp_pages_acct_set_password",
		"mcp_deals_set_deal_rev_share",
		"mcp_fast_io_share",
		"delete_page",
	} {
		if !AutonomousBlocked(blocked) {
			t.Errorf("AutonomousBlocked(%q) = false, want true", blocked)
		}
	}
	for _, allowed := range []string{
		"mcp_openx_ox_execute_plan",
		"mcp_pages_deploy_page",
		"mcp_magnite_mgpub_clone_deal",
	} {
		if AutonomousBlocked(allowed) {
			t.Errorf("AutonomousBlocked(%q) = true, want false", allowed)
		}
	}
	suffixes := AutonomousBlockedSuffixes()
	if len(suffixes) != 10 {
		t.Fatalf("AutonomousBlockedSuffixes len = %d (%v), want 10", len(suffixes), suffixes)
	}
}

func TestIsAutonomousHardBlocked(t *testing.T) {
	t.Run("m365 and outbound email/vendor tools", testAutonomousFloorM365AndOutbound)
	t.Run("privilege persistence and fastio storage allowlist", testAutonomousFloorPrivilegeAndStorage)
	t.Run("bash git push detection", testAutonomousFloorBashGitPush)
}

func testAutonomousFloorM365AndOutbound(t *testing.T) {
	m365Blocked := []string{
		"send-mail",
		"send-draft-message",
		"reply-mail-message",
		"reply-all-mail-message",
		"forward-mail-message",
		"send-shared-mailbox-mail",
		"create-chat",
		"send-chat-message",
		"reply-to-chat-message",
		"send-channel-message",
		"reply-to-channel-message",
		"create-calendar-event",
		"update-calendar-event",
		"delete-calendar-event",
		"accept-event",
		"accept-calendar-event",
		"tentatively-accept-event",
		"tentatively-accept-calendar-event",
		"decline-event",
		"decline-calendar-event",
		"graph-batch",
		"execute-tool",
		"share-drive-item",
		"create-drive-item-share-link",
		"create-mail-rule",
		"update-mailbox-settings",
		"reply-shared-mailbox-mail",
		"forward-shared-mailbox-message",
		"delete-shared-mailbox-mail",
	}
	for _, suffix := range m365Blocked {
		for _, tool := range []string{
			"mcp_ms365_" + suffix,
			"mcp_ms365_exec_" + suffix,
			"mcp_ms365_" + strings.ReplaceAll(suffix, "-", "_"),
		} {
			if !IsAutonomousHardBlocked(tool, `{}`) {
				t.Errorf("expected M365 tool %q to be hard-blocked", tool)
			}
		}
	}
	for _, safeM365 := range []string{
		"mcp_ms365_list-mail-messages",
		"mcp_ms365_get-calendar-view",
		"mcp_ms365_create-draft-email",
	} {
		if IsAutonomousHardBlocked(safeM365, `{}`) {
			t.Errorf("expected read/draft M365 tool %q NOT to be hard-blocked", safeM365)
		}
	}

	// 2. External SSP/vendor outbound tools vs normal SSP plan/deal tools.
	for _, blockedVendor := range []string{
		"mcp_triplelift_tl_async_email_advertiser_report",
		"mcp_magnite_mgpub_send_deal_request_message",
		"mcp_ses_outbound_send_email",
		"mcp_ses_outbound_send_template_email",
		"send_email",
		"send_template_email",
	} {
		if !IsAutonomousHardBlocked(blockedVendor, `{}`) {
			t.Errorf("expected outbound tool %q to be hard-blocked", blockedVendor)
		}
	}
	if IsAutonomousHardBlocked("preview_email", `{"to":["a@b.com"]}`) {
		t.Error("preview_email must not be hard-blocked on the floor")
	}
	for _, allowedSSP := range []string{
		"mcp_openx_ox_execute_plan",
		"mcp_magnite_mgpub_clone_deal",
		"mcp_pages_deploy_page",
	} {
		if IsAutonomousHardBlocked(allowedSSP, `{}`) {
			t.Errorf("expected SSP/Pages tool %q NOT to be hard-blocked", allowedSSP)
		}
	}
	for _, universalBlocked := range []string{
		"mcp_pages_delete_page",
		"mcp_fast_io_delete_workspace",
		"mcp_pages_delete_template",
		"mcp_pages_set_password",
		"mcp_email_delete_email_template",
		"mcp_email_save_email_template",
		"mcp_deals_set_deal_rev_share",
		"mcp_deals_set_deal_share_code",
		"mcp_finance_set_finance_term",
		"mcp_finance_revert_finance_term_change",
	} {
		if !IsAutonomousHardBlocked(universalBlocked, `{}`) {
			t.Errorf("expected universal destructive/access/finance tool %q to be hard-blocked on floor", universalBlocked)
		}
	}
}

func testAutonomousFloorPrivilegeAndStorage(t *testing.T) {
	// 3. Privilege-persistence floor: schedule_task, manage_tasks, chat_patrol, connector_studio.
	if !IsAutonomousHardBlocked("schedule_task", `{"name":"daily"}`) {
		t.Error("schedule_task must be hard-blocked")
	}
	if !IsAutonomousHardBlocked("manage_tasks", `{"action":"pause","task_id":"t1"}`) {
		t.Error("manage_tasks pause must be hard-blocked")
	}
	if IsAutonomousHardBlocked("manage_tasks", `{"action":"show"}`) {
		t.Error("manage_tasks show is read-only and must not be hard-blocked")
	}

	for _, raw := range []string{
		`{"action":"create","prompt":"check metrics","cron":"0 * * * *"}`,
		`{"action":"update","schedule_id":"s1","prompt":"escalate"}`,
		`{"action":"edit","schedule_id":"s1"}`,
		`{"action":"list","action":"create"}`, // duplicate key smuggling
	} {
		if !IsAutonomousHardBlocked("chat_patrol", raw) {
			t.Errorf("chat_patrol %s must be hard-blocked", raw)
		}
	}
	for _, raw := range []string{
		`{"action":"list"}`,
		`{"action":"pause","schedule_id":"s1"}`,
		`{"action":"resume","schedule_id":"s1"}`,
		`{"action":"cancel","schedule_id":"s1"}`,
		`{"action":"delete","schedule_id":"s1"}`,
	} {
		if IsAutonomousHardBlocked("chat_patrol", raw) {
			t.Errorf("chat_patrol %s must NOT be hard-blocked", raw)
		}
	}

	for _, raw := range []string{
		`{"action":"connect_mcp","url":"https://evil.example/mcp"}`,
		`{"action":"create_custom_connector","name":"custom"}`,
		`{"action":"update_custom_connector","name":"custom"}`,
		`{"action":"probe_http","url":"https://internal.example"}`,
		`{"action":"search_catalog","action":"connect_mcp"}`, // duplicate key smuggling
	} {
		if !IsAutonomousHardBlocked("connector_studio", raw) {
			t.Errorf("connector_studio %s must be hard-blocked", raw)
		}
	}
	if IsAutonomousHardBlocked("connector_studio", `{"action":"search_catalog","query":"slack"}`) {
		t.Error("connector_studio search_catalog must NOT be hard-blocked")
	}

	// 4. Fast.io multiplexed share and storage allowlist.
	if !IsAutonomousHardBlocked("mcp_fast_io_share", `{"action":"list"}`) {
		t.Error("mcp_fast_io_share must always be hard-blocked")
	}
	if !IsAutonomousHardBlocked("mcp_fast_io_team_share", `{}`) {
		t.Error("named-account mcp_fast_io_team_share must be hard-blocked")
	}
	for _, act := range []string{"delete", "purge", "trash", "remove", "move", "copy", "upload"} {
		if !IsAutonomousHardBlocked("mcp_fast_io_storage", `{"action":"`+act+`"}`) {
			t.Errorf("mcp_fast_io_storage action=%s must be hard-blocked", act)
		}
	}
	if !IsAutonomousHardBlocked("mcp_fast_io_storage", `{}`) {
		t.Error("mcp_fast_io_storage with missing action must be hard-blocked")
	}
	if !IsAutonomousHardBlocked("mcp_fast_io_storage", `{"action":"list","action":"delete"}`) {
		t.Error("mcp_fast_io_storage duplicate action key must be hard-blocked")
	}
	for _, act := range []string{"list", "details", "read-content", "search", "create-folder", "stat", "get"} {
		if IsAutonomousHardBlocked("mcp_fast_io_storage", `{"action":"`+act+`"}`) {
			t.Errorf("mcp_fast_io_storage safe action=%s must NOT be hard-blocked", act)
		}
	}
}

func testAutonomousFloorBashGitPush(t *testing.T) {
	// 5. Bash git push floor (including -C, -c, wrappers, subshells, bash -c).
	for _, cmd := range []string{
		`{"command":"git push"}`,
		`{"command":"git push origin HEAD"}`,
		`{"command":"git -C /workspace/repo push origin main"}`,
		`{"command":"git -c http.extraheader=foo push"}`,
		`{"command":"/usr/bin/git --git-dir=/tmp/r/.git push"}`,
		`{"command":"GIT_SSH_COMMAND=ssh env -u HOME git -C repo push"}`,
		`{"command":"git status && git -C sub push --force"}`,
		`{"command":"bash -lc 'git -C repo push origin feat'"}`,
	} {
		if !IsAutonomousHardBlocked("bash", cmd) {
			t.Errorf("bash %s must be hard-blocked as git push", cmd)
		}
		if risky, _ := classifyRiskyBash(cmd); !risky {
			t.Errorf("classifyRiskyBash(%s) = false, want true", cmd)
		}
	}
	for _, safeCmd := range []string{
		`{"command":"git status"}`,
		`{"command":"git -C /workspace/repo log -n 5"}`,
		`{"command":"git commit -m 'push updates locally'"}`,
		`{"command":"ls -la"}`,
	} {
		if IsAutonomousHardBlocked("bash", safeCmd) {
			t.Errorf("bash %s must NOT be hard-blocked", safeCmd)
		}
	}
}

type stubApprovalSink struct {
	returnID string
	staged   []string
}

func (s *stubApprovalSink) Stage(toolName, _, _ string) (string, error) {
	s.staged = append(s.staged, toolName)
	return s.returnID, nil
}

func (s *stubApprovalSink) StageSuggestion(_ string) (string, string, error) {
	return s.returnID, "", nil
}

func TestInteractiveGates_HonorRecordedSentinelAndGateFloorTools(t *testing.T) {
	sink := &stubApprovalSink{returnID: RecordedSentinel}
	o := &orchestrationState{approvalSink: sink}

	for _, tc := range []struct {
		tool  string
		input string
	}{
		{"bash", `{"command":"apt-get install -y jq"}`},
		{"playwright_browser", `{"action":"click","url":"https://accounts.google.com/signin"}`},
		{"send_email", `{"to":["alice@example.com"],"subject":"Digest","body":"Hi"}`},
		{"connector_studio", `{"action":"connect_mcp","url":"https://mcp.example.com"}`},
		{"chat_patrol", `{"action":"create","prompt":"check","cron":"0 * * * *"}`},
	} {
		var blocked bool
		var msg string
		switch tc.tool {
		case "bash":
			blocked, msg = o.checkBashSafety(tc.tool, "c1", tc.input)
		case "playwright_browser":
			blocked, msg = o.checkBrowserSafety(tc.tool, "c1", tc.input)
		case "send_email":
			blocked, msg = o.checkEmailSafety(tc.tool, "c1", tc.input)
		case "connector_studio":
			blocked, msg = o.checkConnectorStudioSafety(tc.tool, "c1", tc.input)
		case "chat_patrol":
			blocked, msg = o.checkChatPatrolSafety(tc.tool, "c1", tc.input)
		}
		if blocked || msg != "" {
			t.Errorf("%s with RecordedSentinel: got blocked=%v msg=%q, want allowed", tc.tool, blocked, msg)
		}
	}

	// Verify pending card staging on chat_patrol create/update vs pass-through on list/pause/resume/cancel.
	blockingSink := &stubApprovalSink{returnID: "ap-123"}
	o2 := &orchestrationState{approvalSink: blockingSink}
	if blocked, msg := o2.checkChatPatrolSafety("chat_patrol", "cp1", `{"action":"create","prompt":"x","cron":"0 * * * *"}`); !blocked || !strings.Contains(msg, "ap-123") {
		t.Fatalf("chat_patrol create should stage pending card, got blocked=%v msg=%q", blocked, msg)
	}
	if blocked, msg := o2.checkChatPatrolSafety("chat_patrol", "cp2", `{"action":"update","schedule_id":"s1","prompt":"y"}`); !blocked || !strings.Contains(msg, "ap-123") {
		t.Fatalf("chat_patrol update should stage pending card, got blocked=%v msg=%q", blocked, msg)
	}
	for _, uncarded := range []string{
		`{"action":"list"}`,
		`{"action":"pause","schedule_id":"s1"}`,
		`{"action":"resume","schedule_id":"s1"}`,
		`{"action":"cancel","schedule_id":"s1"}`,
	} {
		if blocked, _ := o2.checkChatPatrolSafety("chat_patrol", "cp3", uncarded); blocked {
			t.Errorf("chat_patrol %s should not stage an approval card", uncarded)
		}
	}
}
