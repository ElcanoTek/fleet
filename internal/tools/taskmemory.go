package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/google/uuid"
)

// Persistent task-scoped memory tools (#198): remember / recall. Registered only
// for SCHEDULED runs of a task that opted into Captain's Log
// (instruction_self_improve). Unlike propose_memory (which stages a proposal for
// a human to confirm in interactive chat), these commit immediately — scheduled
// runs are unattended, and the data is task-scoped runtime STATE in the
// scheduler DB, not the operator-owned config bundle, so a direct write is
// reproducibility-safe.

// TaskMemory is one (key, value) fact, mirroring the sched store's row shape so
// the tools layer needs no dependency on the sched package.
type TaskMemory struct {
	Key   string
	Value string
}

// TaskMemoryStore is the narrow seam the remember/recall tools depend on. The
// production implementation adapts the sched store; tests use a fake.
type TaskMemoryStore interface {
	// UpsertTaskMemory writes a fact for the task. maxKeys (>0) bounds the number
	// of keys (LRU eviction); maxValueBytes (>0) hard-rejects an oversized value.
	UpsertTaskMemory(ctx context.Context, taskID uuid.UUID, key, value string, maxKeys, maxValueBytes int) error
	// GetTaskMemory returns one fact's value, or an error if absent.
	GetTaskMemory(ctx context.Context, taskID uuid.UUID, key string) (string, error)
	// ListTaskMemories returns all facts for the task, ordered oldest-updated first.
	ListTaskMemories(ctx context.Context, taskID uuid.UUID) ([]TaskMemory, error)
}

// TaskMemoryConfig caps the memory a single task may accumulate. Zero values
// disable the respective cap.
type TaskMemoryConfig struct {
	MaxKeys       int
	MaxValueBytes int
}

// RememberParams are the typed parameters for the remember tool.
type RememberParams struct {
	Key   string `json:"key" description:"Short snake_case identifier for this fact (e.g. \"last_seen_price\"), max 128 chars. Reusing a key overwrites its value."`
	Value string `json:"value" description:"The fact to remember. Free-form text, JSON, or markdown."`
}

// NewRememberTool builds the remember tool bound to a task. The write is
// committed immediately (no human approval — scheduled runs are unattended).
func NewRememberTool(store TaskMemoryStore, taskID uuid.UUID, cfg TaskMemoryConfig) fantasy.AgentTool {
	description := `Save a fact to this task's persistent memory so future runs of the SAME scheduled task can recall it.

Use this to carry state across runs — e.g. "last_seen_price", a list of anomalies already triaged, a running digest of items already processed. The fact is stored immediately and reloaded at the start of every future run of this task.

- key: a short, stable snake_case identifier. Reusing a key OVERWRITES its value.
- value: whatever you need to remember (text / JSON / markdown).
- Do NOT store secrets or credentials.`

	return fantasy.NewAgentTool("remember", description,
		func(ctx context.Context, p RememberParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if p.Key == "" {
				return fantasy.NewTextErrorResponse("remember requires a non-empty key."), nil
			}
			if err := store.UpsertTaskMemory(ctx, taskID, p.Key, p.Value, cfg.MaxKeys, cfg.MaxValueBytes); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Remembered %q.", p.Key)), nil
		})
}

// RecallParams are the typed parameters for the recall tool.
type RecallParams struct {
	Key string `json:"key,omitempty" description:"If provided, return only this key's value. If omitted, return ALL stored facts as a JSON object."`
}

// NewRecallTool builds the recall tool bound to a task.
func NewRecallTool(store TaskMemoryStore, taskID uuid.UUID) fantasy.AgentTool {
	description := `Read back this task's persistent memory saved by earlier runs via the remember tool.

Memories are also injected into your system prompt at run start, so you usually do not need to call this — use it to re-read a value mid-run or to fetch one specific key.

- key: return just that key's value. Omit to return every stored fact as a JSON object.`

	return fantasy.NewAgentTool("recall", description,
		func(ctx context.Context, p RecallParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if p.Key != "" {
				val, err := store.GetTaskMemory(ctx, taskID, p.Key)
				if err != nil {
					// A missing key is a normal result for the model, not a tool error —
					// report it as plain content so the agent can proceed.
					return fantasy.NewTextResponse(fmt.Sprintf("(no memory stored for key %q)", p.Key)), nil //nolint:nilerr // missing key is a non-error result
				}
				return fantasy.NewTextResponse(val), nil
			}
			mems, err := store.ListTaskMemories(ctx, taskID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			obj := make(map[string]string, len(mems))
			for _, m := range mems {
				obj[m.Key] = m.Value
			}
			b, err := json.Marshal(obj)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(string(b)), nil
		})
}
