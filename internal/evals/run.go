package evals

import (
	"context"
	"fmt"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/scorers"
)

// The replay engine. Each golden runs through agent.Manager.RunTurn — the SAME
// governed interactive entrypoint a live chat turn uses (RunInteractiveTurn →
// agentcore.Run) — so a replay inherits the mandatory sandbox, the cost/token
// ceilings, secret redaction, and the persona/prompt composition from the LIVE
// bundle for free. No second, weaker run path exists for evals.

// TurnRunner is the slice of *agent.Manager the engine needs: one governed
// turn per case, plus the shared host-side model resolver for the judge.
type TurnRunner interface {
	RunTurn(ctx context.Context, in agent.TurnInput, sink agent.EventSink) (*agent.TurnResult, error)
	Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error)
}

// Options tune one RunSet invocation.
type Options struct {
	// RunID scopes each case's workspace/conversation id ("eval-<RunID>-<n>").
	// Required and unique per invocation so cases can't see each other's files.
	RunID string
	// BundleSHA is the replayed bundle's content fingerprint (BundleFingerprint),
	// recorded on the result for regression comparison across bundle edits.
	BundleSHA string
	// Progress, when non-nil, receives one line per case as it completes.
	Progress func(format string, args ...any)
}

// ScorerResult is one scorer's verdict on one case.
type ScorerResult struct {
	Kind string `json:"kind"`
	Pass bool   `json:"pass"`
	// Label is the short machine-readable verdict (the internal/scorers label
	// contract, e.g. "regex:matched", or "judge:0.85" / "judge:error").
	Label string  `json:"label"`
	Score float64 `json:"score"`
	// Reasoning carries the judge's explanation (llm_judge only).
	Reasoning string `json:"reasoning,omitempty"`
}

// CaseResult is one golden's replay outcome.
type CaseResult struct {
	Name    string         `json:"name"`
	Pass    bool           `json:"pass"`
	Score   float64        `json:"score"`
	Scorers []ScorerResult `json:"scorers,omitempty"`
	// Error is set when the replay itself failed (model unresolvable, run
	// error); the case fails with no scorer verdicts.
	Error            string  `json:"error,omitempty"`
	Model            string  `json:"model"`
	CostUSD          float64 `json:"cost_usd"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	DurationMS       int64   `json:"duration_ms"`
	// Output is the replay's final answer, clamped for storage.
	Output string `json:"output,omitempty"`
}

// RunResult aggregates one set replay — the row persisted to eval_runs.
type RunResult struct {
	Set         string       `json:"set"`
	RunID       string       `json:"run_id"`
	BundleSHA   string       `json:"bundle_sha,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	CompletedAt time.Time    `json:"completed_at"`
	Total       int          `json:"total"`
	Passed      int          `json:"passed"`
	MeanScore   float64      `json:"mean_score"`
	Threshold   float64      `json:"threshold"`
	Pass        bool         `json:"pass"`
	CostUSD     float64      `json:"cost_usd"`
	Cases       []CaseResult `json:"cases"`
}

// maxStoredOutputChars clamps each case's stored output so a verbose replay
// can't bloat the eval_runs row (the full transcript is not the eval's job).
const maxStoredOutputChars = 8000

// noopSink discards run events — the eval CLI has no SSE stream to feed.
type noopSink struct{}

func (noopSink) Emit(string, any) {}

// RunSet replays every case in the set sequentially and scores the outputs.
// It returns an error only for invocation-level problems (empty set / missing
// RunID); a failing CASE is a result, not an error, so the CLI can report the
// full table and exit non-zero on the gate.
func RunSet(ctx context.Context, tr TurnRunner, set *Set, opts Options) (*RunResult, error) {
	if set == nil || len(set.Cases) == 0 {
		return nil, fmt.Errorf("eval set is empty")
	}
	if opts.RunID == "" {
		return nil, fmt.Errorf("options: RunID is required")
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}

	res := &RunResult{
		Set:       set.Name,
		RunID:     opts.RunID,
		BundleSHA: opts.BundleSHA,
		StartedAt: time.Now().UTC(),
		Total:     len(set.Cases),
		Threshold: set.EffectiveThreshold(),
	}

	var scoreSum float64
	for i := range set.Cases {
		c := &set.Cases[i]
		cr := runCase(ctx, tr, set, c, fmt.Sprintf("eval-%s-%d", opts.RunID, i+1))
		res.Cases = append(res.Cases, cr)
		res.CostUSD += cr.CostUSD
		scoreSum += cr.Score
		if cr.Pass {
			res.Passed++
		}
		status := "FAIL"
		if cr.Pass {
			status = "PASS"
		}
		progress("  [%d/%d] %-30s %s (score %.2f, $%.4f)", i+1, res.Total, c.Name, status, cr.Score, cr.CostUSD)
		// A cancelled context fails the remaining cases fast rather than
		// half-running the set with misleading per-case errors.
		if ctx.Err() != nil && i < len(set.Cases)-1 {
			return nil, ctx.Err()
		}
	}

	res.CompletedAt = time.Now().UTC()
	res.MeanScore = scoreSum / float64(res.Total)
	res.Pass = float64(res.Passed)/float64(res.Total) >= res.Threshold
	return res, nil
}

// runCase replays one golden and applies its scorers. The case passes only
// when the replay succeeded AND every scorer passed.
func runCase(ctx context.Context, tr TurnRunner, set *Set, c *Case, convID string) CaseResult {
	cr := CaseResult{Name: c.Name, Model: c.Model}
	start := time.Now()

	turn, err := tr.RunTurn(ctx, agent.TurnInput{
		UserMessage:    c.Prompt,
		Persona:        c.Persona,
		Model:          c.Model,
		ConversationID: convID,
	}, noopSink{})
	cr.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.CostUSD = turn.CostUSD
	cr.PromptTokens = turn.PromptTokens
	cr.CompletionTokens = turn.CompletionTokens
	cr.Output = clamp(turn.FinalText, maxStoredOutputChars)

	actual := turn.FinalText
	allPass := true
	var scoreSum float64
	for i := range c.Scorers {
		sr := applyScorer(ctx, tr, set, c, &c.Scorers[i], actual)
		cr.Scorers = append(cr.Scorers, sr)
		scoreSum += sr.Score
		if !sr.Pass {
			allPass = false
		}
	}
	if n := len(cr.Scorers); n > 0 {
		cr.Score = scoreSum / float64(n)
	}
	cr.Pass = allPass && len(cr.Scorers) > 0
	return cr
}

// applyScorer evaluates one scorer spec against the replay output. Judge
// failures score 0 and FAIL the scorer (fail-closed): a regression gate must
// not pass on a grade it never got.
func applyScorer(ctx context.Context, tr TurnRunner, set *Set, c *Case, sp *ScorerSpec, actual string) ScorerResult {
	switch sp.Kind() {
	case "contains":
		pass, label := scorers.Contains(sp.Contains, actual)
		return deterministicResult("contains", pass, label)
	case "regex":
		pass, label := scorers.Regex(sp.Regex, actual)
		return deterministicResult("regex", pass, label)
	case "equals":
		pass, label := scorers.Equals(sp.Equals, actual)
		return deterministicResult("equals", pass, label)
	case "llm_judge":
		slug := sp.LLMJudge.Model
		if slug == "" {
			slug = set.JudgeModel
		}
		if slug == "" {
			slug = c.Model
		}
		v, err := RunJudge(ctx, tr, slug, sp.LLMJudge.Rubric, c.Prompt, c.Expected, actual)
		if err != nil {
			return ScorerResult{Kind: "llm_judge", Pass: false, Label: "judge:error", Score: 0, Reasoning: err.Error()}
		}
		pass := v.Pass && v.Score >= sp.LLMJudge.EffectiveMinScore()
		return ScorerResult{
			Kind:      "llm_judge",
			Pass:      pass,
			Label:     fmt.Sprintf("judge:%.2f", v.Score),
			Score:     v.Score,
			Reasoning: v.Reasoning,
		}
	default:
		// Unreachable for loader-validated sets; fail closed for hand-built ones.
		return ScorerResult{Kind: "invalid", Pass: false, Label: "scorer:invalid", Score: 0}
	}
}

func deterministicResult(kind string, pass bool, label string) ScorerResult {
	score := 0.0
	if pass {
		score = 1.0
	}
	return ScorerResult{Kind: kind, Pass: pass, Label: label, Score: score}
}

func clamp(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n…[truncated]"
}
