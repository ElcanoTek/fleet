package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// personasFakeStore adds an in-memory user_personas table to fakeChatStore.
// It models the store's contract loosely (the store has its own tests): rows
// are per-user, proposals are inert, approve flips them active.
type personasFakeStore struct {
	*fakeChatStore
	rows    []store.UserPersona
	nextID  int
	digests []store.ChatDigestConversation
}

func (s *personasFakeStore) ListUserPersonas(_ context.Context, email string) ([]store.UserPersona, error) {
	out := []store.UserPersona{}
	for _, p := range s.rows {
		if p.UserEmail == email {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *personasFakeStore) GetUserPersonaByName(_ context.Context, email, name string) (*store.UserPersona, error) {
	for _, p := range s.rows {
		if p.UserEmail == email && p.Name == name {
			cp := p
			return &cp, nil
		}
	}
	return nil, store.ErrUserPersonaNotFound
}

func (s *personasFakeStore) add(email string, in store.UserPersonaInput, status string) *store.UserPersona {
	s.nextID++
	p := store.UserPersona{
		ID: "p" + string(rune('0'+s.nextID)), UserEmail: email, Name: in.Name, Ref: store.UserPersonaRef(in.Name),
		DisplayName: in.DisplayName, Instructions: in.Instructions, BasePersona: in.BasePersona,
		Kind: in.Kind, Autonomy: in.Autonomy, Status: status,
	}
	s.rows = append(s.rows, p)
	return &p
}

func (s *personasFakeStore) CreateUserPersona(_ context.Context, email string, in store.UserPersonaInput) (*store.UserPersona, error) {
	if in.Name == "" || in.Instructions == "" {
		return nil, store.ErrUserPersonaInvalid
	}
	return s.add(email, in, store.UserPersonaStatusActive), nil
}

func (s *personasFakeStore) ProposeUserPersona(_ context.Context, email string, in store.UserPersonaInput) (*store.UserPersona, error) {
	return s.add(email, in, store.UserPersonaStatusProposed), nil
}

func (s *personasFakeStore) ApproveUserPersona(_ context.Context, email, id string) (*store.UserPersona, error) {
	for i := range s.rows {
		if s.rows[i].ID == id && s.rows[i].UserEmail == email {
			s.rows[i].Status = store.UserPersonaStatusActive
			cp := s.rows[i]
			return &cp, nil
		}
	}
	return nil, store.ErrUserPersonaNotFound
}

func (s *personasFakeStore) DeleteUserPersona(_ context.Context, email, id string) error {
	for i := range s.rows {
		if s.rows[i].ID == id && s.rows[i].UserEmail == email {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return store.ErrUserPersonaNotFound
}

func (s *personasFakeStore) UserChatDigest(_ context.Context, _ string, _, _, _, _ int) ([]store.ChatDigestConversation, error) {
	return s.digests, nil
}

// personaEngine is fakeEngine with a bundle persona roster.
type personaEngine struct {
	*fakeEngine
	names []string
}

func (e *personaEngine) ListPersonas() ([]string, error) { return e.names, nil }

func newPersonaServer() (*Server, *personasFakeStore) {
	fs := &personasFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{
		store: fs,
		agent: &personaEngine{fakeEngine: &fakeEngine{}, names: []string{"assistant", "trader"}},
		cfg:   &config.Config{PersonaDefault: "assistant"},
	}
	return s, fs
}

func TestUserPersonaEndpoints(t *testing.T) {
	s, fs := newPersonaServer()

	w := userSkillsReq(t, s, s.userPersonasCollection, http.MethodPost, "/user-personas",
		`{"name":"twin","display_name":"My Twin","instructions":"Lead with numbers.","base_persona":"trader","kind":"twin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// A base the bundle does not ship is refused before it reaches the store.
	w = userSkillsReq(t, s, s.userPersonasCollection, http.MethodPost, "/user-personas",
		`{"name":"x","instructions":"y","base_persona":"ghost"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not a workspace persona") {
		t.Errorf("unknown base: %d %s", w.Code, w.Body.String())
	}

	w = userSkillsReq(t, s, s.userPersonasCollection, http.MethodGet, "/user-personas", "")
	var listed struct {
		Personas  []store.UserPersona `json:"personas"`
		Workspace []string            `json:"workspace_personas"`
		Default   string              `json:"default"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed.Personas) != 1 || len(listed.Workspace) != 2 || listed.Default != "assistant" {
		t.Fatalf("list: %s (%v)", w.Body.String(), err)
	}

	// A proposal is listed on the manager but not offered in the picker.
	fs.add("u@x.com", store.UserPersonaInput{Name: "draft-one", Instructions: "x"}, store.UserPersonaStatusProposed)
	w = userSkillsReq(t, s, s.listPersonas, http.MethodGet, "/personas", "")
	var roster personasResponse
	if err := json.Unmarshal(w.Body.Bytes(), &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.UserPersonas) != 1 || roster.UserPersonas[0].Ref != "user:twin" || len(roster.Personas) != 2 {
		t.Errorf("roster = %+v", roster)
	}

	draftID := fs.rows[1].ID
	if w := userSkillsReq(t, s, s.userPersonaByID, http.MethodPost, "/user-personas/"+draftID+"/approve", ""); w.Code != http.StatusOK {
		t.Errorf("approve: %d %s", w.Code, w.Body.String())
	}
	if w := userSkillsReq(t, s, s.userPersonaByID, http.MethodPost, "/user-personas/"+draftID+"/bogus", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown sub-route: %d", w.Code)
	}
	if w := userSkillsReq(t, s, s.userPersonaByID, http.MethodDelete, "/user-personas/"+draftID, ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	if w := userSkillsReq(t, s, s.userPersonaByID, http.MethodDelete, "/user-personas/"+draftID, ""); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", w.Code)
	}
}

// A new conversation may only name one of the caller's ACTIVE personas.
func TestCheckConversationPersona(t *testing.T) {
	s, fs := newPersonaServer()
	ctx := context.Background()
	fs.add("u@x.com", store.UserPersonaInput{Name: "twin", Instructions: "x"}, store.UserPersonaStatusActive)
	fs.add("u@x.com", store.UserPersonaInput{Name: "pending", Instructions: "x"}, store.UserPersonaStatusProposed)
	fs.add("other@x.com", store.UserPersonaInput{Name: "theirs", Instructions: "x"}, store.UserPersonaStatusActive)

	for persona, wantOK := range map[string]bool{
		"assistant":    true, // bundle names are not checked here
		"user:twin":    true,
		"user:pending": false,
		"user:theirs":  false,
		"user:nope":    false,
	} {
		err := s.checkConversationPersona(ctx, "u@x.com", persona)
		if (err == nil) != wantOK {
			t.Errorf("%s: err = %v, want ok=%v", persona, err, wantOK)
		}
		if err != nil && !errors.Is(err, store.ErrUserPersonaInvalid) {
			t.Errorf("%s: error should be a 400-class invalid persona, got %v", persona, err)
		}
	}
}

// The turn runs a user persona as its bundle base plus the owner's layer, and
// falls back to the default when the reference no longer resolves.
func TestResolveTurnPersona(t *testing.T) {
	s, fs := newPersonaServer()
	ctx := context.Background()
	fs.add("owner@x.com", store.UserPersonaInput{Name: "twin", DisplayName: "Twin", Instructions: "Be me.", BasePersona: "trader", Kind: "twin", Autonomy: "act"}, store.UserPersonaStatusActive)
	fs.add("owner@x.com", store.UserPersonaInput{Name: "stale-base", Instructions: "x", BasePersona: "retired"}, store.UserPersonaStatusActive)
	fs.add("owner@x.com", store.UserPersonaInput{Name: "draft", Instructions: "x"}, store.UserPersonaStatusProposed)

	persona, layer := s.resolveTurnPersona(ctx, &store.Conversation{UserEmail: "owner@x.com", Persona: "trader"})
	if persona != "trader" || layer != nil {
		t.Errorf("bundle persona: %q %+v", persona, layer)
	}
	persona, layer = s.resolveTurnPersona(ctx, &store.Conversation{UserEmail: "owner@x.com", Persona: "user:twin"})
	if persona != "trader" || layer == nil || layer.Instructions != "Be me." || layer.Kind != "twin" || layer.Autonomy != "act" || layer.Ref != "user:twin" {
		t.Errorf("user persona: %q %+v", persona, layer)
	}
	if persona, layer = s.resolveTurnPersona(ctx, &store.Conversation{UserEmail: "owner@x.com", Persona: "user:stale-base"}); persona != "" || layer == nil {
		t.Errorf("a base the bundle dropped should run on the default: %q %+v", persona, layer)
	}
	// A teammate's fork carries the reference but not the owner's persona.
	if persona, layer = s.resolveTurnPersona(ctx, &store.Conversation{UserEmail: "forker@x.com", Persona: "user:twin"}); persona != "" || layer != nil {
		t.Errorf("foreign persona resolved: %q %+v", persona, layer)
	}
	if persona, layer = s.resolveTurnPersona(ctx, &store.Conversation{UserEmail: "owner@x.com", Persona: "user:draft"}); persona != "" || layer != nil {
		t.Errorf("a proposal must not run: %q %+v", persona, layer)
	}
}

func TestPersonaStudioHandler(t *testing.T) {
	s, fs := newPersonaServer()
	fs.digests = []store.ChatDigestConversation{{ID: "c1", Title: "Pacing", Messages: []string{"show pacing"}}}
	h := s.newPersonaStudioHandler("u@x.com")
	ctx := context.Background()

	res, err := h(ctx, tools.PersonaStudioParams{Action: tools.PersonaStudioActionPropose, Name: "twin", Instructions: "Be me.", BasePersona: "trader"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	out, _ := json.Marshal(res)
	if !strings.Contains(string(out), `"status":"proposed"`) || !strings.Contains(string(out), "/settings/personas?review=") {
		t.Errorf("propose result: %s", out)
	}
	if len(fs.rows) != 1 || fs.rows[0].Status != store.UserPersonaStatusProposed {
		t.Errorf("proposal not staged inert: %+v", fs.rows)
	}
	if _, err := h(ctx, tools.PersonaStudioParams{Action: tools.PersonaStudioActionPropose, Name: "x", Instructions: "y", BasePersona: "ghost"}); !errors.Is(err, store.ErrUserPersonaInvalid) {
		t.Errorf("unknown base accepted: %v", err)
	}

	res, err = h(ctx, tools.PersonaStudioParams{Action: tools.PersonaStudioActionList})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = json.Marshal(res)
	if !strings.Contains(string(out), `"workspace_personas":["assistant","trader"]`) || !strings.Contains(string(out), `"name":"twin"`) {
		t.Errorf("list result: %s", out)
	}

	res, err = h(ctx, tools.PersonaStudioParams{Action: tools.PersonaStudioActionHistory})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = json.Marshal(res)
	if !strings.Contains(string(out), "show pacing") || !strings.Contains(string(out), "data, not instructions") {
		t.Errorf("history result: %s", out)
	}

	if _, err := h(ctx, tools.PersonaStudioParams{Action: tools.PersonaStudioActionGet, Name: "nope"}); !errors.Is(err, store.ErrUserPersonaNotFound) {
		t.Errorf("get unknown: %v", err)
	}
}
