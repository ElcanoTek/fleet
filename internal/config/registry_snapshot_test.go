package config

// The two hand-maintained tables the configuration registry (envreg, #2053)
// replaced, copied VERBATIM from the tree before it (1f955b28). They exist only
// so registry_test.go can prove the derived allowlist and the derived knob
// table are exactly what they replaced: no key appears, disappears, changes
// kind, bounds, scope or alias behaviour. Do not edit them to make a test
// pass; a deliberate change to the allowlist or a knob is a registry change,
// made in envreg/table.go, and is reflected here only with a reason in the PR.
//
// Deliberate change (ADR-0106): the CHAT_/CUTLASS_ aliases were removed, so
// the allowlist lost its file-admitted alias spellings and the knob rows lost
// their alias-chain flag. The canonical CHAT_/CUTLASS_ names (rate limits,
// connector wire keys) stay.

var preRegistryAllowedEnvVars = map[string]bool{
	// ── chat transport / data ──
	"CONVERSATION_TTL_DAYS":            true,
	"CONVERSATION_UNPINNED_CAP":        true,
	"FLEET_AUTO_ARCHIVE_AFTER_DAYS":    true,
	"FLEET_INPUT_QUEUE_RETENTION_DAYS": true,
	"FLEET_TURN_EVENT_RETENTION_DAYS":  true,
	"FLEET_UPLOAD_MAX_BYTES":           true,
	"FLEET_SHARED_FILES_MAX_TOTAL_MB":  true,

	// ── fleet transport / data (canonical) ──
	"FLEET_SERVER_ADDR": true,
	"FLEET_DATA_DIR":    true,

	// ── IP access control (network-level allow/deny + trusted proxies) ──
	"FLEET_IP_ALLOWLIST":    true,
	"FLEET_IP_DENYLIST":     true,
	"FLEET_TRUSTED_PROXIES": true,

	// ── TLS termination (chat server) ──
	"FLEET_TLS_MODE":       true,
	"FLEET_TLS_CERT_FILE":  true,
	"FLEET_TLS_KEY_FILE":   true,
	"FLEET_TLS_DOMAIN":     true,
	"FLEET_TLS_ACME_DIR":   true,
	"FLEET_TLS_ACME_EMAIL": true,
	"FLEET_TLS_HTTP_ADDR":  true,

	// ── database (chat) ──
	"DATABASE_URL": true,
	// Explicit per-store DSNs (read by cmd/fleet main; deploy/fleet.service
	// documents them as env-file keys). Missing from the allowlist they were
	// silently dropped when supplied via FLEET_ENV_FILE — boot then failed
	// with an empty-DSN error despite a correct env file.
	"FLEET_CHAT_DATABASE_URL":  true,
	"FLEET_SCHED_DATABASE_URL": true,
	// Legacy bare spelling schedDSN still reads as a fallback.
	"SCHED_DATABASE_URL": true,
	// Other keys boot reads via os.Getenv that predated this allowlist (an
	// env-file value was silently ignored; only the process env worked).
	"ADMIN_API_KEY":                       true,
	"FLEET_BACKUP_DIR":                    true,
	"FLEET_BACKUP_RETENTION_DAYS":         true,
	"FLEET_DEFAULT_TIMEZONE":              true,
	"FLEET_TIMEZONE":                      true,
	"FLEET_ORCHESTRATOR_ADDR":             true,
	"FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS": true,
	"FLEET_OTEL_ENDPOINT":                 true,
	"FLEET_OTEL_SAMPLE_RATIO":             true,
	// Added after the registry (#2052, OTLP over TLS/mTLS): a deliberate
	// registry change, recorded here per the rule above.
	"FLEET_OTEL_INSECURE":                  true,
	"FLEET_OTEL_CA_FILE":                   true,
	"FLEET_OTEL_CLIENT_CERT_FILE":          true,
	"FLEET_OTEL_CLIENT_KEY_FILE":           true,
	"FLEET_SERVICE_NAME":                   true,
	"FLEET_SANDBOX_STATS_INTERVAL_SECONDS": true,
	"FLEET_TOOL_DISCLOSURE_THRESHOLD":      true,
	"FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES":   true,
	// DB connection-pool tuning (#276), per pool.
	"FLEET_CHAT_DB_MAX_CONNS":           true,
	"FLEET_CHAT_DB_MIN_CONNS":           true,
	"FLEET_CHAT_DB_MAX_CONN_IDLE_TIME":  true,
	"FLEET_CHAT_DB_MAX_CONN_LIFETIME":   true,
	"FLEET_CHAT_DB_CONNECT_TIMEOUT":     true,
	"FLEET_SCHED_DB_MAX_CONNS":          true,
	"FLEET_SCHED_DB_MIN_CONNS":          true,
	"FLEET_SCHED_DB_MAX_CONN_IDLE_TIME": true,
	"FLEET_SCHED_DB_MAX_CONN_LIFETIME":  true,
	"FLEET_SCHED_DB_CONNECT_TIMEOUT":    true,
	"FLEET_SCHED_TICK_SECONDS":          true,
	"DB_HOST":                           true,
	"DB_PORT":                           true,
	"DB_USER":                           true,
	"DB_PASSWORD":                       true,
	"DB_NAME":                           true,
	"DB_SSLMODE":                        true,

	// ── LLM (shared) ──
	"OPENROUTER_API_KEY":                             true,
	"OPENROUTER_BASE_URL":                            true,
	"FLEET_OPENROUTER_BASE_URL":                      true,
	"FLEET_TURN_TIMEOUT_SECONDS":                     true,
	"FLEET_SHUTDOWN_GRACE_SECONDS":                   true,
	"FLEET_DEFAULT_THINKING_BUDGET_TOKENS":           true,
	"FLEET_MAX_ITERATIONS":                           true,
	"FLEET_MAX_COST_USD":                             true,
	"FLEET_MAX_TOTAL_TOKENS":                         true,
	"FLEET_MONTHLY_SOFT_TOKEN_LIMIT":                 true,
	"FLEET_TASK_DEFAULT_MAX_RETRIES":                 true,
	"FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS":     true,
	"FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS": true,
	"FLEET_TEMPERATURE":                              true,
	"FLEET_TITLE_MODEL":                              true,
	"FLEET_METADATA_MODEL":                           true,
	"FLEET_ERROR_ANALYSIS_MODEL":                     true,
	"FLEET_ERROR_ANALYSIS_ENABLED":                   true,
	"FLEET_ESCALATION_SIGNALS_ENABLED":               true,
	"FLEET_ESCALATION_CLASSIFIER_MODE":               true,
	"FLEET_ESCALATION_CLASSIFIER_MODEL":              true,
	"FLEET_ESCALATION_CLASSIFIER_THRESHOLD":          true,
	"FLEET_AUTO_TITLE":                               true,
	"FLEET_MEMORY_MODEL":                             true,
	"FLEET_MEMORY_AUTOINDEX_ENABLED":                 true,
	"FLEET_RECURRING_TASK_MODEL":                     true,
	"FLEET_LIBRARY_PROMPT_MODEL":                     true,
	"FLEET_APPROVAL_TIMEOUT_SECONDS":                 true,
	"FLEET_AUTO_APPROVE_IN_TEST":                     true,
	"FLEET_MAX_CONCURRENT_AGENTS":                    true,
	"FLEET_TASK_WALL_TIMEOUT":                        true,
	"FLEET_RUN_LOG_RETENTION_DAYS":                   true,
	"FLEET_WORKTREE_PRUNE_AGE":                       true,
	"FLEET_DISK_MIN_FREE_PERCENT":                    true,
	// Read by internal/httpapi directly (not the loader), but listed so an
	// operator can set it in FLEET_ENV_FILE like every other knob.
	"FLEET_MAINTENANCE_MIN_INTERVAL": true,
	"FLEET_KEEP_RUNS_PER_TASK":       true,
	"FLEET_CLEANUP_HOUR":             true,
	"LLM_MAX_TOKENS":                 true,
	"FLEET_MAX_TOOL_OUTPUT_BYTES":    true,

	// ── knobs parsed at their point of use, not by the loader (#1273) ──
	// These are scopeExternal rows in the env-knob registry (knobs.go): the
	// boot gate and `fleet validate-config` validate them from that table, but
	// both read the PROCESS env — so a knob missing from this allowlist would
	// be dropped when set only in FLEET_ENV_FILE and neither would ever see
	// the operator's value. FLEET_DISABLE_PROMPT_CACHE in particular has been
	// documented as an env-file knob in docs/OPERATORS.md all along while
	// being silently dropped from the file (the #1107 bug class).
	// TestEnvKnobsAreEnvFileSettable keeps the two tables paired.
	"FLEET_DISABLE_PROMPT_CACHE":            true,
	"FLEET_DISABLE_OPENROUTER_MODELS":       true,
	"FLEET_SCHEDULED_AUTO_COMPACT":          true,
	"FLEET_SCHEDULED_SHARED_WORKSPACE":      true,
	"FLEET_MODEL_CACHE_TTL_MINUTES":         true,
	"FLEET_RETRY_MAX_ATTEMPTS":              true,
	"FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD": true,
	"FLEET_CONTEXT_COMPACTION_THRESHOLD":    true,
	"FLEET_CONTEXT_RESEND_BUDGET_TOKENS":    true,
	"FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY":    true,
	"FLEET_BUDGET_WINDDOWN_FRACTION":        true,
	"FLEET_SSE_BUFFER_DURATION":             true,
	"FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN":   true,
	"FLEET_SSE_HEARTBEAT_INTERVAL":          true,
	"FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE":   true,
	"FLEET_UPDATE_WAIT_SECONDS":             true,
	"FLEET_UPDATE_DRAIN_FILE":               true,
	"FLEET_DRAIN_FILE":                      true,

	// ── process log file sink (#298) — opt-in rotating file, default OFF ──
	"FLEET_LOG_FILE":         true,
	"FLEET_LOG_MAX_SIZE_MB":  true,
	"FLEET_LOG_MAX_AGE_DAYS": true,
	"FLEET_LOG_MAX_BACKUPS":  true,
	"FLEET_LOG_FORMAT":       true,
	"FLEET_LOG_LEVEL":        true,
	"FLEET_LOG_COMPRESS":     true,

	// ── log archival (#272) ── the AES key is a SECRET held host-side; it is
	// decoded in-process and never logged or shipped into the sandbox.
	"FLEET_LOG_ARCHIVE_AFTER_DAYS":     true,
	"FLEET_LOG_ARCHIVE_ENCRYPTION_KEY": true,

	// ── remote MCP OAuth (#443) ── the AES key is a SECRET held host-side; it
	// is decoded in-process and never logged or shipped into the sandbox.
	"FLEET_PUBLIC_BASE_URL":                true,
	"FLEET_MCP_OAUTH_ENCRYPTION_KEY":       true,
	"FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP": true,

	// ── built-in Microsoft 365 OAuth + Graph MCP surface (internal/ms365mcp) ──
	"MS365_MCP_CLIENT_ID":             true,
	"MS365_MCP_TENANT_ID":             true,
	"MS365_MCP_CLIENT_SECRET":         true,
	"MS365_MCP_PUBLIC_URL":            true,
	"MS365_MCP_ALIASES":               true,
	"MS365_MCP_ALLOWED_REDIRECT_URIS": true,
	"MS365_MCP_ATTACHMENT_URL_TTL_S":  true,

	// ── personas / protocols ──
	// The two default-persona knobs are read as FLEET_<X>
	// with a bare-name fallback (getenvFleetOrBare), so the canonical FLEET_
	// spelling must survive the env-file allowlist alongside the historical one.
	"FLEET_PERSONA_DEFAULT": true,
	"PERSONA_DEFAULT":       true,
	"FLEET_PERSONA":         true,
	"PERSONA":               true,
	"SYSTEM_PROMPT":         true,

	// ── cutlass agent knobs ──
	"MAX_ITERATIONS": true,

	// ── cutlass image gen ──
	"CUTLASS_IMAGE_OUTPUT":    true,
	"CUTLASS_IMAGE_MODEL":     true,
	"OPENROUTER_HTTP_REFERER": true,
	"OPENROUTER_X_TITLE":      true,

	// ── cutlass task input (set by runner) ──
	// The loader reads FLEET_TASK_MODEL (#1015), so it must survive the
	// env-file filter (#1107).
	"FLEET_TASK_MODEL":          true,
	"FLEET_TASK_FALLBACK_MODEL": true,
	// Dedicated end-of-run verifier model (empty = the run's own model).
	"FLEET_VERIFIER_MODEL": true,

	// ── model tiers (#1187): env defaults of the default_model /
	// advanced_model admin settings ──
	"FLEET_DEFAULT_MODEL":  true,
	"FLEET_ADVANCED_MODEL": true,
	// ── operator-pinned / relabelled picker rows ──
	"FLEET_PINNED_MODELS":  true,
	"CUTLASS_ALLOWED_DIRS": true,
	"GH_TOKEN":             true,

	// ── sub-agents / delegation (#175, #264, #1043) ──
	"FLEET_SUBAGENTS_ENABLED":         true,
	"FLEET_SUBAGENTS_MAX_DEPTH":       true,
	"FLEET_SUBAGENTS_MAX_CHILDREN":    true,
	"FLEET_SUBAGENTS_BUDGET_FRACTION": true,
	"FLEET_SUBAGENTS_MODEL":           true,

	// ── task scheduler pacing: anti-starvation (#230), paused expiry (#510),
	// and the POST /tasks + /upload sliding-window rate limits (cmd/fleet) ──
	"FLEET_TASK_STARVATION_WINDOW_MINUTES":     true,
	"FLEET_PAUSED_TASK_EXPIRY_MINUTES":         true,
	"FLEET_SCHED_RATE_LIMIT_PER_MINUTE":        true,
	"FLEET_SCHED_RATE_LIMIT_PER_DAY":           true,
	"FLEET_SCHED_RATE_LIMIT_GLOBAL_PER_MINUTE": true,

	// ── logging / debug (cutlass) ──
	"LOG_LEVEL": true,
	"DEBUG":     true,
	"VERBOSE":   true,

	// ── MCP: email ──
	"AWS_ACCESS_KEY_ID":             true,
	"AWS_SECRET_ACCESS_KEY":         true,
	"AWS_REGION":                    true,
	"EMAIL_S3_BUCKET":               true,
	"EMAIL_S3_PREFIX":               true,
	"EMAIL_S3_DATE_PREFIX_FORMAT":   true,
	"EMAIL_S3_MAX_DATE_PREFIX_DAYS": true,
	"EMAIL_ATTACHMENT_DIR":          true,
	"EMAIL_LAST_CHECK_FILE":         true,
	"SENDGRID_API_KEY":              true,
	"SENDGRID_FROM_EMAIL":           true,

	// ── web search ──
	"TAVILY_API_KEY": true,

	// ── hosted browser automation (#987) ──
	// Mints Browserbase live-view URLs host-side (internal/tools/browserbase_live_view.go).
	// Deliberately separate from the browserbase MCP connector's own per-user key,
	// which is sealed in remote_mcp_servers.api_key_enc and brokered — see
	// docs/BROWSERBASE.md for why the two are not shared.
	"BROWSERBASE_API_KEY": true,

	// NOTE: client-specific MCP connector credentials (DSPs, fast.io, gamma,
	// etc.) are NOT enumerated here. The MCP catalog lives in the client
	// bundle's manifest (internal/clientconfig), and cmd/fleet admits the
	// manifest-referenced env-var names at startup via RegisterAllowedEnvVars,
	// keeping fleet client-agnostic while preserving the .env allowlist's
	// security model.

	// ── rate limiting (chat) ──
	"CHAT_RATE_PER_MIN":                true,
	"CHAT_RATE_PER_DAY":                true,
	"FLEET_CHAT_RATE_LIMIT_ENABLED":    true,
	"FLEET_CHAT_RATE_LIMIT_CONCURRENT": true,
	"FLEET_SEARCH_ENABLED":             true,
	"FLEET_CONVERSATION_SOFT_DELETE":   true,

	// ── admin ──
	"ADMIN_EMAILS": true,

	// ── task completion notifications (#208, #316) ──
	// Host-side outbound notifier for scheduled-task terminal status. All OFF by
	// default: with none of these set, no notifications fire. The SMTP password
	// and webhook signing secret are held host-side and never logged or shipped
	// into the sandbox. When FLEET_WEBHOOK_SECRET is set the webhook body is signed
	// with HMAC-SHA256 (X-Fleet-Signature/X-Fleet-Timestamp, #316); unset = the
	// webhook is delivered unsigned. FLEET_PUBLIC_URL is reused to build the
	// per-task log link.
	"FLEET_NOTIFY_ON":             true,
	"FLEET_NOTIFY_EMAIL_TO":       true,
	"FLEET_NOTIFY_TIMEOUT":        true,
	"FLEET_NOTIFY_RETRIES":        true,
	"FLEET_SMTP_HOST":             true,
	"FLEET_SMTP_PORT":             true,
	"FLEET_SMTP_USERNAME":         true,
	"FLEET_SMTP_PASSWORD":         true,
	"FLEET_SMTP_FROM":             true,
	"FLEET_WEBHOOK_URL":           true,
	"FLEET_WEBHOOK_METHOD":        true,
	"FLEET_WEBHOOK_BODY_TEMPLATE": true,
	"FLEET_WEBHOOK_SECRET":        true,
	"FLEET_PUBLIC_URL":            true,

	// ── browser tier (internal/webui, ADR-0091) ──
	// What the Next.js tier read from /etc/fleet/fleet-web.env, now read by the
	// fleet binary itself. APP_SESSION_SECRET signs the session cookie; the
	// AUTH_* keys configure the legacy Elcano magic-link cookie and the
	// back-channel signing keys; FLEET_OIDC_* is central-Auth SSO, and with it
	// configured Fleet passwords are off unless FLEET_OIDC_ALLOW_PASSWORD=1
	// (Config.PasswordLoginDisabled); FLEET_OIDC_GENERIC=1 marks another OIDC
	// provider, which leaves passwords and accounts to Fleet.
	// NEXT_PUBLIC_PUBLIC_ORIGIN is read only as a fallback for
	// FLEET_PUBLIC_BASE_URL on a box whose env predates the move.
	"FLEET_WEB_ADDR":                true,
	"FLEET_WEB_DEV_SERVER":          true,
	"APP_SESSION_SECRET":            true,
	"AUTH_SIGNING_PUBKEY":           true,
	"AUTH_SIGNING_PREVIOUS_PUBKEYS": true,
	"AUTH_COOKIE_NAME":              true,
	"AUTH_COOKIE_DOMAIN":            true,
	"AUTH_LOGIN_URL":                true,
	"FLEET_OIDC_ISSUER":             true,
	"FLEET_OIDC_CLIENT_ID":          true,
	"FLEET_OIDC_CLIENT_SECRET":      true,
	"FLEET_OIDC_SCOPES":             true,
	"FLEET_OIDC_ALLOWED_DOMAINS":    true,
	"FLEET_OIDC_BUTTON_LABEL":       true,
	"FLEET_OIDC_REDIRECT_URI":       true,
	"FLEET_OIDC_AUTO_START":         true,
	"FLEET_OIDC_ALLOW_PASSWORD":     true,
	"FLEET_OIDC_GENERIC":            true,
	"CATWALK_URL":                   true,
	"NEXT_PUBLIC_PUBLIC_ORIGIN":     true,

	// Retired with the account-events feed (ADR-0096). Still loaded from the
	// env file so Load's boot warning can name a stale value; nothing reads
	// either key.
	"FLEET_ACCOUNT_EVENTS_URL":    true,
	"FLEET_ACCOUNT_EVENTS_SECRET": true,

	// ── browser Web Push notifications (#292) ──
	// OFF unless all three FLEET_VAPID_* vars are set (generate them with
	// `fleet generate-vapid-keys`). The private key is a SECRET held host-side
	// and never logged or shipped into the sandbox; the public key is served
	// to browsers. The two flags gate the triggers and default ON when the
	// keys are configured. FLEET_PUBLIC_URL (above) is reused for deep links.
	"FLEET_VAPID_PUBLIC_KEY":         true,
	"FLEET_VAPID_PRIVATE_KEY":        true,
	"FLEET_VAPID_CONTACT":            true,
	"FLEET_PUSH_ON_TASK_COMPLETE":    true,
	"FLEET_PUSH_ON_APPROVAL_REQUEST": true,

	// ── sandbox ──
	"FLEET_SANDBOX_IMAGE":   true,
	"FLEET_SANDBOX_RUNTIME": true,
	// The podman seccomp knob. Its kubernetes counterpart below has always been
	// here; this one was not, so a value set only in FLEET_ENV_FILE was dropped
	// and neither the podman profile override nor the kubernetes backend's
	// fail-closed refusal of it ever saw the operator's value.
	"FLEET_SANDBOX_SECCOMP_PROFILE":              true,
	"FLEET_SANDBOX_BACKEND":                      true,
	"FLEET_SANDBOX_K8S_NAMESPACE":                true,
	"FLEET_SANDBOX_K8S_WORKSPACE_CLAIM":          true,
	"FLEET_SANDBOX_K8S_SERVICE_ACCOUNT":          true,
	"FLEET_SANDBOX_K8S_IMAGE_PULL_SECRET":        true,
	"FLEET_SANDBOX_K8S_RUNTIME_CLASS":            true,
	"FLEET_SANDBOX_K8S_SECCOMP_PROFILE":          true,
	"FLEET_SANDBOX_K8S_KUBECONFIG":               true,
	"FLEET_SANDBOX_K8S_NETWORK_POLICY":           true,
	"FLEET_SANDBOX_K8S_OPEN_EGRESS_POLICY":       true,
	"FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED": true,
	"FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE":     true,
	"FLEET_SANDBOX_K8S_NODE_SELECTOR":            true,
	"FLEET_SANDBOX_K8S_TOLERATIONS":              true,
	"FLEET_DEFAULT_NETWORK_MODE":                 true,
	"FLEET_PII_REDACTION_ENABLED":                true,
	"FLEET_PII_REDACTION_MODE":                   true,
	// Retired with the Rampart engine (ADR-0063). Still loaded from the env
	// file so Load's boot warning can name a stale value wherever it is set;
	// nothing reads either key.
	"FLEET_PII_REDACTION_ENGINE":              true,
	"FLEET_PII_RAMPART_URL":                   true,
	"FLEET_GUARDRAIL_MODE":                    true,
	"FLEET_GUARDRAIL_PROFILE":                 true,
	"FLEET_GUARDRAIL_URL":                     true,
	"FLEET_CONTEXT_HANDLES_ENABLED":           true,
	"FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED": true,
	"FLEET_TEAM_CHATS_SHARED_BY_DEFAULT":      true,

	// ── A2A protocol server (#1279) ──
	"FLEET_A2A_ENABLED":            true,
	"FLEET_A2A_PUSH_ALLOW_PRIVATE": true,
	"FLEET_A2A_UNARY_WAIT_SECONDS": true,
	"FLEET_A2A_PERSONA":            true,
	"FLEET_A2A_MODEL":              true,
	// ── outbound A2A delegation (#1368); point-of-use knobs (knobs.go) ──
	"FLEET_A2A_MAX_DELEGATION_DEPTH": true,
	"FLEET_A2A_CLIENT_ALLOW_PRIVATE": true,

	"FLEET_SANDBOX_MEMORY":                true,
	"FLEET_SANDBOX_CPUS":                  true,
	"FLEET_SANDBOX_PIDS":                  true,
	"FLEET_SANDBOX_START_TIMEOUT_SECONDS": true,
	"FLEET_SANDBOX_KATA_OVERHEAD_MB":      true,
	"FLEET_SANDBOX_DISK_GB":               true,
	"FLEET_SANDBOX_MEMORY_MAX_MB":         true,
	"FLEET_SANDBOX_CPUS_MAX":              true,
	"FLEET_SANDBOX_PIDS_MAX":              true,
	"FLEET_SANDBOX_WARM_SIZE":             true,
	"FLEET_SANDBOX_WARM_TTL":              true,
	"FLEET_PYTHON_REPL_MODE":              true,
	"FLEET_PYTHON_CELL_TIMEOUT":           true,
	"FLEET_PYTHON_REPL_IDLE_TTL":          true,
	"FLEET_PYTHON_REPL_MAX":               true,
	"FLEET_WORKSPACE_ROOT":                true,
	// Lockdown reads FLEET_ (#1080).
	"FLEET_LOCKDOWN_ONLY":           true,
	"FLEET_LOCKDOWN_ALLOWED_MODELS": true,

	// ── test harness ──
	"FLEET_MOCK_MODE": true,

	// ── observability: Sentry error tracking (#193) ──
	// FLEET_SENTRY_DSN is the OPTIONAL Sentry-protocol endpoint (Sentry or a
	// Better Stack Errors ingest URL). Unset = Sentry is a complete no-op with
	// zero overhead; the SDK is initialized only when set. FLEET_ENVIRONMENT
	// tags events by deployment tier ("production" | "staging" | "dev") for
	// scope filtering in the Sentry UI. The DSN itself is a public ingest
	// identifier, not a secret — it is safe to ship in the .env file.
	"FLEET_SENTRY_DSN":  true,
	"FLEET_ENVIRONMENT": true,
}
var preRegistryEnvKnobs = []envKnob{
	// ── data (interactive) ──
	{key: "CONVERSATION_TTL_DAYS", kind: kindInt},
	{key: "CONVERSATION_UNPINNED_CAP", kind: kindInt},
	{key: "FLEET_INPUT_QUEUE_RETENTION_DAYS", kind: kindInt, min: bound(0)},
	{key: "FLEET_TURN_EVENT_RETENTION_DAYS", kind: kindInt},
	{key: "FLEET_UPLOAD_MAX_BYTES", kind: kindInt64},
	{key: "FLEET_SHARED_FILES_MAX_TOTAL_MB", kind: kindInt, min: bound(0)},
	{key: "FLEET_AUTO_ARCHIVE_AFTER_DAYS", kind: kindInt},
	{key: "FLEET_SEARCH_ENABLED", kind: kindBool},
	{key: "FLEET_CONVERSATION_SOFT_DELETE", kind: kindBool},

	// ── DB connection pools (#276) ──
	{key: "FLEET_CHAT_DB_MAX_CONNS", kind: kindInt},
	{key: "FLEET_CHAT_DB_MIN_CONNS", kind: kindInt},
	{key: "FLEET_CHAT_DB_MAX_CONN_IDLE_TIME", kind: kindDuration},
	{key: "FLEET_CHAT_DB_MAX_CONN_LIFETIME", kind: kindDuration},
	{key: "FLEET_CHAT_DB_CONNECT_TIMEOUT", kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONNS", kind: kindInt},
	{key: "FLEET_SCHED_DB_MIN_CONNS", kind: kindInt},
	{key: "FLEET_SCHED_DB_MAX_CONN_IDLE_TIME", kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONN_LIFETIME", kind: kindDuration},
	{key: "FLEET_SCHED_DB_CONNECT_TIMEOUT", kind: kindDuration},
	// min 1: a zero tick would spin the due-task scan; "as fast as possible"
	// is 1s, and the production default (30s) lives at the getenv site.
	{key: "FLEET_SCHED_TICK_SECONDS", kind: kindInt, min: bound(1)},
	// Deployment default for a task's max_retries when the create request
	// omits it (#1538); same 0–10 bounds as the per-task field.
	{key: "FLEET_TASK_DEFAULT_MAX_RETRIES", kind: kindInt, min: bound(0), max: bound(10)},
	// Kill switch for per-job scheduled workspaces (#1543): on, every
	// non-worktree scheduled run works in the shared root as before.
	{key: "FLEET_SCHEDULED_SHARED_WORKSPACE", kind: kindBool},

	// ── LLM (shared) ── bounds on the four hot-reloadable ceilings match
	// reload.go, so boot and reload agree (#1119).
	{key: "FLEET_MAX_ITERATIONS", kind: kindInt, min: bound(1), max: bound(10000)},
	{key: "FLEET_MAX_COST_USD", kind: kindFloat, min: bound(0)},
	{key: "FLEET_MAX_TOTAL_TOKENS", kind: kindInt, min: bound(0)},
	{key: "FLEET_DEFAULT_THINKING_BUDGET_TOKENS", kind: kindInt},
	{key: "FLEET_SHUTDOWN_GRACE_SECONDS", kind: kindInt},
	{key: "FLEET_TURN_TIMEOUT_SECONDS", kind: kindInt},
	{key: "FLEET_TEMPERATURE", kind: kindFloat, min: bound(0)},
	{key: "LLM_MAX_TOKENS", kind: kindInt},
	{key: "FLEET_ERROR_ANALYSIS_ENABLED", kind: kindBool},
	{key: "FLEET_AUTO_TITLE", kind: kindBool},
	{key: "FLEET_ESCALATION_SIGNALS_ENABLED", kind: kindBool},
	// Percent, the same 50–99 the admin setting allows: below 50 a coin flip
	// would count as frustration and stage cards on most messages.
	{key: "FLEET_ESCALATION_CLASSIFIER_THRESHOLD", kind: kindInt, min: bound(50), max: bound(99)},
	{key: "FLEET_MEMORY_AUTOINDEX_ENABLED", kind: kindBool},
	{key: "FLEET_APPROVAL_TIMEOUT_SECONDS", kind: kindInt},
	{key: "FLEET_AUTO_APPROVE_IN_TEST", kind: kindBool},
	// min 1: `fleet serve` hands this straight to admission.New, which floors
	// a 0 to total=1 with no interactive reserve — a box-wide concurrency cap
	// of ONE, not "use a default". (Only the standalone runner pool treats
	// n<1 as "fall back to 8".) A set 0 is a misconfiguration; refuse it.
	{key: "FLEET_MAX_CONCURRENT_AGENTS", kind: kindInt, min: bound(1)},

	// ── sub-agents (#175, #264, #1043) ──
	{key: "FLEET_SUBAGENTS_ENABLED", kind: kindBool},
	{key: "FLEET_SUBAGENTS_MAX_DEPTH", kind: kindInt},
	{key: "FLEET_SUBAGENTS_MAX_CHILDREN", kind: kindInt},
	// Out-of-range numeric fractions are CLAMPED by normalizeBudgetFraction
	// (documented: a misconfiguration must never mean "unbounded"), so no
	// bounds here — only a non-numeric value errors.
	{key: "FLEET_SUBAGENTS_BUDGET_FRACTION", kind: kindFloat},

	// ── run-history retention (#252) + task pacing (#230, #510) ──
	{key: "FLEET_RUN_LOG_RETENTION_DAYS", kind: kindInt},
	{key: "FLEET_KEEP_RUNS_PER_TASK", kind: kindInt},
	{key: "FLEET_CLEANUP_HOUR", kind: kindInt},
	{key: "FLEET_TASK_STARVATION_WINDOW_MINUTES", kind: kindInt},
	{key: "FLEET_PAUSED_TASK_EXPIRY_MINUTES", kind: kindInt},

	// ── host reclamation + disk backpressure ──
	// The prune age is floored at zero (which disables the sweep) rather than
	// allowed negative, and the free-space floor is a percentage, so it is
	// bounded to [0, 100] — a typo like 500 would otherwise shed all scheduled
	// work forever.
	{key: "FLEET_WORKTREE_PRUNE_AGE", kind: kindDuration, min: bound(0)},
	{key: "FLEET_DISK_MIN_FREE_PERCENT", kind: kindInt, min: bound(0), max: bound(100)},

	// ── process log file sink (#298) + log archival (#272) ──
	{key: "FLEET_LOG_MAX_SIZE_MB", kind: kindInt},
	{key: "FLEET_LOG_MAX_AGE_DAYS", kind: kindInt},
	{key: "FLEET_LOG_MAX_BACKUPS", kind: kindInt},
	{key: "FLEET_LOG_COMPRESS", kind: kindBool},
	{key: "FLEET_LOG_ARCHIVE_AFTER_DAYS", kind: kindInt},

	// ── remote MCP OAuth (#443) ──
	{key: "FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP", kind: kindBool},

	// ── rate limit (interactive) ──
	{key: "CHAT_RATE_PER_MIN", kind: kindInt},
	{key: "CHAT_RATE_PER_DAY", kind: kindInt},
	{key: "FLEET_CHAT_RATE_LIMIT_ENABLED", kind: kindBool},
	{key: "FLEET_CHAT_RATE_LIMIT_CONCURRENT", kind: kindInt},

	// ── sandbox ──
	{key: "FLEET_PII_REDACTION_ENABLED", kind: kindBool},
	{key: "FLEET_CONTEXT_HANDLES_ENABLED", kind: kindBool},
	{key: "FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED", kind: kindBool},
	{key: "FLEET_MONTHLY_SOFT_TOKEN_LIMIT", kind: kindInt, min: bound(0)},
	{key: "FLEET_TEAM_CHATS_SHARED_BY_DEFAULT", kind: kindBool},
	// Waives the boot requirement for the open-egress NetworkPolicy, so it is
	// a security posture rather than a preference: registered like any other
	// bool so a typo ("ture") is refused at the seam instead of silently
	// reading as false and — here — as the SAFE value, which would hide the
	// operator's intent behind an error they never see.
	{key: "FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED", kind: kindBool},
	{key: "FLEET_A2A_ENABLED", kind: kindBool},
	{key: "FLEET_A2A_PUSH_ALLOW_PRIVATE", kind: kindBool},
	// min 1: a zero would mean "answer with the SUBMITTED row immediately",
	// which is what returnImmediately is for — a set 0 is a misconfiguration.
	{key: "FLEET_A2A_UNARY_WAIT_SECONDS", kind: kindInt, min: bound(1)}, // gitleaks:allow — a knob NAME, not a credential (generic-api-key entropy false positive)
	{key: "FLEET_SANDBOX_PIDS", kind: kindInt},
	// min 1: a set 0 would mean "no time at all for a sandbox start", which is
	// a misconfiguration — unset is the way to keep the backend default (#1358).
	{key: "FLEET_SANDBOX_START_TIMEOUT_SECONDS", kind: kindInt, min: bound(1)},
	{key: "FLEET_SANDBOX_DISK_GB", kind: kindInt},
	{key: "FLEET_SANDBOX_MEMORY_MAX_MB", kind: kindInt},
	{key: "FLEET_SANDBOX_CPUS_MAX", kind: kindFloat},
	{key: "FLEET_SANDBOX_PIDS_MAX", kind: kindInt},
	// min 0: 0 is a real value (no warm pool, #1264) and unset means "derive",
	// so every explicit negative — the -1 sentinel spelled out included — is a
	// misconfiguration to refuse at the seam, not something for the pool to
	// absorb (#1299).
	{key: "FLEET_SANDBOX_WARM_SIZE", kind: kindInt, min: bound(0)},
	{key: "FLEET_SANDBOX_WARM_TTL", kind: kindInt},

	// ── python REPL (#213) ──
	{key: "FLEET_PYTHON_CELL_TIMEOUT", kind: kindInt},
	{key: "FLEET_PYTHON_REPL_IDLE_TTL", kind: kindInt},
	{key: "FLEET_PYTHON_REPL_MAX", kind: kindInt},

	// ── lockdown / test harness ── FLEET_LOCKDOWN_ONLY is the security knob
	// whose silent fail-open motivated #1119: an unrecognized token used to
	// leave lockdown OFF; now it refuses to boot.
	{key: "FLEET_LOCKDOWN_ONLY", kind: kindBool},
	{key: "FLEET_MOCK_MODE", kind: kindBool},

	// ── central Auth sign-in ── the break-glass that turns Fleet passwords
	// back on while Auth SSO is configured (Config.PasswordLoginDisabled), and
	// the mark for an OIDC provider that is not central Auth
	// (CentralAuthConfigured).
	{key: "FLEET_OIDC_ALLOW_PASSWORD", kind: kindBool},
	{key: "FLEET_OIDC_GENERIC", kind: kindBool},

	// ─────────────────────────────────────────────────────────────────────
	// scopeExternal (#1273): parsed at the point of use, NOT by Load. Load
	// still validates every row below (externalKnobProblems), so a malformed
	// value refuses boot exactly like a loader knob; the consumers keep their
	// local default as defense-in-depth for the cases the boot gate cannot
	// cover (a value that appears only in a child process's env, a `fleet`
	// verb that never calls Load — those verbs resolve through the exported
	// EnvKnob* helpers instead).
	//
	// The bounds on each row are exactly the acceptance its consumer already
	// applies, so no value that is HONORED today starts being refused — what
	// changes is that a value which was silently DISCARDED now refuses boot.
	// ─────────────────────────────────────────────────────────────────────

	// ── orchestrator create/upload rate limits (cmd/fleet, read at serve
	// start into handlers.Config; 0 disables a window, so negatives — which
	// used to be honored and mean something undefined — are refused).
	{key: "FLEET_SCHED_RATE_LIMIT_PER_MINUTE", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "cmd/fleet (handlers.Config)"},
	{key: "FLEET_SCHED_RATE_LIMIT_PER_DAY", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "cmd/fleet (handlers.Config)"},
	{key: "FLEET_SCHED_RATE_LIMIT_GLOBAL_PER_MINUTE", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "cmd/fleet (handlers.Config)"},

	// ── outbound A2A delegation (#1368) ── read at the point of use because
	// the broker child registers the peer tools without ever running Load,
	// and the inbound depth guard shares the same ceiling. min 1: a zero
	// depth ceiling would refuse every inbound A2A create, which is what
	// leaving FLEET_A2A_ENABLED unset is for.
	{key: "FLEET_A2A_MAX_DELEGATION_DEPTH", kind: kindInt, min: bound(1), // gitleaks:allow — a knob NAME, not a credential (generic-api-key entropy false positive)
		scope: scopeExternal, readBy: "internal/agent (RegisterA2APeers) + cmd/fleet (buildA2AConfig)"},
	{key: "FLEET_A2A_CLIENT_ALLOW_PRIVATE", kind: kindBool,
		scope: scopeExternal, readBy: "internal/agent (RegisterA2APeers)"},

	// ── backup retention (`fleet backup`, a verb that never calls Load —
	// internal/admincli resolves it through EnvKnobInt below).
	{key: "FLEET_BACKUP_RETENTION_DAYS", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/admincli (backup prune)"},

	// ── sandbox runtime sizing ── the Kata guest-memory overhead only ever
	// ADDS memory, but a typo silently sizing every VM off the default is the
	// #1119 bug class; the consumer requires a POSITIVE integer.
	{key: "FLEET_SANDBOX_KATA_OVERHEAD_MB", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/sandbox (kataOverheadMB)"},

	// ── agent runtime (internal/agentcore) ── read through agentcore's
	// fleetEnv reader or a plain read of the canonical name.
	{key: "FLEET_TOOL_DISCLOSURE_THRESHOLD", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/agentcore (deferred tool disclosure)"},
	{key: "FLEET_MODEL_CACHE_TTL_MINUTES", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/agentcore (OpenRouter catalog TTL)"},
	{key: "FLEET_RETRY_MAX_ATTEMPTS", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/agentcore (provider retry budget)"},
	// Clamped by the consumer to [MinMaxToolOutputBytes, HardMaxToolOutputBytes]
	// and to (0,1] respectively — a documented clamp, like the subagent budget
	// fraction above — so only a non-numeric value is refused.
	{key: "FLEET_MAX_TOOL_OUTPUT_BYTES", kind: kindInt,
		scope: scopeExternal, readBy: "internal/agentcore (model-output boundary)"},
	{key: "FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD", kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (context pressure)"},
	// Cost-aware compaction for scheduled runs (#1534): the per-call resent
	// prompt budget in tokens; 0 disables. Read lazily per check in agentcore.
	{key: "FLEET_CONTEXT_RESEND_BUDGET_TOKENS", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/agentcore (cost-aware context compaction)"},
	// Scheduled compaction elides stale tool results before buying a summary
	// (#1742); on unless set false. Read with strconv.ParseBool, so it takes
	// that narrower token set, like the kill-switches further down.
	{key: "FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (scheduled compaction elision)"},
	{key: "FLEET_CONTEXT_COMPACTION_THRESHOLD", kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (context compaction)"},
	// Clamped by the consumer to (0,1] like the two context thresholds above;
	// 1 disables in practice (the hard ceiling fires first at 100%).
	{key: "FLEET_BUDGET_WINDDOWN_FRACTION", kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (budget wind-down notice)"},
	// First-chunk watchdog (#1537): base deadline (default 75 s since #1585 —
	// a reasoning model's hidden thinking can pass 30 s before its first
	// visible token, and a route that does not stream reasoning shows the
	// watchdog nothing meanwhile; floor 5 s applied by the consumer) and cap
	// (default 180 s, never below the base) for the prompt-scaled wait on a
	// provider's first streamed event.
	{key: "FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS", kind: kindFloat, min: bound(0),
		scope: scopeExternal, readBy: "internal/agentcore (provider first-chunk watchdog)"},
	{key: "FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS", kind: kindFloat, min: bound(0),
		scope: scopeExternal, readBy: "internal/agentcore (provider first-chunk watchdog)"},
	// These agentcore kill-switches are read with strconv.ParseBool, so they
	// take its narrower token set (see kindStrconvBool).
	{key: "FLEET_DISABLE_PROMPT_CACHE", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (prompt-cache breakpoints)"},
	{key: "FLEET_DISABLE_OPENROUTER_MODELS", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (live model catalog)"},
	{key: "FLEET_SCHEDULED_AUTO_COMPACT", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (scheduled auto-compaction)"},

	// ── scheduled-run wall clock (internal/runner) ── "0" disables the
	// ceiling; a negative would mean "unbounded", which the consumer refuses.
	{key: "FLEET_TASK_WALL_TIMEOUT", kind: kindDuration, min: bound(0),
		scope: scopeExternal, readBy: "internal/runner (per-task wall clock)"},

	// ── task notifications (internal/notify) ──
	{key: "FLEET_NOTIFY_TIMEOUT", kind: kindDuration, min: bound(0),
		scope: scopeExternal, readBy: "internal/notify"},
	{key: "FLEET_NOTIFY_RETRIES", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/notify"},

	// ── chat HTTP surface (internal/httpapi) ── package-level vars, resolved
	// at package init, i.e. before Load; the boot gate below is what makes a
	// typo fatal rather than a log line nobody reads.
	{key: "FLEET_SSE_BUFFER_DURATION", kind: kindDuration, min: bound(0),
		scope: scopeExternal, readBy: "internal/httpapi (SSE replay retention)"},
	{key: "FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/httpapi (SSE replay cap)"},
	{key: "FLEET_SSE_HEARTBEAT_INTERVAL", kind: kindDuration, min: bound(0),
		scope: scopeExternal, readBy: "internal/httpapi (SSE keepalive)"},
	{key: "FLEET_MAINTENANCE_MIN_INTERVAL", kind: kindDuration, min: bound(0),
		scope: scopeExternal, readBy: "internal/httpapi (maintenance pacing)"},
	{key: "FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/httpapi (webhook trigger limit)"},

	// ── task workspace downloads (internal/sched/handlers) ──
	{key: "FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES", kind: kindInt64, min: bound(1),
		scope: scopeExternal, readBy: "internal/sched/handlers (workspace download cap)"},

	// ── Web Push triggers (internal/webpush) ── opt-OUT flags, read with
	// strconv.ParseBool: an unparseable value used to leave the trigger ON,
	// so `FLEET_PUSH_ON_TASK_COMPLETE=off` silently kept notifying.
	{key: "FLEET_PUSH_ON_TASK_COMPLETE", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/webpush"},
	{key: "FLEET_PUSH_ON_APPROVAL_REQUEST", kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/webpush"},

	// ── operator lifecycle CLI (internal/admincli) ──
	{key: "FLEET_UPDATE_WAIT_SECONDS", kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/admincli (restart/stop --wait-timeout default)"},

	// ── documented-lenient (#1273) ── the ONE knob whose leniency is
	// deliberate. Registered so validate-config preflights the syntax as an
	// advisory; boot does not refuse.
	// Added after the registry (#2052): the OTLP exporter's plaintext switch.
	{key: "FLEET_OTEL_INSECURE", kind: kindBool,
		scope: scopeExternal, readBy: "internal/otelsetup (exporter transport)"},
	{key: "FLEET_OTEL_SAMPLE_RATIO", kind: kindFloat,
		scope: scopeExternal, readBy: "internal/otelsetup (head sampler)",
		lenient: true,
		why:     "tracing dial with a documented total order over bad input: unset/unparseable/NaN/±Inf/≥1 all mean AlwaysSample and ≤0 means never. Refusing to boot the whole service over an observability knob is worse than sampling everything, so validate-config reports a malformed ratio as an advisory instead"},

	// ── built-in Microsoft 365 MCP proxy (internal/ms365mcp) ──
	{key: "MS365_MCP_ATTACHMENT_URL_TTL_S", kind: kindInt, min: bound(1), max: bound(3600),
		scope: scopeExternal, readBy: "internal/ms365mcp (attachment download ticket TTL)"},
}

// bound builds a *float64 for the snapshot literals above.
func bound(v float64) *float64 { return &v }
