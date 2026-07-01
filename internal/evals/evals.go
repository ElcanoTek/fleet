// Package evals is the self-hosted eval & regression harness (#502): golden
// prompts captured from past runs are replayed through the ONE governed loop
// (agent.Manager.RunTurn → agentcore.Run) at their pinned model/persona and
// scored with the shared deterministic scorers (internal/scorers) plus an
// LLM-judge whose verdict is schema-validated (internal/structuredoutput).
//
// Eval DEFINITIONS are client content: they live in the external bundle's
// evals/ dir (ADR-0006), one YAML file per set. Eval RESULTS are operational
// data: they land in the orchestrator DB's eval_runs table. Graded data and
// judge rubrics never leave the box — the judge is the operator's own model
// routed through the same host-side resolver as every other run; there is no
// external grader.
package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Set is one eval set — a named group of golden cases replayed and gated
// together. One YAML file under the bundle's evals/ dir defines one set.
type Set struct {
	// Name identifies the set (`fleet eval run <name>`). Defaults to the file's
	// basename without extension when the `set:` key is absent.
	Name        string `yaml:"set"`
	Description string `yaml:"description,omitempty"`
	// Threshold is the minimum fraction of cases that must pass for the set to
	// pass (the CI gate). Absent = 1.0 (every case must pass).
	Threshold *float64 `yaml:"threshold,omitempty"`
	// JudgeModel is the default model slug for llm_judge scorers in this set.
	// A scorer's own `model` wins; when both are empty the judge runs on the
	// case's pinned run model.
	JudgeModel string `yaml:"judge_model,omitempty"`
	Cases      []Case `yaml:"cases"`

	// File is the source filename (relative to evals/), set by LoadSets.
	File string `yaml:"-"`
}

// EffectiveThreshold returns the set's pass-fraction gate, defaulting to 1.0.
func (s *Set) EffectiveThreshold() float64 {
	if s.Threshold == nil {
		return 1.0
	}
	return *s.Threshold
}

// Case is one golden: a prompt replayed at a pinned config. The model is
// pinned (that is what a model-swap eval compares); the persona/system-prompt
// CONTENT deliberately is not — it resolves from the live bundle at replay
// time, so a bundle edit is exactly what the eval detects.
type Case struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	// Model is the pinned model slug the golden replays at. Required.
	Model string `yaml:"model"`
	// Persona is the bundle persona name (e.g. "assistant"). Empty = the
	// server's default persona, exactly like a chat turn.
	Persona string `yaml:"persona,omitempty"`
	// Expected is an optional reference answer handed to llm_judge scorers.
	Expected string       `yaml:"expected,omitempty"`
	Scorers  []ScorerSpec `yaml:"scorers"`
	// Source records provenance when the case was captured from a past run
	// ("task:<uuid>" / "conversation:<id>"). Informational only.
	Source string `yaml:"source,omitempty"`
}

// ScorerSpec declares exactly ONE scorer. The deterministic kinds map onto
// internal/scorers; llm_judge is the rubric-scored model judge.
type ScorerSpec struct {
	Contains string     `yaml:"contains,omitempty"`
	Regex    string     `yaml:"regex,omitempty"`
	Equals   string     `yaml:"equals,omitempty"`
	LLMJudge *JudgeSpec `yaml:"llm_judge,omitempty"`
}

// Kind returns the scorer's kind name, or "" when none/multiple are set.
func (sp *ScorerSpec) Kind() string {
	kinds := make([]string, 0, 1)
	if sp.Contains != "" {
		kinds = append(kinds, "contains")
	}
	if sp.Regex != "" {
		kinds = append(kinds, "regex")
	}
	if sp.Equals != "" {
		kinds = append(kinds, "equals")
	}
	if sp.LLMJudge != nil {
		kinds = append(kinds, "llm_judge")
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// JudgeSpec configures one llm_judge scorer.
type JudgeSpec struct {
	// Rubric is the grading instruction ("Does the answer correctly …?").
	Rubric string `yaml:"rubric"`
	// Model overrides the set's judge_model for this scorer.
	Model string `yaml:"model,omitempty"`
	// MinScore is the pass bar on the judge's 0..1 score. Absent = 0.7.
	MinScore *float64 `yaml:"min_score,omitempty"`
}

// EffectiveMinScore returns the judge pass bar, defaulting to 0.7.
func (j *JudgeSpec) EffectiveMinScore() float64 {
	if j.MinScore == nil {
		return 0.7
	}
	return *j.MinScore
}

// LoadSets reads every *.yaml / *.yml directly under evalsDir and returns the
// well-formed sets (sorted by name) plus a human-readable problem per
// malformed file/set — the skills-loader contract (clientconfig.ReadSkills):
// an absent or empty evals/ dir is NOT a problem, and a malformed set is
// excluded rather than half-loaded, so `fleet eval run` can never gate on a
// definition it only partially understood.
func LoadSets(evalsDir string) (sets []Set, problems []string) {
	if strings.TrimSpace(evalsDir) == "" {
		return nil, nil
	}
	info, err := os.Stat(evalsDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(evalsDir)
	if err != nil {
		return nil, []string{fmt.Sprintf("evals: cannot read %s: %v", evalsDir, err)}
	}
	seen := map[string]string{} // set name → file, to reject duplicates
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(evalsDir, name)) // #nosec G304 — operator-supplied bundle path.
		if err != nil {
			problems = append(problems, fmt.Sprintf("evals/%s: %v", name, err))
			continue
		}
		var set Set
		if err := yaml.UnmarshalWithOptions(raw, &set, yaml.Strict()); err != nil {
			problems = append(problems, fmt.Sprintf("evals/%s: invalid YAML: %v", name, err))
			continue
		}
		if strings.TrimSpace(set.Name) == "" {
			set.Name = strings.TrimSuffix(name, ext)
		}
		set.File = name
		if prev, dup := seen[set.Name]; dup {
			problems = append(problems, fmt.Sprintf("evals/%s: duplicate set name %q (already defined in evals/%s)", name, set.Name, prev))
			continue
		}
		if probs := validateSet(&set); len(probs) > 0 {
			for _, p := range probs {
				problems = append(problems, fmt.Sprintf("evals/%s: %s", name, p))
			}
			continue
		}
		seen[set.Name] = name
		sets = append(sets, set)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })
	return sets, problems
}

// validateSet returns every problem that makes a set unrunnable. A set with
// any problem is excluded from the roster — a gate must not run on a
// definition it only partially parsed.
func validateSet(set *Set) (problems []string) {
	if set.Threshold != nil && (*set.Threshold < 0 || *set.Threshold > 1) {
		problems = append(problems, fmt.Sprintf("threshold %v is outside [0,1]", *set.Threshold))
	}
	if len(set.Cases) == 0 {
		problems = append(problems, "set has no cases")
	}
	names := map[string]bool{}
	for i := range set.Cases {
		c := &set.Cases[i]
		where := fmt.Sprintf("case %d (%q)", i+1, c.Name)
		if strings.TrimSpace(c.Name) == "" {
			problems = append(problems, fmt.Sprintf("case %d: missing name", i+1))
		} else if names[c.Name] {
			problems = append(problems, fmt.Sprintf("%s: duplicate case name", where))
		}
		names[c.Name] = true
		if strings.TrimSpace(c.Prompt) == "" {
			problems = append(problems, fmt.Sprintf("%s: missing prompt", where))
		}
		if strings.TrimSpace(c.Model) == "" {
			problems = append(problems, fmt.Sprintf("%s: missing model (a golden pins the model it replays at)", where))
		}
		if len(c.Scorers) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no scorers (an unscored case cannot gate anything)", where))
		}
		for j := range c.Scorers {
			sp := &c.Scorers[j]
			kind := sp.Kind()
			if kind == "" {
				problems = append(problems, fmt.Sprintf("%s scorer %d: exactly one of contains/regex/equals/llm_judge must be set", where, j+1))
				continue
			}
			if sp.Regex != "" {
				if _, err := regexp.Compile(sp.Regex); err != nil {
					problems = append(problems, fmt.Sprintf("%s scorer %d: invalid regex: %v", where, j+1, err))
				}
			}
			if sp.LLMJudge != nil {
				if strings.TrimSpace(sp.LLMJudge.Rubric) == "" {
					problems = append(problems, fmt.Sprintf("%s scorer %d: llm_judge needs a rubric", where, j+1))
				}
				if sp.LLMJudge.MinScore != nil && (*sp.LLMJudge.MinScore < 0 || *sp.LLMJudge.MinScore > 1) {
					problems = append(problems, fmt.Sprintf("%s scorer %d: min_score %v is outside [0,1]", where, j+1, *sp.LLMJudge.MinScore))
				}
			}
		}
	}
	return problems
}

// FindSet returns the named set from sets, or nil.
func FindSet(sets []Set, name string) *Set {
	for i := range sets {
		if sets[i].Name == name {
			return &sets[i]
		}
	}
	return nil
}
