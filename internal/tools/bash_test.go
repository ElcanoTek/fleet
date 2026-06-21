package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func parseBashResult(t *testing.T, raw string) bashResult {
	t.Helper()
	var result bashResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("bash output is not valid JSON: %v\nraw: %s", err, raw[:min(len(raw), 500)])
	}
	return result
}

func TestBashTool(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "echo 'Hello from bash'",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if !strings.Contains(result.Stdout, "Hello from bash") {
		t.Errorf("Expected stdout to contain 'Hello from bash', got %q", result.Stdout)
	}

	if result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", result.ExitCode)
	}

	if result.ExecutionTimeMs < 0 {
		t.Errorf("Expected non-negative execution time, got %d", result.ExecutionTimeMs)
	}

	if result.WorkingDir == "" {
		t.Error("Expected non-empty working directory")
	}
}

func TestBashToolStdoutStderrSeparation(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "echo stdout_msg; echo stderr_msg >&2",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if !strings.Contains(result.Stdout, "stdout_msg") {
		t.Errorf("Expected stdout to contain 'stdout_msg', got %q", result.Stdout)
	}

	if !strings.Contains(result.Stderr, "stderr_msg") {
		t.Errorf("Expected stderr to contain 'stderr_msg', got %q", result.Stderr)
	}

	// Verify stdout does NOT contain stderr
	if strings.Contains(result.Stdout, "stderr_msg") {
		t.Error("stdout should not contain stderr content")
	}
}

func TestBashToolWithWorkingDir(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command:    "pwd",
		WorkingDir: "/tmp",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if !strings.Contains(result.Stdout, "/tmp") {
		t.Errorf("Expected stdout to contain '/tmp', got %q", result.Stdout)
	}

	if result.WorkingDir != "/tmp" {
		t.Errorf("Expected working_directory to be '/tmp', got %q", result.WorkingDir)
	}
}

func TestBashToolWithPipes(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "echo 'line1\nline2\nline3' | grep line2",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if !strings.Contains(result.Stdout, "line2") {
		t.Errorf("Expected stdout to contain 'line2', got %q", result.Stdout)
	}
}

func TestBashToolError(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "nonexistentcommand",
	})

	if err != nil {
		t.Fatalf("Expected no error (errors are in output), got %v", err)
	}

	result := parseBashResult(t, raw)

	if result.ExitCode == 0 {
		t.Error("Expected non-zero exit code for missing command")
	}

	if result.Error == "" {
		t.Error("Expected non-empty error field")
	}

	// Stderr should contain the "not found" message
	if !strings.Contains(result.Stderr, "not found") {
		t.Errorf("Expected stderr to contain 'not found', got %q", result.Stderr)
	}
}

func TestBashToolTimeout(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command:        "sleep 60",
		TimeoutSeconds: 1,
	})

	if err != nil {
		t.Fatalf("Expected no error (timeout is in output), got %v", err)
	}

	result := parseBashResult(t, raw)

	if !strings.Contains(result.Error, "timed out") {
		t.Errorf("Expected error to mention timeout, got %q", result.Error)
	}
}

func TestBashToolLargeOutput(t *testing.T) {
	// Generate output larger than the truncation threshold (32KB)
	raw, err := runBash(context.Background(), BashParams{
		Command: "python3 -c \"print('x' * 50000)\"",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", result.ExitCode)
	}

	if result.TruncationInfo == nil {
		t.Fatal("Expected truncation_info to be present for large output")
	}

	if !result.TruncationInfo.StdoutTruncated {
		t.Error("Expected stdout_truncated to be true")
	}

	if result.TruncationInfo.StdoutFullBytes < 50000 {
		t.Errorf("Expected stdout_full_bytes >= 50000, got %d", result.TruncationInfo.StdoutFullBytes)
	}

	if result.TruncationInfo.StdoutFullPath == "" {
		t.Error("Expected stdout_full_path to be set")
	}

	// Verify the temp file exists and contains full output
	if result.TruncationInfo.StdoutFullPath != "" {
		data, readErr := os.ReadFile(result.TruncationInfo.StdoutFullPath)
		if readErr != nil {
			t.Fatalf("Failed to read temp file: %v", readErr)
		}
		if len(data) < 50000 {
			t.Errorf("Temp file should contain full output, got %d bytes", len(data))
		}
		_ = os.Remove(result.TruncationInfo.StdoutFullPath)
	}

	// Verify the inline output contains the TRUNCATED marker
	if !strings.Contains(result.Stdout, "[TRUNCATED") {
		t.Error("Expected inline stdout to contain [TRUNCATED marker")
	}
}

func TestBashToolExecutionTimeTracking(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "sleep 0.1",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if result.ExecutionTimeMs < 50 {
		t.Errorf("Expected execution time >= 50ms for sleep 0.1, got %d ms", result.ExecutionTimeMs)
	}
}

func TestBashToolJSONParseable(t *testing.T) {
	// Verify that all bash results are valid JSON that can be parsed
	// by the agent's ParseToolResult (which looks for stdout/stderr/vars fields)
	raw, err := runBash(context.Background(), BashParams{
		Command: "echo hello",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("bash output should be valid JSON, got: %v", err)
	}

	// Verify critical fields exist
	requiredFields := []string{"exit_code", "stdout", "stderr", "command", "working_directory", "execution_time_ms"}
	for _, field := range requiredFields {
		if _, ok := parsed[field]; !ok {
			t.Errorf("missing required field %q in bash output", field)
		}
	}
}

func TestBashToolBlockedCommands(t *testing.T) {
	blockedTests := []struct {
		name    string
		command string
	}{
		// Lateral movement
		{"ssh", "ssh user@host"},
		{"scp", "scp file.txt user@host:/path"},
		{"nc", "nc -l 8080"},
		{"telnet", "telnet host 23"},
		// Privilege escalation
		{"sudo", "sudo ls"},
		{"su", "su - root"},
		{"doas", "doas ls"},
		// Host management
		{"mount", "mount /dev/sda1 /mnt"},
		{"systemctl", "systemctl restart nginx"},
		{"reboot", "reboot"},
		{"dd", "dd if=/dev/zero of=/dev/sda"},
		{"iptables", "iptables -F"},
		// User management
		{"useradd", "useradd evil"},
		{"passwd", "passwd"},
	}

	for _, tt := range blockedTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runBash(context.Background(), BashParams{
				Command: tt.command,
			})

			if err == nil {
				t.Errorf("Expected error for blocked command '%s', got nil", tt.command)
			}

			if !strings.Contains(err.Error(), "blocked") {
				t.Errorf("Expected error message to contain 'blocked', got %v", err)
			}
		})
	}
}

func TestBashToolBlockedSensitivePaths(t *testing.T) {
	blocked := []string{
		"cat /etc/shadow",
		"cat ~/.ssh/id_rsa",
		"cat /root/.aws/credentials",
		`cat "~/.ssh/id_ed25519"`,
		"ls /root/.claude",
		"cat /root/.gnupg/pubring.kbx",
		"cat .env.production",
	}
	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			_, err := runBash(context.Background(), BashParams{Command: cmd})
			if err == nil || !strings.Contains(err.Error(), "blocked") {
				t.Errorf("expected %q to be blocked, got err=%v", cmd, err)
			}
		})
	}
}

func TestBashToolBlockedDestructivePatterns(t *testing.T) {
	blocked := []string{
		"rm -rf /",
		"rm  -rf   /",
		"RM -rf /",
		"rm -rf ~",
		"rm -rf /root",
		"rm -rf /etc",
		"git push --force origin main",
		"git push -f origin main",
	}
	for _, cmd := range blocked {
		t.Run(cmd, func(t *testing.T) {
			_, err := runBash(context.Background(), BashParams{Command: cmd})
			if err == nil || !strings.Contains(err.Error(), "blocked") {
				t.Errorf("expected %q to be blocked, got err=%v", cmd, err)
			}
		})
	}
}

func TestBashToolAllowedPackageInstalls(t *testing.T) {
	// Package installs aren't blocked at the application layer — they
	// just won't succeed as the unprivileged `chat` user under
	// ProtectSystem=strict. Verify the policy doesn't reject them.
	allowedTests := []struct {
		name    string
		command string
	}{
		{"pip install --user", "pip install --user nonexistent-pkg-12345 || true"},
		{"npm install -g", "npm install -g nonexistent-pkg-12345 || true"},
		{"dnf install", "dnf install --assumeno nonexistent-pkg-12345 || true"},
		{"go test -exec", "go test -exec echo . || true"},
	}

	for _, tt := range allowedTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runBash(context.Background(), BashParams{
				Command: tt.command,
			})

			// These commands should NOT be blocked by security policy
			if err != nil && strings.Contains(err.Error(), "blocked") {
				t.Errorf("Command '%s' should not be blocked, got error: %v", tt.command, err)
			}
		})
	}
}

func TestBashToolAllowedCommands(t *testing.T) {
	allowedTests := []struct {
		name    string
		command string
	}{
		{"git status", "git status || true"},
		{"python", "python3 --version"},
		{"ls", "ls -la"},
		{"curl", "curl --version"},
		{"wget", "wget --version || true"},
	}

	for _, tt := range allowedTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runBash(context.Background(), BashParams{
				Command: tt.command,
			})

			// These commands should not be blocked.
			if err != nil && strings.Contains(err.Error(), "blocked") {
				t.Errorf("Command '%s' should not be blocked, got error: %v", tt.command, err)
			}
		})
	}
}

func TestBashToolNonZeroExitPreservesOutput(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command: "echo 'partial output' && exit 42",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if result.ExitCode != 42 {
		t.Errorf("Expected exit code 42, got %d", result.ExitCode)
	}

	if !strings.Contains(result.Stdout, "partial output") {
		t.Errorf("Expected stdout to contain 'partial output' even on non-zero exit, got %q", result.Stdout)
	}
}

func TestBashToolPermissionError(t *testing.T) {
	tempDir := t.TempDir()
	blockedFile := tempDir + "/blocked.txt"
	if err := os.WriteFile(blockedFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if err := os.Chmod(blockedFile, 0o000); err != nil {
		t.Fatalf("failed to chmod test file: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(blockedFile, 0o600)
	})

	raw, err := runBash(context.Background(), BashParams{
		Command: "cat " + blockedFile,
	})

	if err != nil {
		t.Fatalf("Expected no hard error, got %v", err)
	}

	result := parseBashResult(t, raw)

	if result.ExitCode == 0 {
		t.Skip("Running as root, permission test not applicable")
	}

	// stderr should contain the permission error with the exact path
	if !strings.Contains(result.Stderr, "Permission denied") && !strings.Contains(result.Stderr, "permission denied") {
		t.Errorf("Expected permission denied in stderr, got %q", result.Stderr)
	}
}

func TestBashToolMalformedInput(t *testing.T) {
	// Empty command
	_, err := runBash(context.Background(), BashParams{
		Command: "",
	})
	if err == nil {
		t.Error("Expected error for empty command")
	}
}

func TestBashToolInvalidWorkingDir(t *testing.T) {
	raw, err := runBash(context.Background(), BashParams{
		Command:    "echo test",
		WorkingDir: "/nonexistent/path/12345",
	})

	if err != nil {
		t.Fatalf("Expected no hard error, got %v", err)
	}

	result := parseBashResult(t, raw)

	// Should fail with a clear error about the directory
	if result.Error == "" {
		t.Error("Expected error for nonexistent working directory")
	}
}

func TestBashToolSpecialCharOutput(t *testing.T) {
	// Output with unicode, quotes, backslashes — characters that can break JSON
	raw, err := runBash(context.Background(), BashParams{
		Command: `echo '日本語 "quotes" back\slash	tab'`,
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Must still be valid JSON even with special characters
	result := parseBashResult(t, raw)

	if result.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", result.ExitCode)
	}

	if result.Stdout == "" {
		t.Error("Expected non-empty stdout")
	}
}

func TestBashToolContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	raw, err := runBash(ctx, BashParams{
		Command:        "sleep 60",
		TimeoutSeconds: 300,
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	result := parseBashResult(t, raw)

	// Should show timeout error since parent context expired
	if result.Error == "" {
		t.Error("Expected error field to be set on context cancellation")
	}
}

// TestBashToolBackgroundChildDoesNotHang pins the WaitDelay fix folded in
// from cutlass's bash path: a command that exits immediately but leaves a
// background child holding the stdout/stderr pipes must return within
// sandbox.BashWaitDelay instead of blocking cmd.Run forever on the pipe
// copy. Exercises the unified sandboxed bash path (host sandbox in tests).
func TestBashToolBackgroundChildDoesNotHang(t *testing.T) {
	start := time.Now()
	raw, err := runBash(context.Background(), BashParams{
		// bash exits at once; `sleep` inherits the output pipes and holds
		// them open well past the tool call.
		Command:        "sleep 30 & echo started",
		TimeoutSeconds: 60,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	result := parseBashResult(t, raw)
	if !strings.Contains(result.Stdout, "started") {
		t.Errorf("Expected stdout to contain 'started', got %q", result.Stdout)
	}
	// sandbox.BashWaitDelay is 10s; allow generous slack but require it to
	// be far below the 30s the orphan holds the pipe.
	if elapsed > 20*time.Second {
		t.Fatalf("runBash blocked %v on a background child's pipe; WaitDelay regression", elapsed)
	}
}
