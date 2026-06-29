package scheduledrun

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestBuildMCPSpecs_CarriesOptionalMetadata is the regression guard for the
// 128-tool-ceiling overflow: BuildMCPSpecs must propagate the Optional-server
// metadata into agent.MCPServerSpec. When Optional was dropped here, the chat
// Manager's optional-set came back empty, Gate-1 never skipped any optional
// connector, and every server's tools registered on every turn — blowing past
// the model's tool ceiling and aborting the turn with no reply.
func TestBuildMCPSpecs_CarriesOptionalMetadata(t *testing.T) {
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"opt_connector": {
				Type:             "stdio",
				Command:          "python3",
				Enabled:          true,
				Optional:         true,
				EnabledByDefault: true,
				DisplayName:      "Opt Connector",
				Description:      "an optional connector",
				Beta:             true,
			},
			"always_on": {
				Type:    "stdio",
				Command: "python3",
				Enabled: true,
				// Optional defaults false → always registered.
			},
		},
	}

	specs := BuildMCPSpecs(cfg)

	opt, ok := specs["opt_connector"]
	if !ok {
		t.Fatal("opt_connector missing from specs")
	}
	if !opt.Optional {
		t.Error("opt_connector.Optional was dropped — Gate-1 would never gate it (the ceiling bug)")
	}
	if !opt.EnabledByDefault || !opt.Beta || opt.DisplayName != "Opt Connector" || opt.Description != "an optional connector" {
		t.Errorf("optional metadata not fully propagated: %+v", opt)
	}

	if always, ok := specs["always_on"]; !ok || always.Optional {
		t.Errorf("always_on should be present and non-optional, got ok=%v spec=%+v", ok, always)
	}
}
