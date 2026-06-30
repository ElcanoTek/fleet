package tools

// LLM-powered git-metadata tools (#191). suggest_branch_name,
// suggest_commit_message, and suggest_pr_description let an agent self-document
// the code it produces — a git-safe branch name, a Conventional-Commits message,
// and a ready-to-paste PR description — by asking the operator-configured
// fast/cheap metadata model (FLEET_METADATA_MODEL, defaulting to the title
// model). They mirror the SuggestTitle pattern in internal/agent/title.go: a
// short-lived fantasy.NewAgent call with a tight system prompt and a 20s timeout,
// resolved through the SAME host-side ModelResolver the run already uses — so the
// operator's key never leaves the host and the call rides the shared, governed
// provider rather than a bare HTTP request.
//
// The agent can pass garbage, and a model can hallucinate bad output, so every
// tool VALIDATES the model response before returning it, via pure functions
// (normalizeBranchName / normalizeCommitMessage / normalizePRDescription) that
// are unit-tested without any live model. The strength of the guarantee differs
// by tool: a branch name is constrained to a strict git-safe character class
// (so it is never an unsafe ref handed to git), whereas a commit message BODY
// and a PR description are returned as free-form text/markdown after only their
// HEADER (Conventional-Commits subject / "## Summary" heading) is validated.
// That is safe because these outputs are consumed by the agent inside the
// mandatory sandbox (it composes any `git` invocation itself via the bash tool),
// never interpolated into a host-side shell here.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
)

// ModelResolver resolves a model slug to a language model. It is the minimal
// surface these tools need from the run's resolver; *agentcore.ModelResolver and
// *agent.Manager both satisfy it structurally. Declared here (rather than
// imported) so the tools package stays free of an agentcore import cycle.
type ModelResolver interface {
	Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error)
}

// Canonical tool names, exported so orchestration/tests can reference them
// without re-typing the literal.
const (
	SuggestBranchNameToolName    = "suggest_branch_name"
	SuggestCommitMessageToolName = "suggest_commit_message"
	SuggestPRDescriptionToolName = "suggest_pr_description"
)

const (
	// metadataTimeout bounds a single metadata generation, matching SuggestTitle.
	metadataTimeout = 20 * time.Second
	// Per-tool output ceilings: a branch name is tiny, a commit message a few
	// lines, a PR description a short markdown doc. Tight ceilings keep the call
	// cheap and bound its token cost.
	branchNameMaxTokens    = 64
	commitMessageMaxTokens = 512
	prDescriptionMaxTokens = 1024
	// maxCommitSubjectLen is the Conventional Commits subject-line ceiling.
	maxCommitSubjectLen = 72
)

// MetadataTools returns the three #191 git-metadata tools wired to resolver +
// model. The scheduled driver appends these to its native set (see
// internal/scheduledrun). A nil resolver is tolerated — the tools then refuse at
// call time with a clear error rather than panicking.
func MetadataTools(resolver ModelResolver, model string) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		NewSuggestBranchNameTool(resolver, model),
		NewSuggestCommitMessageTool(resolver, model),
		NewSuggestPRDescriptionTool(resolver, model),
	}
}

// ── suggest_branch_name ──

// SuggestBranchNameParams is the agent-facing payload.
type SuggestBranchNameParams struct {
	Context string `json:"context" description:"What the branch is for — describe the change in plain English. E.g. 'adds OAuth2 login for the web app'."`
}

const suggestBranchNameDescription = `Generate a git-safe branch name (max 60 chars, lowercase, hyphen-separated, '<type>/<slug>') for the described change. Call this before creating a branch so the name is descriptive and follows project conventions. Returns an error if the model produces an unsafe name — re-describe the change more concretely and retry.`

const branchNameSysPrompt = `You generate git branch names. Given a description of a code change, output ONLY a single branch name.
Rules:
- Format: <type>/<short-slug>  where type is one of: feat, fix, chore, docs, refactor, test, ci
- Lowercase letters, digits, a single forward-slash separator, and hyphens only
- Max 60 characters total
- No quotes, no explanation, no trailing punctuation, no leading/trailing whitespace`

// branchNameRe is the git-safe branch-name contract: a leading lowercase letter
// then 1-59 of [a-z0-9/-] (total length 2-60). It is matched in Go (never sent
// to a model schema), so the literal escaping does not interact with the OpenAI
// pattern sanitizer.
var branchNameRe = regexp.MustCompile(`^[a-z][a-z0-9/-]{1,59}$`)

// NewSuggestBranchNameTool returns the suggest_branch_name tool.
func NewSuggestBranchNameTool(resolver ModelResolver, model string) fantasy.AgentTool {
	return fantasy.NewAgentTool(SuggestBranchNameToolName, suggestBranchNameDescription,
		func(ctx context.Context, params SuggestBranchNameParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			raw, err := generateMetadata(ctx, resolver, model, branchNameSysPrompt, params.Context, branchNameMaxTokens)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestBranchNameToolName, err)), nil
			}
			name, err := normalizeBranchName(raw)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestBranchNameToolName, err)), nil
			}
			return fantasy.NewTextResponse(name), nil
		})
}

// normalizeBranchName cleans a raw model response into a git-safe branch name,
// or returns an error when the output cannot be made safe. Pure — no I/O.
func normalizeBranchName(raw string) (string, error) {
	s := firstNonEmptyLine(raw)
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`'\".,;:"))
	s = strings.ToLower(s)
	if !branchNameRe.MatchString(s) {
		return "", fmt.Errorf("model returned an invalid branch name %q", strings.TrimSpace(raw))
	}
	// The character-class regex still permits a few shapes git itself refuses
	// (a ref cannot end in "/" or contain "//"). Reject them here so we never
	// hand git a name it will bounce.
	if strings.HasSuffix(s, "/") || strings.Contains(s, "//") {
		return "", fmt.Errorf("model returned a git-invalid branch name %q", strings.TrimSpace(raw))
	}
	return s, nil
}

// ── suggest_commit_message ──

// SuggestCommitMessageParams is the agent-facing payload.
type SuggestCommitMessageParams struct {
	DiffSummary string `json:"diff_summary" description:"A plain-English summary of what changed — files touched, why, and the net effect. Does not need to be a literal diff."`
	Context     string `json:"context" description:"Optional additional context: ticket number, PR goal, related issues."`
}

const suggestCommitMessageDescription = `Generate a Conventional Commits message (subject <type>(<scope>): <subject> on the first line, max 72 chars, then a blank line and a body) for the described change. Call this before committing. Returns an error if the model produces a non-conforming subject — re-summarize and retry.`

const commitMessageSysPrompt = `You write git commit messages following the Conventional Commits spec.

Output format:
<type>(<optional scope>): <subject>   ← max 72 characters including type and scope

<body>   ← plain paragraphs, wrapped at 72 chars; explain WHY, not just WHAT

Rules:
- Valid types: feat, fix, docs, chore, refactor, test, perf, ci, build, revert
- Subject line must be <= 72 characters
- Do NOT include a "Co-authored-by" line
- No markdown, no bullet points in the subject
- Output ONLY the commit message — no preamble, no explanation, no code fences`

// commitSubjectRe validates a Conventional Commits subject header:
// type, optional (scope), optional ! breaking marker, ": ", then text.
var commitSubjectRe = regexp.MustCompile(`^(feat|fix|docs|chore|refactor|test|perf|ci|build|revert)(\([^)]+\))?!?: .+`)

// NewSuggestCommitMessageTool returns the suggest_commit_message tool.
func NewSuggestCommitMessageTool(resolver ModelResolver, model string) fantasy.AgentTool {
	return fantasy.NewAgentTool(SuggestCommitMessageToolName, suggestCommitMessageDescription,
		func(ctx context.Context, params SuggestCommitMessageParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			prompt := params.DiffSummary
			if c := strings.TrimSpace(params.Context); c != "" {
				prompt = prompt + "\n\nAdditional context: " + c
			}
			raw, err := generateMetadata(ctx, resolver, model, commitMessageSysPrompt, prompt, commitMessageMaxTokens)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestCommitMessageToolName, err)), nil
			}
			msg, err := normalizeCommitMessage(raw)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestCommitMessageToolName, err)), nil
			}
			return fantasy.NewTextResponse(msg), nil
		})
}

// normalizeCommitMessage validates and cleans a raw model response into a
// Conventional Commits message, or returns an error. Pure — no I/O.
func normalizeCommitMessage(raw string) (string, error) {
	s := stripCodeFence(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("model returned an empty commit message")
	}
	subject := strings.TrimSpace(firstNonEmptyLine(s))
	if n := utf8.RuneCountInString(subject); n > maxCommitSubjectLen {
		return "", fmt.Errorf("commit subject is %d chars, exceeds the %d-char limit: %q", n, maxCommitSubjectLen, subject)
	}
	if !commitSubjectRe.MatchString(subject) {
		return "", fmt.Errorf("commit subject %q is not a valid Conventional Commits header (expected '<type>(<scope>): <subject>')", subject)
	}
	return s, nil
}

// ── suggest_pr_description ──

// SuggestPRDescriptionParams is the agent-facing payload.
type SuggestPRDescriptionParams struct {
	Changes string `json:"changes" description:"Summary of what changed: which files, what they do, and the net user-visible or operator-visible effect."`
	Context string `json:"context" description:"Optional context: linked issue, motivation, deployment notes."`
}

const suggestPRDescriptionDescription = `Generate a GitHub pull-request description in Markdown with ## Summary, ## Changes, and ## Testing sections for the described change. Call this when opening a PR. Returns an error if the model omits the required Summary section.`

const prDescriptionSysPrompt = `You write GitHub pull request descriptions in Markdown.

Output exactly this structure (keep the headings verbatim):

## Summary
<1-3 bullet points — what and why>

## Changes
<bulleted list of notable files or components modified and what changed in each>

## Testing
<bulleted markdown checklist of what a reviewer should verify manually or via CI>

Rules:
- Output ONLY the Markdown — no preamble, no code fences around the whole block
- Keep each section tight; omit a section only if it would be genuinely empty`

// NewSuggestPRDescriptionTool returns the suggest_pr_description tool.
func NewSuggestPRDescriptionTool(resolver ModelResolver, model string) fantasy.AgentTool {
	return fantasy.NewAgentTool(SuggestPRDescriptionToolName, suggestPRDescriptionDescription,
		func(ctx context.Context, params SuggestPRDescriptionParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			prompt := params.Changes
			if c := strings.TrimSpace(params.Context); c != "" {
				prompt = prompt + "\n\nAdditional context: " + c
			}
			raw, err := generateMetadata(ctx, resolver, model, prDescriptionSysPrompt, prompt, prDescriptionMaxTokens)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestPRDescriptionToolName, err)), nil
			}
			desc, err := normalizePRDescription(raw)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v", SuggestPRDescriptionToolName, err)), nil
			}
			return fantasy.NewTextResponse(desc), nil
		})
}

// normalizePRDescription strips an enclosing code fence and verifies the
// required ## Summary section is present, or returns an error. Pure — no I/O.
func normalizePRDescription(raw string) (string, error) {
	s := stripCodeFence(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("model returned an empty PR description")
	}
	if !prSummaryHeadingRe.MatchString(s) {
		return "", errors.New("PR description is missing the required '## Summary' section")
	}
	return s, nil
}

// prSummaryHeadingRe matches a "## Summary" heading at the start of a line,
// tolerating extra leading '#' and trailing whitespace.
var prSummaryHeadingRe = regexp.MustCompile(`(?mi)^#{1,6}\s*summary\b`)

// ── shared helpers ──

// generateMetadata runs a single tight metadata generation against the
// operator's metadata model, resolved host-side through the shared resolver. It
// returns the trimmed text response. Mirrors SuggestTitle (internal/agent/title.go).
func generateMetadata(ctx context.Context, resolver ModelResolver, modelSlug, sys, userPrompt string, maxOut int) (string, error) {
	if resolver == nil {
		return "", errors.New("no model resolver configured for metadata generation")
	}
	if strings.TrimSpace(userPrompt) == "" {
		return "", errors.New("describe the change first — the context argument was empty")
	}
	model, err := resolver.Resolve(ctx, modelSlug)
	if err != nil {
		return "", fmt.Errorf("resolve metadata model %q: %w", modelSlug, err)
	}

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.1),
		fantasy.WithMaxOutputTokens(int64(maxOut)),
	)

	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	maxTok := int64(maxOut)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(userPrompt)},
		MaxOutputTokens: &maxTok,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Response.Content.Text()), nil
}

// firstNonEmptyLine returns the first non-blank line of s, trimmed. Models
// sometimes prepend a blank line or append an explanation; the contract for
// branch names and commit subjects lives on the first real line.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

// stripCodeFence removes an enclosing ```-fence when the model wraps its whole
// answer in one (```markdown ... ``` or ``` ... ```). A non-fenced string is
// returned unchanged.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (which may carry a language tag).
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		return ""
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
