package agentcore

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutonomousBlockedAndProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	badPolicy := AgentPolicy{
		CriticalToolSuffixes: []string{
			"delete_email_template",
			"send_external_notice",
			"set_contract_rate",
			"set_share_code",
			"set_billing_term",
			"revert_billing_term_change",
			"delete_template",
			"set_password",
			"delete_resource",
			"share",
		},
		CriticalToolAutonomousBlocked: []string{
			" delete_resource ",
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
		"send_external_notice",
		"set_contract_rate",
		"set_share_code",
		"set_billing_term",
		"revert_billing_term_change",
		"delete_template",
		"set_password",
		"delete_resource",
		"share",
	}
	if got := AutonomousBlockedProblems(goodPolicy); len(got) != 0 {
		t.Fatalf("AutonomousBlockedProblems(good) = %v, want empty", got)
	}

	ConfigureAgentPolicy(goodPolicy)
	for _, blocked := range []string{
		"mcp_alpha_delete_resource",
		"mcp_alpha_acct_set_password",
		"mcp_beta_set_contract_rate",
		"mcp_fast_io_share",
		"delete_resource",
	} {
		if !AutonomousBlocked(blocked) {
			t.Errorf("AutonomousBlocked(%q) = false, want true", blocked)
		}
	}
	for _, allowed := range []string{
		"mcp_alpha_execute_plan",
		"mcp_alpha_deploy_resource",
		"mcp_beta_clone_record",
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

	// 2. Generic outbound email & template tools vs bundle-configured MCP tools.
	for _, blockedEmail := range []string{
		"mcp_ses_outbound_send_email",
		"mcp_ses_outbound_send_template_email",
		"send_email",
		"send_template_email",
		"mcp_email_delete_email_template",
		"mcp_email_save_email_template",
	} {
		if !IsAutonomousHardBlocked(blockedEmail, `{}`) {
			t.Errorf("expected outbound/template email tool %q to be hard-blocked", blockedEmail)
		}
	}
	if IsAutonomousHardBlocked("preview_email", `{"to":["a@b.com"]}`) {
		t.Error("preview_email must not be hard-blocked on the floor")
	}
	for _, bundleManaged := range []string{
		"mcp_alpha_execute_plan",
		"mcp_beta_clone_record",
		"mcp_gamma_deploy_resource",
		"mcp_gamma_delete_resource",
		"mcp_delta_send_external_notice",
	} {
		if IsAutonomousHardBlocked(bundleManaged, `{}`) {
			t.Errorf("expected bundle-managed MCP tool %q NOT to be hardcoded on engine floor", bundleManaged)
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
	for _, act := range []string{
		"delete", "purge", "trash", "remove", "move", "copy", "upload",
		// Read-sounding but not reads of the content: each hands out a link.
		"preview-url", "preview-transform",
		// Not an action the tool has: unknown stays gated.
		"list-workspaces", "metadata-set", "version-restore", "lock",
	} {
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
	for _, act := range []string{
		"list", "details", "read-content", "search", "create-folder", "stat", "get",
		"describe", "recent", "content", "version-list", "trash-list", "lock-status",
		"metadata-facts", "metadata-get", "metadata-versions",
		" Version-List ", // spelling is normalized, as before
	} {
		if IsAutonomousHardBlocked("mcp_fast_io_storage", `{"action":"`+act+`"}`) {
			t.Errorf("mcp_fast_io_storage safe action=%s must NOT be hard-blocked", act)
		}
	}
}

func testAutonomousFloorBashGitPush(t *testing.T) {
	// 5. Bash git push floor (including -C, -c, wrappers, command/param substitutions, pipes, heredocs).
	for _, cmd := range []string{
		`{"command":"git push"}`,
		`{"command":"git push origin HEAD"}`,
		`{"command":"git -C /workspace/repo push origin main"}`,
		`{"command":"git -c http.extraheader=foo push"}`,
		`{"command":"/usr/bin/git --git-dir=/tmp/r/.git push"}`,
		`{"command":"/usr/lib/git-core/git-push origin main"}`,
		`{"command":"GIT_SSH_COMMAND=ssh env -u HOME git -C repo push"}`,
		`{"command":"env -S 'git push origin main'"}`,
		`{"command":"git status && git -C sub push --force"}`,
		`{"command":"bash -lc 'git -C repo push origin feat'"}`,
		`{"command":"git -c alias.p=push p origin main"}`,
		`{"command":"git -calias.p='push --force' p origin main"}`,
		`{"command":"git -c alias.ship='!git push origin HEAD' ship"}`,
		`{"command":"git --config-env=alias.p=MY_PUSH_ALIAS p origin main"}`,
		`{"command":"git config alias.p push && git p origin main"}`,
		`{"command":"if git push origin main; then :; fi"}`,
		`{"command":"while ! git push origin main; do break; done"}`,
		`{"command":"{ git push origin HEAD; }"}`,
		`{"command":"for b in main; do git push origin \"$b\"; done"}`,
		`{"command":"eval 'git push origin main'"}`,
		`{"command":"a=git; b=push; eval \"$a $b\""}`,
		`{"command":"echo main | xargs -n 1 git push origin"}`,
		`{"command":"echo push | xargs git"}`,
		`{"command":"g=git; $g push origin main"}`,
		`{"command":"a=git; b=push; $a $b origin main"}`,
		`{"command":"op=push; git $op origin main"}`,
		`{"command":"git $(printf push) origin main"}`,
		`{"command":"git \"p$(printf u)sh\" origin main"}`,
		"{\"command\":\"git `printf push` origin main\"}",
		`{"command":"$(printf git) push origin main"}`,
		`{"command":"$(printf 'git push') origin main"}`,
		`{"command":"$'\\x67\\x69\\x74' $'\\x70\\x75\\x73\\x68' origin main"}`,
		`{"command":"git $'push' origin main"}`,
		`{"command":"git {push,status} origin main"}`,
		`{"command":"git pus? origin main"}`,
		`{"command":"git pu\\\nsh origin main"}`,
		`{"command":"git 2>/dev/null push origin main"}`,
		`{"command":"echo 'git push origin main' | sh"}`,
		`{"command":"bash <<< 'git push origin main'"}`,
		"{\"command\":\"sh <<'EOF'\\ngit push origin main\\nEOF\"}",
		`{"command":"find . -maxdepth 1 -exec git push origin main \\;"}`,
		`{"command":"flock /tmp/repo.lock git -C /workspace/repo push origin main"}`,
		`{"command":"python3 -c 'import subprocess; subprocess.run([\"git\", \"push\"])'"}`,
		"{\"command\":\"python3 - <<'EOF'\\nimport subprocess\\nsubprocess.run(['git', 'push'])\\nEOF\"}",
		`{"command":"git submodule foreach 'git push origin HEAD'"}`,
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
		`{"command":"git -C \"$REPO_DIR\" status"}`,
		`{"command":"git -C $(pwd) diff HEAD~1"}`,
		`{"command":"git commit -m \"fix $(date): push updates locally\""}`,
		`{"command":"git log -- *.go"}`,
		`{"command":"\"$VIRTUAL_ENV/bin/pytest\" -q"}`,
		"{\"command\":\"sh <<'EOF'\\necho hello\\ngit status\\nEOF\"}",
		`{"command":"echo file.go | xargs git add"}`,
		`{"command":"echo $(date)"}`,
		`{"command":"grep 'foo$' README.md"}`,
		`{"command":"ls -la"}`,
	} {
		if IsAutonomousHardBlocked("bash", safeCmd) {
			t.Errorf("bash %s must NOT be hard-blocked", safeCmd)
		}
		if risky, reason := classifyRiskyBash(safeCmd); risky {
			t.Errorf("classifyRiskyBash(%s) = (true, %q), want false", safeCmd, reason)
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

func TestRunPythonGitPushFloor(t *testing.T) {
	policy := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: RecordedSentinel}, nil)

	blockedCases := []struct {
		name  string
		input string
	}{
		{"subprocess run git push", `{"code":"import subprocess\nsubprocess.run(['git', 'push', 'origin', 'main'], check=True)"}`},
		{"os system git push", `{"code":"import os\nos.system('git -C /workspace/repo push origin HEAD')"}`},
		{"ipython bang escape", `{"code":"!git push origin main"}`},
		{"ipython bash cell magic", `{"code":"%%bash\ngit -C /workspace/repo push origin HEAD"}`},
		{"git config guard tamper", `{"code":"import os\ndel os.environ['GIT_CONFIG_COUNT']"}`},
		{"duplicate key smuggling", `{"code":"print(1)","Code":"import subprocess; subprocess.run(['git', 'push'])"}`},
	}
	for _, tc := range blockedCases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsAutonomousHardBlocked("run_python", tc.input) {
				t.Errorf("IsAutonomousHardBlocked(run_python, %s) = false, want true", tc.name)
			}
			blocked, msg := policy.BeforeToolCall("run_python", "py-1", tc.input)
			if !blocked || !strings.Contains(msg, "REFUSED") || !strings.Contains(msg, "bash") {
				t.Errorf("BeforeToolCall(run_python, %s) = (%v, %q), want REFUSED directing to bash", tc.name, blocked, msg)
			}
		})
	}

	allowedCases := []struct {
		name  string
		input string
	}{
		{"pandas with os path and git push text", `{"code":"import os\nmsg = 'remember not to git push without review'\nprint(os.path.basename('/tmp/a.csv'), msg)"}`},
		{"subprocess git status", `{"code":"import subprocess\nprint(subprocess.check_output(['git', 'status', '--short'], text=True))"}`},
		{"subprocess git log", `{"code":"import subprocess\nprint(subprocess.check_output(['git', 'log', '-n', '1'], text=True))"}`},
	}
	for _, tc := range allowedCases {
		t.Run(tc.name, func(t *testing.T) {
			if IsAutonomousHardBlocked("run_python", tc.input) {
				t.Errorf("IsAutonomousHardBlocked(run_python, %s) = true, want false", tc.name)
			}
			if blocked, msg := policy.BeforeToolCall("run_python", "py-ok", tc.input); blocked {
				t.Errorf("BeforeToolCall(run_python, %s) unexpectedly blocked: %q", tc.name, msg)
			}
		})
	}
}

// TestGitPushFloorBareAssignment pins the 2026-11-20 false positive: a
// scheduled report ran `f=$(ls -t ... | head -1)` and Fleet staged a
// "git push to a remote" card, because a bare assignment with a command
// substitution value was read as an unknown dynamic command word. A segment
// that only assigns variables runs no command; everything that can still
// reach git (the substitution bodies, a later dynamic command word, an env
// prefix on a real git, assignments that steer git or the shell, opaque
// expansions, quoted names, substitutions that run a program the gate
// cannot read, integer-attribute targets, stdin carried in) must stay
// blocked.
func TestGitPushFloorBareAssignment(t *testing.T) {
	repro := `ls -t .fleet/tool-output/ | head -3; f=$(ls -t .fleet/tool-output/* | head -1); echo $f; grep -o '"chess.com"\|"calculatorsoup.com"\|"comicbook.com"\|"cheezburger.com"' $f | sort | uniq -c`
	for _, cmd := range []string{
		repro,
		`f=$(ls)`,
		`f=$(ls); echo $f`,
		`f=$(ls) ; cat "$f"`,
		`f="$(ls -t dir/* | head -1)"`,
		"f=`ls`",
		`a=$(ls) b=$(date) c=x`,
		`n=$1`,
		`d=${HOME}/x`, // already allowed before: dynamic then a slash
		`out=$(LC_ALL=C sort -k2 x | uniq -c)`,
		`s=$(printf '%s\n' a b | sort -r)`,
		`j=$(jq -r '.id' x.json); n=$(grep -c foo x)`,
		`ls | head -1; f=$(date)`,
		`n=$(wc -l < x)`,
		`printf '%s\n' a; f=$(ls)`,
		`echo "${HOME}"; f=$(ls)`,
		`f=$(ls) > "$(date +%F).txt"`,
		`for x in a b; do n=$(wc -l < "$x"); done`,
		`export f=$(ls)`, // a command word, already allowed before
		`OUT=$(ls); LATEST=$(ls -t | head -1)`,
		`if f=$(ls); then echo "$f"; fi`,
		`for x in a b; do n=$(wc -l "$x"); echo $n; done`,
		`while line=$(head -1 f); do break; done`,
		`! f=$(ls)`,
		`{ f=$(ls); }`,
		"f=$(ls)\necho $f",
		"f=\\\n$(ls)",
		"cat <<'EOF'\n$(not run)\nEOF\nf=$(ls)",
		`git_ref=$(date)`, // lower-case: bash names are case-sensitive; git reads GIT_*
	} {
		if bashCommandRunsGitPush(cmd) {
			t.Errorf("bashCommandRunsGitPush(%q) = true, want false", cmd)
		}
	}
	raw, err := json.Marshal(map[string]string{"command": repro})
	if err != nil {
		t.Fatal(err)
	}
	if IsGitPushBash(string(raw)) {
		t.Errorf("IsGitPushBash(repro) = true, want false")
	}
	if risky, reason := classifyRiskyBash(string(raw)); risky {
		t.Errorf("classifyRiskyBash(repro) = (true, %q), want false", reason)
	}
	if IsAutonomousHardBlocked("bash", string(raw)) {
		t.Errorf("repro must NOT be hard-blocked")
	}

	for _, cmd := range []string{
		// The substitution body is still checked.
		`f=$(git push origin main)`,
		"f=`git push`",
		`f="$(git -C repo push)"`,
		`f=$(ls) g=$(git push)`,
		`if f=$(git push); then :; fi`,
		// Stdin reaches the substitution when there is no command name.
		`echo 'git push origin main' | f=$(sh)`,
		`printf 'git push' | { f=$(bash); }`,
		`f=$(sh) <<< 'git push origin main'`,
		"f=$(sh) <<'EOF'\ngit push origin main\nEOF",
		// A substitution that runs a program the gate cannot read.
		`f=$(sh /tmp/push-script)`,
		`f=$(bash ./x.sh)`,
		`f=$(source /tmp/push-script)`,
		`f=$(. /tmp/push-script)`,
		`f=$(./push.sh)`,
		`f=$(/tmp/x/ls)`,
		`f=$(python3 x.py)`,
		`f=$(find . -exec sh {} \;)`,
		`f=$(echo x | xargs sh)`,
		`f=$(ls | sort --compress-program=sh)`,
		`f=$(printf -v 'a[$(git push)]' x)`,
		`f=$(ls | sort --co=sh)`,
		`f=$(ls | sort --com sh)`,
		`o=--compress-program=sh; f=$(ls | sort $o)`,
		`o=-v; f=$(printf $o 'a[$(git push)]' x)`,
		`x='a[$(git push)]'; v=-v; f=$(printf "$v" "$x" x)`,
		`f=$(ls $(sh x))`,
		`f=$(git -c core.fsmonitor=x status)`,
		`f=$("ls")`,
		`f=$(if true; then sh x; fi)`,
		`f=$( (sh x) )`,
		`f=$(PATH=/tmp/x ls)`,
		// Opaque expansions in an inert command's words or redirects.
		`x='$(git push)'; f=$(echo ${x@P})`,
		`x='a[$(git push)]'; f=$(echo $((x)))`,
		`f=$(echo "${x:-y}")`,
		`x='a[$(git push)]'; f=$(echo $[x])`,
		`x='a[$(git push)]'; y=$[x]`,
		`f=$(ls ${a[x]})`,
		`f=$(ls > ${x@P})`,
		`f=$(cat < ${x@P})`,
		// Redirects on the assignment itself.
		`x='$(git push)out'; f=$(ls) > "${x@P}"`,
		`f=$(ls) > $((x))`,
		`f=$(ls) > "$(sh /tmp/push-script)"`,
		`f=$(ls > "$(sh /tmp/push-script)")`,
		`LD_BIND_NOW=$(cat f)`,
		`GCONV_PATH=$(pwd)`,
		// Here-doc bodies expand substitutions before the command reads them.
		"f=$(cat <<EOF\n$($(printf '\\x67it') $(printf '\\x70ush'))\nEOF\n)",
		"f=$(cat <<EOF\n`sh x`\nEOF\n)",
		"f=$(cat <<< \"$(sh x)\")",
		"cat <<EOF\n$($(printf '\\x67it') $(printf '\\x70ush'))\nEOF",
		"cat <<-EOF\n\t${x:-$(sh -c 'git push')}\nEOF",
		// Names an inert command could resolve to something else.
		`ls() { sh x; }; f=$(ls)`,
		`function ls { sh x; }; f=$(ls)`,
		`PATH=/tmp/x; f=$(ls)`,
		`hash -p /tmp/x ls; f=$(ls)`,
		`printf -v PATH %s /tmp/reviewbin; f=$(ls)`,
		`builtin printf -v PATH %s /tmp/x; f=$(ls)`,
		`printf $o PATH %s /tmp/x; f=$(ls)`,
		`read PATH < x; f=$(ls)`,
		`getopts o PATH; f=$(ls)`,
		`let PATH=1; f=$(ls)`,
		`unset PATH; f=$(ls)`,
		`export PATH=/tmp/x; f=$(ls)`,
		`PATH+=:/tmp/x; f=$(ls)`,
		`BASH_CMDS[ls]=/tmp/x; f=$(ls)`,
		`BASH_ALIASES[ls]=x; f=$(ls)`,
		`: ${PATH:=/tmp/x}; f=$(ls)`,
		`echo $((PATH=1)); f=$(ls)`,
		`shopt -s expand_aliases; f=$(ls)`,
		// Stdin that reaches the substitution from a redirect, an enclosing
		// group or loop, a function call or exec.
		`f=$(sh </tmp/push-script)`,
		`f=$(bash 0<x)`,
		`f=$(sh <&3)`,
		`f=$(sh <>x)`,
		`sh </tmp/push-script`,
		`echo 'git push' | (sh)`,
		`function fn { $(printf git) push; }; fn`,
		`{ f=$(sh); } < /tmp/push-script`,
		`{ true; f=$(sh); } <<< 'git push'`,
		`echo 'git push' | { true; f=$(sh); }`,
		`echo 'git push' | (f=$(sh))`,
		`echo 'git push' | (true; f=$(sh))`,
		`cat x | while read -r l; do f=$(sh); done`,
		`while :; do f=$(sh); done < x`,
		`exec < /tmp/push-script; f=$(sh)`,
		`exec 0<x; f=$(sh /dev/stdin)`,
		`fn() { f=$(sh); }; echo 'git push' | fn`,
		`function fn { f=$(sh); }; fn < x`,
		// Integer-attribute targets evaluate the value as arithmetic, whose
		// array subscripts run commands hidden in a variable.
		`x='a[$(git push)]'; OPTIND=$x`,
		`OPTIND=$(printf 'a[$(git push)]')`,
		`RANDOM=$x`,
		`HISTCMD=$x`,
		`BASH_COMPAT=$x`,
		`BASH_XTRACEFD=$x`,
		`declare -i f; x='a[$(git push)]'; f=$x`,
		`typeset -i f; f=$x`,
		`fn() { local -i f; f=$x; }; fn`,
		`declare -n r='a[$(git push)]'; r=$(date)`,
		`builtin declare -i f; f=$x`,
		`"decl""are" -i f; f=$x`,
		`eval 'declare -i f'; f=$x`,
		`. ./attrs.sh; f=$x`,
		`source ./attrs.sh; f=$(date)`,
		// Strings run later in the same shell can set the attribute.
		`trap 'declare -gi f' DEBUG; x='a[$(git push)]'; f=$x`,
		`cmd=declare; trap '$cmd -gi f' DEBUG; f=$x`,
		`trap "$t" DEBUG; f=$x`,
		`PS4='$(typeset -gi f)'; set -x; f=$x`,
		`mapfile -C 'declare -gi f' -c 1 a < x; f=$x`,
		`enable -f ./x.so x; f=$x`,
		`readarray -C cb a < x; f=$(date)`,
		// Below the top level the parent's attributes are unknown.
		`declare -i f; eval 'f=$x'`,
		`bash -c 'f=$(ls)'`,
		`echo $(f=$(ls))`,
		// A later dynamic command word still fails closed.
		`f=$(ls); $f`,
		`f=$(printf git); $f push origin main`,
		`f=$(printf git); "$f" push origin main`,
		`cmd=$(printf 'git push'); $cmd`,
		`f=$(printf git) $f push`,
		`f=$(ls); eval "$f"`,
		`f=$(printf git); exec $f push`,
		// An env prefix on a real command still detects.
		`GIT_DIR=x git push`,
		`GIT_DIR=$(pwd)/.git git push`,
		`X=$(ls) git push`,
		`X=$(ls) $f`,
		// Assignments that steer git or the shell keep failing closed with
		// an unknown value.
		`GIT_SSH_COMMAND=$(cat f)`,
		`GIT_CONFIG_COUNT=$(printf 0)`,
		`PATH=$(pwd):$PATH`,
		`HOME=$(mktemp -d)`,
		`XDG_CONFIG_HOME=$(pwd)`,
		`BASH_ENV=$(pwd)`,
		`BASHOPTS=$(cat f)`,
		`EXECIGNORE=$(cat f)`,
		`PS4=$(cat f)`,
		`IFS=$(printf ' ')`,
		`LD_PRELOAD=$(cat f)`,
		`f=$(ls) PATH=$(pwd)`,
		// Opaque expansions: arithmetic and ${...} can evaluate a variable's
		// text, and array subscripts there run hidden commands.
		`y=$((x))`,
		`y=$((a[$(ls)]))`,
		`y=${a[x]}`,
		`y=${x@P}`,
		`y=${x:i}`,
		`y=$'\x67'`,
		`y=$"x"`,
		`y=<(ls)`,
		`y=a{b,c}$(ls)`,
		`y=*$(ls)`,
		`f+=$(ls)`,
		// A quoted or escaped name is a command named "f=...", not an
		// assignment; so is a word after exec/builtin/coproc/time or a
		// quoted keyword.
		`"f"=$(ls)`,
		`'f'=$(ls)`,
		`\f=$(ls)`,
		`f"="$(ls)`,
		`$x=$(ls)`,
		`$""f=$(ls)`,
		`''f=$(ls)`,
		`exec f=$(ls)`,
		`builtin f=$(ls)`,
		`coproc f=$(ls)`,
		`time f=$(ls)`,
		`"if" f=$(ls)`,
		`fi f=$(ls)`,
		`nohup f=$(ls)`,
		`sudo f=$(ls)`,
	} {
		if !bashCommandRunsGitPush(cmd) {
			t.Errorf("bashCommandRunsGitPush(%q) = false, want true", cmd)
		}
	}
}
