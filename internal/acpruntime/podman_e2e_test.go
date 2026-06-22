package acpruntime

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// TestPodmanE2E exercises the REAL production path: the ClientRuntime spawns the
// native-agent image via `podman run -i`, the agent runs a real agentcore.Run
// loop against a fake LLM, issues a governed bash tool call that delegates over
// `_fleet/tool` back to the host Executor, and streams the result back. It
// proves the full no-DinD governed round-trip end-to-end.
//
// Gated on FLEET_ACP_E2E_IMAGE (the native-agent image tag) so the standard CI
// suite — which may not have podman or the image — skips it; the parity gate is
// covered without podman by TestACPRoundTripGovernedTool.
func TestPodmanE2E(t *testing.T) {
	image := os.Getenv("FLEET_ACP_E2E_IMAGE")
	if image == "" {
		t.Skip("set FLEET_ACP_E2E_IMAGE to the native-agent image tag to run the podman e2e")
	}

	// Fake LLM: round 0 calls bash, round 1 replies with final text.
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call-1", "echo hello-acp"),
		fakellm.TextStep("native-acp run complete"),
	}})

	// Listen on all interfaces so the container can reach us via the host IP.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: fake.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	hostIP := os.Getenv("FLEET_ACP_E2E_HOST_IP")
	if hostIP == "" {
		hostIP = outboundHostIP(t)
	}
	baseURL := "http://" + hostIP + ":" + itoaPort(port) + "/api/v1"

	rt := NewClientRuntime(ClientConfig{
		Image: image,
		// Model-endpoint env only — the agent's one allowed egress. MCP creds
		// are never shipped regardless of network.
		ModelEnv: map[string]string{
			"OPENROUTER_API_KEY":  "test-key",
			"OPENROUTER_BASE_URL": baseURL,
		},
		StartTimeout: 60 * time.Second,
	})

	exec := &recordingExecutor{}
	obs := &recordingObserver{}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := rt.Run(ctx,
		RunSpec{
			Mode: agentcore.ModeInteractive.String(), ModelSlug: "anthropic/claude-opus-4.8",
			SystemPrompt: "test", Temperature: 0, MaxTokens: 256,
		},
		"run echo", PromptMeta{},
		Deps{Executor: exec, Observer: obs},
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	exec.mu.Lock()
	cmds := append([]string(nil), exec.bashCmds...)
	exec.mu.Unlock()
	if len(cmds) != 1 || cmds[0] != "echo hello-acp" {
		t.Fatalf("host executor bash cmds = %v, want [echo hello-acp]", cmds)
	}
	if !strings.Contains(res.FinalText, "native-acp run complete") {
		t.Fatalf("final text = %q, want 'native-acp run complete'", res.FinalText)
	}
}

// TestPodmanE2E_MCPDelegatedNoCredsInContainer exercises the P-ACP-2b MCP
// credential-brokering path END-TO-END over podman: the agent advertises an
// mcp_<server>_<tool> tool (descriptor only, no creds), the fake LLM calls it,
// the call delegates over `_fleet/mcp` to the HOST broker which runs it host-side,
// and the result flows back. It asserts the SECURITY-CRITICAL invariant: the
// agent container holds NO MCP credential — only the model-endpoint key — yet the
// MCP call succeeds via host brokering.
//
// Gated on FLEET_ACP_E2E_IMAGE like TestPodmanE2E; the no-podman parity proof is
// TestACPGovern_MCPDelegatedHostSide.
func TestPodmanE2E_MCPDelegatedNoCredsInContainer(t *testing.T) {
	image := os.Getenv("FLEET_ACP_E2E_IMAGE")
	if image == "" {
		t.Skip("set FLEET_ACP_E2E_IMAGE to the native-agent image tag to run the podman e2e")
	}

	// Fake LLM: round 0 calls the MCP tool, round 1 replies with final text.
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(fakellm.ToolCall{ID: "m1", Name: "mcp_acme_lookup", Arguments: `{"q":"ping"}`}),
		fakellm.TextStep("mcp delegated ok"),
	}})

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: fake.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	hostIP := os.Getenv("FLEET_ACP_E2E_HOST_IP")
	if hostIP == "" {
		hostIP = outboundHostIP(t)
	}
	baseURL := "http://" + hostIP + ":" + itoaPort(port) + "/api/v1"

	rt := NewClientRuntime(ClientConfig{
		Image: image,
		// Model-endpoint env ONLY. Critically, NO MCP credential is in this map —
		// the MCP call is brokered host-side, so the container never sees creds.
		ModelEnv: map[string]string{
			"OPENROUTER_API_KEY":  "test-key",
			"OPENROUTER_BASE_URL": baseURL,
		},
		StartTimeout: 60 * time.Second,
	})

	broker := &recordingMCPBroker{resp: "host-side mcp result"}
	obs := &recordingObserver{}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := rt.Run(ctx,
		RunSpec{
			Mode: agentcore.ModeInteractive.String(), ModelSlug: "anthropic/claude-opus-4.8",
			SystemPrompt: "test", Temperature: 0, MaxTokens: 256,
			// Descriptor only — NO credential anywhere in the spec.
			MCPTools: []MCPToolDescriptor{{Server: "acme", Tool: "lookup", Description: "lookup"}},
		},
		"do the lookup", PromptMeta{},
		Deps{Executor: &recordingExecutor{}, Observer: obs, MCPBroker: broker},
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The MCP call executed HOST-side via the broker (creds applied host-side).
	broker.mu.Lock()
	calls := append([]mcpCall(nil), broker.calls...)
	broker.mu.Unlock()
	if len(calls) != 1 || calls[0].server != "acme" || calls[0].tool != "lookup" {
		t.Fatalf("host MCP broker calls = %+v, want one acme/lookup", calls)
	}
	if !strings.Contains(res.FinalText, "mcp delegated ok") {
		t.Fatalf("final text = %q, want 'mcp delegated ok'", res.FinalText)
	}
}

// outboundHostIP returns the host's primary outbound IP (the address a
// container reaches the host on under the default bridge/netavark network).
func outboundHostIP(t *testing.T) string {
	t.Helper()
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		t.Fatalf("resolve host IP: %v", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func itoaPort(p int) string {
	if p == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = byte('0' + p%10)
		p /= 10
	}
	return string(buf[i:])
}
