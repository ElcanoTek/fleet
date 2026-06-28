package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskPersonaRoundTrip pins the nullable persona column (#221): empty
// round-trips empty, a set persona round-trips intact, and an edit replaces it.
func TestTaskPersonaRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	plain := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	withPersona := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), Persona: "security-auditor"}
	for _, tk := range []*models.Task{plain, withPersona} {
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	if got, err := store.GetTask(plain.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Persona != "" {
		t.Errorf("empty persona must round-trip empty, got %q", got.Persona)
	}
	if got, err := store.GetTask(withPersona.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Persona != "security-auditor" {
		t.Errorf("persona did not round-trip, got %q", got.Persona)
	}

	// Edit replaces the persona (unconditional, like RuntimeFlavor).
	edit := TaskEdit{Prompt: "p", Persona: "tech-writer"}
	if upd, err := store.UpdateEditableTask(ctx, withPersona.ID, edit); err != nil {
		t.Fatalf("edit: %v", err)
	} else if upd.Persona != "tech-writer" {
		t.Errorf("edit should have set persona, got %q", upd.Persona)
	}
}
