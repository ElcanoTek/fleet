package agentcore

import "log/slog"

// Run-scoped loggers (#2052). The orchestration state and the run engine log
// from methods that take no context, deep in the loop's enforcement and
// resilience paths. Run gives each the run's context logger (logging.From of
// the run's context: conv_id, turn_id, task_id, run_id and the run's trace),
// so those lines join the run they are about — a retry of the same task is
// told apart by its run_id. Outside a run (unit tests building the state
// directly) they log through the process default.

// setLogger installs the run's context logger.
func (o *orchestrationState) setLogger(l *slog.Logger) { o.logger.Store(l) }

// log returns the run's context logger, or the default outside a run.
func (o *orchestrationState) log() *slog.Logger {
	if l := o.logger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// log returns the run's context logger, or the default outside a run.
func (e *engine) log() *slog.Logger {
	if e != nil && e.logger != nil {
		return e.logger
	}
	return slog.Default()
}
