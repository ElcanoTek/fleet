package agent

import (
	"slices"
	"sort"
)

// OptionalServerInfo is the catalog-row shape returned by MCPServerCatalog.
// It mirrors the frontend toggle affordance 1:1 so the HTTP handler can
// marshal straight to JSON without further transformation.
type OptionalServerInfo struct {
	// Name is the server's internal id (e.g. "indexexchange"). Stable
	// for cross-API references — toggles, system-prompt roster, logs.
	Name string `json:"name"`
	// DisplayName is the prettified label the settings UI renders.
	// Falls back to Name on the client when empty.
	DisplayName string   `json:"display_name,omitempty"`
	Description string   `json:"description"`
	ToolCount   int      `json:"tool_count"`
	Tools       []string `json:"tools"`
	// Beta surfaces a "BETA" badge in the settings UI next to the
	// server name. Set per-spec by the catalog wiring; carries no
	// runtime semantics — the gate is purely cosmetic + expectation-
	// setting ("this connector still flakes occasionally; we're
	// treating it as a feature preview").
	Beta bool `json:"beta,omitempty"`
	// EnabledByDefault is true when a brand-new conversation should start
	// with this server toggled on. The /mcp-servers preview reports it as
	// the initial `enabled` value; per-conversation state still wins once a
	// conversation has persisted its opt-in list.
	EnabledByDefault bool `json:"enabled_by_default,omitempty"`
}

// buildOptionalServerMetadata snapshots the Optional-server subset of the
// spec map into the catalog shape. Cheap: tool counts come from the live
// mcp.Client and tool descriptions are discarded (the settings UI only
// shows server names + human descriptions, not every tool's description).
// Returns a deterministic list sorted by server name so catalog JSON is
// stable across requests.
func (m *Manager) buildOptionalServerMetadata(specs map[string]MCPServerSpec) []OptionalServerInfo {
	out := make([]OptionalServerInfo, 0)
	serverTools := m.mcpClient.GetAllTools()
	for name, spec := range specs {
		if !spec.Enabled || !spec.Optional {
			continue
		}
		info := OptionalServerInfo{
			Name:             name,
			DisplayName:      spec.DisplayName,
			Description:      spec.Description,
			Beta:             spec.Beta,
			EnabledByDefault: spec.EnabledByDefault,
			// Empty (not nil) so JSON renders `[]` instead of `null`
			// when the underlying MCP fails to start. The picker calls
			// `.join()` on this client-side; null would crash the render.
			Tools: []string{},
		}
		for _, st := range serverTools {
			if st.ServerName != name {
				continue
			}
			if len(spec.ToolAllowlist) > 0 && !slices.Contains(spec.ToolAllowlist, st.Tool.Name) {
				continue
			}
			info.Tools = append(info.Tools, st.Tool.Name)
		}
		info.ToolCount = len(info.Tools)
		sort.Strings(info.Tools)
		out = append(out, info)
	}
	// Append synthetic native-tool entries — image generation reuses the
	// same picker so users have ONE place to toggle paid/optional tools.
	out = append(out, optionalNativeImageGenInfo())
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MCPServerCatalog returns the snapshot built at Manager.New() — list of
// Optional MCP servers the user can toggle from the conversation settings,
// plus any synthetic native-tool entries (image generation, etc.) that
// re-use the same picker UI for parity. Non-optional servers never appear
// here; they're always on.
func (m *Manager) MCPServerCatalog() []OptionalServerInfo {
	return m.optionalServerMetadata
}

// OptionalNativeImageGenName is the synthetic "server" name used in the
// catalog entry that gates the native generate_image tool. Stored in the
// same OptionalMCPServersEnabled list as real MCP servers so the UI picker
// and persistence path don't need a separate code path.
const OptionalNativeImageGenName = "image_generation"

// nativeOptInGate maps a native tool's name to the synthetic optional-server
// name that controls its visibility, or "" if the tool is always on. Used by
// buildFantasyTools to drop opt-in tools when the conversation hasn't
// enabled them, and by buildSystemPrompt to mirror the same gating in the
// dynamic tool advertisement section.
func nativeOptInGate(toolName string) string {
	if toolName == "generate_image" {
		return OptionalNativeImageGenName
	}
	return ""
}

// optionalNativeImageGenInfo is the catalog entry that surfaces the
// generate_image tool in the same Tools picker users already use to toggle
// gamma / DSP MCPs. Image gen costs ~$0.14/call on Nano Banana Pro, so it's
// off by default — and keeping it off also reduces "make a chart" ambiguity
// where the model might pick generate_image instead of run_python.
func optionalNativeImageGenInfo() OptionalServerInfo {
	return OptionalServerInfo{
		Name:        OptionalNativeImageGenName,
		DisplayName: "Image generation",
		Description: "Turn this on, then ask for an image (e.g., \"make me a banner of a golden retriever in a sunlit garden\") and the agent will generate one.",
		Tools:       []string{"generate_image"},
		ToolCount:   1,
	}
}
