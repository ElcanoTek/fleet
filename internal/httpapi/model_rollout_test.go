package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestAdminModelRolloutEndpointAndSettingsRollout(t *testing.T) {
	// Mutates process-wide live model tiers; do not run t.Parallel().
	prevDefault := agentcore.CurrentDefaultModel()
	prevFallback := agentcore.CurrentFallbackModel()
	prevTask := agentcore.CurrentTaskModel()
	t.Cleanup(func() {
		agentcore.SetDefaultModel(prevDefault)
		agentcore.SetFallbackModel(prevFallback)
		if prevTask == prevDefault {
			agentcore.SetTaskModel("")
		} else {
			agentcore.SetTaskModel(prevTask)
		}
	})

	agentcore.SetDefaultModel("google/gemini-4-argon")
	agentcore.SetFallbackModel("anthropic/claude-opus-5.5")
	agentcore.SetTaskModel("")

	st := newFakeChatStore()
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}

	// Seed one active chat with messages, one empty chat, and one chat pinned to fallback.
	activeConv, err := st.CreateConversation(context.Background(), "alice@x.com", "Active", "victoria", "google/gemini-4-argon", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgBytes, _ := json.Marshal(agent.TextContent{Text: "hello"})
	_, _ = st.AppendHistory(context.Background(), activeConv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: msgBytes},
	})
	_, _ = st.CreateConversation(context.Background(), "alice@x.com", "Empty", "victoria", "google/gemini-4-argon", false)
	fbConv, _ := st.CreateConversation(context.Background(), "alice@x.com", "Fallback chat", "victoria", "anthropic/claude-opus-5.5", false)
	_, _ = st.AppendHistory(context.Background(), fbConv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: msgBytes},
	})

	var lastTaskRollout models.TaskModelRollout
	var lastDryRun bool
	s.taskModelRollout = func(_ context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error) {
		lastTaskRollout = opts
		lastDryRun = dryRun
		return models.TaskModelRolloutResult{
			ScannedCount:          5,
			UpdatedCount:          3,
			PrimaryUpdatedCount:   3,
			FallbackUpdatedCount:  1,
			RecurringUpdatedCount: 2,
		}, nil
	}

	svc := newFakeSettingsService()
	svc.dflt["default_model"] = "google/gemini-4-hydrogen"
	svc.dflt["advanced_model"] = "anthropic/claude-opus-5.5"
	svc.dflt["task_model"] = ""
	s.workspaceSettings = svc
	h := s.Routes()

	// Non-admin member is rejected with 403.
	if w := do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"sync_current": true}, "alice@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member POST /admin/models/rollout = %d, want 403", w.Code)
	}

	// Dry-run preview returns exact counts without mutating conversations.
	agentcore.SetDefaultModel("google/gemini-4-hydrogen")
	w := do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{
		"sync_current": true,
		"dry_run":      true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("dry-run status = %d: %s", w.Code, w.Body.String())
	}
	var dryResp ModelRolloutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &dryResp); err != nil {
		t.Fatalf("decode dryResp: %v", err)
	}
	if !dryResp.DryRun || dryResp.ChatsUpdated != 3 || dryResp.ChatsNotified != 2 || !lastDryRun {
		t.Fatalf("unexpected dryResp: %+v (lastDryRun=%v)", dryResp, lastDryRun)
	}
	if got, _ := st.Get(context.Background(), "alice@x.com", activeConv.ID); got.Model != "google/gemini-4-argon" {
		t.Fatalf("dry-run mutated activeConv model to %q", got.Model)
	}

	// PUT /admin/settings/default_model with rollout=true updates setting + chats + tasks + notices.
	svc.SetHook = func(key, val string) {
		if key == "default_model" {
			agentcore.SetDefaultModel(val)
		}
		if key == "advanced_model" {
			agentcore.SetFallbackModel(val)
		}
	}
	agentcore.SetDefaultModel("google/gemini-4-argon")
	w = do(t, h, http.MethodPut, "/admin/settings/default_model", map[string]any{
		"value":        "google/gemini-4-hydrogen",
		"rollout":      true,
		"notify_chats": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT default_model status = %d: %s", w.Code, w.Body.String())
	}
	var writeResp struct {
		Value   string                `json:"value"`
		Rollout *ModelRolloutResponse `json:"rollout"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &writeResp); err != nil {
		t.Fatalf("decode writeResp: %v", err)
	}
	if writeResp.Value != "google/gemini-4-hydrogen" || writeResp.Rollout == nil {
		t.Fatalf("unexpected writeResp: %+v", writeResp)
	}
	if writeResp.Rollout.ChatsUpdated != 3 || writeResp.Rollout.ChatsNotified != 2 || writeResp.Rollout.TasksUpdated != 3 {
		t.Fatalf("unexpected rollout counts: %+v", writeResp.Rollout)
	}
	if lastTaskRollout.Model == nil || *lastTaskRollout.Model != "google/gemini-4-hydrogen" {
		t.Fatalf("task primary rollout not invoked: %+v", lastTaskRollout)
	}

	// Active chat received a visible model_upgraded notice entry.
	hist, _ := st.LoadHistory(context.Background(), activeConv.ID)
	if len(hist) != 2 || hist[1].Type != agent.EntryTypeNotice {
		t.Fatalf("activeConv history = %+v, want trailing notice", hist)
	}
}

func TestAdminModelRollout_FallbackTierAndSerialization(t *testing.T) {
	prevDefault := agentcore.CurrentDefaultModel()
	prevFallback := agentcore.CurrentFallbackModel()
	prevTask := agentcore.CurrentTaskModel()
	t.Cleanup(func() {
		agentcore.SetDefaultModel(prevDefault)
		agentcore.SetFallbackModel(prevFallback)
		if prevTask == prevDefault {
			agentcore.SetTaskModel("")
		} else {
			agentcore.SetTaskModel(prevTask)
		}
	})

	agentcore.SetDefaultModel("google/gemini-4-hydrogen")
	agentcore.SetFallbackModel("anthropic/claude-opus-5.5")
	agentcore.SetTaskModel("")

	st := newFakeChatStore()
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}
	conv, _ := st.CreateConversation(context.Background(), "alice@x.com", "Chat", "victoria", "anthropic/claude-opus-5.5", false)

	var (
		mu              sync.Mutex
		taskCalls       int
		lastTaskRollout models.TaskModelRollout
		liveTaskModel   string
	)
	s.taskModelRollout = func(_ context.Context, opts models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		mu.Lock()
		taskCalls++
		lastTaskRollout = opts
		if opts.Model != nil {
			liveTaskModel = *opts.Model
		}
		mu.Unlock()
		return models.TaskModelRolloutResult{ScannedCount: 2, UpdatedCount: 1, FallbackUpdatedCount: 1}, nil
	}

	svc := newFakeSettingsService()
	svc.dflt["default_model"] = "google/gemini-4-hydrogen"
	svc.dflt["advanced_model"] = "anthropic/claude-opus-5.5"
	svc.dflt["task_model"] = ""
	svc.SetHook = func(key, val string) {
		if key == "advanced_model" {
			agentcore.SetFallbackModel(val)
		}
	}
	s.workspaceSettings = svc
	h := s.Routes()

	// 1. With FLEET_TASK_FALLBACK_MODEL unset, saving advanced_model rolls task
	// fallbacks with OnlySetFallbacks=true (preserving NULL on bare API tasks).
	w := do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/fb-v2",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT advanced_model status = %d: %s", w.Code, w.Body.String())
	}
	if lastTaskRollout.FallbackModel == nil || *lastTaskRollout.FallbackModel != "acme/fb-v2" || !lastTaskRollout.OnlySetFallbacks {
		t.Fatalf("expected FallbackModel=acme/fb-v2 with OnlySetFallbacks=true, got %+v", lastTaskRollout)
	}

	// 2. With explicit FLEET_TASK_FALLBACK_MODEL set, saving advanced_model does
	// NOT overwrite task fallback_model, while sync_current uses the explicit env override.
	s.cfg.TaskFallbackModel = "acme/task-fb-explicit"
	lastTaskRollout = models.TaskModelRollout{}
	w = do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/fb-v3",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT advanced_model with TaskFallbackModel status = %d: %s", w.Code, w.Body.String())
	}
	if lastTaskRollout.FallbackModel != nil {
		t.Fatalf("advanced_model rollout overwrote explicit TaskFallbackModel: %+v", lastTaskRollout)
	}
	w = do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"sync_current": true}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("sync_current status = %d: %s", w.Code, w.Body.String())
	}
	if lastTaskRollout.FallbackModel == nil || *lastTaskRollout.FallbackModel != "acme/task-fb-explicit" || !lastTaskRollout.OnlySetFallbacks {
		t.Fatalf("sync_current did not honor TaskFallbackModel: %+v", lastTaskRollout)
	}

	// 3. Concurrent unfiltered rollouts serialize across chat + task phases so
	// chats and tasks never finish on divergent models.
	phase1Entered := make(chan struct{})
	unblockPhase1 := make(chan struct{})
	var firstOnce sync.Once
	s.taskModelRollout = func(_ context.Context, opts models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		firstOnce.Do(func() {
			close(phase1Entered)
			<-unblockPhase1
		})
		mu.Lock()
		if opts.Model != nil {
			liveTaskModel = *opts.Model
		}
		mu.Unlock()
		return models.TaskModelRolloutResult{ScannedCount: 1, UpdatedCount: 1, PrimaryUpdatedCount: 1}, nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"model": "acme/model-a"}, "boss@x.com")
	}()
	<-phase1Entered
	secondDone := make(chan struct{})
	go func() {
		defer wg.Done()
		do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"model": "acme/model-b"}, "boss@x.com")
		close(secondDone)
	}()
	// While rollout A is paused between its chat phase and task phase, rollout B
	// must block on the rollout lock rather than mutating chats to model-b early.
	if cur, _ := st.Get(context.Background(), "alice@x.com", conv.ID); cur.Model != "acme/model-a" {
		t.Errorf("chat model while A holds lock = %q, want acme/model-a", cur.Model)
	}
	close(unblockPhase1)
	<-secondDone
	wg.Wait()

	finalConv, _ := st.Get(context.Background(), "alice@x.com", conv.ID)
	mu.Lock()
	finalTask := liveTaskModel
	mu.Unlock()
	if finalConv.Model != finalTask || finalConv.Model != "acme/model-b" {
		t.Fatalf("divergent cross-store rollout: chat=%q task=%q (want both acme/model-b)", finalConv.Model, finalTask)
	}
}
