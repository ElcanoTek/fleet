package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// availableOptionalConnectorsForUser returns the catalog of connected Optional
// MCP connectors available to user right now:
//  1. Bundled optional MCP servers from s.agent.MCPServerCatalog() whose live
//     discovery status is available and that are not disabled in the user's
//     ConnectorPrefs.
//  2. The user's connected remote (hosted) MCP servers from s.remoteMCP.
//
// Used both to populate TurnInput.AvailableOptionalMCPServers (for the system
// prompt's "Available Optional MCP Connectors" section) and to validate
// enable_mcp_connectors requests.
func (s *Server) availableOptionalConnectorsForUser(ctx context.Context, user string) []agent.OptionalServerInfo {
	if s.agent == nil {
		return nil
	}
	var prefs map[string]store.ConnectorPref
	if s.store != nil && user != "" {
		if list, err := s.store.ListConnectorPrefs(ctx, user); err == nil {
			prefs = list
		}
	}
	catalog := s.agent.MCPServerCatalog()
	out := make([]agent.OptionalServerInfo, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, info := range catalog {
		if optionalConnectorStatus(info) != connectorStatusAvailable {
			continue
		}
		avail, _, _ := bundledPrefFor(prefs, info)
		if !avail {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(info.Name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, info)
	}
	if s.remoteMCP != nil && s.remoteMCP.Enabled() && user != "" {
		if conns, err := s.remoteMCP.ConnectedServersForUser(ctx, user); err == nil {
			for _, g := range agent.GroupRemoteMCPSeats(conns) {
				key := strings.ToLower(strings.TrimSpace(g.Name))
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, agent.OptionalServerInfo{
					Name:        key,
					DisplayName: g.Name,
					Description: "Remote MCP server you connected (" + g.URL + ").",
					Accounts:    g.Accounts,
					Remote:      true,
				})
			}
		} else {
			log.Printf("availableOptionalConnectorsForUser: remote server lookup failed for %s: %v", logSafe(user), err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// validateEnableMCPConnectors checks that every requested connector name is a
// valid, connected, prefs-enabled Optional connector (or already-on Always-On
// connector) whose tools are permitted by interactive_hidden_tools and the
// active persona policy. Returns the canonical lowercase optional connector
// names to enable (excluding any that are already always-on) and their display
// metadata.
func (s *Server) validateEnableMCPConnectors(
	ctx context.Context,
	user string,
	requested []string,
	personaPolicy *agentcore.PersonaToolPermissions,
) (optionalToEnable []string, displayNames map[string]string, err error) {
	if len(requested) == 0 {
		return nil, nil, fmt.Errorf("no connectors specified")
	}

	var prefs map[string]store.ConnectorPref
	if s.store != nil && user != "" {
		if list, perr := s.store.ListConnectorPrefs(ctx, user); perr == nil {
			prefs = list
		}
	}

	// Index always-on connectors so we can distinguish "already always on" from
	// "always-on but down" or "unknown".
	alwaysOn := map[string]agent.AlwaysOnServerInfo{}
	if provider, ok := s.agent.(alwaysOnMCPServerCatalogProvider); ok && provider != nil {
		for _, info := range provider.AlwaysOnMCPServerCatalog() {
			alwaysOn[strings.ToLower(strings.TrimSpace(info.Name))] = info
		}
	}

	// Index bundled optional connectors (before pref filtering) so we can give a
	// specific error if one is disabled in ConnectorPrefs or unavailable.
	bundledOptional := map[string]agent.OptionalServerInfo{}
	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			bundledOptional[strings.ToLower(strings.TrimSpace(info.Name))] = info
		}
	}

	// Index unconfigured manifest connectors (needs_setup).
	needsSetup := map[string]bool{}
	for _, m := range s.connectorMeta() {
		if !m.Configured {
			needsSetup[strings.ToLower(strings.TrimSpace(m.Name))] = true
		}
	}

	// Index connected optional connectors for this user (bundled + remote).
	available := map[string]agent.OptionalServerInfo{}
	for _, info := range s.availableOptionalConnectorsForUser(ctx, user) {
		available[strings.ToLower(strings.TrimSpace(info.Name))] = info
	}

	// Index all remote seats (including disconnected/needs_reauth) for clear
	// diagnostic messages.
	allRemote := s.remoteSeatCatalog(ctx, user)
	hidden := agentcore.InteractiveHiddenTools()

	displayNames = make(map[string]string, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, raw := range requested {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		if ao, ok := alwaysOn[name]; ok {
			if !ao.Available {
				return nil, nil, fmt.Errorf("connector %q is an always-on connector that is currently unavailable on this server", name)
			}
			// Already always-on: no opt-in needed, don't fail if requested alongside optional ones.
			continue
		}

		if bInfo, isBundled := bundledOptional[name]; isBundled {
			if optionalConnectorStatus(bInfo) != connectorStatusAvailable {
				return nil, nil, fmt.Errorf("connector %q is currently unavailable (exposed 0 tools at startup) and cannot be enabled", name)
			}
			if avail, _, _ := bundledPrefFor(prefs, bInfo); !avail {
				return nil, nil, fmt.Errorf("connector %q is disabled in the user's Settings → Connections preferences; ask the user to enable it there first", name)
			}
		}

		info, ok := available[name]
		if !ok {
			if needsSetup[name] {
				return nil, nil, fmt.Errorf("connector %q is not configured on this server (needs administrator credential setup) and cannot be enabled", name)
			}
			if _, hasRemote := allRemote[name]; hasRemote {
				return nil, nil, fmt.Errorf("remote connector %q is not currently connected (it may need re-authentication or be disabled under Settings → Connections)", name)
			}
			return nil, nil, fmt.Errorf("unknown or unavailable optional MCP connector %q — only connectors listed under 'Available Optional MCP Connectors' can be enabled", name)
		}

		// Verify at least one tool on the connector survives interactive_hidden_tools
		// and the turn's persona policy.
		if !connectorPermittedInChat(info, hidden, personaPolicy) {
			return nil, nil, fmt.Errorf("connector %q is not permitted for the current persona or interactive chat policy", name)
		}

		optionalToEnable = append(optionalToEnable, name)
		if strings.TrimSpace(info.DisplayName) != "" {
			displayNames[name] = strings.TrimSpace(info.DisplayName)
		} else {
			displayNames[name] = name
		}
	}
	return optionalToEnable, displayNames, nil
}

func connectorPermittedInChat(info agent.OptionalServerInfo, hidden []string, policy *agentcore.PersonaToolPermissions) bool {
	name := strings.ToLower(strings.TrimSpace(info.Name))
	if name == agent.OptionalNativeImageGenName {
		return !agentcore.InteractiveHiddenIn(hidden, "generate_image") && agentcore.PersonaPermitsTool(policy, "generate_image")
	}
	safeName := agentcore.PromptSafeName(name)
	if len(info.Tools) > 0 {
		for _, t := range info.Tools {
			full := t
			if !strings.HasPrefix(full, "mcp_") {
				full = "mcp_" + safeName + "_" + t
			}
			if !agentcore.InteractiveHiddenIn(hidden, full) && agentcore.PersonaPermitsTool(policy, full) {
				return true
			}
		}
		return false
	}
	probe := "mcp_" + safeName + "_probe"
	return !agentcore.InteractiveHiddenIn(hidden, probe) && agentcore.PersonaPermitsTool(policy, probe)
}

type stagedEnableMCPConnectorsArgs struct {
	Connectors   []string          `json:"connectors"`
	DisplayNames map[string]string `json:"display_names,omitempty"`
	Reason       string            `json:"reason,omitempty"`
}

func summarizeEnableMCPConnectorsInput(toolName, rawInput string) map[string]any {
	var args stagedEnableMCPConnectorsArgs
	_ = json.Unmarshal([]byte(rawInput), &args)
	labels := make([]string, 0, len(args.Connectors))
	for _, c := range args.Connectors {
		if dn := strings.TrimSpace(args.DisplayNames[c]); dn != "" && !strings.EqualFold(dn, c) {
			labels = append(labels, fmt.Sprintf("%s (%s)", dn, c))
		} else {
			labels = append(labels, c)
		}
	}
	return map[string]any{
		"tool":          toolName,
		"connectors":    args.Connectors,
		"display_names": args.DisplayNames,
		"labels":        labels,
		"reason":        args.Reason,
	}
}

func (s *Server) newEnableMCPConnectorsHandler(
	conv *store.Conversation,
	user string,
	buf *turnBuffer,
	turnStager *approvalStager,
) tools.EnableMCPConnectorsHandler {
	return func(ctx context.Context, toolCallID string, params tools.EnableMCPConnectorsParams) (*tools.EnableMCPConnectorsResult, error) {
		if s.store == nil {
			return nil, fmt.Errorf("enable_mcp_connectors requires a conversation store")
		}
		var policy *agentcore.PersonaToolPermissions
		if turnStager != nil {
			turnStager.mcpMu.RLock()
			policy = turnStager.personaPolicy
			turnStager.mcpMu.RUnlock()
		}

		toEnable, displayNames, err := s.validateEnableMCPConnectors(ctx, user, params.Connectors, policy)
		if err != nil {
			return nil, err
		}

		// Load fresh conversation state so we merge against the latest enabled list.
		currentEnabled := append([]string(nil), conv.OptionalMCPServersEnabled...)
		if s.store != nil {
			if fresh, gerr := s.store.Get(ctx, user, conv.ID); gerr == nil && fresh != nil {
				currentEnabled = fresh.OptionalMCPServersEnabled
			}
		}
		enabledSet := make(map[string]bool, len(currentEnabled))
		for _, n := range currentEnabled {
			enabledSet[strings.ToLower(strings.TrimSpace(n))] = true
		}

		var newlyAdded []string
		for _, n := range toEnable {
			if !enabledSet[n] {
				newlyAdded = append(newlyAdded, n)
			}
		}
		if len(newlyAdded) == 0 {
			return &tools.EnableMCPConnectorsResult{
				Status:          "already_enabled",
				Connectors:      params.Connectors,
				EnabledOptional: currentEnabled,
				Message:         fmt.Sprintf("Connector(s) %s are already enabled in this conversation — their tools are already active.", strings.Join(params.Connectors, ", ")),
			}, nil
		}

		merged := append([]string(nil), currentEnabled...)
		for _, n := range newlyAdded {
			if !slices.Contains(merged, n) {
				merged = append(merged, n)
			}
		}
		sort.Strings(merged)

		rawBytes, _ := json.Marshal(stagedEnableMCPConnectorsArgs{
			Connectors:   newlyAdded,
			DisplayNames: displayNames,
			Reason:       params.Reason,
		})
		rawInput := string(rawBytes)

		autoApprove := turnStager != nil && (turnStager.isAutoApprove() || turnStager.isPatrolOrPatrolResume() || turnStager.autoApproveInTest)
		if autoApprove {
			if err := s.store.SetOptionalMCPServers(ctx, user, conv.ID, merged); err != nil {
				return nil, fmt.Errorf("enable connectors: %w", err)
			}
			conv.OptionalMCPServersEnabled = merged

			if _, err := s.store.SupersedePendingApprovals(ctx, conv.ID, tools.EnableMCPConnectorsToolName); err != nil {
				log.Printf("supersede enable_mcp_connectors (conv=%s): %v", logSafe(conv.ID), err)
			}

			var approvalID string
			recordText := fmt.Sprintf("%s: auto-approved enabling optional MCP connector(s) %s for this conversation.",
				notifyRecordResultPrefix, strings.Join(newlyAdded, ", "))
			if approval, aerr := s.store.CreateApproval(ctx, conv.ID, user,
				tools.EnableMCPConnectorsToolName, toolCallID, rawInput,
				turnStager.expiryUnixFor(tools.EnableMCPConnectorsToolName), store.ApprovalSeat{}); aerr == nil && approval != nil {
				approvalID = approval.ID
				_, _ = s.store.ArmApprovalResume(ctx, user, approval.ID)
				_ = s.store.ResolveApproval(ctx, user, approval.ID, "approved", recordText)
			}

			if buf != nil {
				buf.Emit("mcp.servers_updated", map[string]any{
					"enabled_optional": merged,
					"added":            newlyAdded,
				})
				if approvalID != "" {
					buf.Emit("tool.action_recorded", map[string]any{
						"approval_id":      approvalID,
						"tool":             tools.EnableMCPConnectorsToolName,
						"summary":          summarizeEnableMCPConnectorsInput(tools.EnableMCPConnectorsToolName, rawInput),
						"undo_hint":        "You can toggle connectors off anytime in the Tools picker.",
						"result_text":      recordText,
						"enabled_optional": merged,
					})
				}
			}

			// Claim the armed approval and enqueue the 'resume' continuation turn
			// immediately so that when StopTurn ends this step, the completion tail's
			// maybeDrainQueue launches the continuation turn with the newly enabled
			// MCP connectors mounted.
			s.decideApprovalResume(conv.ID, 0)

			return &tools.EnableMCPConnectorsResult{
				Status:          "enabled",
				Connectors:      newlyAdded,
				EnabledOptional: merged,
				AutoApproved:    true,
				ApprovalID:      approvalID,
				Message:         fmt.Sprintf("Auto-approved and enabled optional MCP connector(s) [%s] for this conversation. Stopping this step now so your turn automatically continues with their tools mounted.", strings.Join(newlyAdded, ", ")),
				StopTurn:        true,
			}, nil
		}

		// User-approval mode: supersede any older pending card for this tool,
		// create and arm a pending approval card, emit tool.approval_required,
		// and instruct the model to explain the suggestion and wait.
		if n, err := s.store.SupersedePendingApprovals(ctx, conv.ID, tools.EnableMCPConnectorsToolName); err != nil {
			log.Printf("supersede enable_mcp_connectors (conv=%s): %v", logSafe(conv.ID), err)
		} else if n > 0 && buf != nil {
			buf.Emit("tool.approval_superseded", map[string]any{
				"tool":  tools.EnableMCPConnectorsToolName,
				"count": n,
			})
		}

		expiry := int64(0)
		if turnStager != nil {
			expiry = turnStager.expiryUnixFor(tools.EnableMCPConnectorsToolName)
		}
		approval, err := s.store.CreateApproval(ctx, conv.ID, user,
			tools.EnableMCPConnectorsToolName, toolCallID, rawInput, expiry, store.ApprovalSeat{})
		if err != nil {
			return nil, fmt.Errorf("stage connector approval: %w", err)
		}
		if ok, aerr := s.store.ArmApprovalResume(ctx, user, approval.ID); aerr == nil && ok {
			approval.ResumeState = store.ApprovalResumeArmed
		}
		if buf != nil {
			buf.Emit("tool.approval_required", approvalRequiredEvent(approval, rawInput, conv.ID))
		}
		if turnStager != nil {
			turnStager.firePushNotification(tools.EnableMCPConnectorsToolName)
		}

		return &tools.EnableMCPConnectorsResult{
			Status:     "approval_required",
			Connectors: newlyAdded,
			ApprovalID: approval.ID,
			Message: fmt.Sprintf(
				"APPROVAL_REQUIRED: Enabling optional MCP connector(s) [%s] has been staged for explicit user approval (approval_id=%s). "+
					"Do NOT call enable_mcp_connectors again and do NOT try to work around the missing connector with bash or other tools. "+
					"Briefly explain to the user which connector(s) you suggested enabling and why, and wait for them to click Approve — "+
					"your turn will resume automatically with those connectors mounted as soon as they approve.",
				strings.Join(newlyAdded, ", "), approval.ID,
			),
		}, nil
	}
}

// runStagedEnableMCPConnectors executes a user-approved enable_mcp_connectors
// card: re-validates the requested connectors, merges them into the
// conversation's OptionalMCPServersEnabled list, persists the update, and
// returns the model-visible tool_result text.
func (s *Server) runStagedEnableMCPConnectors(ctx context.Context, user, convID string, approval *store.Approval) (string, error) {
	var args stagedEnableMCPConnectorsArgs
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid enable_mcp_connectors payload: %w", err)
	}
	if len(args.Connectors) == 0 {
		return "", fmt.Errorf("no connectors specified in approval payload")
	}

	if s.cfg != nil && s.cfg.MockMode {
		if conv, err := s.store.Get(ctx, user, convID); err == nil && conv != nil {
			merged := append([]string(nil), conv.OptionalMCPServersEnabled...)
			for _, c := range args.Connectors {
				c = strings.ToLower(strings.TrimSpace(c))
				if c != "" && !slices.Contains(merged, c) {
					merged = append(merged, c)
				}
			}
			sort.Strings(merged)
			_ = s.store.SetOptionalMCPServers(ctx, user, convID, merged)
		}
		return fmt.Sprintf("Enabled optional MCP connector(s) for this conversation: %s. Their tools are now active and mounted in your roster — proceed with the user's request.", strings.Join(args.Connectors, ", ")), nil
	}

	conv, err := s.store.Get(ctx, user, convID)
	if err != nil || conv == nil {
		return "", fmt.Errorf("load conversation: %w", err)
	}

	toEnable, _, err := s.validateEnableMCPConnectors(ctx, user, args.Connectors, nil)
	if err != nil {
		return "", err
	}

	merged := append([]string(nil), conv.OptionalMCPServersEnabled...)
	for _, c := range toEnable {
		if !slices.Contains(merged, c) {
			merged = append(merged, c)
		}
	}
	sort.Strings(merged)

	if err := s.store.SetOptionalMCPServers(ctx, user, convID, merged); err != nil {
		return "", fmt.Errorf("persist enabled connectors: %w", err)
	}
	return fmt.Sprintf(
		"Enabled optional MCP connector(s) for this conversation: %s. Their tools are now active and mounted in your roster — proceed with the user's request.",
		strings.Join(toEnable, ", "),
	), nil
}
