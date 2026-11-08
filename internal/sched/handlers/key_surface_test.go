package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Every orchestrator edge that authenticates a key asks
// apikeys.KeyType.AllowsSurface (via ValidateKeyOn / Lookup*On), so a key
// type is admitted exactly where the table says — the user key nowhere
// here. Each row is one key type, each column one edge.
func TestKeyTypesAcrossOrchestratorEdges(t *testing.T) {
	t.Parallel()
	store, keyMgr, _, h := setupA2AHandlers(t, false)
	var err error
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks", h.ListTasks)
	})
	r.Post("/tasks", h.CreateTask)
	r.Post("/upload", h.HandleUpload)
	r.Post("/triggers/{slug}", h.HandleWebhookTrigger)
	r.Post("/a2a", h.A2ARPC)

	template := models.NewTask(models.TaskCreate{Prompt: "base", TriggerType: models.TriggerTypeWebhook})
	if _, err := store.AddTask(template); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTrigger(context.Background(), &models.TaskTrigger{
		ID: uuid.New(), TaskID: template.ID, Slug: "hook", Secret: "webhook-secret-at-least-32-bytes-long!",
		PromptTemplate: "x",
	}); err != nil {
		t.Fatal(err)
	}

	keys := map[apikeys.KeyType]string{}
	for _, kt := range []apikeys.KeyType{apikeys.KeyTypeAdmin, apikeys.KeyTypeTask, apikeys.KeyTypeReadonly, apikeys.KeyTypeWebhook} {
		keys[kt] = mustCreateTypedKey(t, keyMgr, kt, []string{"hook"})
	}
	_, keys[apikeys.KeyTypeUser], err = keyMgr.CreateUserKey("cli", "u@x.com", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// recognised reports whether the edge accepted the key as a credential:
	// anything but the 401 an unknown key gets (a 403 is a recognised key
	// refused by its scope).
	send := func(method, path, key, body string) int {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-API-Key", key)
		req.Header.Set("Content-Type", "application/json")
		if path == "/a2a" {
			req.Header.Set("A2A-Version", "1.0")
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code
	}
	edges := []struct {
		name    string
		surface apikeys.Surface
		code    func(key string) int
	}{
		{"GET /tasks (scoped group)", apikeys.SurfaceOrchestrator, func(k string) int { return send("GET", "/tasks", k, "") }},
		{"POST /tasks (task create)", apikeys.SurfaceOrchestrator, func(k string) int { return send("POST", "/tasks", k, "{}") }},
		{"POST /upload", apikeys.SurfaceOrchestrator, func(k string) int { return send("POST", "/upload", k, "") }},
		{"POST /a2a", apikeys.SurfaceA2A, func(k string) int {
			return send("POST", "/a2a", k, `{"jsonrpc":"2.0","id":1,"method":"GetExtendedAgentCard","params":{}}`)
		}},
		{"POST /triggers/{slug}", apikeys.SurfaceTrigger, func(k string) int { return send("POST", "/triggers/hook", k, `{}`) }},
	}
	for kt, key := range keys {
		for _, e := range edges {
			code := e.code(key)
			if recognised := code != http.StatusUnauthorized; recognised != kt.AllowsSurface(e.surface) {
				t.Errorf("%s key on %s: %d (recognised=%v), want recognised=%v", kt, e.name, code, recognised, kt.AllowsSurface(e.surface))
			}
		}
	}
	// The user key's A2A refusal is the transport 401, not a card.
	if code := edges[3].code(keys[apikeys.KeyTypeUser]); code != http.StatusUnauthorized {
		t.Fatalf("user key on /a2a: %d", code)
	}
}
