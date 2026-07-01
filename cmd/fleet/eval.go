package main

// The `fleet eval` verb (#502) — the self-hosted eval & regression harness's
// CLI face, mirroring validate-config (a server-family verb loading the SAME
// bundle+config loaders the server boots through):
//
//	fleet eval run <set>      replay a set's goldens through the governed loop,
//	                          score them, persist an eval_runs row, gate on the
//	                          set's threshold (exit 0 pass / 1 fail — the CI gate)
//	fleet eval list           list the bundle's eval sets (+ definition problems)
//	fleet eval history [set]  newest-first eval_runs rows
//	fleet eval capture        save a past run (scheduled task or chat
//	                          conversation) as a golden case in a set file
//
// Replays run IN-PROCESS: the verb builds the same agent.Manager the server
// boots (model resolver, sandbox pool, persona/prompt dirs) and drives each
// golden through Manager.RunTurn → agentcore.Run, so a replay inherits the
// mandatory sandbox, ceilings, and redaction with zero bespoke run path. That
// means `fleet eval run` needs what `fleet serve` needs: the model API key (or
// providers block) and a working sandbox runtime.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/evals"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scorers"
	"github.com/ElcanoTek/fleet/internal/store"
)

const evalUsage = `Usage: fleet eval <subcommand> [flags]

Subcommands:
  run <set>     Replay the set's goldens through the governed loop and gate on
                its threshold. Exit 0 = pass, 1 = regression/failure, 2 = usage.
                Flags: --bundle-path DIR  --json  --no-db  --temperature N
  list          List the bundle's eval sets and any definition problems.
                Flags: --bundle-path DIR  --json
  history [set] Show persisted eval runs, newest first.
                Flags: --limit N  --json
  capture       Save a past run as a golden case in <bundle>/evals/<set>.yaml.
                Flags: --task UUID | --conversation ID --user EMAIL
                       --set NAME  [--name NAME] [--model SLUG]
                       [--rubric TEXT] [--out PATH|-]

Eval sets are client content: YAML files under the bundle's evals/ dir. See
docs/EVALS.md for the file format and the CI-gate recipe.`

// runEvalCmd is the `fleet eval` entry point; returns the process exit code.
func runEvalCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, evalUsage)
		return 2
	}
	switch args[0] {
	case "run":
		return evalRun(args[1:])
	case "list":
		return evalList(args[1:])
	case "history":
		return evalHistory(args[1:])
	case "capture":
		return evalCapture(args[1:])
	case "help", "-h", "--help":
		fmt.Println(evalUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fleet eval: unknown subcommand %q\n%s\n", args[0], evalUsage)
		return 2
	}
}

// loadEvalEnv loads the bundle + config through the boot loaders (the
// validate-config pattern) after honoring --bundle-path.
func loadEvalEnv(bundlePath string) (*clientconfig.Bundle, *config.Config, error) {
	if strings.TrimSpace(bundlePath) != "" {
		_ = os.Setenv(clientconfig.EnvDir, bundlePath)
	}
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return nil, nil, fmt.Errorf("load bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	return bundle, cfg, nil
}

// ── fleet eval run ──

func evalRun(args []string) int {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	bundlePath := fs.String("bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR)")
	jsonOut := fs.Bool("json", false, "emit the run result as JSON")
	noDB := fs.Bool("no-db", false, "do not persist the run to eval_runs (no DB needed)")
	temperature := fs.Float64("temperature", 0, "sampling temperature for replays (0 = deterministic-ish, the eval default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "fleet eval run: exactly one <set> argument required\n%s\n", evalUsage)
		return 2
	}
	setName := fs.Arg(0)

	bundle, cfg, err := loadEvalEnv(*bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval run: %v\n", err)
		return 1
	}

	sets, problems := evals.LoadSets(bundle.EvalsDir)
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "warning: %s\n", p)
	}
	set := evals.FindSet(sets, setName)
	if set == nil {
		fmt.Fprintf(os.Stderr, "fleet eval run: no eval set %q under %s (known sets: %s)\n",
			setName, bundle.EvalsDir, setNames(sets))
		return 1
	}

	if cfg.OpenRouterAPIKey == "" && !cfg.MockMode && len(toAgentcoreProviders(bundle)) == 0 {
		fmt.Fprintln(os.Stderr, "fleet eval run: no model credentials — set OPENROUTER_API_KEY (or a manifest providers: block)")
		return 1
	}

	// Pin the replay temperature. The eval CLI owns this Config and registers no
	// reload triggers, so the direct field write is race-free; a live server's
	// FLEET_TEMPERATURE never leaks into a replay's score.
	cfg.Temperature = *temperature

	// The same Manager construction the server boots (minus MCP: goldens replay
	// with the native tool set only — see docs/EVALS.md honest scope).
	mgr, err := agent.New(agent.ManagerOptions{
		Config:               cfg,
		PersonasDir:          bundle.PersonasDir,
		ProtocolsDir:         bundle.ProtocolsDir,
		SkillsDir:            bundle.SkillsDir,
		SystemPromptsDir:     bundle.SystemPromptsDir,
		ChatSystemPromptFile: "chat.md",
		LLMProviders:         toAgentcoreProviders(bundle),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval run: build engine: %v\n", err)
		return 1
	}
	defer mgr.Close()

	sha, err := evals.BundleFingerprint(clientconfig.Dir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: bundle fingerprint: %v\n", err)
	}

	runID := uuid.NewString()[:8]
	fmt.Fprintf(os.Stderr, "eval set %q: %d case(s), threshold %.2f, bundle %s\n",
		set.Name, len(set.Cases), set.EffectiveThreshold(), shortSHA(sha))

	result, err := evals.RunSet(context.Background(), mgr, set, evals.Options{
		RunID:     runID,
		BundleSHA: sha,
		Progress: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval run: %v\n", err)
		return 1
	}

	// Baseline delta + persistence (best-effort: an eval is still a valid gate
	// on a box with no orchestrator DB, e.g. a bundle repo's CI job).
	var baseline *models.EvalRun
	if !*noDB {
		if st, code := openEvalStorage(); code == 0 {
			baseline, _ = st.LatestEvalRun(context.Background(), set.Name)
			if err := persistEvalRun(st, result); err != nil {
				fmt.Fprintf(os.Stderr, "warning: persist eval run: %v\n", err)
			}
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
	} else {
		printEvalReport(os.Stdout, result, baseline)
	}
	if result.Pass {
		return 0
	}
	return 1
}

func setNames(sets []evals.Set) string {
	if len(sets) == 0 {
		return "none"
	}
	names := make([]string, len(sets))
	for i := range sets {
		names[i] = sets[i].Name
	}
	return strings.Join(names, ", ")
}

func shortSHA(sha string) string {
	if len(sha) > 19 { // "sha256:" + 12
		return sha[:19]
	}
	if sha == "" {
		return "(unknown)"
	}
	return sha
}

// openEvalStorage opens the orchestrator DB from the same DSN resolution the
// server uses (FLEET_SCHED_DATABASE_URL / SCHED_DATABASE_URL / DATABASE_URL /
// DB_* parts). Returns (nil, nonzero) with a warning when no DSN is available.
func openEvalStorage() (*storage.Storage, int) {
	dsn := schedDSN()
	if dsn == "" && strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" &&
		strings.TrimSpace(os.Getenv("DB_HOST")) == "" {
		fmt.Fprintln(os.Stderr, "warning: no orchestrator DB configured (DATABASE_URL); skipping eval_runs persistence — use --no-db to silence")
		return nil, 1
	}
	st := storage.New()
	if err := st.Initialize(dsn, storage.DefaultPoolConfig()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: open orchestrator DB: %v; skipping eval_runs persistence\n", err)
		return nil, 1
	}
	return st, 0
}

func persistEvalRun(st *storage.Storage, r *evals.RunResult) error {
	cases, err := json.Marshal(r.Cases)
	if err != nil {
		return err
	}
	return st.AddEvalRun(context.Background(), &models.EvalRun{
		ID:          uuid.New(),
		EvalSet:     r.Set,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		BundleSHA:   r.BundleSHA,
		Total:       r.Total,
		Passed:      r.Passed,
		MeanScore:   r.MeanScore,
		Threshold:   r.Threshold,
		Pass:        r.Pass,
		CostUSD:     r.CostUSD,
		Results:     cases,
	})
}

func printEvalReport(w io.Writer, r *evals.RunResult, baseline *models.EvalRun) {
	fmt.Fprintf(w, "\nEval set %q — %d/%d passed (%.0f%%), mean score %.2f, threshold %.2f, cost $%.4f\n",
		r.Set, r.Passed, r.Total, 100*float64(r.Passed)/float64(r.Total), r.MeanScore, r.Threshold, r.CostUSD)
	for _, c := range r.Cases {
		mark := "✗"
		if c.Pass {
			mark = "✓"
		}
		fmt.Fprintf(w, "  %s %-32s score %.2f  $%.4f  %dms  %s\n", mark, c.Name, c.Score, c.CostUSD, c.DurationMS, c.Model)
		if c.Error != "" {
			fmt.Fprintf(w, "      run error: %s\n", c.Error)
		}
		for _, s := range c.Scorers {
			if !s.Pass {
				fmt.Fprintf(w, "      failed %s (%s)%s\n", s.Kind, s.Label, reasonSuffix(s.Reasoning))
			}
		}
	}
	if baseline != nil {
		delta := r.MeanScore - baseline.MeanScore
		note := ""
		if baseline.BundleSHA != r.BundleSHA {
			note = " (bundle content changed since baseline)"
		}
		fmt.Fprintf(w, "baseline %s: %d/%d passed, mean %.2f → delta %+.2f%s\n",
			baseline.StartedAt.Format("2006-01-02 15:04"), baseline.Passed, baseline.Total, baseline.MeanScore, delta, note)
	}
	if r.Pass {
		fmt.Fprintln(w, "RESULT: PASS")
	} else {
		fmt.Fprintln(w, "RESULT: FAIL (pass fraction below threshold)")
	}
}

func reasonSuffix(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	if len(reason) > 200 {
		reason = reason[:200] + "…"
	}
	return ": " + reason
}

// ── fleet eval list ──

func evalList(args []string) int {
	fs := flag.NewFlagSet("eval list", flag.ContinueOnError)
	bundlePath := fs.String("bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR)")
	jsonOut := fs.Bool("json", false, "emit as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bundle, _, err := loadEvalEnv(*bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval list: %v\n", err)
		return 1
	}
	sets, problems := evals.LoadSets(bundle.EvalsDir)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"sets": sets, "problems": problems})
		return 0
	}
	if len(sets) == 0 && len(problems) == 0 {
		fmt.Printf("no eval sets under %s\n", bundle.EvalsDir)
		return 0
	}
	for i := range sets {
		s := &sets[i]
		fmt.Printf("%-24s %2d case(s)  threshold %.2f  (evals/%s)\n", s.Name, len(s.Cases), s.EffectiveThreshold(), s.File)
	}
	for _, p := range problems {
		fmt.Printf("problem: %s\n", p)
	}
	return 0
}

// ── fleet eval history ──

func evalHistory(args []string) int {
	fs := flag.NewFlagSet("eval history", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "max runs to show")
	jsonOut := fs.Bool("json", false, "emit as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	setName := ""
	if fs.NArg() > 0 {
		setName = fs.Arg(0)
	}
	st, code := openEvalStorage()
	if st == nil {
		return code
	}
	runs, err := st.ListEvalRuns(context.Background(), setName, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval history: %v\n", err)
		return 1
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(runs)
		return 0
	}
	if len(runs) == 0 {
		fmt.Println("no eval runs recorded")
		return 0
	}
	for _, r := range runs {
		mark := "FAIL"
		if r.Pass {
			mark = "PASS"
		}
		fmt.Printf("%s  %-20s %s  %d/%d passed  mean %.2f  thr %.2f  $%.4f  %s\n",
			r.StartedAt.Format("2006-01-02 15:04"), r.EvalSet, mark, r.Passed, r.Total, r.MeanScore, r.Threshold, r.CostUSD, shortSHA(r.BundleSHA))
	}
	return 0
}

// ── fleet eval capture ──

const defaultCaptureRubric = "Does the answer satisfy the original request, matching the reference answer in substance (not wording)?"

func evalCapture(args []string) int {
	fs := flag.NewFlagSet("eval capture", flag.ContinueOnError)
	bundlePath := fs.String("bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR)")
	taskID := fs.String("task", "", "scheduled task UUID to capture")
	convID := fs.String("conversation", "", "chat conversation id to capture")
	user := fs.String("user", "", "conversation owner email (required with --conversation)")
	setName := fs.String("set", "", "eval set name to add the golden to (required)")
	caseName := fs.String("name", "", "case name (default: derived from the source)")
	modelOverride := fs.String("model", "", "pinned model slug (default: the source run's model)")
	rubric := fs.String("rubric", defaultCaptureRubric, "llm_judge rubric for the captured case")
	out := fs.String("out", "", "output file (default <bundle>/evals/<set>.yaml; '-' = stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*setName) == "" {
		fmt.Fprintln(os.Stderr, "fleet eval capture: --set is required")
		return 2
	}
	if (*taskID == "") == (*convID == "") {
		fmt.Fprintln(os.Stderr, "fleet eval capture: exactly one of --task or --conversation is required")
		return 2
	}

	bundle, cfg, err := loadEvalEnv(*bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval capture: %v\n", err)
		return 1
	}

	var c evals.Case
	if *taskID != "" {
		c, err = captureFromTask(*taskID)
	} else {
		c, err = captureFromConversation(cfg, *convID, *user)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval capture: %v\n", err)
		return 1
	}
	if *caseName != "" {
		c.Name = *caseName
	}
	if *modelOverride != "" {
		c.Model = *modelOverride
	}
	if strings.TrimSpace(c.Model) == "" {
		fmt.Fprintln(os.Stderr, "fleet eval capture: the source run pinned no model — pass --model <slug>")
		return 1
	}
	c.Scorers = []evals.ScorerSpec{{LLMJudge: &evals.JudgeSpec{Rubric: *rubric}}}

	dest := *out
	if dest == "" {
		dest = filepath.Join(bundle.EvalsDir, *setName+".yaml")
	}
	if err := writeCapturedCase(dest, *setName, c); err != nil {
		fmt.Fprintf(os.Stderr, "fleet eval capture: %v\n", err)
		return 1
	}
	if dest != "-" {
		fmt.Printf("captured %q into %s (review the scorers — the default is a single llm_judge rubric)\n", c.Name, dest)
	}
	return 0
}

func captureFromTask(id string) (evals.Case, error) {
	tid, err := uuid.Parse(id)
	if err != nil {
		return evals.Case{}, fmt.Errorf("--task: %w", err)
	}
	st, code := openEvalStorage()
	if st == nil || code != 0 {
		return evals.Case{}, errors.New("orchestrator DB required for --task capture (set DATABASE_URL)")
	}
	task, err := st.GetTask(tid)
	if err != nil {
		return evals.Case{}, fmt.Errorf("load task %s: %w", tid, err)
	}
	c := evals.Case{
		Name:    "task-" + tid.String()[:8],
		Prompt:  task.Prompt,
		Persona: task.Persona,
		Source:  "task:" + tid.String(),
	}
	if task.Model != nil {
		c.Model = *task.Model
	}
	// The task's LAST run output becomes the reference answer (the logs table
	// upserts per task, so only the latest run is capturable — capture soon
	// after the run you mean to bless).
	if session, err := st.GetLog(tid); err == nil {
		c.Expected = scorers.LastAssistantMessage(session)
	}
	return c, nil
}

func captureFromConversation(cfg *config.Config, id, user string) (evals.Case, error) {
	if strings.TrimSpace(user) == "" {
		return evals.Case{}, errors.New("--user is required with --conversation (conversations are user-scoped)")
	}
	chatStore, err := store.Open(chatDSN(cfg), store.DefaultPoolConfig())
	if err != nil {
		return evals.Case{}, fmt.Errorf("open chat DB: %w", err)
	}
	defer chatStore.Close()
	ctx := context.Background()
	conv, err := chatStore.Get(ctx, user, id)
	if err != nil {
		return evals.Case{}, fmt.Errorf("load conversation: %w", err)
	}
	if conv == nil {
		return evals.Case{}, fmt.Errorf("conversation %s not found for %s", id, user)
	}
	history, err := chatStore.LoadHistory(ctx, id)
	if err != nil {
		return evals.Case{}, fmt.Errorf("load history: %w", err)
	}
	prompt, expected := firstUserAndLastAssistantText(history)
	if strings.TrimSpace(prompt) == "" {
		return evals.Case{}, errors.New("conversation has no user text message to capture")
	}
	return evals.Case{
		Name:     "conv-" + shortID(id),
		Prompt:   prompt,
		Model:    conv.Model,
		Persona:  conv.Persona,
		Expected: expected,
		Source:   "conversation:" + id,
	}, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// firstUserAndLastAssistantText extracts the FIRST user text message (the
// golden's prompt — a multi-turn conversation replays as a one-shot; capture
// the turn you actually want to golden) and the LAST assistant text (the
// reference answer).
func firstUserAndLastAssistantText(history []agent.HistoryEntry) (prompt, expected string) {
	for _, h := range history {
		if h.Type != "text" {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		switch h.Role {
		case "user":
			if prompt == "" {
				prompt = tc.Text
			}
		case "assistant":
			expected = tc.Text
		}
	}
	return prompt, expected
}

// writeCapturedCase appends the case to an existing set file (or creates it).
// "-" writes a single-set YAML document to stdout.
func writeCapturedCase(dest, setName string, c evals.Case) error {
	set := evals.Set{Name: setName}
	if dest != "-" {
		if raw, err := os.ReadFile(dest); err == nil { // #nosec G304 — operator-chosen output path.
			if err := yaml.Unmarshal(raw, &set); err != nil {
				return fmt.Errorf("existing %s is not a valid eval set: %w", dest, err)
			}
			if set.Name == "" {
				set.Name = setName
			}
		}
	}
	for i := range set.Cases {
		if set.Cases[i].Name == c.Name {
			return fmt.Errorf("set already has a case named %q — pass --name", c.Name)
		}
	}
	set.Cases = append(set.Cases, c)
	raw, err := yaml.Marshal(&set)
	if err != nil {
		return err
	}
	if dest == "-" {
		_, err = os.Stdout.Write(raw)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, raw, 0o644) // #nosec G306 — eval definitions are not secrets.
}
