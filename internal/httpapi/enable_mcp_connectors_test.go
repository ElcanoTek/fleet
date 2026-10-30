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
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.approvals[id]; a != nil {
		a.Status = status
		a.ResultText = resultText
	}
	return nil
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
	toEnable, displayNames, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{" Slack ", "pages"}, policy)
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
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"nonexistent"}, policy); err == nil || !strings.Contains(err.Error(), "unknown or unavailable") {
		t.Fatalf("expected unknown connector error, got: %v", err)
	}

	// 3. Unconfigured (needs_setup) connector rejected with clear setup message.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"unconfigured_crm"}, policy); err == nil || !strings.Contains(err.Error(), "not configured on this server") {
		t.Fatalf("expected unconfigured connector error, got: %v", err)
	}

	// 4. Connector disabled in user preferences rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"disabled_by_user"}, policy); err == nil || !strings.Contains(err.Error(), "Settings → Connections") {
		t.Fatalf("expected user-pref disabled error, got: %v", err)
	}

	// 5. Unavailable 0-tool connector rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"down_server"}, policy); err == nil || !strings.Contains(err.Error(), "exposed 0 tools") {
		t.Fatalf("expected unavailable 0-tool connector error, got: %v", err)
	}

	// 6. Persona-blocked connector rejected.
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"persona_denied"}, policy); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected persona-blocked error, got: %v", err)
	}

	// 7. Prompt-safe name ("google_drive") resolves back to canonical stored connector name ("google drive").
	toEnableSafe, dnSafe, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"google_drive"}, policy)
	if err != nil {
		t.Fatalf("expected prompt-safe connector name to resolve, got: %v", err)
	}
	if !slices.Equal(toEnableSafe, []string{"google drive"}) {
		t.Fatalf("toEnableSafe = %v, want [\"google drive\"]", toEnableSafe)
	}
	if dnSafe["google drive"] != "Google Drive" {
		t.Fatalf("dnSafe[\"google drive\"] = %q, want Google Drive", dnSafe["google drive"])
	}

	// 8. Ambiguous prompt-safe alias ("Google Drive" vs "google_drive") is rejected.
	engine.catalog = append(engine.catalog, agent.OptionalServerInfo{
		Name:      "google_drive",
		ToolCount: 1,
		Tools:     []string{"list"},
	})
	if _, _, err := srv.validateEnableMCPConnectors(context.Background(), "alice@example.com", []string{"google_drive"}, policy); err == nil || !strings.Contains(err.Error(), "ambiguous identifier") {
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
	res, err := handler(context.Background(), "call-enable-1", tools.EnableMCPConnectorsParams{
		Connectors: []string{"slack"},
		Reason:     "Read the #ops channel",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.Status != "approval_required" || res.StopTurn {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.Message, "APPROVAL_REQUIRED") {
		t.Fatalf("expected APPROVAL_REQUIRED in message, got %q", res.Message)
	}

	// Approval row must be pending and armed for automatic resume.
	appr, _ := st.GetApproval(context.Background(), "alice@example.com", res.ApprovalID)
	if appr == nil || appr.Status != "pending" || appr.ResumeState != store.ApprovalResumeArmed {
		t.Fatalf("expected armed pending approval row, got %+v", appr)
	}

	// Approve via decideApproval and verify OptionalMCPServersEnabled updates and resume=true.
	decision := srv.decideApproval(context.Background(), nil, "alice@example.com", conv.ID, res.ApprovalID, approvalRequest{Approved: true})
	if decision.status != 0 {
		t.Fatalf("decideApproval error (%d): %s", decision.status, decision.errText)
	}
	if decision.body["status"] != "approved" || decision.body["resume"] != true {
		t.Fatalf("expected status=approved and resume=true, got %+v", decision.body)
	}

	updatedConv, _ := st.Get(context.Background(), "alice@example.com", conv.ID)
	if !slices.Equal(updatedConv.OptionalMCPServersEnabled, []string{"existing_srv", "slack"}) {
		t.Fatalf("OptionalMCPServersEnabled = %v, want [existing_srv slack]", updatedConv.OptionalMCPServersEnabled)
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
