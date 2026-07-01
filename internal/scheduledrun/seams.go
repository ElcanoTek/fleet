package scheduledrun

import "context"

// WorkspaceReporter records the host filesystem path of the per-run workspace
// directory a scheduled task ran in (#287), so the file-browser endpoints can
// later list and stream the artifacts the agent produced. The runner reports the
// path exactly once, when the run begins and the effective workspace (the
// per-run git worktree subdir when isolation is enabled, else the shared
// workspace root the sandbox bind-mounts) is known. A reporting failure is
// non-fatal — it only disables the after-the-fact file browser for that run,
// never the run itself.
//
// This mirrors agentcore.WithStreamObserver: a run-context seam the driver
// (the runner pool) installs and the run loop reads. It lives here, beside the
// scheduled runner that emits it, rather than in the generic pool, so the pool's
// TaskRunner contract stays a plain (ctx, task) → (session, error).
type WorkspaceReporter func(ctx context.Context, path string)

// workspaceReporterKey carries an OPTIONAL WorkspaceReporter on the run context.
// It is a private struct-typed key so it cannot collide with another package's
// context values. The runner pool installs a reporter that persists the path to
// the task row; tests and the cutlass dev one-shot leave it unset (nil), in
// which case reportWorkspacePath is a no-op and behaviour is unchanged.
type workspaceReporterKey struct{}

// WithWorkspaceReporter returns a context carrying r. A nil r leaves the context
// untouched so a caller that opts out is indistinguishable from one that never
// set a reporter.
func WithWorkspaceReporter(ctx context.Context, r WorkspaceReporter) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, workspaceReporterKey{}, r)
}

// reportWorkspacePath invokes the context's WorkspaceReporter with path when one
// is installed; otherwise it is a no-op. An empty path is never reported (a run
// with no resolvable workspace simply records nothing).
func reportWorkspacePath(ctx context.Context, path string) {
	if path == "" {
		return
	}
	if r, ok := ctx.Value(workspaceReporterKey{}).(WorkspaceReporter); ok && r != nil {
		r(ctx, path)
	}
}
