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

// availableOptionalConnectorsForUser returns the catalog of connected bundled
// Optional MCP connectors available to user right now:
//   - Bundled optional MCP servers from s.agent.MCPServerCatalog() (excluding
//     the synthetic image_generation toggle) whose live discovery status is
//     available and that are not disabled in the user's ConnectorPrefs.
//   - Fails closed (returns an empty slice) if the user's ConnectorPrefs
//     cannot be loaded from the store so a preference-read error never
//     advertises or enables a connector the user disabled.
//
// Used both to populate TurnInput.AvailableOptionalMCPServers (for the system
// prompt's "Available Optional MCP Connectors" section) and to validate
// enable_mcp_connectors requests.
func (s *Server) availableOptionalConnectorsForUser(ctx context.Context, user string) []agent.OptionalServerInfo {
	if s.agent == nil {
		return []agent.OptionalServerInfo{}
	}
	var prefs map[string]store.ConnectorPref
	if s.store != nil && user != "" {
		list, err := s.store.ListConnectorPrefs(ctx, user)
		if err != nil {
			log.Printf("availableOptionalConnectorsForUser: load connector prefs failed for %s: %s", logSafe(user), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF.
			return []agent.OptionalServerInfo{}
		}
		prefs = list
	}
	catalog := s.agent.MCPServerCatalog()
	out := make([]agent.OptionalServerInfo, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, info := range catalog {
		key := strings.ToLower(strings.TrimSpace(info.Name))
		if key == "" || key == agent.OptionalNativeImageGenName || info.Remote || seen[key] {
			continue
		}
		if optionalConnectorStatus(info) != connectorStatusAvailable {
			continue
		}
		avail, _, _ := bundledPrefFor(prefs, info)
		if !avail {
			continue
		}
		seen[key] = true
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func putWithPromptSafeAlias[V any](m map[string]V, safeOwners map[string]map[string]bool, rawName string, val V) {
	k := strings.ToLower(strings.TrimSpace(rawName))
	if k == "" {
		return
	}
	if _, exists := m[k]; !exists {
		m[k] = val
	}
	safe := agentcore.PromptSafeName(k)
	if safe == "" {
		return
	}
	if safeOwners != nil {
		if safeOwners[safe] == nil {
			safeOwners[safe] = map[string]bool{}
		}
		safeOwners[safe][k] = true
	}
	if safe != k {
		if _, exists := m[safe]; !exists {
			m[safe] = val
		}
	}
}

func promptSafeSlice(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if safe := agentcore.PromptSafeName(strings.ToLower(strings.TrimSpace(n))); safe != "" {
			out = append(out, safe)
		}
	}
	return out
}

type connectorValidationCatalog struct {
	prefs           map[string]store.ConnectorPref
	prefsErr        error
	alwaysOn        map[string]agent.AlwaysOnServerInfo
	bundledOptional map[string]agent.OptionalServerInfo
	needsSetup      map[string]bool
	available       map[string]agent.OptionalServerInfo
	allRemote       map[string]map[string]bool
	safeOwners      map[string]map[string]bool
}

func (s *Server) buildConnectorValidationCatalog(ctx context.Context, user string) connectorValidationCatalog {
	cat := connectorValidationCatalog{
		alwaysOn:        map[string]agent.AlwaysOnServerInfo{},
		bundledOptional: map[string]agent.OptionalServerInfo{},
		needsSetup:      map[string]bool{},
		available:       map[string]agent.OptionalServerInfo{},
		safeOwners:      map[string]map[string]bool{},
	}
	if s.store != nil && user != "" {
		list, perr := s.store.ListConnectorPrefs(ctx, user)
		if perr != nil {
			cat.prefsErr = perr
		} else {
			cat.prefs = list
		}
	}
	if provider, ok := s.agent.(alwaysOnMCPServerCatalogProvider); ok && provider != nil {
		for _, info := range provider.AlwaysOnMCPServerCatalog() {
			putWithPromptSafeAlias(cat.alwaysOn, nil, info.Name, info)
		}
	}
	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			putWithPromptSafeAlias(cat.bundledOptional, nil, info.Name, info)
		}
	}
	for _, m := range s.connectorMeta() {
		if !m.Configured {
			putWithPromptSafeAlias(cat.needsSetup, nil, m.Name, true)
		}
	}
	for _, info := range s.availableOptionalConnectorsForUser(ctx, user) {
		putWithPromptSafeAlias(cat.available, cat.safeOwners, info.Name, info)
	}
	rawRemote := s.remoteSeatCatalog(ctx, user)
	cat.allRemote = make(map[string]map[string]bool, len(rawRemote))
	for k, v := range rawRemote {
		putWithPromptSafeAlias(cat.allRemote, nil, k, v)
	}
	return cat
}

func (c connectorValidationCatalog) isAmbiguous(lookup string) bool {
	if len(c.safeOwners[lookup]) > 1 {
		return true
	}
	if safe := agentcore.PromptSafeName(lookup); safe != "" && len(c.safeOwners[safe]) > 1 {
		return true
	}
	return false
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

	cat := s.buildConnectorValidationCatalog(ctx, user)
	if cat.prefsErr != nil {
		return nil, nil, fmt.Errorf("load connector preferences: %w", cat.prefsErr)
	}
	hidden := agentcore.InteractiveHiddenTools()

	displayNames = make(map[string]string, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, raw := range requested {
		lookup := strings.ToLower(strings.TrimSpace(raw))
		if lookup == "" {
			continue
		}
		safeLookup := agentcore.PromptSafeName(lookup)
		if lookup == agent.OptionalNativeImageGenName || safeLookup == agent.OptionalNativeImageGenName {
			return nil, nil, fmt.Errorf("connector %q is the native image generation toggle, not an MCP connector; toggle it directly in the Tools picker", safeLookup)
		}
		if cat.isAmbiguous(lookup) {
			return nil, nil, fmt.Errorf("connector %q has an ambiguous identifier that collides with another connector; toggle the intended connector directly in the Tools picker", safeLookup)
		}

		info, ok := cat.available[lookup]
		if !ok {
			if ao, isAO := cat.alwaysOn[lookup]; isAO {
				if !ao.Available {
					return nil, nil, fmt.Errorf("connector %q is an always-on connector that is currently unavailable on this server", safeLookup)
				}
				continue
			}
			if bInfo, isBundled := cat.bundledOptional[lookup]; isBundled {
				if optionalConnectorStatus(bInfo) != connectorStatusAvailable {
					return nil, nil, fmt.Errorf("connector %q is currently unavailable (exposed 0 tools at startup) and cannot be enabled", safeLookup)
				}
				if avail, _, _ := bundledPrefFor(cat.prefs, bInfo); !avail {
					return nil, nil, fmt.Errorf("connector %q is disabled in the user's Settings → Connections preferences; ask the user to enable it there first", safeLookup)
				}
			}
			if cat.needsSetup[lookup] {
				return nil, nil, fmt.Errorf("connector %q is not configured on this server (needs administrator credential setup) and cannot be enabled", safeLookup)
			}
			if _, hasRemote := cat.allRemote[lookup]; hasRemote {
				return nil, nil, fmt.Errorf("remote connector %q must be enabled directly in the Tools picker", safeLookup)
			}
			return nil, nil, fmt.Errorf("unknown or unavailable optional MCP connector %q — only connectors listed under 'Available Optional MCP Connectors' can be enabled", safeLookup)
		}

		canonName := strings.ToLower(strings.TrimSpace(info.Name))
		if canonName == "" || seen[canonName] {
			continue
		}
		seen[canonName] = true

		if !connectorPermittedInChat(info, hidden, personaPolicy) {
			return nil, nil, fmt.Errorf("connector %q is not permitted for the current persona or interactive chat policy", agentcore.PromptSafeName(canonName))
		}

		optionalToEnable = append(optionalToEnable, canonName)
		if strings.TrimSpace(info.DisplayName) != "" {
			displayNames[canonName] = strings.TrimSpace(info.DisplayName)
		} else {
			displayNames[canonName] = canonName
		}
	}
	return optionalToEnable, displayNames, nil
}

func connectorPermittedInChat(info agent.OptionalServerInfo, hidden []string, policy *agentcore.PersonaToolPermissions) bool {
	name := strings.ToLower(strings.TrimSpace(info.Name))
	if name == "" || name == agent.OptionalNativeImageGenName || info.Remote {
		return false
	}
	for _, t := range info.Tools {
		full := agentcore.MCPToolName(info.Name, t)
		if !agentcore.InteractiveHiddenIn(hidden, full) && agentcore.PersonaPermitsTool(policy, full) {
			return true
		}
	}
	return false
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
		unlock := s.connectorLocks.lock(conv.ID)
		defer unlock()

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
		fresh, gerr := s.store.Get(ctx, user, conv.ID)
		if gerr != nil || fresh == nil {
			if gerr == nil {
				gerr = fmt.Errorf("conversation not found")
			}
			return nil, fmt.Errorf("load conversation: %w", gerr)
		}
		currentEnabled := append([]string(nil), fresh.OptionalMCPServersEnabled...)
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
			safeReq := promptSafeSlice(params.Connectors)
			return &tools.EnableMCPConnectorsResult{
				Status:          "already_enabled",
				Connectors:      safeReq,
				EnabledOptional: promptSafeSlice(currentEnabled),
				Message:         fmt.Sprintf("Connector(s) %s are already enabled in this conversation — their tools are already active.", strings.Join(safeReq, ", ")),
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

		autoApprove := turnStager != nil && (turnStager.isAutoApprove() || turnStager.autoApproveInTest)
		if autoApprove {
			return s.autoApproveEnableMCPConnectors(ctx, conv, user, toolCallID, rawInput, currentEnabled, newlyAdded, merged, buf, turnStager)
		}
		return s.stageEnableMCPConnectorsApproval(ctx, conv, user, toolCallID, rawInput, newlyAdded, buf, turnStager)
	}
}

func (s *Server) autoApproveEnableMCPConnectors(
	ctx context.Context,
	conv *store.Conversation,
	user, toolCallID, rawInput string,
	previousEnabled, newlyAdded, merged []string,
	buf *turnBuffer,
	turnStager *approvalStager,
) (*tools.EnableMCPConnectorsResult, error) {
	if n, err := s.store.SupersedePendingApprovals(ctx, conv.ID, tools.EnableMCPConnectorsToolName); err != nil {
		return nil, fmt.Errorf("supersede pending connector approvals: %w", err)
	} else if n > 0 && buf != nil {
		buf.Emit("tool.approval_superseded", map[string]any{
			"tool":  tools.EnableMCPConnectorsToolName,
			"count": n,
		})
	}

	safeAdded := promptSafeSlice(newlyAdded)
	recordText := fmt.Sprintf("%s: auto-approved enabling optional MCP connector(s) %s for this conversation.",
		notifyRecordResultPrefix, strings.Join(safeAdded, ", "))
	approval, aerr := s.store.CreateApproval(ctx, conv.ID, user,
		tools.EnableMCPConnectorsToolName, toolCallID, rawInput,
		turnStager.expiryUnixFor(tools.EnableMCPConnectorsToolName), store.ApprovalSeat{})
	if aerr != nil || approval == nil {
		if aerr == nil {
			aerr = fmt.Errorf("store returned nil approval")
		}
		return nil, fmt.Errorf("create auto-approved connector record: %w", aerr)
	}

	compensateCtx := context.WithoutCancel(ctx)
	if err := s.store.SetOptionalMCPServers(ctx, user, conv.ID, merged); err != nil {
		_ = s.store.ResolveApproval(compensateCtx, user, approval.ID, "failed", err.Error())
		return nil, fmt.Errorf("enable connectors: %w", err)
	}
	armed, armErr := s.store.ArmApprovalResume(ctx, user, approval.ID)
	if armErr != nil || !armed {
		if armErr == nil {
			armErr = fmt.Errorf("approval resume state could not be armed")
		}
		_ = s.store.SetOptionalMCPServers(compensateCtx, user, conv.ID, previousEnabled)
		_ = s.store.ResolveApproval(compensateCtx, user, approval.ID, "failed", armErr.Error())
		return nil, fmt.Errorf("arm auto-approved connector resume: %w", armErr)
	}
	if rerr := s.store.ResolveApproval(ctx, user, approval.ID, "approved", recordText); rerr != nil {
		_ = s.store.SetOptionalMCPServers(compensateCtx, user, conv.ID, previousEnabled)
		_ = s.store.ResolveApproval(compensateCtx, user, approval.ID, "failed", rerr.Error())
		return nil, fmt.Errorf("resolve auto-approved connector record: %w", rerr)
	}
	conv.OptionalMCPServersEnabled = merged

	if buf != nil {
		buf.Emit("mcp.servers_updated", map[string]any{
			"enabled_optional": merged,
			"added":            newlyAdded,
		})
		buf.Emit("tool.action_recorded", map[string]any{
			"approval_id":      approval.ID,
			"tool":             tools.EnableMCPConnectorsToolName,
			"summary":          summarizeEnableMCPConnectorsInput(tools.EnableMCPConnectorsToolName, rawInput),
			"undo_hint":        "You can toggle connectors off anytime in the Tools picker.",
			"result_text":      recordText,
			"enabled_optional": merged,
		})
	}

	s.decideApprovalResume(conv.ID, 0)

	return &tools.EnableMCPConnectorsResult{
		Status:          "enabled",
		Connectors:      safeAdded,
		EnabledOptional: promptSafeSlice(merged),
		AutoApproved:    true,
		ApprovalID:      approval.ID,
		Message:         fmt.Sprintf("Auto-approved and enabled optional MCP connector(s) [%s] for this conversation. Stopping this step now so your turn automatically continues with their tools mounted.", strings.Join(safeAdded, ", ")),
		StopTurn:        true,
	}, nil
}

func (s *Server) stageEnableMCPConnectorsApproval(
	ctx context.Context,
	conv *store.Conversation,
	user, toolCallID, rawInput string,
	newlyAdded []string,
	buf *turnBuffer,
	turnStager *approvalStager,
) (*tools.EnableMCPConnectorsResult, error) {
	if n, err := s.store.SupersedePendingApprovals(ctx, conv.ID, tools.EnableMCPConnectorsToolName); err != nil {
		return nil, fmt.Errorf("supersede pending connector approvals: %w", err)
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
	ok, aerr := s.store.ArmApprovalResume(ctx, user, approval.ID)
	if aerr != nil || !ok {
		if aerr == nil {
			aerr = fmt.Errorf("approval resume state could not be armed")
		}
		_ = s.store.ResolveApproval(context.WithoutCancel(ctx), user, approval.ID, "failed", aerr.Error())
		return nil, fmt.Errorf("arm connector approval resume: %w", aerr)
	}
	approval.ResumeState = store.ApprovalResumeArmed

	if buf != nil {
		buf.Emit("tool.approval_required", approvalRequiredEvent(approval, rawInput, conv.ID))
	}
	if turnStager != nil {
		turnStager.firePushNotification(tools.EnableMCPConnectorsToolName)
	}

	safeAdded := promptSafeSlice(newlyAdded)
	return &tools.EnableMCPConnectorsResult{
		Status:     "approval_required",
		Connectors: safeAdded,
		ApprovalID: approval.ID,
		Message: fmt.Sprintf(
			"APPROVAL_REQUIRED: Enabling optional MCP connector(s) [%s] has been staged for explicit user approval (approval_id=%s). "+
				"Do NOT call enable_mcp_connectors again and do NOT try to work around the missing connector with bash or other tools. "+
				"Briefly explain to the user which connector(s) you suggested enabling and why, and wait for them to click Approve — "+
				"your turn will resume automatically with those connectors mounted as soon as they approve.",
			strings.Join(safeAdded, ", "), approval.ID,
		),
	}, nil
}

// runStagedEnableMCPConnectors executes a user-approved enable_mcp_connectors
// card: re-validates the requested connectors, merges them into the
// conversation's OptionalMCPServersEnabled list, persists the update, and
// returns the model-visible tool_result text.
func (s *Server) runStagedEnableMCPConnectors(ctx context.Context, user, convID string, approval *store.Approval) (string, error) {
	unlock := s.connectorLocks.lock(convID)
	defer unlock()

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
		return fmt.Sprintf("Enabled optional MCP connector(s) for this conversation: %s. Their tools are now active and mounted in your roster — proceed with the user's request.", strings.Join(promptSafeSlice(args.Connectors), ", ")), nil
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
		strings.Join(promptSafeSlice(toEnable), ", "),
	), nil
}
