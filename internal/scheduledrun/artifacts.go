package scheduledrun

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// maxArtifactsPerRun caps how many artifacts a single run may publish, bounding
// the manifest a misbehaving (or adversarially-prompted) agent can accumulate.
const maxArtifactsPerRun = 50

// ArtifactCollector accumulates the artifacts a scheduled run's agent publishes
// via the publish_artifact tool (#204). It satisfies tools.ArtifactRecorder. The
// runner pool installs one on the run context (mirroring WorkspaceReporter); the
// tool records into it during the run; the runner reads Marshal() afterward to
// persist the curated manifest. It is concurrency-safe because parallel tool
// calls in one turn may record at the same time.
type ArtifactCollector struct {
	mu   sync.Mutex
	list []models.TaskArtifact
}

// NewArtifactCollector returns an empty collector.
func NewArtifactCollector() *ArtifactCollector { return &ArtifactCollector{} }

// RecordArtifact implements tools.ArtifactRecorder. Re-publishing the same path
// updates its name/description/size in place (dedup by path); a new path is
// appended up to the per-run cap.
func (c *ArtifactCollector) RecordArtifact(name, relPath, description string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.list {
		if c.list[i].Path == relPath {
			c.list[i].Name = name
			c.list[i].Description = description
			c.list[i].Size = size
			return nil
		}
	}
	if len(c.list) >= maxArtifactsPerRun {
		return fmt.Errorf("artifact limit reached: a run may publish at most %d artifacts", maxArtifactsPerRun)
	}
	c.list = append(c.list, models.TaskArtifact{Name: name, Path: relPath, Description: description, Size: size})
	return nil
}

// Marshal returns the manifest as JSON for persistence, or nil when nothing was
// published (so the storage layer leaves the column untouched).
func (c *ArtifactCollector) Marshal() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.list) == 0 {
		return nil
	}
	b, err := json.Marshal(c.list)
	if err != nil {
		return nil
	}
	return b
}

type artifactCollectorKey struct{}

// WithArtifactCollector returns a context carrying c. A nil c leaves the context
// untouched, so the publish_artifact tool is simply not wired and behaviour is
// unchanged (tests / the cutlass one-shot leave it unset).
func WithArtifactCollector(ctx context.Context, c *ArtifactCollector) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, artifactCollectorKey{}, c)
}

// ArtifactCollectorFromContext returns the installed collector, or nil when none
// was set (the publish_artifact tool is then not registered for the run).
func ArtifactCollectorFromContext(ctx context.Context) *ArtifactCollector {
	if c, ok := ctx.Value(artifactCollectorKey{}).(*ArtifactCollector); ok {
		return c
	}
	return nil
}
