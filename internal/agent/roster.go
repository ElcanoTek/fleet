package agent

import (
	"slices"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// These catalog filters remain driver-side data shapes. Tool registration and
// execution live only in agentcore.Run; keeping the aliases here does not
// recreate the retired legacy Fantasy builder.
type mcpAllowlist map[string][]string
type mcpOptionalSet map[string]bool

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
	// Accounts are the provisioned credential-seat names for this server (from
	// the `<VAR>_<ACCOUNT>` suffix scan over AccountVars). Names only — never
	// secret values. Empty when the server has no named accounts provisioned.
	Accounts []string `json:"accounts,omitempty"`
	// DataSources: manifest-declared external data identifiers this connector
	// touches ("s3://bucket", "jmap://host"). Display-only inventory for the
	// settings connections page; the credential's scope stays the authority.
	DataSources []string `json:"data_sources,omitempty"`
}

// AlwaysOnServerInfo is the public status row for an enabled non-optional MCP
// server. Available is derived from the live discovery catalog, not merely the
// manifest flag, so Operations can distinguish an always-on invariant from a
// connector that failed to expose any usable tools.
type AlwaysOnServerInfo struct {
	Name        string
	DisplayName string
	Description string
	ToolCount   int
	Available   bool
	Accounts    []string
}

// buildOptionalServerMetadata snapshots the Optional-server subset of the
// spec map into the catalog shape. Cheap: tool counts come from the public MCP
// catalog and tool descriptions are discarded (the settings UI only
// shows server names + human descriptions, not every tool's description).
// Returns a deterministic list sorted by server name so catalog JSON is
// stable across requests.
func (m *Manager) buildOptionalServerMetadata(specs map[string]MCPServerSpec) []OptionalServerInfo {
	m.mcpGatingMu.RLock()
	catalog := cloneMCPCatalog(m.mcpCatalog)
	accounts := cloneMCPAccounts(m.mcpAccounts)
	m.mcpGatingMu.RUnlock()
	if catalog == nil && m.mcpClient != nil {
		catalog = m.mcpClient.GetAllTools()
	}
	return buildOptionalServerMetadataFromCatalog(specs, catalog, accounts)
}

func buildOptionalServerMetadataFromCatalog(
	specs map[string]MCPServerSpec,
	serverTools []mcp.ServerTool,
	accounts map[string][]string,
) []OptionalServerInfo {
	out := make([]OptionalServerInfo, 0)
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
			DataSources:      append([]string(nil), spec.DataSources...),
			// Provisioned credential-account seats (names only) so the picker can
			// surface which accounts back this server. Nil when none are set.
			Accounts: append([]string(nil), accounts[name]...),
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

func (m *Manager) buildAlwaysOnServerMetadata(specs map[string]MCPServerSpec) []AlwaysOnServerInfo {
	m.mcpGatingMu.RLock()
	catalog := cloneMCPCatalog(m.mcpCatalog)
	accounts := cloneMCPAccounts(m.mcpAccounts)
	m.mcpGatingMu.RUnlock()
	if catalog == nil && m.mcpClient != nil {
		catalog = m.mcpClient.GetAllTools()
	}
	return buildAlwaysOnServerMetadataFromCatalog(specs, catalog, accounts)
}

func buildAlwaysOnServerMetadataFromCatalog(
	specs map[string]MCPServerSpec,
	serverTools []mcp.ServerTool,
	accounts map[string][]string,
) []AlwaysOnServerInfo {
	out := make([]AlwaysOnServerInfo, 0)
	for name, spec := range specs {
		if !spec.Enabled || spec.Optional {
			continue
		}
		info := AlwaysOnServerInfo{
			Name:        name,
			DisplayName: spec.DisplayName,
			Description: spec.Description,
			Accounts:    append([]string(nil), accounts[name]...),
		}
		for _, st := range serverTools {
			if st.ServerName != name {
				continue
			}
			// Seeing the server in live discovery makes it operationally
			// available even when its manifest allowlist hides this particular
			// tool from the agent.
			info.Available = true
			if len(spec.ToolAllowlist) == 0 || slices.Contains(spec.ToolAllowlist, st.Tool.Name) {
				info.ToolCount++
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MCPServerCatalog returns the snapshot built at Manager.New() — list of
// Optional MCP servers the user can toggle from the conversation settings,
// plus any synthetic native-tool entries (image generation, etc.) that
// re-use the same picker UI for parity. Non-optional servers never appear
// here; they're always on.
func (m *Manager) MCPServerCatalog() []OptionalServerInfo {
	m.mcpGatingMu.RLock()
	defer m.mcpGatingMu.RUnlock()
	return m.optionalServerMetadata
}

// AlwaysOnMCPServerCatalog returns enabled non-optional servers plus their live
// discovery status. These rows are informational: callers must not turn them
// into per-task choices because the runtime binds them independently.
func (m *Manager) AlwaysOnMCPServerCatalog() []AlwaysOnServerInfo {
	m.mcpGatingMu.RLock()
	defer m.mcpGatingMu.RUnlock()
	out := append([]AlwaysOnServerInfo(nil), m.alwaysOnServerMetadata...)
	for i := range out {
		out[i].Accounts = append([]string(nil), out[i].Accounts...)
	}
	return out
}

// OptionalNativeImageGenName is the synthetic "server" name used in the
// catalog entry that gates the native generate_image tool. Stored in the
// same OptionalMCPServersEnabled list as real MCP servers so the UI picker
// and persistence path don't need a separate code path.
const OptionalNativeImageGenName = "image_generation"

// nativeOptInGate maps a native tool's name to the synthetic optional-server
// name that controls its visibility, or "" if the tool is always on.
func nativeOptInGate(toolName string) string {
	if toolName == "generate_image" {
		return OptionalNativeImageGenName
	}
	return ""
}

// filterNativeToolsByOptIn applies the interactive native-tool visibility gate
// before the roster enters the single agentcore.Run builder. Keeping this as a
// data filter avoids reviving the retired second Fantasy registration loop.
func filterNativeToolsByOptIn(all []fantasy.AgentTool, enabled []string) []fantasy.AgentTool {
	optedIn := make(map[string]bool, len(enabled))
	for _, name := range enabled {
		if name = strings.TrimSpace(name); name != "" {
			optedIn[name] = true
		}
	}
	out := make([]fantasy.AgentTool, 0, len(all))
	for _, tool := range all {
		if tool == nil {
			continue
		}
		gate := nativeOptInGate(tool.Info().Name)
		if gate != "" && !optedIn[gate] {
			continue
		}
		out = append(out, tool)
	}
	return out
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
