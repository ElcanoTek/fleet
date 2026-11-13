package admincli

import (
	"bytes"
	"context"
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
	lastModel       string
	lastFrom        string
	lastLockdown    bool
	lastNotify      bool
	settingsWritten map[string]string
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
	calls    int
	lastOpts models.TaskModelRollout
	lastDry  bool
}

func (f *fakeRolloutTaskStore) RolloutTaskModels(_ context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error) {
	f.calls++
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

	// 3. When the live server rejects or fails reload, workspace_settings is
	// rolled back and neither chats nor tasks are partially mutated.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	badModel := "openai/gpt-6-luna-pro"
	outBuf.Reset()
	errBuf.Reset()
	code = runModelRollout(context.Background(), chatSt, taskSt, &outBuf, &errBuf, modelRolloutOptions{
		model:             &badModel,
		setWorkspaceTiers: true,
		serverAddr:        failSrv.URL,
		adminKey:          "test-admin-key",
		noConfirm:         true,
	})
	if code != 5 {
		t.Fatalf("expected exit 5 on live server reload failure, got %d (stderr=%s)", code, errBuf.String())
	}
	if chatSt.settingsWritten["default_model"] != "google/gemini-4-hydrogen" {
		t.Errorf("default_model not rolled back after reload failure: got %q", chatSt.settingsWritten["default_model"])
	}
	if chatSt.rolloutCalls != 1 {
		t.Errorf("chat rollout ran despite tier reload failure: calls=%d", chatSt.rolloutCalls)
	}
}
