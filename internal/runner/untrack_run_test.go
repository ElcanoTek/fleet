package runner

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A stale execution's late return must not drop the run ids a fresh attempt
// of the same task (after a lease reclaim) registered.
func TestUntrackRunKeepsAFreshAttempt(t *testing.T) {
	p := &Pool{}
	id := uuid.New()
	stale := p.trackRun(context.Background(), id, uuid.New())
	fresh := p.trackRun(context.Background(), id, uuid.New())
	p.untrackRun(id, stale)
	got, ok := p.runIDs.Load(id)
	if !ok || got.(runCorrelation) != fresh {
		t.Fatalf("after the stale execution returned, entry = %v (present %v), want the fresh attempt's %v", got, ok, fresh)
	}
	p.untrackRun(id, fresh)
	if _, ok := p.runIDs.Load(id); ok {
		t.Fatal("the fresh attempt's own return left its entry behind")
	}
}
