package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// Security model (read this before editing):
//
// In production, every bash invocation is dispatched to a per-turn
// rootless Podman container via the [sandbox] package. The container
// is --read-only / dropped caps; the workspace bind is the only writable
// surface. Network egress is per-turn: lockdown chats run with
// --network=none (egress structurally impossible), non-lockdown chats
// keep the rootless slirp4netns default so curl + pip install work.
//
// For tests and dev environments without Podman, the same code path
// runs through a host-mode sandbox that exec()s bash directly; that
// preserves legacy behavior and inherits the chat-server systemd
// profile (ProtectSystem=strict, ProtectHome=true, dropped caps,
// User=chat) — see deploy/chat-server.service.
//
// The application-level denylist below is THIRD-LAYER defense (clean
// error messages, audit-log signal on intent). It is not the primary
// control. Don't let it drift to primary control.
//
// What we block at this layer:
//  1. Privilege-escalation and host-management commands. Even though
//     the `chat` user can't use them, blocking them surfaces intent in
//     the audit log and prevents noisy failures.
//  2. Lateral-movement tools (ssh/scp/nc/telnet).
//  3. Reads/writes of well-known secret paths (~/.ssh, ~/.aws, .env,
//     etc.) — systemd already blocks most of this via ProtectHome, but
//     the workspace is writable, and nothing stops the agent from
//     leaking secrets it happens to read from env vars into a file.
//  4. Catastrophically destructive patterns (rm -rf /, fork bombs).
//  5. Shell obfuscation that would bypass (1)–(4).
var bannedCommands = []string{
	// Lateral movement
	"nc", "ncat", "scp", "ssh", "sftp", "telnet",
	// Privilege escalation / identity
	"sudo", "su", "doas", "pkexec",
	// User / auth management
	"passwd", "chpasswd", "useradd", "userdel", "usermod",
	"groupadd", "groupdel", "groupmod", "visudo",
	// Host / kernel management
	"mount", "umount", "mkfs", "mkswap", "swapon", "swapoff",
	"fdisk", "parted", "dd",
	"shutdown", "reboot", "halt", "poweroff", "kexec",
	"insmod", "rmmod", "modprobe",
	// Networking / firewall reconfiguration
	"iptables", "ip6tables", "nft", "ufw", "firewall-cmd",
	// Scheduling persistence
	"crontab", "at", "batch",
	// Service management (systemd already blocks via caps, but intent signal)
	"systemctl", "service", "journalctl", "loginctl",
}

// sensitivePathFragments are substrings in a bash command that indicate
// the agent is trying to touch secrets/config it has no legitimate
// reason to read or write. Matching is substring-based (case-sensitive
// on purpose for paths), applied after quote stripping. Defense in
// depth — systemd ProtectHome already hides most of these.
var sensitivePathFragments = []string{
	"/etc/shadow",
	"/etc/gshadow",
	"/etc/sudoers",
	"/root/.ssh",
	"/root/.aws",
	"/root/.gnupg",
	"/root/.claude", // this agent's own config/memory
	"/root/.config/gcloud",
	"~/.ssh",
	"~/.aws",
	"~/.gnupg",
	"~/.config/gcloud",
	".env.local",
	".env.production",
	"id_rsa",
	"id_ed25519",
	"id_ecdsa",
	"credentials.json",
}

// destructivePatterns are literal substrings that match commands so
// dangerous they're always a bug. We match after normalization (quote
// + backslash stripping, lowercase).
var destructivePatterns = []string{
	"rm -rf /",
	"rm -rf /*",
	"rm -rf ~",
	"rm -rf /root",
	"rm -rf /home",
	"rm -rf /opt",
	"rm -rf /etc",
	"rm -rf /var",
	"rm -rf /usr",
	":(){:|:&};:", // classic fork bomb (post-normalize)
	"chmod -r 777 /",
	"chown -r",
}

// Blocked argument patterns (command + args to block)
type blockedPattern struct {
	command string
	args    []string
}

var blockedPatterns = []blockedPattern{
	// git push --force to shared remotes — agent should use --force-with-lease
	// and only after explicit user approval. Blocking here means the approval
	// gate (E) is the only path.
	{command: "git", args: []string{"push", "--force"}},
	{command: "git", args: []string{"push", "-f"}},
}

// BashParams are the typed parameters for the bash tool.
type BashParams struct {
	Command        string `json:"command" description:"The bash command to execute. Can include pipes, redirects, and command chaining (&&, ||, ;)."`
	WorkingDir     string `json:"working_dir,omitempty" description:"The working directory to execute the command in. Defaults to current directory."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" description:"Maximum time in seconds to wait for command completion. Defaults to 300 (5 minutes)."`
}

// NewBashTool creates a fantasy.AgentTool bound to a per-turn sandbox.
// The caller MUST pass a non-nil sandbox from the per-turn pool;
// production turns flow through agent/session.go which Take()s a
// container before constructing the tool. A nil sandbox here is a
// programmer error and surfaces as a tool-call failure at runtime.
func NewBashTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	description := `Executes bash commands inside the per-turn sandbox container. Use for local git, file operations, document conversion (pandoc), image manipulation (ImageMagick), shell pipelines, and one-liners.

CLI TOOLS available inside the sandbox:
- Shell + coreutils: bash, ls, cat, cp, mv, rm, mkdir, head, tail, wc, sort, uniq, tr, cut, tee, xargs.
- Search/parse: grep, ripgrep (` + "`rg`" + `), sed, awk, find, jq, less.
- Version control: git (local only — see NETWORK).
- Documents: pandoc (markdown ↔ html ↔ docx ↔ pdf etc.).
- Images: ImageMagick (` + "`convert`" + `, ` + "`identify`" + `, ` + "`montage`" + `, ` + "`mogrify`" + `).
- Python: ` + "`python3`" + ` for one-liners (full data stack pre-imported via the run_python tool — prefer that for non-trivial work).

STATE — each bash call is INDEPENDENT: cwd, env vars, and shell variables do NOT carry across calls. Chain multi-step work with ` + "`&&`" + ` or ` + "`;`" + ` in a single command. Files written in a previous call ARE still on disk in your workspace; only in-memory shell state is reset.

FILESYSTEM — your cwd is a private per-conversation scratch directory inside the sandbox. Bare writes (` + "`touch foo`" + `, ` + "`echo > file`" + `) land in THIS chat's scratch and are invisible to other chats. The same workspace is visible to run_python and to the file tools, so you can write a CSV in bash and read it from Python in the next call. Supporting docs — protocols/, personas/, system_prompts/ — are exposed as symlinks inside your scratch so relative reads still work.

NETWORK — outbound HTTP works in normal chats: ` + "`curl`" + `, ` + "`wget`" + `, ` + "`git clone`" + `, and ` + "`pip install`" + ` reach the public internet via rootless slirp4netns. Lockdown chats run with the network namespace sealed (no DNS, no routes); those calls will fail there. The webfetch MCP also works in both modes and stages downloads into your workspace, so prefer it when you need a one-off file and aren't sure which mode you're in.

SECURITY:
- Privilege escalation (sudo/su/doas), host management (mount/systemctl/iptables/reboot), and lateral-movement tools (ssh/scp/nc/telnet) are blocked.
- Reads/writes of secret paths (~/.ssh, ~/.aws, .env files, id_rsa, etc.) are blocked.
- Catastrophic patterns (rm -rf /, fork bombs, git push --force) are blocked.
- Shell obfuscation (eval, $(...), backticks, complex ${...}) is blocked.
- Every invocation is audit-logged.

For long-running commands (servers, watch tasks), set timeout_seconds.`

	return fantasy.NewAgentTool("bash", description,
		func(ctx context.Context, params BashParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runBashWithSandbox(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

// RunBashForApproval executes a bash invocation that was previously
// staged in the approvals table. Exposed for the httpapi approval
// handler — same safety checks and audit logging apply as the agent-side
// path.
//
// The caller (httpapi/approvals.go) MUST Take() a sandbox from the
// manager's warm pool and pass it here, then Close it after the call.
// nil is a programmer error and returns an error rather than silently
// running on the host.
func RunBashForApproval(ctx context.Context, sb *sandbox.Sandbox, params BashParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("bash approval requires a sandbox; pool.Take returned nil or was bypassed")
	}
	return runBashWithSandbox(ctx, sb, params)
}

// extractCommands extracts all potential command names from a bash command string.
// This handles pipes, command chaining, subshells, and command substitution.
func extractCommands(command string) []string {
	var commands []string

	// Normalize: remove quotes content to avoid false negatives from quoted commands
	// but track if banned commands appear even in quotes (defense in depth)
	normalized := command

	// Split on command separators: |, &&, ||, ;, newline
	// Also handle $(...) and `...` command substitution
	separators := []string{"|", "&&", "||", ";", "\n", "$(", "`", "(", ")"}

	// Replace separators with spaces for splitting
	for _, sep := range separators {
		normalized = strings.ReplaceAll(normalized, sep, " ")
	}

	// Split into tokens
	tokens := strings.Fields(normalized)

	// Track if next token could be a command (after separator or at start)
	isCommandPosition := true

	for _, token := range tokens {
		// Skip common bash operators and redirects
		if token == "<" || token == ">" || token == ">>" || token == "2>" ||
			token == "2>>" || token == "&>" || token == "<<<" || token == "<<" ||
			token == "<&" || token == ">&" {
			isCommandPosition = false
			continue
		}

		// Skip if it looks like a flag
		if strings.HasPrefix(token, "-") && len(token) > 1 {
			isCommandPosition = false
			continue
		}

		// Skip if it looks like a variable assignment (VAR=value)
		if strings.Contains(token, "=") && !strings.HasPrefix(token, "=") {
			// Check if this is env VAR=value cmd pattern
			isCommandPosition = true
			continue
		}

		// Skip redirection targets (numbers followed by nothing)
		if len(token) == 1 && token >= "0" && token <= "9" {
			continue
		}

		if isCommandPosition {
			// Extract base command name (remove path)
			baseCmd := token
			if strings.Contains(baseCmd, "/") {
				parts := strings.Split(baseCmd, "/")
				baseCmd = parts[len(parts)-1]
			}

			// Remove any trailing/leading quotes
			baseCmd = strings.Trim(baseCmd, `"'`)

			if baseCmd != "" {
				commands = append(commands, baseCmd)
			}
		}

		isCommandPosition = false
	}

	return commands
}

// normalizeCommand applies normalizations to detect obfuscation attempts
func normalizeCommand(cmd string) string {
	// Convert to lowercase for case-insensitive matching
	normalized := strings.ToLower(cmd)

	// Remove common obfuscation characters that bash ignores
	// Note: This is defense in depth - bash would execute these
	normalized = strings.ReplaceAll(normalized, `\`, "")
	normalized = strings.ReplaceAll(normalized, `"`, "")
	normalized = strings.ReplaceAll(normalized, `'`, "")

	return normalized
}

// normalizeWhitespace collapses runs of ASCII whitespace into single
// spaces. Used by the destructive-pattern matcher so `rm  -rf   /` and
// `rm\t-rf\n/` both match the literal `rm -rf /`.
func normalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// containsExecCommand checks if the command contains 'exec' as a standalone command
// (not as a flag like -exec used by find, go test, etc.)
func containsExecCommand(cmdLower string) bool {
	// Check for 'exec' at the start of the command
	if strings.HasPrefix(cmdLower, "exec ") || strings.HasPrefix(cmdLower, "exec\t") {
		return true
	}

	// Check for 'exec' after command separators (|, &&, ||, ;)
	// These patterns indicate exec is being used as a command, not a flag
	dangerousPatterns := []string{
		"; exec ", "; exec\t",
		"| exec ", "| exec\t",
		"&& exec ", "&& exec\t",
		"|| exec ", "|| exec\t",
	}

	for _, pattern := range dangerousPatterns {
		if strings.Contains(cmdLower, pattern) {
			return true
		}
	}

	return false
}

// checkCommandSafety validates that the command doesn't contain blocked patterns
func checkCommandSafety(command string) error {
	// First, check for obvious obfuscation attempts in the raw command
	// These patterns are almost always malicious
	obfuscationPatterns := []string{
		`$'`,       // ANSI-C quoting
		`\x`,       // Hex escapes
		`\u`,       // Unicode escapes
		"${",       // Variable expansion that could hide commands
		"`",        // Backtick command substitution
		"$(",       // Command substitution
		"eval ",    // Eval is dangerous
		"eval\t",   // Eval with tab
		"source ",  // Source can run scripts
		"source\t", // Source with tab
		". /",      // Dot-sourcing absolute path
	}

	cmdLower := strings.ToLower(command)
	for _, pattern := range obfuscationPatterns {
		if strings.Contains(cmdLower, pattern) {
			// Allow simple variable references like $HOME, $PATH but not ${...} expansion
			if pattern == "${" {
				// Check if this is complex expansion vs simple like ${HOME}
				// Block ${VAR:-default}, ${VAR:+alt}, ${!prefix*}, etc.
				if strings.Contains(command, "${!") ||
					strings.Contains(command, ":-") ||
					strings.Contains(command, ":+") ||
					strings.Contains(command, ":?") ||
					strings.Contains(command, "##") ||
					strings.Contains(command, "%%") {
					return fmt.Errorf("complex variable expansion is blocked for security reasons")
				}
				continue
			}
			return fmt.Errorf("potentially dangerous shell construct '%s' is blocked", pattern)
		}
	}

	// Check for dangerous 'exec' usage (but allow -exec flags used by find, go test, etc.)
	// We block 'exec' only when it appears as a standalone command (start of line or after separator)
	if containsExecCommand(cmdLower) {
		return fmt.Errorf("potentially dangerous shell construct 'exec' is blocked")
	}

	// Sensitive-path fragments: any bash command that mentions one of
	// these secret/config paths is rejected. Matching is case-sensitive
	// (paths on Linux are case-sensitive), with quotes stripped so
	// `cat "~/.ssh/id_rsa"` is caught.
	unquoted := strings.ReplaceAll(strings.ReplaceAll(command, `"`, ""), `'`, "")
	for _, frag := range sensitivePathFragments {
		if strings.Contains(unquoted, frag) {
			return fmt.Errorf("access to sensitive path %q is blocked", frag)
		}
	}

	// Destructive patterns: match against a normalized form
	// (lowercase, whitespace collapsed, quotes stripped) so
	// `  RM -rf  /  ` is caught the same as `rm -rf /`.
	normalized := normalizeWhitespace(strings.ToLower(unquoted))
	for _, pat := range destructivePatterns {
		if strings.Contains(normalized, pat) {
			return fmt.Errorf("catastrophically destructive pattern %q is blocked", pat)
		}
	}

	// Extract all commands from the input
	commands := extractCommands(command)

	// Check each extracted command against the blocklist
	for _, cmd := range commands {
		normalizedCmd := normalizeCommand(cmd)

		// Check against banned commands (case-insensitive)
		for _, banned := range bannedCommands {
			if normalizedCmd == strings.ToLower(banned) {
				return fmt.Errorf("command '%s' is blocked for security reasons", banned)
			}
		}
	}

	// Check for blocked argument patterns (more strict matching)
	for _, pattern := range blockedPatterns {
		patternLower := strings.ToLower(pattern.command)

		// Check if the command appears in the input
		for _, cmd := range commands {
			if normalizeCommand(cmd) == patternLower {
				// Found the command, now check for blocked argument patterns
				allArgsPresent := true
				for _, arg := range pattern.args {
					argLower := strings.ToLower(arg)
					if !strings.Contains(cmdLower, argLower) {
						allArgsPresent = false
						break
					}
				}
				if allArgsPresent {
					return fmt.Errorf("command pattern '%s %s' is blocked for security reasons",
						pattern.command, strings.Join(pattern.args, " "))
				}
			}
		}
	}

	return nil
}

// bashOutputTruncateThreshold is the max bytes of stdout or stderr to include
// inline in the structured response. Outputs larger than this are saved to a
// temp file and head+tail excerpts are returned inline.
const bashOutputTruncateThreshold = 32768 // ~8K tokens

// bashTruncateHeadTail controls how many bytes of head and tail to keep inline
// when truncating large output.
const bashTruncateHeadTail = 4096

const truncationTempFileMaxAge = 24 * time.Hour

// bashResult is the structured JSON response returned by the bash tool.
type bashResult struct {
	ExitCode        int             `json:"exit_code"`
	Stdout          string          `json:"stdout"`
	Stderr          string          `json:"stderr"`
	Command         string          `json:"command"`
	WorkingDir      string          `json:"working_directory"`
	ExecutionTimeMs int64           `json:"execution_time_ms"`
	Error           string          `json:"error,omitempty"`
	TruncationInfo  *truncationInfo `json:"truncation_info,omitempty"`
}

type truncationInfo struct {
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	StdoutFullPath  string `json:"stdout_full_path,omitempty"`
	StderrFullPath  string `json:"stderr_full_path,omitempty"`
	StdoutFullBytes int    `json:"stdout_full_bytes,omitempty"`
	StderrFullBytes int    `json:"stderr_full_bytes,omitempty"`
}

// truncateWithFile saves full output to a temp file and returns head+tail with
// a truncation marker. Returns the truncated string and the temp file path.
func truncateWithFile(output []byte, prefix string) (string, string) {
	cleanupOldTruncationFiles()
	tmpFile, err := os.CreateTemp("", fmt.Sprintf("chat-%s-*.txt", prefix))
	if err != nil {
		// If we can't create a temp file, return head+tail with a warning
		head := string(output[:bashTruncateHeadTail])
		tail := string(output[len(output)-bashTruncateHeadTail:])
		return head + fmt.Sprintf("\n\n[TRUNCATED — %d bytes total, temp file creation failed: %v. Re-run with smaller output, or capture the value via run_python `return_vars` (those are never truncated).]\n\n", len(output), err) + tail, ""
	}
	path := tmpFile.Name()
	_, _ = tmpFile.Write(output)
	_ = tmpFile.Close()

	head := string(output[:bashTruncateHeadTail])
	tail := string(output[len(output)-bashTruncateHeadTail:])
	return head + fmt.Sprintf("\n\n[TRUNCATED — %d bytes total; head+tail shown above. Recover the FULL bytes with `view_file path=%s` (best for inspecting), or — if you need to feed them back to another tool — re-run inside run_python and capture via `return_vars` (vars are never truncated; do NOT copy-paste the head+tail above as if it were the whole payload).]\n\n", len(output), path) + tail, path
}

// auditBashInvocation appends one JSON line describing a bash
// invocation to an audit log. Best-effort — on failure we log a warning
// once per process and otherwise stay silent so audit problems never
// break a turn. Log dir resolution: $FLEET_AUDIT_DIR (or legacy
// $CHAT_AUDIT_DIR), else $FLEET_DATA_DIR/audit (or legacy
// $CHAT_DATA_DIR/audit), else ./data/audit.
func auditBashInvocation(command, workingDir string, exitCode int, elapsedMs int64, blockedReason string) {
	dir := fleetEnv("AUDIT_DIR")
	if dir == "" {
		dataDir := fleetEnv("DATA_DIR")
		if dataDir == "" {
			dataDir = "./data"
		}
		dir = filepath.Join(dataDir, "audit")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { // audit dir built from operator-set env or default
		auditWarnOnce("mkdir audit dir failed: " + err.Error())
		return
	}
	path := filepath.Join(dir, "bash.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // audit log readable by the chat group
	if err != nil {
		auditWarnOnce("open audit log failed: " + err.Error())
		return
	}
	defer f.Close()

	entry := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"command":    command,
		"cwd":        workingDir,
		"exit_code":  exitCode,
		"elapsed_ms": elapsedMs,
	}
	if blockedReason != "" {
		entry["blocked"] = blockedReason
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// auditWarnedAbout is touched from concurrent bash invocations across
// turns; an unsynchronized map write here is a process-fatal crash, so
// use sync.Map.
var auditWarnedAbout sync.Map

func auditWarnOnce(msg string) {
	if _, loaded := auditWarnedAbout.LoadOrStore(msg, true); loaded {
		return
	}
	log.Printf("bash audit: %s", msg) // msg comes from our own audit code, not user input
}

func cleanupOldTruncationFiles() {
	patterns := []string{
		"chat-stdout-*.txt",
		"chat-stderr-*.txt",
		"chat-python-stdout-*.txt",
		"chat-python-stderr-*.txt",
		"chat-webfetch-*.txt",
		"chat-output-*.txt",
	}
	now := time.Now()
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(os.TempDir(), pattern))
		if err != nil {
			continue
		}
		for _, path := range matches {
			info, statErr := os.Stat(path)
			if statErr != nil {
				continue
			}
			if now.Sub(info.ModTime()) <= truncationTempFileMaxAge {
				continue
			}
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				log.Printf("Warning: failed to remove old truncation temp file %s: %v", path, removeErr)
			}
		}
	}
}

// runBash is a test-only convenience that constructs a fresh host
// sandbox per call. Production paths (agent turns + approval handler)
// always pass a pool-issued container sandbox to runBashWithSandbox
// directly; the host-mode shortcut here is unreachable from any
// non-test code.
func runBash(ctx context.Context, params BashParams) (string, error) {
	return runBashWithSandbox(ctx, sandbox.NewHost(nil), params)
}

// runBashWithSandbox is the production dispatch path. The caller MUST
// pass a non-nil per-turn sandbox handed out by the pool — there is no
// host-mode fallback in production, because letting agent-emitted code
// execute as the chat-server process is the whole risk we're guarding
// against. A nil sandbox returns an error rather than silently running
// on the host.
func runBashWithSandbox(ctx context.Context, sb *sandbox.Sandbox, params BashParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("bash requires a sandbox; pool.Take returned nil or was bypassed")
	}

	command := params.Command
	if command == "" {
		return "", fmt.Errorf("command is required")
	}

	workingDir, err := resolveBashWorkingDir(ctx, params.WorkingDir)
	if err != nil {
		return "", err
	}

	if err := checkCommandSafety(command); err != nil {
		auditBashInvocation(command, workingDir, -1, 0, err.Error())
		return "", err
	}

	timeoutSeconds := params.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}

	start := time.Now()
	out, runErr := sb.RunBash(ctx, sandbox.BashRequest{
		Command:    command,
		WorkingDir: workingDir,
		Timeout:    time.Duration(timeoutSeconds) * time.Second,
	})
	elapsed := time.Since(start)

	result := bashResult{
		Command:         command,
		WorkingDir:      workingDir,
		ExecutionTimeMs: elapsed.Milliseconds(),
		Stdout:          string(out.Stdout),
		Stderr:          string(out.Stderr),
		ExitCode:        out.ExitCode,
	}
	switch {
	case out.TimedOut:
		result.Error = fmt.Sprintf("command timed out after %d seconds", timeoutSeconds)
	case runErr != nil:
		result.Error = runErr.Error()
		// If the sandbox itself failed to start the process (binary
		// missing, container dead), surface a -1 exit so callers can
		// distinguish "command failed" from "command never ran".
		if result.ExitCode == 0 {
			result.ExitCode = -1
		}
	case result.ExitCode != 0:
		// Non-zero exit but the process ran. Match the legacy shape —
		// the original implementation captured exec.ExitError.Error()
		// here, which is "exit status N".
		result.Error = fmt.Sprintf("exit status %d", result.ExitCode)
	}

	// Surface the in-memory output cap (sandbox.BashOutputCaptureCap):
	// bytes beyond the cap were discarded before they could OOM the
	// process. Folded in from cutlass's direct-exec bash path.
	if out.StdoutDiscarded > 0 || out.StderrDiscarded > 0 {
		capNote := fmt.Sprintf(
			"output exceeded the %d MB capture cap (stdout dropped %d bytes, stderr dropped %d bytes); re-run with filtered/paginated output",
			sandbox.BashOutputCaptureCap/(1024*1024), out.StdoutDiscarded, out.StderrDiscarded)
		if result.Error != "" {
			result.Error += " | " + capNote
		} else {
			result.Error = capNote
		}
	}

	if len(out.Stdout) > bashOutputTruncateThreshold || len(out.Stderr) > bashOutputTruncateThreshold {
		ti := &truncationInfo{}
		if len(out.Stdout) > bashOutputTruncateThreshold {
			truncated, path := truncateWithFile(out.Stdout, "stdout")
			ti.StdoutTruncated = true
			ti.StdoutFullPath = path
			ti.StdoutFullBytes = len(out.Stdout)
			result.Stdout = truncated
		}
		if len(out.Stderr) > bashOutputTruncateThreshold {
			truncated, path := truncateWithFile(out.Stderr, "stderr")
			ti.StderrTruncated = true
			ti.StderrFullPath = path
			ti.StderrFullBytes = len(out.Stderr)
			result.Stderr = truncated
		}
		result.TruncationInfo = ti
	}

	auditBashInvocation(command, workingDir, result.ExitCode, result.ExecutionTimeMs, "")

	jsonBytes, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		// JSON marshal of a plain struct should never fail; fall back to a
		// best-effort plain-text rendering rather than poisoning the turn.
		//nolint:nilerr // intentional: marshal of a plain struct should never fail; fall back to a best-effort text rendering rather than erroring the turn.
		return fmt.Sprintf("Exit Code: %d\nStdout: %s\nStderr: %s\nError: %v",
			result.ExitCode, result.Stdout, result.Stderr, result.Error), nil
	}
	return string(jsonBytes), nil
}

// resolveBashWorkingDir decides what cwd to run a bash command in.
// Existing logic, lifted out of runBash for clarity.
func resolveBashWorkingDir(ctx context.Context, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	if convID := ConversationIDFromContext(ctx); convID != "" {
		dir, err := EnsureWorkspaceDir(convID)
		if err != nil {
			wd, wdErr := os.Getwd()
			if wdErr != nil {
				return "", fmt.Errorf("error getting current directory: %w", wdErr)
			}
			log.Printf("workspace ensure failed for conv %s (%v); using cwd %s", convID, err, wd)
			return wd, nil
		}
		if abs, absErr := filepath.Abs(dir); absErr == nil {
			return abs, nil
		}
		return dir, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("error getting current directory: %w", err)
	}
	return wd, nil
}
