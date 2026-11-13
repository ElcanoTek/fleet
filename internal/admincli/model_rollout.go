package admincli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/settings"
	"github.com/ElcanoTek/fleet/internal/store"
)

func cmdModel(argv []string) int {
	if len(argv) == 0 {
		return errf(1, "usage: fleet model rollout [--model <slug>] [--fallback-model <slug>] [--from-model <slug>] [--from-fallback-model <slug>] [--notify=true] [--recurring-only] [--set-workspace-tiers [--server <addr>] [--admin-key <key>] [--offline]] [--dry-run] [--no-confirm]")
	}
	switch argv[0] {
	case "rollout", "upgrade":
		return modelRollout(argv[1:])
	default:
		return errf(1, "unknown model subcommand %q (want rollout)", argv[0])
	}
}

type modelRolloutChatStore interface {
	ListConversationsForModelUpdate(ctx context.Context, fromModel string, includeLockdown bool) ([]store.ConversationModelEntry, error)
	RolloutConversationModel(ctx context.Context, model, fromModel string, includeLockdown, notify bool) (store.ConversationModelRolloutResult, error)
}

type modelRolloutWorkspaceStore interface {
	WorkspaceSettings(ctx context.Context) (map[string]store.WorkspaceSetting, error)
	SetWorkspaceSetting(ctx context.Context, key, value, updatedBy string) (store.WorkspaceSetting, error)
	DeleteWorkspaceSetting(ctx context.Context, key string) error
}

type modelRolloutTaskStore interface {
	RolloutTaskModels(ctx context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error)
}

func resolveEffectiveBoxTiers(ctx context.Context, chatSt any) (defaultModel, taskModel, fallbackModel string, err error) {
	defaultModel = agentcore.DefaultCoreModel
	if v := strings.TrimSpace(envOrFile("FLEET_DEFAULT_MODEL")); v != "" {
		defaultModel = v
	}
	fallbackModel = agentcore.DefaultFallbackModel
	if v := strings.TrimSpace(envOrFile("FLEET_ADVANCED_MODEL")); v != "" {
		fallbackModel = v
	}
	taskOverride := strings.TrimSpace(envOrFile("FLEET_TASK_MODEL"))
	if r, ok := chatSt.(chatWorkspaceSettingsReader); ok {
		ws, err := r.WorkspaceSettings(ctx)
		if err != nil {
			return "", "", "", err
		}
		if v, ok := ws["default_model"]; ok && strings.TrimSpace(v.Value) != "" {
			defaultModel = strings.TrimSpace(v.Value)
		}
		if v, ok := ws["advanced_model"]; ok && strings.TrimSpace(v.Value) != "" {
			fallbackModel = strings.TrimSpace(v.Value)
		}
		if v, ok := ws["task_model"]; ok && strings.TrimSpace(v.Value) != "" {
			taskOverride = strings.TrimSpace(v.Value)
		}
	}
	taskModel = defaultModel
	if taskOverride != "" {
		taskModel = taskOverride
	}
	return defaultModel, taskModel, fallbackModel, nil
}

func validateCLIModelSlug(flagName, raw string) (string, error) {
	v, err := settings.Validate(settings.Spec{Key: flagName, Kind: settings.KindModel}, raw)
	if err != nil || len(v) > 200 {
		return "", fmt.Errorf("--%s: want a model slug like provider/model, got %q", flagName, raw)
	}
	return v, nil
}

// modelRollout implements `fleet model rollout`: one-shot upgrade across chats
// (with optional upgrade notices) and scheduled/recurring tasks.
func modelRollout(argv []string) int {
	fs := flag.NewFlagSet("model rollout", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN (default: DATABASE_URL)")
	model := fs.String("model", "", "primary model slug to roll out (omit with --fallback-model omitted to sync to the box's effective tiers)")
	fallback := fs.String("fallback-model", "", "fallback model slug to roll out ('' clears task fallback_model to NULL)")
	fromModel := fs.String("from-model", "", "only update chats and task primary models currently pinned to this slug")
	fromFallback := fs.String("from-fallback-model", "", "only update task fallback_model currently pinned to this slug")
	notify := fs.Bool("notify", true, "insert a UI-only model upgrade notice into non-empty chats whose model changes")
	recurringOnly := fs.Bool("recurring-only", false, "restrict task updates to recurring cron tasks")
	setTiers := fs.Bool("set-workspace-tiers", false, "also persist --model / --fallback-model as workspace_settings overrides and reload the running server")
	serverAddr := fs.String("server", "", "orchestrator address for live --set-workspace-tiers reload (default FLEET_ORCHESTRATOR_ADDR or 127.0.0.1:8000)")
	adminKey := fs.String("admin-key", "", "admin API key for live --set-workspace-tiers reload (default ADMIN_API_KEY env or fleet.env)")
	offline := fs.Bool("offline", false, "persist --set-workspace-tiers without notifying a running server (use only when fleet serve is stopped)")
	dryRun := fs.Bool("dry-run", false, "preview chats and tasks that would update without writing")
	noConfirm := fs.Bool("no-confirm", false, "skip interactive confirmation")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected positional argument %q", fs.Arg(0))
	}

	modelOpt := optionalFlag(fs, "model", model)
	fallbackOpt := optionalFlag(fs, "fallback-model", fallback)

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

	return runModelRollout(context.Background(), chatSt, schedSt, os.Stdout, os.Stderr, modelRolloutOptions{
		model:             modelOpt,
		fallbackModel:     fallbackOpt,
		fromModel:         strings.TrimSpace(*fromModel),
		fromFallbackModel: strings.TrimSpace(*fromFallback),
		notify:            *notify,
		recurringOnly:     *recurringOnly,
		setWorkspaceTiers: *setTiers,
		serverAddr:        *serverAddr,
		adminKey:          *adminKey,
		offline:           *offline,
		dryRun:            *dryRun,
		noConfirm:         *noConfirm,
	})
}

type modelRolloutOptions struct {
	model             *string
	fallbackModel     *string
	fromModel         string
	fromFallbackModel string
	notify            bool
	recurringOnly     bool
	setWorkspaceTiers bool
	serverAddr        string
	adminKey          string
	offline           bool
	dryRun            bool
	noConfirm         bool
}

type modelRolloutPlan struct {
	chatTarget      string
	chatFrom        string
	includeLockdown bool
	taskRollout     models.TaskModelRollout
}

func resolveModelRolloutPlan(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer, opts modelRolloutOptions) (modelRolloutPlan, int) {
	if code := validateWorkspaceTierFlags(errW, opts); code != 0 {
		return modelRolloutPlan{}, code
	}
	curDefault, curTask, curFallback, err := resolveEffectiveBoxTiers(ctx, chatSt)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return modelRolloutPlan{}, 5
	}
	plan := modelRolloutPlan{
		chatFrom: opts.fromModel,
		taskRollout: models.TaskModelRollout{
			FromModel:         opts.fromModel,
			FromFallbackModel: opts.fromFallbackModel,
			RecurringOnly:     opts.recurringOnly,
		},
	}

	if opts.model == nil && opts.fallbackModel == nil {
		plan.chatTarget = curDefault
		plan.taskRollout.Model = &curTask
		plan.taskRollout.FallbackModel = &curFallback
	} else {
		if opts.model != nil {
			v, err := validateCLIModelSlug("model", *opts.model)
			if err != nil {
				fmt.Fprintln(errW, err)
				return modelRolloutPlan{}, 1
			}
			plan.chatTarget = v
			plan.taskRollout.Model = &v
		}
		if opts.fallbackModel != nil {
			if code := applyFallbackPlanFlag(&plan, curDefault, curFallback, errW, opts); code != 0 {
				return modelRolloutPlan{}, code
			}
		}
	}

	plan.includeLockdown = resolveRolloutLockdown(ctx, curDefault, curFallback, errW, plan.chatTarget, opts)
	return plan, 0
}

func validateWorkspaceTierFlags(errW io.Writer, opts modelRolloutOptions) int {
	if !opts.setWorkspaceTiers {
		return 0
	}
	if opts.model == nil && opts.fallbackModel == nil {
		fmt.Fprintln(errW, "--set-workspace-tiers requires --model and/or --fallback-model")
		return 1
	}
	if !opts.dryRun && !opts.offline && resolveAdminKey(opts.adminKey) == "" {
		fmt.Fprintf(errW, "admin key required for --set-workspace-tiers against a running server: pass --admin-key, set ADMIN_API_KEY (in the shell or in %s), or pass --offline if fleet serve is stopped\n", serverEnvFile(""))
		return 1
	}
	return 0
}

func applyFallbackPlanFlag(plan *modelRolloutPlan, curDefault, curFallback string, errW io.Writer, opts modelRolloutOptions) int {
	trimmed := strings.TrimSpace(*opts.fallbackModel)
	if trimmed == "" {
		if opts.setWorkspaceTiers {
			fmt.Fprintln(errW, "--set-workspace-tiers requires a non-empty --fallback-model")
			return 1
		}
		empty := ""
		plan.taskRollout.FallbackModel = &empty
		return 0
	}
	v, err := validateCLIModelSlug("fallback-model", trimmed)
	if err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	plan.taskRollout.FallbackModel = &v
	if plan.chatTarget == "" && curFallback != "" && curFallback != v && curFallback != curDefault && opts.fromModel == "" {
		plan.chatTarget = v
		plan.chatFrom = curFallback
		plan.taskRollout.Model = &v
		plan.taskRollout.FromModel = curFallback
	}
	return 0
}

func resolveRolloutLockdown(ctx context.Context, curDefault, curFallback string, errW io.Writer, chatTarget string, opts modelRolloutOptions) bool {
	if chatTarget == "" {
		return true
	}
	includeLockdown := true
	writingTier := opts.setWorkspaceTiers && (opts.model != nil || (opts.fallbackModel != nil && strings.TrimSpace(*opts.fallbackModel) != ""))
	if raw := strings.TrimSpace(envOrFile("FLEET_LOCKDOWN_ALLOWED_MODELS")); raw != "" {
		includeLockdown = chatLockdownAllowsModel(ctx, nil, chatTarget)
	} else if !writingTier {
		includeLockdown = config.ModelAllowed(chatTarget, []string{curDefault, curFallback})
	}
	if !includeLockdown {
		fmt.Fprintf(errW, "note: %s is not in the lockdown allow-list; excluding lockdown conversations\n", chatTarget)
	}
	return includeLockdown
}

func previewModelRollout(ctx context.Context, chatSt modelRolloutChatStore, taskSt modelRolloutTaskStore, outW, errW io.Writer, plan modelRolloutPlan, notify bool) int {
	chatsToUpdate, chatsToNotify := 0, 0
	if plan.chatTarget != "" && chatSt != nil {
		convs, err := chatSt.ListConversationsForModelUpdate(ctx, plan.chatFrom, plan.includeLockdown)
		if err != nil {
			fmt.Fprintf(errW, "list conversations: %v\n", err)
			return 5
		}
		for _, c := range convs {
			if strings.TrimSpace(c.Model) == plan.chatTarget {
				continue
			}
			chatsToUpdate++
			if notify && c.HasMessages {
				chatsToNotify++
			}
			cur := c.Model
			if cur == "" {
				cur = "<none>"
			}
			fmt.Fprintf(outW, "chat\t%s\t%s\tmodel %s → %s\n", c.ID, c.UserEmail, cur, plan.chatTarget)
		}
	}
	var taskRes models.TaskModelRolloutResult
	if taskSt != nil && (plan.taskRollout.Model != nil || plan.taskRollout.FallbackModel != nil) {
		var err error
		taskRes, err = taskSt.RolloutTaskModels(ctx, plan.taskRollout, true)
		if err != nil {
			fmt.Fprintf(errW, "list tasks for rollout: %v\n", err)
			return 5
		}
		for _, t := range taskRes.Tasks {
			nextM, nextFB, _, _ := plan.taskRollout.ApplyToPointers(t.Model, t.FallbackModel)
			fmt.Fprintf(outW, "task\t%s\t%s\tmodel %s → %s\tfallback %s → %s\n",
				t.ID, taskLabel(t), optModelStr(t.Model), optModelStr(nextM), optModelStr(t.FallbackModel), optModelStr(nextFB))
		}
	}
	fmt.Fprintf(errW, "dry-run: %d chat(s) (%d notified) and %d task(s) (%d recurring) would be updated\n",
		chatsToUpdate, chatsToNotify, taskRes.UpdatedCount, taskRes.RecurringUpdatedCount)
	return 0
}

func persistRolloutWorkspaceTiers(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer, opts modelRolloutOptions) int {
	ws, ok := chatSt.(modelRolloutWorkspaceStore)
	if !ok {
		fmt.Fprintln(errW, "chat store does not support workspace_settings writes")
		return 5
	}
	prev, err := ws.WorkspaceSettings(ctx)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return 5
	}
	var touched []string
	if opts.model != nil {
		if _, err := ws.SetWorkspaceSetting(ctx, "default_model", strings.TrimSpace(*opts.model), "cli"); err != nil {
			fmt.Fprintf(errW, "save workspace default_model: %v\n", err)
			return 5
		}
		touched = append(touched, "default_model")
	}
	if opts.fallbackModel != nil {
		if _, err := ws.SetWorkspaceSetting(ctx, "advanced_model", strings.TrimSpace(*opts.fallbackModel), "cli"); err != nil {
			if rerr := restoreWorkspaceSettings(ctx, ws, prev, touched); rerr != nil {
				fmt.Fprintf(errW, "save workspace advanced_model: %v (rollback also failed: %v)\n", err, rerr)
			} else {
				fmt.Fprintf(errW, "save workspace advanced_model: %v\n", err)
			}
			return 5
		}
		touched = append(touched, "advanced_model")
	}
	if opts.offline {
		return 0
	}
	adminKey := resolveAdminKey(opts.adminKey)
	if err := notifyRunningServerConfigReload(ctx, opts.serverAddr, adminKey); err != nil {
		rollbackLiveTierReload(ctx, ws, prev, touched, opts.serverAddr, adminKey, err, errW)
		return 5
	}
	return 0
}

func rollbackLiveTierReload(
	ctx context.Context,
	ws modelRolloutWorkspaceStore,
	prev map[string]store.WorkspaceSetting,
	keys []string,
	serverAddr, adminKey string,
	reloadErr error,
	errW io.Writer,
) {
	if rerr := restoreWorkspaceSettings(ctx, ws, prev, keys); rerr != nil {
		fmt.Fprintf(errW, "apply workspace tiers on running server failed: %v (also failed to restore workspace_settings: %v)\n", reloadErr, rerr)
		return
	}
	if reapplyErr := notifyRunningServerConfigReload(ctx, serverAddr, adminKey); reapplyErr != nil {
		fmt.Fprintf(errW, "apply workspace tiers on running server failed: %v (rolled back workspace_settings in DB, but re-applying restored tiers on server failed: %v; pass --offline if fleet serve is stopped)\n", reloadErr, reapplyErr)
		return
	}
	fmt.Fprintf(errW, "apply workspace tiers on running server failed: %v (rolled back workspace_settings and re-applied previous tiers on server)\n", reloadErr)
}

func restoreWorkspaceSettings(ctx context.Context, ws modelRolloutWorkspaceStore, prev map[string]store.WorkspaceSetting, keys []string) error {
	var errs []error
	for _, k := range keys {
		if old, had := prev[k]; had {
			if _, err := ws.SetWorkspaceSetting(ctx, k, old.Value, old.UpdatedBy); err != nil {
				errs = append(errs, err)
			}
		} else {
			if err := ws.DeleteWorkspaceSetting(ctx, k); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func notifyRunningServerConfigReload(ctx context.Context, addrFlag, adminKey string) error {
	url := resolveOrchestratorURL(addrFlag) + "/admin/reload-config"
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func rolloutPromptSummary(plan modelRolloutPlan) string {
	var parts []string
	if plan.chatTarget != "" {
		parts = append(parts, "chats→"+plan.chatTarget)
	}
	if plan.taskRollout.Model != nil {
		parts = append(parts, "task-model→"+*plan.taskRollout.Model)
	}
	if plan.taskRollout.FallbackModel != nil {
		if *plan.taskRollout.FallbackModel == "" {
			parts = append(parts, "task-fallback→<cleared>")
		} else {
			parts = append(parts, "task-fallback→"+*plan.taskRollout.FallbackModel)
		}
	}
	return strings.Join(parts, ", ")
}

func runModelRollout(
	ctx context.Context,
	chatSt modelRolloutChatStore,
	taskSt modelRolloutTaskStore,
	outW, errW io.Writer,
	opts modelRolloutOptions,
) int {
	plan, code := resolveModelRolloutPlan(ctx, chatSt, errW, opts)
	if code != 0 {
		return code
	}
	if opts.dryRun {
		return previewModelRollout(ctx, chatSt, taskSt, outW, errW, plan, opts.notify)
	}

	prompt := fmt.Sprintf("Roll out model upgrade (%s)? [y/N]: ", rolloutPromptSummary(plan))
	if code := confirmBulkMutation(opts.noConfirm, prompt); code != 0 {
		return code
	}
	if opts.setWorkspaceTiers {
		if code := persistRolloutWorkspaceTiers(ctx, chatSt, errW, opts); code != 0 {
			return code
		}
	}

	var chatRes store.ConversationModelRolloutResult
	if plan.chatTarget != "" && chatSt != nil {
		var err error
		chatRes, err = chatSt.RolloutConversationModel(ctx, plan.chatTarget, plan.chatFrom, plan.includeLockdown, opts.notify)
		if err != nil {
			fmt.Fprintf(errW, "rollout conversations: %v\n", err)
			return 5
		}
	}

	var taskRes models.TaskModelRolloutResult
	if taskSt != nil && (plan.taskRollout.Model != nil || plan.taskRollout.FallbackModel != nil) {
		var err error
		taskRes, err = taskSt.RolloutTaskModels(ctx, plan.taskRollout, false)
		if err != nil {
			fmt.Fprintf(errW, "rollout tasks: %v\n", err)
			return 5
		}
	}

	fmt.Fprintf(errW, "rolled out models: %d chat(s) updated (%d notified), %d task(s) updated (%d recurring)\n",
		chatRes.UpdatedCount, chatRes.NotifiedCount, taskRes.UpdatedCount, taskRes.RecurringUpdatedCount)
	return 0
}

func optModelStr(p *string) string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return "<none>"
	}
	return strings.TrimSpace(*p)
}
