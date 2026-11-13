package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
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
		switch key {
		case "default_model":
			agentcore.SetDefaultModel(val)
		case "advanced_model":
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

	// 3. Concurrent unfiltered rollouts serialize across task + chat phases so
	// chats and tasks never finish on divergent models.
	phase1Entered := make(chan struct{})
	unblockPhase1 := make(chan struct{})
	var firstOnce sync.Once
	s.taskModelRollout = func(_ context.Context, opts models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		mu.Lock()
		if opts.Model != nil {
			liveTaskModel = *opts.Model
		}
		mu.Unlock()
		firstOnce.Do(func() {
			close(phase1Entered)
			<-unblockPhase1
		})
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
	// While rollout A is paused in its task phase, rollout B AND a non-rollout
	// tier write (rollout: false) must both block on the rollout lock.
	nonRolloutDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		do(t, h, http.MethodPut, "/admin/settings/default_model", map[string]any{
			"value":   "acme/non-rollout-tier",
			"rollout": false,
		}, "boss@x.com")
		close(nonRolloutDone)
	}()
	select {
	case <-nonRolloutDone:
		t.Errorf("non-rollout PUT default_model completed while rollout A held the rollout lock")
	default:
	}
	if cur, _ := st.Get(context.Background(), "alice@x.com", conv.ID); cur.Model == "acme/model-b" {
		t.Errorf("chat model mutated to model-b while A holds lock")
	}
	close(unblockPhase1)
	<-secondDone
	<-nonRolloutDone
	wg.Wait()

	finalConv, _ := st.Get(context.Background(), "alice@x.com", conv.ID)
	mu.Lock()
	finalTask := liveTaskModel
	mu.Unlock()
	if finalConv.Model != finalTask || finalConv.Model != "acme/model-b" {
		t.Fatalf("divergent cross-store rollout: chat=%q task=%q (want both acme/model-b)", finalConv.Model, finalTask)
	}
}

type failingRolloutChatStore struct {
	*fakeChatStore
	rolloutErr error
}

func (f *failingRolloutChatStore) RolloutConversationModel(ctx context.Context, model, fromModel string, includeLockdown, notify bool) (store.ConversationModelRolloutResult, error) {
	if f.rolloutErr != nil {
		return store.ConversationModelRolloutResult{}, f.rolloutErr
	}
	return f.fakeChatStore.RolloutConversationModel(ctx, model, fromModel, includeLockdown, notify)
}

func TestModelRollout_TaskFailureAbortsBeforeChatAndPartialError207(t *testing.T) {
	baseSt := newFakeChatStore()
	st := &failingRolloutChatStore{fakeChatStore: baseSt}
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}
	conv, _ := baseSt.CreateConversation(context.Background(), "alice@x.com", "Active Chat", "victoria", "anthropic/claude-opus-5.5", false)
	msgBytes, _ := json.Marshal(agent.TextContent{Text: "hello"})
	_, _ = baseSt.AppendHistory(context.Background(), conv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: msgBytes},
	})
	h := s.Routes()

	// 1. Scheduler failure aborts BEFORE mutating conversations or posting notices.
	s.taskModelRollout = func(_ context.Context, _ models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		return models.TaskModelRolloutResult{}, errors.New("scheduler db unavailable")
	}
	w := do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"model": "google/gemini-4-hydrogen"}, "boss@x.com")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	cur, _ := baseSt.Get(context.Background(), "alice@x.com", conv.ID)
	if cur.Model != "anthropic/claude-opus-5.5" {
		t.Fatalf("chat model mutated despite task failure: got %q", cur.Model)
	}
	hist, _ := baseSt.LoadHistory(context.Background(), conv.ID)
	if len(hist) != 1 {
		t.Fatalf("unexpected upgrade notice posted despite task failure: len=%d", len(hist))
	}

	// 2. If tasks commit updates and chat rollout subsequently fails, return 207
	// Multi-Status with committed task counts and PartialError.
	s.taskModelRollout = func(_ context.Context, _ models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		return models.TaskModelRolloutResult{ScannedCount: 3, UpdatedCount: 2, PrimaryUpdatedCount: 2}, nil
	}
	st.rolloutErr = errors.New("chat db unavailable")
	w = do(t, h, http.MethodPost, "/admin/models/rollout", map[string]any{"model": "google/gemini-4-hydrogen"}, "boss@x.com")
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207: %s", w.Code, w.Body.String())
	}
	var partial ModelRolloutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &partial); err != nil {
		t.Fatalf("decode 207 response: %v", err)
	}
	if partial.TasksUpdated != 2 || !strings.Contains(partial.PartialError, "2 scheduled task(s) updated") {
		t.Fatalf("unexpected 207 body: %+v", partial)
	}
}

func TestAdminSettingFallbackRolloutRetryPreservesOldFallbackTarget(t *testing.T) {
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

	baseSt := newFakeChatStore()
	st := &failingRolloutChatStore{fakeChatStore: baseSt}
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}
	fbConv, _ := baseSt.CreateConversation(context.Background(), "alice@x.com", "Pinned Fallback", "victoria", "anthropic/claude-opus-5.5", false)

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

	var failTasks bool
	s.taskModelRollout = func(_ context.Context, _ models.TaskModelRollout, _ bool) (models.TaskModelRolloutResult, error) {
		if failTasks {
			return models.TaskModelRolloutResult{}, errors.New("transient scheduler outage")
		}
		return models.TaskModelRolloutResult{ScannedCount: 1, UpdatedCount: 1, FallbackUpdatedCount: 1}, nil
	}
	h := s.Routes()

	// First attempt saves advanced_model = acme/opus-next, but task rollout fails.
	failTasks = true
	w := do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/opus-next",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var writeResp adminSettingWriteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &writeResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if writeResp.RolloutError == "" || writeResp.RolloutRetry == nil || writeResp.RolloutRetry.UpgradeFromFallback != "anthropic/claude-opus-5.5" {
		t.Fatalf("expected RolloutRetry with UpgradeFromFallback=anthropic/claude-opus-5.5, got %+v", writeResp)
	}

	// Retrying PUT /admin/settings/advanced_model with the same value still
	// migrates chats pinned to anthropic/claude-opus-5.5 -> acme/opus-next.
	failTasks = false
	w = do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/opus-next",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d: %s", w.Code, w.Body.String())
	}
	cur, _ := baseSt.Get(context.Background(), "alice@x.com", fbConv.ID)
	if cur.Model != "acme/opus-next" {
		t.Fatalf("expected fallback-pinned chat to migrate to acme/opus-next on retry, got %q", cur.Model)
	}
}

type failingPendingFallbackStore struct {
	*fakeChatStore
	pending    string
	failRead   bool
	failSave   bool
	failClear  bool
	saveCalls  int
	clearCalls int
}

func (f *failingPendingFallbackStore) PendingFallbackRolloutFrom(context.Context) (string, bool, error) {
	if f.failRead {
		return "", true, errors.New("db read pending fallback failed")
	}
	return f.pending, true, nil
}

func (f *failingPendingFallbackStore) SetPendingFallbackRolloutFrom(_ context.Context, from string) error {
	if from == "" {
		f.clearCalls++
		if f.failClear {
			return errors.New("db clear pending fallback failed")
		}
		f.pending = ""
		return nil
	}
	f.saveCalls++
	if f.failSave {
		return errors.New("db save pending fallback failed")
	}
	f.pending = from
	return nil
}

func TestPendingFallbackRolloutFromPropagatesStoreErrors(t *testing.T) {
	prevDefault := agentcore.CurrentDefaultModel()
	prevFallback := agentcore.CurrentFallbackModel()
	t.Cleanup(func() {
		agentcore.SetDefaultModel(prevDefault)
		agentcore.SetFallbackModel(prevFallback)
	})
	agentcore.SetDefaultModel("google/gemini-4-hydrogen")
	agentcore.SetFallbackModel("anthropic/claude-opus-5.5")

	st := &failingPendingFallbackStore{fakeChatStore: newFakeChatStore()}
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}

	svc := newFakeSettingsService()
	svc.dflt["default_model"] = "google/gemini-4-hydrogen"
	svc.dflt["advanced_model"] = "anthropic/claude-opus-5.5"
	svc.SetHook = func(key, val string) {
		if key == "advanced_model" {
			agentcore.SetFallbackModel(val)
		}
	}
	s.workspaceSettings = svc
	h := s.Routes()

	// 1) A DB error saving the pending fallback marker aborts BEFORE advanced_model mutates.
	st.failSave = true
	w := do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/opus-next",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when saving pending fallback marker fails, got %d: %s", w.Code, w.Body.String())
	}
	if got := agentcore.CurrentFallbackModel(); got != "anthropic/claude-opus-5.5" {
		t.Fatalf("advanced_model mutated despite pre-write marker failure: %q", got)
	}

	// 2) A DB error clearing the pending fallback marker after rollout surfaces in rollout_error.
	st.failSave = false
	st.failClear = true
	w = do(t, h, http.MethodPut, "/admin/settings/advanced_model", map[string]any{
		"value":   "acme/opus-next",
		"rollout": true,
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp adminSettingWriteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.RolloutError, "clear pending fallback rollout state") {
		t.Fatalf("expected RolloutError to surface clear failure, got %+v", resp)
	}
}

func TestPostChatBaseModelPreservesRolledOutConversationModel(t *testing.T) {
	st := newFakeChatStore()
	s := newDefaultChatServer(t, nil, st)
	s.cfg.AdminEmails = []string{"boss@x.com"}
	conv, _ := st.CreateConversation(context.Background(), "alice@x.com", "Open Tab", "victoria", "anthropic/claude-opus-5.5", false)

	h := s.Routes()
	// Admin rolls out Gemini 4 Hydrogen across chats while Alice still has the conversation open in a tab.
	w := do(t, h, http.MethodPost, "/admin/models/rollout", ModelRolloutRequest{
		Model: "google/gemini-4-hydrogen",
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("rollout status = %d: %s", w.Code, w.Body.String())
	}

	// Open tab submits its next message echoing the stale hydrated model as both model and base_model:
	// resolveExistingChatConversation must preserve the rolled-out model instead of reverting it.
	reqBody := chatRequest{
		ConversationID: conv.ID,
		Message:        "hello after rollout",
		Model:          "anthropic/claude-opus-5.5",
		BaseModel:      "anthropic/claude-opus-5.5",
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/chat", nil)
	resolved, unlock, ok := s.resolveExistingChatConversation(rec, httpReq, "alice@x.com", &reqBody, reqBody.Model)
	unlock()
	if !ok {
		t.Fatalf("resolveExistingChatConversation failed: %d %s", rec.Code, rec.Body.String())
	}
	if resolved.Model != "google/gemini-4-hydrogen" {
		t.Fatalf("stale tab echo reverted rolled-out model: got %q, want google/gemini-4-hydrogen", resolved.Model)
	}
	stored, _ := st.Get(context.Background(), "alice@x.com", conv.ID)
	if stored.Model != "google/gemini-4-hydrogen" {
		t.Fatalf("stored conversation model = %q, want google/gemini-4-hydrogen", stored.Model)
	}

	// An explicit user model selection omits base_model and DOES override conv.Model.
	explicitReq := chatRequest{
		ConversationID: conv.ID,
		Message:        "switch back explicitly",
		Model:          "anthropic/claude-opus-5.5",
	}
	rec2 := httptest.NewRecorder()
	resolved2, unlock2, ok2 := s.resolveExistingChatConversation(rec2, httpReq, "alice@x.com", &explicitReq, explicitReq.Model)
	unlock2()
	if !ok2 || resolved2.Model != "anthropic/claude-opus-5.5" {
		t.Fatalf("explicit model pick failed: ok=%v model=%q", ok2, resolved2.Model)
	}
}
