// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package orchestrator

import "github.com/ElcanoTek/fleet/internal/apispec"

// The security lists most operations share. Each entry is an alternative.
var (
	adminScopedOrSession = apispec.AnyOf("adminApiKey", "scopedApiKey", "elcanoAuthCookie")
	adminKey             = apispec.AnyOf("adminApiKey")
	adminKeyOrSession    = apispec.AnyOf("adminApiKey", "elcanoAuthCookie")
	public               = apispec.Public()
)

// PathParams types the path parameters that are not plain strings.
var PathParams = map[string]*apispec.Schema{
	"datasetID": apispec.Typed("string", "uuid"),
	"version":   apispec.Typed("integer"),
	"entry_id":  apispec.Typed("integer"),
	"child_session_id": {
		Type:    "string",
		Pattern: "^subagent-[0-9a-f-]{36}$",
	},
	"prompt_id": apispec.Typed("string", "uuid"),
	"budget_id": apispec.Typed("string", "uuid"),
}

// Operations describes every route buildOrchestratorMux registers, keyed by
// "METHOD /pattern" exactly as chi registers it.
var Operations = map[string]apispec.Operation{
	"GET /health": {
		Summary:    "Liveness check.",
		Security:   public,
		Permission: "none",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("HealthResponse"))),
		},
	},
	"GET /api-info": {
		Summary: "API version discovery (#321).",
		Description: "Machine-readable version metadata so a client can assert compatibility" +
			" at startup: the API major version (which changes only on a breaking " +
			"change — see docs/api-versioning.md), the fleet binary version, and " +
			"the supported/deprecated version lists. Unauthenticated (same posture " +
			"as /health); reachable at both /api-info and /v1/api-info.",
		Security:   public,
		Permission: "none",
		Responses: []apispec.Response{
			apispec.OK("200", "Version metadata.", apispec.JSON(apispec.Object(
				apispec.Prop("api_version", apispec.Typed("string")),
				apispec.Prop("fleet_version", apispec.Typed("string")),
				apispec.Prop("supported_versions", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("deprecated_versions", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("schema_url", apispec.Typed("string")),
			))),
		},
	},
	"GET /metrics": {
		Summary:    "Prometheus metrics (text exposition format). Admin-API-key gated.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.Media{Type: "text/plain", Schema: apispec.Typed("string")}),
		},
	},
	"GET /api/config": {
		Summary: "Public dashboard config (version, server clock, task-scheduling " +
			"default zone).",
		Security:   public,
		Permission: "none",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Object(
				apispec.Prop("version", apispec.Typed("string")),
				apispec.Prop("timezone", &apispec.Schema{
					Type:        "string",
					Description: "IANA name of the server clock (FLEET_TIMEZONE).",
				}),
				apispec.Prop("server_time", &apispec.Schema{
					Type:   "string",
					Format: "date-time",
					Description: "The orchestrator's current time, formatted in its own configured " +
						"location so the UTC offset travels with it. A client computes its " +
						"clock skew from this once and ticks locally, so a wrong browser clock " +
						"cannot make the dashboard display a confident lie about server time.",
				}),
				apispec.Prop("default_task_timezone", &apispec.Schema{
					Type: "string",
					Description: "IANA zone a scheduled task gets when its create request names none " +
						"(FLEET_DEFAULT_TIMEZONE). Distinct from timezone — this is the zone a " +
						"cron recurrence fires in.",
				}),
			))),
		},
	},
	"GET /.well-known/agent-card.json": {
		Summary: "A2A Agent Card — the protocol's discovery document (#1279).",
		Description: "Public by construction (it names capabilities and the JSON-RPC " +
			"endpoint URL, nothing else; no secrets, no per-deployment identity " +
			"beyond the operator's branding bundle). Spec-fixed well-known path, " +
			"unversioned forever (never Deprecation-tagged on the bare path). " +
			"Served with an ETag + Cache-Control per A2A spec §8.6. Answers 501 " +
			"until the operator sets FLEET_A2A_ENABLED. See docs/A2A.md; spec pin " +
			"internal/a2a.SpecVersion.",
		Security:   public,
		Permission: "none",
		Responses: []apispec.Response{
			apispec.OK("200", "The Agent Card.", apispec.JSON(apispec.Typed("object"))),
			apispec.OK("304", "Not modified (If-None-Match hit the card's ETag)."),
			apispec.OK("501", "The A2A server is not enabled on this deployment."),
		},
	},
	"POST /a2a": {
		Summary: "A2A JSON-RPC 2.0 endpoint — every A2A method over one route (#1279).",
		Description: "The A2A protocol's JSON-RPC binding (spec v1.0.1, JSON-RPC + SSE " +
			"only): SendMessage / SendStreamingMessage / GetTask / ListTasks / " +
			"CancelTask / SubscribeToTask multiplex over this POST; streaming " +
			"methods respond with text/event-stream where each data: line is a " +
			"complete JSON-RPC envelope. Authentication is per JSON-RPC method " +
			"inside the dispatcher and accepts ONLY the credential the Agent Card " +
			"declares — X-API-Key (typed fleet keys or the admin key); " +
			"JSON-RPC-layer errors ride HTTP 200 with the envelope's error member " +
			"(A2A codes -32001..-32009, each carrying a google.rpc.ErrorInfo " +
			"detail). SendMessage runs the same governed create pipeline as POST " +
			"/tasks; reads are creator-scoped (ADR-0043), and an invisible task " +
			"answers TaskNotFound rather than 403 (A2A §3.3.2). Requires the " +
			"A2A-Version: \"1.0\" header. Answers 501 until FLEET_A2A_ENABLED is set." +
			" See docs/A2A.md.",
		Security:    apispec.AnyOf("scopedApiKey", "adminApiKey"),
		Permission:  "authenticated",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Typed("object"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "A JSON-RPC 2.0 response envelope (result or error) — or, for the "+
				"streaming methods, a text/event-stream of envelopes that closes when "+
				"the task reaches a terminal state.", apispec.JSON(apispec.Typed("object")), apispec.Media{Type: "text/event-stream", Schema: apispec.Typed("string")}),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.OK("501", "The A2A server is not enabled on this deployment."),
		},
	},
	"POST /a2a/": {
		Summary: "Trailing-slash twin of POST /a2a (#1279 TCK finding).",
		Description: "Identical to POST /a2a. Exists because A2A clients built on httpx (the" +
			" official TCK included) resolve the Agent Card's interface URL as a " +
			"base and POST to \"<url>/\", which chi's exact matching otherwise 404s. " +
			"Same handler, same auth, same rate limiter.",
		Security:    apispec.AnyOf("scopedApiKey", "adminApiKey"),
		Permission:  "authenticated",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Typed("object"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "A JSON-RPC 2.0 response envelope, as on POST /a2a.", apispec.JSON(apispec.Typed("object")), apispec.Media{Type: "text/event-stream", Schema: apispec.Typed("string")}),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.OK("501", "The A2A server is not enabled on this deployment."),
		},
	},
	"GET /tasks": {
		Summary: "List tasks. Visibility is own-rows for principals without a fleet-wide" +
			" grant (#1082): a non-admin user or scoped API key lists only tasks it" +
			" created; the admin key, PermissionAdmin carriers, and view_all_logs " +
			"holders list every task. The same scope applies to the single-task " +
			"reads, export, and the upcoming projection.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.ArrayOf(apispec.Ref("Task")))),
		},
	},
	"POST /tasks": {
		Summary: "Create (schedule) a task.",
		Description: "Authenticated in-handler — accepts an admin key, a scoped key, or a " +
			"signed-in session (the Elcano cookie). A user whose role is readonly " +
			"is refused with 403 (\"insufficient role: a readonly user cannot modify" +
			" resources\"), as on every mutating route. A task name that is already " +
			"taken answers 409.",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Ref("TaskCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Created", apispec.JSON(apispec.Ref("Task"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.OK("409", "Duplicate task name — a task with that name already exists.", apispec.JSON(apispec.Ref("ErrorResponse"))),
		},
	},
	"POST /tasks/batch": {
		Summary: "Create multiple tasks in a single call.",
		Description: "Accepts a slice of TaskCreate recipes (up to 100) and creates them in " +
			"one API call. When atomic=true the whole batch is validated up front " +
			"and inserted in a single transaction — any failure rolls back all (422" +
			" with per-task errors). When atomic=false (default) valid tasks are " +
			"created while invalid ones are skipped, yielding 207 Multi-Status with" +
			" per-task created/failed results. Same auth + rate limiter as POST " +
			"/tasks; the scoped API key's hourly cap is charged N tokens for a " +
			"batch of N tasks (one by the auth check, N-1 by the handler) so the " +
			"batch path is never a rate-limit bypass. A readonly user is refused " +
			"with 403.\n",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("atomic", &apispec.Schema{
					Type:    "boolean",
					Default: false,
				}),
				apispec.Prop("tasks", &apispec.Schema{
					Type:     "array",
					MaxItems: 100,
					Items:    apispec.Ref("TaskCreate"),
				}),
			},
			Required: []string{"tasks"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "All tasks created (atomic or non-atomic with no failures).", apispec.JSON(apispec.Object(
				apispec.Prop("created", apispec.ArrayOf(apispec.Object(
					apispec.Prop("id", apispec.Typed("string", "uuid")),
					apispec.Prop("index", apispec.Typed("integer")),
				))),
				apispec.Prop("failed", apispec.ArrayOf(apispec.Typed("object"))),
				apispec.Prop("count", apispec.Typed("integer")),
			))),
			apispec.OK("207", "Partial success (non-atomic only); some tasks were skipped."),
			apispec.OK("413", "Batch size exceeds the 100-task limit."),
			apispec.OK("422", "Atomic rollback or every task failed validation."),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("429", "Error"),
		},
	},
	"POST /tasks/estimate": {
		Summary: "Pre-submission cost forecast for a task.",
		Description: "Same request body as POST /tasks, but returns a token + cost forecast " +
			"and creates nothing. Pure local computation (no model call). Returns " +
			"200 with cost fields populated for a model whose pricing is known, or " +
			"202 with null cost fields and an explanatory note when the model's " +
			"pricing is unknown. Same authentication and rate limiter as POST " +
			"/tasks, so a readonly user is refused with 403.\n",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Ref("TaskCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Forecast (model pricing known)", apispec.JSON(apispec.Ref("CostForecast"))),
			apispec.OK("202", "Forecast with null cost fields (model pricing unknown)", apispec.JSON(apispec.Ref("CostForecast"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /tasks/tags": {
		Summary: "List the distinct task tags in use with per-tag task counts (busiest " +
			"first). Visibility is own-rows for principals without a fleet-wide " +
			"grant (#1082): a non-admin user or scoped API key gets only tags on " +
			"tasks it created, with counts over those tasks; the admin key, " +
			"PermissionAdmin carriers, and view_all_logs holders get the fleet-wide" +
			" catalogue.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.ArrayOf(apispec.Object(
				apispec.Prop("tag", apispec.Typed("string")),
				apispec.Prop("task_count", apispec.Typed("integer")),
			)))),
		},
	},
	"GET /tasks/recipients": {
		Summary: "List the email addresses that visible tasks already deliver their " +
			"reports to (the CRITICAL ACTION recipients block the task form " +
			"appends), for the form's Email results suggestions. Most recently used" +
			" first, at most 200, from tasks created in the last 180 days; " +
			"task_count counts distinct jobs, so a recurring task counts once. " +
			"Visibility matches /tasks/tags (#1082): a non-admin user or scoped API" +
			" key gets only the addresses on tasks it created.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.ArrayOf(apispec.Object(
				apispec.Prop("email", apispec.Typed("string")),
				apispec.Prop("task_count", apispec.Typed("integer")),
				apispec.Prop("last_used", apispec.Typed("string", "date-time")),
			)))),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /tasks/export": {
		Summary: "Export task DEFINITIONS (not runs/logs) as a versioned JSON or YAML " +
			"envelope for backup, cross-box migration, and cloning (#238).",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Parameters: []apispec.Parameter{
			{Name: "ids", In: "query", Description: "Comma-separated task UUIDs to export; omit for all tasks.", Schema: apispec.Typed("string")},
			{Name: "format", In: "query", Schema: &apispec.Schema{
				Type:    "string",
				Enum:    []any{"json", "yaml"},
				Default: "json",
			}},
			{Name: "recurrence_only", In: "query", Description: "Export only tasks with a non-empty recurrence (cron tasks).", Schema: &apispec.Schema{
				Type:    "boolean",
				Default: false,
			}},
		},
		Responses: []apispec.Response{
			{Code: "200", Description: "A versioned envelope of task definitions.", Headers: []apispec.Header{{Name: "Content-Disposition", Schema: apispec.Typed("string")}}, Content: []apispec.Media{apispec.JSON(apispec.Object(
				apispec.Prop("version", apispec.Typed("string")),
				apispec.Prop("exported_at", apispec.Typed("string", "date-time")),
				apispec.Prop("tasks", apispec.ArrayOf(apispec.Typed("object"))),
			)), {Type: "application/yaml", Schema: apispec.Typed("object")}}},
			apispec.RefResponse("403", "Error"),
		},
	},
	"POST /tasks/import": {
		Summary: "Import task definitions from a previously exported envelope; creates " +
			"tasks in a single batch with per-task conflict handling (#238).",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		Parameters: []apispec.Parameter{
			{Name: "dry_run", In: "query", Description: "Validate and return the plan without writing.", Schema: &apispec.Schema{
				Type:    "boolean",
				Default: false,
			}},
			{Name: "conflict", In: "query", Description: "Name-collision handling. conflict=replace additionally requires admin.", Schema: &apispec.Schema{
				Type:    "string",
				Enum:    []any{"error", "skip", "replace"},
				Default: "error",
			}},
		},
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("version", apispec.Typed("string")),
			apispec.Prop("exported_at", apispec.Typed("string", "date-time")),
			apispec.Prop("tasks", apispec.ArrayOf(apispec.Typed("object"))),
		)), {Type: "application/yaml", Schema: apispec.Typed("object")}}},
		Responses: []apispec.Response{
			apispec.OK("200", "Import result (all records created/skipped/replaced, or dry_run).", apispec.JSON(apispec.Object(
				apispec.Prop("dry_run", apispec.Typed("boolean")),
				apispec.Prop("total", apispec.Typed("integer")),
				apispec.Prop("created", apispec.Typed("integer")),
				apispec.Prop("skipped", apispec.Typed("integer")),
				apispec.Prop("replaced", apispec.Typed("integer")),
				apispec.Prop("errors", apispec.Typed("integer")),
				apispec.Prop("results", apispec.ArrayOf(apispec.Object(
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("status", &apispec.Schema{
						Type: "string",
						Enum: []any{"created", "skipped", "replaced", "error"},
					}),
					apispec.Prop("id", apispec.Typed("string", "uuid")),
					apispec.Prop("reason", apispec.Typed("string")),
					apispec.Prop("error", apispec.Typed("string")),
				))),
			))),
			apispec.OK("207", "Multi-status — one or more records errored but others succeeded."),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /tasks/{task_id}": {
		Summary:    "Get a task.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("Task"))),
			apispec.RefResponse("404", "Error"),
		},
	},
	"PUT /tasks/{task_id}": {
		Summary: "Update a task.",
		Description: "Updates a pending or scheduled task's definition. Omitting optional " +
			"collection/ceiling/priority fields (mcp_selection, tags, files, " +
			"credential_allowlist, loop_config, worktree_config, retry_policy, " +
			"priority, max_total_tokens) preserves the task's stored values; pass " +
			"an explicit null for max_total_tokens to clear a task-specific token " +
			"ceiling back to inheriting the deployment ceiling, or 0 for priority " +
			"to reset it to normal (50).",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Ref("TaskCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Updated", apispec.JSON(apispec.Ref("Task"))),
		},
	},
	"DELETE /tasks/{task_id}": {
		Summary: "Cancel a task — and stop its live run (#508).",
		Description: "Flips the task to cancelled with who-stopped-it attribution recorded " +
			"on the terminal result (\"stopped by <principal>\"). If the task is " +
			"executing in this server process, the governed run is interrupted at " +
			"its next checkpoint: the sandbox is released, the partial transcript " +
			"persists, and the live stream's terminal frame reports \"stopped\". " +
			"Allowed for a principal with cancel permission on a task it can see " +
			"(its own tasks, or every task for admin and view_all_logs principals)," +
			" or for the task's creator — the user or API key that created it. A " +
			"task outside the caller's scope answers 404. A readonly user is " +
			"refused with 403, even on a task it created.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "Cancelled (attribution on the task's result)."),
			apispec.RefResponse("403", "Error"),
		},
	},
	"DELETE /tasks/{task_id}/permanent": {
		Summary: "Permanently delete a task and its transcripts.",
		Description: "Removes the task row, its run transcripts, and every cascading child " +
			"(webhook triggers, loop config, task memory, feedback, learned " +
			"instructions). Distinct from cancelling: a cancelled task keeps its " +
			"row, and with it its name — which carries a partial unique index, so a" +
			" broken job could never be replaced under the same name and could not " +
			"be renamed either (PUT /tasks/{task_id} only edits pending or " +
			"scheduled tasks). Deleting is the only operation that frees a name for" +
			" reuse.\n" +
			"Refuses with 409 while the task is leased or running: the worker still" +
			" holds the lease and is writing to the row. Stop it first, then " +
			"delete. Same authorization as cancelling — cancel permission on a task" +
			" the caller can see, or the task's creator (user or API key); 404 for " +
			"a task outside the caller's scope.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "Deleted.", apispec.JSON(apispec.Object(
				apispec.Prop("deleted", apispec.Typed("boolean")),
				apispec.Prop("id", apispec.Typed("string")),
			))),
			apispec.OK("403", "Not the task's creator and lacking cancel permission, or a readonly "+
				"user (refused on every mutating route)."),
			apispec.OK("404", "No such task."),
			apispec.OK("409", "The task is running; stop it before deleting."),
		},
	},
	"GET /tasks/{task_id}/output": {
		Summary: "Get a task's validated structured output (#244).",
		Description: "Returns the validated JSON result of a structured-output task (one " +
			"created with an output_schema), raw and unwrapped, for direct " +
			"programmatic consumption. output_json and terminal success are " +
			"committed atomically under the worker lease, before success " +
			"notifications or recurrence side effects. Therefore every successful " +
			"structured task returns 200 here. 404 means no output_schema was " +
			"declared. 409 means the task is not yet terminal — poll again later. " +
			"410 means the task failed terminally: the declared output will never " +
			"exist for this run, so stop polling. 500 marks a legacy/corrupt " +
			"invariant-violating success row (no output_json, or stored bytes that " +
			"are not valid JSON).\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The validated structured output (shape defined by the task's "+
				"output_schema).", apispec.JSON(&apispec.Schema{})),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("410", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /tasks/{task_id}/error-analysis": {
		Summary: "Get a task's post-failure error diagnosis (#317).",
		Description: "Returns the asynchronous LLM diagnosis of a TERMINALLY-failed task — a" +
			" structured classification plus suggested remediation — raw and " +
			"unwrapped, for direct programmatic consumption. The diagnosis is " +
			"produced off-thread after a terminal failure with a cheap model and a " +
			"hard timeout. 404 when the task has no error analysis (it did not fail" +
			" terminally, analysis is disabled, or the diagnosis was not produced);" +
			" 409 while the task has not yet reached a terminal state.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The error diagnosis.", apispec.JSON(&apispec.Schema{
				Type: "object",
				Properties: []apispec.Property{
					apispec.Prop("category", &apispec.Schema{
						Type: "string",
						Enum: []any{"configuration", "credentials", "network", "rate_limit", "timeout", "tool_error", "model_refusal", "resource_exhausted", "logic_error", "unknown"},
					}),
					apispec.Prop("summary", apispec.Typed("string")),
					apispec.Prop("remediation", apispec.ArrayOf(apispec.Typed("string"))),
				},
				Required: []string{"category", "summary"},
			})),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"GET /tasks/{task_id}/artifacts": {
		Summary: "List a task's published output artifacts (#204).",
		Description: "Returns the curated manifest of named output files the run's agent " +
			"explicitly published via the publish_artifact tool — a JSON array of " +
			"{name, path, description, size} — distinct from the raw per-run " +
			"workspace the workspace endpoints expose. Each entry's path is " +
			"downloadable via the task's workspace file endpoint. Restricted to the" +
			" task's creator (or an admin), like the workspace file endpoints — the" +
			" manifest indexes the creator-private workspace. 404 when the run " +
			"published no artifacts; 409 while the task has not yet reached a " +
			"terminal state.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The published-artifact manifest.", apispec.JSON(apispec.ArrayOf(&apispec.Schema{
				Type: "object",
				Properties: []apispec.Property{
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("path", apispec.Typed("string")),
					apispec.Prop("description", apispec.Typed("string")),
					apispec.Prop("size", apispec.Typed("integer", "int64")),
				},
				Required: []string{"name", "path", "size"},
			}))),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"POST /tasks/{task_id}/tags": {
		Summary:    "Atomically add and/or remove tags on a task.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("add", apispec.ArrayOf(apispec.Typed("string"))),
			apispec.Prop("remove", apispec.ArrayOf(apispec.Typed("string"))),
		))}},
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("Task"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /tasks/{task_id}/rerun": {
		Summary: "Re-run a task once now (a new task copied from this one, optional " +
			"overrides).",
		Description: "The copy belongs to the calling user. A caller with no user behind it " +
			"(the admin API key or a scoped API key) gives the copy the source's " +
			"owner (created_by) instead — and, for the admin key, the source's " +
			"authorizing key — so the copy resolves the same hosted connections and" +
			" reaches the same owner digest as the task it was copied from. By " +
			"default the copy joins the source's lineage (its per-job workspace " +
			"directory and owner-digest entry); overrides.new_lineage starts a job " +
			"of its own. The copy's effective mcp_selection must resolve: a server " +
			"that is neither in the active bundle catalog nor among the copy " +
			"owner's hosted connections is a 400 carrying the message its run would" +
			" have dead-lettered with.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("overrides", apispec.Object(
				apispec.Prop("prompt", apispec.Typed("string")),
				apispec.Prop("model", apispec.Typed("string")),
				apispec.Prop("fallback_model", apispec.Typed("string")),
				apispec.Prop("max_iterations", apispec.Typed("integer")),
				apispec.Prop("priority", apispec.Typed("integer")),
				apispec.Prop("allow_network", apispec.Typed("boolean")),
				apispec.Prop("description", apispec.Typed("string")),
				apispec.Prop("title", &apispec.Schema{
					Type:        "string",
					Description: "Relabel the copy; omitted inherits the source's title.",
				}),
				apispec.Prop("tags", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("files", &apispec.Schema{
					Type: "array",
					Description: "Replaces attachments; omitted inherits, an empty array clears. Values " +
						"are uploaded filenames.",
					Items: apispec.Typed("string"),
				}),
				apispec.Prop("file_names", &apispec.Schema{
					Type: "array",
					Description: "Optional logical names paired 1:1 with files. Replacing files clears " +
						"inherited names unless supplied here.",
					Items: apispec.Typed("string"),
				}),
				apispec.Prop("allow_delegation", apispec.Typed("boolean")),
				apispec.Prop("thinking_budget_tokens", &apispec.Schema{
					Type:        "integer",
					Description: "Negative clears to the global default; omitted inherits the source.",
				}),
				apispec.Prop("persona", apispec.Typed("string")),
				apispec.Prop("mcp_selection", apispec.Ref("MCPSelection")),
				apispec.Prop("expected_duration_minutes", &apispec.Schema{
					Type: "integer",
					Description: "SLA expectation for the copy; 0 clears it, omitted inherits the " +
						"source.",
				}),
				apispec.Prop("sla_warn_multiplier", &apispec.Schema{
					Type:        "number",
					Description: "0 = the default (1.5); omitted inherits.",
				}),
				apispec.Prop("sla_fail_multiplier", &apispec.Schema{
					Type:        "number",
					Description: "0 = the default (2.0); omitted inherits.",
				}),
				apispec.Prop("sandbox_limits", &apispec.Schema{
					Type: "object",
					Description: "Per-run sandbox cgroup override; an explicit null (or an all-zero " +
						"object) clears to the global defaults, omitted inherits.",
					Nullable: true,
					Properties: []apispec.Property{
						apispec.Prop("memory_mb", apispec.Typed("integer")),
						apispec.Prop("cpus", apispec.Typed("number")),
						apispec.Prop("pids", apispec.Typed("integer")),
					},
				}),
				apispec.Prop("run_if", &apispec.Schema{
					Type: "object",
					Description: "Pre-run shell gate for the copy (POSIX sh); an explicit null clears " +
						"it, omitted inherits.",
					Nullable: true,
					Properties: []apispec.Property{
						apispec.Prop("command", apispec.Typed("string")),
						apispec.Prop("exit_code_is", apispec.Typed("integer")),
						apispec.Prop("timeout_seconds", apispec.Typed("integer")),
					},
				}),
				apispec.Prop("carry_context", apispec.Typed("boolean")),
				apispec.Prop("instruction_self_improve", apispec.Typed("boolean")),
				apispec.Prop("new_lineage", &apispec.Schema{
					Type: "boolean",
					Description: "true gives the copy a lineage of its own (its own per-job workspace " +
						"directory and owner-digest entry) — use it when the copy is a " +
						"different job. Omitted / false joins the source's lineage, which " +
						"Resubmit with changes relies on.",
				}),
			)),
		))}},
		Responses: []apispec.Response{
			apispec.OK("201", "Created", apispec.JSON(apispec.Ref("Task"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /tasks/{task_id}/clone": {
		Summary: "Clone a task (a new task copied from this one, preserving recurrence; " +
			"optional overrides).",
		Description: "Same ownership, lineage and mcp_selection rules as POST " +
			"/tasks/{task_id}/rerun: an admin-key or scoped-key clone keeps the " +
			"source's owner, overrides.new_lineage starts a job of its own, and an " +
			"unresolvable mcp_selection is a 400.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("overrides", &apispec.Schema{
				Type: "object",
				Description: "Accepts the same override fields as POST /tasks/{task_id}/rerun, " +
					"including files, file_names and new_lineage.",
			}),
		))}},
		Responses: []apispec.Response{
			apispec.OK("201", "Created", apispec.JSON(apispec.Ref("Task"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"PUT /tasks/{task_id}/owner": {
		Summary: "Set the user who owns a task (admin).",
		Description: "Sets created_by — whose hosted connections the task's runs resolve, " +
			"who receives its owner digest, and who may read and edit it as its " +
			"creator — on the task, on every unfinished later occurrence of its " +
			"recurrence chain, and on the chain's last occurrence (the chain " +
			"continues from it: a pending spawn or a replay of a parked " +
			"dead-letter). The chain is followed through previous_occurrence_id, " +
			"not lineage_id, so other jobs copied from the same source keep their " +
			"owner; occurrences spawned afterwards inherit the new owner, and " +
			"finished occurrences in between keep theirs. The mcp_selection of " +
			"every row it rewrites must resolve for the new owner (400 naming the " +
			"row otherwise, with the message its run would fail with; nothing is " +
			"written). Admin-only, enforced in-handler on the admin permission (the" +
			" admin API key, a scoped key carrying admin, or an admin-role " +
			"session). CLI twin: fleet sched task set-owner <task_id> <username>.",
		Security:   adminScopedOrSession,
		Permission: "admin",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("username", &apispec.Schema{
					Type: "string",
					Description: "An existing, enabled user's username (on a standard deployment, their " +
						"email).",
				}),
			},
			Required: []string{"username"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "The task as it now reads, and every row the reassignment rewrote (the "+
				"task first, then the chain rows in chain order).", apispec.JSON(&apispec.Schema{
				Type: "object",
				Properties: []apispec.Property{
					apispec.Prop("task", apispec.Ref("Task")),
					apispec.Prop("updated_task_ids", apispec.ArrayOf(apispec.Typed("string", "uuid"))),
				},
				Required: []string{"task", "updated_task_ids"},
			})),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /tasks/{task_id}/stream": {
		Summary: "Stream a task's run log live (SSE).",
		Description: "Server-Sent Events stream of the task's run log. While the task is in " +
			"flight the in-memory event buffer is tailed live, with Last-Event-ID " +
			"replay on reconnect; once the task has finished the persisted log is " +
			"replayed one-shot. Event types: agent_message, tool_call, tool_result," +
			" and a terminal status. Same auth + ownership gate as GET " +
			"/logs/{task_id}.\n",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "An SSE stream of run-log events.", apispec.Media{Type: "text/event-stream", Schema: apispec.Typed("string")}),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"GET /tasks/{task_id}/workspace": {
		Summary: "List (or ZIP-export) the files an agent created in a completed task's " +
			"workspace.",
		Description: "Lists the files the task's agent wrote into its per-run workspace " +
			"directory (reports, data, code, charts). Access is restricted to the " +
			"admin or the task's creator (the creating user or API key) — strictly " +
			"more than ordinary task visibility, because workspace files are " +
			"private artifacts. Only available for completed tasks (success/error);" +
			" a running task's workspace is mid-write. With ?format=zip the entire " +
			"workspace is streamed as a ZIP archive (capped at " +
			"FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES total uncompressed size, default 50" +
			" MiB).\n",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Parameters: []apispec.Parameter{
			{Name: "format", In: "query", Description: "When \"zip\", stream the whole workspace as a ZIP archive instead of the" +
				" JSON listing.", Schema: &apispec.Schema{
				Type: "string",
				Enum: []any{"zip"},
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The workspace listing (JSON), or the ZIP archive when ?format=zip.", apispec.JSON(apispec.Ref("WorkspaceListing")), apispec.Media{Type: "application/zip", Schema: apispec.Typed("string", "binary")}),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("413", "Error"),
		},
	},
	"GET /tasks/{task_id}/workspace/*": {
		Summary: "Download a single file from a completed task's workspace.",
		Description: "Streams one file from the task's per-run workspace as an attachment. " +
			"The trailing path is interpreted relative to the workspace root and is" +
			" guarded against traversal (\"..\"/absolute/NUL paths are rejected; " +
			"symlinks that escape the workspace root are not followable). Access is" +
			" restricted to the admin or the task's creator. A file larger than " +
			"FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES (default 50 MiB) returns 413 and " +
			"must be fetched via the CLI.\n",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "The file contents, as an attachment.", apispec.Media{Type: "application/octet-stream", Schema: apispec.Typed("string", "binary")}),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("413", "Error"),
		},
	},
	"POST /tasks/cleanup": {
		Summary:    "Cleanup completed/old task history.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
		},
	},
	"POST /tasks/model": {
		Summary: "Bulk re-assign the pinned model/fallback across scheduled tasks " +
			"(admin).",
		Description: "Fleet-wide mutation; admin-only. Optionally limited to tasks pinned to" +
			" from_model. dry_run returns the matched tasks without writing.",
		Security:   adminKey,
		Permission: "admin",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("model", apispec.Typed("string")),
				apispec.Prop("fallback_model", &apispec.Schema{
					Type: "string",
					Description: "Omit to leave each task's existing fallback_model untouched; send an " +
						"empty string to clear it to NULL; send a slug to set it.",
				}),
				apispec.Prop("from_model", apispec.Typed("string")),
				apispec.Prop("dry_run", apispec.Typed("boolean")),
			},
			Required: []string{"model"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
			apispec.RefResponse("400", "Error"),
		},
	},
	"GET /admin/migrations": {
		Summary: "Migration status — applied vs pending migrations of the one sequence (#256, #2049 M6).",
		Description: "Read-only operator view of the database's schema migrations: one " +
			"sequence (legacy chat 1–99, legacy sched as 101–177, new migrations " +
			"from 200) recorded in one ledger, `core.schema_versions` (ADR-0107, " +
			"docs/MIGRATIONS.md). `current_version` is the highest applied version, " +
			"`dirty` says a migration was left half-applied (fleet refuses to start " +
			"until `fleet migrate force`), `adopted` whether the database has moved " +
			"onto the one ledger yet (before it has, `applied` is what adoption will " +
			"record from the legacy ledgers, described in `legacy`). Versions the " +
			"build does not ship are listed under `applied` (and `unknown`), so a " +
			"downgrade is visible; `unknown`, `mismatched` (an edited migration) and " +
			"`dirty` are what fleet refuses to start on. Applies nothing — safe to call any time. Admin-only. The chat " +
			"server answers the same report at the same path on its port. The keys " +
			"of the pre-M6 per-store reports are kept; `layout` is `unified`.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Migration status of the one database.", apispec.JSON(apispec.Object(
				apispec.Prop("db", &apispec.Schema{Type: "string", Description: `always "fleet"`}),
				apispec.Prop("runner", &apispec.Schema{Type: "string", Description: `always "fleet"`}),
				apispec.Prop("migration_table", &apispec.Schema{Type: "string", Description: `always "core.schema_versions"`}),
				apispec.Prop("layout", &apispec.Schema{Type: "string", Enum: []any{"unified"}}),
				apispec.Prop("adopted", apispec.Typed("boolean")),
				apispec.Prop("current_version", &apispec.Schema{
					Type:     "integer",
					Nullable: true,
				}),
				apispec.Prop("dirty", apispec.Typed("boolean")),
				apispec.Prop("legacy", &apispec.Schema{Type: "string", Description: "the legacy ledgers, before adoption"}),
				apispec.Prop("applied", &apispec.Schema{Type: "array", Nullable: true, Items: apispec.Object(
					apispec.Prop("version", apispec.Typed("integer")),
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("applied_at", &apispec.Schema{Type: "integer", Description: "Unix seconds"}),
				)}),
				apispec.Prop("pending", &apispec.Schema{Type: "array", Nullable: true, Items: apispec.Object(
					apispec.Prop("version", apispec.Typed("integer")),
					apispec.Prop("name", apispec.Typed("string")),
				)}),
				apispec.Prop("unknown", &apispec.Schema{Type: "array", Description: "applied versions this build does not ship (a newer fleet migrated the database); fleet refuses to start", Items: apispec.Object(
					apispec.Prop("version", apispec.Typed("integer")),
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("applied_at", &apispec.Schema{Type: "integer", Description: "Unix seconds"}),
				)}),
				apispec.Prop("mismatched", &apispec.Schema{Type: "array", Description: "applied versions recorded with another name or checksum than this build's file (an edited migration); fleet refuses to start", Items: apispec.Object(
					apispec.Prop("version", apispec.Typed("integer")),
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("applied_at", &apispec.Schema{Type: "integer", Description: "Unix seconds"}),
				)}),
			))),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /datasets": {
		Summary: "List datasets (#514).",
		Description: "The caller's datasets (table agents) with per-status row counts, " +
			"newest first. Datasets are scoped to their creator like tasks: a " +
			"principal lists the datasets it created, and admin and view_all_logs " +
			"principals list every dataset. A dataset with no recorded creator is " +
			"listed only for those fleet-wide principals. Every " +
			"/datasets/{datasetID} route answers 404 for a dataset outside the " +
			"caller's scope.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The dataset list.", apispec.JSON(apispec.Object(
				apispec.Prop("datasets", apispec.ArrayOf(apispec.Ref("Dataset"))),
			))),
		},
	},
	"POST /datasets": {
		Summary: "Create a dataset (#514).",
		Description: "Defines a table agent: a name, the per-row goal, typed columns (at " +
			"least one output column — those define the structured write-back " +
			"schema), the pinned model each row runs at, and an optional persona + " +
			"worker concurrency (1..8, default 2).",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("name", &apispec.Schema{
					Type:      "string",
					MaxLength: 128,
				}),
				apispec.Prop("goal", &apispec.Schema{
					Type:      "string",
					MaxLength: 8000,
				}),
				apispec.Prop("columns", apispec.ArrayOf(apispec.Ref("DatasetColumn"))),
				apispec.Prop("model", apispec.Typed("string")),
				apispec.Prop("persona", apispec.Typed("string")),
				apispec.Prop("concurrency", &apispec.Schema{
					Type:    "integer",
					Minimum: 0,
					Maximum: 8,
				}),
			},
			Required: []string{"name", "goal", "columns", "model"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "The created dataset.", apispec.JSON(apispec.Ref("Dataset"))),
			apispec.RefResponse("400", "Error"),
		},
	},
	"GET /datasets/{datasetID}": {
		Summary:    "Get one dataset (#514).",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The dataset with row counts.", apispec.JSON(apispec.Ref("Dataset"))),
			apispec.RefResponse("404", "Error"),
		},
	},
	"DELETE /datasets/{datasetID}": {
		Summary:     "Delete a dataset and its rows (#514).",
		Description: "Refused (409) while the dataset is running — pause it first.",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		Responses: []apispec.Response{
			apispec.OK("204", "Deleted."),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"GET /datasets/{datasetID}/rows": {
		Summary: "List a dataset's rows (#514).",
		Description: "Ordered by row_index; filter with ?status= and page with " +
			"?limit=&offset= (default limit 200). A limit outside 1-1000, a " +
			"negative offset, or a non-integer value for either is a 400.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Parameters: []apispec.Parameter{
			{Name: "status", In: "query", Schema: &apispec.Schema{
				Type: "string",
				Enum: []any{"pending", "running", "proposed", "approved", "failed"},
			}},
			{Name: "limit", In: "query", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 200,
				Minimum: 1,
				Maximum: 1000,
			}},
			{Name: "offset", In: "query", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 0,
				Minimum: 0,
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The rows plus the dataset's per-status counts."),
			apispec.OK("400", "limit or offset is not an integer in range.", apispec.JSON(apispec.Object(
				apispec.Prop("rows", apispec.ArrayOf(apispec.Ref("DatasetRow"))),
				apispec.Prop("row_counts", &apispec.Schema{
					Type:                 "object",
					AdditionalProperties: apispec.Typed("integer"),
				}),
			))),
		},
	},
	"POST /datasets/{datasetID}/rows": {
		Summary: "Import rows (#514).",
		Description: "Appends rows from JSON ({\"rows\":[{column: value}]}) or CSV " +
			"(Content-Type text/csv; the header row names INPUT columns). Values " +
			"may only target input columns; unknown names are rejected. Capped at " +
			"5000 rows / 16 MiB per request.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("rows", apispec.ArrayOf(apispec.Typed("object"))),
		)), {Type: "text/csv", Schema: apispec.Typed("string")}}},
		Responses: []apispec.Response{
			apispec.OK("200", "Rows imported.", apispec.JSON(apispec.Object(
				apispec.Prop("imported", apispec.Typed("integer")),
			))),
			apispec.RefResponse("400", "Error"),
		},
	},
	"POST /datasets/{datasetID}/run": {
		Summary: "Start working the dataset's pending rows (#514).",
		Description: "Claims the dataset (idle|paused → running) and fans out its configured" +
			" worker concurrency; each row runs through the governed loop and lands" +
			" as a PROPOSED write-back for review. 409 when already running. " +
			"Carries the task-create rate limit (429) and the per-principal budget " +
			"gate (402 when the caller's budget window is exhausted), as do row " +
			"import and re-run.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		Responses: []apispec.Response{
			apispec.OK("200", "Run started.", apispec.JSON(apispec.Object(
				apispec.Prop("status", apispec.Typed("string")),
			))),
			apispec.RefResponse("402", "Error"),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("429", "Error"),
			apispec.RefResponse("503", "Error"),
		},
	},
	"POST /datasets/{datasetID}/pause": {
		Summary: "Pause a running dataset (#514).",
		Description: "Cancels the active run between rows; pending rows stay pending and " +
			"resume on the next run.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		Responses: []apispec.Response{
			apispec.OK("200", "Paused.", apispec.JSON(apispec.Object(
				apispec.Prop("status", apispec.Typed("string")),
			))),
			apispec.RefResponse("409", "Error"),
			apispec.RefResponse("503", "Error"),
		},
	},
	"POST /datasets/{datasetID}/approve": {
		Summary: "Approve proposed write-backs (#514 review queue).",
		Description: "Merges each proposed object into the row's cells and marks the row " +
			"approved. Empty/omitted row_ids approves EVERY proposed row (bulk " +
			"approve). Only rows in status \"proposed\" are touched.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("row_ids", apispec.ArrayOf(apispec.Typed("string", "uuid"))),
		))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Rows approved.", apispec.JSON(apispec.Object(
				apispec.Prop("approved", apispec.Typed("integer")),
			))),
		},
	},
	"POST /datasets/{datasetID}/rerun": {
		Summary: "Reset rows to pending for a re-run (#514).",
		Description: "The given row_ids (any non-running status), or EVERY failed row when " +
			"omitted (bulk retry). Clears proposed/error state; approved cells " +
			"stay.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Object(
			apispec.Prop("row_ids", apispec.ArrayOf(apispec.Typed("string", "uuid"))),
		))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Rows reset.", apispec.JSON(apispec.Object(
				apispec.Prop("reset", apispec.Typed("integer")),
			))),
		},
	},
	"GET /datasets/{datasetID}/export": {
		Summary: "Export the dataset as CSV (#514).",
		Description: "All columns (approved values merged) plus _status/_note/_error per " +
			"row.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The CSV export.", apispec.Media{Type: "text/csv", Schema: apispec.Typed("string")}),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /tasks/{task_id}/feedback": {
		Summary: "Submit feedback on a task's output (#516 self-improving memory).",
		Description: "Records a thumbs up/down (optional critique) signal. When fresh " +
			"down-signals cross the distillation threshold and self-improvement is " +
			"enabled, a learned instruction is DISTILLED and staged as 'proposed' " +
			"(off-thread) — it does not change behavior until activated. A readonly" +
			" user is refused with 403.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("rating", &apispec.Schema{
					Type: "string",
					Enum: []any{"up", "down"},
				}),
				apispec.Prop("critique", apispec.Typed("string")),
			},
			Required: []string{"rating"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "The recorded feedback.", apispec.JSON(apispec.Ref("TaskFeedback"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /tasks/{task_id}/learned-instructions": {
		Summary:    "List a task's learned instructions (#516), newest version first.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "The task's learned instructions.", apispec.JSON(apispec.Object(
				apispec.Prop("learned_instructions", apispec.ArrayOf(apispec.Ref("TaskLearnedInstruction"))),
			))),
		},
	},
	"POST /tasks/{task_id}/learned-instructions/{version}/activate": {
		Summary: "Activate (or revert to) a learned-instruction version (#516).",
		Description: "Makes the version the task's sole active learned instruction " +
			"(archiving any prior active) — this is both the activate and the " +
			"revert primitive. Requires operator (cancel) permission.",
		Security:   adminScopedOrSession,
		Permission: "cancel_task",
		Responses: []apispec.Response{
			apispec.OK("200", "The activated instruction.", apispec.JSON(apispec.Ref("TaskLearnedInstruction"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"DELETE /tasks/{task_id}/learned-instructions/active": {
		Summary: "Deactivate the active learned instruction (#516 full revert).",
		Description: "Archives the task's active learned instruction so future runs inject " +
			"none. Requires operator (cancel) permission.",
		Security:   adminScopedOrSession,
		Permission: "cancel_task",
		Responses: []apispec.Response{
			apispec.OK("200", "Whether an instruction was active.", apispec.JSON(apispec.Object(
				apispec.Prop("deactivated", apispec.Typed("boolean")),
			))),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /tasks/paused": {
		Summary: "List tasks paused awaiting a human answer (#510).",
		Description: "The \"needs a human answer\" queue — tasks whose agent called `ask` and " +
			"parked in paused_awaiting_input (holding no sandbox/lease). Newest " +
			"first; ?limit= outside 1-1000 (or non-integer) is a 400.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Parameters: []apispec.Parameter{
			{Name: "limit", In: "query", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 100,
				Minimum: 1,
				Maximum: 1000,
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The paused tasks."),
			apispec.OK("400", "limit is not an integer in range.", apispec.JSON(apispec.Object(
				apispec.Prop("tasks", apispec.ArrayOf(apispec.Ref("Task"))),
			))),
		},
	},
	"POST /tasks/{task_id}/resume": {
		Summary: "Answer a paused task's question and re-queue it (#510).",
		Description: "Supplies the human answer to a task in paused_awaiting_input and " +
			"re-queues it (status → pending); the next run is given the Q&A and " +
			"continues. 409 when the task is not paused. Requires operator (cancel)" +
			" permission; a readonly user is refused with 403.",
		Security:   adminScopedOrSession,
		Permission: "cancel_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("answer", apispec.Typed("string")),
			},
			Required: []string{"answer"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "Re-queued.", apispec.JSON(apispec.Object(
				apispec.Prop("status", apispec.Typed("string")),
			))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"POST /tasks/{task_id}/wake": {
		Summary: "Fire a named event at a task parked by wake_on_event (self-wake).",
		Description: "Re-queues a task in paused_awaiting_wake early (status → pending); the" +
			" event key must match the one the task is waiting for, so a caller can" +
			" never wake an arbitrary sleeping task. The next run is told the event" +
			" fired (with the optional note). 409 when the task is not awaiting a " +
			"wake or waits for a different event. Requires operator (cancel) " +
			"permission, mirroring /tasks/{task_id}/resume; a readonly user is " +
			"refused with 403. See docs/SELF-WAKE.md.",
		Security:   adminScopedOrSession,
		Permission: "cancel_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(&apispec.Schema{
			Type: "object",
			Properties: []apispec.Property{
				apispec.Prop("event", apispec.Typed("string")),
				apispec.Prop("note", apispec.Typed("string")),
			},
			Required: []string{"event"},
		})}},
		Responses: []apispec.Response{
			apispec.OK("200", "Re-queued.", apispec.JSON(apispec.Object(
				apispec.Prop("status", apispec.Typed("string")),
			))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"GET /tasks/upcoming": {
		Summary: "Upcoming scheduled runs — calendar/timeline feed (#504).",
		Description: "Projects future executions from the existing scheduler data: up to a " +
			"few cron occurrences per recurring task (in the task's timezone) plus " +
			"each one-shot's scheduled_for, sorted by time. A computed view (no " +
			"run-records table). ?limit= outside 1-1000 (or non-integer) is a 400, " +
			"as is an ?until= that is not an RFC3339 timestamp.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Parameters: []apispec.Parameter{
			{Name: "limit", In: "query", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 50,
				Minimum: 1,
				Maximum: 1000,
			}},
			{Name: "until", In: "query", Description: "Horizon-based projection: every occurrence of every recurring task up " +
				"to this RFC3339 instant (capped by limit), instead of the count-based " +
				"projection of a few occurrences per task.", Schema: apispec.Typed("string", "date-time")},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "Upcoming runs, soonest first."),
			apispec.OK("400", "limit is not an integer in range, or until is not RFC3339.", apispec.JSON(apispec.Object(
				apispec.Prop("upcoming", apispec.ArrayOf(apispec.Object(
					apispec.Prop("task_id", apispec.Typed("string")),
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("title", &apispec.Schema{
						Type:        "string",
						Description: "The task's display label; survives every occurrence, unlike name.",
					}),
					apispec.Prop("prompt", apispec.Typed("string")),
					apispec.Prop("recurrence", apispec.Typed("string")),
					apispec.Prop("next_run", apispec.Typed("string", "date-time")),
					apispec.Prop("recurring", apispec.Typed("boolean")),
				))),
			))),
		},
	},
	"GET /admin/pipeline-metrics": {
		Summary: "Tool-pipeline metrics derived from retained session logs (#543).",
		Description: "The sensor behind data-driven optimization decisions (see #505's " +
			"reopen criteria): per-run tool turns / total + distinct tool calls / " +
			"tokens / cost / wall clock, derived from the session logs fleet " +
			"already persists, plus a fleet-wide aggregate (tool-turn histogram and" +
			" the share of runs that were long multi-tool pipelines). Admin-only.",
		Security:   adminKey,
		Permission: "admin",
		Parameters: []apispec.Parameter{
			{Name: "runs", In: "query", Description: "Cap on per-run rows returned (most recent first); the aggregate always" +
				" covers every retained log.", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 100,
				Maximum: 500,
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "Aggregate + most-recent per-run pipeline metrics.", apispec.JSON(apispec.Object(
				apispec.Prop("aggregate", apispec.Object(
					apispec.Prop("runs", apispec.Typed("integer")),
					apispec.Prop("tool_turn_histogram", &apispec.Schema{
						Type:                 "object",
						AdditionalProperties: apispec.Typed("integer"),
					}),
					apispec.Prop("pct_runs_at_least_5_tool_turns", apispec.Typed("number")),
					apispec.Prop("avg_tool_turns", apispec.Typed("number")),
					apispec.Prop("avg_distinct_tools", apispec.Typed("number")),
					apispec.Prop("avg_prompt_tokens", apispec.Typed("number")),
					apispec.Prop("avg_completion_tokens", apispec.Typed("number")),
					apispec.Prop("avg_wall_clock_seconds", apispec.Typed("number")),
				)),
				apispec.Prop("runs", apispec.ArrayOf(apispec.Object(
					apispec.Prop("task_id", apispec.Typed("string")),
					apispec.Prop("tool_turns", apispec.Typed("integer")),
					apispec.Prop("total_tool_calls", apispec.Typed("integer")),
					apispec.Prop("distinct_tools", apispec.Typed("integer")),
					apispec.Prop("max_calls_in_one_turn", apispec.Typed("integer")),
					apispec.Prop("prompt_tokens", apispec.Typed("integer")),
					apispec.Prop("completion_tokens", apispec.Typed("integer")),
					apispec.Prop("cost_usd", apispec.Typed("number")),
					apispec.Prop("wall_clock_seconds", apispec.Typed("integer")),
					apispec.Prop("created_at", apispec.Typed("integer")),
				))),
			))),
			apispec.RefResponse("401", "Error"),
		},
	},
	"GET /admin/queue": {
		Summary: "Pending task-queue stats — per-tier depth + oldest wait (#230).",
		Description: "Operator view of the pending task queue: total depth, the oldest " +
			"pending wait, and the count + oldest wait per named priority tier " +
			"(critical/high/normal/low/bulk). Lower priority integer = higher " +
			"urgency. Admin-only — backlog data is operator-sensitive.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Pending-queue depth and oldest wait, overall and per tier.", apispec.JSON(apispec.Object(
				apispec.Prop("pending_total", apispec.Typed("integer")),
				apispec.Prop("oldest_age_seconds", apispec.Typed("integer")),
				apispec.Prop("tiers", apispec.ArrayOf(apispec.Object(
					apispec.Prop("tier", apispec.Typed("string")),
					apispec.Prop("min_priority", apispec.Typed("integer")),
					apispec.Prop("max_priority", apispec.Typed("integer")),
					apispec.Prop("count", apispec.Typed("integer")),
					apispec.Prop("oldest_age_seconds", apispec.Typed("integer")),
				))),
			))),
			apispec.RefResponse("500", "Error"),
		},
	},
	"POST /admin/reload-config": {
		Summary: "Hot-reload reloadable settings without a restart (#286).",
		Description: "Re-reads the small, explicitly safe subset of settings that a running " +
			"fleet may change mid-flight — the per-run cost/token ceilings, the " +
			"per-turn iteration ceiling, and the sampling temperature shared by " +
			"interactive and scheduled runs (FLEET_MAX_COST_USD, " +
			"FLEET_MAX_TOTAL_TOKENS, FLEET_MAX_ITERATIONS, FLEET_TEMPERATURE). " +
			"Values are re-read from the env file (FLEET_ENV_FILE) and process " +
			"environment with the same precedence as boot. Equivalent to sending " +
			"SIGUSR2. Settings that are bound into a listener, connection pool, or " +
			"signing context at startup (bind addresses, DB DSNs, auth secrets, the" +
			" admission-semaphore size, TLS) are NOT reloadable and are reported " +
			"under `skipped` when changed. Admin-only.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "A diff of what changed, was skipped (needs restart), or errored.", apispec.JSON(apispec.Object(
				apispec.Prop("reloaded_at", apispec.Typed("string", "date-time")),
				apispec.Prop("changed", apispec.ArrayOf(apispec.Object(
					apispec.Prop("key", apispec.Typed("string")),
					apispec.Prop("old", apispec.Typed("string")),
					apispec.Prop("new", apispec.Typed("string")),
				))),
				apispec.Prop("skipped", apispec.ArrayOf(apispec.Object(
					apispec.Prop("key", apispec.Typed("string")),
					apispec.Prop("reason", apispec.Typed("string")),
				))),
				apispec.Prop("errors", apispec.ArrayOf(apispec.Object(
					apispec.Prop("key", apispec.Typed("string")),
					apispec.Prop("error", apispec.Typed("string")),
				))),
			))),
			apispec.RefResponse("500", "Error"),
			apispec.RefResponse("503", "Error"),
		},
	},
	"POST /admin/mcp-servers/reload": {
		Summary: "Hot-reload the MCP server catalog without a restart (#218).",
		Description: "Re-reads the client-config bundle's MCP catalog (manifest.yaml) and " +
			"applies the minimum set of server mutations to the live interactive " +
			"agent — starting newly-added servers, draining + closing removed ones," +
			" and restarting changed ones — WITHOUT tearing down unchanged servers " +
			"or restarting the process. The change takes effect on the next " +
			"interactive chat turn and the next scheduled run that uses the shared " +
			"catalog; in-flight runs finish on their current tool set. Equivalent " +
			"to sending SIGHUP. Admin-only. Returns a summary of what changed.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Summary of the servers added, removed, restarted, or left unchanged.", apispec.JSON(apispec.Object(
				apispec.Prop("added", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("removed", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("restarted", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("unchanged", apispec.ArrayOf(apispec.Typed("string"))),
			))),
			apispec.RefResponse("500", "Error"),
			apispec.RefResponse("503", "Error"),
		},
	},
	"GET /admin/mcp-servers/schema-issues": {
		Summary: "MCP tools whose input schema needed translation or was withheld.",
		Description: "The runtime record the model boundary keeps while building tool lists." +
			" Model providers validate every tool's input schema as JSON Schema " +
			"draft 2020-12, and Anthropic rejects the whole request when one tool " +
			"fails. Fleet translates older-draft constructs (status rewritten: the " +
			"tool works, its connector should update) and withholds a tool that is " +
			"still invalid (status invalid). The record lives in process memory: " +
			"since is when this process started recording, and a connector no turn " +
			"has used since then is not reflected. A fixed connector clears on its " +
			"next turn. Admin-only; read-only. `fleet mcp schema-issues` and `fleet" +
			" doctor` read it.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "The findings, sorted by server then tool.", apispec.JSON(&apispec.Schema{
				Type: "object",
				Properties: []apispec.Property{
					apispec.Prop("since", apispec.Typed("string", "date-time")),
					apispec.Prop("issues", apispec.ArrayOf(&apispec.Schema{
						Type: "object",
						Properties: []apispec.Property{
							apispec.Prop("server", apispec.Typed("string")),
							apispec.Prop("tool", apispec.Typed("string")),
							apispec.Prop("status", &apispec.Schema{
								Type: "string",
								Enum: []any{"rewritten", "invalid"},
							}),
							apispec.Prop("detail", apispec.Typed("string")),
							apispec.Prop("last_seen", apispec.Typed("string", "date-time")),
						},
						Required: []string{"server", "tool", "status", "detail"},
					})),
				},
				Required: []string{"since", "issues"},
			})),
		},
	},
	"GET /sla-report": {
		Summary: "SLA report — per-task actual-duration p50/p95 + breach rate (#274).",
		Description: "Aggregates the per-prompt SLA actuals over a window: the expected " +
			"duration, the p50/p95 actual run duration (completed_at - started_at)," +
			" and the breach rate (fraction of runs that crossed the fail " +
			"threshold). Admin-only — duration/breach data is operator-sensitive " +
			"and the report is global (all tasks) while a member may be scoped. The" +
			" gate is enforced in-handler on the admin permission, so the admin API" +
			" key OR an admin-role user (the dashboard's header-trust session) is " +
			"accepted while a non-admin member gets 403 (#458). days= (default 7, " +
			"clamped to [1, 90]) selects the window.",
		Security:   adminKeyOrSession,
		Permission: "admin",
		Parameters: []apispec.Parameter{
			{Name: "days", In: "query", Schema: &apispec.Schema{
				Type:    "integer",
				Default: 7,
				Minimum: 1,
				Maximum: 90,
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The SLA report for the window.", apispec.JSON(apispec.Ref("SLAReport"))),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /admin/usage": {
		Summary: "Usage analytics — cost/token roll-up by principal, project, model, or " +
			"time bucket (#601 part 1).",
		Description: "Aggregates the already-persisted metering over a window: task-side " +
			"from task_iterations joined to tasks (cost_usd, prompt/completion " +
			"tokens per iteration, keyed by created_by / created_by_key_id / " +
			"model), chat-side from the turn_metrics session log (keyed by user " +
			"email, project, conversation model). Admin-only — spend data is global" +
			" across all principals. Like /sla-report, the gate is enforced " +
			"in-handler on the admin permission so both the admin API key and an " +
			"admin-role dashboard session are accepted (#458). group_by defaults to" +
			" user; from/to default to the trailing 30 days (to is exclusive; the " +
			"window is clamped to 366 days). Dollar figures depend on pricing " +
			"configuration (#289): native- provider runs accrue $0 unless a pricing" +
			" override is set — token totals are complete regardless.",
		Security:   adminKeyOrSession,
		Permission: "admin",
		Parameters: []apispec.Parameter{
			{Name: "group_by", In: "query", Schema: &apispec.Schema{
				Type:    "string",
				Enum:    []any{"user", "key", "project", "model", "day", "week"},
				Default: "user",
			}},
			{Name: "from", In: "query", Description: "Window start (inclusive), RFC 3339 or YYYY-MM-DD. Default = to − 30 " +
				"days.", Schema: apispec.Typed("string")},
			{Name: "to", In: "query", Description: "Window end (exclusive), RFC 3339 or YYYY-MM-DD. Default = now.", Schema: apispec.Typed("string")},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The usage report for the window.", apispec.JSON(apispec.Ref("UsageReport"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /admin/usage/adoption": {
		Summary: "Adoption analytics — per-user AI-adoption audit with daily series and " +
			"trend deltas.",
		Description: "The read model behind the Operations Center's exec Adoption view. Same" +
			" persisted metering as /admin/usage, aggregated by (user, UTC day) at " +
			"once: per-user merged totals with per-day token series (sparklines, " +
			"active days), an equal-length previous window for trend deltas, a " +
			"daily activity trend, and the provisioned-account roster so inactive " +
			"seats are listed explicitly. Admin-only with the /admin/usage posture:" +
			" registered for proxy reachability, gated in-handler on the admin " +
			"permission (#458). from/to default to the trailing 30 days (to is " +
			"exclusive; the window is clamped to 366 days). Dollar figures depend " +
			"on pricing configuration (#289) — token totals are complete " +
			"regardless, which is why the leaderboard reads token volume first.",
		Security:   adminKeyOrSession,
		Permission: "admin",
		Parameters: []apispec.Parameter{
			{Name: "from", In: "query", Description: "Window start (inclusive), RFC 3339 or YYYY-MM-DD. Default = to − 30 " +
				"days.", Schema: apispec.Typed("string")},
			{Name: "to", In: "query", Description: "Window end (exclusive), RFC 3339 or YYYY-MM-DD. Default = now.", Schema: apispec.Typed("string")},
			{Name: "format", In: "query", Description: "json (default) or csv (the per-user leaderboard as an attachment).", Schema: &apispec.Schema{
				Type:    "string",
				Enum:    []any{"json", "csv"},
				Default: "json",
			}},
		},
		Responses: []apispec.Response{
			apispec.OK("200", "The adoption report for the window.", apispec.JSON(apispec.Ref("AdoptionReport"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /admin/budgets": {
		Summary: "List per-principal rolling budgets with their live window spend (#601 " +
			"part 2).",
		Description: "Every configured budget plus its current-window evaluation — spend " +
			"recomputed from the persisted metering (the part-1 usage read model), " +
			"the effective hard bounds after the fail-safe clamp against the live " +
			"global ceilings (#286), and whether this window's soft alert fired. " +
			"Admin-only; like /admin/usage the gate is enforced in-handler on the " +
			"admin permission so both the admin API key and an admin-role dashboard" +
			" session are accepted (#458).",
		Security:   adminKeyOrSession,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "The configured budgets with live evaluation.", apispec.JSON(&apispec.Schema{
				Type: "object",
				Properties: []apispec.Property{
					apispec.Prop("budgets", apispec.ArrayOf(apispec.Ref("BudgetStatus"))),
				},
				Required: []string{"budgets"},
			})),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"POST /admin/budgets": {
		Summary: "Create or replace a per-principal rolling budget (#601 part 2).",
		Description: "Upserts on (scope, principal_id, window) — one budget per principal " +
			"per window kind — and resets the once-per-window soft-alert marker so " +
			"an edited budget re-arms its alert. Enforcement applies at every " +
			"task-create path (POST /tasks, POST /tasks/batch, chat schedule_task):" +
			" at a soft bound one notify alert fires per window crossing; at a hard" +
			" bound new task creation is refused with 402 until the window rolls " +
			"over. Budgets only narrow the live global FLEET_MAX_COST_USD / " +
			"FLEET_MAX_TOTAL_TOKENS ceilings, never exceed them. Admin-only " +
			"(in-handler admin gate, #458).",
		Security:    adminKeyOrSession,
		Permission:  "admin",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(apispec.Ref("BudgetCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "The persisted budget.", apispec.JSON(apispec.Ref("Budget"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"DELETE /admin/budgets/{budget_id}": {
		Summary:    "Delete a per-principal rolling budget (#601 part 2).",
		Security:   adminKeyOrSession,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Deleted."),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
			apispec.RefResponse("500", "Error"),
		},
	},
	"GET /logs/{task_id}": {
		Summary: "Get the captain's-log session for a task.",
		Description: "Requires the view_logs permission AND either ownership of the task " +
			"(the creating user or API key) or the explicit fleet-wide " +
			"view_all_logs permission, which PermissionAdmin implies. A transcript " +
			"carries the run's verbatim tool traffic, so a view_logs holder reads " +
			"its OWN tasks only; anyone else's is 403. Same gate on the /history " +
			"siblings and on GET /tasks/{task_id}/stream.",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("LogSession"))),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"GET /logs/{task_id}/history": {
		Summary: "List the task's superseded transcripts (per-attempt run log history).",
		Description: "Metadata only (entry id + when a newer transcript replaced it), newest" +
			" first. History exists only where a retry / ask-resume / wake cycle of" +
			" the SAME task id overwrote a transcript. Same auth + ownership gate " +
			"as GET /logs/{task_id}.",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Object(
				apispec.Prop("entries", apispec.ArrayOf(apispec.Object(
					apispec.Prop("id", apispec.Typed("integer")),
					apispec.Prop("superseded_at", apispec.Typed("string", "date-time")),
				))),
			))),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"GET /logs/{task_id}/history/{entry_id}": {
		Summary: "Fetch one superseded transcript from the task's run log history.",
		Description: "Returns the LogSession shape GET /logs/{task_id} returns. Entry " +
			"lookups are task-scoped; same auth + ownership gate as GET " +
			"/logs/{task_id}.",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("LogSession"))),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"GET /logs/{task_id}/subagents/{child_session_id}": {
		Summary: "Fetch a spawned sub-agent's own transcript (#1043).",
		Description: "Returns the sibling session log (LogSession shape) a sub-agent child " +
			"run wrote. Same auth + ownership gate as GET /logs/{task_id}, PLUS a " +
			"linkage check — the child id must appear in a subagent_spawned entry " +
			"of this task's persisted log (latest or any history attempt). The " +
			"transcript file is host-local and best-effort: 404 when it is no " +
			"longer present, while the parent log's linkage entry remains the " +
			"durable record.",
		Security:   adminScopedOrSession,
		Permission: "view_logs",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("LogSession"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /users": {
		Summary:     "Create a sched user.",
		Security:    adminKey,
		Permission:  "admin",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Ref("UserCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("201", "Created", apispec.JSON(apispec.Ref("UserResponse"))),
		},
	},
	"GET /api/me": {
		Summary:    "The authenticated principal.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("CurrentUserResponse"))),
		},
	},
	"GET /stats": {
		Summary:    "Dashboard stats.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Ref("DashboardStats"))),
		},
	},
	"GET /mcp-servers": {
		Summary: "Optional-MCP catalog for the scheduled-task picker (read-only; never " +
			"secret values).",
		Description: "Mirrors chat's GET /mcp-servers so the same MCP picker renders in the " +
			"task form. Returns each Optional server's name, display name, " +
			"description, tool count, default-enabled flag, and its " +
			"credential-account seat names (derived from the <VAR>_<ACCOUNT> env " +
			"suffix scan). Secret values are never returned.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Object(
				apispec.Prop("servers", apispec.ArrayOf(apispec.Object(
					apispec.Prop("name", apispec.Typed("string")),
					apispec.Prop("display_name", apispec.Typed("string")),
					apispec.Prop("description", apispec.Typed("string")),
					apispec.Prop("tool_count", apispec.Typed("integer")),
					apispec.Prop("enabled", apispec.Typed("boolean")),
					apispec.Prop("accounts", apispec.ArrayOf(apispec.Typed("string"))),
				))),
			))),
		},
	},
	"GET /mcp-accounts": {
		Summary: "Flat (server, account) credential-seat catalog (names only).",
		Description: "Companion to /mcp-servers for the credential-account admin table. " +
			"Lists every provisioned (server, account) seat by name; never returns " +
			"secret values.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.Object(
				apispec.Prop("accounts", apispec.ArrayOf(apispec.Object(
					apispec.Prop("server", apispec.Typed("string")),
					apispec.Prop("account", apispec.Typed("string")),
				))),
			))),
		},
	},
	"GET /task-templates": {
		Summary: "Read-only task-template catalog for the \"new task from a template\" UI.",
		Description: "Returns the client bundle's task_templates catalog: each entry's name," +
			" description, icon, a partial-TaskCreate `task` payload that seeds the" +
			" task-create form, and the {variable} placeholder names extracted from" +
			" its prompt (so the UI can prompt for them before pre-filling). Pure " +
			"config read-through — no template is ever persisted and no task is " +
			"created here (the task is created through the ordinary POST /tasks " +
			"path). Returns an empty array when the bundle ships no templates.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK", apispec.JSON(apispec.ArrayOf(apispec.Object(
				apispec.Prop("name", apispec.Typed("string")),
				apispec.Prop("description", apispec.Typed("string")),
				apispec.Prop("icon", apispec.Typed("string")),
				apispec.Prop("variables", apispec.ArrayOf(apispec.Typed("string"))),
				apispec.Prop("task", &apispec.Schema{
					Type: "object",
					Description: "Partial TaskCreate payload (subset of editable fields) used to " +
						"pre-fill the form. All fields optional.\n",
					Properties: []apispec.Property{
						apispec.Prop("prompt", apispec.Typed("string")),
						apispec.Prop("model", apispec.Typed("string")),
						apispec.Prop("fallback_model", apispec.Typed("string")),
						apispec.Prop("max_iterations", apispec.Typed("integer")),
						apispec.Prop("max_retries", apispec.Typed("integer")),
						apispec.Prop("recurrence", apispec.Typed("string")),
						apispec.Prop("timezone", apispec.Typed("string")),
						apispec.Prop("priority", apispec.Typed("integer")),
						apispec.Prop("allow_network", apispec.Typed("boolean")),
						apispec.Prop("instruction_self_improve", apispec.Typed("boolean")),
						apispec.Prop("persona", apispec.Typed("string")),
						apispec.Prop("description", apispec.Typed("string")),
						apispec.Prop("tags", apispec.ArrayOf(apispec.Typed("string"))),
					},
				}),
			)))),
		},
	},
	"GET /prompts": {
		Summary: "List the visible hybrid prompt library.",
		Description: "Merges live-read, read-only files from the client bundle's prompts/ " +
			"directory with the caller's private prompts and all workspace-shared " +
			"database prompts.\n",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "Visible prompt entries.", apispec.JSON(apispec.ArrayOf(apispec.Ref("PromptLibraryItem")))),
		},
	},
	"POST /prompts": {
		Summary:     "Create a private or workspace-shared prompt.",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(apispec.Ref("PromptLibraryWrite"))}},
		Responses: []apispec.Response{
			apispec.OK("201", "Created", apispec.JSON(apispec.Ref("PromptLibraryItem"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("409", "Error"),
		},
	},
	"GET /prompts/export": {
		Summary:    "Export the visible hybrid prompt library as versioned JSON.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "Portable prompt-library backup.", apispec.JSON(apispec.Object(
				apispec.Prop("format", &apispec.Schema{
					Type: "string",
					Enum: []any{"fleet-prompt-library"},
				}),
				apispec.Prop("version", &apispec.Schema{
					Type: "integer",
					Enum: []any{1},
				}),
				apispec.Prop("exported_at", apispec.Typed("string", "date-time")),
				apispec.Prop("prompts", apispec.ArrayOf(apispec.Ref("PromptLibraryItem"))),
			))),
		},
	},
	"PUT /prompts/{prompt_id}": {
		Summary:     "Replace an owned database-backed prompt (admins may replace any).",
		Security:    adminScopedOrSession,
		Permission:  "create_task",
		RequestBody: &apispec.RequestBody{Required: true, Content: []apispec.Media{apispec.JSON(apispec.Ref("PromptLibraryWrite"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Updated", apispec.JSON(apispec.Ref("PromptLibraryItem"))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"DELETE /prompts/{prompt_id}": {
		Summary:    "Delete an owned database-backed prompt (admins may delete any).",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		Responses: []apispec.Response{
			apispec.OK("204", "Deleted"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /keys": {
		Summary:     "Create an API key (plaintext shown once).",
		Security:    adminKey,
		Permission:  "admin",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Ref("APIKeyCreate"))}},
		Responses: []apispec.Response{
			apispec.OK("200", "Created", apispec.JSON(apispec.Ref("APIKeyCreated"))),
		},
	},
	"GET /keys": {
		Summary:    "List API keys (metadata only).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
		},
	},
	"GET /keys/audit": {
		Summary:    "API-key audit log.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
		},
	},
	"GET /keys/{key_id}": {
		Summary:    "Get an API key's metadata.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"DELETE /keys/{key_id}": {
		Summary:    "Delete an API key.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Deleted"),
		},
	},
	"POST /keys/{key_id}/rotate": {
		Summary:    "Rotate an API key (new plaintext shown once).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Rotated"),
		},
	},
	"POST /keys/{key_id}/revoke": {
		Summary:    "Revoke an API key.",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Revoked"),
		},
	},
	"GET /notes": {
		Summary:    "List published notes.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
		},
	},
	"POST /notes": {
		Summary:    "Create a note (admin).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Created"),
		},
	},
	"GET /notes/{slug}": {
		Summary:    "Get a note by slug.",
		Security:   adminScopedOrSession,
		Permission: "view_tasks",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"PUT /notes/{slug}": {
		Summary:    "Update a note (admin).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Updated"),
		},
	},
	"DELETE /notes/{slug}": {
		Summary:    "Archive a note (admin).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Archived"),
		},
	},
	"GET /notes/proposals": {
		Summary: "List note proposals (admin).",
		Description: "Requires the admin permission (admin API key, a key carrying admin, or" +
			" an admin-role user).",
		Security:   adminScopedOrSession,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /notes/proposals/{id}": {
		Summary: "Get a note proposal (admin).",
		Description: "Requires the admin permission (admin API key, a key carrying admin, or" +
			" an admin-role user).",
		Security:   adminScopedOrSession,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "OK"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /notes/proposals/{id}/publish": {
		Summary:    "Publish a note proposal (admin).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Published"),
		},
	},
	"POST /notes/proposals/{id}/reject": {
		Summary:    "Reject a note proposal (admin).",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "Rejected"),
		},
	},
	"POST /upload": {
		Summary: "Upload a file for a task's workspace.",
		Description: "Authenticated in-handler — accepts an admin key, a scoped API key " +
			"carrying create_task permission (#719: an uploaded file is only usable" +
			" via a subsequent task create, so the same keys that may create tasks " +
			"may stage uploads; a valid readonly/webhook key gets a definitive " +
			"403), or a signed-in session (the Elcano cookie). A readonly user gets" +
			" the same 403.",
		Security:   adminScopedOrSession,
		Permission: "create_task",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{{Type: "multipart/form-data", Schema: apispec.Object(
			apispec.Prop("file", apispec.Typed("string", "binary")),
		)}}},
		Responses: []apispec.Response{
			apispec.OK("200", "Uploaded"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"GET /files/{filename}": {
		Summary: "Download a previously-uploaded file (admin-only; the worker-node " +
			"download path was removed in",
		Security:   adminKey,
		Permission: "admin",
		Responses: []apispec.Response{
			apispec.OK("200", "The file bytes"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("404", "Error"),
		},
	},
	"POST /triggers/{slug}": {
		Summary: "Fire a webhook-triggered task run.",
		Description: "Spawns a one-shot run cloned from the trigger's template task, with " +
			"the rendered prompt_template as the prompt. Authenticated SOLELY by " +
			"the per-trigger HMAC-SHA256 secret over the raw body " +
			"(GitHub-compatible X-Hub-Signature-256, or X-Fleet-Signature-256), OR " +
			"by a typed fleet_webhook_ API key whose allowed trigger slugs include " +
			"this slug (#190), sent as X-API-Key or Authorization: Bearer — never " +
			"the admin API key or any other key type. A valid webhook key that does" +
			" not list this slug is a definitive 403. Unknown slug and bad " +
			"signature both return 401 (indistinguishable, with equalized HMAC " +
			"work) to prevent slug enumeration. A leaked secret or key lets a " +
			"caller inject the spawned run's prompt under the template's seat " +
			"(network/MCP/files); the sandbox remains the execution boundary. " +
			"Rotate the secret with `fleet sched trigger rotate`.",
		Security:    apispec.AnyOf("webhookKey", "webhookKeyBearer", "triggerSignature"),
		Permission:  "webhook",
		RequestBody: &apispec.RequestBody{Content: []apispec.Media{apispec.JSON(apispec.Typed("object"))}},
		Responses: []apispec.Response{
			apispec.OK("202", "Accepted — a run was enqueued.", apispec.JSON(apispec.Object(
				apispec.Prop("run_id", apispec.Typed("string", "uuid")),
			))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
		},
	},
	"POST /triggers/email/{slug}": {
		Summary: "Fire an email-ingress trigger (#511).",
		Description: "An email provider's inbound-parse webhook posts a normalized email " +
			"payload to spawn a governed run cloned from the trigger's template " +
			"task. Authenticated by the per-trigger HMAC-SHA256 secret over the raw" +
			" body (X-Hub-Signature-256 or X-Fleet-Signature-256) ONLY — no API key" +
			" of any type, webhook keys included, is accepted here. An unknown " +
			"slug, a webhook-kind slug, or a bad signature all return an identical " +
			"timing-equalized 401 (no slug/kind enumeration). After auth the " +
			"email-kind policy is enforced (approved senders → 403; required " +
			"DKIM/SPF → 403; attachment count/size caps → 413) and the Message-ID " +
			"is deduped (a duplicate delivery returns 200 with status=duplicate and" +
			" spawns no run). The spawned run inherits the template's write-capable" +
			" MCP connectors ONLY when the template task set " +
			"allow_event_triggers=true; otherwise it runs with native tools only. " +
			"The run is recorded against the inbound event, and on success a reply " +
			"is sent to the sender when SMTP is configured.",
		Security:   apispec.AnyOf("triggerSignature"),
		Permission: "hmac",
		RequestBody: &apispec.RequestBody{Description: "A normalized inbound email: { message_id, from, to, subject, text, " +
			"html, spf, dkim, attachments:[{filename,size,content_type}] }.", Content: []apispec.Media{apispec.JSON(apispec.Typed("object"))}},
		Responses: []apispec.Response{
			apispec.OK("202", "Accepted — a run was enqueued.", apispec.JSON(apispec.Object(
				apispec.Prop("run_id", apispec.Typed("string", "uuid")),
			))),
			apispec.OK("200", "Duplicate delivery — already processed, no new run.", apispec.JSON(apispec.Object(
				apispec.Prop("status", &apispec.Schema{
					Type:    "string",
					Example: "duplicate",
				}),
			))),
			apispec.RefResponse("400", "Error"),
			apispec.RefResponse("401", "Error"),
			apispec.RefResponse("403", "Error"),
			apispec.RefResponse("413", "Error"),
		},
	},
}
