package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
)

type fakeRolloutChatStore struct {
	entries         []store.ConversationModelEntry
	rolloutCalls    int
	rolloutErr      error
	lastModel       string
	lastFrom        string
	lastLockdown    bool
	lastNotify      bool
	settingsWritten map[string]string
	settingsErr     error
	lockAcquires    int
	lockReleases    int
}

func (f *fakeRolloutChatStore) AcquireModelRolloutLock(context.Context) (func(), error) {
	f.lockAcquires++
	return func() { f.lockReleases++ }, nil
}

func (f *fakeRolloutChatStore) ListConversationsForModelUpdate(_ context.Context, fromModel string, includeLockdown bool) ([]store.ConversationModelEntry, error) {
	var out []store.ConversationModelEntry
	for _, e := range f.entries {
		if !includeLockdown && e.Lockdown {
			continue
		}
		if fromModel == "" || e.Model == fromModel {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeRolloutChatStore) RolloutConversationModel(_ context.Context, model, fromModel string, includeLockdown, notify bool) (store.ConversationModelRolloutResult, error) {
	f.rolloutCalls++
	if f.rolloutErr != nil {
		return store.ConversationModelRolloutResult{}, f.rolloutErr
	}
	f.lastModel = model
	f.lastFrom = fromModel
	f.lastLockdown = includeLockdown
	f.lastNotify = notify
	var res store.ConversationModelRolloutResult
	for _, e := range f.entries {
		if !includeLockdown && e.Lockdown {
			continue
		}
		if fromModel != "" && e.Model != fromModel {
			continue
		}
		res.MatchedCount++
		if e.Model != model {
			res.UpdatedCount++
			if notify && e.HasMessages {
				res.NotifiedCount++
			}
		}
	}
	return res, nil
}

func (f *fakeRolloutChatStore) WorkspaceSettings(context.Context) (map[string]store.WorkspaceSetting, error) {
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	out := map[string]store.WorkspaceSetting{}
	for k, v := range f.settingsWritten {
		out[k] = store.WorkspaceSetting{Key: k, Value: v}
	}
	return out, nil
}

func (f *fakeRolloutChatStore) SetWorkspaceSetting(_ context.Context, key, value, updatedBy string) (store.WorkspaceSetting, error) {
	if f.settingsWritten == nil {
		f.settingsWritten = map[string]string{}
	}
	f.settingsWritten[key] = value
	return store.WorkspaceSetting{Key: key, Value: value, UpdatedBy: updatedBy}, nil
}

func (f *fakeRolloutChatStore) DeleteWorkspaceSetting(_ context.Context, key string) error {
	delete(f.settingsWritten, key)
	return nil
}

type fakeRolloutTaskStore struct {
	calls       int
	rolloutErr  error
	lastOpts    models.TaskModelRollout
	lastDry     bool
	statusTasks []*models.Task
}

func (f *fakeRolloutTaskStore) ListTasksForModelStatus(_ context.Context) ([]*models.Task, error) {
	return f.statusTasks, nil
}

func (f *fakeRolloutTaskStore) RolloutTaskModels(_ context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error) {
	f.calls++
	if f.rolloutErr != nil {
		return models.TaskModelRolloutResult{}, f.rolloutErr
	}
	f.lastOpts = opts
	f.lastDry = dryRun
	return models.TaskModelRolloutResult{
		ScannedCount:          6,
		UpdatedCount:          4,
		PrimaryUpdatedCount:   3,
		FallbackUpdatedCount:  2,
		RecurringUpdatedCount: 4,
	}, nil
}

func TestRunModelRollout_DryRunAndSyncAndWorkspaceTiers(t *testing.T) {
	var reloadCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/reload-config" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "test-admin-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reloadCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changed":[],"skipped":[],"errors":[]}`))
	}))
	defer srv.Close()

	chatSt := &fakeRolloutChatStore{
		entries: []store.ConversationModelEntry{
			{ID: "c1", UserEmail: "alice@example.com", Model: "google/gemini-4-argon", HasMessages: true},
			{ID: "c2", UserEmail: "bob@example.com", Model: "google/gemini-4-argon", HasMessages: false},
			{ID: "c3", UserEmail: "carol@example.com", Model: "google/gemini-4-hydrogen", HasMessages: true},
		},
	}
	taskSt := &fakeRolloutTaskStore{}
	modelSlug := "google/gemini-4-hydrogen"
	fallback := "anthropic/claude-opus-5.5"

	// 1. Dry-run prints exact preview counts and mutates nothing.
	var outBuf, errBuf bytes.Buffer
	code := runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		model:         &modelSlug,
		fallbackModel: &fallback,
		notify:        true,
		dryRun:        true,
	})
	if code != 0 {
		t.Fatalf("dry-run code = %d, stderr=%s", code, errBuf.String())
	}
	if chatSt.rolloutCalls != 0 {
		t.Fatalf("dry-run called RolloutConversationModel %d times", chatSt.rolloutCalls)
	}
	if !strings.Contains(errBuf.String(), "dry-run: 2 chat(s) (1 notified) and 4 task(s) (4 recurring) would be updated") {
		t.Errorf("unexpected dry-run summary: %s", errBuf.String())
	}

	// 2. Apply with --set-workspace-tiers notifies the running server via
	// POST /admin/reload-config and updates chats, tasks, notices, and settings.
	outBuf.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		model:             &modelSlug,
		fallbackModel:     &fallback,
		notify:            true,
		setWorkspaceTiers: true,
		serverAddr:        srv.URL,
		adminKey:          "test-admin-key",
		noConfirm:         true,
	})
	if code != 0 {
		t.Fatalf("apply code = %d, stderr=%s", code, errBuf.String())
	}
	if reloadCalls.Load() != 1 {
		t.Fatalf("reloadCalls = %d, want 1", reloadCalls.Load())
	}
	if chatSt.rolloutCalls != 1 || !chatSt.lastNotify || chatSt.lastModel != "google/gemini-4-hydrogen" {
		t.Fatalf("unexpected chat rollout call: %+v", chatSt)
	}
	if taskSt.lastDry || taskSt.lastOpts.Model == nil || *taskSt.lastOpts.Model != "google/gemini-4-hydrogen" {
		t.Fatalf("unexpected task rollout call: %+v", taskSt)
	}
	if chatSt.settingsWritten["default_model"] != "google/gemini-4-hydrogen" || chatSt.settingsWritten["advanced_model"] != "anthropic/claude-opus-5.5" {
		t.Errorf("workspace settings written = %+v", chatSt.settingsWritten)
	}
	if !strings.Contains(errBuf.String(), "rolled out models: 2 chat(s) updated (1 notified), 4 task(s) updated (4 recurring)") {
		t.Errorf("apply stderr = %s", errBuf.String())
	}

	// 3. When the live server applies the new setting and then fails a later hook
	// on call 1, workspace_settings is rolled back AND a compensating reload is
	// triggered on call 2 so the running server re-applies the restored tier.
	var liveServerDefault string
	var flakyCalls atomic.Int32
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := flakyCalls.Add(1)
		liveServerDefault = chatSt.settingsWritten["default_model"]
		if n == 1 {
			http.Error(w, "later hook boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changed":[],"skipped":[],"errors":[]}`))
	}))
	defer flakySrv.Close()

	badModel := "openai/gpt-6-luna-pro"
	outBuf.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		model:             &badModel,
		setWorkspaceTiers: true,
		serverAddr:        flakySrv.URL,
		adminKey:          "test-admin-key",
		noConfirm:         true,
	})
	if code != 5 {
		t.Fatalf("expected exit 5 on live server reload failure, got %d (stderr=%s)", code, errBuf.String())
	}
	if flakyCalls.Load() != 2 {
		t.Fatalf("expected 2 reload calls (initial + compensating rollback reload), got %d", flakyCalls.Load())
	}
	if chatSt.settingsWritten["default_model"] != "google/gemini-4-hydrogen" {
		t.Errorf("default_model not rolled back after reload failure: got %q", chatSt.settingsWritten["default_model"])
	}
	if liveServerDefault != "google/gemini-4-hydrogen" {
		t.Errorf("live server in-memory default_model after compensating reload = %q, want google/gemini-4-hydrogen", liveServerDefault)
	}
	if chatSt.rolloutCalls != 1 {
		t.Errorf("chat rollout ran despite tier reload failure: calls=%d", chatSt.rolloutCalls)
	}

	// 4. Sync mode (no explicit flags) aborts with exit 5 if WorkspaceSettings fails,
	// rather than silently falling back to env/compiled defaults.
	chatSt.settingsErr = errors.New("transient postgres timeout")
	outBuf.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		noConfirm: true,
	})
	if code != 5 {
		t.Fatalf("expected exit 5 when WorkspaceSettings fails, got %d (stderr=%s)", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "read workspace_settings: transient postgres timeout") {
		t.Errorf("unexpected stderr on WorkspaceSettings failure: %s", errBuf.String())
	}
	if chatSt.rolloutCalls != 1 {
		t.Errorf("chat rollout ran despite WorkspaceSettings read failure: calls=%d", chatSt.rolloutCalls)
	}
}

func TestModelStatusSplitChatTaskAndReset(t *testing.T) {
	var reloadCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/admin/reload-config" && r.Header.Get("X-API-Key") == "test-admin-key" {
			reloadCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"changed":[],"skipped":[],"errors":[]}`))
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	m1 := "google/gemini-4-argon"
	fb1 := "anthropic/claude-opus-5.5"
	chatSt := &fakeRolloutChatStore{
		entries: []store.ConversationModelEntry{
			{ID: "c1", UserEmail: "alice@example.com", Model: "google/gemini-4-argon", HasMessages: true},
			{ID: "c2", UserEmail: "bob@example.com", Model: "google/gemini-4-argon", HasMessages: true, Lockdown: true},
		},
	}
	taskSt := &fakeRolloutTaskStore{
		statusTasks: []*models.Task{
			{Prompt: "daily", Model: &m1, FallbackModel: &fb1, Recurrence: "@daily"},
		},
	}

	// 1. Split --chat-model vs --task-model with --json output.
	chatSlug := "google/gemini-4-hydrogen"
	taskSlug := "anthropic/claude-sonnet-4-6"
	var outBuf, errBuf bytes.Buffer
	code := runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		chatModel:         &chatSlug,
		taskModel:         &taskSlug,
		fallbackModel:     &fb1,
		notify:            true,
		setWorkspaceTiers: true,
		serverAddr:        srv.URL,
		adminKey:          "test-admin-key",
		noConfirm:         true,
		asJSON:            true,
	})
	if code != 0 {
		t.Fatalf("split set code = %d, stderr=%s", code, errBuf.String())
	}
	var jout modelRolloutJSONOutput
	if err := json.Unmarshal(outBuf.Bytes(), &jout); err != nil {
		t.Fatalf("decode JSON output: %v\nraw=%s", err, outBuf.String())
	}
	if jout.ChatTargetModel != chatSlug || jout.TaskTargetModel != taskSlug || jout.ChatsUpdated != 2 || jout.ChatsNotified != 2 {
		t.Fatalf("unexpected split JSON output: %+v", jout)
	}
	if chatSt.settingsWritten["default_model"] != chatSlug || chatSt.settingsWritten["task_model"] != taskSlug {
		t.Fatalf("expected split workspace_settings tiers, got %+v", chatSt.settingsWritten)
	}

	// 2. fleet model status --json reflects workspace_settings provenance + counts.
	outBuf.Reset()
	errBuf.Reset()
	if code := runModelStatus(context.Background(), chatSt, taskSt, &outBuf, &errBuf, true); code != 0 {
		t.Fatalf("runModelStatus code = %d, stderr=%s", code, errBuf.String())
	}
	var statusRep modelStatusReport
	if err := json.Unmarshal(outBuf.Bytes(), &statusRep); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if statusRep.Tiers.DefaultModel.Value != chatSlug || statusRep.Tiers.DefaultModel.Source != "workspace_settings" {
		t.Errorf("unexpected default_model status: %+v", statusRep.Tiers.DefaultModel)
	}
	if statusRep.Tiers.TaskModel.Value != taskSlug || statusRep.Tiers.TaskModel.Source != "workspace_settings" {
		t.Errorf("unexpected task_model status: %+v", statusRep.Tiers.TaskModel)
	}
	if statusRep.Conversations.Total != 2 || statusRep.Tasks.Total != 1 || statusRep.Tasks.Recurring != 1 {
		t.Errorf("unexpected status counts: convs=%+v tasks=%+v", statusRep.Conversations, statusRep.Tasks)
	}

	// 3. fleet model reset --tier task clears task_model override, reloads server,
	// and syncs tasks back to default_model without touching chats.
	prevChatCalls := chatSt.rolloutCalls
	outBuf.Reset()
	errBuf.Reset()
	code = runModelReset(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelResetOptions{
		tier:       "task",
		rollout:    true,
		notify:     true,
		serverAddr: srv.URL,
		adminKey:   "test-admin-key",
		noConfirm:  true,
		asJSON:     true,
	})
	if code != 0 {
		t.Fatalf("reset --tier task code = %d, stderr=%s", code, errBuf.String())
	}
	if _, exists := chatSt.settingsWritten["task_model"]; exists {
		t.Errorf("expected task_model deleted from workspace_settings, got %+v", chatSt.settingsWritten)
	}
	if chatSt.rolloutCalls != prevChatCalls {
		t.Errorf("reset --tier task should not touch chats: calls=%d want %d", chatSt.rolloutCalls, prevChatCalls)
	}
	if taskSt.lastOpts.Model == nil || *taskSt.lastOpts.Model != chatSlug {
		t.Errorf("expected task reset rollout target %q, got %+v", chatSlug, taskSt.lastOpts.Model)
	}
}

func TestConfirmAndLockRolloutPlan_RepromptsOnConcurrentTierChange(t *testing.T) {
	t.Parallel()

	chatSt := &fakeRolloutChatStore{
		settingsWritten: map[string]string{"default_model": "google/gemini-4-argon"},
	}
	opts := modelRolloutOptions{chats: true, tasks: true, notify: true}
	opts.normalizeDefaults()
	var prompts []string
	confirm := func(noConfirm bool, warning string) int {
		if noConfirm {
			t.Fatalf("expected interactive prompt (noConfirm=false)")
		}
		prompts = append(prompts, warning)
		if len(prompts) == 1 {
			// Simulate a concurrent operator updating default_model while stdin waits.
			chatSt.settingsWritten["default_model"] = "google/gemini-4-hydrogen"
		}
		return 0
	}

	var errBuf bytes.Buffer
	plan, release, code := confirmAndLockRolloutPlan(context.Background(), chatSt, &errBuf, false, confirm, func() (modelRolloutPlan, int) {
		return resolveModelRolloutPlan(context.Background(), chatSt, &errBuf, opts)
	})
	if code != 0 {
		t.Fatalf("confirmAndLockRolloutPlan exit=%d stderr=%s", code, errBuf.String())
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d, want 2: %v", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0], "google/gemini-4-argon") || !strings.Contains(prompts[1], "google/gemini-4-hydrogen") {
		t.Fatalf("expected initial and updated prompts, got %v", prompts)
	}
	if chatSt.lockAcquires != 2 || chatSt.lockReleases != 1 {
		t.Fatalf("expected lock release before second prompt and re-acquire: acquires=%d releases=%d", chatSt.lockAcquires, chatSt.lockReleases)
	}
	if plan.chatTarget != "google/gemini-4-hydrogen" {
		t.Fatalf("plan.chatTarget = %q, want google/gemini-4-hydrogen", plan.chatTarget)
	}
	release()
	if chatSt.lockReleases != 2 {
		t.Fatalf("final lockReleases = %d, want 2", chatSt.lockReleases)
	}
}

func TestRunModelRollout_TaskFirstAndPartialError(t *testing.T) {
	t.Parallel()

	// 1. Task rollout failure aborts before mutating chats.
	chatSt := &fakeRolloutChatStore{
		entries: []store.ConversationModelEntry{{ID: "c1", Model: "anthropic/claude-opus-5.5", HasMessages: true}},
	}
	taskSt := &fakeRolloutTaskStore{rolloutErr: errors.New("sched db down")}
	var out, errBuf bytes.Buffer
	targetSlug := "google/gemini-4-hydrogen"
	code := runModelRollout(context.Background(), chatSt, taskSt, &out, &errBuf, modelRolloutOptions{
		model:     &targetSlug,
		chats:     true,
		tasks:     true,
		notify:    true,
		noConfirm: true,
	})
	if code != 5 {
		t.Fatalf("exit=%d, want 5", code)
	}
	if chatSt.rolloutCalls != 0 {
		t.Fatalf("chat rollout ran despite task failure: rolloutCalls=%d", chatSt.rolloutCalls)
	}

	// 2. Tasks succeed and chat rollout fails -> partial counts emitted in --json and stderr.
	taskSt.rolloutErr = nil
	chatSt.rolloutErr = errors.New("chat db down")
	out.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &out, &errBuf, modelRolloutOptions{
		model:     &targetSlug,
		chats:     true,
		tasks:     true,
		notify:    true,
		noConfirm: true,
		asJSON:    true,
	})
	if code != 5 {
		t.Fatalf("exit=%d, want 5", code)
	}
	var jout modelRolloutJSONOutput
	if err := json.Unmarshal(out.Bytes(), &jout); err != nil {
		t.Fatalf("unmarshal partial json: %v (raw=%s)", err, out.String())
	}
	if jout.TasksUpdated != 4 || !strings.Contains(jout.PartialError, "4 task(s) updated") {
		t.Fatalf("unexpected partial json: %+v", jout)
	}

	// 3. `fleet model set --offline` commits workspace_settings and then task
	// rollout fails -> partial JSON reports tier_updates + PartialError.
	taskSt.rolloutErr = errors.New("sched db down after tier write")
	chatSt.rolloutErr = nil
	out.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &out, &errBuf, modelRolloutOptions{
		model:             &targetSlug,
		setWorkspaceTiers: true,
		rollout:           true,
		offline:           true,
		chats:             true,
		tasks:             true,
		notify:            true,
		noConfirm:         true,
		asJSON:            true,
	})
	if code != 5 {
		t.Fatalf("exit=%d, want 5", code)
	}
	jout = modelRolloutJSONOutput{}
	if err := json.Unmarshal(out.Bytes(), &jout); err != nil {
		t.Fatalf("unmarshal tier-committed partial json: %v (raw=%s)", err, out.String())
	}
	if jout.WorkspaceTiersUpdated["default_model"] != targetSlug || !strings.Contains(jout.PartialError, "workspace model tier(s) updated before task rollout failed") {
		t.Fatalf("unexpected tier-committed partial json: %+v", jout)
	}
}

func TestRunModelRollout_ChatOnlySetPreservesEnvTaskModelOwnership(t *testing.T) {
	t.Setenv("FLEET_TASK_MODEL", "acme/env-task-model")

	chatSt := &fakeRolloutChatStore{}
	taskSt := &fakeRolloutTaskStore{}
	chatSlug := "google/gemini-4-hydrogen"
	var out, errBuf bytes.Buffer
	code := runModelRollout(context.Background(), chatSt, taskSt, &out, &errBuf, modelRolloutOptions{
		chatModel:         &chatSlug,
		setWorkspaceTiers: true,
		rollout:           true,
		offline:           true,
		chats:             true,
		tasksSet:          true,
		tasks:             false,
		notify:            true,
		noConfirm:         true,
	})
	if code != 0 {
		t.Fatalf("chat-only set exit=%d stderr=%s", code, errBuf.String())
	}
	if got := chatSt.settingsWritten["default_model"]; got != chatSlug {
		t.Fatalf("default_model = %q, want %q", got, chatSlug)
	}
	if _, pinned := chatSt.settingsWritten["task_model"]; pinned {
		t.Fatalf("chat-only set unexpectedly persisted FLEET_TASK_MODEL into workspace_settings: %v", chatSt.settingsWritten)
	}
}
