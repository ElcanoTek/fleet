package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
)

const modelUsageText = `Usage:
  fleet model status  [--json] [--database-url <dsn>]
  fleet model set     [--model <slug>] [--chat-model <slug>] [--task-model <slug>] [--fallback-model <slug>]
                      [--chats=true] [--tasks=true] [--notify=true] [--rollout=true] [--recurring-only]
                      [--from-model <slug>] [--from-fallback-model <slug>] [--offline] [--dry-run] [--yes] [--json]
  fleet model rollout [--model <slug>] [--chat-model <slug>] [--task-model <slug>] [--fallback-model <slug>]
                      [--chats=true] [--tasks=true] [--notify=true] [--set-workspace-tiers] [--recurring-only]
                      [--from-model <slug>] [--from-fallback-model <slug>] [--offline] [--dry-run] [--yes] [--json]
  fleet model reset   [--tier all|default|task|fallback] [--rollout=true] [--notify=true] [--offline] [--dry-run] [--yes] [--json]

Examples (SSH operator / agent workflows):
  # Inspect effective box tiers + live chat and scheduled/recurring task counts:
  fleet model status
  fleet model status --json

  # Upgrade primary + fallback everywhere (persists workspace_settings, hot-reloads server,
  # updates all chats + recurring/scheduled tasks, and posts upgrade notice in active chats):
  fleet model set --model google/gemini-4-hydrogen --fallback-model anthropic/claude-opus-5.5 --yes

  # Set DIFFERENT models for live chats vs scheduled/recurring tasks:
  fleet model set --chat-model google/gemini-4-hydrogen --task-model anthropic/claude-sonnet-4-6 --yes

  # Update ONLY live chats (or ONLY recurring tasks), silently without chat banners:
  fleet model set --chat-model google/gemini-4-hydrogen --tasks=false --notify=false --yes
  fleet model set --task-model google/gemini-4-hydrogen --chats=false --recurring-only --yes

  # Sync existing chats + tasks to the box's effective tiers (e.g. after fleet update):
  fleet model rollout --yes

  # Revert workspace_settings overrides back to env/compiled defaults and sync chats + tasks:
  fleet model reset --yes
`

type modelStatusTaskStore interface {
	ListTasksForModelStatus(ctx context.Context) ([]*models.Task, error)
}

type tierInfo struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type chatModelBucket struct {
	Model        string `json:"model"`
	Count        int    `json:"count"`
	WithMessages int    `json:"with_messages"`
	Lockdown     int    `json:"lockdown"`
}

type taskModelBucket struct {
	Model         string `json:"model"`
	FallbackModel string `json:"fallback_model"`
	Count         int    `json:"count"`
	Recurring     int    `json:"recurring"`
}

type modelStatusReport struct {
	Tiers struct {
		DefaultModel      tierInfo `json:"default_model"`
		TaskModel         tierInfo `json:"task_model"`
		FallbackModel     tierInfo `json:"fallback_model"`
		TaskFallbackModel tierInfo `json:"task_fallback_model"`
	} `json:"tiers"`
	Conversations struct {
		Total        int               `json:"total"`
		WithMessages int               `json:"with_messages"`
		Lockdown     int               `json:"lockdown"`
		ByModel      []chatModelBucket `json:"by_model"`
	} `json:"conversations"`
	Tasks struct {
		Total     int               `json:"total"`
		Recurring int               `json:"recurring"`
		ByModel   []taskModelBucket `json:"by_model"`
	} `json:"tasks"`
}

func resolveBoxTierDetails(ctx context.Context, chatSt any, ignoreKeys map[string]bool) (defTier, taskTier, fbTier tierInfo, err error) {
	defTier = tierInfo{Value: agentcore.DefaultCoreModel, Source: "default"}
	if v := strings.TrimSpace(envOrFile("FLEET_DEFAULT_MODEL")); v != "" {
		defTier = tierInfo{Value: v, Source: "env"}
	}
	fbTier = tierInfo{Value: agentcore.DefaultFallbackModel, Source: "default"}
	if v := strings.TrimSpace(envOrFile("FLEET_ADVANCED_MODEL")); v != "" {
		fbTier = tierInfo{Value: v, Source: "env"}
	}
	taskOverride := tierInfo{}
	if v := strings.TrimSpace(envOrFile("FLEET_TASK_MODEL")); v != "" {
		taskOverride = tierInfo{Value: v, Source: "env"}
	}
	if r, ok := chatSt.(chatWorkspaceSettingsReader); ok {
		ws, werr := r.WorkspaceSettings(ctx)
		if werr != nil {
			return tierInfo{}, tierInfo{}, tierInfo{}, werr
		}
		if !ignoreKeys["default_model"] {
			if v, ok := ws["default_model"]; ok && strings.TrimSpace(v.Value) != "" {
				defTier = tierInfo{Value: strings.TrimSpace(v.Value), Source: "workspace_settings"}
			}
		}
		if !ignoreKeys["advanced_model"] {
			if v, ok := ws["advanced_model"]; ok && strings.TrimSpace(v.Value) != "" {
				fbTier = tierInfo{Value: strings.TrimSpace(v.Value), Source: "workspace_settings"}
			}
		}
		if !ignoreKeys["task_model"] {
			if v, ok := ws["task_model"]; ok && strings.TrimSpace(v.Value) != "" {
				taskOverride = tierInfo{Value: strings.TrimSpace(v.Value), Source: "workspace_settings"}
			}
		}
	}
	taskTier = tierInfo{Value: defTier.Value, Source: "follows_default"}
	if taskOverride.Value != "" {
		taskTier = taskOverride
	}
	return defTier, taskTier, fbTier, nil
}

func modelStatus(argv []string) int {
	fs := flag.NewFlagSet("model status", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN (default: DATABASE_URL)")
	asJSON := fs.Bool("json", false, "emit structured JSON report")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected positional argument %q", fs.Arg(0))
	}
	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	chatSt, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer chatSt.Close()

	schedSt, code := openSchedStorage(*dbURL)
	if schedSt == nil {
		return code
	}
	defer schedSt.Close()

	return runModelStatus(context.Background(), chatSt, schedSt, os.Stdout, os.Stderr, *asJSON)
}

func runModelStatus(ctx context.Context, chatSt modelRolloutChatStore, taskSt any, outW, errW io.Writer, asJSON bool) int {
	defTier, taskTier, fbTier, err := resolveBoxTierDetails(ctx, chatSt, nil)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return 5
	}
	var rep modelStatusReport
	rep.Tiers.DefaultModel = defTier
	rep.Tiers.TaskModel = taskTier
	rep.Tiers.FallbackModel = fbTier
	rep.Tiers.TaskFallbackModel = tierInfo{Value: fbTier.Value, Source: "follows_fallback"}
	if tfb := configuredTaskFallbackEnv(); tfb != "" {
		rep.Tiers.TaskFallbackModel = tierInfo{Value: tfb, Source: "env"}
	}

	if chatSt != nil {
		convs, cerr := chatSt.ListConversationsForModelUpdate(ctx, "", true)
		if cerr != nil {
			fmt.Fprintf(errW, "list conversations: %v\n", cerr)
			return 5
		}
		rep.Conversations.Total, rep.Conversations.WithMessages, rep.Conversations.Lockdown, rep.Conversations.ByModel = summarizeChatModels(convs)
	} else {
		rep.Conversations.ByModel = []chatModelBucket{}
	}

	if ts, ok := taskSt.(modelStatusTaskStore); ok && ts != nil {
		tasks, terr := ts.ListTasksForModelStatus(ctx)
		if terr != nil {
			fmt.Fprintf(errW, "list tasks: %v\n", terr)
			return 5
		}
		rep.Tasks.Total, rep.Tasks.Recurring, rep.Tasks.ByModel = summarizeTaskModels(tasks)
	} else {
		rep.Tasks.ByModel = []taskModelBucket{}
	}

	if asJSON {
		enc := json.NewEncoder(outW)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	printModelStatusText(outW, rep)
	return 0
}

func summarizeChatModels(convs []store.ConversationModelEntry) (total, withMsgs, lockdown int, buckets []chatModelBucket) {
	byM := map[string]*chatModelBucket{}
	for _, c := range convs {
		total++
		m := strings.TrimSpace(c.Model)
		if m == "" {
			m = "<default>"
		}
		b := byM[m]
		if b == nil {
			b = &chatModelBucket{Model: m}
			byM[m] = b
		}
		b.Count++
		if c.HasMessages {
			withMsgs++
			b.WithMessages++
		}
		if c.Lockdown {
			lockdown++
			b.Lockdown++
		}
	}
	buckets = make([]chatModelBucket, 0, len(byM))
	for _, b := range byM {
		buckets = append(buckets, *b)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		return buckets[i].Model < buckets[j].Model
	})
	return total, withMsgs, lockdown, buckets
}

func summarizeTaskModels(tasks []*models.Task) (total, recurring int, buckets []taskModelBucket) {
	type key struct{ m, fb string }
	byK := map[key]*taskModelBucket{}
	for _, t := range tasks {
		total++
		m := optModelStr(t.Model)
		fb := optModelStr(t.FallbackModel)
		k := key{m: m, fb: fb}
		b := byK[k]
		if b == nil {
			b = &taskModelBucket{Model: m, FallbackModel: fb}
			byK[k] = b
		}
		b.Count++
		if strings.TrimSpace(t.Recurrence) != "" {
			recurring++
			b.Recurring++
		}
	}
	buckets = make([]taskModelBucket, 0, len(byK))
	for _, b := range byK {
		buckets = append(buckets, *b)
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		if buckets[i].Model != buckets[j].Model {
			return buckets[i].Model < buckets[j].Model
		}
		return buckets[i].FallbackModel < buckets[j].FallbackModel
	})
	return total, recurring, buckets
}

func printModelStatusText(w io.Writer, rep modelStatusReport) {
	fmt.Fprintln(w, "Workspace model tiers:")
	fmt.Fprintf(w, "  default_model (chats):    %s (%s)\n", rep.Tiers.DefaultModel.Value, rep.Tiers.DefaultModel.Source)
	fmt.Fprintf(w, "  task_model    (tasks):    %s (%s)\n", rep.Tiers.TaskModel.Value, rep.Tiers.TaskModel.Source)
	fmt.Fprintf(w, "  fallback_model:           %s (%s)\n", rep.Tiers.FallbackModel.Value, rep.Tiers.FallbackModel.Source)
	fmt.Fprintf(w, "  task_fallback_model:      %s (%s)\n", rep.Tiers.TaskFallbackModel.Value, rep.Tiers.TaskFallbackModel.Source)

	fmt.Fprintf(w, "\nConversations (%d total, %d with messages, %d lockdown):\n",
		rep.Conversations.Total, rep.Conversations.WithMessages, rep.Conversations.Lockdown)
	if len(rep.Conversations.ByModel) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, b := range rep.Conversations.ByModel {
		fmt.Fprintf(w, "  %-36s %4d  (%d with messages, %d lockdown)\n", b.Model, b.Count, b.WithMessages, b.Lockdown)
	}

	fmt.Fprintf(w, "\nUpcoming & recurring tasks (%d total, %d recurring):\n", rep.Tasks.Total, rep.Tasks.Recurring)
	if len(rep.Tasks.ByModel) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, b := range rep.Tasks.ByModel {
		fmt.Fprintf(w, "  %-36s fallback=%-32s %4d  (%d recurring)\n", b.Model, b.FallbackModel, b.Count, b.Recurring)
	}
}

func optModelStr(s *string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return "<none>"
	}
	return strings.TrimSpace(*s)
}
