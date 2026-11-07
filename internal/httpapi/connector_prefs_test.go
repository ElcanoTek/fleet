package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// prefsFakeStore is fakeChatStore plus an in-memory user_connector_prefs so
// the /connector-prefs handler and the availability helpers can be exercised
// without a database.
type prefsFakeStore struct {
	*fakeChatStore
	prefs    map[string]store.ConnectorPref
	projects map[string]*store.Project
}

func (s *prefsFakeStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	if s.projects == nil {
		return nil, nil
	}
	return s.projects[id], nil
}

func (s *prefsFakeStore) CreateProjectConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool, projectID string, mcpServers []string) (*store.Conversation, error) {
	conv, err := s.CreateConversation(ctx, userEmail, title, persona, model, lockdown)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	conv.ProjectID = projectID
	if mcpServers == nil {
		conv.OptionalMCPServersEnabled = []string{}
	} else {
		conv.OptionalMCPServersEnabled = append([]string(nil), mcpServers...)
	}
	s.mu.Unlock()
	return conv, nil
}

func (s *prefsFakeStore) SetConnectorPref(_ context.Context, _ string, p store.ConnectorPref) error {
	if p.Kind != store.ConnectorKindBundled && p.Kind != store.ConnectorKindRemote {
		return store.ErrConnectorPrefInvalid
	}
	if s.prefs == nil {
		s.prefs = map[string]store.ConnectorPref{}
	}
	s.prefs[store.ConnectorPrefKey(p.Kind, p.ConnectorID)] = p
	return nil
}

func (s *prefsFakeStore) DeleteConnectorPref(_ context.Context, _, kind, id string) error {
	delete(s.prefs, store.ConnectorPrefKey(kind, id))
	return nil
}

func (s *prefsFakeStore) ListConnectorPrefs(_ context.Context, _ string) (map[string]store.ConnectorPref, error) {
	return s.prefs, nil
}

func prefsRequest(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	s.connectorPrefs(w, r)
	return w
}

// The /connector-prefs endpoint: upsert, list, revert; malformed prefs are
// rejected; a seat is validated against the live catalog.
func TestConnectorPrefsEndpoint(t *testing.T) {
	t.Parallel()

	fs := &prefsFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{store: fs}

	if w := prefsRequest(t, s, http.MethodPut, "/connector-prefs",
		`{"kind":"bundled","connector_id":"gamma","enabled":false}`); w.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := prefsRequest(t, s, http.MethodPut, "/connector-prefs",
		`{"kind":"weird","connector_id":"x","enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad kind accepted: %d", w.Code)
	}

	w := prefsRequest(t, s, http.MethodGet, "/connector-prefs", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	var resp struct {
		Prefs []store.ConnectorPref `json:"prefs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Prefs) != 1 || resp.Prefs[0].ConnectorID != "gamma" || resp.Prefs[0].Enabled {
		t.Errorf("prefs = %+v", resp.Prefs)
	}

	if w := prefsRequest(t, s, http.MethodDelete, "/connector-prefs?kind=bundled&id=gamma", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	w = prefsRequest(t, s, http.MethodGet, "/connector-prefs", "")
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Prefs) != 0 {
		t.Errorf("revert left prefs: %+v", resp.Prefs)
	}
}

// The availability helpers implement the layer contract: no explicit pref =
// available with the operator default; explicit disable hides; a stale seat
// degrades to the default seat.
func TestBundledPrefResolution(t *testing.T) {
	t.Parallel()

	info := agent.OptionalServerInfo{Name: "gamma", EnabledByDefault: true, Accounts: []string{"client-a"}}
	none := map[string]store.ConnectorPref{}

	if avail, on, seat := bundledPrefFor(none, info); !avail || !on || seat != "" {
		t.Errorf("no pref: avail=%v on=%v seat=%q", avail, on, seat)
	}
	off := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: false},
	}
	if avail, _, _ := bundledPrefFor(off, info); avail {
		t.Error("explicit disable should hide the connector")
	}
	staleSeat := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, DefaultAccount: "gone"},
	}
	if _, _, seat := bundledPrefFor(staleSeat, info); seat != "" {
		t.Errorf("stale seat should degrade to default, got %q", seat)
	}
	goodSeat := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, DefaultAccount: "client-a"},
	}
	if _, _, seat := bundledPrefFor(goodSeat, info); seat != "client-a" {
		t.Errorf("seat = %q, want client-a", seat)
	}

	// Two-toggle semantics: an explicit row carries the user's complete
	// choice — available without auto_enable means new chats start OFF even
	// when the operator default is on; auto_enable turns seeding back on;
	// and a disabled row never seeds regardless of auto_enable.
	availOnly := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true},
	}
	if avail, on, _ := bundledPrefFor(availOnly, info); !avail || on {
		t.Errorf("available-only pref: avail=%v on=%v, want true,false", avail, on)
	}
	alwaysOn := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: true, AutoEnable: true},
	}
	if avail, on, _ := bundledPrefFor(alwaysOn, info); !avail || !on {
		t.Errorf("always-on pref: avail=%v on=%v, want true,true", avail, on)
	}
	disabledAuto := map[string]store.ConnectorPref{
		store.ConnectorPrefKey(store.ConnectorKindBundled, "gamma"): {Kind: store.ConnectorKindBundled, ConnectorID: "gamma", Enabled: false, AutoEnable: true},
	}
	if avail, on, _ := bundledPrefFor(disabledAuto, info); avail || on {
		t.Errorf("disabled pref: avail=%v on=%v, want false,false", avail, on)
	}
}

// POST /conversations (used by Settings → Personas "Start patrol" and
// Orchestrator "Discuss this run") creates a conversation before any pre-chat
// POST /chat carries enabled_optional. It must seed OptionalMCPServersEnabled
// from the same default-on calculation as GET /mcp-servers so patrol and
// discussion turns do not run with optional_mcp_servers_enabled = [].
func TestCreateConversationSeedsDefaultOptionalMCPServers(t *testing.T) {
	t.Parallel()

	fs := &prefsFakeStore{
		fakeChatStore: newFakeChatStore(),
		prefs: map[string]store.ConnectorPref{
			// Explicit opt-in for a connector whose operator default is off.
			store.ConnectorPrefKey(store.ConnectorKindBundled, "beta_opt"): {
				Kind:        store.ConnectorKindBundled,
				ConnectorID: "beta_opt",
				Enabled:     true,
				AutoEnable:  true,
			},
			// Explicit disable of a connector whose operator default is on.
			store.ConnectorPrefKey(store.ConnectorKindBundled, "muted_default"): {
				Kind:        store.ConnectorKindBundled,
				ConnectorID: "muted_default",
				Enabled:     false,
			},
		},
	}
	engine := &catalogEngine{
		catalog: []agent.OptionalServerInfo{
			{Name: "Pages", EnabledByDefault: true},
			{Name: "explorer", EnabledByDefault: true},
			{Name: "muted_default", EnabledByDefault: true},
			{Name: "off_by_default", EnabledByDefault: false},
			{Name: "beta_opt", EnabledByDefault: false},
		},
	}
	srv := newDefaultChatServer(t, engine, fs)

	w := do(t, srv.Routes(), http.MethodPost, "/conversations",
		map[string]any{"title": "Morning sweep", "persona": "generic"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /conversations: %d %s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	want := []string{"beta_opt", "explorer", "pages"}
	if strings.Join(conv.OptionalMCPServersEnabled, ",") != strings.Join(want, ",") {
		t.Fatalf("response OptionalMCPServersEnabled = %v, want %v", conv.OptionalMCPServersEnabled, want)
	}
	if conv.Model != agentcore.CurrentDefaultModel() {
		t.Fatalf("response Model = %q, want %q", conv.Model, agentcore.CurrentDefaultModel())
	}
	stored, err := fs.Get(context.Background(), "u@x.com", conv.ID)
	if err != nil || stored == nil {
		t.Fatalf("store.Get(%s): %v", conv.ID, err)
	}
	if strings.Join(stored.OptionalMCPServersEnabled, ",") != strings.Join(want, ",") {
		t.Fatalf("persisted OptionalMCPServersEnabled = %v, want %v", stored.OptionalMCPServersEnabled, want)
	}
	if stored.Model != agentcore.CurrentDefaultModel() {
		t.Fatalf("persisted Model = %q, want %q", stored.Model, agentcore.CurrentDefaultModel())
	}
}

// POST /conversations with a project_id (used by startProjectChat when clicking
// "+ New chat" inside a project) must inherit the caller's default-on optional
// MCP connectors when the project itself has no curated mcp_servers override,
// while preserving explicit project mcp_servers / default_persona / default_model
// when configured.
func TestCreateProjectConversationSeedsDefaultOptionalMCPServers(t *testing.T) {
	t.Parallel()

	fs := &prefsFakeStore{
		fakeChatStore: newFakeChatStore(),
		prefs: map[string]store.ConnectorPref{
			store.ConnectorPrefKey(store.ConnectorKindBundled, "ix_pub"): {
				Kind:        store.ConnectorKindBundled,
				ConnectorID: "ix_pub",
				Enabled:     true,
				AutoEnable:  true,
			},
			store.ConnectorPrefKey(store.ConnectorKindBundled, "mg_pub"): {
				Kind:        store.ConnectorKindBundled,
				ConnectorID: "mg_pub",
				Enabled:     true,
				AutoEnable:  true,
			},
			store.ConnectorPrefKey(store.ConnectorKindBundled, "muted_default"): {
				Kind:        store.ConnectorKindBundled,
				ConnectorID: "muted_default",
				Enabled:     false,
			},
		},
		projects: map[string]*store.Project{
			"proj-default": {
				ID:         "proj-default",
				OwnerEmail: "u@x.com",
				Name:       "Untapped Optimizations",
				MCPServers: []string{},
			},
			"proj-curated": {
				ID:             "proj-curated",
				OwnerEmail:     "u@x.com",
				Name:           "Curated Space",
				DefaultPersona: "analyst",
				DefaultModel:   "anthropic/claude-sonnet-4.6",
				MCPServers:     []string{"pages"},
			},
		},
	}
	engine := &catalogEngine{
		catalog: []agent.OptionalServerInfo{
			{Name: "Pages", EnabledByDefault: true},
			{Name: "muted_default", EnabledByDefault: true},
			{Name: "ix_pub", EnabledByDefault: false},
			{Name: "mg_pub", EnabledByDefault: false},
		},
	}
	srv := newDefaultChatServer(t, engine, fs)

	// 1. Project with empty mcp_servers inherits user's default-on connectors,
	// server default persona, and current default model.
	w := do(t, srv.Routes(), http.MethodPost, "/conversations",
		map[string]any{"title": "New conversation", "project_id": "proj-default"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /conversations (proj-default): %d %s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	want := []string{"ix_pub", "mg_pub", "pages"}
	if strings.Join(conv.OptionalMCPServersEnabled, ",") != strings.Join(want, ",") {
		t.Fatalf("proj-default OptionalMCPServersEnabled = %v, want %v", conv.OptionalMCPServersEnabled, want)
	}
	if conv.Persona != "generic" {
		t.Fatalf("proj-default Persona = %q, want %q", conv.Persona, "generic")
	}
	if conv.Model != agentcore.CurrentDefaultModel() {
		t.Fatalf("proj-default Model = %q, want %q", conv.Model, agentcore.CurrentDefaultModel())
	}

	// 2. Project with explicit mcp_servers / default_persona / default_model
	// keeps those project-level defaults when the request leaves them blank.
	w = do(t, srv.Routes(), http.MethodPost, "/conversations",
		map[string]any{"title": "New conversation", "project_id": "proj-curated"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /conversations (proj-curated): %d %s", w.Code, w.Body.String())
	}
	var curated store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &curated); err != nil {
		t.Fatalf("decode curated conversation: %v", err)
	}
	if strings.Join(curated.OptionalMCPServersEnabled, ",") != "pages" {
		t.Fatalf("proj-curated OptionalMCPServersEnabled = %v, want [pages]", curated.OptionalMCPServersEnabled)
	}
	if curated.Persona != "analyst" {
		t.Fatalf("proj-curated Persona = %q, want %q", curated.Persona, "analyst")
	}
	if curated.Model != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("proj-curated Model = %q, want %q", curated.Model, "anthropic/claude-sonnet-4.6")
	}
}
