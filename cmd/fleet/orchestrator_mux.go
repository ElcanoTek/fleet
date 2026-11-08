package main

import (
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/otelsetup"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/version"
)

// buildOrchestratorMux registers the orchestrator routes (chi), mirroring moc's
// auth groups, plus the P6b notes CRUD + proposal-decision routes (admin-gated).
//
// trustedProxies is cfg.TrustedProxies (FLEET_TRUSTED_PROXIES); it decides
// whether X-Forwarded-For is consulted at all — see the comment at its use.
func buildOrchestratorMux(h *handlers.Handlers, notes *handlers.NotesHandlers, reloadConfig, reloadMCP http.HandlerFunc, trustedProxies []net.IP) http.Handler {
	r := chi.NewRouter()
	// The client IP keys login and API rate limiting (getClientIP), so it
	// follows the FLEET_TRUSTED_PROXIES contract, resolved by the same
	// httpmw.TrustedProxies the chat server's IP filter uses
	// (internal/httpapi/ipfilter.go) so the two servers always name the same
	// client: empty, the default, means X-Forwarded-For is NEVER consulted
	// (Middleware is a pass-through) and getClientIP falls back to the TCP
	// peer in r.RemoteAddr; set, it is read only when that peer is one of the
	// named proxies. ClientIPFromXFF treats r.RemoteAddr as the
	// rightmost hop, so a client reaching fleet directly cannot pick its own
	// identity by sending the header (GHSA-j6qf-3m9c-vwj3); behind a trusted
	// proxy it walks the chain right-to-left past trusted hops, so a
	// prepended spoof never wins either (GHSA-3fxj-6jh8-hvhx et al.).
	r.Use(httpmw.NewTrustedProxies(trustedProxies).Middleware())
	r.Use(httpmw.Recoverer)
	r.Use(otelsetup.Middleware)          // #186: server span + X-Request-Id (no-op when tracing disabled)
	r.Use(orchestratorMetricsMiddleware) // #176: record request count + latency
	r.Use(h.SecurityHeadersMiddleware)
	r.Use(h.BodySizeLimitMiddleware)
	// A caller still presenting the retired service token (ADR-0108) gets a
	// 401 naming the replacement, before CSRF could answer 403 instead.
	r.Use(h.RetiredServiceTokenMiddleware)
	r.Use(h.CSRFMiddleware)

	r.Get("/health", h.HealthCheck)
	r.Get("/api/config", h.GetDashboardConfig)
	// Version discovery (#321): unauthenticated, unversioned-forever (like /health).
	// Also reachable at /v1/api-info via the apiversion.Router wrapper.
	r.Get("/api-info", apiversion.InfoHandler(version.Version()))
	// A2A discovery (#1279): the Agent Card, public by construction (it names
	// capabilities and the endpoint URL, nothing else) at the well-known path
	// the A2A spec fixes — unversioned forever (apiversion.unversionedForever).
	// Answers 501 until FLEET_A2A_ENABLED wires SetA2A.
	r.Get("/.well-known/agent-card.json", h.A2AAgentCard)

	// Admin-gated mutations.
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		// Prometheus scrape endpoint (#176) — admin-API-key gated like other
		// sensitive reads; cost/token data must not be public.
		r.Get("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte(metrics.Render()))
		})
		r.Post("/tasks/cleanup", h.CleanupHistory)
		r.Post("/tasks/model", h.BulkSetTaskModel) // fleet-wide model re-assignment (admin-gated)
		// Pending-queue inspection (#230): per-tier depth + oldest wait, so an
		// operator can see backlog and starvation. Admin-gated like the other
		// sensitive reads.
		r.Get("/admin/queue", h.QueueStats)
		r.Get("/admin/pipeline-metrics", h.PipelineMetrics)
		// Migration status (#256): applied vs pending sched-DB migrations, the
		// current tracking-row version + dirty flag. Admin-gated like the other
		// sensitive reads; strictly read-only (applies nothing).
		r.Get("/admin/migrations", h.MigrationStatus)
		// Config hot-reload (#286): re-read the reloadable settings (cost/token/
		// iteration ceilings, temperatures) without a restart. Admin-gated like the
		// other sensitive mutations; returns a JSON diff of what changed, what was
		// skipped (non-reloadable, needs restart), and any parse errors.
		r.Post("/admin/reload-config", reloadConfig)
		// MCP hot-reload (#218): re-read the client-config bundle's MCP catalog and
		// apply server add/remove/restart to the live interactive Manager without a
		// restart. Admin-gated like the other sensitive mutations; returns a JSON
		// summary of what changed. Equivalent to sending SIGHUP.
		r.Post("/admin/mcp-servers/reload", reloadMCP)
		// MCP tool schema findings: tools whose input schema used older JSON
		// Schema drafts (translated) or is invalid (withheld from the model),
		// as recorded by the turns this process has run. Admin-gated read;
		// `fleet mcp schema-issues` and `fleet doctor` consume it.
		r.Get("/admin/mcp-servers/schema-issues", mcpSchemaIssuesHandler)
		r.Post("/users", h.CreateUser)
		r.Post("/keys", h.CreateAPIKey)
		r.Get("/keys", h.ListAPIKeys)
		r.Get("/keys/audit", h.GetAuditLog)
		r.Get("/keys/{key_id}", h.GetAPIKey)
		r.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
		r.Post("/keys/{key_id}/revoke", h.RevokeAPIKey)
		r.Delete("/keys/{key_id}", h.DeleteAPIKey)

		// Notes admin: CRUD + proposal decisions (NOTES_WIKI_SPEC §6).
		r.Post("/notes", notes.CreateNote)
		r.Put("/notes/{slug}", notes.UpdateNote)
		r.Delete("/notes/{slug}", notes.ArchiveNote)
		r.Post("/notes/proposals/{id}/publish", notes.PublishProposal)
		r.Post("/notes/proposals/{id}/reject", notes.RejectProposal)
	})

	// Admin-or-user reads.
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		// SLA report (#274): per-prompt actual-duration p50/p95 + breach rate over a
		// window. Registered here (not under AdminAuthMiddleware) so the gateway's
		// session principal resolves the caller into a principal; the handler
		// then gates on PermissionAdmin (#458). The bare admin-API-key gate made it
		// unreachable from the dashboard — the proxy can never send X-API-Key.
		r.Get("/sla-report", h.GetSLAReport)
		// Usage analytics (#601 part 1): cost/token roll-up by principal /
		// project / model / time bucket over the persisted metering. Same
		// placement rationale as /sla-report: behind AdminOrUserAuthMiddleware
		// so the gateway's session principal resolves a principal,
		// with the admin gate enforced in-handler on PermissionAdmin.
		r.Get("/admin/usage", h.GetUsageReport)
		// Adoption analytics: the per-user AI-adoption read model behind the
		// Operations Center's exec Adoption view (per-user daily series, trend
		// deltas vs the previous window, and the seat roster). Same placement
		// rationale and in-handler PermissionAdmin gate as /admin/usage.
		r.Get("/admin/usage/adoption", h.GetAdoptionReport)
		// Per-principal rolling budgets (#601 part 2): admin CRUD over the
		// budget rows the shared create gate enforces. Same placement rationale
		// as /admin/usage — AdminOrUserAuthMiddleware for proxy reachability,
		// with the admin gate enforced in-handler on PermissionAdmin.
		r.Get("/admin/budgets", h.ListBudgets)
		r.Post("/admin/budgets", h.UpsertBudget)
		r.Delete("/admin/budgets/{budget_id}", h.DeleteBudget)
		// Dataset / table agent (#514): typed tables whose rows the agent works
		// in the background, proposing structured write-backs a human approves.
		// Reads + review actions live here; run/pause drive the in-process
		// dataset runner via the SetDatasetRunner seam. Import, run and
		// re-run lead to row runs, so they carry the task-create rate limiter
		// (and, in-handler, the budget gate) like /tasks and /tasks/import.
		r.Get("/datasets", h.ListDatasets)
		r.Post("/datasets", h.CreateDataset)
		r.Get("/datasets/{datasetID}", h.GetDataset)
		r.Delete("/datasets/{datasetID}", h.DeleteDataset)
		r.Get("/datasets/{datasetID}/rows", h.ListDatasetRows)
		r.With(h.SchedRateLimitMiddleware).Post("/datasets/{datasetID}/rows", h.ImportDatasetRows)
		r.With(h.SchedRateLimitMiddleware).Post("/datasets/{datasetID}/run", h.RunDataset)
		r.Post("/datasets/{datasetID}/pause", h.PauseDataset)
		r.Post("/datasets/{datasetID}/approve", h.ApproveDatasetRows)
		r.With(h.SchedRateLimitMiddleware).Post("/datasets/{datasetID}/rerun", h.RerunDatasetRows)
		r.Get("/datasets/{datasetID}/export", h.ExportDataset)
		r.Get("/tasks", h.ListTasks)
		// Scheduler UX 2.0 (#504): upcoming-runs timeline (computed from cron +
		// scheduled_for). Static segment, registered with the other task reads.
		r.Get("/tasks/upcoming", h.GetUpcomingRuns)
		// /tasks/tags is registered before /tasks/{task_id} so the static segment
		// wins over the wildcard (#212 tag catalogue). /tasks/export and
		// /tasks/import are likewise static segments that must precede the
		// {task_id} wildcard (#238).
		r.Get("/tasks/tags", h.GetTagCatalogue)
		// Same static-before-wildcard rule: the task form's recipient suggestions.
		r.Get("/tasks/recipients", h.GetRecipientHistory)
		r.Get("/tasks/export", h.HandleTaskExport)
		// Import is a bulk create (the batch path's sibling), so it carries the
		// same task-create rate limiter as /tasks, /tasks/batch and /upload.
		r.With(h.SchedRateLimitMiddleware).Post("/tasks/import", h.HandleTaskImport)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Get("/tasks/{task_id}/output", h.GetTaskOutput)
		r.Get("/tasks/{task_id}/error-analysis", h.GetTaskErrorAnalysis)
		r.Get("/tasks/{task_id}/artifacts", h.GetTaskArtifacts)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Post("/tasks/{task_id}/tags", h.UpdateTaskTags)
		// Admin reassignment of a task's owner (created_by), carried onto the
		// rest of its recurrence chain that later runs continue from. Same
		// placement rationale as /admin/budgets: PermissionAdmin is enforced
		// in-handler.
		r.Put("/tasks/{task_id}/owner", h.SetTaskOwner)
		// Rerun/clone mint a new task each, so they are metered like POST /tasks.
		r.With(h.SchedRateLimitMiddleware).Post("/tasks/{task_id}/rerun", h.RerunTask)
		r.With(h.SchedRateLimitMiddleware).Post("/tasks/{task_id}/clone", h.CloneTask)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		// Permanently remove a task. Deliberately NOT the same route as the
		// cancel above: cancel stops a job and keeps the record, this destroys
		// the record — and only this frees the task's name for reuse (#238's
		// partial unique index outlives a cancelled row).
		r.Delete("/tasks/{task_id}/permanent", h.DeleteTask)
		// Self-improving memory (#516): feedback capture + versioned learned
		// instructions (list / activate a version / deactivate).
		r.Post("/tasks/{task_id}/feedback", h.SubmitFeedback)
		r.Get("/tasks/{task_id}/learned-instructions", h.LearnedInstructions)
		r.Post("/tasks/{task_id}/learned-instructions/{version}/activate", h.LearnedInstructions)
		r.Delete("/tasks/{task_id}/learned-instructions/active", h.LearnedInstructions)
		// ask/notify (#510): the "needs a human answer" queue + resume with an
		// answer (re-queues the paused task). /tasks/paused is registered before
		// /tasks/{task_id} static-vs-param, but chi routes them distinctly.
		r.Get("/tasks/paused", h.ListPausedTasks)
		r.Post("/tasks/{task_id}/resume", h.ResumeTask)
		// self-wake (docs/SELF-WAKE.md): fire a named event at a task parked
		// by wake_on_event; the key must match what the task waits for.
		r.Post("/tasks/{task_id}/wake", h.WakeTask)
		r.Get("/logs/{task_id}", h.GetLogs)
		// Per-attempt run log history: transcripts a retry / ask-resume /
		// wake cycle superseded. Same auth/ownership gate as /logs/{task_id}.
		r.Get("/logs/{task_id}/history", h.GetLogHistory)
		r.Get("/logs/{task_id}/history/{entry_id}", h.GetLogHistoryEntry)
		// Sub-agent child transcript (#1043): the sibling session log a spawned
		// child wrote, gated by the same transcript gate PLUS a linkage check
		// (the child id must appear in this task's persisted log).
		r.Get("/logs/{task_id}/subagents/{child_session_id}", h.GetSubagentLog)
		// Live SSE run-log stream for an in-progress task, falling back to a one-shot
		// replay of the persisted log once finished (#200). Same auth/ownership gate
		// as /logs/{task_id}.
		r.Get("/tasks/{task_id}/stream", h.StreamTaskLogs)
		// Workspace file browser (#287): list + download artifacts the task's agent
		// wrote into its per-run workspace. Stricter than the generic task gate —
		// the handler restricts access to the admin or the task's creator and
		// enforces the shared path-traversal guard on every file access.
		r.Get("/tasks/{task_id}/workspace", h.TaskWorkspace)
		r.Get("/tasks/{task_id}/workspace/*", h.TaskWorkspaceFile)
		r.Get("/stats", h.GetDashboardStats)
		r.Get("/api/me", h.GetCurrentUser)

		// Optional-MCP catalog + credential-account seats for the task-form
		// picker + admin table (read-only; never secret values). The web app
		// proxies /api/orchestrator/mcp-servers + /mcp-accounts to these.
		r.Get("/mcp-servers", h.GetMCPServers)
		r.Get("/mcp-accounts", h.GetMCPAccounts)

		// Read-only task-template catalog for the task-create UI's "new task from a
		// template" affordance (#262). The web app proxies
		// /api/orchestrator/task-templates to this. Never persists or creates a task.
		r.Get("/task-templates", h.ListTaskTemplates)
		r.Get("/prompts", h.PromptLibraryCollection)
		r.Post("/prompts", h.PromptLibraryCollection)
		r.Get("/prompts/export", h.ExportPromptLibrary)
		r.Put("/prompts/{prompt_id}", h.PromptLibraryItem)
		r.Delete("/prompts/{prompt_id}", h.PromptLibraryItem)

		// Notes reads (admin + scoped user). The proposal queue reads gate
		// on PermissionAdmin in-handler, like /admin/usage above.
		r.Get("/notes", notes.ListNotes)
		r.Get("/notes/{slug}", notes.GetNote)
		r.Get("/notes/proposals", notes.ListProposals)
		r.Get("/notes/proposals/{id}", notes.GetProposal)
	})

	// The two high-cost endpoints carry the sliding-window rate limiter
	// (per-API-key + global), so a runaway key can't flood the task queue or
	// drain the LLM budget. The admin key bypasses it (see SchedRateLimitMiddleware).
	r.With(h.SchedRateLimitMiddleware).Post("/tasks", h.CreateTask)
	// Batch task submission (#227): accepts up to MaxBatchSize TaskCreate recipes
	// in one call. Atomic mode wraps the insert in a single transaction; the
	// default (non-atomic) is best-effort with a 207 Multi-Status. Same auth +
	// rate limiter as POST /tasks; the handler additionally charges the scoped
	// key's hourly cap for (N-1) extra tasks so a batch is never a rate-limit
	// bypass.
	r.With(h.SchedRateLimitMiddleware).Post("/tasks/batch", h.CreateTaskBatch)
	// Pre-submission cost forecast (#233): same body, auth, and rate limiter as
	// POST /tasks, but pure local computation — it creates nothing.
	r.With(h.SchedRateLimitMiddleware).Post("/tasks/estimate", h.EstimateTask)
	r.With(h.SchedRateLimitMiddleware).Post("/upload", h.HandleUpload)
	r.Get("/files/{filename}", h.HandleDownload)

	// A2A JSON-RPC endpoint (#1279): every A2A method multiplexes over this one
	// POST (streaming methods answer with SSE), reachable as /v1/a2a via the
	// apiversion wrapper. Outside every auth group — authentication is per
	// JSON-RPC method inside the dispatcher (X-API-Key only, the credential the
	// Agent Card declares), because HTTP-verb key scoping cannot see which
	// method a JSON-RPC body carries. Same rate limiter as POST /tasks; answers
	// 501 until FLEET_A2A_ENABLED wires SetA2A. CSRF-exempt by path
	// (middleware.go): its auth never involves a browser-auto-sent credential.
	r.With(h.SchedRateLimitMiddleware).Post("/a2a", h.A2ARPC)
	// The trailing-slash twin is deliberate, not tidiness: A2A clients built
	// on httpx (the official TCK included) resolve the Agent Card's interface
	// URL as a base and POST to "<url>/", which arrives here as /v1/a2a/ —
	// and chi's exact matching 404'd every such call until this route existed.
	r.With(h.SchedRateLimitMiddleware).Post("/a2a/", h.A2ARPC)

	// Webhook triggers (#177): authenticated by per-trigger HMAC-SHA256, NOT the
	// admin API key, so external services (GitHub, Slack, CI) can fire tasks
	// without admin credentials. Deliberately outside every auth group.
	r.With(h.SchedRateLimitMiddleware).Post("/triggers/{slug}", h.HandleWebhookTrigger)
	// Email triggers (#511): an email provider's inbound-parse webhook posts a
	// normalized email to spawn a governed run. Same HMAC auth + per-slug rate
	// limit as the webhook trigger; the email-kind security controls (approved
	// senders, DKIM/SPF, attachments, dedup) run inside the handler.
	r.With(h.SchedRateLimitMiddleware).Post("/triggers/email/{slug}", h.HandleEmailTrigger)
	// No /auth/login or /auth/logout: the Operations Center's password bearer
	// is retired (#2049 M4, ADR-0105). People sign in and out at the web
	// gateway (/api/auth/*); programs use API keys.

	return r
}

// orchestratorMetricsMiddleware records per-request count + latency for the
// Prometheus /metrics endpoint (#176), labeled by chi route pattern (so high-
// cardinality path params don't explode the series), method, and status.
func orchestratorMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := httpmw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		metrics.RecordHTTPRequest(route, r.Method, strconv.Itoa(ww.Status()), time.Since(start).Seconds())
	})
}
