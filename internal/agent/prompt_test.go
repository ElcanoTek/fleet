package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// TestBuildSystemPrompt_AgentNotesInjection verifies the admin-curated notes
// section renders after the User Memories block, and that the Note Proposal
// Tool block is always present. This is the prompt-assembly seam the notes
// feature shares across both modes.
func TestBuildSystemPrompt_AgentNotesInjection(t *testing.T) {
	m := fixtureManager(t)

	notes := []agentcore.Note{
		{Slug: "xandr-limits", Title: "Xandr Limits", Body: "Max 5 deals/min."},
	}

	withBoth, err := m.buildSystemPrompt("victoria", "c", []string{"Prefers concise answers."}, notes, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	memIdx := strings.Index(withBoth, "## User Memories")
	notesIdx := strings.Index(withBoth, "## Agent Notes (Admin-Maintained Knowledge Base)")
	if memIdx < 0 {
		t.Fatal("expected ## User Memories section")
	}
	if notesIdx < 0 {
		t.Fatal("expected ## Agent Notes section")
	}
	if notesIdx < memIdx {
		t.Error("Agent Notes must render AFTER User Memories")
	}
	for _, want := range []string{"Xandr Limits", "xandr-limits", "Max 5 deals/min.", "## Note Proposal Tool", "propose_note"} {
		if !strings.Contains(withBoth, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	notesOnly, err := m.buildSystemPrompt("victoria", "c", nil, notes, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt notes-only: %v", err)
	}
	if strings.Contains(notesOnly, "## User Memories") {
		t.Error("notes-only prompt must NOT contain a User Memories section")
	}
	if !strings.Contains(notesOnly, "## Agent Notes (Admin-Maintained Knowledge Base)") {
		t.Error("notes-only prompt must contain the Agent Notes section")
	}

	none, err := m.buildSystemPrompt("victoria", "c", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt none: %v", err)
	}
	if strings.Contains(none, "## Agent Notes (Admin-Maintained Knowledge Base)") {
		t.Error("empty notes must omit the Agent Notes section")
	}
	if !strings.Contains(none, "## Note Proposal Tool") {
		t.Error("Note Proposal Tool block should always be present")
	}
}

// writeFile creates path with content, making parent dirs as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// fixtureManager returns a Manager with only the directory-reading bits
// initialized — sufficient for testing buildSystemPrompt and ListPersonas
// without hitting OpenRouter.
func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "system_prompts", "chat.md"), "# Chat System Prompt\n\nBe helpful.\n")
	writeFile(t, filepath.Join(dir, "personas", "victoria.yaml"), "name: Victoria\n")
	writeFile(t, filepath.Join(dir, "personas", "generic.yaml"), "name: Generic\n")
	writeFile(t, filepath.Join(dir, "protocols", "optimization.md"), "# Optimization Protocol\n")
	writeFile(t, filepath.Join(dir, "protocols", "other.yaml"), "name: other\n")
	writeFile(t, filepath.Join(dir, "protocols", "README.txt"), "ignored\n")

	return &Manager{
		config:               &config.Config{PersonaDefault: "victoria"},
		personasDir:          filepath.Join(dir, "personas"),
		protocolsDir:         filepath.Join(dir, "protocols"),
		skillsDir:            filepath.Join(dir, "skills"),
		systemPromptsDir:     filepath.Join(dir, "system_prompts"),
		chatSystemPromptFile: "chat.md",
	}
}

// TestBuildSystemPrompt_SkillsRoster verifies the Skills section appears only
// when the bundle ships at least one well-formed skill, and that each listed
// skill carries its name, bundle-relative SKILL.md path, and description
// (Level-1 progressive-disclosure metadata).
func TestBuildSystemPrompt_SkillsRoster(t *testing.T) {
	m := fixtureManager(t)

	// No skills yet → no Skills section.
	none, err := m.buildSystemPrompt("victoria", "conv-x", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt (no skills): %v", err)
	}
	if strings.Contains(none, "## Skills") {
		t.Error("Skills section should be absent when the bundle ships no skills")
	}

	// Add one well-formed skill + one malformed (no SKILL.md) → only the good one
	// is rostered.
	writeFile(t, filepath.Join(m.skillsDir, "deal-pacing", "SKILL.md"),
		"---\nname: deal-pacing\ndescription: Pace a deal toward its budget. Use when a campaign is over/under-delivering.\n---\n\n# Deal pacing\n")
	if err := os.MkdirAll(filepath.Join(m.skillsDir, "broken-skill"), 0o755); err != nil {
		t.Fatalf("mkdir broken-skill: %v", err)
	}

	with, err := m.buildSystemPrompt("victoria", "conv-x", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt (with skills): %v", err)
	}
	must := []string{
		"## Skills",
		"**deal-pacing**",
		"skills/deal-pacing/SKILL.md",
		"Pace a deal toward its budget",
	}
	for _, want := range must {
		if !strings.Contains(with, want) {
			t.Errorf("prompt missing %q\n\n--- prompt ---\n%s", want, with)
		}
	}
	if strings.Contains(with, "broken-skill") {
		t.Error("malformed skill (no SKILL.md) should not be rostered")
	}
}

func TestListPersonas_AlphaSorted(t *testing.T) {
	m := fixtureManager(t)

	names, err := m.ListPersonas()
	if err != nil {
		t.Fatalf("ListPersonas: %v", err)
	}
	want := []string{"generic", "victoria"}
	if len(names) != len(want) {
		t.Fatalf("count: got %v", names)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("names[%d] = %q want %q", i, n, want[i])
		}
	}
}

func TestBuildSystemPrompt_Layering(t *testing.T) {
	m := fixtureManager(t)

	prompt, err := m.buildSystemPrompt("victoria", "test-conv", []string{"User prefers concise answers."}, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	must := []string{
		"Chat System Prompt",   // system prompt is first
		"# Persona: victoria",  // persona header
		"name: Victoria",       // persona content
		"Runtime Date Context", // date context
		"## User Memories",
		"User prefers concise answers.",
		"protocols/optimization.md",
		"protocols/other.yaml",
	}
	for _, m := range must {
		if !strings.Contains(prompt, m) {
			t.Errorf("prompt missing %q\n\n--- prompt ---\n%s", m, prompt)
		}
	}
	// README.txt in protocols dir should NOT be listed.
	if strings.Contains(prompt, "README.txt") {
		t.Error("prompt listed README.txt as a protocol")
	}
}

// TestBuildSystemPrompt_FastIOGated verifies the Fast.io system-prompt
// section, the fastio-mcp.md protocol entry, and the fastio-specific
// tool guidance are ONLY included when the fast.io MCP server has
// tools wired up for this turn. Fast.io is optional — deployments
// without FAST_IO_MCP_TOKEN must not see Fast.io references at all,
// or the agent wastes context and may try to call tools that don't
// exist.
func TestBuildSystemPrompt_FastIOGated(t *testing.T) {
	m := fixtureManager(t)
	// Add the fastio-mcp.md protocol file to the fixture so we can
	// verify the gate filters it out / includes it.
	writeFile(t, filepath.Join(m.protocolsDir, "fastio-mcp.md"), "# Fast.io Protocol\n")

	// Fast.io OFF — empty tool roster, no fast.io entries.
	off, err := m.buildSystemPrompt("victoria", "conv-x", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt off: %v", err)
	}
	for _, mustnt := range []string{
		"## Fast.io: the shared read/write file store",
		"fastio_find",
		"fastio_upload_file",
		"protocols/fastio-mcp.md",
	} {
		if strings.Contains(off, mustnt) {
			t.Errorf("Fast.io content leaked into the OFF prompt: %q\n--- prompt ---\n%s", mustnt, off)
		}
	}

	// Fast.io ON — pretend the fast_io MCP server is wired up by
	// inserting a fast-io-prefixed tool into the frozen roster.
	m.mcpToolRoster = []string{"mcp_fast_io_storage", "mcp_fast_io_workspace"}
	on, err := m.buildSystemPrompt("victoria", "conv-x", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt on: %v", err)
	}
	for _, must := range []string{
		"## Fast.io: the shared read/write file store",
		"fastio_find",
		"fastio_upload_file",
		"File-pick policy",
		"protocols/fastio-mcp.md", // listed in the dynamic protocols block
	} {
		if !strings.Contains(on, must) {
			t.Errorf("Fast.io content missing from the ON prompt: %q\n--- prompt ---\n%s", must, on)
		}
	}
}

func TestBuildSystemPrompt_UnknownPersona(t *testing.T) {
	m := fixtureManager(t)

	_, err := m.buildSystemPrompt("nope-does-not-exist", "test-conv", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown persona")
	}
}

func TestBuildSystemPrompt_PathTraversalRejected(t *testing.T) {
	m := fixtureManager(t)

	// Load code uses filepath.Base to strip any traversal, so feeding
	// "../../../etc/passwd" should attempt to read ".persona.yaml"
	// (base=etc, ext=passwd) which doesn't exist → error, not a breach.
	_, err := m.buildSystemPrompt("../../../etc/passwd", "test-conv", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error (file not found after path-sanitize)")
	}
}

func TestRuntimeDateContextFormat(t *testing.T) {
	now, err := time.Parse(time.RFC3339, "2026-04-17T10:20:30Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := runtimeDateContext(now)
	if !strings.Contains(s, "2026-04-17") {
		t.Errorf("missing UTC date: %s", s)
	}
	if !strings.Contains(s, "Runtime Date Context") {
		t.Errorf("missing heading: %s", s)
	}
}

// Day-precision is load-bearing for prompt caching: the system prompt feeds
// every turn, and any sub-day variance (hour, minute, second) would break the
// cache breakpoint each turn. Guard against regressions that re-introduce
// finer precision "for completeness".
func TestRuntimeDateContextStableWithinUTCDay(t *testing.T) {
	day, err := time.Parse(time.RFC3339, "2026-04-17T00:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := runtimeDateContext(day)

	// Several moments across the same UTC day must produce byte-identical output.
	for _, offset := range []time.Duration{
		1 * time.Second,
		37 * time.Minute,
		5 * time.Hour,
		23*time.Hour + 59*time.Minute + 59*time.Second,
	} {
		got := runtimeDateContext(day.Add(offset))
		if got != want {
			t.Errorf("runtime date context changed %s into the UTC day — cache breakpoint will reset every turn.\nwant:\n%s\ngot:\n%s",
				offset, want, got)
		}
	}

	// Crossing the UTC day boundary SHOULD change the output — otherwise
	// "today" would be stuck at yesterday forever.
	nextDay := runtimeDateContext(day.Add(24 * time.Hour))
	if nextDay == want {
		t.Error("runtime date context must update across UTC day boundaries")
	}
}
