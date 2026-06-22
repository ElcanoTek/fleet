package acpingress

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// TestIngressSubprocessE2E exercises the REAL production transport: it builds the
// `fleet` binary, spawns `fleet acp` as an ACTUAL subprocess, and drives it over
// REAL stdio (the binary's stdin/stdout pipes, stderr → the test log) with a fake
// ACP client — initialize → new-session → prompt → assert a streamed
// session/update text chunk + end_turn. The LLM is the wire-compatible fake
// (reached via OPENROUTER_BASE_URL), so it needs NO live key; FLEET_MOCK_MODE=1
// gives a host-mode sandbox pool so no podman / sandbox image is required.
//
// Gated on FLEET_ACP_INGRESS_E2E + a Postgres DSN (the binary opens both the chat
// + sched DBs) so the standard CI `go` job — which builds no binary up-front and
// runs deterministically — skips it; the no-subprocess coverage is the in-memory
// round-trip + governance tests in agent_test.go. It mirrors the spawn/teardown
// shape of internal/acpruntime/podman_e2e_test.go.
//
// To run locally:
//
//	FLEET_ACP_INGRESS_E2E=1 \
//	FLEET_TEST_DATABASE_URL=postgres://… DATABASE_URL=postgres://… \
//	go test ./internal/acpingress/ -run TestIngressSubprocessE2E -v
func TestIngressSubprocessE2E(t *testing.T) {
	if os.Getenv("FLEET_ACP_INGRESS_E2E") == "" {
		t.Skip("set FLEET_ACP_INGRESS_E2E=1 (and the chat/sched Postgres DSNs) to run the fleet-acp subprocess e2e")
	}
	chatDSN := firstNonEmpty(os.Getenv("FLEET_ACP_INGRESS_CHAT_DSN"), os.Getenv("FLEET_TEST_DATABASE_URL"), os.Getenv("CHAT_TEST_DATABASE_URL"))
	schedDSN := firstNonEmpty(os.Getenv("FLEET_ACP_INGRESS_SCHED_DSN"), os.Getenv("DATABASE_URL"))
	if chatDSN == "" || schedDSN == "" {
		t.Skip("need a chat DSN (FLEET_TEST_DATABASE_URL) and a sched DSN (DATABASE_URL) for the subprocess e2e")
	}

	// 1. Build the fleet binary (the same one editors launch as `fleet acp`).
	bin := filepath.Join(t.TempDir(), "fleet")
	build := exec.Command("go", "build", "-o", bin, "github.com/ElcanoTek/fleet/cmd/fleet")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fleet: %v\n%s", err, out)
	}

	// 2. Fake LLM: one round of final text, reached via OPENROUTER_BASE_URL.
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("ingress subprocess e2e ok")}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: fake.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/api/v1"

	// 3. Spawn `fleet acp` over real stdio.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "acp")
	cmd.Env = append(os.Environ(),
		"FLEET_MOCK_MODE=1", // host-mode sandbox pool: no podman / image needed
		"OPENROUTER_API_KEY=test-key",
		"OPENROUTER_BASE_URL="+baseURL,
		"FLEET_ACP_MODEL=anthropic/claude-opus-4.8",
		"FLEET_ACP_PRINCIPAL=e2e@fleet.local",
		"FLEET_CHAT_DATABASE_URL="+chatDSN,
		"FLEET_SCHED_DATABASE_URL="+schedDSN,
		"FLEET_CLIENT_CONFIG_DIR="+firstNonEmpty(os.Getenv("FLEET_CLIENT_CONFIG_DIR"), repoConfigDir()),
	)
	cmd.Stderr = os.Stderr // diagnostics → the test log

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fleet acp: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// 4. Drive it with a fake ACP client over the subprocess's real stdio.
	editor := &subprocEditor{}
	conn := acp.NewClientSideConnection(editor, stdin, bufio.NewReader(stdout))

	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("say hi")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if got := editor.text(); !strings.Contains(got, "ingress subprocess e2e ok") {
		t.Fatalf("streamed text = %q, want the fake-LLM reply", got)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// repoConfigDir returns the in-repo default client-config bundle dir (used when
// FLEET_CLIENT_CONFIG_DIR is unset).
func repoConfigDir() string {
	// internal/acpingress → repo root is ../../
	if abs, err := filepath.Abs(filepath.Join("..", "..", "config", "default")); err == nil {
		return abs
	}
	return ""
}

// subprocEditor is a minimal ACP client that accumulates streamed text and
// auto-denies any permission request (the e2e prompt does not trigger one).
type subprocEditor struct {
	mu  sync.Mutex
	buf []byte
}

func (c *subprocEditor) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

func (c *subprocEditor) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	if u := p.Update; u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
		c.mu.Lock()
		c.buf = append(c.buf, u.AgentMessageChunk.Content.Text.Text...)
		c.mu.Unlock()
	}
	return nil
}

func (c *subprocEditor) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return rejectResp(p), nil
}

func (c *subprocEditor) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}
func (c *subprocEditor) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}
func (c *subprocEditor) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}
func (c *subprocEditor) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}
func (c *subprocEditor) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}
func (c *subprocEditor) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}
func (c *subprocEditor) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}
