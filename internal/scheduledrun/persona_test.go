package scheduledrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// newPersonaTestRunner builds a Runner with on-disk system-prompt + persona
// fixtures so the per-task persona resolution (#221) can be tested without a DB
// or sandbox.
func newPersonaTestRunner(t *testing.T) *Runner {
	t.Helper()
	spDir := t.TempDir()
	personaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(spDir, "default.md"), []byte("SYSTEM PROMPT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "assistant.yaml"), []byte("GLOBAL PERSONA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "security-auditor.yaml"), []byte("SECURITY EXPERTISE"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		cfg:              &config.Config{Persona: "assistant.yaml", SystemPrompt: "default.md"},
		personasDir:      personaDir,
		systemPromptsDir: spDir,
	}
	r.baseSystemPrompt = r.buildBaseSystemPrompt()
	return r
}

func TestTaskPromptAndPersona(t *testing.T) {
	r := newPersonaTestRunner(t)

	// Sanity: base prompt carries the global persona.
	if !strings.Contains(r.baseSystemPrompt, "GLOBAL PERSONA") || !strings.Contains(r.baseSystemPrompt, "SYSTEM PROMPT") {
		t.Fatalf("base prompt missing expected content: %q", r.baseSystemPrompt)
	}

	t.Run("no override uses base + global persona", func(t *testing.T) {
		sp, persona := r.taskPromptAndPersona(&models.Task{ID: uuid.New()})
		if sp != r.baseSystemPrompt || persona != "assistant.yaml" {
			t.Errorf("got (%q persona, base==%v), want global", persona, sp == r.baseSystemPrompt)
		}
	})

	t.Run("valid override swaps in the persona", func(t *testing.T) {
		sp, persona := r.taskPromptAndPersona(&models.Task{ID: uuid.New(), Persona: "security-auditor"})
		if !strings.Contains(sp, "SECURITY EXPERTISE") {
			t.Errorf("override prompt missing the persona expertise block: %q", sp)
		}
		if strings.Contains(sp, "GLOBAL PERSONA") {
			t.Errorf("override prompt must not include the global persona")
		}
		if persona != "security-auditor.yaml" {
			t.Errorf("persona metadata = %q, want security-auditor.yaml", persona)
		}
	})

	t.Run("unknown persona falls back to base + global", func(t *testing.T) {
		sp, persona := r.taskPromptAndPersona(&models.Task{ID: uuid.New(), Persona: "does-not-exist"})
		if sp != r.baseSystemPrompt || persona != "assistant.yaml" {
			t.Errorf("unknown persona should fall back to global, got persona=%q base=%v", persona, sp == r.baseSystemPrompt)
		}
	})

	t.Run("path traversal is neutralized (falls back, never escapes)", func(t *testing.T) {
		sp, persona := r.taskPromptAndPersona(&models.Task{ID: uuid.New(), Persona: "../../etc/passwd"})
		if sp != r.baseSystemPrompt || persona != "assistant.yaml" {
			t.Errorf("traversal attempt must fall back to global, got persona=%q", persona)
		}
	})
}
