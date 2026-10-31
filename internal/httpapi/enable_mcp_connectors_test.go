package httpapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

type enableMCPTestEngine struct {
	*fakeEngine
	catalog  []agent.OptionalServerInfo
	alwaysOn []agent.AlwaysOnServerInfo
	meta     []clientconfig.ConnectorMeta
}

func (e *enableMCPTestEngine) MCPServerCatalog() []agent.OptionalServerInfo {
	return e.catalog
}

func (e *enableMCPTestEngine) AlwaysOnMCPServerCatalog() []agent.AlwaysOnServerInfo {
	return e.alwaysOn
}

func (e *enableMCPTestEngine) ConnectorMeta() []clientconfig.ConnectorMeta {
	return e.meta
}

type enableMCPTestStore struct {
	*fakeChatStore
	mu             sync.Mutex
	prefs          map[string]store.ConnectorPref
	approvals      map[string]*store.Approval
	nextApproval   int
	resumeClaimed  int
	failArmResume  bool
	failCreateAppr bool
	skipResume     bool
}

func newEnableMCPTestStore() *enableMCPTestStore {
	return &enableMCPTestStore{
		fakeChatStore: newFakeChatStore(),
		approvals:     make(map[string]*store.Approval),
	}
}

func (s *enableMCPTestStore) ListConnectorPrefs(_ context.Context, _ string) (map[string]store.ConnectorPref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefs, nil
}

func (s *enableMCPTestStore) SupersedePendingApprovals(_ context.Context, convID, toolName string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, a := range s.approvals {
		if a.ConversationID == convID && a.ToolName == toolName && a.Status == "pending" {
			a.Status = "superseded"
			n++
		}
	}
	return n, nil
}

func (s *enableMCPTestStore) CreateApproval(_ context.Context, convID, user, toolName, callID, argsJSON string, expiry int64, _ store.ApprovalSeat) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCreateAppr {
		return nil, fmt.Errorf("create approval failed")
	}
	s.nextApproval++
	id := fmt.Sprintf("appr-%d", s.nextApproval)
	a := &store.Approval{
		ID:             id,
		ConversationID: convID,
		UserEmail:      user,
		ToolName:       toolName,
		ToolCallID:     callID,
		ArgsJSON:       argsJSON,
		Status:         "pending",
		ExpiresAt:      expiry,
	}
	s.approvals[id] = a
	return a, nil
}

func (s *enableMCPTestStore) GetApproval(_ context.Context, user, id string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.approvals[id]
	if a == nil || (user != "" && a.UserEmail != user) {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

func (s *enableMCPTestStore) ArmApprovalResume(_ context.Context, _, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failArmResume {
		return false, fmt.Errorf("arm resume failed")
	}
	a := s.approvals[id]
	if a == nil {
		return false, nil
	}
	a.ResumeState = store.ApprovalResumeArmed
	return true, nil
}

func (s *enableMCPTestStore) ResolveApproval(_ context.Context, _, id, status, resultText string) error {
	if status != "approved" && status != "rejected" {
		return fmt.Errorf("invalid approval status %q", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.approvals[id]; a != nil {
		a.Status = status
		a.ResultText = resultText
	}
	return nil
}

func (s *enableMCPTestStore) AutoApproveEnableMCPConnectors(ctx context.Context, req store.AutoApproveConnectorsRequest) (store.AutoApproveConnectorsResult, error) {
	var out store.AutoApproveConnectorsResult
	s.mu.Lock()
	if s.failCreateAppr {
		s.mu.Unlock()
		return out, fmt.Errorf("create approval failed")
	}
	if s.failArmResume {
		s.mu.Unlock()
		return out, fmt.Errorf("arm resume failed")
	}
	for _, a := range s.approvals {
		if a.ConversationID == req.ConversationID && a.ToolName == req.ToolName && a.Status == "pending" {
			a.Status = "rejected"
			if a.ResumeState == store.ApprovalResumeArmed {
				a.ResumeState = store.ApprovalResumeSuperseded
			}
			out.Superseded++
		}
	}
	s.nextApproval++
	id := fmt.Sprintf("appr-%d", s.nextApproval)
	resumeState := store.ApprovalResumeClaimed
	if !req.AllowResume || s.skipResume {
		resumeState = store.ApprovalResumeSkipped
		if s.skipResume {
			out.ResumeSkipped = store.ApprovalResumeSkipRateLimited
		}
	} else {
		s.resumeClaimed++
		out.ResumeClaimed = []store.ApprovalResumeItem{{
			ID:       id,
			ToolName: req.ToolName,
			Outcome:  store.ApprovalResumeOutcomeApproved,
		}}
		out.ResumeInput = &store.InputQueueRow{
			ID:             fmt.Sprintf("resume-%d", s.resumeClaimed),
			ConversationID: req.ConversationID,
			UserEmail:      req.UserEmail,
			Mode:           "resume",
		}
	}
	a := &store.Approval{
		ID:             id,
		ConversationID: req.ConversationID,
		UserEmail:      req.UserEmail,
		ToolName:       req.ToolName,
		ToolCallID:     req.ToolCallID,
		ArgsJSON:       req.ArgsJSON,
		Status:         "approved",
		ResultText:     req.ResultText,
		ExpiresAt:      req.ExpiresAt,
		ResumeState:    resumeState,
	}
	s.approvals[id] = a
	s.mu.Unlock()

	if err := s.SetOptionalMCPServers(ctx, req.UserEmail, req.ConversationID, req.MergedServers); err != nil {
		return out, err
	}
	out.Approval = a
	return out, nil
}

func (s *enableMCPTestStore) StageEnableMCPConnectorsApproval(_ context.Context, userEmail, convID, toolName, toolCallID, argsJSON string, expiresAt int64, buildArgs func(supersededArgsJSON []string) string) (*store.Approval, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCreateAppr {
		return nil, 0, fmt.Errorf("create approval failed")
	}
	if s.failArmResume {
		return nil, 0, fmt.Errorf("arm resume failed")
	}
	var superseded int64
	var supersededArgs []string
	for _, a := range s.approvals {
		if a.ConversationID == convID && a.ToolName == toolName && a.Status == "pending" {
			a.Status = "rejected"
			if a.ResumeState == store.ApprovalResumeArmed {
				a.ResumeState = store.ApprovalResumeSuperseded
			}
			supersededArgs = append(supersededArgs, a.ArgsJSON)
			superseded++
		}
	}
	if buildArgs != nil && len(supersededArgs) > 0 {
		if merged := buildArgs(supersededArgs); merged != "" {
			argsJSON = merged
		}
	}
	s.nextApproval++
	id := fmt.Sprintf("appr-%d", s.nextApproval)
	a := &store.Approval{
		ID:             id,
		ConversationID: convID,
		UserEmail:      userEmail,
		ToolName:       toolName,
		ToolCallID:     toolCallID,
		ArgsJSON:       argsJSON,
		Status:         "pending",
		ExpiresAt:      expiresAt,
		ResumeState:    store.ApprovalResumeArmed,
	}
	s.approvals[id] = a
	return a, superseded, nil
}

func (s *enableMCPTestStore) ClaimApproval(_ context.Context, _, id, newStatus, resultText string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.approvals[id]
	if a == nil || a.Status != "pending" {
		return false, nil
	}
	a.Status = newStatus
	a.ResultText = resultText
	return true, nil
}

func (s *enableMCPTestStore) SetApprovalResult(_ context.Context, _, id, resultText string, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.approvals[id]; a != nil {
		a.ResultText = resultText
	}
	return nil
}

func (s *enableMCPTestStore) ClaimApprovalResume(_ context.Context, req store.ApprovalResumeRequest) (store.ApprovalResumeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed []store.ApprovalResumeItem
	var owner string
	for _, a := range s.approvals {
		if a.ConversationID != req.ConversationID {
			continue
		}
		if a.Status == "pending" || a.Status == "executing" {
			return store.ApprovalResumeResult{Outstanding: []string{a.ID}}, nil
		}
		if a.ResumeState == store.ApprovalResumeArmed && a.Status == "approved" {
			a.ResumeState = "claimed"
			owner = a.UserEmail
			claimed = append(claimed, store.ApprovalResumeItem{
				ID:       a.ID,
				ToolName: a.ToolName,
				Outcome:  a.Status,
			})
		}
	}
	if len(claimed) == 0 {
		return store.ApprovalResumeResult{}, nil
	}
	if s.skipResume {
		for _, a := range s.approvals {
			if a.ConversationID == req.ConversationID && a.ResumeState == "claimed" {
				a.ResumeState = store.ApprovalResumeSkipped
			}
		}
		return store.ApprovalResumeResult{
			Owner:   owner,
			Claimed: claimed,
			Skipped: store.ApprovalResumeSkipRateLimited,
		}, nil
	}
	s.resumeClaimed++
	return store.ApprovalResumeResult{
		Owner:   owner,
		Claimed: claimed,
		Input: &store.InputQueueRow{
			ID:             fmt.Sprintf("resume-%d", s.resumeClaimed),
			ConversationID: req.ConversationID,
			UserEmail:      owner,
			Mode:           "resume",
		},
	}, nil
}

func (s *enableMCPTestStore) ListPendingInputs(_ context.Context, _, _ string) ([]store.InputQueueRow, error) {
	return nil, nil
}

func (s *enableMCPTestStore) ClaimNextInput(_ context.Context, _ string) (*store.InputQueueRow, error) {
	return nil, nil
}

func TestValidateEnableMCPConnectors(t *testing.T) {
	st := newEnableMCPTestStore()
	st.prefs = map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "disabled_by_user"): {
			Kind:        store.ConnectorKindBundled,
			ConnectorID: "disabled_by_user",
			Enabled:     false,
		},
	}
	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "slack",
				DisplayName: "Slack",
				Description: "Post messages and read channels",
				ToolCount:   3,
				Tools:       []string{"post_message", "list_channels", "read_thread"},
			},
			{
				Name:        "disabled_by_user",
				DisplayName: "Disabled By User",
				ToolCount:   2,
				Tools:       []string{"foo", "bar"},
			},
			{
				Name:        "down_server",
				DisplayName: "Down Server",
				ToolCount:   0,
			},
			{
				Name:        "persona_denied",
				DisplayName: "Persona Denied",
				ToolCount:   1,
				Tools:       []string{"secret_op"},
			},
			{
				Name:        "Google Drive",
				DisplayName: "Google Drive",
				ToolCount:   2,
				Tools:       []string{"search_files", "read_file"},
			},
		},
		alwaysOn: []agent.AlwaysOnServerInfo{
			{Name: "pages", Available: true, ToolCount: 4},
		},
		meta: []clientconfig.ConnectorMeta{
			{Name: "unconfigured_crm", Configured: false, Optional: true},
		},
	}
	srv := &Server{
		cfg:   &config.Config{},
		agent: engine,
		store: st,
	}

	policy := &agentcore.PersonaToolPermissions{
		Deny: []string{"mcp:persona_denied/*"},
	}

	// 1. Valid optional connector + already always-on connector succeeds and returns only the optional connector.
	toEnable, displayNames, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{" Slack ", "pages"}, nil, policy, nil)
	if err != nil {
		t.Fatalf("expected valid connectors to succeed, got: %v", err)
	}
	if !slices.Equal(toEnable, []string{"slack"}) {
		t.Fatalf("toEnable = %v, want [slack]", toEnable)
	}
	if displayNames["slack"] != "Slack" {
		t.Fatalf("displayNames[slack] = %q, want Slack", displayNames["slack"])
	}

	// 2. Unknown connector rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"nonexistent"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "unknown or unavailable") {
		t.Fatalf("expected unknown connector error, got: %v", err)
	}

	// 3. Unconfigured (needs_setup) connector rejected with clear setup message.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"unconfigured_crm"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "not configured on this server") {
		t.Fatalf("expected unconfigured connector error, got: %v", err)
	}

	// 4. Connector disabled in user preferences rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"disabled_by_user"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "Settings → Connections") {
		t.Fatalf("expected user-pref disabled error, got: %v", err)
	}

	// 5. Unavailable 0-tool connector rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"down_server"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "exposed 0 tools") {
		t.Fatalf("expected unavailable 0-tool connector error, got: %v", err)
	}

	// 6. Persona-blocked connector rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"persona_denied"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected persona-blocked error, got: %v", err)
	}

	// 7. Prompt-safe name ("google_drive") resolves back to canonical stored connector name ("Google Drive").
	toEnableSafe, dnSafe, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"google_drive"}, nil, policy, nil)
	if err != nil {
		t.Fatalf("expected prompt-safe connector name to resolve, got: %v", err)
	}
	if !slices.Equal(toEnableSafe, []string{"Google Drive"}) {
		t.Fatalf("toEnableSafe = %v, want [\"Google Drive\"]", toEnableSafe)
	}
	if dnSafe["Google Drive"] != "Google Drive" {
		t.Fatalf("dnSafe[\"Google Drive\"] = %q, want Google Drive", dnSafe["Google Drive"])
	}

	// 8. Ambiguous prompt-safe alias ("Google Drive" vs "google_drive") is rejected.
	engine.catalog = append(engine.catalog, agent.OptionalServerInfo{
		Name:      "google_drive",
		ToolCount: 1,
		Tools:     []string{"list"},
	})
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"google_drive"}, nil, policy, nil); err == nil || !strings.Contains(err.Error(), "ambiguous identifier") {
		t.Fatalf("expected ambiguous identifier error, got: %v", err)
	}
}

func TestEnableMCPConnectorsHandler_UserApprovalAndExecution(t *testing.T) {
	st := newEnableMCPTestStore()
	conv, _ := st.CreateConversation(context.Background(), "alice@example.com", "Test", "victoria", "anthropic/claude-opus-4.8", false)
	conv.OptionalMCPServersEnabled = []string{"existing_srv"}

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "slack",
				DisplayName: "Slack",
				Description: "Post messages",
				ToolCount:   2,
				Tools:       []string{"post_message", "list_channels"},
			},
			{
				Name:        "hubspot",
				DisplayName: "HubSpot",
				Description: "CRM records",
				ToolCount:   2,
				Tools:       []string{"get_contact", "list_deals"},
			},
		},
	}
	srv := &Server{
		cfg:      &config.Config{},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	buf := newTurnBuffer(conv.ID, "turn-1")
	stager := &approvalStager{
		ctx:            context.Background(),
		store:          st,
		conversationID: conv.ID,
		userEmail:      "alice@example.com",
		sink:           buf,
	}

	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager)
	res1, err := handler(context.Background(), "call-enable-1", tools.EnableMCPConnectorsParams{
		Connectors: []string{"slack"},
		Reason:     "Read the #ops channel",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res1.Status != "approval_required" || res1.StopTurn {
		t.Fatalf("unexpected result: %+v", res1)
	}

	// A second enable_mcp_connectors call in the same step for a distinct connector
	// supersedes the first card while merging its requested connector(s) into the new card.
	res, err := handler(context.Background(), "call-enable-2", tools.EnableMCPConnectorsParams{
		Connectors: []string{"hubspot"},
		Reason:     "Look up the account owner",
	})
	if err != nil {
		t.Fatalf("second handler error: %v", err)
	}
	if !slices.Equal(res.Connectors, []string{"slack", "hubspot"}) {
		t.Fatalf("merged connectors on second staged card = %v, want [slack hubspot]", res.Connectors)
	}

	// Approval row must be pending and armed for automatic resume.
	appr, _ := st.GetApproval(context.Background(), "alice@example.com", res.ApprovalID)
	if appr == nil || appr.Status != "pending" || appr.ResumeState != store.ApprovalResumeArmed {
		t.Fatalf("expected armed pending approval row, got %+v", appr)
	}

	// Approve via decideApproval and verify OptionalMCPServersEnabled updates with both connectors and resume=true.
	decision := srv.decideApproval(context.Background(), nil, "alice@example.com", conv.ID, res.ApprovalID, approvalRequest{Approved: true})
	if decision.status != 0 {
		t.Fatalf("decideApproval error (%d): %s", decision.status, decision.errText)
	}
	if decision.body["status"] != "approved" || decision.body["resume"] != true {
		t.Fatalf("expected status=approved and resume=true, got %+v", decision.body)
	}

	updatedConv, _ := st.Get(context.Background(), "alice@example.com", conv.ID)
	if !slices.Equal(updatedConv.OptionalMCPServersEnabled, []string{"existing_srv", "hubspot", "slack"}) {
		t.Fatalf("OptionalMCPServersEnabled = %v, want [existing_srv hubspot slack]", updatedConv.OptionalMCPServersEnabled)
	}
}

func TestEnableMCPConnectorsHandler_AutoApprove(t *testing.T) {
	st := newEnableMCPTestStore()
	conv, _ := st.CreateConversation(context.Background(), "alice@example.com", "Test", "victoria", "anthropic/claude-opus-4.8", false)

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "slack",
				DisplayName: "Slack",
				Description: "Post messages",
				ToolCount:   2,
				Tools:       []string{"post_message", "list_channels"},
			},
		},
	}
	srv := &Server{
		cfg:      &config.Config{},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	buf := newTurnBuffer(conv.ID, "turn-auto")
	stager := &approvalStager{
		ctx:               context.Background(),
		store:             st,
		conversationID:    conv.ID,
		userEmail:         "alice@example.com",
		sink:              buf,
		autoApproveInTest: true,
	}

	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager)
	res, err := handler(context.Background(), "call-enable-auto", tools.EnableMCPConnectorsParams{
		Connectors: []string{"slack"},
		Reason:     "Post status update",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.Status != "enabled" || !res.AutoApproved || !res.StopTurn {
		t.Fatalf("expected enabled + AutoApproved + StopTurn, got %+v", res)
	}

	updatedConv, _ := st.Get(context.Background(), "alice@example.com", conv.ID)
	if !slices.Equal(updatedConv.OptionalMCPServersEnabled, []string{"slack"}) {
		t.Fatalf("OptionalMCPServersEnabled = %v, want [slack]", updatedConv.OptionalMCPServersEnabled)
	}

	st.mu.Lock()
	claimedCount := st.resumeClaimed
	st.mu.Unlock()
	if claimedCount != 1 {
		t.Fatalf("expected decideApprovalResume to claim 1 armed resume input, got %d", claimedCount)
	}
}

func TestEnableMCPConnectorsHandler_AutoApproveFailsClosedOnResumePersistenceError(t *testing.T) {
	st := newEnableMCPTestStore()
	st.failArmResume = true
	conv, _ := st.CreateConversation(context.Background(), "alice@example.com", "Test", "victoria", "anthropic/claude-opus-4.8", false)

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "slack",
				DisplayName: "Slack",
				ToolCount:   2,
				Tools:       []string{"post_message", "list_channels"},
			},
		},
	}
	srv := &Server{
		cfg:      &config.Config{},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	buf := newTurnBuffer(conv.ID, "turn-auto-fail")
	stager := &approvalStager{
		ctx:               context.Background(),
		store:             st,
		conversationID:    conv.ID,
		userEmail:         "alice@example.com",
		sink:              buf,
		autoApproveInTest: true,
	}

	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager)
	res, err := handler(context.Background(), "call-enable-auto-fail", tools.EnableMCPConnectorsParams{
		Connectors: []string{"slack"},
	})
	if err == nil {
		t.Fatalf("expected error when ArmApprovalResume fails, got res=%+v", res)
	}
	updatedConv, _ := st.Get(context.Background(), "alice@example.com", conv.ID)
	if len(updatedConv.OptionalMCPServersEnabled) != 0 {
		t.Fatalf("expected OptionalMCPServersEnabled to remain empty on failure, got %v", updatedConv.OptionalMCPServersEnabled)
	}
	for id, appr := range st.approvals {
		if appr.Status != "rejected" {
			t.Fatalf("approval %s left in status %q on cleanup failure, want rejected", id, appr.Status)
		}
	}
}

func TestEnableMCPConnectors_SkippedResumeDoesNotStopTurnAndEnforces25Cap(t *testing.T) {
	st := newEnableMCPTestStore()
	st.skipResume = true
	conv, _ := st.CreateConversation(context.Background(), "alice@example.com", "Test", "victoria", "anthropic/claude-opus-4.8", false)

	var catalog []agent.OptionalServerInfo
	for i := 0; i < 30; i++ {
		catalog = append(catalog, agent.OptionalServerInfo{
			Name:      fmt.Sprintf("conn_%02d", i),
			ToolCount: 1,
			Tools:     []string{"ping"},
		})
	}
	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog:    catalog,
	}
	srv := &Server{
		cfg:      &config.Config{},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	// Connector #25 (0-indexed: conn_25 is the 26th entry) is outside the 25-entry prompt cap.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"conn_25"}, nil, nil, nil); err == nil {
		t.Fatalf("expected conn_25 (26th connector) to be rejected by the 25-connector prompt cap")
	}

	// Connector #00 is inside the cap; when auto-approved with skipResume=true, StopTurn must be false.
	buf := newTurnBuffer(conv.ID, "turn-skip-resume")
	stager := &approvalStager{
		ctx:               context.Background(),
		store:             st,
		conversationID:    conv.ID,
		userEmail:         "alice@example.com",
		sink:              buf,
		autoApproveInTest: true,
	}
	turnSnapshot := srv.availableOptionalConnectorsForUser(context.Background(), "alice@example.com")
	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager, turnSnapshot)
	// Simulate a mid-turn ReloadMCPServers that prepends an alphabetically earlier connector ("aaa_new").
	// conn_24 (the 25th connector in the turn-start snapshot) must STILL be accepted, while aaa_new (never advertised) must be rejected.
	engine.catalog = append([]agent.OptionalServerInfo{{
		Name:      "aaa_new",
		ToolCount: 1,
		Tools:     []string{"ping"},
	}}, engine.catalog...)
	if _, err := handler(context.Background(), "call-unadvertised", tools.EnableMCPConnectorsParams{
		Connectors: []string{"aaa_new"},
	}); err == nil {
		t.Fatalf("expected unadvertised mid-turn connector aaa_new to be rejected")
	}
	res, err := handler(context.Background(), "call-skip", tools.EnableMCPConnectorsParams{
		Connectors: []string{"conn_24"},
	})
	if err != nil {
		t.Fatalf("unexpected error enabling advertised conn_24 after mid-turn reload: %v", err)
	}
	if res.StopTurn {
		t.Fatalf("expected StopTurn=false when AutoApproveEnableMCPConnectors skips enqueueing a continuation")
	}
}

func TestEnableMCPConnectorsHandler_MockModeApprovalUpdatesConversation(t *testing.T) {
	st := newEnableMCPTestStore()
	conv, _ := st.CreateConversation(context.Background(), "alice@example.com", "Test", "victoria", "anthropic/claude-opus-4.8", false)

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:        "slack",
				DisplayName: "Slack",
				ToolCount:   2,
				Tools:       []string{"post_message", "list_channels"},
			},
		},
	}
	srv := &Server{
		cfg:      &config.Config{MockMode: true},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	buf := newTurnBuffer(conv.ID, "turn-mock")
	stager := &approvalStager{
		ctx:            context.Background(),
		store:          st,
		conversationID: conv.ID,
		userEmail:      "alice@example.com",
		sink:           buf,
	}

	handler := srv.newEnableMCPConnectorsHandler(conv, "alice@example.com", buf, stager)
	res, err := handler(context.Background(), "call-enable-mock", tools.EnableMCPConnectorsParams{
		Connectors: []string{"slack"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	decision := srv.decideApproval(context.Background(), nil, "alice@example.com", conv.ID, res.ApprovalID, approvalRequest{Approved: true})
	if decision.status != 0 {
		t.Fatalf("decideApproval error (%d): %s", decision.status, decision.errText)
	}
	updatedConv, _ := st.Get(context.Background(), "alice@example.com", conv.ID)
	if !slices.Equal(updatedConv.OptionalMCPServersEnabled, []string{"slack"}) {
		t.Fatalf("OptionalMCPServersEnabled in MockMode = %v, want [slack]", updatedConv.OptionalMCPServersEnabled)
	}
}

func TestEnableMCPConnectors_UnavailableCollisionAndRemotePersonaAllow(t *testing.T) {
	st := newEnableMCPTestStore()

	engine := &enableMCPTestEngine{
		fakeEngine: &fakeEngine{},
		catalog: []agent.OptionalServerInfo{
			{
				Name:      "foo-bar",
				ToolCount: 0, // unavailable 0-tool bundled server
			},
			{
				Name:      "foo_bar",
				ToolCount: 2,
				Tools:     []string{"read_item", "list_items"},
			},
			{
				Name:      "Slack",
				ToolCount: 1,
				Tools:     []string{"post_message"},
			},
		},
	}
	srv := &Server{
		cfg:      &config.Config{},
		agent:    engine,
		store:    st,
		inflight: make(map[string]inflightEntry),
	}

	policy := &agentcore.PersonaToolPermissions{
		Allow: []string{
			"mcp:foo_bar/read_item",
			"mcp:Slack/post_message",
		},
	}
	got, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"foo_bar", "slack"}, nil, policy, nil)
	if err != nil {
		t.Fatalf("unexpected error enabling connected foo_bar + Slack under narrow persona Allow: %v", err)
	}
	if !slices.Equal(got, []string{"foo_bar", "Slack"}) {
		t.Fatalf("got %v, want [foo_bar Slack]", got)
	}
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{agent.OptionalNativeImageGenName}, nil, policy, nil); err == nil {
		t.Fatalf("expected image_generation to be rejected by validateEnableMCPConnectors")
	}
	if connectorPermittedInChat(agent.OptionalServerInfo{Name: "remote_box", Remote: true}, nil, policy) {
		t.Fatalf("expected remote_box to be rejected by connectorPermittedInChat")
	}
}
