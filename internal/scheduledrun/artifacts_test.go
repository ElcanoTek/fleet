package scheduledrun

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestArtifactCollector(t *testing.T) {
	t.Run("empty collector marshals to nil (column left untouched)", func(t *testing.T) {
		c := NewArtifactCollector()
		if c.Marshal() != nil {
			t.Errorf("empty collector should Marshal to nil, got %s", c.Marshal())
		}
	})

	t.Run("records and marshals a manifest", func(t *testing.T) {
		c := NewArtifactCollector()
		if err := c.RecordArtifact("report.csv", "report.csv", "Q3", 12); err != nil {
			t.Fatal(err)
		}
		if err := c.RecordArtifact("summary.txt", "out/summary.txt", "", 5); err != nil {
			t.Fatal(err)
		}
		var got []models.TaskArtifact
		if err := json.Unmarshal(c.Marshal(), &got); err != nil {
			t.Fatalf("manifest is not valid JSON: %v", err)
		}
		if len(got) != 2 || got[0].Name != "report.csv" || got[1].Path != "out/summary.txt" {
			t.Errorf("manifest = %+v", got)
		}
	})

	t.Run("re-publishing the same path updates in place (dedup)", func(t *testing.T) {
		c := NewArtifactCollector()
		_ = c.RecordArtifact("report.csv", "report.csv", "old", 1)
		_ = c.RecordArtifact("report.csv", "report.csv", "new", 99)
		var got []models.TaskArtifact
		_ = json.Unmarshal(c.Marshal(), &got)
		if len(got) != 1 || got[0].Description != "new" || got[0].Size != 99 {
			t.Errorf("expected one updated entry, got %+v", got)
		}
	})

	t.Run("enforces the per-run cap", func(t *testing.T) {
		c := NewArtifactCollector()
		for i := 0; i < maxArtifactsPerRun; i++ {
			if err := c.RecordArtifact(fmt.Sprintf("f%d", i), fmt.Sprintf("f%d", i), "", 1); err != nil {
				t.Fatalf("record %d should succeed: %v", i, err)
			}
		}
		if err := c.RecordArtifact("over", "over", "", 1); err == nil {
			t.Error("expected the cap+1 record to error")
		}
		// Re-publishing an EXISTING path past the cap is still allowed (no growth).
		if err := c.RecordArtifact("f0", "f0", "updated", 2); err != nil {
			t.Errorf("dedup update past cap should succeed, got %v", err)
		}
	})

	t.Run("context seam round-trips; nil is a no-op", func(t *testing.T) {
		if ArtifactCollectorFromContext(context.Background()) != nil {
			t.Error("expected nil collector on a bare context")
		}
		if WithArtifactCollector(context.Background(), nil) != context.Background() {
			t.Error("nil collector should leave the context untouched")
		}
		c := NewArtifactCollector()
		ctx := WithArtifactCollector(context.Background(), c)
		if ArtifactCollectorFromContext(ctx) != c {
			t.Error("collector did not round-trip through the context")
		}
	})
}
