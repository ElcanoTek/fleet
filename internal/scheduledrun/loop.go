package scheduledrun

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/scorers"
)

// Iterative verification loops (#179). A task with a LoopConfig runs its worker
// agent to completion, evaluates an exit condition, and — if it fails and
// budget remains — re-runs the worker with the prior output fed forward, up to
// MaxIterations. Each iteration is the SAME governed worker pass an ordinary
// scheduled task uses (runWorker → agentcore.Run); the loop only adds the
// verify/retry control around it, so "governance is one core" holds per cycle.

const (
	// shellExitTimeout bounds a shell: exit-condition command.
	shellExitTimeout = 2 * time.Minute
	// llmExitTimeout bounds an llm: verifier model call.
	llmExitTimeout = 2 * time.Minute
)

// workerFunc runs one worker pass with the prior iteration's output fed forward,
// returning the session, whether the exit condition passed, its result label,
// and any run error. runWithLoop binds it to r.runWorker; tests substitute a fake.
type workerFunc func(ctx context.Context, extraPrompt string) (*models.LogSession, bool, string, error)

// runWithLoop drives the worker+verify loop for a task whose LoopConfig is set,
// binding the live worker + telemetry sink to the testable runLoop core.
func (r *Runner) runWithLoop(ctx context.Context, task *models.Task, wtPath string) (*models.LogSession, error) {
	worker := func(c context.Context, extra string) (*models.LogSession, bool, string, error) {
		return r.runWorker(c, task, extra, task.LoopConfig, wtPath)
	}
	return runLoop(ctx, task.LoopConfig, task.ID, time.Now, worker, func(it *models.TaskIteration) {
		r.recordIteration(ctx, it)
	})
}

// runLoop is the loop control, decoupled from model/sandbox setup so it is
// unit-testable with a fake worker + clock. It returns the last session and nil
// on a passing exit, or the last session and an error when the loop is
// exhausted, cancelled, or hits the time / cost ceiling. Telemetry is recorded
// best-effort via `record` (never fails the run).
func runLoop(
	ctx context.Context,
	lc *models.LoopConfig,
	taskID uuid.UUID,
	now func() time.Time,
	worker workerFunc,
	record func(*models.TaskIteration),
) (*models.LogSession, error) {
	maxIter := lc.MaxIterations
	if maxIter <= 0 {
		maxIter = models.DefaultLoopMaxIterations
	}
	var deadline time.Time
	if lc.TimeBudgetSeconds > 0 {
		deadline = now().Add(time.Duration(lc.TimeBudgetSeconds) * time.Second)
	}

	stopped := func(n int, reason string) {
		t := now().UTC()
		record(&models.TaskIteration{
			TaskID: taskID, IterationNumber: n, StartedAt: t, CompletedAt: &t,
			Status: models.IterationStatusStopped, ExitConditionResult: reason,
		})
	}

	var (
		lastSession *models.LogSession
		priorOutput string
		accumCost   float64
	)
	for i := 1; i <= maxIter; i++ {
		// Cancellation (Stop / shutdown / deadline) aborts the loop; the partial
		// transcript so far is returned with the cancellation error.
		if ctx.Err() != nil {
			return lastSession, ctx.Err()
		}
		// Time budget + cost ceiling are checked BEFORE starting an iteration, so
		// already-accrued cost from a crashed/partial iteration still counts
		// (mirrors the per-run ceiling, applied across runs).
		if !deadline.IsZero() && now().After(deadline) {
			stopped(i, "time_budget_exceeded")
			return lastSession, fmt.Errorf("loop time budget (%ds) exceeded after %d iteration(s)", lc.TimeBudgetSeconds, i-1)
		}
		if lc.MaxCostUSD > 0 && accumCost >= lc.MaxCostUSD {
			stopped(i, "cost_ceiling_exceeded")
			return lastSession, fmt.Errorf("loop cost ceiling ($%.4f) reached after %d iteration(s) ($%.4f accrued)", lc.MaxCostUSD, i-1, accumCost)
		}

		it := &models.TaskIteration{
			TaskID:          taskID,
			IterationNumber: i,
			StartedAt:       now().UTC(),
			Status:          models.IterationStatusRunning,
		}
		record(it)

		session, passed, result, err := worker(ctx, priorOutput)
		lastSession = session
		t := now().UTC()
		it.CompletedAt = &t
		it.ExitConditionResult = result
		if session != nil {
			it.WorkerSessionID = session.Title
			it.CostUSD = session.Cost
			it.PromptTokens = int64(session.PromptTokens)
			it.CompletionTokens = int64(session.CompletionTokens)
			accumCost += session.Cost
		}

		if err != nil {
			it.Status = models.IterationStatusFailed
			if it.ExitConditionResult == "" {
				it.ExitConditionResult = "worker_error"
			}
			record(it)
			return session, err
		}
		if passed {
			it.Status = models.IterationStatusPassed
			record(it)
			log.Printf("scheduled loop task %s: passed on iteration %d/%d (%s)", taskID, i, maxIter, result)
			return session, nil
		}
		it.Status = models.IterationStatusFailed
		record(it)
		priorOutput = lastAssistantMessage(session)
		log.Printf("scheduled loop task %s: iteration %d/%d did not pass (%s); retrying", taskID, i, maxIter, result)
	}
	return lastSession, fmt.Errorf("loop exhausted %d iteration(s) without passing exit condition %q", maxIter, lc.ExitCondition)
}

// recordIteration upserts a telemetry row best-effort. It uses a cancel-detached
// context so the row still persists when the loop is being torn down by a
// cancellation. nil iterationStore = telemetry disabled.
func (r *Runner) recordIteration(ctx context.Context, it *models.TaskIteration) {
	if r.iterationStore == nil {
		return
	}
	if err := r.iterationStore.AddTaskIteration(context.WithoutCancel(ctx), it); err != nil {
		log.Printf("scheduled loop: failed to record iteration %d for task %s: %v", it.IterationNumber, it.TaskID, err)
	}
}

// evaluateExitCondition decides whether an iteration passed. The shell: form
// runs in the live worker sandbox; regex: matches the worker's last assistant
// message; llm asks the verifier model a YES/NO. The returned label is recorded
// as the iteration's exit_condition_result.
func (r *Runner) evaluateExitCondition(ctx context.Context, lc *models.LoopConfig, sb *sandbox.Sandbox, session *models.LogSession, fallback fantasy.LanguageModel) (bool, string) {
	cond := strings.TrimSpace(lc.ExitCondition)
	switch {
	case strings.HasPrefix(cond, "shell:"):
		return r.evalShellExit(ctx, sb, strings.TrimSpace(strings.TrimPrefix(cond, "shell:")))
	case strings.HasPrefix(cond, "regex:"):
		return evalRegexExit(strings.TrimPrefix(cond, "regex:"), lastAssistantMessage(session))
	case cond == "llm":
		return r.evalLLMExit(ctx, lc, session, fallback)
	default:
		log.Printf("scheduled loop: unknown exit_condition %q; treating as not-passed", cond)
		return false, "unknown_exit_condition"
	}
}

// evalShellExit / evalRegexExit delegate to the shared internal/scorers
// implementations (#502 extracted them so the eval harness judges output the
// same way loop exit-conditions do). The nil-sandbox guard stays HERE: a typed
// nil *sandbox.Sandbox inside the scorers.BashRunner interface would defeat a
// nil check on the other side.
func (r *Runner) evalShellExit(ctx context.Context, sb *sandbox.Sandbox, cmd string) (bool, string) {
	if sb == nil {
		return false, "shell:no_sandbox"
	}
	return scorers.Shell(ctx, sb, cmd, shellExitTimeout)
}

func evalRegexExit(pattern, text string) (bool, string) {
	return scorers.Regex(pattern, text)
}

func (r *Runner) evalLLMExit(ctx context.Context, lc *models.LoopConfig, session *models.LogSession, fallback fantasy.LanguageModel) (bool, string) {
	verifier := fallback
	if slug := strings.TrimSpace(lc.VerifierModel); slug != "" {
		if m, err := r.mgr.Resolve(ctx, slug); err == nil {
			verifier = m
		} else {
			log.Printf("scheduled loop: verifier model %q unresolved (%v); using fallback", slug, err)
		}
	}
	if verifier == nil {
		return false, "llm:no_model"
	}
	prompt := strings.TrimSpace(lc.VerifierPrompt)
	if prompt == "" {
		prompt = "Did the worker complete the task successfully?"
	}
	user := fmt.Sprintf("%s\n\n---\nWorker's final output:\n%s", prompt, lastAssistantMessage(session))

	// The verifier is a single bounded YES/NO call per iteration. Its spend is NOT
	// separately metered into the iteration cost / the across-iteration MaxCostUSD
	// ceiling — matching #179's accounting model, which is the (dominant) worker
	// session.Cost. The full worker pass (the cost that matters) is always counted.
	vctx, cancel := context.WithTimeout(ctx, llmExitTimeout)
	defer cancel()
	verifyAgent := fantasy.NewAgent(verifier, fantasy.WithSystemPrompt(
		"You are a strict verifier judging whether a worker agent satisfied a requirement. "+
			"Reply with a single word: YES if it is satisfied, otherwise NO."))
	out, err := verifyAgent.Generate(vctx, fantasy.AgentCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage(user)},
	})
	if err != nil {
		log.Printf("scheduled loop: llm verifier call failed: %v", err)
		return false, "llm:error"
	}
	if firstWordIsYes(out.Response.Content.Text()) {
		return true, "llm:YES"
	}
	return false, "llm:NO"
}

// firstWordIsYes reports whether the verifier's reply begins with YES (ignoring
// case, leading whitespace, and a leading markdown/quote/backtick character).
func firstWordIsYes(s string) bool {
	return scorers.FirstWordIsYes(s)
}

// lastAssistantMessage returns the text of the worker's final assistant message,
// which a regex: / llm exit condition judges and which is fed forward as context
// to the next iteration.
func lastAssistantMessage(session *models.LogSession) string {
	return scorers.LastAssistantMessage(session)
}
