package agent

import (
	"slices"

	"github.com/ElcanoTek/fleet/internal/acpruntime"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Host-side governance brokers for the native-acp flavor (P-ACP-2b). The agent
// container runs the SAME agentcore.Run loop with the SAME policy, but the
// governed EFFECTS that must stay host-side ride `_fleet/*` back here:
//
//   - MCP tool calls execute against the per-task credentialed mcp.Client
//     (mcpBroker) — MCP credentials NEVER enter the agent container;
//   - approval / memory / note staging hits the real stagers (stageBroker) —
//     the DB write + SSE card belong to the host.
//
// This is the same set of seams the in-process path uses, applied to the
// agent's delegated calls — so native-acp governs identically.

// The host-side MCP broker for native-acp is agentcore.NewLocalMCPBroker (wired in
// buildACPHostGovernance below): it runs each delegated call against the per-task
// credentialed mcp.Client, applying credentials at THIS call, never shipping them
// into the agent container. It is the SAME implementation the in-process mcpTool
// uses, so both flavors render an identical result through one seam (issue #167) —
// no second MCP-call path to drift.

// stageBroker implements acpruntime.StageBroker by forwarding to the real
// host-side stagers (ApprovalStager / MemoryProposer / NoteProposer) the
// in-process path uses. Any of the three may be nil — a delegated request for an
// unwired surface returns an error the agent surfaces exactly as the in-process
// "not wired" path would.
type stageBroker struct {
	approval ApprovalStager
	memory   MemoryProposer
	note     agentcore.NoteProposer
}

var _ acpruntime.StageBroker = (*stageBroker)(nil)

func (b *stageBroker) StageApproval(toolName, toolCallID, rawInput string) (string, error) {
	if b.approval == nil {
		return "", errStagerNotWired
	}
	return b.approval.Stage(toolName, toolCallID, rawInput)
}

func (b *stageBroker) StageSuggestion(reason string) (string, string, error) {
	if b.approval == nil {
		return "", "", errStagerNotWired
	}
	return b.approval.StageSuggestion(reason)
}

func (b *stageBroker) StageMemory(content string) (string, error) {
	if b.memory == nil {
		return "", errStagerNotWired
	}
	return b.memory.Propose(content)
}

func (b *stageBroker) StageNote(slug, title, body, reason string) (string, error) {
	if b.note == nil {
		return "", errStagerNotWired
	}
	return b.note.Propose(slug, title, body, reason)
}

// errStagerNotWired is returned when a delegated staging request targets a
// surface the host has no stager for. The agent maps it onto the same "not wired"
// agent-facing message the in-process gate produces.
var errStagerNotWired = stagerNotWiredError("staging surface not wired host-side")

type stagerNotWiredError string

func (e stagerNotWiredError) Error() string { return string(e) }

// acpHostGovernance bundles the host-side governance wiring a native-acp run
// hands acpruntime: the public MCP tool descriptors the agent advertises (no
// credentials), the host MCP broker that runs each delegated call against the
// per-task credentialed client, and the staging broker (approval/memory/note).
// It is the SAME seam set for both modes — the interactive driver and the
// scheduled driver build it identically through buildACPHostGovernance, differing
// only in which stagers they wire (interactive wires approval+memory+note;
// scheduled wires note only). Factoring it here keeps the two RunSpec builders
// DRY and guarantees the cred-isolation invariant (descriptors carry no creds;
// the broker holds the credentialed client) holds the same way in both paths.
type acpHostGovernance struct {
	// MCPDescriptors are the public mcp_<server>_<tool> descriptors (no creds).
	MCPDescriptors []acpruntime.MCPToolDescriptor
	// MCPBroker runs delegated MCP calls host-side against the credentialed
	// client. Nil when no descriptors (so the agent advertises no MCP tools).
	MCPBroker acpruntime.MCPBroker
	// StageBroker forwards delegated staging effects to the real stagers. Nil
	// when neither staging surface is wired.
	StageBroker acpruntime.StageBroker
	// StagingWired / NoteProposerWired tell the agent which staging gates to wire
	// so it reports "not wired" identically to an in-process run with no stagers.
	StagingWired      bool
	NoteProposerWired bool
}

// acpStagers carries the host-side stagers a native-acp run delegates to. The
// interactive driver supplies all three; the scheduled driver supplies only the
// note proposer (approval/memory are interactive-only), matching the in-process
// scheduled policy which wires the note proposer but no approval/memory staging.
type acpStagers struct {
	approval ApprovalStager
	memory   MemoryProposer
	note     agentcore.NoteProposer
}

// buildACPHostGovernance assembles the shared host-side governance seam for a
// native-acp run from the per-run MCP client + gates + selection and the stagers
// the caller wires. Both the interactive and the scheduled driver call this so
// the MCP credential brokering + staging delegation are IDENTICAL across modes:
// the descriptors carry only public schema (no creds), the broker runs every call
// host-side against the credentialed client, and staging effects forward to the
// real stagers. The cred-isolation invariant lives here — credentials never enter
// the spec the agent receives.
func buildACPHostGovernance(
	client *mcp.Client,
	allow agentcore.MCPAllowlist,
	optionalServers agentcore.MCPOptionalSet,
	selection agentcore.MCPSelection,
	credentialAllowlist agentcore.CredentialAllowlist,
	stagers acpStagers,
) acpHostGovernance {
	g := acpHostGovernance{}

	g.MCPDescriptors = buildMCPDescriptors(client, allow, optionalServers, selection)
	if len(g.MCPDescriptors) > 0 && client != nil {
		// The SAME in-process broker the native-inprocess loop uses — one seam,
		// one result rendering. DefaultRemediationHints because the host broker
		// has no per-conversation remediation context (parity with the prior
		// dedicated broker, which also used the defaults). Gate-3 (#184) wraps the
		// broker so a denied (server, account) pair is refused HERE, at the host
		// credential boundary, on the native-acp path exactly as in-process — a nil
		// allowlist = inherit global (no-op wrap).
		g.MCPBroker = agentcore.GateMCPBrokerWithAllowlist(
			agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
			credentialAllowlist)
	}

	g.StagingWired = stagers.approval != nil || stagers.memory != nil
	g.NoteProposerWired = stagers.note != nil
	if g.StagingWired || g.NoteProposerWired {
		g.StageBroker = &stageBroker{
			approval: stagers.approval,
			memory:   stagers.memory,
			note:     stagers.note,
		}
	}
	return g
}

// buildMCPDescriptors computes the mcp_<server>_<tool> descriptors the native-acp
// agent should advertise, applying the SAME Gate-1 (Optional opt-in) and Gate-2
// (per-server allowlist) filters agentcore.buildFantasyTools applies in-process,
// so the agent's MCP tool surface matches the in-process path exactly. Returns
// descriptors with NO credentials — only the public server/tool/description/schema.
func buildMCPDescriptors(
	client *mcp.Client,
	allow agentcore.MCPAllowlist,
	optionalServers agentcore.MCPOptionalSet,
	selection agentcore.MCPSelection,
) []acpruntime.MCPToolDescriptor {
	if client == nil {
		return nil
	}
	optIn := selection.OptInSet()
	var out []acpruntime.MCPToolDescriptor
	for _, st := range client.GetAllTools() {
		// Gate 1: Optional servers only when opted in (byte-identical to in-process).
		if optionalServers[st.ServerName] && !optIn[st.ServerName] {
			continue
		}
		// Gate 2: per-server tool allowlist.
		if list, ok := allow[st.ServerName]; ok && len(list) > 0 && !slices.Contains(list, st.Tool.Name) {
			continue
		}
		out = append(out, acpruntime.MCPToolDescriptor{
			Server:      st.ServerName,
			Tool:        st.Tool.Name,
			Description: st.Tool.Description,
			InputSchema: st.Tool.InputSchema,
		})
	}
	return out
}
