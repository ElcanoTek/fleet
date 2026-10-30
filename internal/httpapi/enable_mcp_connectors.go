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
	"github.com/ElcanoTek/fleet/internal/metrics"
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
func (s *Server) availableOptionalConnectorsForUser(ctx context.Context, user string, convAccounts ...map[string]string) []agent.OptionalServerInfo {
	if s.agent == nil {
		return []agent.OptionalServerInfo{}
	}
	var overrides map[string]string
	if len(convAccounts) > 0 {
		overrides = convAccounts[0]
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
		avail, _, seat := bundledPrefFor(prefs, info)
		if !avail {
			continue
		}
		if o := strings.TrimSpace(overrides[info.Name]); o != "" && slices.Contains(info.Accounts, o) {
			seat = o
		} else if o := strings.TrimSpace(overrides[key]); o != "" && slices.Contains(info.Accounts, o) {
			seat = o
		}
		info.SelectedAccount = seat
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
	availableList   []agent.OptionalServerInfo
	allRemote       map[string]map[string]bool
	safeOwners      map[string]map[string]bool
}

func (s *Server) buildConnectorValidationCatalog(ctx context.Context, user string, convAccounts ...map[string]string) connectorValidationCatalog {
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
	cat.availableList = s.availableOptionalConnectorsForUser(ctx, user, convAccounts...)
	for _, info := range cat.availableList {
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

// displayedPromptSet returns the canonical lowercase connector names that
// appear in the system prompt's "Available Optional MCP Connectors" section
// for the current turn (up to agent.MaxAvailableOptionalInPrompt entries,
// sorted by prompt-safe name after filtering out already-enabled, ambiguous,
// and policy-hidden connectors).
func (c connectorValidationCatalog) displayedPromptSet(
	currentEnabled []string,
	hidden []string,
	personaPolicy *agentcore.PersonaToolPermissions,
) map[string]bool {
	enabledSet := make(map[string]bool, len(currentEnabled)*2)
	for _, raw := range currentEnabled {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k != "" {
			enabledSet[k] = true
			if safe := agentcore.PromptSafeName(k); safe != "" {
				enabledSet[safe] = true
			}
		}
	}
	type pair struct {
		canon string
		safe  string
	}
	eligible := make([]pair, 0, len(c.availableList))
	seenSafe := make(map[string]bool, len(c.availableList))
	for _, info := range c.availableList {
		canon := strings.ToLower(strings.TrimSpace(info.Name))
		safe := agentcore.PromptSafeName(canon)
		if canon == "" || safe == "" || enabledSet[canon] || enabledSet[safe] || seenSafe[safe] || c.isAmbiguous(canon) {
			continue
		}
		if !connectorPermittedInChat(info, hidden, personaPolicy) {
			continue
		}
		seenSafe[safe] = true
		eligible = append(eligible, pair{canon: canon, safe: safe})
	}
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].safe < eligible[j].safe
	})
	if len(eligible) > agent.MaxAvailableOptionalInPrompt {
		eligible = eligible[:agent.MaxAvailableOptionalInPrompt]
	}
	out := make(map[string]bool, len(eligible))
	for _, p := range eligible {
		out[p.canon] = true
	}
	return out
}

func (s *Server) displayedPromptSetFromSnapshot(
	availableList []agent.OptionalServerInfo,
	turnStartEnabled []string,
	hidden []string,
	personaPolicy *agentcore.PersonaToolPermissions,
) map[string]bool {
	safeOwners := make(map[string]map[string]bool, len(availableList)+len(turnStartEnabled))
	addOwner := func(raw string) {
		k := strings.ToLower(strings.TrimSpace(raw))
		safe := agentcore.PromptSafeName(k)
		if k == "" || safe == "" {
			return
		}
		if safeOwners[safe] == nil {
			safeOwners[safe] = map[string]bool{}
		}
		safeOwners[safe][k] = true
	}
	if provider, ok := s.agent.(alwaysOnMCPServerCatalogProvider); ok && provider != nil {
		for _, info := range provider.AlwaysOnMCPServerCatalog() {
			addOwner(info.Name)
		}
	}
	for _, raw := range turnStartEnabled {
		addOwner(raw)
	}
	for _, info := range availableList {
		if strings.ToLower(strings.TrimSpace(info.Name)) == agent.OptionalNativeImageGenName || info.Remote {
			continue
		}
		addOwner(info.Name)
	}
	snapCat := connectorValidationCatalog{
		availableList: availableList,
		safeOwners:    safeOwners,
	}
	return snapCat.displayedPromptSet(turnStartEnabled, hidden, personaPolicy)
}

func advertisedSetFromNames(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, raw := range names {
		if k := strings.ToLower(strings.TrimSpace(raw)); k != "" {
			out[k] = true
		}
	}
	return out
}

// validateEnableMCPConnectors checks that every requested connector name is a
// valid, connected, prefs-enabled Optional connector (or already-on Always-On
// connector) whose tools are permitted by interactive_hidden_tools and the
// active persona policy and that falls within the 25-connector system-prompt
// cap (using turnAdvertised when provided so a mid-turn ReloadMCPServers does
// not shift the advertised 25-entry window). Returns the canonical optional
// connector names to enable (preserving each connector's catalog Name case
// while excluding any that are already always-on) and their display metadata.
func (s *Server) validateEnableMCPConnectors(
	ctx context.Context,
	user string,
	requested []string,
	currentEnabled []string,
	personaPolicy *agentcore.PersonaToolPermissions,
	convAccounts map[string]string,
	turnAdvertised ...map[string]bool,
) (optionalToEnable []string, displayNames map[string]string, err error) {
	if len(requested) == 0 {
		return nil, nil, fmt.Errorf("no connectors specified")
	}

	cat := s.buildConnectorValidationCatalog(ctx, user, convAccounts)
	if cat.prefsErr != nil {
		return nil, nil, fmt.Errorf("load connector preferences: %w", cat.prefsErr)
	}
	hidden := agentcore.InteractiveHiddenTools()
	var displayed map[string]bool
	if len(turnAdvertised) > 0 && turnAdvertised[0] != nil {
		displayed = turnAdvertised[0]
	} else {
		displayed = cat.displayedPromptSet(currentEnabled, hidden, personaPolicy)
	}
	alreadyEnabled := make(map[string]bool, len(currentEnabled))
	for _, n := range currentEnabled {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" {
			alreadyEnabled[k] = true
		}
	}

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

		canonName := strings.TrimSpace(info.Name)
		normName := strings.ToLower(canonName)
		if normName == "" || seen[normName] {
			continue
		}
		seen[normName] = true

		if !connectorPermittedInChat(info, hidden, personaPolicy) {
			return nil, nil, fmt.Errorf("connector %q is not permitted for the current persona or interactive chat policy", agentcore.PromptSafeName(normName))
		}
		if !alreadyEnabled[normName] && !displayed[normName] {
			return nil, nil, fmt.Errorf("connector %q is outside the %d optional connectors listed in the system prompt for this turn; toggle it directly in the Tools picker", agentcore.PromptSafeName(normName), agent.MaxAvailableOptionalInPrompt)
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
	regServer := agentcore.RegisteredMCPName(info.Name, info.SelectedAccount)
	for _, t := range info.Tools {
		full := agentcore.MCPToolName(regServer, t)
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
	turnStartOptional ...[]agent.OptionalServerInfo,
) tools.EnableMCPConnectorsHandler {
	var turnSnapshot []agent.OptionalServerInfo
	hasSnapshot := false
	if len(turnStartOptional) > 0 && turnStartOptional[0] != nil {
		turnSnapshot = append([]agent.OptionalServerInfo(nil), turnStartOptional[0]...)
		hasSnapshot = true
	}
	var turnStartEnabled []string
	if conv != nil {
		turnStartEnabled = append([]string(nil), conv.OptionalMCPServersEnabled...)
	}
	return func(ctx context.Context, toolCallID string, params tools.EnableMCPConnectorsParams) (*tools.EnableMCPConnectorsResult, error) {
		if s.store == nil {
			return nil, fmt.Errorf("enable_mcp_connectors requires a conversation store")
		}
		unlock := s.connectorLocks.lock(user + "\x00" + conv.ID)
		defer unlock()

		// Load fresh conversation state so we validate against its seat overrides and merge against the latest enabled list.
		fresh, gerr := s.store.Get(ctx, user, conv.ID)
		if gerr != nil || fresh == nil {
			if gerr == nil {
				gerr = fmt.Errorf("conversation not found")
			}
			return nil, fmt.Errorf("load conversation: %w", gerr)
		}

		var policy *agentcore.PersonaToolPermissions
		if turnStager != nil {
			turnStager.mcpMu.RLock()
			policy = turnStager.personaPolicy
			turnStager.mcpMu.RUnlock()
		}
		if policy == nil {
			effectivePersona, _ := s.resolveTurnPersona(ctx, fresh)
			policy = s.personaPolicyFor(effectivePersona)
		}

		var turnAdvertised map[string]bool
		if hasSnapshot {
			turnAdvertised = s.displayedPromptSetFromSnapshot(turnSnapshot, turnStartEnabled, agentcore.InteractiveHiddenTools(), policy)
		}

		currentEnabled := append([]string(nil), fresh.OptionalMCPServersEnabled...)
		toEnable, displayNames, err := s.validateEnableMCPConnectors(ctx, user, params.Connectors, currentEnabled, policy, fresh.MCPAccounts, turnAdvertised)
		if err != nil {
			return nil, err
		}

		enabledSet := make(map[string]bool, len(currentEnabled))
		for _, n := range currentEnabled {
			enabledSet[strings.ToLower(strings.TrimSpace(n))] = true
		}

		var newlyAdded []string
		for _, n := range toEnable {
			if !enabledSet[strings.ToLower(strings.TrimSpace(n))] {
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
			norm := strings.ToLower(strings.TrimSpace(n))
			if norm != "" && !enabledSet[norm] {
				enabledSet[norm] = true
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
			return s.autoApproveEnableMCPConnectors(ctx, conv, user, toolCallID, rawInput, newlyAdded, merged, buf, turnStager)
		}
		return s.stageEnableMCPConnectorsApproval(ctx, conv, user, toolCallID, rawInput, newlyAdded, buf, turnStager)
	}
}

func (s *Server) personaPolicyFor(persona string) *agentcore.PersonaToolPermissions {
	type personaPolicyProvider interface {
		PersonaPolicy(persona string) *agentcore.PersonaToolPermissions
	}
	if p, ok := s.agent.(personaPolicyProvider); ok && p != nil {
		return p.PersonaPolicy(persona)
	}
	return nil
}

func (s *Server) autoApproveEnableMCPConnectors(
	ctx context.Context,
	conv *store.Conversation,
	user, toolCallID, rawInput string,
	newlyAdded, merged []string,
	buf *turnBuffer,
	turnStager *approvalStager,
) (*tools.EnableMCPConnectorsResult, error) {
	safeAdded := promptSafeSlice(newlyAdded)
	recordText := fmt.Sprintf("%s: auto-approved enabling optional MCP connector(s) %s for this conversation.",
		notifyRecordResultPrefix, strings.Join(safeAdded, ", "))
	expiry := int64(0)
	if turnStager != nil {
		expiry = turnStager.expiryUnixFor(tools.EnableMCPConnectorsToolName)
	}
	allowResume := !s.shuttingDown.Load() && !UpdateDrainActive()
	maxPerHour := agentcore.ResumeMaxPerHour()
	autoRes, err := s.store.AutoApproveEnableMCPConnectors(ctx, store.AutoApproveConnectorsRequest{
		UserEmail:      user,
		ConversationID: conv.ID,
		ToolName:       tools.EnableMCPConnectorsToolName,
		ToolCallID:     toolCallID,
		ArgsJSON:       rawInput,
		ResultText:     recordText,
		ExpiresAt:      expiry,
		MergedServers:  merged,
		AllowResume:    allowResume,
		MaxPerHour:     maxPerHour,
		MaxPending:     maxPendingInputs,
		IgnoreOutstanding: func(id string) bool {
			_, failed := s.approvalPersistenceFailures.Load(id)
			return failed
		},
	})
	approval := autoRes.Approval
	if err != nil || approval == nil {
		if err == nil {
			err = fmt.Errorf("store returned nil approval")
		}
		return nil, fmt.Errorf("auto-approve enable connectors: %w", err)
	}
	if autoRes.Superseded > 0 && buf != nil {
		buf.Emit("tool.approval_superseded", map[string]any{
			"tool":  tools.EnableMCPConnectorsToolName,
			"count": autoRes.Superseded,
		})
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

	resumed := autoRes.ResumeInput != nil
	if resumed {
		metrics.RecordApprovalResume("queued")
		//nolint:gosec // G706: every value is %q-quoted (CR/LF escaped) and server-generated.
		log.Printf("audit: approval resume queued for conversation %q as input %q after %q",
			conv.ID, autoRes.ResumeInput.ID, approvalResumeIDs(autoRes.ResumeClaimed))
		s.emitQueueUpdate(ctx, autoRes.ResumeInput.UserEmail, conv.ID)
		s.maybeDrainQueue(conv.ID)
	} else if autoRes.ResumeSkipped != "" {
		metrics.RecordApprovalResume(autoRes.ResumeSkipped)
		//nolint:gosec // G706: every value is %q-quoted (CR/LF escaped) and server-generated.
		log.Printf("audit: approval resume skipped for conversation %q (%s; %d in the last hour, cap %d) after %q",
			conv.ID, autoRes.ResumeSkipped, autoRes.RecentResumes, maxPerHour, approval.ID)
	}

	msg := fmt.Sprintf("Auto-approved and enabled optional MCP connector(s) [%s] for this conversation. Stopping this step now so your turn automatically continues with their tools mounted.", strings.Join(safeAdded, ", "))
	if !resumed {
		msg = fmt.Sprintf("Auto-approved and enabled optional MCP connector(s) [%s] for this conversation. Automatic turn continuation was not queued (resume cap, queue limit, or pending state), so their tools will be mounted on the next turn — finish what you can with your current tools and let the user know.", strings.Join(safeAdded, ", "))
	}

	return &tools.EnableMCPConnectorsResult{
		Status:          "enabled",
		Connectors:      safeAdded,
		EnabledOptional: promptSafeSlice(merged),
		AutoApproved:    true,
		ApprovalID:      approval.ID,
		Message:         msg,
		StopTurn:        resumed,
	}, nil
}

func mergeStagedEnableMCPConnectorsArgs(current stagedEnableMCPConnectorsArgs, supersededArgsJSON []string) string {
	seen := make(map[string]bool, len(current.Connectors))
	var mergedConns []string
	mergedDisplay := make(map[string]string, len(current.DisplayNames))
	var reasons []string
	addEntry := func(conn string, disp map[string]string) {
		c := strings.TrimSpace(conn)
		norm := strings.ToLower(c)
		if norm == "" || seen[norm] {
			return
		}
		seen[norm] = true
		mergedConns = append(mergedConns, c)
		if d := strings.TrimSpace(disp[c]); d != "" {
			mergedDisplay[c] = d
		} else if d := strings.TrimSpace(disp[norm]); d != "" {
			mergedDisplay[c] = d
		}
	}
	for _, raw := range supersededArgsJSON {
		var prev stagedEnableMCPConnectorsArgs
		if err := json.Unmarshal([]byte(raw), &prev); err != nil {
			continue
		}
		for _, c := range prev.Connectors {
			addEntry(c, prev.DisplayNames)
		}
		if r := strings.TrimSpace(prev.Reason); r != "" && !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	for _, c := range current.Connectors {
		addEntry(c, current.DisplayNames)
	}
	if r := strings.TrimSpace(current.Reason); r != "" && !slices.Contains(reasons, r) {
		reasons = append(reasons, r)
	}
	outBytes, err := json.Marshal(stagedEnableMCPConnectorsArgs{
		Connectors:   mergedConns,
		DisplayNames: mergedDisplay,
		Reason:       strings.Join(reasons, "; "),
	})
	if err != nil {
		return ""
	}
	return string(outBytes)
}

func (s *Server) stageEnableMCPConnectorsApproval(
	ctx context.Context,
	conv *store.Conversation,
	user, toolCallID, rawInput string,
	newlyAdded []string,
	buf *turnBuffer,
	turnStager *approvalStager,
) (*tools.EnableMCPConnectorsResult, error) {
	expiry := int64(0)
	if turnStager != nil {
		expiry = turnStager.expiryUnixFor(tools.EnableMCPConnectorsToolName)
	}
	var currentArgs stagedEnableMCPConnectorsArgs
	_ = json.Unmarshal([]byte(rawInput), &currentArgs)
	approval, superseded, err := s.store.StageEnableMCPConnectorsApproval(
		ctx, user, conv.ID, tools.EnableMCPConnectorsToolName, toolCallID, rawInput, expiry,
		func(supersededArgsJSON []string) string {
			return mergeStagedEnableMCPConnectorsArgs(currentArgs, supersededArgsJSON)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("stage connector approval: %w", err)
	}
	if superseded > 0 && buf != nil {
		buf.Emit("tool.approval_superseded", map[string]any{
			"tool":  tools.EnableMCPConnectorsToolName,
			"count": superseded,
		})
	}

	if buf != nil {
		buf.Emit("tool.approval_required", approvalRequiredEvent(approval, approval.ArgsJSON, conv.ID))
	}
	if turnStager != nil {
		turnStager.firePushNotification(tools.EnableMCPConnectorsToolName)
	}

	stagedConns := newlyAdded
	var finalArgs stagedEnableMCPConnectorsArgs
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &finalArgs); err == nil && len(finalArgs.Connectors) > 0 {
		stagedConns = finalArgs.Connectors
	}
	safeAdded := promptSafeSlice(stagedConns)
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
	unlock := s.connectorLocks.lock(user + "\x00" + convID)
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
			seen := make(map[string]bool, len(merged))
			for _, m := range merged {
				seen[strings.ToLower(strings.TrimSpace(m))] = true
			}
			for _, c := range args.Connectors {
				c = strings.TrimSpace(c)
				norm := strings.ToLower(c)
				if norm != "" && !seen[norm] {
					seen[norm] = true
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

	effectivePersona, _ := s.resolveTurnPersona(ctx, conv)
	toEnable, _, err := s.validateEnableMCPConnectors(ctx, user, args.Connectors, conv.OptionalMCPServersEnabled, s.personaPolicyFor(effectivePersona), conv.MCPAccounts, advertisedSetFromNames(args.Connectors))
	if err != nil {
		return "", err
	}

	merged := append([]string(nil), conv.OptionalMCPServersEnabled...)
	seen := make(map[string]bool, len(merged))
	for _, m := range merged {
		seen[strings.ToLower(strings.TrimSpace(m))] = true
	}
	for _, c := range toEnable {
		norm := strings.ToLower(strings.TrimSpace(c))
		if norm != "" && !seen[norm] {
			seen[norm] = true
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
