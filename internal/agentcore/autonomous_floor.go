package agentcore

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// m365HardBlockedToolSuffixes enumerates the Microsoft 365 MCP tools (from
// internal/ms365mcp/tools_catalog.json) that send external mail/chat/channel
// messages, mutate shared calendars, share OneDrive files externally, install
// mail forwarding/filtering rules, or execute arbitrary Graph batch/meta calls.
// These always remain gated on an approval card even when a conversation owner
// enables Autonomous mode.
var m365HardBlockedToolSuffixes = []string{
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
}

// m365SharedMailboxBlockedPrefixes covers all M365 shared-mailbox outbound or
// destructive tool families (send/reply/forward/delete-shared-mailbox-*).
var m365SharedMailboxBlockedPrefixes = []string{
	"send_shared_mailbox_",
	"reply_shared_mailbox_",
	"forward_shared_mailbox_",
	"delete_shared_mailbox_",
}

// vendorOutboundHardBlockedSuffixes enumerates external SSP/vendor outbound
// messaging tools that send emails or messages to external advertisers/buyers
// and must never run without an explicit approval card in Autonomous mode.
var vendorOutboundHardBlockedSuffixes = []string{
	"tl_async_email_advertiser_report",
	"mgpub_send_deal_request_message",
}

// universalDestructiveAndAccessFloorSuffixes enumerates destructive deletion,
// access-control, email-template mutation, and finance/rev-share tools across
// all client bundles so every deployment is protected on the engine floor even
// before individual bundle manifests add critical_tool_autonomous_blocked.
var universalDestructiveAndAccessFloorSuffixes = []string{
	"delete_page",
	"delete_workspace",
	"delete_template",
	"set_password",
	"delete_email_template",
	"save_email_template",
	"set_deal_rev_share",
	"set_deal_share_code",
	"set_finance_term",
	"revert_finance_term_change",
}

var autonomousHardBlockedSuffixSet = func() map[string]bool {
	total := len(m365HardBlockedToolSuffixes) + len(vendorOutboundHardBlockedSuffixes) + len(universalDestructiveAndAccessFloorSuffixes)
	m := make(map[string]bool, total*2)
	for _, list := range [][]string{
		m365HardBlockedToolSuffixes,
		vendorOutboundHardBlockedSuffixes,
		universalDestructiveAndAccessFloorSuffixes,
	} {
		for _, s := range list {
			m[s] = true
			m[strings.ReplaceAll(s, "-", "_")] = true
		}
	}
	return m
}()

// IsAutonomousHardBlocked reports whether a tool call falls on the Engine
// Hard-Block Floor and therefore requires an explicit approval card even when
// Autonomous mode is active.
//
// Note: outbound email tools (send_email, send_template_email) return true here
// by default; approvalStager.Stage applies the narrow self-addressed owner
// email carve-out AFTER content_file/attachment materialization and
// prevalidation.
func IsAutonomousHardBlocked(toolName, rawInput string) bool {
	trimmed := strings.TrimSpace(toolName)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)

	// 1. Outbound email tools (self-addressed owner carve-out is evaluated
	// after materialization in httpapi/approvals.go).
	if IsOutboundEmailTool(lower) || tools.IsEmailToolName(lower) {
		return lower != toolNamePreviewEmail
	}

	// 2. Privilege-persistence native tools.
	switch lower {
	case toolNameScheduleTask:
		return true
	case toolNameManageTasks:
		return !tools.IsManageTasksShow(rawInput)
	case tools.ChatPatrolToolName:
		return IsGatedChatPatrolCall(rawInput)
	case toolNameConnectorStudio:
		return IsGatedConnectorStudioCall(rawInput)
	case toolNameBash:
		return IsGitPushBash(rawInput)
	}

	// 3. M365 outbound/sharing/rules/batch tools and external SSP messaging tools.
	for suffix := range autonomousHardBlockedSuffixSet {
		if lower == suffix || strings.HasSuffix(lower, "_"+suffix) {
			return true
		}
	}
	normUnderscore := strings.ReplaceAll(lower, "-", "_")
	for suffix := range autonomousHardBlockedSuffixSet {
		if normUnderscore == suffix || strings.HasSuffix(normUnderscore, "_"+suffix) {
			return true
		}
	}
	if isM365SharedMailboxBlocked(normUnderscore) {
		return true
	}

	// 4. Multiplexed Fast.io sharing and storage actions (only safe read/folder
	// actions are allowed on mcp_fast_io_storage; destructive, move, copy,
	// upload, or untyped storage calls stay gated on the floor).
	if isFastIOShareTool(normUnderscore) {
		return true
	}
	if isFastIOStorageTool(normUnderscore) && !isAllowedFastIOStorageAction(rawInput) {
		return true
	}

	return false
}

func isM365SharedMailboxBlocked(normTool string) bool {
	for _, p := range m365SharedMailboxBlockedPrefixes {
		if strings.HasPrefix(normTool, p) || strings.Contains(normTool, "_"+p) {
			return true
		}
	}
	return false
}

func isFastIOShareTool(normTool string) bool {
	return normTool == "mcp_fast_io_share" ||
		normTool == "fast_io_share" ||
		(strings.HasPrefix(normTool, "mcp_fast_io_") && strings.HasSuffix(normTool, "_share"))
}

func isFastIOStorageTool(normTool string) bool {
	return normTool == "mcp_fast_io_storage" ||
		normTool == "fast_io_storage" ||
		(strings.HasPrefix(normTool, "mcp_fast_io_") && strings.HasSuffix(normTool, "_storage"))
}

func isAllowedFastIOStorageAction(rawInput string) bool {
	actions, duplicateOrInvalid := extractTopLevelStringFieldSpellings(rawInput, "action")
	if duplicateOrInvalid || len(actions) != 1 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(actions[0])) {
	case "list", "details", "read-content", "search", "create-folder", "stat", "get":
		return true
	default:
		return false
	}
}

// IsGatedChatPatrolCall reports whether a chat_patrol invocation creates or
// updates a recurring/timer schedule (requiring an approval card on the floor).
// Read/lifecycle actions (list, pause, resume, cancel/delete) return false.
func IsGatedChatPatrolCall(rawInput string) bool {
	actions, duplicateOrInvalid := extractTopLevelStringFieldSpellings(rawInput, "action")
	if duplicateOrInvalid {
		return true
	}
	if len(actions) == 0 {
		return false
	}
	switch tools.NormalizeChatPatrolAction(actions[0]) {
	case tools.ChatPatrolActionCreate, tools.ChatPatrolActionUpdate:
		return true
	default:
		return false
	}
}

// IsGatedConnectorStudioCall reports whether a connector_studio invocation
// performs a mutating or network-probing action (anything recognized other than
// read-only search_catalog). Unknown action names return false so the tool's
// own validation returns a clean error rather than staging an un-executable card.
func IsGatedConnectorStudioCall(rawInput string) bool {
	actions, duplicateOrInvalid := extractTopLevelStringFieldSpellings(rawInput, "action")
	if duplicateOrInvalid {
		return true
	}
	if len(actions) == 0 {
		return false
	}
	canonical := tools.NormalizeConnectorStudioAction(actions[0])
	return canonical != "" && canonical != tools.ConnectorStudioActionSearchCatalog
}

// extractTopLevelStringFieldSpellings inspects a JSON object's top-level keys
// case-insensitively without allowing duplicate spellings to mask an action.
func extractTopLevelStringFieldSpellings(rawInput, wantKey string) (values []string, duplicateOrInvalidType bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(rawInput)))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, false
	}
	seenCount := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return values, seenCount > 1
		}
		key, _ := keyTok.(string)
		var rawVal json.RawMessage
		if err := dec.Decode(&rawVal); err != nil {
			return values, seenCount > 1
		}
		if !strings.EqualFold(strings.TrimSpace(key), wantKey) {
			continue
		}
		seenCount++
		if seenCount > 1 {
			return values, true
		}
		trimmed := bytes.TrimSpace(rawVal)
		if bytes.Equal(trimmed, []byte("null")) {
			seenCount--
			continue
		}
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, true
		}
		values = append(values, s)
	}
	return values, false
}

// IsGitPushBash reports whether a bash tool JSON payload invokes `git push`
// (including `git -C <dir> push`, `git -c <k=v> push`, `--git-dir=...`, path
// prefixes like `/usr/bin/git`, wrappers like `env`/`sudo`, or chained shell
// commands).
func IsGitPushBash(rawInput string) bool {
	cmds, dupOrBad := extractTopLevelStringFieldSpellings(rawInput, "command")
	if dupOrBad {
		return true
	}
	for _, c := range cmds {
		if bashCommandRunsGitPush(c) {
			return true
		}
	}
	return false
}

func bashCommandRunsGitPush(cmd string) bool {
	return bashCommandRunsGitPushDepth(cmd, 0)
}

func bashCommandRunsGitPushDepth(cmd string, depth int) bool {
	if depth > 4 {
		return true
	}
	segments := splitShellCommandSegments(cmd)
	for _, seg := range segments {
		if segmentIsGitPush(seg, depth) {
			return true
		}
	}
	return false
}

func splitShellCommandSegments(cmd string) [][]string {
	var (
		segments [][]string
		cur      []string
		tok      strings.Builder
		hasTok   bool
		inSingle bool
		inDouble bool
		escaped  bool
	)
	flushTok := func() {
		if hasTok {
			cur = append(cur, tok.String())
			tok.Reset()
			hasTok = false
		}
	}
	flushSeg := func() {
		flushTok()
		if len(cur) > 0 {
			segments = append(segments, cur)
			cur = nil
		}
	}
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			tok.WriteByte(ch)
			hasTok = true
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			} else {
				tok.WriteByte(ch)
			}
			hasTok = true
			continue
		}
		if inDouble {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			case '`':
				flushSeg()
			case '$':
				if i+1 < len(cmd) && cmd[i+1] == '(' {
					flushSeg()
					i++
					continue
				}
				tok.WriteByte(ch)
			default:
				tok.WriteByte(ch)
			}
			hasTok = true
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
			hasTok = true
		case '"':
			inDouble = true
			hasTok = true
		case ' ', '\t', '\r':
			flushTok()
		case ';', '\n', '&', '|', '(', ')', '`':
			flushSeg()
		case '$':
			if i+1 < len(cmd) && cmd[i+1] == '(' {
				flushSeg()
				i++
				continue
			}
			tok.WriteByte(ch)
			hasTok = true
		default:
			tok.WriteByte(ch)
			hasTok = true
		}
	}
	flushSeg()
	return segments
}

var gitFlagsTakingArg = map[string]bool{
	"-C":             true,
	"-c":             true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--super-prefix": true,
	"--exec-path":    true,
	"--config-env":   true,
}

func segmentIsGitPush(tokens []string, depth int) bool {
	i, nestedPush, terminated := skipShellWrappers(tokens, depth)
	if nestedPush {
		return true
	}
	if terminated || i >= len(tokens) || executableBaseName(tokens[i]) != "git" {
		return false
	}
	return gitTokensHavePushSubcommand(tokens[i+1:])
}

func skipShellWrappers(tokens []string, depth int) (next int, nestedPush bool, terminated bool) {
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if isEnvAssignmentToken(t) {
			i++
			continue
		}
		base := executableBaseName(t)
		switch base {
		case "sh", "bash", "dash", "zsh":
			return i, shellDashCRunsGitPush(tokens[i+1:], depth), true
		case "env", "command", "nohup", "time":
			i = skipFlagRun(tokens, i+1, envArgFlags(base))
			continue
		case "sudo":
			i = skipFlagRun(tokens, i+1, sudoArgFlags)
			continue
		case "nice":
			i = skipNiceFlags(tokens, i+1)
			continue
		case "timeout":
			i = skipTimeoutWrapper(tokens, i+1)
			continue
		}
		break
	}
	return i, false, false
}

func shellDashCRunsGitPush(args []string, depth int) bool {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		flag := args[i]
		i++
		if flag == "-c" || (len(flag) > 2 && flag[0] == '-' && flag[1] != '-' && strings.HasSuffix(flag, "c")) {
			return i < len(args) && bashCommandRunsGitPushDepth(args[i], depth+1)
		}
	}
	return false
}

var (
	envFlagsWithArg = map[string]bool{"-u": true, "-C": true, "-S": true}
	sudoArgFlags    = map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true}
	timeoutArgFlags = map[string]bool{"-k": true, "-s": true}
)

func envArgFlags(base string) map[string]bool {
	if base == "env" {
		return envFlagsWithArg
	}
	return nil
}

func skipFlagRun(tokens []string, i int, flagsTakingArg map[string]bool) int {
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		if flagsTakingArg[tokens[i]] && i+1 < len(tokens) {
			i += 2
			continue
		}
		i++
	}
	return i
}

func skipNiceFlags(tokens []string, i int) int {
	if i < len(tokens) && tokens[i] == "-n" && i+1 < len(tokens) {
		return i + 2
	}
	if i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
		return i + 1
	}
	return i
}

func skipTimeoutWrapper(tokens []string, i int) int {
	i = skipFlagRun(tokens, i, timeoutArgFlags)
	if i < len(tokens) {
		i++ // duration arg
	}
	return i
}

func gitTokensHavePushSubcommand(args []string) bool {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return strings.EqualFold(arg, "push")
		}
		if arg == "-h" || arg == "--help" || arg == "-v" || arg == "--version" {
			return false
		}
		if gitFlagsTakingArg[arg] {
			i += 2
			continue
		}
		i++
	}
	return i < len(args) && strings.EqualFold(args[i], "push")
}

func isEnvAssignmentToken(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if i == 0 {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' {
				return false
			}
		} else if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func executableBaseName(tok string) string {
	if idx := strings.LastIndexByte(tok, '/'); idx >= 0 {
		tok = tok[idx+1:]
	}
	return strings.ToLower(strings.TrimSpace(tok))
}
