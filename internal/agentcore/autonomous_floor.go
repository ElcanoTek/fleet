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

// emailTemplateHardBlockedSuffixes covers team-wide email template mutations
// alongside the base nonReversibleSuffixes (send_email, send_template_email).
// Per AGENTS.md ("Bundles are data, fleet is engine"), customer- or
// bundle-specific MCP suffixes belong in manifest.yaml
// agent_policy.critical_tool_autonomous_blocked, never hardcoded here.
var emailTemplateHardBlockedSuffixes = []string{
	"delete_email_template",
	"save_email_template",
}

var autonomousHardBlockedSuffixSet = func() map[string]bool {
	total := len(m365HardBlockedToolSuffixes) + len(emailTemplateHardBlockedSuffixes)
	m := make(map[string]bool, total*2)
	for _, list := range [][]string{
		m365HardBlockedToolSuffixes,
		emailTemplateHardBlockedSuffixes,
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
	case toolNameRunPython:
		return IsGitPushPython(rawInput)
	}

	// 3. M365 outbound/sharing/rules/batch tools and team-wide email template tools.
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

// isAllowedFastIOStorageAction reports whether a Fast.io `storage` call names
// exactly one action from the list that may run in Autonomous mode without a
// card: the tool's read actions, plus create-folder. Every other action stays
// gated, and so does one this list does not know: the tool is multiplexed, so
// an unrecognized action may be a write.
//
// The reads are the ones the hosted tool declares. The second row was added
// after an Autonomous chat kept raising cards for plain reads such as
// `content` and `version-list`. `preview-url` and `preview-transform` are left
// out: both produce a link to the content, not the content itself.
func isAllowedFastIOStorageAction(rawInput string) bool {
	actions, duplicateOrInvalid := extractTopLevelStringFieldSpellings(rawInput, "action")
	if duplicateOrInvalid || len(actions) != 1 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(actions[0])) {
	case "list", "details", "read-content", "search", "create-folder", "stat", "get",
		"describe", "recent", "content", "version-list", "trash-list", "lock-status",
		"metadata-facts", "metadata-get", "metadata-versions":
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
// prefixes like `/usr/bin/git` or `/usr/lib/git-core/git-push`, wrappers like
// `env`/`sudo`/`xargs`, command/parameter substitutions, or shell pipelines and
// heredocs).
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

var pythonProcessCapabilityMarkers = []string{
	"subprocess",
	"system",
	"popen",
	"exec",
	"spawn",
	"pty",
	"ctypes",
	"cffi",
	"getoutput",
	"__import__",
	"importlib",
	"getattr",
}

// IsGitPushPython reports whether a run_python tool JSON payload attempts to
// launch `git push` (or tamper with the kernel's Git push transport guard) via
// IPython shell escapes or Python process-execution APIs.
func IsGitPushPython(rawInput string) bool {
	codes, dupOrBad := extractTopLevelStringFieldSpellings(rawInput, "code")
	if dupOrBad {
		return true
	}
	for _, code := range codes {
		if pythonCodeRunsGitPush(code) {
			return true
		}
	}
	return false
}

func pythonCodeRunsGitPush(code string) bool {
	if pythonIPythonEscapeRunsGitPush(code) {
		return true
	}
	lower := strings.ToLower(code)
	if strings.Contains(lower, "git_config") || strings.Contains(lower, "fleet-approval-required") || strings.Contains(lower, "pushinsteadof") {
		return true
	}
	if !pythonHasProcessCapability(lower) {
		return false
	}
	if strings.Contains(lower, "git-push") || strings.Contains(lower, "git-send-pack") || strings.Contains(lower, "git-receive-pack") {
		return true
	}
	return containsASCIIWordFold(code, "git") &&
		(containsASCIIWordFold(code, "push") || containsASCIIWordFold(code, "send-pack") || containsASCIIWordFold(code, "receive-pack"))
}

func pythonIPythonEscapeRunsGitPush(code string) bool {
	lines := strings.Split(code, "\n")
	for idx, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "!") {
			shellCmd := strings.TrimLeft(trimmed, "!")
			if bashCommandRunsGitPush(shellCmd) {
				return true
			}
		}
		lowerLine := strings.ToLower(trimmed)
		if strings.HasPrefix(lowerLine, "%sx ") || strings.HasPrefix(lowerLine, "%system ") {
			space := strings.IndexByte(trimmed, ' ')
			if space >= 0 && bashCommandRunsGitPush(trimmed[space+1:]) {
				return true
			}
		}
		if strings.HasPrefix(lowerLine, "%%bash") || strings.HasPrefix(lowerLine, "%%sh") || strings.HasPrefix(lowerLine, "%%script") {
			body := strings.Join(lines[idx+1:], "\n")
			if bashCommandRunsGitPush(body) {
				return true
			}
		}
	}
	return false
}

func pythonHasProcessCapability(lower string) bool {
	for _, marker := range pythonProcessCapabilityMarkers {
		if strings.Contains(lower, marker) {
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
	segments, subcmds, grouped := parseShellScript(cmd)
	for _, sub := range subcmds {
		if bashCommandRunsGitPushDepth(sub, depth+1) {
			return true
		}
	}
	writesPushText := anySegmentWritesPushText(segments)
	stdinMayReachAssign := anySegmentReceivesStdin(segments) && (grouped || anySegmentIsCompound(segments))
	// grouped also covers `ls() { ...; }`, which would redefine an inert name.
	assignOnlyOK := depth == 0 && !grouped && !stdinMayReachAssign && !anySegmentMaySetVarAttributes(segments)
	for _, seg := range segments {
		if segmentIsGitPush(seg, writesPushText, assignOnlyOK, depth) {
			return true
		}
	}
	return false
}

func anySegmentWritesPushText(segments []shellSegment) bool {
	for _, seg := range segments {
		if !seg.hasOutRedir && !segmentUsesTee(seg.tokens) {
			continue
		}
		for _, tok := range seg.tokens {
			if tok.dynamic || containsASCIIWordFold(tok.val, "git") || containsASCIIWordFold(tok.val, "push") {
				return true
			}
		}
		for _, body := range seg.stdinBodies {
			if containsASCIIWordFold(body, "git") && containsASCIIWordFold(body, "push") {
				return true
			}
		}
	}
	return false
}

func segmentUsesTee(tokens []shellToken) bool {
	for _, tok := range tokens {
		if executableBaseName(tok.val) == "tee" {
			return true
		}
	}
	return false
}

type shellToken struct {
	val               string
	dynamic           bool
	dynamicAfterSlash bool
	// literal: the whole word is unquoted, unescaped source text with no
	// expansion, so a reserved word like `if` really is the keyword.
	literal bool
	// valueOnlyAssign: the word is a shell variable assignment `NAME=value`
	// whose `NAME=` is unquoted literal source text (so bash parses it as
	// an assignment, not as a command named `NAME=...`) and whose every
	// expansion is a command substitution or a simple `$param` — forms that
	// only produce a string. See segmentOnlyAssignsVariables.
	valueOnlyAssign bool
	// subcmds are the bodies of the command and process substitutions
	// inside this word (also listed in the script's own subcmds).
	subcmds []string
	// evaluates: the word has a `${...}` or `$((...))` expansion, which can
	// evaluate a variable's text (`${x@P}` prompt expansion, arithmetic
	// whose array subscripts run commands); see markEvalDynamic.
	evaluates bool
}

type shellSegment struct {
	tokens      []shellToken
	pipedStdin  bool
	hasOutRedir bool
	// opaqueRedir: a redirect target had a `${...}` or `$((...))`
	// expansion (`> ${x@P}`); the target word itself is not kept in tokens.
	opaqueRedir bool
	stdinBodies []string
	stdinDyn    bool
}

type pendingHeredoc struct {
	segIdx int
	delim  string
	strip  bool
	// expand: the delimiter is unquoted, so bash expands `$(...)`,
	// backticks and `${...}` in the body before the command reads it.
	expand bool
}

type shellLexer struct {
	src         string
	pos         int
	segments    []shellSegment
	curSeg      shellSegment
	tok         strings.Builder
	hasTok      bool
	tokDyn      bool
	tokDynSlash bool
	// tokPlainLen counts the leading bytes of the current word that came
	// from unquoted, unescaped source text; tokPlainStop ends that run at
	// the first quote, escape or expansion. tokOpaqueDyn marks a word with
	// an expansion other than `$(...)`, backticks or a simple `$param`.
	tokPlainLen  int
	tokPlainStop bool
	tokOpaqueDyn bool
	tokEvalDyn   bool
	tokSubFrom   int
	subcmds      []string
	heredocs     []pendingHeredoc
	grouped      bool
}

// parseShellScript splits a script into segments and the bodies of its
// command and process substitutions. grouped reports a `(` or `)` at this
// level (a subshell, a function definition, a case pattern), which can carry
// a pipe or redirect into the segments inside it.
func parseShellScript(cmd string) (segments []shellSegment, subcmds []string, grouped bool) {
	l := &shellLexer{src: cmd}
	for l.pos < len(l.src) {
		l.stepUnquoted()
	}
	l.flushSeg(false)
	return l.segments, l.subcmds, l.grouped
}

func (l *shellLexer) writeLiteralByte(ch byte) {
	l.tok.WriteByte(ch)
	l.hasTok = true
	l.tokPlainStop = true
	if ch == '/' {
		l.tokDynSlash = false
	}
}

// writePlainByte writes a byte that appeared unquoted and unescaped in the
// source, extending the word's plain prefix while nothing else came first.
func (l *shellLexer) writePlainByte(ch byte) {
	plain := !l.tokPlainStop
	l.writeLiteralByte(ch)
	if plain {
		l.tokPlainLen++
		l.tokPlainStop = false
	}
}

// markDynamic records an expansion the gate cannot resolve and that may do
// more than yield a string (globs, `${...}` with its arithmetic subscripts
// and `@P` prompt expansion, `$((...))`, `$'...'`, process substitution).
func (l *shellLexer) markDynamic(placeholder string) {
	l.tokOpaqueDyn = true
	l.markStringDynamic(placeholder)
}

// markEvalDynamic records a `${...}` or `$((...))` expansion: opaque, and
// able to evaluate a variable's text as a prompt string or arithmetic.
func (l *shellLexer) markEvalDynamic(placeholder string) {
	l.tokEvalDyn = true
	l.markDynamic(placeholder)
}

// markStringDynamic records a command substitution (`$(...)` or backticks,
// whose bodies are collected in subcmds and checked on their own) or a
// simple `$param`: unknown text, but the expansion itself only yields a string.
func (l *shellLexer) markStringDynamic(placeholder string) {
	if placeholder != "" {
		l.tok.WriteString(placeholder)
	}
	l.hasTok = true
	l.tokDyn = true
	l.tokDynSlash = true
	l.tokPlainStop = true
}

func (l *shellLexer) flushTok() {
	if !l.hasTok {
		return
	}
	val := l.tok.String()
	eq := strings.IndexByte(val, '=')
	l.curSeg.tokens = append(l.curSeg.tokens, shellToken{
		val:               val,
		dynamic:           l.tokDyn,
		dynamicAfterSlash: l.tokDynSlash,
		literal:           !l.tokDyn && l.tokPlainLen == len(val),
		valueOnlyAssign:   eq > 0 && eq < l.tokPlainLen && !l.tokOpaqueDyn && isEnvAssignmentToken(val),
		subcmds:           append([]string(nil), l.subcmds[l.tokSubFrom:]...),
		evaluates:         l.tokEvalDyn,
	})
	l.tokSubFrom = len(l.subcmds)
	l.tok.Reset()
	l.hasTok = false
	l.tokDyn = false
	l.tokDynSlash = false
	l.tokPlainLen = 0
	l.tokPlainStop = false
	l.tokOpaqueDyn = false
	l.tokEvalDyn = false
}

func (l *shellLexer) flushSeg(nextPiped bool) {
	l.flushTok()
	hasContent := len(l.curSeg.tokens) > 0 || len(l.curSeg.stdinBodies) > 0 || l.curSeg.stdinDyn || l.curSeg.hasOutRedir
	if hasContent {
		segIdx := len(l.segments)
		l.segments = append(l.segments, l.curSeg)
		for idx := range l.heredocs {
			if l.heredocs[idx].segIdx < 0 {
				l.heredocs[idx].segIdx = segIdx
			}
		}
	} else {
		l.heredocs = nil
	}
	// A pipe into `(`: the empty segment between `|` and `(` is dropped, so
	// hand its piped stdin to the first segment inside the subshell, which
	// reads it (`echo 'git push' | (sh)`).
	carried := !hasContent && l.curSeg.pipedStdin
	l.curSeg = shellSegment{pipedStdin: nextPiped || carried}
}

func (l *shellLexer) stepUnquoted() {
	if !l.hasTok && l.startsIONumberRedir() {
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		l.handleRedir()
		return
	}
	ch := l.src[l.pos]
	switch ch {
	case '\\':
		l.consumeBackslash()
	case '\'':
		l.readSingleQuoted()
	case '"':
		l.readDoubleQuoted()
	case '$':
		l.readDollarInWord(false)
	case '`':
		l.subcmds = append(l.subcmds, l.readBacktickSubst())
		l.markStringDynamic("$()")
	case ' ', '\t', '\r':
		l.flushTok()
		l.pos++
	case '#':
		l.stepHash()
	case '\n':
		l.flushSeg(false)
		l.pos++
		l.consumePendingHeredocs()
	case ';', '(', ')':
		if ch != ';' {
			l.grouped = true
		}
		l.flushSeg(false)
		l.pos++
	case '&':
		l.stepAmpersand()
	case '|':
		l.stepPipe()
	case '<', '>':
		l.handleRedir()
	case '*', '?', '[':
		l.markDynamic(string(ch))
		l.pos++
	case '{', '}':
		l.stepBrace(ch)
	default:
		l.writePlainByte(ch)
		l.pos++
	}
}

func (l *shellLexer) startsIONumberRedir() bool {
	j := l.pos
	for j < len(l.src) && l.src[j] >= '0' && l.src[j] <= '9' {
		j++
	}
	if j == l.pos || j >= len(l.src) {
		return false
	}
	if l.src[j] != '<' && l.src[j] != '>' {
		return false
	}
	return j+1 >= len(l.src) || l.src[j+1] != '('
}

func (l *shellLexer) consumeBackslash() {
	if l.pos+1 >= len(l.src) {
		l.pos++
		return
	}
	if l.src[l.pos+1] == '\n' {
		l.pos += 2
		return
	}
	l.writeLiteralByte(l.src[l.pos+1])
	l.pos += 2
}

func (l *shellLexer) readSingleQuoted() {
	l.pos++
	l.hasTok = true
	l.tokPlainStop = true
	for l.pos < len(l.src) && l.src[l.pos] != '\'' {
		l.writeLiteralByte(l.src[l.pos])
		l.pos++
	}
	if l.pos < len(l.src) {
		l.pos++
	}
}

func (l *shellLexer) readDoubleQuoted() {
	l.pos++
	l.hasTok = true
	l.tokPlainStop = true
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		switch ch {
		case '"':
			l.pos++
			return
		case '\\':
			l.consumeDoubleQuoteBackslash()
		case '`':
			l.subcmds = append(l.subcmds, l.readBacktickSubst())
			l.markStringDynamic("$()")
		case '$':
			l.readDollarInWord(true)
		default:
			l.writeLiteralByte(ch)
			l.pos++
		}
	}
}

func (l *shellLexer) consumeDoubleQuoteBackslash() {
	if l.pos+1 >= len(l.src) {
		l.pos++
		return
	}
	next := l.src[l.pos+1]
	if next == '\n' {
		l.pos += 2
		return
	}
	if next == '$' || next == '`' || next == '"' || next == '\\' {
		l.writeLiteralByte(next)
		l.pos += 2
		return
	}
	l.writeLiteralByte('\\')
	l.writeLiteralByte(next)
	l.pos += 2
}

func (l *shellLexer) readDollarInWord(inDouble bool) {
	if l.pos+1 >= len(l.src) {
		l.writeLiteralByte('$')
		l.pos++
		return
	}
	next := l.src[l.pos+1]
	switch {
	case next == '(':
		// `$((...))` is arithmetic, not a command substitution: bash
		// evaluates variable values inside it as expressions, and an array
		// subscript there (`a[$(cmd)]`) runs commands hidden in a variable
		// the gate never sees, so it stays opaque.
		arith := l.pos+2 < len(l.src) && l.src[l.pos+2] == '('
		l.pos += 2
		l.subcmds = append(l.subcmds, l.readParenBody())
		if arith {
			l.markEvalDynamic("$()")
		} else {
			l.markStringDynamic("$()")
		}
	case next == '{':
		l.pos += 2
		l.readBraceBody()
		l.markEvalDynamic("${}")
	case !inDouble && next == '\'':
		l.pos += 2
		l.skipANSICBody()
		l.markDynamic("$''")
	case !inDouble && next == '"':
		l.pos++
		l.markDynamic("")
		l.readDoubleQuoted()
	case next == '[':
		// `$[...]` is bash's old arithmetic expansion: it evaluates like
		// `$((...))`. The bracket text that follows is lexed as usual.
		l.pos++
		l.markEvalDynamic("$")
	case isShellParamStart(next):
		l.pos += 2
		if isASCIILetterOrUnderscore(next) {
			for l.pos < len(l.src) && isASCIIAlnumOrUnderscore(l.src[l.pos]) {
				l.pos++
			}
		}
		l.markStringDynamic("$v")
	default:
		l.writeLiteralByte('$')
		l.pos++
	}
}

func isShellParamStart(b byte) bool {
	if isASCIIAlnumOrUnderscore(b) {
		return true
	}
	switch b {
	case '@', '*', '#', '?', '$', '!', '-':
		return true
	}
	return false
}

func isASCIILetterOrUnderscore(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isASCIIAlnumOrUnderscore(b byte) bool {
	return isASCIILetterOrUnderscore(b) || (b >= '0' && b <= '9')
}

func (l *shellLexer) skipANSICBody() {
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		if ch == '\'' {
			l.pos++
			return
		}
		if ch == '\\' && l.pos+1 < len(l.src) {
			l.pos += 2
			continue
		}
		l.pos++
	}
}

func (l *shellLexer) readBacktickSubst() string {
	l.pos++
	var inner strings.Builder
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		if ch == '`' {
			l.pos++
			break
		}
		if ch == '\\' && l.pos+1 < len(l.src) {
			next := l.src[l.pos+1]
			if next == '\n' {
				l.pos += 2
				continue
			}
			if next == '`' || next == '\\' || next == '$' {
				inner.WriteByte(next)
				l.pos += 2
				continue
			}
		}
		inner.WriteByte(ch)
		l.pos++
	}
	return inner.String()
}

func (l *shellLexer) readParenBody() string {
	start := l.pos
	depth := 1
	inSingle := false
	inDouble := false
	for l.pos < len(l.src) && depth > 0 {
		ch := l.src[l.pos]
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			l.pos++
			continue
		}
		if ch == '\\' && l.pos+1 < len(l.src) {
			l.pos += 2
			continue
		}
		if inDouble {
			if ch == '"' {
				inDouble = false
			}
			l.pos++
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				body := l.src[start:l.pos]
				l.pos++
				return body
			}
		}
		l.pos++
	}
	return l.src[start:l.pos]
}

func (l *shellLexer) readBraceBody() {
	depth := 1
	inSingle := false
	inDouble := false
	for l.pos < len(l.src) && depth > 0 {
		ch := l.src[l.pos]
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			l.pos++
			continue
		}
		if ch == '\\' && l.pos+1 < len(l.src) {
			l.pos += 2
			continue
		}
		if ch == '`' {
			l.subcmds = append(l.subcmds, l.readBacktickSubst())
			continue
		}
		if ch == '$' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '(' {
			l.pos += 2
			l.subcmds = append(l.subcmds, l.readParenBody())
			continue
		}
		if inDouble {
			if ch == '"' {
				inDouble = false
			}
			l.pos++
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '{':
			depth++
		case '}':
			depth--
		}
		l.pos++
	}
}

func (l *shellLexer) stepHash() {
	if l.hasTok {
		l.writeLiteralByte('#')
		l.pos++
		return
	}
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		l.pos++
	}
}

func (l *shellLexer) stepAmpersand() {
	if l.pos+1 < len(l.src) && l.src[l.pos+1] == '>' {
		l.flushTok()
		l.curSeg.hasOutRedir = true
		l.pos += 2
		if l.pos < len(l.src) && l.src[l.pos] == '>' {
			l.pos++
		}
		_ = l.readRedirectTarget()
		return
	}
	l.flushSeg(false)
	if l.pos+1 < len(l.src) && l.src[l.pos+1] == '&' {
		l.pos += 2
		return
	}
	l.pos++
}

func (l *shellLexer) stepPipe() {
	if l.pos+1 < len(l.src) && l.src[l.pos+1] == '|' {
		l.flushSeg(false)
		l.pos += 2
		return
	}
	if l.pos+1 < len(l.src) && l.src[l.pos+1] == '&' {
		l.flushSeg(true)
		l.pos += 2
		return
	}
	l.flushSeg(true)
	l.pos++
}

func (l *shellLexer) stepBrace(ch byte) {
	if !l.hasTok && (l.pos+1 >= len(l.src) || isShellWordDelimiter(l.src[l.pos+1])) {
		l.writePlainByte(ch)
		l.pos++
		return
	}
	l.markDynamic(string(ch))
	l.pos++
}

func isShellWordDelimiter(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

func (l *shellLexer) handleRedir() {
	ch := l.src[l.pos]
	if l.pos+1 < len(l.src) && l.src[l.pos+1] == '(' {
		l.pos += 2
		l.subcmds = append(l.subcmds, l.readParenBody())
		l.markDynamic(string(ch) + "()")
		return
	}
	l.flushTok()
	if strings.HasPrefix(l.src[l.pos:], "<<<") {
		l.pos += 3
		w := l.readRedirectTarget()
		l.curSeg.stdinBodies = append(l.curSeg.stdinBodies, w.val)
		if w.dynamic {
			l.curSeg.stdinDyn = true
		}
		return
	}
	if strings.HasPrefix(l.src[l.pos:], "<<") {
		strip := strings.HasPrefix(l.src[l.pos:], "<<-")
		if strip {
			l.pos += 3
		} else {
			l.pos += 2
		}
		delim := l.readRedirectTarget()
		if delim.val != "" {
			l.heredocs = append(l.heredocs, pendingHeredoc{segIdx: -1, delim: delim.val, strip: strip, expand: delim.literal || delim.dynamic})
		}
		return
	}
	if ch == '>' {
		l.curSeg.hasOutRedir = true
	} else {
		// `< file`, `<&3`, `<> file`: stdin is content the gate cannot
		// see, so an interpreter reading it (`sh < script`) fails closed
		// just as it does for a pipe.
		l.curSeg.stdinDyn = true
	}
	l.pos++
	if l.pos < len(l.src) && (l.src[l.pos] == '>' || l.src[l.pos] == '&' || l.src[l.pos] == '|') {
		l.pos++
	}
	_ = l.readRedirectTarget()
}

func (l *shellLexer) readRedirectTarget() shellToken {
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.pos++
	}
	savedSeg := l.curSeg
	l.curSeg = shellSegment{}
	for l.pos < len(l.src) && !isShellWordDelimiter(l.src[l.pos]) {
		l.stepUnquoted()
	}
	l.flushTok()
	var out shellToken
	if len(l.curSeg.tokens) > 0 {
		out = l.curSeg.tokens[0]
	}
	for _, tok := range l.curSeg.tokens {
		if tok.evaluates {
			savedSeg.opaqueRedir = true
		}
	}
	l.curSeg = savedSeg
	return out
}

func (l *shellLexer) consumePendingHeredocs() {
	if len(l.heredocs) == 0 {
		return
	}
	pending := l.heredocs
	l.heredocs = nil
	for _, hd := range pending {
		body := l.readHeredocBody(hd.delim, hd.strip)
		if hd.segIdx >= 0 && hd.segIdx < len(l.segments) {
			l.segments[hd.segIdx].stdinBodies = append(l.segments[hd.segIdx].stdinBodies, body)
		}
		if hd.expand {
			l.subcmds = append(l.subcmds, heredocSubstitutions(body)...)
		}
	}
	l.tokSubFrom = len(l.subcmds)
}

// heredocSubstitutions returns the command substitution bodies bash runs
// while expanding an unquoted here-doc: `$(...)`, backticks, and those
// nested in `${...}`. Quotes are literal in a here-doc body, so only
// backslash escapes are honoured.
func heredocSubstitutions(body string) []string {
	sub := &shellLexer{src: body}
	for sub.pos < len(sub.src) {
		switch sub.src[sub.pos] {
		case '\\':
			sub.pos += 2
		case '`':
			sub.subcmds = append(sub.subcmds, sub.readBacktickSubst())
		case '$':
			sub.readDollarInWord(true)
		default:
			sub.pos++
		}
	}
	return sub.subcmds
}

func (l *shellLexer) readHeredocBody(delim string, stripTabs bool) string {
	var body strings.Builder
	for l.pos < len(l.src) {
		lineStart := l.pos
		nl := strings.IndexByte(l.src[lineStart:], '\n')
		var line string
		if nl < 0 {
			line = l.src[lineStart:]
			l.pos = len(l.src)
		} else {
			line = l.src[lineStart : lineStart+nl]
			l.pos = lineStart + nl + 1
		}
		cmp := strings.TrimSuffix(line, "\r")
		if stripTabs {
			cmp = strings.TrimLeft(cmp, "\t")
		}
		if cmp == delim {
			break
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	return body.String()
}

var gitFlagsTakingArg = map[string]bool{
	"-C":           true,
	"-c":           true,
	"--git-dir":    true,
	"--work-tree":  true,
	"--namespace":  true,
	"--config-env": true,
}

func segmentIsGitPush(seg shellSegment, writesPushText, assignOnlyOK bool, depth int) bool {
	if assignOnlyOK && segmentOnlyAssignsVariables(seg) {
		return false
	}
	res := skipShellWrappers(seg, writesPushText, depth)
	if res.nestedPush {
		return true
	}
	if res.terminated || res.next >= len(seg.tokens) {
		return false
	}
	tok := seg.tokens[res.next]
	if tok.dynamicAfterSlash {
		return true
	}
	cmdBase := executableBaseName(tok.val)
	if cmdBase == "git-push" {
		return true
	}
	if cmdBase == "git" {
		return gitTokensHavePushSubcommandDepth(seg.tokens[res.next+1:], res.viaXargs, depth)
	}
	if (cmdBase == "echo" || cmdBase == "printf") && !seg.hasOutRedir && !seg.pipedStdin {
		return false
	}
	if nonGitCommandEmbedsGitPush(seg.tokens[res.next+1:], depth) {
		return true
	}
	for _, body := range seg.stdinBodies {
		if tokenContainsEmbeddedGitAndPush(body) {
			return true
		}
	}
	return false
}

// assignmentLeadKeywords are the reserved words after which bash still
// parses `NAME=value` as an assignment (`if f=$(ls); then`, `do n=$1`).
// `exec`, `builtin`, `coproc`, `time` and the closing words are left out on
// purpose: after them the word is a command or an argument.
var assignmentLeadKeywords = map[string]bool{
	"!":     true,
	"{":     true,
	"if":    true,
	"then":  true,
	"elif":  true,
	"else":  true,
	"while": true,
	"until": true,
	"do":    true,
}

// segmentOnlyAssignsVariables reports whether a segment is nothing but shell
// variable assignments, such as `f=$(ls -t dir/* | head -1)`, so it runs no
// command of its own. skipShellWrappers fails closed on any dynamic first
// word because `$cmd push` or `$(printf git) push` can expand to git; a
// bare assignment is not a command word, though, and without this a harmless
// `f=$(ls ...)` staged a "git push to a remote" card. It stays safe because:
//   - command substitution bodies are collected in subcmds and checked on
//     their own by bashCommandRunsGitPushDepth, so `f=$(git push)` is caught,
//     and beyond that every body must only run inert commands
//     (substBodyIsInert): no interpreter, script file, `source`, `find
//     -exec`, `xargs` or anything else that can run a program the gate cannot
//     read, so `f=$(sh /tmp/push-script)` keeps failing closed;
//   - only `$(...)`, backticks and simple `$param` count (valueOnlyAssign):
//     `$((...))` and `${...}` can evaluate a variable's text as arithmetic,
//     whose array subscripts run hidden commands, so those stay opaque;
//   - `NAME=` must be unquoted literal source, so `"f"=$(...)` or `\f=$(...)`
//     (a command named `f=...` to bash) and `$x=$(...)` stay opaque;
//   - every word must be an assignment, so a later dynamic command word
//     (`f=$(ls) $f push`, `X=$(y) git push`) still takes the normal path;
//   - assignments to variables that steer which program or config later
//     commands use (gitSensitiveAssignName) are not let through with an
//     unknown value;
//   - a segment that receives stdin (a pipe, here-doc or here-string) is
//     left to the normal path: with no command name, bash hands a pipe's
//     stdin to the substitution, so `echo 'git push' | f=$(sh)` runs the
//     piped text, and the body `sh` alone looks harmless once parsed apart.
//     The caller extends this to stdin that reaches the segment from
//     around it: when any segment at this level receives stdin and the
//     script has a group, loop, conditional, function or `exec`
//     (`{ f=$(sh); } < f`, `cat f | while :; do f=$(sh); done`,
//     `exec < f; f=$(sh)`), the shortcut is off for the whole level;
//   - the value must not be evaluated as arithmetic. Assigning to a variable
//     with the integer attribute evaluates the value, and an array subscript
//     there runs commands (`x='a[$(git push)]'; OPTIND=$x`). Bash's own
//     integer and behaviour variables are refused by name
//     (gitSensitiveAssignName), and a user attribute (`declare -i`/`-n`,
//     `typeset`, `local`) can only come from the same `bash -c` script, so
//     the caller only allows this at depth 0 when no word in the script,
//     including strings run later such as a trap body, can set one
//     (anySegmentMaySetVarAttributes). Deeper scripts (eval strings,
//     substitution bodies, `sh -c`) cannot see their parent's attributes and
//     keep failing closed.
//
// Literal-value assignments never reach here dynamic, so their verdict is
// unchanged.
func segmentOnlyAssignsVariables(seg shellSegment) bool {
	if seg.pipedStdin || seg.stdinDyn || len(seg.stdinBodies) > 0 {
		return false
	}
	sawAssign := false
	for _, tok := range seg.tokens {
		if !sawAssign && tok.literal && assignmentLeadKeywords[tok.val] {
			continue
		}
		if !tok.valueOnlyAssign || gitSensitiveAssignName(tok.val) {
			return false
		}
		for _, body := range tok.subcmds {
			if !substBodyIsInert(body, 1) {
				return false
			}
		}
		sawAssign = true
	}
	return sawAssign
}

// inertCommands only read files or their arguments and print text: none
// of them runs another program, interprets its input as code, or changes
// shell state (every body runs in a subshell). The command word must be the
// bare literal name, so `./ls` or `/tmp/x/cat` is not one of them, and the
// caller turns the shortcut off when the script can redefine a name
// (functions, alias, hash, PATH).
var inertCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "tac": true, "nl": true, "paste": true,
	"column": true, "date": true, "pwd": true, "basename": true,
	"dirname": true, "realpath": true, "readlink": true, "stat": true,
	"du": true, "df": true, "echo": true, "printf": true, "jq": true,
	"mktemp": true, "whoami": true, "id": true, "file": true, "seq": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true, "base64": true,
	"true": true, "false": true,
}

// substBodyIsInert reports whether a command substitution body only runs
// inertCommands. `sort --compress-program=PROG` runs PROG and `printf -v`
// assigns through an evaluated subscript, so those flags are refused.
func substBodyIsInert(body string, depth int) bool {
	if depth > 4 {
		return false
	}
	segments, subcmds, grouped := parseShellScript(body)
	if grouped {
		return false
	}
	for _, sub := range subcmds {
		if !substBodyIsInert(sub, depth+1) {
			return false
		}
	}
	for _, seg := range segments {
		if len(seg.stdinBodies) > 0 || seg.opaqueRedir {
			// Here-docs and here-strings: their expansions are checked
			// above, but nothing here needs one, so stay strict.
			return false
		}
		for _, tok := range seg.tokens {
			// `echo ${x@P}` runs prompt expansion and `echo $((x))`
			// evaluates x's text as arithmetic, whose subscripts run
			// commands. Globs (`ls dir/*`) only list file names.
			if tok.evaluates {
				return false
			}
		}
		i := 0
		for i < len(seg.tokens) && seg.tokens[i].literal && isEnvAssignmentToken(seg.tokens[i].val) && !gitSensitiveAssignName(seg.tokens[i].val) {
			i++
		}
		if i >= len(seg.tokens) {
			continue
		}
		cmd := seg.tokens[i]
		if !cmd.literal || !inertCommands[cmd.val] {
			return false
		}
		for _, arg := range seg.tokens[i+1:] {
			if cmd.val == "sort" && strings.HasPrefix(arg.val, "--comp") {
				return false
			}
			if cmd.val == "printf" && strings.HasPrefix(arg.val, "-v") {
				return false
			}
		}
	}
	return true
}

// attributeSettingWords can give a variable the integer or nameref
// attribute in the current shell (`declare -i f`, `local -n r`), or run text
// later in the current shell where one could hide: `eval`, `source`/`.`,
// `alias`, a `trap` body (a DEBUG trap runs before the very next command),
// a `mapfile -C` callback, PS4 under xtrace, `enable -f` loading a builtin.
//
// It also lists the words that change what a command name runs, which
// inertCommands relies on: `function`, `alias`, `hash`, `enable`, `shopt`
// (a `name() {` definition is caught by its parentheses).
var attributeSettingWords = []string{
	"declare", "typeset", "local", "eval", "source", "alias", "trap",
	"mapfile", "readarray", "enable", "PS4", "PROMPT_COMMAND",
	"function", "hash", "shopt",
}

// anySegmentMaySetVarAttributes reports whether any word of the script
// could set a variable attribute (attributeSettingWords). It matches each
// name as a whole word anywhere inside every token, not just as the command
// word, so wrappers (`builtin declare`), function bodies, and strings run
// later (`trap 'declare -gi f' DEBUG`, `cmd=declare`) are covered; `.` must
// be the whole word. Over-matching (`/usr/local/bin`) only keeps the old,
// fail-closed verdict.
func anySegmentMaySetVarAttributes(segments []shellSegment) bool {
	for _, seg := range segments {
		for _, tok := range seg.tokens {
			if tok.val == "." {
				return true
			}
			if isEnvAssignmentToken(tok.val) && gitSensitiveAssignName(tok.val) {
				// `PATH=/tmp/x; f=$(ls)` would resolve ls elsewhere.
				return true
			}
			for _, w := range attributeSettingWords {
				if containsASCIIWordFold(tok.val, w) {
					return true
				}
			}
		}
		for _, body := range seg.stdinBodies {
			for _, w := range attributeSettingWords {
				if containsASCIIWordFold(body, w) {
					return true
				}
			}
		}
	}
	return false
}

// stdinCarryingWords are the reserved words (and `exec`) whose pipe or
// redirect reaches commands in other segments: `{ ...; } < f`,
// `... | while ...; do ...; done`, `exec < f`, a function body.
var stdinCarryingWords = map[string]bool{
	"{": true, "}": true, "if": true, "then": true, "elif": true, "else": true,
	"fi": true, "while": true, "until": true, "for": true, "select": true,
	"do": true, "done": true, "case": true, "esac": true, "exec": true,
	"coproc": true, "function": true,
}

// anySegmentIsCompound reports whether any word of the script is one of
// stdinCarryingWords, so a pipe or redirect on one segment may feed another.
func anySegmentIsCompound(segments []shellSegment) bool {
	for _, seg := range segments {
		for _, tok := range seg.tokens {
			if stdinCarryingWords[tok.val] {
				return true
			}
		}
	}
	return false
}

// anySegmentReceivesStdin reports whether any segment is fed by a pipe, a
// here-doc or here-string, or an input redirect.
func anySegmentReceivesStdin(segments []shellSegment) bool {
	for _, seg := range segments {
		if seg.pipedStdin || seg.stdinDyn || len(seg.stdinBodies) > 0 {
			return true
		}
	}
	return false
}

// gitSensitiveVarNames are variables whose value changes which program a
// later command resolves to, which git config it reads, or what the shell
// runs on its own (BASH_ENV, PS4 under xtrace, PROMPT_COMMAND, EXECIGNORE),
// plus bash's own variables that carry the integer attribute or evaluate
// their value (OPTIND, RANDOM, HISTCMD, ...; every BASH* name is covered by
// prefix in gitSensitiveAssignName).
var gitSensitiveVarNames = map[string]bool{
	"PATH":            true,
	"HOME":            true,
	"XDG_CONFIG_HOME": true,
	"PREFIX":          true,
	"ENV":             true,
	"PS0":             true,
	"PS1":             true,
	"PS2":             true,
	"PS3":             true,
	"PS4":             true,
	"PROMPT_COMMAND":  true,
	"IFS":             true,
	"SHELLOPTS":       true,
	"CDPATH":          true,
	"EXECIGNORE":      true,
	"GLOBIGNORE":      true,
	"GLOBSORT":        true,
	"POSIXLY_CORRECT": true,
	"TIMEFORMAT":      true,
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"LD_AUDIT":        true,
	"SSH_ASKPASS":     true,
	"OPTIND":          true,
	"OPTERR":          true,
	"OPTARG":          true,
	"RANDOM":          true,
	"SRANDOM":         true,
	"SECONDS":         true,
	"LINENO":          true,
	"HISTCMD":         true,
	"HISTSIZE":        true,
	"HISTFILESIZE":    true,
	"EPOCHSECONDS":    true,
	"EPOCHREALTIME":   true,
	"COLUMNS":         true,
	"LINES":           true,
	"TMOUT":           true,
	"MAILCHECK":       true,
	"FUNCNEST":        true,
	"SHLVL":           true,
	"PPID":            true,
	"UID":             true,
	"EUID":            true,
	"GROUPS":          true,
	"FUNCNAME":        true,
	"DIRSTACK":        true,
	"PIPESTATUS":      true,
}

// gitSensitiveAssignName matches names exactly: the bash tool runs
// `bash -c`, where variable names are case-sensitive and git reads only the
// upper-case GIT_* environment, so `git_ref=$(git rev-parse HEAD)` is a
// plain shell variable.
func gitSensitiveAssignName(tok string) bool {
	name, _, _ := strings.Cut(tok, "=")
	return strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "BASH") || gitSensitiveVarNames[name]
}

func nonGitCommandEmbedsGitPush(args []shellToken, depth int) bool {
	for j, tok := range args {
		base := executableBaseName(tok.val)
		if base == "git-push" {
			return true
		}
		if base == "git" && gitTokensHavePushSubcommandDepth(args[j+1:], false, depth+1) {
			return true
		}
		if tokenContainsEmbeddedGitAndPush(tok.val) {
			return true
		}
	}
	return false
}

func tokenContainsEmbeddedGitAndPush(s string) bool {
	return containsASCIIWordFold(s, "git") && containsASCIIWordFold(s, "push")
}

func containsASCIIWordFold(s, word string) bool {
	lower := strings.ToLower(s)
	w := strings.ToLower(word)
	for offset := 0; offset < len(lower); {
		idx := strings.Index(lower[offset:], w)
		if idx < 0 {
			return false
		}
		pos := offset + idx
		end := pos + len(w)
		leftOK := pos == 0 || !isASCIIIdentChar(lower[pos-1])
		rightOK := end == len(lower) || !isASCIIIdentChar(lower[end])
		if leftOK && rightOK {
			return true
		}
		offset = pos + 1
	}
	return false
}

func isASCIIIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

var shellControlKeywords = map[string]bool{
	"!":       true,
	"{":       true,
	"}":       true,
	"if":      true,
	"then":    true,
	"elif":    true,
	"else":    true,
	"fi":      true,
	"while":   true,
	"until":   true,
	"do":      true,
	"done":    true,
	"case":    true,
	"esac":    true,
	"coproc":  true,
	"exec":    true,
	"builtin": true,
}

var shellInterpreterNames = map[string]bool{
	"sh":   true,
	"bash": true,
	"dash": true,
	"zsh":  true,
	"ksh":  true,
	"mksh": true,
	"ash":  true,
	"csh":  true,
	"tcsh": true,
	"fish": true,
}

type wrapperSkipResult struct {
	next       int
	nestedPush bool
	terminated bool
	viaXargs   bool
}

func skipShellWrappers(seg shellSegment, writesPushText bool, depth int) wrapperSkipResult {
	tokens := seg.tokens
	i := 0
	viaXargs := false
	sawNonXargsWrapper := false
	for i < len(tokens) {
		t := tokens[i]
		if t.dynamicAfterSlash {
			return wrapperSkipResult{next: i, viaXargs: viaXargs}
		}
		if isEnvAssignmentToken(t.val) || shellControlKeywords[t.val] {
			i++
			continue
		}
		if t.val == "function" && i+1 < len(tokens) {
			// `function name { body; }`: the body's first command follows
			// the name (and the `{` keyword skipped next round).
			i += 2
			continue
		}
		base := executableBaseName(t.val)
		if shellInterpreterNames[base] {
			return wrapperSkipResult{next: i, nestedPush: shellInvocationRunsGitPush(seg, tokens[i+1:], writesPushText, viaXargs, depth), terminated: true}
		}
		switch base {
		case "eval":
			return wrapperSkipResult{next: i, nestedPush: evalTokensRunGitPush(tokens[i+1:], viaXargs, depth), terminated: true}
		case ".", "source":
			return wrapperSkipResult{next: i, nestedPush: sourceRunsGitPush(tokens[i+1:], writesPushText, viaXargs), terminated: true}
		case "env":
			var envPush bool
			i, envPush = skipEnvWrapper(tokens, i+1, depth)
			if envPush {
				return wrapperSkipResult{next: i, nestedPush: true, terminated: true}
			}
			sawNonXargsWrapper = true
			continue
		case "xargs":
			viaXargs = true
			i = skipFlagRun(tokens, i+1, xargsFlagsWithArg)
			continue
		case "command", "nohup", "time":
			sawNonXargsWrapper = true
			i = skipFlagRun(tokens, i+1, nil)
			continue
		case "sudo":
			sawNonXargsWrapper = true
			i = skipFlagRun(tokens, i+1, sudoArgFlags)
			continue
		case "nice":
			sawNonXargsWrapper = true
			i = skipNiceFlags(tokens, i+1)
			continue
		case "timeout":
			sawNonXargsWrapper = true
			i = skipTimeoutWrapper(tokens, i+1)
			continue
		}
		break
	}
	if i >= len(tokens) && viaXargs && sawNonXargsWrapper {
		return wrapperSkipResult{next: i, nestedPush: true, terminated: true}
	}
	return wrapperSkipResult{next: i, viaXargs: viaXargs}
}

func shellInvocationRunsGitPush(seg shellSegment, args []shellToken, writesPushText, viaXargs bool, depth int) bool {
	if viaXargs {
		return true
	}
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].val, "-") {
		flagTok := args[i]
		if flagTok.dynamic {
			return true
		}
		i++
		flag := flagTok.val
		if flag == "-c" || (len(flag) > 2 && flag[0] == '-' && flag[1] != '-' && strings.HasSuffix(flag, "c")) {
			if i >= len(args) {
				return false
			}
			if args[i].dynamic {
				return true
			}
			return bashCommandRunsGitPushDepth(args[i].val, depth+1)
		}
	}
	if seg.pipedStdin || seg.stdinDyn {
		return true
	}
	for _, body := range seg.stdinBodies {
		if bashCommandRunsGitPushDepth(body, depth+1) {
			return true
		}
	}
	for _, arg := range args[i:] {
		if arg.dynamic || writesPushText {
			return true
		}
	}
	return false
}

func evalTokensRunGitPush(args []shellToken, viaXargs bool, depth int) bool {
	if viaXargs {
		return true
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a.dynamic {
			return true
		}
		parts = append(parts, a.val)
	}
	return bashCommandRunsGitPushDepth(strings.Join(parts, " "), depth+1)
}

func sourceRunsGitPush(args []shellToken, writesPushText, viaXargs bool) bool {
	if viaXargs || writesPushText {
		return true
	}
	for _, a := range args {
		if a.dynamic {
			return true
		}
	}
	return false
}

func skipEnvWrapper(tokens []shellToken, i, depth int) (int, bool) {
	for i < len(tokens) {
		tok := tokens[i]
		if isEnvAssignmentToken(tok.val) {
			i++
			continue
		}
		if !strings.HasPrefix(tok.val, "-") {
			break
		}
		if tok.dynamic {
			return i, true
		}
		if tok.val == "-S" || strings.HasPrefix(tok.val, "-S") || strings.HasPrefix(tok.val, "--split-string=") {
			spec, nextIdx, dyn := extractEnvSplitString(tokens, i)
			if dyn || bashCommandRunsGitPushDepth(spec, depth+1) {
				return nextIdx, true
			}
			i = nextIdx
			continue
		}
		if envFlagsWithArg[tok.val] && i+1 < len(tokens) {
			i += 2
			continue
		}
		i++
	}
	return i, false
}

func extractEnvSplitString(tokens []shellToken, i int) (string, int, bool) {
	arg := tokens[i].val
	switch {
	case arg == "-S":
		if i+1 >= len(tokens) {
			return "", i + 1, false
		}
		return tokens[i+1].val, i + 2, tokens[i+1].dynamic
	case strings.HasPrefix(arg, "--split-string="):
		return strings.TrimPrefix(arg, "--split-string="), i + 1, tokens[i].dynamic
	default:
		return arg[2:], i + 1, tokens[i].dynamic
	}
}

var (
	envFlagsWithArg   = map[string]bool{"-u": true, "-C": true}
	xargsFlagsWithArg = map[string]bool{"-I": true, "-i": true, "-L": true, "-l": true, "-n": true, "-P": true, "-s": true, "-d": true, "-a": true, "-E": true, "-e": true}
	sudoArgFlags      = map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true}
	timeoutArgFlags   = map[string]bool{"-k": true, "-s": true}
)

func skipFlagRun(tokens []shellToken, i int, flagsTakingArg map[string]bool) int {
	for i < len(tokens) && strings.HasPrefix(tokens[i].val, "-") {
		if tokens[i].dynamic {
			return i
		}
		if flagsTakingArg[tokens[i].val] && i+1 < len(tokens) {
			i += 2
			continue
		}
		i++
	}
	return i
}

func skipNiceFlags(tokens []shellToken, i int) int {
	if i < len(tokens) && tokens[i].val == "-n" && i+1 < len(tokens) {
		return i + 2
	}
	if i < len(tokens) && strings.HasPrefix(tokens[i].val, "-") && !tokens[i].dynamic {
		return i + 1
	}
	return i
}

func skipTimeoutWrapper(tokens []shellToken, i int) int {
	i = skipFlagRun(tokens, i, timeoutArgFlags)
	if i < len(tokens) && !tokens[i].dynamic {
		i++ // duration arg
	}
	return i
}

func gitTokensHavePushSubcommandDepth(args []shellToken, viaXargs bool, depth int) bool {
	if depth > 4 {
		return true
	}
	var aliases map[string]string
	hasConfigEnvAlias := false
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg.val == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg.val, "-") || arg.val == "-" {
			if arg.dynamic {
				return true
			}
			return gitSubcommandIsPush(arg.val, args[i+1:], aliases, hasConfigEnvAlias, viaXargs, depth)
		}
		if arg.dynamic {
			return true
		}
		if arg.val == "-h" || arg.val == "--help" || arg.val == "-v" || arg.val == "--version" {
			return false
		}
		name, val, dynSpec, isAlias := parseGitInlineAliasFlag(args, i)
		if dynSpec {
			return true
		}
		if isAlias {
			if aliases == nil {
				aliases = make(map[string]string)
			}
			aliases[name] = val
		} else if gitConfigEnvDefinesOrDynAlias(args, i) {
			hasConfigEnvAlias = true
		}
		if gitFlagsTakingArg[arg.val] {
			i += 2
			continue
		}
		i++
	}
	if i >= len(args) {
		return viaXargs
	}
	if args[i].dynamic {
		return true
	}
	return gitSubcommandIsPush(args[i].val, args[i+1:], aliases, hasConfigEnvAlias, viaXargs, depth)
}

func parseGitInlineAliasFlag(args []shellToken, i int) (name, val string, dynSpec, ok bool) {
	arg := args[i].val
	var spec string
	switch {
	case arg == "-c":
		if i+1 >= len(args) {
			return "", "", false, false
		}
		if args[i+1].dynamic {
			return "", "", true, false
		}
		spec = args[i+1].val
	case strings.HasPrefix(arg, "-c") && len(arg) > 2:
		spec = arg[2:]
	default:
		return "", "", false, false
	}
	key, v, hasEq := strings.Cut(strings.TrimSpace(spec), "=")
	if !hasEq {
		return "", "", false, false
	}
	keyLower := strings.ToLower(strings.TrimSpace(key))
	if !strings.HasPrefix(keyLower, "alias.") || len(keyLower) <= len("alias.") {
		return "", "", false, false
	}
	return keyLower[len("alias."):], strings.TrimSpace(v), false, true
}

func gitConfigEnvDefinesOrDynAlias(args []shellToken, i int) bool {
	arg := args[i].val
	var spec string
	switch {
	case arg == "--config-env":
		if i+1 >= len(args) {
			return false
		}
		if args[i+1].dynamic {
			return true
		}
		spec = args[i+1].val
	case strings.HasPrefix(arg, "--config-env="):
		spec = strings.TrimPrefix(arg, "--config-env=")
	default:
		return false
	}
	key, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(spec)), "=")
	return strings.HasPrefix(key, "alias.")
}

func gitSubcommandIsPush(sub string, rest []shellToken, aliases map[string]string, hasConfigEnvAlias, viaXargs bool, depth int) bool {
	if strings.EqualFold(sub, "push") {
		return true
	}
	if strings.EqualFold(sub, "submodule") {
		if viaXargs && len(rest) == 1 && strings.EqualFold(rest[0].val, "foreach") {
			return true
		}
		return nonGitCommandEmbedsGitPush(rest, depth)
	}
	if strings.EqualFold(sub, "config") && gitConfigSetsPushAlias(rest, depth) {
		return true
	}
	if hasConfigEnvAlias {
		return true
	}
	target, ok := aliases[strings.ToLower(strings.TrimSpace(sub))]
	if !ok {
		return false
	}
	return gitAliasTargetRunsPush(target, depth)
}

func gitConfigSetsPushAlias(rest []shellToken, depth int) bool {
	for idx, tok := range rest {
		lower := strings.ToLower(strings.TrimSpace(tok.val))
		if !strings.HasPrefix(lower, "alias.") {
			continue
		}
		if idx+1 >= len(rest) {
			return false
		}
		valTok := rest[idx+1]
		if valTok.dynamic || gitAliasTargetRunsPush(valTok.val, depth) {
			return true
		}
	}
	return false
}

func gitAliasTargetRunsPush(target string, depth int) bool {
	if strings.HasPrefix(target, "!") {
		return bashCommandRunsGitPushDepth(strings.TrimPrefix(target, "!"), depth+1)
	}
	subSegs, subCmds, _ := parseShellScript(target)
	for _, sc := range subCmds {
		if bashCommandRunsGitPushDepth(sc, depth+1) {
			return true
		}
	}
	if len(subSegs) == 0 {
		return false
	}
	return gitTokensHavePushSubcommandDepth(subSegs[0].tokens, false, depth+1)
}

func isEnvAssignmentToken(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if i == 0 {
			if !isASCIILetterOrUnderscore(c) {
				return false
			}
		} else if !isASCIIAlnumOrUnderscore(c) {
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
