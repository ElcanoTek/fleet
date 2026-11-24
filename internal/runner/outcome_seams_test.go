package runner

import (
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// newOutcomeTestPool builds a pool for the outcome-write tests: one slot, a
// fast poll, no lease renewals (a test drives lease expiry itself), and
// outcome retries spaced in milliseconds so a test covers several attempts
// without waiting out the production backoff.
func newOutcomeTestPool(store *storage.Storage, runner TaskRunner, cfg Config) *Pool {
	if cfg.MaxConcurrentAgents == 0 {
		cfg.MaxConcurrentAgents = 1
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 20 * time.Millisecond
	}
	if cfg.LeaseRenewInterval == 0 {
		cfg.LeaseRenewInterval = time.Hour
	}
	p := NewPool(store, runner, cfg)
	p.outcomeRetryBase = time.Millisecond
	p.outcomeRetryMax = 20 * time.Millisecond
	return p
}
