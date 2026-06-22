// Command fleet-native-agent is the agent-side of fleet's native ACP flavor. It
// is baked into the native sandbox image and spawned by fleet (the ACP client)
// via `podman run -i`. It wraps fleet's unified fantasy loop (agentcore.Run) AS
// a sandboxed ACP agent: the loop runs IN the container, but it has NO local
// executor — every bash/run_python call (and the structured run events) ride the
// ACP `_fleet/*` extension methods back to the client, which runs them in the
// host-managed hardened sandbox and applies full governance.
//
// Protocol stdout carries JSON-RPC; diagnostics go to stderr (split). The
// process reads OPENROUTER_API_KEY from its env (the model endpoint is allowed
// egress); MCP credentials are NEVER handed into this container — they are
// brokered host-side at delegation.
package main

import (
	"fmt"
	"log"
	"os"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[fleet-native-agent] ")

	// OPENROUTER_API_KEY is vendor-named (un-prefixed), like the rest of the
	// codebase reads it. The model endpoint is the only allowed egress; MCP
	// credentials are never handed into this container.
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "[fleet-native-agent] FATAL: OPENROUTER_API_KEY required")
		os.Exit(1)
	}

	resolver, err := agentcore.NewModelResolver(apiKey, agentcore.DefaultProviderHeaders)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[fleet-native-agent] FATAL: model resolver: %v\n", err)
		os.Exit(1)
	}

	runner := acpruntime.NewAgentRunner(resolver)
	conn := acp.NewAgentSideConnection(runner, os.Stdout, os.Stdin)
	runner.SetConn(conn)

	log.Println("ready on stdio")
	<-conn.Done()
	log.Println("peer disconnected")
}
