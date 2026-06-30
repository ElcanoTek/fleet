package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// ── fake model + resolver (no live LLM, no HTTP) ──

// fakeMetadataModel returns a canned text response (or error) so the metadata
// tools can be exercised end-to-end through generateMetadata without a network
// call. Only Generate is meaningful; the other LanguageModel methods are unused
// by fantasy.Agent.Generate for a text-only response.
type fakeMetadataModel struct {
	text string
	err  error
}

func (m *fakeMetadataModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *fakeMetadataModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("stream not supported")
}

func (m *fakeMetadataModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("object generation not supported")
}

func (m *fakeMetadataModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("object streaming not supported")
}

func (m *fakeMetadataModel) Provider() string { return "fake" }
func (m *fakeMetadataModel) Model() string    { return "fake/metadata" }

// fakeResolver hands out a fixed model (or a resolve error).
type fakeResolver struct {
	model      fantasy.LanguageModel
	resolveErr error
}

func (r *fakeResolver) Resolve(context.Context, string) (fantasy.LanguageModel, error) {
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.model, nil
}

func okResolver(text string) *fakeResolver {
	return &fakeResolver{model: &fakeMetadataModel{text: text}}
}

func runTool(t *testing.T, tool fantasy.AgentTool, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: input})
	if err != nil {
		t.Fatalf("tool.Run returned a transport error (should be in-band): %v", err)
	}
	return resp
}

// ── pure normalization/validation logic ──

func TestNormalizeBranchName(t *testing.T) {
	valid := map[string]string{
		"feat/add-oauth2-user-authentication": "feat/add-oauth2-user-authentication",
		"fix/null-pointer-in-parser":          "fix/null-pointer-in-parser",
		"chore/bump-deps":                     "chore/bump-deps",
		"  Feat/Add-OAuth2  ":                 "feat/add-oauth2", // trim + lowercase
		"`feat/add-thing`":                    "feat/add-thing",  // strip backticks
		"\"fix/bug\"":                         "fix/bug",         // strip quotes
		"feat/add-thing\nsome explanation":    "feat/add-thing",  // first line only
	}
	for raw, want := range valid {
		got, err := normalizeBranchName(raw)
		if err != nil {
			t.Errorf("normalizeBranchName(%q) unexpected error: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeBranchName(%q) = %q, want %q", raw, got, want)
		}
	}

	invalid := []string{
		"",                      // empty
		"feat/add some thing",   // internal space
		"-leading-hyphen",       // must start with a letter
		"9starts-with-digit",    // must start with a letter
		"feat/add_underscore",   // underscores not allowed
		"Feat/Has UPPER mid",    // space, even after lowercasing the rest fails
		strings.Repeat("a", 61), // 61 chars > 60 ceiling
		"feat/has~tilde",        // disallowed punctuation
		"feat/",                 // git refuses a ref ending in "/"
		"feat//double",          // git refuses "//"
	}
	for _, raw := range invalid {
		if got, err := normalizeBranchName(raw); err == nil {
			t.Errorf("normalizeBranchName(%q) = %q, expected an error", raw, got)
		}
	}
}

func TestNormalizeBranchName_LengthBoundary(t *testing.T) {
	// Exactly 60 chars passes; 61 fails.
	at60 := "feat/" + strings.Repeat("a", 55) // 5 + 55 = 60
	if got, err := normalizeBranchName(at60); err != nil {
		t.Errorf("60-char branch name should pass, got error: %v (got %q)", err, got)
	}
	at61 := "feat/" + strings.Repeat("a", 56) // 61
	if _, err := normalizeBranchName(at61); err == nil {
		t.Error("61-char branch name should be rejected")
	}
}

func TestNormalizeCommitMessage(t *testing.T) {
	valid := []string{
		"feat: add a thing",
		"feat(auth): add user authentication via OAuth2\n\nImplemented the flow.",
		"fix(parser)!: handle nil node", // breaking-change marker
		"chore: bump deps",
		"```\nfeat: fenced message\n\nbody\n```", // enclosing fence stripped
	}
	for _, raw := range valid {
		if _, err := normalizeCommitMessage(raw); err != nil {
			t.Errorf("normalizeCommitMessage(%q) unexpected error: %v", raw, err)
		}
	}

	// Fence stripping returns the inner message, not the fence.
	got, err := normalizeCommitMessage("```\nfeat: fenced message\n\nbody\n```")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "```") {
		t.Errorf("expected fences stripped, got %q", got)
	}

	invalid := []string{
		"",                                 // empty
		"add a thing",                      // no type prefix
		"feature: wrong type word",         // 'feature' is not a valid type
		"feat add a thing",                 // missing colon
		"feat: " + strings.Repeat("x", 70), // 76-char subject > 72
		"random first line\nfeat: late",    // first line is not a valid header
	}
	for _, raw := range invalid {
		if got, err := normalizeCommitMessage(raw); err == nil {
			t.Errorf("normalizeCommitMessage(%q) = %q, expected an error", raw, got)
		}
	}
}

func TestNormalizeCommitMessage_SubjectBoundary(t *testing.T) {
	// "feat: " is 6 chars; 66 'x' → 72 total subject passes; 67 → 73 fails.
	at72 := "feat: " + strings.Repeat("x", 66)
	if _, err := normalizeCommitMessage(at72); err != nil {
		t.Errorf("72-char subject should pass, got: %v", err)
	}
	at73 := "feat: " + strings.Repeat("x", 67)
	if _, err := normalizeCommitMessage(at73); err == nil {
		t.Error("73-char subject should be rejected")
	}
}

func TestNormalizePRDescription(t *testing.T) {
	valid := "## Summary\n- did a thing\n\n## Changes\n- file.go\n\n## Testing\n- [ ] CI"
	if _, err := normalizePRDescription(valid); err != nil {
		t.Errorf("valid PR description rejected: %v", err)
	}

	// Enclosing ```markdown fence is stripped, Summary still detected.
	fenced := "```markdown\n## Summary\n- thing\n```"
	got, err := normalizePRDescription(fenced)
	if err != nil {
		t.Fatalf("fenced PR description rejected: %v", err)
	}
	if strings.Contains(got, "```") {
		t.Errorf("expected fences stripped, got %q", got)
	}

	invalid := []string{
		"",                                  // empty
		"Just a paragraph with no headings", // no Summary
		"## Changes\n- only changes",        // missing Summary
	}
	for _, raw := range invalid {
		if _, err := normalizePRDescription(raw); err == nil {
			t.Errorf("normalizePRDescription(%q) expected an error", raw)
		}
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := map[string]string{
		"no fence here":           "no fence here",
		"```\nbody\n```":          "body",
		"```go\ncode\n```":        "code",
		"```json\n{\"a\":1}\n```": "{\"a\":1}",
		"```only-open-line":       "", // a lone fence line with no newline → empty inner
	}
	for in, want := range cases {
		if got := stripCodeFence(in); got != want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	cases := map[string]string{
		"first\nsecond":      "first",
		"\n\n  hello  \nbye": "hello",
		"   ":                "",
		"only":               "only",
	}
	for in, want := range cases {
		if got := firstNonEmptyLine(in); got != want {
			t.Errorf("firstNonEmptyLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── tool wiring (end-to-end via fake model) ──

func TestSuggestBranchNameTool_HappyPath(t *testing.T) {
	tool := NewSuggestBranchNameTool(okResolver("feat/add-oauth2-login"), "fake/model")
	resp := runTool(t, tool, `{"context":"adds oauth2 login for the web app"}`)
	if resp.IsError {
		t.Fatalf("expected success, got error: %q", resp.Content)
	}
	if resp.Content != "feat/add-oauth2-login" {
		t.Errorf("got %q, want feat/add-oauth2-login", resp.Content)
	}
}

func TestSuggestBranchNameTool_RejectsUnsafe(t *testing.T) {
	// Model returns something with a space → tool must error, not hand garbage to git.
	tool := NewSuggestBranchNameTool(okResolver("feat add a thing please"), "fake/model")
	resp := runTool(t, tool, `{"context":"x"}`)
	if !resp.IsError {
		t.Fatalf("expected an error response for an unsafe branch name, got: %q", resp.Content)
	}
}

func TestSuggestCommitMessageTool_HappyPath(t *testing.T) {
	tool := NewSuggestCommitMessageTool(okResolver("feat(auth): add oauth2\n\nbody"), "fake/model")
	resp := runTool(t, tool, `{"diff_summary":"added auth","context":"ELC-1"}`)
	if resp.IsError {
		t.Fatalf("expected success, got error: %q", resp.Content)
	}
	if !strings.HasPrefix(resp.Content, "feat(auth): add oauth2") {
		t.Errorf("got %q", resp.Content)
	}
}

func TestSuggestPRDescriptionTool_HappyPath(t *testing.T) {
	tool := NewSuggestPRDescriptionTool(okResolver("## Summary\n- thing\n\n## Changes\n- x\n\n## Testing\n- [ ] ci"), "fake/model")
	resp := runTool(t, tool, `{"changes":"touched x"}`)
	if resp.IsError {
		t.Fatalf("expected success, got error: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "## Summary") {
		t.Errorf("got %q", resp.Content)
	}
}

func TestMetadataTools_NilResolverErrors(t *testing.T) {
	// DefaultTools constructs these with a nil resolver (schema-only). If one is
	// ever invoked through that slice it must refuse cleanly, not panic.
	for _, tool := range MetadataTools(nil, "") {
		resp := runTool(t, tool, `{"context":"x","diff_summary":"x","changes":"x"}`)
		if !resp.IsError {
			t.Errorf("%s with nil resolver should error, got: %q", tool.Info().Name, resp.Content)
		}
	}
}

func TestMetadataTools_ResolveErrorIsInBand(t *testing.T) {
	r := &fakeResolver{resolveErr: errors.New("provider down")}
	tool := NewSuggestBranchNameTool(r, "fake/model")
	resp := runTool(t, tool, `{"context":"x"}`)
	if !resp.IsError {
		t.Fatalf("expected an error response when resolve fails, got: %q", resp.Content)
	}
}

func TestMetadataTools_EmptyContextErrors(t *testing.T) {
	tool := NewSuggestBranchNameTool(okResolver("feat/x"), "fake/model")
	resp := runTool(t, tool, `{"context":"   "}`)
	if !resp.IsError {
		t.Fatalf("expected an error for empty context, got: %q", resp.Content)
	}
}

func TestMetadataTools_RegisteredWithExpectedNames(t *testing.T) {
	got := MetadataTools(nil, "")
	if len(got) != 3 {
		t.Fatalf("expected 3 metadata tools, got %d", len(got))
	}
	want := map[string]bool{
		SuggestBranchNameToolName:    false,
		SuggestCommitMessageToolName: false,
		SuggestPRDescriptionToolName: false,
	}
	for _, tool := range got {
		info := tool.Info()
		if _, ok := want[info.Name]; !ok {
			t.Errorf("unexpected tool %q", info.Name)
			continue
		}
		want[info.Name] = true
		if strings.TrimSpace(info.Description) == "" {
			t.Errorf("tool %q has an empty description", info.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("expected tool %q in MetadataTools", name)
		}
	}
}

func TestMetadataTools_NotInSharedTurnSet(t *testing.T) {
	// Scoping guard (#191 + ceiling protection): the metadata tools must NOT be
	// in the shared DefaultTools/NewTurnTools set — they are wired only into the
	// scheduled native set (see internal/scheduledrun), so the interactive chat
	// turn, which runs near the 128-tool ceiling once per-user MCP servers (#449)
	// load, is never pushed over by these 3 always-on tools. If a future change
	// adds them to the shared set, this test fires so the ceiling impact is a
	// deliberate, reviewed decision.
	metadataNames := map[string]bool{
		SuggestBranchNameToolName:    true,
		SuggestCommitMessageToolName: true,
		SuggestPRDescriptionToolName: true,
	}
	for _, tool := range DefaultTools() {
		if metadataNames[tool.Info().Name] {
			t.Errorf("%s must not be in DefaultTools() — it is scheduled-only (chat tool-count ceiling)", tool.Info().Name)
		}
	}
}
