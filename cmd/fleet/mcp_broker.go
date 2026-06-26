package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
)

// runMCPBroker is `fleet mcp-broker`: the out-of-process MCP credential broker
// (issue #167). It owns the connector secrets — loaded from the env file into THIS
// process — builds the credentialed MCP client + its server subprocesses, and
// serves CallMCP + discovery over its stdio to the parent fleet process. The
// parent's agent loop then holds no connector secrets: it delegates every MCP call
// here, so "in-process" means only where the loop runs, not where secrets live.
//
// Frames ride stdin/stdout; all logging goes to stderr (the std logger's default)
// so it never corrupts the frame stream the parent decodes.
func runMCPBroker() error {
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.MCPServers = bundle.MCPServerConfigs()

	// The SAME builder the interactive Manager uses — one credential path.
	client := agent.BuildMCPClient(scheduledrun.BuildMCPSpecs(cfg))
	//nolint:gosec // G706 false positive: the only arg is an int tool count rendered with %d (no CR/LF can forge a log line); it is the size of the connected MCP catalog, not request input.
	log.Printf("mcp-broker: serving %d MCP tools over stdio", len(client.GetAllTools()))

	backend := &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
	}
	return mcpbroker.ServeStdio(context.Background(), backend)
}

// brokerBackend is the mcpbroker.Backend the broker process serves: calls run
// through the in-process localMCPBroker over the credentialed client; discovery
// returns the live tool catalog and the provisioned account names — resolved
// against THIS process's environment, where the secrets live. Only public data
// (rendered text, tool descriptors, account names) ever crosses back to the parent.
type brokerBackend struct {
	agentcore.MCPBroker // CallMCP, via localMCPBroker over the credentialed client
	client              *mcp.Client
}

func (b *brokerBackend) ListTools(context.Context) ([]mcpbroker.ToolDescriptor, error) {
	tools := b.client.GetAllTools()
	out := make([]mcpbroker.ToolDescriptor, 0, len(tools))
	for _, st := range tools {
		out = append(out, mcpbroker.ToolDescriptor{
			Server:      st.ServerName,
			Tool:        st.Tool.Name,
			Description: st.Tool.Description,
			InputSchema: st.Tool.InputSchema,
		})
	}
	return out, nil
}

func (b *brokerBackend) ListAccounts(_ context.Context, _ string, baseVars []string) ([]string, error) {
	return creds.AccountsFor(baseVars), nil
}
