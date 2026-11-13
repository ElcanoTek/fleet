package admincli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/settings"
	"github.com/ElcanoTek/fleet/internal/store"
)

func cmdModel(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, modelUsageText)
		return 1
	}
	switch argv[0] {
	case "help", "-h", "--help":
		fmt.Print(modelUsageText)
		return 0
	case "status", "show", "list":
		return modelStatus(argv[1:])
	case "set":
		return modelRolloutCmd("model set", true, argv[1:])
	case "rollout", "upgrade":
		return modelRolloutCmd("model rollout", false, argv[1:])
	case "reset":
		return modelResetCmd(argv[1:])
	default:
		return errf(1, "unknown model subcommand %q (want status, set, rollout, or reset; see 'fleet model help')", argv[0])
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
	defTier, taskTier, fbTier, err := resolveBoxTierDetails(ctx, chatSt, nil)
	if err != nil {
		return "", "", "", err
	}
	return defTier.Value, taskTier.Value, fbTier.Value, nil
}

func validateCLIModelSlug(flagName, raw string) (string, error) {
	v, err := settings.Validate(settings.Spec{Key: flagName, Kind: settings.KindModel}, raw)
	if err != nil || len(v) > 200 {
		return "", fmt.Errorf("--%s: want a model slug like provider/model, got %q", flagName, raw)
	}
	return v, nil
}

func modelRolloutCmd(name string, defaultSetTiers bool, argv []string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN (default: DATABASE_URL)")
	model := fs.String("model", "", "primary model slug for both live chats and scheduled/recurring tasks")
	chatModel := fs.String("chat-model", "", "primary model slug for live chats only (overrides --model for chats)")
	taskModel := fs.String("task-model", "", "primary model slug for scheduled/recurring tasks only (overrides --model for tasks)")
	fallback := fs.String("fallback-model", "", "fallback model slug ('' clears task fallback_model to NULL when --set-workspace-tiers=false)")
	fromModel := fs.String("from-model", "", "only update chats and task primary models currently pinned to this slug")
	fromFallback := fs.String("from-fallback-model", "", "only update task fallback_model currently pinned to this slug")
	chats := fs.Bool("chats", true, "include live chat conversations in the rollout")
	tasks := fs.Bool("tasks", true, "include scheduled and recurring tasks in the rollout")
	rollout := fs.Bool("rollout", true, "roll out model changes to existing chats and/or tasks (false updates workspace_settings tiers only)")
	notify := fs.Bool("notify", true, "insert a UI-only model upgrade notice into active (non-empty) chats whose model changes")
	recurringOnly := fs.Bool("recurring-only", false, "restrict task updates to recurring cron tasks")
	setTiers := fs.Bool("set-workspace-tiers", defaultSetTiers, "persist tier overrides in workspace_settings and hot-reload the running server")
	serverAddr := fs.String("server", "", "orchestrator address for live tier reload (default FLEET_ORCHESTRATOR_ADDR or 127.0.0.1:8000)")
	adminKey := fs.String("admin-key", "", "admin API key for live tier reload (default ADMIN_API_KEY env or fleet.env)")
	offline := fs.Bool("offline", false, "persist workspace_settings without notifying a running server (use only when fleet serve is stopped)")
	dryRun := fs.Bool("dry-run", false, "preview changes without writing")
	noConfirm := fs.Bool("no-confirm", false, "skip interactive confirmation")
	yes := fs.Bool("yes", false, "skip interactive confirmation (alias for --no-confirm)")
	shortYes := fs.Bool("y", false, "skip interactive confirmation (shorthand for --yes)")
	asJSON := fs.Bool("json", false, "emit structured JSON result on stdout")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected positional argument %q", fs.Arg(0))
	}

	opts := modelRolloutOptions{
		model:             optionalFlag(fs, "model", model),
		chatModel:         optionalFlag(fs, "chat-model", chatModel),
		taskModel:         optionalFlag(fs, "task-model", taskModel),
		fallbackModel:     optionalFlag(fs, "fallback-model", fallback),
		fromModel:         strings.TrimSpace(*fromModel),
		fromFallbackModel: strings.TrimSpace(*fromFallback),
		chatsSet:          flagWasSet(fs, "chats"),
		chats:             *chats,
		tasksSet:          flagWasSet(fs, "tasks"),
		tasks:             *tasks,
		rolloutSet:        flagWasSet(fs, "rollout"),
		rollout:           *rollout,
		notify:            *notify,
		recurringOnly:     *recurringOnly,
		setWorkspaceTiers: *setTiers,
		serverAddr:        *serverAddr,
		adminKey:          *adminKey,
		offline:           *offline,
		dryRun:            *dryRun,
		noConfirm:         *noConfirm || *yes || *shortYes,
		asJSON:            *asJSON,
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

	return runModelRollout(context.Background(), chatSt, schedSt, os.Stdout, os.Stderr, opts)
}

type modelRolloutOptions struct {
	model             *string
	chatModel         *string
	taskModel         *string
	fallbackModel     *string
	fromModel         string
	fromFallbackModel string
	chatsSet          bool
	chats             bool
	tasksSet          bool
	tasks             bool
	rolloutSet        bool
	rollout           bool
	notify            bool
	recurringOnly     bool
	setWorkspaceTiers bool
	serverAddr        string
	adminKey          string
	offline           bool
	dryRun            bool
	noConfirm         bool
	asJSON            bool
}

func (o *modelRolloutOptions) normalizeDefaults() {
	if !o.chatsSet {
		o.chats = true
	}
	if !o.tasksSet {
		o.tasks = true
	}
	if !o.rolloutSet {
		o.rollout = true
	}
}

func (o modelRolloutOptions) hasAnyModelFlag() bool {
	return o.model != nil || o.chatModel != nil || o.taskModel != nil || o.fallbackModel != nil
}

// tierAction describes a single workspace_settings write (set==true) or delete (set==false).
type tierAction struct {
	key   string
	set   bool
	value string
}

type modelRolloutPlan struct {
	tierActions     []tierAction
	chatTarget      string
	chatFrom        string
	includeLockdown bool
	runTasks        bool
	taskRollout     models.TaskModelRollout
}

type modelRolloutJSONOutput struct {
	DryRun                bool              `json:"dry_run"`
	WorkspaceTiersUpdated map[string]string `json:"workspace_tiers_updated,omitempty"`
	WorkspaceTiersCleared []string          `json:"workspace_tiers_cleared,omitempty"`
	ChatTargetModel       string            `json:"chat_target_model,omitempty"`
	ChatsMatched          int               `json:"chats_matched"`
	ChatsUpdated          int               `json:"chats_updated"`
	ChatsNotified         int               `json:"chats_notified"`
	TaskTargetModel       string            `json:"task_target_model,omitempty"`
	TaskTargetFallback    *string           `json:"task_target_fallback,omitempty"`
	TasksScanned          int               `json:"tasks_scanned"`
	TasksUpdated          int               `json:"tasks_updated"`
	TasksPrimaryUpdated   int               `json:"tasks_primary_updated"`
	TasksFallbackUpdated  int               `json:"tasks_fallback_updated"`
	RecurringTasksUpdated int               `json:"recurring_tasks_updated"`
	PartialError          string            `json:"partial_error,omitempty"`
}

func resolveModelRolloutPlan(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer, opts modelRolloutOptions) (modelRolloutPlan, int) {
	opts.normalizeDefaults()
	if code := validateRolloutOptionFlags(errW, opts); code != 0 {
		return modelRolloutPlan{}, code
	}
	curDefault, curTask, curFallback, err := resolveEffectiveBoxTiers(ctx, chatSt)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return modelRolloutPlan{}, 5
	}

	validated, code := validateAllModelFlags(errW, opts)
	if code != 0 {
		return modelRolloutPlan{}, code
	}

	plan := buildRolloutPlan(curDefault, curTask, curFallback, opts, validated)
	effDefault, effFallback := effectivePostTierModels(curDefault, curFallback, plan.tierActions)
	plan.includeLockdown = resolveRolloutLockdown(ctx, effDefault, effFallback, errW, plan.chatTarget, opts)
	return plan, 0
}

func validateRolloutOptionFlags(errW io.Writer, opts modelRolloutOptions) int {
	if opts.chatModel != nil && !opts.chats {
		fmt.Fprintln(errW, "--chat-model cannot be combined with --chats=false")
		return 1
	}
	if opts.taskModel != nil && !opts.tasks {
		fmt.Fprintln(errW, "--task-model cannot be combined with --tasks=false")
		return 1
	}
	if opts.recurringOnly && !opts.tasks {
		fmt.Fprintln(errW, "--recurring-only cannot be combined with --tasks=false")
		return 1
	}
	if !opts.rollout && !opts.setWorkspaceTiers {
		fmt.Fprintln(errW, "nothing to do: --rollout=false requires --set-workspace-tiers (or 'fleet model set')")
		return 1
	}
	if opts.rollout && !opts.chats && !opts.tasks && !opts.setWorkspaceTiers {
		fmt.Fprintln(errW, "nothing to do: both --chats=false and --tasks=false")
		return 1
	}
	if !opts.setWorkspaceTiers {
		return 0
	}
	if !opts.hasAnyModelFlag() {
		fmt.Fprintln(errW, "--set-workspace-tiers (and 'fleet model set') requires at least one of --model, --chat-model, --task-model, or --fallback-model")
		return 1
	}
	if opts.fallbackModel != nil && strings.TrimSpace(*opts.fallbackModel) == "" {
		fmt.Fprintln(errW, "--set-workspace-tiers requires a non-empty --fallback-model (use 'fleet model reset --tier fallback' to clear an override)")
		return 1
	}
	if !opts.dryRun && !opts.offline && resolveAdminKey(opts.adminKey) == "" {
		fmt.Fprintf(errW, "admin key required for live tier reload: pass --admin-key, set ADMIN_API_KEY (in the shell or in %s), or pass --offline if fleet serve is stopped\n", serverEnvFile(""))
		return 1
	}
	return 0
}

type validatedModelSlugs struct {
	model         *string
	chatModel     *string
	taskModel     *string
	fallbackModel *string
}

func validateAllModelFlags(errW io.Writer, opts modelRolloutOptions) (validatedModelSlugs, int) {
	var out validatedModelSlugs
	check := func(name string, src *string) (*string, int) {
		if src == nil {
			return nil, 0
		}
		v, err := validateCLIModelSlug(name, *src)
		if err != nil {
			fmt.Fprintln(errW, err)
			return nil, 1
		}
		return &v, 0
	}
	var code int
	if out.model, code = check("model", opts.model); code != 0 {
		return out, code
	}
	if out.chatModel, code = check("chat-model", opts.chatModel); code != 0 {
		return out, code
	}
	if out.taskModel, code = check("task-model", opts.taskModel); code != 0 {
		return out, code
	}
	if opts.fallbackModel != nil {
		trimmed := strings.TrimSpace(*opts.fallbackModel)
		if trimmed == "" {
			empty := ""
			out.fallbackModel = &empty
		} else if out.fallbackModel, code = check("fallback-model", opts.fallbackModel); code != 0 {
			return out, code
		}
	}
	return out, 0
}

func buildRolloutPlan(curDefault, curTask, curFallback string, opts modelRolloutOptions, v validatedModelSlugs) modelRolloutPlan {
	plan := modelRolloutPlan{
		chatFrom: opts.fromModel,
		taskRollout: models.TaskModelRollout{
			FromModel:         opts.fromModel,
			FromFallbackModel: opts.fromFallbackModel,
			RecurringOnly:     opts.recurringOnly,
		},
	}

	// Effective desired chat primary and task primary when flags are supplied.
	var wantChat, wantTask *string
	if opts.chats {
		if v.chatModel != nil {
			wantChat = v.chatModel
		} else if v.model != nil {
			wantChat = v.model
		}
	}
	if opts.tasks {
		if v.taskModel != nil {
			wantTask = v.taskModel
		} else if v.model != nil {
			wantTask = v.model
		}
	}

	if opts.setWorkspaceTiers {
		plan.tierActions = computeWorkspaceTierActions(curDefault, curTask, wantChat, wantTask, v.fallbackModel)
	}

	if !opts.rollout {
		return plan
	}

	if !opts.hasAnyModelFlag() {
		if opts.chats {
			plan.chatTarget = curDefault
		}
		if opts.tasks {
			fb := configuredTaskFallbackEnv()
			if fb == "" {
				fb = curFallback
			}
			plan.taskRollout.Model = &curTask
			plan.taskRollout.FallbackModel = &fb
			plan.taskRollout.OnlySetFallbacks = true
			plan.runTasks = true
		}
		return plan
	}

	if wantChat != nil {
		plan.chatTarget = *wantChat
	}
	if wantTask != nil {
		plan.taskRollout.Model = wantTask
		plan.runTasks = true
	}
	if v.fallbackModel != nil {
		applyFallbackToPlan(&plan, curDefault, curTask, curFallback, opts, *v.fallbackModel)
	}
	return plan
}

func computeWorkspaceTierActions(curDefault, curTask string, wantChat, wantTask, wantFallback *string) []tierAction {
	var actions []tierAction
	newDefault := curDefault
	if wantChat != nil {
		newDefault = *wantChat
		actions = append(actions, tierAction{key: "default_model", set: true, value: newDefault})
	}
	envTask := strings.TrimSpace(envOrFile("FLEET_TASK_MODEL"))
	switch {
	case wantTask != nil:
		if *wantTask == newDefault && envTask == "" {
			actions = append(actions, tierAction{key: "task_model", set: false})
		} else {
			actions = append(actions, tierAction{key: "task_model", set: true, value: *wantTask})
		}
	case wantChat != nil && curTask != newDefault:
		// Operator changed ONLY the chat model (--chat-model or --tasks=false);
		// pin task_model to curTask so newly created tasks keep their current tier.
		actions = append(actions, tierAction{key: "task_model", set: true, value: curTask})
	}
	if wantFallback != nil && *wantFallback != "" {
		actions = append(actions, tierAction{key: "advanced_model", set: true, value: *wantFallback})
	}
	return actions
}

func applyFallbackToPlan(plan *modelRolloutPlan, curDefault, curTask, curFallback string, opts modelRolloutOptions, fb string) {
	if opts.tasks {
		plan.taskRollout.FallbackModel = &fb
		plan.runTasks = true
		if fb != "" && plan.taskRollout.Model == nil && curFallback != "" && curFallback != fb && curFallback != curTask && opts.fromModel == "" {
			plan.taskRollout.Model = &fb
			plan.taskRollout.FromModel = curFallback
		}
	}
	if opts.chats && fb != "" && plan.chatTarget == "" && curFallback != "" && curFallback != fb && curFallback != curDefault && opts.fromModel == "" {
		plan.chatTarget = fb
		plan.chatFrom = curFallback
	}
}

func effectivePostTierModels(curDefault, curFallback string, actions []tierAction) (effDefault, effFallback string) {
	effDefault, effFallback = curDefault, curFallback
	for _, a := range actions {
		if !a.set {
			continue
		}
		switch a.key {
		case "default_model":
			effDefault = a.value
		case "advanced_model":
			effFallback = a.value
		}
	}
	return effDefault, effFallback
}

func resolveRolloutLockdown(ctx context.Context, effDefault, effFallback string, errW io.Writer, chatTarget string, opts modelRolloutOptions) bool {
	if chatTarget == "" {
		return true
	}
	var includeLockdown bool
	if raw := strings.TrimSpace(envOrFile("FLEET_LOCKDOWN_ALLOWED_MODELS")); raw != "" {
		includeLockdown = chatLockdownAllowsModel(ctx, nil, chatTarget)
	} else {
		includeLockdown = config.ModelAllowed(chatTarget, []string{effDefault, effFallback})
	}
	if !includeLockdown && !opts.asJSON {
		fmt.Fprintf(errW, "note: %s is not in the lockdown allow-list; excluding lockdown conversations\n", chatTarget)
	}
	return includeLockdown
}

func previewModelRollout(ctx context.Context, chatSt modelRolloutChatStore, taskSt modelRolloutTaskStore, outW, errW io.Writer, plan modelRolloutPlan, opts modelRolloutOptions) int {
	var jout modelRolloutJSONOutput
	jout.DryRun = true
	populateJSONTierActions(&jout, plan)

	if plan.chatTarget != "" && chatSt != nil {
		convs, err := chatSt.ListConversationsForModelUpdate(ctx, plan.chatFrom, plan.includeLockdown)
		if err != nil {
			fmt.Fprintf(errW, "list conversations: %v\n", err)
			return 5
		}
		jout.ChatsMatched = len(convs)
		for _, c := range convs {
			if strings.TrimSpace(c.Model) == plan.chatTarget {
				continue
			}
			jout.ChatsUpdated++
			if opts.notify && c.HasMessages {
				jout.ChatsNotified++
			}
			if !opts.asJSON {
				cur := c.Model
				if cur == "" {
					cur = "<none>"
				}
				fmt.Fprintf(outW, "chat\t%s\t%s\tmodel %s → %s\n", c.ID, c.UserEmail, cur, plan.chatTarget)
			}
		}
	}
	if plan.runTasks && taskSt != nil {
		taskRes, err := taskSt.RolloutTaskModels(ctx, plan.taskRollout, true)
		if err != nil {
			fmt.Fprintf(errW, "list tasks for rollout: %v\n", err)
			return 5
		}
		jout.TasksScanned = taskRes.ScannedCount
		jout.TasksUpdated = taskRes.UpdatedCount
		jout.TasksPrimaryUpdated = taskRes.PrimaryUpdatedCount
		jout.TasksFallbackUpdated = taskRes.FallbackUpdatedCount
		jout.RecurringTasksUpdated = taskRes.RecurringUpdatedCount
		if !opts.asJSON {
			for _, t := range taskRes.Tasks {
				nextM, nextFB, _, _ := plan.taskRollout.ApplyToPointers(t.Model, t.FallbackModel)
				fmt.Fprintf(outW, "task\t%s\t%s\tmodel %s → %s\tfallback %s → %s\n",
					t.ID, taskLabel(t), optModelStr(t.Model), optModelStr(nextM), optModelStr(t.FallbackModel), optModelStr(nextFB))
			}
		}
	}
	if opts.asJSON {
		emitRolloutJSON(outW, jout)
		return 0
	}
	fmt.Fprintf(errW, "dry-run: %d chat(s) (%d notified) and %d task(s) (%d recurring) would be updated\n",
		jout.ChatsUpdated, jout.ChatsNotified, jout.TasksUpdated, jout.RecurringTasksUpdated)
	return 0
}

func populateJSONTierActions(jout *modelRolloutJSONOutput, plan modelRolloutPlan) {
	jout.ChatTargetModel = plan.chatTarget
	if plan.taskRollout.Model != nil {
		jout.TaskTargetModel = *plan.taskRollout.Model
	}
	if plan.taskRollout.FallbackModel != nil {
		v := *plan.taskRollout.FallbackModel
		jout.TaskTargetFallback = &v
	}
	for _, a := range plan.tierActions {
		if a.set {
			if jout.WorkspaceTiersUpdated == nil {
				jout.WorkspaceTiersUpdated = map[string]string{}
			}
			jout.WorkspaceTiersUpdated[a.key] = a.value
		} else {
			jout.WorkspaceTiersCleared = append(jout.WorkspaceTiersCleared, a.key)
		}
	}
}

func emitRolloutJSON(outW io.Writer, jout modelRolloutJSONOutput) {
	enc := json.NewEncoder(outW)
	enc.SetIndent("", "  ")
	_ = enc.Encode(jout)
}

func applyWorkspaceTierActions(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer, actions []tierAction, serverAddr, adminKeyFlag string, offline bool) int {
	if len(actions) == 0 {
		return 0
	}
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
	for _, a := range actions {
		var werr error
		if a.set {
			_, werr = ws.SetWorkspaceSetting(ctx, a.key, a.value, "cli")
		} else {
			werr = ws.DeleteWorkspaceSetting(ctx, a.key)
		}
		if werr != nil {
			if rerr := restoreWorkspaceSettings(ctx, ws, prev, touched); rerr != nil {
				fmt.Fprintf(errW, "save workspace %s: %v (rollback also failed: %v)\n", a.key, werr, rerr)
			} else {
				fmt.Fprintf(errW, "save workspace %s: %v\n", a.key, werr)
			}
			return 5
		}
		touched = append(touched, a.key)
	}
	if offline {
		return 0
	}
	adminKey := resolveAdminKey(adminKeyFlag)
	if err := notifyRunningServerConfigReload(ctx, serverAddr, adminKey); err != nil {
		rollbackLiveTierReload(ctx, ws, prev, touched, serverAddr, adminKey, err, errW)
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
	for _, a := range plan.tierActions {
		if a.set {
			parts = append(parts, a.key+"="+a.value)
		} else {
			parts = append(parts, a.key+"=<reset>")
		}
	}
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

type modelRolloutAdvisoryLocker interface {
	AcquireModelRolloutLock(ctx context.Context) (func(), error)
}

func configuredTaskFallbackEnv() string {
	return strings.TrimSpace(envOrFile("FLEET_TASK_FALLBACK_MODEL"))
}

func acquireCLIModelRolloutLock(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer) (func(), int) {
	locker, ok := chatSt.(modelRolloutAdvisoryLocker)
	if !ok {
		return func() {}, 0
	}
	release, err := locker.AcquireModelRolloutLock(ctx)
	if err != nil {
		fmt.Fprintf(errW, "acquire model rollout lock: %v\n", err)
		return nil, 5
	}
	return release, 0
}

func rolloutPlansEqual(a, b modelRolloutPlan) bool {
	if len(a.tierActions) != len(b.tierActions) {
		return false
	}
	for i := range a.tierActions {
		if a.tierActions[i] != b.tierActions[i] {
			return false
		}
	}
	if a.chatTarget != b.chatTarget || a.chatFrom != b.chatFrom || a.includeLockdown != b.includeLockdown || a.runTasks != b.runTasks {
		return false
	}
	return optModelStr(a.taskRollout.Model) == optModelStr(b.taskRollout.Model) &&
		a.taskRollout.FromModel == b.taskRollout.FromModel &&
		(a.taskRollout.FallbackModel == nil) == (b.taskRollout.FallbackModel == nil) &&
		(a.taskRollout.FallbackModel == nil || *a.taskRollout.FallbackModel == *b.taskRollout.FallbackModel) &&
		a.taskRollout.FromFallbackModel == b.taskRollout.FromFallbackModel &&
		a.taskRollout.OnlySetFallbacks == b.taskRollout.OnlySetFallbacks &&
		a.taskRollout.RecurringOnly == b.taskRollout.RecurringOnly
}

func confirmAndLockRolloutPlan(
	ctx context.Context,
	chatSt modelRolloutChatStore,
	errW io.Writer,
	noConfirm bool,
	confirm func(bool, string) int,
	resolve func() (modelRolloutPlan, int),
) (modelRolloutPlan, func(), int) {
	initialPlan, code := resolve()
	if code != 0 {
		return modelRolloutPlan{}, nil, code
	}
	prompt := fmt.Sprintf("Apply model update (%s)?", rolloutPromptSummary(initialPlan))
	if code := confirm(noConfirm, prompt); code != 0 {
		return modelRolloutPlan{}, nil, code
	}

	release, code := acquireCLIModelRolloutLock(ctx, chatSt, errW)
	if code != 0 {
		return modelRolloutPlan{}, nil, code
	}
	plan, code := resolve()
	if code != 0 {
		release()
		return modelRolloutPlan{}, nil, code
	}
	if noConfirm || rolloutPlansEqual(initialPlan, plan) {
		return plan, release, 0
	}

	// Another operator mutated workspace_settings while waiting on interactive
	// confirmation: release the lock before reading stdin again, ask the
	// operator to confirm the updated plan, and verify once more under the lock.
	release()
	fmt.Fprintln(errW, "note: workspace model tiers changed while waiting for confirmation")
	prompt2 := fmt.Sprintf("Apply updated model plan (%s)?", rolloutPromptSummary(plan))
	if code := confirm(false, prompt2); code != 0 {
		return modelRolloutPlan{}, nil, code
	}
	release2, code := acquireCLIModelRolloutLock(ctx, chatSt, errW)
	if code != 0 {
		return modelRolloutPlan{}, nil, code
	}
	plan2, code := resolve()
	if code != 0 {
		release2()
		return modelRolloutPlan{}, nil, code
	}
	if !rolloutPlansEqual(plan, plan2) {
		release2()
		fmt.Fprintln(errW, "workspace model tiers changed again during confirmation; re-run command")
		return modelRolloutPlan{}, nil, 5
	}
	return plan2, release2, 0
}

func runModelRollout(
	ctx context.Context,
	chatSt modelRolloutChatStore,
	taskSt modelRolloutTaskStore,
	outW, errW io.Writer,
	opts modelRolloutOptions,
) int {
	opts.normalizeDefaults()
	if opts.dryRun {
		plan, code := resolveModelRolloutPlan(ctx, chatSt, errW, opts)
		if code != 0 {
			return code
		}
		return previewModelRollout(ctx, chatSt, taskSt, outW, errW, plan, opts)
	}

	plan, release, code := confirmAndLockRolloutPlan(ctx, chatSt, errW, opts.noConfirm, confirmBulkMutation, func() (modelRolloutPlan, int) {
		return resolveModelRolloutPlan(ctx, chatSt, errW, opts)
	})
	if code != 0 {
		return code
	}
	defer release()
	return executeLockedModelRolloutPlan(ctx, chatSt, taskSt, outW, errW, plan, opts)
}

func executeLockedModelRolloutPlan(
	ctx context.Context,
	chatSt modelRolloutChatStore,
	taskSt modelRolloutTaskStore,
	outW, errW io.Writer,
	plan modelRolloutPlan,
	opts modelRolloutOptions,
) int {
	tiersCommitted := false
	if len(plan.tierActions) > 0 {
		if code := applyWorkspaceTierActions(ctx, chatSt, errW, plan.tierActions, opts.serverAddr, opts.adminKey, opts.offline); code != 0 {
			return code
		}
		tiersCommitted = true
	}

	// Execute task rollout BEFORE chat rollout so a scheduler failure aborts
	// before conversations are updated or chat upgrade notices are inserted.
	var taskRes models.TaskModelRolloutResult
	if plan.runTasks && taskSt != nil {
		var err error
		taskRes, err = taskSt.RolloutTaskModels(ctx, plan.taskRollout, false)
		if err != nil {
			if tiersCommitted {
				partialMsg := fmt.Sprintf("rollout tasks: %v (workspace model tier(s) updated before task rollout failed; re-run 'fleet model rollout' once resolved)", err)
				emitPartialRolloutFailure(outW, errW, opts.asJSON, plan, taskRes, partialMsg)
				return 5
			}
			fmt.Fprintf(errW, "rollout tasks: %v\n", err)
			return 5
		}
	}

	var chatRes store.ConversationModelRolloutResult
	if plan.chatTarget != "" && chatSt != nil {
		var err error
		chatRes, err = chatSt.RolloutConversationModel(ctx, plan.chatTarget, plan.chatFrom, plan.includeLockdown, opts.notify)
		if err != nil {
			if tiersCommitted || taskRes.UpdatedCount > 0 {
				partialMsg := formatChatRolloutPartialError(err, tiersCommitted, taskRes.UpdatedCount)
				emitPartialRolloutFailure(outW, errW, opts.asJSON, plan, taskRes, partialMsg)
				return 5
			}
			fmt.Fprintf(errW, "rollout conversations: %v\n", err)
			return 5
		}
	}

	if opts.asJSON {
		var jout modelRolloutJSONOutput
		populateJSONTierActions(&jout, plan)
		jout.ChatsMatched = chatRes.MatchedCount
		jout.ChatsUpdated = chatRes.UpdatedCount
		jout.ChatsNotified = chatRes.NotifiedCount
		jout.TasksScanned = taskRes.ScannedCount
		jout.TasksUpdated = taskRes.UpdatedCount
		jout.TasksPrimaryUpdated = taskRes.PrimaryUpdatedCount
		jout.TasksFallbackUpdated = taskRes.FallbackUpdatedCount
		jout.RecurringTasksUpdated = taskRes.RecurringUpdatedCount
		emitRolloutJSON(outW, jout)
		return 0
	}

	fmt.Fprintf(errW, "rolled out models: %d chat(s) updated (%d notified), %d task(s) updated (%d recurring)\n",
		chatRes.UpdatedCount, chatRes.NotifiedCount, taskRes.UpdatedCount, taskRes.RecurringUpdatedCount)
	return 0
}

func emitPartialRolloutFailure(
	outW, errW io.Writer,
	asJSON bool,
	plan modelRolloutPlan,
	taskRes models.TaskModelRolloutResult,
	partialMsg string,
) {
	if asJSON {
		var jout modelRolloutJSONOutput
		populateJSONTierActions(&jout, plan)
		jout.TasksScanned = taskRes.ScannedCount
		jout.TasksUpdated = taskRes.UpdatedCount
		jout.TasksPrimaryUpdated = taskRes.PrimaryUpdatedCount
		jout.TasksFallbackUpdated = taskRes.FallbackUpdatedCount
		jout.RecurringTasksUpdated = taskRes.RecurringUpdatedCount
		jout.PartialError = partialMsg
		emitRolloutJSON(outW, jout)
	}
	fmt.Fprintln(errW, partialMsg)
}

func formatChatRolloutPartialError(err error, tiersCommitted bool, tasksUpdated int) string {
	switch {
	case tiersCommitted && tasksUpdated > 0:
		return fmt.Sprintf("rollout conversations: %v (workspace model tier(s) and %d task(s) updated before chat rollout failed)", err, tasksUpdated)
	case tiersCommitted:
		return fmt.Sprintf("rollout conversations: %v (workspace model tier(s) updated before chat rollout failed)", err)
	default:
		return fmt.Sprintf("rollout conversations: %v (%d task(s) updated before chat rollout failed)", err, tasksUpdated)
	}
}

func modelResetCmd(argv []string) int {
	fs := flag.NewFlagSet("model reset", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "Postgres DSN (default: DATABASE_URL)")
	tier := fs.String("tier", "all", "tier to reset: all, default (chats), task (tasks), or fallback")
	rollout := fs.Bool("rollout", true, "also sync existing chats/tasks to the reverted effective tier(s)")
	notify := fs.Bool("notify", true, "insert a UI-only model upgrade notice into active chats whose model changes")
	serverAddr := fs.String("server", "", "orchestrator address for live tier reload (default FLEET_ORCHESTRATOR_ADDR or 127.0.0.1:8000)")
	adminKey := fs.String("admin-key", "", "admin API key for live tier reload (default ADMIN_API_KEY env or fleet.env)")
	offline := fs.Bool("offline", false, "reset workspace_settings without notifying a running server (use only when fleet serve is stopped)")
	dryRun := fs.Bool("dry-run", false, "preview changes without writing")
	noConfirm := fs.Bool("no-confirm", false, "skip interactive confirmation")
	yes := fs.Bool("yes", false, "skip interactive confirmation (alias for --no-confirm)")
	shortYes := fs.Bool("y", false, "skip interactive confirmation (shorthand for --yes)")
	asJSON := fs.Bool("json", false, "emit structured JSON result on stdout")
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

	return runModelReset(context.Background(), chatSt, schedSt, os.Stdout, os.Stderr, modelResetOptions{
		tier:       *tier,
		rollout:    *rollout,
		notify:     *notify,
		serverAddr: *serverAddr,
		adminKey:   *adminKey,
		offline:    *offline,
		dryRun:     *dryRun,
		noConfirm:  *noConfirm || *yes || *shortYes,
		asJSON:     *asJSON,
	})
}

type modelResetOptions struct {
	tier       string
	rollout    bool
	notify     bool
	serverAddr string
	adminKey   string
	offline    bool
	dryRun     bool
	noConfirm  bool
	asJSON     bool
}

func resetKeysForTier(raw string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "all":
		return []string{"default_model", "task_model", "advanced_model"}, nil
	case "default", "chat", "chats", "default_model":
		return []string{"default_model"}, nil
	case "task", "tasks", "task_model":
		return []string{"task_model"}, nil
	case "fallback", "advanced", "advanced_model":
		return []string{"advanced_model"}, nil
	default:
		return nil, fmt.Errorf("unknown --tier %q (want all, default, task, or fallback)", raw)
	}
}

func buildResetRolloutPlan(ctx context.Context, chatSt modelRolloutChatStore, errW io.Writer, keys []string, ropts modelRolloutOptions) (modelRolloutPlan, int) {
	curDef, curTask, curFB, err := resolveEffectiveBoxTiers(ctx, chatSt)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return modelRolloutPlan{}, 5
	}
	ignore := make(map[string]bool, len(keys))
	var actions []tierAction
	for _, k := range keys {
		ignore[k] = true
		actions = append(actions, tierAction{key: k, set: false})
	}
	nextDef, nextTask, nextFB, err := resolveBoxTierDetails(ctx, chatSt, ignore)
	if err != nil {
		fmt.Fprintf(errW, "read workspace_settings: %v\n", err)
		return modelRolloutPlan{}, 5
	}
	plan := modelRolloutPlan{tierActions: actions}
	if ropts.rollout {
		populateResetRolloutPlan(&plan, ignore, curDef, curTask, curFB, nextDef, nextTask, nextFB)
	}
	plan.includeLockdown = resolveRolloutLockdown(ctx, nextDef.Value, nextFB.Value, errW, plan.chatTarget, ropts)
	return plan, 0
}

func runModelReset(ctx context.Context, chatSt modelRolloutChatStore, taskSt modelRolloutTaskStore, outW, errW io.Writer, opts modelResetOptions) int {
	keys, err := resetKeysForTier(opts.tier)
	if err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	if !opts.dryRun && !opts.offline && resolveAdminKey(opts.adminKey) == "" {
		fmt.Fprintf(errW, "admin key required for live tier reload: pass --admin-key, set ADMIN_API_KEY (in the shell or in %s), or pass --offline if fleet serve is stopped\n", serverEnvFile(""))
		return 1
	}
	ropts := modelRolloutOptions{
		chats:      true,
		tasks:      true,
		rollout:    opts.rollout,
		notify:     opts.notify,
		serverAddr: opts.serverAddr,
		adminKey:   opts.adminKey,
		offline:    opts.offline,
		dryRun:     opts.dryRun,
		noConfirm:  opts.noConfirm,
		asJSON:     opts.asJSON,
	}
	if opts.dryRun {
		plan, code := buildResetRolloutPlan(ctx, chatSt, errW, keys, ropts)
		if code != 0 {
			return code
		}
		return previewModelRollout(ctx, chatSt, taskSt, outW, errW, plan, ropts)
	}
	plan, release, code := confirmAndLockRolloutPlan(ctx, chatSt, errW, opts.noConfirm, confirmBulkMutation, func() (modelRolloutPlan, int) {
		return buildResetRolloutPlan(ctx, chatSt, errW, keys, ropts)
	})
	if code != 0 {
		return code
	}
	defer release()
	return executeLockedModelRolloutPlan(ctx, chatSt, taskSt, outW, errW, plan, ropts)
}

func populateResetRolloutPlan(plan *modelRolloutPlan, resetKeys map[string]bool, curDef, curTask, curFB string, nextDef, nextTask, nextFB tierInfo) {
	if resetKeys["default_model"] {
		plan.chatTarget = nextDef.Value
		if nextTask.Source == "follows_default" || resetKeys["task_model"] {
			v := nextTask.Value
			plan.taskRollout.Model = &v
			plan.runTasks = true
		}
	}
	if resetKeys["task_model"] && plan.taskRollout.Model == nil {
		v := nextTask.Value
		plan.taskRollout.Model = &v
		plan.runTasks = true
	}
	if resetKeys["advanced_model"] {
		fb := nextFB.Value
		if configuredTaskFallbackEnv() == "" {
			ropts := modelRolloutOptions{chats: true, tasks: true}
			applyFallbackToPlan(plan, curDef, curTask, curFB, ropts, fb)
			plan.taskRollout.OnlySetFallbacks = true
		} else if curFB != "" && curFB != fb && curFB != curDef {
			if plan.chatTarget == "" {
				plan.chatTarget = fb
				plan.chatFrom = curFB
			}
			if plan.taskRollout.Model == nil && curFB != curTask {
				plan.taskRollout.Model = &fb
				plan.taskRollout.FromModel = curFB
				plan.runTasks = true
			}
		}
	}
}
