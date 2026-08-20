// Package config loads and validates the unified fleet configuration.
//
// fleet runs one process that drives BOTH the long-running interactive
// chat-server and the one-shot scheduled (cutlass) task runner. This package
// is the UNION of chat's and cutlass's config loaders: one Config struct, one
// Load, and one env-file loader that covers both env families.
//
// Env prefix: the canonical prefix is FLEET_, with CHAT_/CUTLASS_ back-compat
// fallbacks so deployments that still set the legacy names keep working and so
// the lifted parity tests (which set the legacy names) stay green. Per-knob
// helpers (getenvFleet*) try FLEET_<SUFFIX> first, then CHAT_/CUTLASS_<SUFFIX>.
//
// Credential-account suffixing reuses internal/creds.ApplyClientSuffix — it is
// NOT re-implemented here.
//
// chat's URL-form DSN builder (url.UserPassword) is the standard DSN builder.
package config

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// Canonical + legacy env prefixes. FLEET_ wins; the two legacy prefixes are
// tried in order for back-compat. The two front-ends never set the same suffix
// to conflicting values, so order between the legacy prefixes is irrelevant.
const (
	canonicalPrefix = "FLEET_"
)

var legacyPrefixes = []string{"CHAT_", "CUTLASS_"}

// Environment-variable names shared by the allowlist and the loader for the
// generic email/SendGrid infrastructure fleet ships with. Client-specific MCP
// connector credentials are NOT enumerated here — the MCP catalog now lives in
// the client bundle's manifest (internal/clientconfig), which references its own
// env-var names; cmd/fleet registers those names via RegisterAllowedEnvVars.
const (
	envSendGridAPIKey    = "SENDGRID_API_KEY"    //nolint:gosec // G101: env var name, not a credential value
	envSendGridFromEmail = "SENDGRID_FROM_EMAIL" //nolint:gosec // G101: env var name, not a credential value
	envEmailS3Bucket     = "EMAIL_S3_BUCKET"
	envEmailS3Prefix     = "EMAIL_S3_PREFIX"
)

// DefaultTitleModel is the fallback for FLEET_TITLE_MODEL / CHAT_TITLE_MODEL.
// Mirrors the frontend's DEFAULT_MODEL (the recommended everyday pick).
const DefaultTitleModel = "google/gemini-3.7-flash"

// DefaultFromEmail is the fallback From address for outgoing mail. Neutral by
// default; a deployment overrides via SENDGRID_FROM_EMAIL / MAILBUX_FROM_EMAIL.
const DefaultFromEmail = "noreply@example.com"

// Sub-agent caps (#175, tightened for delegation #264): deliberately SMALL
// defaults. Depth bounds recursion; fan-out bounds how many children one parent
// may spawn; the budget fraction bounds each child's slice of the parent's
// remaining budget. All are hard refusals / hard caps when exceeded — combined
// with the budget split, they bound total sub-agent spend and prevent a spawn
// fork-bomb.
//
// The depth default is 1 (#264, "parent → sub-agent only"): the root task may
// delegate, but a child may NOT delegate further — the spawn tool is not even
// registered in a child (see internal/agent/subagent.go buildChild). An operator
// can raise FLEET_SUBAGENTS_MAX_DEPTH to allow deeper trees. The budget-fraction
// default is 0.10 (#264, "≤10% of the parent's remaining budget per child").
const (
	defaultSubagentsMaxDepth       = 1
	defaultSubagentsMaxChildren    = 5
	defaultSubagentsBudgetFraction = 0.10
)

// allowedEnvVars is the union allowlist of keys that may be set from a .env
// file. Anything else in the file is ignored. The process environment wins
// over the file so operators can override individual values per invocation.
//
// Every env var the loader reads MUST be listed here, or an env-file-only
// value is silently dropped — a bug class that has now recurred three times
// (the per-store DSNs, the boot-read operator keys below, and ~20 loader
// knobs in #1107). TestEnvFileAllowlistCoversLoaderReads mechanically
// cross-checks this map against every literal env read in internal/config
// and cmd/fleet, so a new read fails the suite until its key lands here.
var allowedEnvVars = map[string]bool{
	// ── chat transport / data ──
	"CHAT_SERVER_ADDR":                 true,
	"CHAT_SERVER_TOKEN":                true,
	"CHAT_DATA_DIR":                    true,
	"CONVERSATION_TTL_DAYS":            true,
	"CONVERSATION_UNPINNED_CAP":        true,
	"FLEET_AUTO_ARCHIVE_AFTER_DAYS":    true,
	"FLEET_INPUT_QUEUE_RETENTION_DAYS": true,
	"FLEET_TURN_EVENT_RETENTION_DAYS":  true,
	"FLEET_UPLOAD_MAX_BYTES":           true,

	// ── fleet transport / data (canonical) ──
	"FLEET_SERVER_ADDR":  true,
	"FLEET_SERVER_TOKEN": true,
	"FLEET_DATA_DIR":     true,

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
	"ADMIN_API_KEY":                        true,
	"CUTLASS_LOG_FILE":                     true,
	"FLEET_BACKUP_DIR":                     true,
	"FLEET_BACKUP_RETENTION_DAYS":          true,
	"FLEET_DEFAULT_TIMEZONE":               true,
	"FLEET_TIMEZONE":                       true,
	"FLEET_ORCHESTRATOR_ADDR":              true,
	"FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS":  true,
	"FLEET_OTEL_ENDPOINT":                  true,
	"FLEET_OTEL_SAMPLE_RATIO":              true,
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
	"DB_HOST":                           true,
	"DB_PORT":                           true,
	"DB_USER":                           true,
	"DB_PASSWORD":                       true,
	"DB_NAME":                           true,
	"DB_SSLMODE":                        true,

	// ── LLM (shared) ──
	"OPENROUTER_API_KEY":                   true,
	"OPENROUTER_BASE_URL":                  true,
	"FLEET_OPENROUTER_BASE_URL":            true,
	"CHAT_MAX_ITERATIONS":                  true,
	"CHAT_MAX_COST_USD":                    true,
	"CHAT_MAX_TOTAL_TOKENS":                true,
	"CHAT_TURN_TIMEOUT_SECONDS":            true,
	"FLEET_TURN_TIMEOUT_SECONDS":           true,
	"FLEET_SHUTDOWN_GRACE_SECONDS":         true,
	"CHAT_TEMPERATURE":                     true,
	"CHAT_TITLE_MODEL":                     true,
	"CHAT_METADATA_MODEL":                  true,
	"FLEET_DEFAULT_THINKING_BUDGET_TOKENS": true,
	"FLEET_MAX_ITERATIONS":                 true,
	"FLEET_MAX_COST_USD":                   true,
	"FLEET_MAX_TOTAL_TOKENS":               true,
	"FLEET_TEMPERATURE":                    true,
	"FLEET_TITLE_MODEL":                    true,
	"FLEET_METADATA_MODEL":                 true,
	"FLEET_ERROR_ANALYSIS_MODEL":           true,
	"FLEET_ERROR_ANALYSIS_ENABLED":         true,
	"FLEET_SELF_IMPROVE_ENABLED":           true,
	"FLEET_AUTO_TITLE":                     true,
	"FLEET_MEMORY_MODEL":                   true,
	"FLEET_MEMORY_AUTOINDEX_ENABLED":       true,
	"FLEET_MEMORY_GRAPH_MODEL":             true,
	"FLEET_MEMORY_GRAPH_ENABLED":           true,
	"FLEET_RECURRING_TASK_MODEL":           true,
	"FLEET_LIBRARY_PROMPT_MODEL":           true,
	"FLEET_APPROVAL_TIMEOUT_SECONDS":       true,
	"FLEET_AUTO_APPROVE_IN_TEST":           true,
	"FLEET_MAX_CONCURRENT_AGENTS":          true,
	"FLEET_TASK_WALL_TIMEOUT":              true,
	"FLEET_RUN_LOG_RETENTION_DAYS":         true,
	"FLEET_WORKTREE_PRUNE_AGE":             true,
	"FLEET_DISK_MIN_FREE_PERCENT":          true,
	// Read by internal/httpapi directly (not the loader), but listed so an
	// operator can set it in FLEET_ENV_FILE like every other knob.
	"FLEET_MAINTENANCE_MIN_INTERVAL":    true,
	"FLEET_KEEP_RUNS_PER_TASK":          true,
	"FLEET_TASK_MEMORY_MAX_KEYS":        true,
	"FLEET_TASK_MEMORY_MAX_VALUE_BYTES": true,
	"FLEET_CLEANUP_HOUR":                true,
	"LLM_MAX_TOKENS":                    true,
	"FLEET_MAX_TOOL_OUTPUT_BYTES":       true,

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

	// ── personas / protocols ──
	// The two default-persona knobs are read through the prefix alias machinery
	// with a bare-name fallback (getenvFleetOrBare), so the canonical FLEET_
	// spelling must survive the env-file allowlist alongside the historical one.
	"FLEET_PERSONA_DEFAULT": true,
	"PERSONA_DEFAULT":       true,
	"FLEET_PERSONA":         true,
	"PERSONA":               true,
	"SYSTEM_PROMPT":         true,

	// ── cutlass agent knobs ──
	"MAX_ITERATIONS":           true,
	"CUTLASS_TEMPERATURE":      true,
	"CUTLASS_MAX_COST_USD":     true,
	"CUTLASS_MAX_TOTAL_TOKENS": true,

	// ── cutlass image gen ──
	"CUTLASS_IMAGE_OUTPUT":    true,
	"CUTLASS_IMAGE_MODEL":     true,
	"OPENROUTER_HTTP_REFERER": true,
	"OPENROUTER_X_TITLE":      true,

	// ── cutlass task input (set by runner) ──
	// The canonical FLEET_ spellings stay adjacent to their legacy twins: the
	// loader resolves FLEET_TASK_MODEL → CHAT_ → CUTLASS_ (#1015), so both must
	// survive the env-file filter (#1107).
	"FLEET_TASK_MODEL":            true,
	"CUTLASS_TASK_MODEL":          true,
	"FLEET_TASK_FALLBACK_MODEL":   true,
	"CUTLASS_TASK_FALLBACK_MODEL": true,

	// ── model tiers (#1187): env defaults of the default_model /
	// advanced_model admin settings ──
	"FLEET_DEFAULT_MODEL":  true,
	"FLEET_ADVANCED_MODEL": true,
	"CUTLASS_ALLOWED_DIRS": true,
	"GH_TOKEN":             true,

	// ── phone a friend: super-LLM review (#175) ──
	"FLEET_PHONE_A_FRIEND_ENABLED": true,
	"FLEET_PHONE_A_FRIEND_MODEL":   true,

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
	envEmailS3Bucket:                true,
	envEmailS3Prefix:                true,
	"EMAIL_S3_DATE_PREFIX_FORMAT":   true,
	"EMAIL_S3_MAX_DATE_PREFIX_DAYS": true,
	"EMAIL_ATTACHMENT_DIR":          true,
	"EMAIL_LAST_CHECK_FILE":         true,
	envSendGridAPIKey:               true,
	envSendGridFromEmail:            true,

	// ── MCP: mailbux (chat-only) ──
	"MAILBUX_USERNAME":             true,
	"MAILBUX_PASSWORD":             true,
	"MAILBUX_FROM_EMAIL":           true,
	"MAILBUX_JMAP_BASE_URL":        true,
	"MAILBUX_SMTP_HOST":            true,
	"MAILBUX_SMTP_PORT":            true,
	"MAILBUX_DOWNLOAD_DIR":         true,
	"MAILBUX_JMAP_TIMEOUT_SECONDS": true,
	"MAILBUX_QUERY_PAGE_LIMIT":     true,
	"MAILBUX_SEARCH_MAX_SCAN":      true,

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
	"CHAT_SANDBOX_IMAGE":                      true,
	"CHAT_SANDBOX_RUNTIME":                    true,
	"CHAT_WORKSPACE_ROOT":                     true,
	"FLEET_SANDBOX_IMAGE":                     true,
	"FLEET_SANDBOX_RUNTIME":                   true,
	"FLEET_DEFAULT_NETWORK_MODE":              true,
	"FLEET_PII_REDACTION_ENABLED":             true,
	"FLEET_PII_REDACTION_MODE":                true,
	"FLEET_PII_REDACTION_ENGINE":              true,
	"FLEET_PII_RAMPART_URL":                   true,
	"FLEET_GUARDRAIL_MODE":                    true,
	"FLEET_GUARDRAIL_PROFILE":                 true,
	"FLEET_GUARDRAIL_URL":                     true,
	"FLEET_CONTEXT_HANDLES_ENABLED":           true,
	"FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED": true,
	"FLEET_SANDBOX_MEMORY":                    true,
	"FLEET_SANDBOX_CPUS":                      true,
	"FLEET_SANDBOX_PIDS":                      true,
	"FLEET_SANDBOX_KATA_OVERHEAD_MB":          true,
	"FLEET_SANDBOX_DISK_GB":                   true,
	"FLEET_SANDBOX_MEMORY_MAX_MB":             true,
	"FLEET_SANDBOX_CPUS_MAX":                  true,
	"FLEET_SANDBOX_PIDS_MAX":                  true,
	"FLEET_SANDBOX_WARM_SIZE":                 true,
	"FLEET_SANDBOX_WARM_TTL":                  true,
	"FLEET_PYTHON_REPL_MODE":                  true,
	"FLEET_PYTHON_CELL_TIMEOUT":               true,
	"FLEET_PYTHON_REPL_IDLE_TTL":              true,
	"FLEET_PYTHON_REPL_MAX":                   true,
	"FLEET_WORKSPACE_ROOT":                    true,
	// Lockdown reads through the FLEET_ alias chain (#1080); the CHAT_
	// spellings stay allowlisted so existing env files keep sealing.
	"FLEET_LOCKDOWN_ONLY":           true,
	"FLEET_LOCKDOWN_ALLOWED_MODELS": true,
	"CHAT_LOCKDOWN_ONLY":            true,
	"CHAT_LOCKDOWN_ALLOWED_MODELS":  true,

	// ── test harness ──
	"CHAT_MOCK_MODE":  true,
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

// allowedEnvPrefixes admits open-ended user/account suffixes the operator names
// at provisioning time. A key matching one of these is treated like an exact
// allowlist entry. A client bundle may register additional prefixes via
// RegisterAllowedEnvPrefixes (e.g. per-user API-key prefixes for its
// connectors).
var allowedEnvPrefixes = []string{}

// registeredEnvVars holds env-var names admitted at runtime from the client
// bundle's manifest (RegisterAllowedEnvVars). Kept separate from the static
// allowedEnvVars map so the generic fleet allowlist stays client-agnostic while
// a bundle's connector credentials still survive the .env-file load. Guarded by
// registerMu because RegisterAllowedEnvVars runs at startup before Load.
var (
	registeredEnvVars = map[string]bool{}
	registerMu        sync.RWMutex
)

// RegisterAllowedEnvVars admits the given env-var names so a bundle's connector
// credentials (the names its manifest references) flow from a .env file into the
// process environment. Call once at startup, before Load. Names are matched
// exactly AND participate in the per-account "<BASE>_<SUFFIX>" rule, so an
// account variant of a registered base var is admitted too.
func RegisterAllowedEnvVars(names ...string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			registeredEnvVars[n] = true
		}
	}
}

// registeredNonReloadableSecrets holds env-var names registered at startup as
// per-request AUTH secrets — the client bundle's webhook/Slack signing-secret
// env vars, which verification reads via os.Getenv on every request. Config
// hot-reload (#286) documents auth secrets as explicitly NON-reloadable, but a
// file-sourced secret is not a process-env winner at boot, so the bootEnv
// restore alone would let a drifted .env copy silently rotate the live secret
// on reload (#584). Reload pins every name registered here to its boot value
// and reports a drifted file copy in Skipped instead. Guarded by registerMu
// (registration runs at startup before Load).
var registeredNonReloadableSecrets = map[string]bool{}

// RegisterNonReloadableSecretEnvVars marks the given env-var names as
// boot-pinned auth secrets: Reload restores their boot values (set OR unset)
// after re-applying the env file and surfaces any drift in
// ReloadResult.Skipped. Call once at startup, before Load — cmd/fleet registers
// the bundle's webhook signing-secret env names. This is orthogonal to the
// .env allowlist; callers still admit the names via RegisterAllowedEnvVars.
func RegisterNonReloadableSecretEnvVars(names ...string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			registeredNonReloadableSecrets[n] = true
		}
	}
}

// nonReloadableSecretNames snapshots the registered auth-secret env-var names.
func nonReloadableSecretNames() []string {
	registerMu.RLock()
	defer registerMu.RUnlock()
	out := make([]string, 0, len(registeredNonReloadableSecrets))
	for n := range registeredNonReloadableSecrets {
		out = append(out, n)
	}
	return out
}

// RegisterAllowedEnvPrefixes admits open-ended env-var prefixes from the client
// bundle (e.g. a per-user API-key prefix). Call once at startup, before Load.
func RegisterAllowedEnvPrefixes(prefixes ...string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	for _, p := range prefixes {
		if p = strings.TrimSpace(p); p != "" {
			allowedEnvPrefixes = append(allowedEnvPrefixes, p)
		}
	}
}

// isRegisteredEnvVar reports whether k was admitted at runtime via
// RegisterAllowedEnvVars.
func isRegisteredEnvVar(k string) bool {
	registerMu.RLock()
	defer registerMu.RUnlock()
	return registeredEnvVars[k]
}

// isAllowedEnvVar returns true when k may flow from a .env file into the
// process environment. A key is allowed when:
//
//  1. It is literally present in allowedEnvVars, OR registered at startup from
//     the client bundle's manifest via RegisterAllowedEnvVars, OR
//  2. It matches a prefix in allowedEnvPrefixes, OR
//  3. It matches "<BASE>_<UPPERCASE_ALPHANUMERIC_SUFFIX>" where BASE is an
//     allowlisted (static or registered) key (per-account credential-variant
//     suffixing, e.g. PROVIDER_API_KEY_ACCOUNTB). The suffix MUST be uppercase
//     alphanumeric so LD_PRELOAD / PATH_PRELOAD style keys cannot match.
func isAllowedEnvVar(k string) bool {
	if allowedEnvVars[k] || isRegisteredEnvVar(k) {
		return true
	}
	registerMu.RLock()
	prefixes := allowedEnvPrefixes
	registerMu.RUnlock()
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	idx := strings.LastIndex(k, "_")
	if idx <= 0 || idx == len(k)-1 {
		return false
	}
	base := k[:idx]
	suffix := k[idx+1:]
	if !allowedEnvVars[base] && !isRegisteredEnvVar(base) {
		return false
	}
	for _, r := range suffix {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// DBPoolConfig tunes one database/sql connection pool (#276). Zero values are
// not meaningful — Load always fills these from env with behavior-preserving
// defaults.
type DBPoolConfig struct {
	MaxOpenConns    int           // SetMaxOpenConns
	MaxIdleConns    int           // SetMaxIdleConns
	ConnMaxIdleTime time.Duration // SetConnMaxIdleTime (0 = unlimited)
	ConnMaxLifetime time.Duration // SetConnMaxLifetime (0 = unlimited)
	ConnectTimeout  time.Duration // initial-ping timeout on open
}

// LogConfig holds the OPT-IN rotating-file sink for fleet's process log (#298).
// The zero value (File == "") means the sink is OFF — the default — and the
// process logs only to stderr (journald rotates that under the systemd unit).
// These knobs apply ONLY when File is set.
type LogConfig struct {
	File       string // FLEET_LOG_FILE — empty disables the file sink (default)
	MaxSizeMB  int    // FLEET_LOG_MAX_SIZE_MB — rotate at this size (default 100)
	MaxAgeDays int    // FLEET_LOG_MAX_AGE_DAYS — delete rotated files older than this (0 = no age limit)
	MaxBackups int    // FLEET_LOG_MAX_BACKUPS — keep this many rotated files (default 7)
	Compress   bool   // FLEET_LOG_COMPRESS — gzip rotated files (default true)
	// Format selects the process-log output format (#178): "json" (default) emits
	// structured log/slog JSON — aggregation-friendly (Loki/Datadog/journald JSON)
	// — by routing the standard log package through an slog JSON handler; "text"
	// keeps the legacy plaintext lines exactly as before. FLEET_LOG_FORMAT.
	Format string
	// Level is the minimum slog level to emit: debug|info|warn|error (default
	// info). FLEET_LOG_LEVEL. Applies to the json format; the text format is
	// unlevelled (legacy behavior).
	Level string
}

// Config holds the union runtime configuration for fleet (interactive +
// scheduled). Interactive-only fields are inert for scheduled runs and vice
// versa.
type Config struct {
	// ── transport (interactive) ──
	Addr        string
	SharedToken string

	// ── IP access control (#314) ──
	// Network-level allow/deny filtering applied by the httpapi ipFilter
	// middleware as defense-in-depth in front of the shared-token auth. All three
	// are EMPTY by default, which is fully backward compatible: no allowlist means
	// every source IP is permitted, exactly as before.
	//
	// IPAllowlist is parsed from FLEET_IP_ALLOWLIST (comma-separated IPs/CIDRs).
	// When non-empty, ONLY addresses matching an entry may reach Fleet; empty =
	// allow all. Bare host addresses (e.g. 203.0.113.7) are coerced to /32 (IPv4)
	// or /128 (IPv6).
	IPAllowlist []*net.IPNet
	// IPDenylist is parsed from FLEET_IP_DENYLIST (comma-separated IPs/CIDRs).
	// Addresses matching an entry are ALWAYS blocked — deny overrides allow.
	IPDenylist []*net.IPNet
	// TrustedProxies is parsed from FLEET_TRUSTED_PROXIES (comma-separated IPs).
	// Only when the immediate peer (r.RemoteAddr) is one of these does Fleet read
	// the real client IP from X-Forwarded-For, walking the chain from the right
	// and skipping hops that are themselves trusted proxies (#1111). Empty (the
	// default) means X-Forwarded-For is NEVER consulted, so an untrusted client
	// cannot spoof an allowlisted address via the header. Operators MUST
	// explicitly opt in by naming their reverse-proxy (e.g. Caddy) IPs.
	TrustedProxies []net.IP

	// ── process lifecycle ──
	// ShutdownGraceSeconds bounds how long graceful shutdown (SIGTERM) waits for
	// in-flight chat turns and scheduled tasks to finish before force-cancelling
	// them. FLEET_SHUTDOWN_GRACE_SECONDS, default 30; 0 means no wait (force
	// immediately). Pair with the systemd unit's TimeoutStopSec (> this value) so
	// systemd sends SIGKILL only after fleet's own drain budget is spent.
	ShutdownGraceSeconds int

	// ── data (interactive) ──
	DataDir         string
	ConversationTTL int // days
	UnpinnedCap     int // per-user
	// InputQueueRetentionDays bounds how long completed/cancelled input-queue
	// rows retain their idempotency keys after terminal transition.
	// FLEET_INPUT_QUEUE_RETENTION_DAYS, default 30; 0 disables the purge.
	// Non-terminal rows are never retention candidates.
	InputQueueRetentionDays int
	// TurnEventRetentionDays bounds how long finished turns keep their durable
	// SSE ledger (turns rows plus turn_events and turn_journal via FK cascade)
	// after reaching a terminal state. The ledger exists for reattach/replay
	// and crash recovery — canonical history lives in messages — so it only
	// needs to outlive any plausible reconnect, not the conversation.
	// FLEET_TURN_EVENT_RETENTION_DAYS, default 14 (matching the conversation
	// TTL default); 0 disables the sweep. Running turns are never retention
	// candidates.
	TurnEventRetentionDays int
	// UploadMaxBytes is the per-file cap for chat /attachments uploads
	// (FLEET_UPLOAD_MAX_BYTES, default 1 GiB). Surfaced to the browser via
	// /server-config so the composer can refuse oversize files before the
	// upload round-trip. Deployments raising it past ~1 GiB must also raise
	// experimental.proxyClientMaxBodySize in web/next.config.ts, which caps
	// the whole proxied request at 2 GB.
	UploadMaxBytes int64
	// AutoArchiveAfterDays soft-archives unpinned conversations untouched for
	// this many days (#282). 0 (the default) disables it — a conversation is
	// then only ever archived by an explicit user action. FLEET_AUTO_ARCHIVE_AFTER_DAYS.
	AutoArchiveAfterDays int

	// SearchEnabled gates full-text search (#308): the GET /search endpoint plus
	// the message-content index maintenance + startup backfill.
	// FLEET_SEARCH_ENABLED, default true; set false to drop the GIN index upkeep
	// on a write-heavy deployment (the endpoint then returns 404).
	SearchEnabled bool

	// ConversationSoftDelete gates soft-delete behavior for conversations
	// (#279): when true, DELETE /conversations/{id} and the bulk delete paths
	// tombstone rows (deleted_at = NOW()) instead of hard-deleting, so a future
	// restore can undelete them within a 30-day window. List/Get/search hide
	// tombstoned rows; SweepExpired permanently purges rows older than 30 days.
	// FLEET_CONVERSATION_SOFT_DELETE, default false (hard delete — no behavior
	// change for existing deployments).
	ConversationSoftDelete bool

	// DatabaseURL is the Postgres DSN. URL-form (url.UserPassword) DSN builder.
	DatabaseURL string

	// ── DB connection pools (#276) ──
	// Per-pool tuning for the two Postgres handles fleet opens (chat + sched).
	// Defaults preserve the historical hard-coded behavior: 25 max open, 5 max
	// idle, no idle-time reaping (unlimited), 5m max lifetime — so existing
	// deployments are unaffected. Operators opt into idle reaping by setting
	// FLEET_{CHAT,SCHED}_DB_MAX_CONN_IDLE_TIME. Combined default ceiling is 50
	// open connections — under Postgres' default max_connections=100.
	ChatDBPool  DBPoolConfig
	SchedDBPool DBPoolConfig

	// ── LLM (shared) ──
	OpenRouterAPIKey string
	MaxIterations    int
	MaxCostUSD       float64
	MaxTotalTokens   int
	// DefaultThinkingBudgetTokens is the global fallback Claude extended-thinking
	// budget (#220, FLEET_DEFAULT_THINKING_BUDGET_TOKENS). 0 (default) = thinking
	// off unless a conversation opts in. A non-zero value enables thinking for
	// every chat/scheduled run that has no per-conversation override, clamped into
	// Claude's [1024, 100000] window by the producer.
	DefaultThinkingBudgetTokens int
	TurnTimeoutSeconds          int
	// Temperature is the sampling temperature for BOTH interactive turns and
	// scheduled-task runs. It resolves through the standard FLEET_ → CHAT_ →
	// CUTLASS_ chain, so the legacy CUTLASS_TEMPERATURE spelling still works as
	// the last-resort alias (#1079 collapsed the separate scheduled-only knob
	// that read CUTLASS_TEMPERATURE exclusively).
	Temperature  float64
	LLMMaxTokens int
	TitleModel   string
	// MetadataModel is the fast/cheap model the suggest_branch_name /
	// suggest_commit_message / suggest_pr_description tools (#191) call to
	// produce git metadata. FLEET_METADATA_MODEL, defaulting to TitleModel so
	// existing deployments need zero new config.
	MetadataModel string
	// ErrorAnalysisModel is the fast/cheap model the post-failure error-recovery
	// diagnosis (#317) calls to classify a terminal task failure + suggest
	// remediation. FLEET_ERROR_ANALYSIS_MODEL, defaulting to MetadataModel (then
	// TitleModel) so deployments need zero new config.
	ErrorAnalysisModel string
	// SelfImproveEnabled gates the feedback→learned-instruction distiller (#516).
	// FLEET_SELF_IMPROVE_ENABLED, default false: feedback is always recorded,
	// but distillation + run-time injection only happen when enabled.
	SelfImproveEnabled bool
	// ErrorAnalysisEnabled gates the post-failure LLM diagnosis (#317).
	// FLEET_ERROR_ANALYSIS_ENABLED, default true. When false, no analysis goroutine
	// or model call fires on a terminal failure (cost/latency escape hatch); the
	// raw error_message is still recorded as before.
	ErrorAnalysisEnabled bool
	// AutoTitle gates the LLM auto-titler (#302). FLEET_AUTO_TITLE, default true.
	// When false, a new conversation keeps its instant heuristic title and no
	// title-model call is made (cost/latency escape hatch).
	AutoTitle bool

	// MemoryModel is the fast/cheap model the conversation memory auto-indexer
	// (#234) uses to extract durable facts from a completed turn.
	// FLEET_MEMORY_MODEL, defaulting to MetadataModel (then TitleModel) so
	// deployments need zero new config.
	MemoryModel string
	// RecurringTaskModel is the model the "promote a chat into a recurring task"
	// synthesizer (#455) uses to turn a conversation transcript into a clean,
	// self-contained scheduled-task prompt + suggested cadence. FLEET_RECURRING_TASK_MODEL,
	// defaulting to MetadataModel (then TitleModel) so deployments need zero new
	// config; point it at a larger model for richer prompt synthesis.
	RecurringTaskModel string
	// LibraryPromptModel is the model the "save a chat to the prompt library"
	// synthesizer uses to distill a conversation transcript into a clean,
	// reusable prompt-library draft (name + description + content).
	// FLEET_LIBRARY_PROMPT_MODEL, defaulting to MetadataModel (then TitleModel)
	// so deployments need zero new config; point it at a larger model for
	// richer prompt synthesis.
	LibraryPromptModel string
	// MemoryAutoIndexEnabled gates the memory auto-indexer (#234). Default
	// FALSE (opt-in): when off, the only memory-write paths are the manual
	// propose_memory tool + POST /memories, exactly as before. When on, each
	// completed turn is mined for durable facts that are surfaced as memory
	// PROPOSALS the user reviews — nothing is written live without consent.
	// FLEET_MEMORY_AUTOINDEX_ENABLED.
	MemoryAutoIndexEnabled bool
	// MemoryGraphModel is the fast/cheap model the knowledge-graph extractor
	// (#523) uses to mine an ACTIVE memory for entities + relations.
	// FLEET_MEMORY_GRAPH_MODEL, defaulting to MemoryModel (then MetadataModel,
	// then TitleModel) so deployments need zero new config.
	MemoryGraphModel string
	// MemoryGraphEnabled gates the temporal knowledge-graph layer's EXTRACTION
	// (#523). Default FALSE (opt-in): when off, no extraction goroutine or model
	// call ever fires and behavior is byte-for-byte unchanged. When on, a memory
	// becoming ACTIVE (manual create / accepted proposal) is mined async +
	// best-effort into derived entity/relation rows; the manual re-extract
	// endpoint also requires it. FLEET_MEMORY_GRAPH_ENABLED.
	MemoryGraphEnabled bool

	// ApprovalTimeoutSeconds is the global default-deny window (in seconds) for
	// critical-tool approval cards on the web path (#225). FLEET_APPROVAL_TIMEOUT_SECONDS,
	// default 3600. The original 300s default reliably denied the final, wanted
	// action of any run the user started and then stopped watching (the same
	// observation that motivated notify mode, #1153); an hour matches the promote
	// card's window — approvals are a human review step, and humans step away.
	// A still-pending approval older than this is auto-denied by the server-side
	// expiry sweep. It is the lowest-priority env layer of the resolution chain
	// (per-tool manifest > per-conversation > admin override > this global > the
	// hardcoded fallback); the admin Features panel can override it live
	// (approval_timeout_seconds — read through LiveApprovalTimeoutSeconds).
	// A value <= 0 is treated as "use the hardcoded default" at resolution time,
	// never as "deny instantly".
	ApprovalTimeoutSeconds int

	// AutoApproveInTest, when true, makes the approval stager auto-approve every
	// staged critical tool instead of waiting for a human (#225).
	// FLEET_AUTO_APPROVE_IN_TEST, default FALSE. This is a CI/test escape hatch
	// for pipelines with no human present and a mocked backend — it WEAKENS the
	// human-in-the-loop gate, so it must NEVER be enabled in production. Off by
	// default; a loud warning is logged at startup when it is on.
	AutoApproveInTest bool

	// MaxConcurrentAgents is the configured concurrency cap (FLEET_MAX_CONCURRENT_AGENTS,
	// default 8). It bounds simultaneous SCHEDULED tasks (the worker-pool semaphore)
	// and sizes the sandbox warm pool; interactive chat turns are NOT gated by it —
	// each takes a sandbox on demand, bounded only by host resources. A set value
	// must be >= 1 (#1119): `fleet serve` passes it into admission.New, which
	// floors 0 to a box-wide total of ONE turn with no interactive reserve — a
	// chat-starving cap, not "use a default" — so boot refuses 0 instead.
	MaxConcurrentAgents int

	// ── personas ──
	PersonaDefault string

	// ── scheduled task config (cutlass) ──
	TaskModel         string
	TaskFallbackModel string
	SystemPrompt      string
	Persona           string

	// ── model tiers (#1187) ──
	// DefaultModel / AdvancedModel seed the workspace's two role slots — what a
	// new conversation starts on and the suggest_advanced_model escalation
	// target — as the env-derived DEFAULTS of the default_model /
	// advanced_model admin settings. Empty means "use the compiled-in
	// agentcore.DefaultCoreModel / DefaultMaxModel". These are the CHAT tiers,
	// not the scheduler's TaskModel above, which stays its own knob.
	DefaultModel  string
	AdvancedModel string

	// ── phone a friend: super-LLM review (#175) ──
	// PhoneAFriendEnabled turns on a one-time, host-side review of a scheduled
	// run's answer/work by a (typically stronger) reviewer model before the run is
	// allowed to finish; material issues it reports are fed back as one more
	// enforcement round (the same shape as the end-of-run verifier). OFF by
	// default (FLEET_PHONE_A_FRIEND_ENABLED) so config/default behaviour is
	// unchanged. PhoneAFriendModel names the reviewer model slug
	// (FLEET_PHONE_A_FRIEND_MODEL); empty falls back to the task's fallback model.
	// The reviewer is just another host-side LLM call — its credentials never
	// enter the sandbox, the agent's model context, or logs.
	PhoneAFriendEnabled bool
	PhoneAFriendModel   string

	// ── sub-agents / delegation (#175, #264, #1043) ──
	// SubagentsEnabled turns on the spawn_subagent native tool fleet-wide, which
	// lets a governed run delegate a scoped piece of work to a CHILD run. The child
	// is not a new or weaker loop: it is another agentcore.Run governed exactly like
	// the parent (ADR-0001), inheriting the parent's sandbox network posture, MCP
	// + credential allowlist (least-privilege: it may only SUBTRACT), and a SLICE
	// of the parent's remaining cost/token budget — the parent ceiling is the hard
	// wall across all descendants. ON by default (#1043): the tool being registered
	// is the feature — the PARENT AGENT decides whether to actually spawn; a
	// sequential run that never delegates is a successful use of it.
	// FLEET_SUBAGENTS_ENABLED=false (or the Admin → Features override) is the
	// fleet-wide KILL SWITCH, not an enable.
	//
	// Delegation is ALSO gated PER TASK via the task's allow_delegation flag
	// (default true, #1043): the two compose as AND (see internal/scheduledrun) —
	// the operator only ever opts OUT, per task or fleet-wide. When the composed
	// gate is false the tool is not registered at all (structural, not a soft
	// check). Interactive chat registers the tool too (#1043) whenever the
	// fleet-wide flag is on; chat has no per-conversation opt-out column.
	//
	// SubagentsMaxDepth caps recursion; the default is 1 ("parent → sub-agent only",
	// #264) — a child does not get the spawn tool. SubagentsMaxChildren caps per-parent
	// fan-out (default 5). SubagentsBudgetFraction is each child's default/maximum
	// slice of the parent's REMAINING budget (default 0.10; a call requesting more is
	// refused). A spawn exceeding any cap is REFUSED with a tool-result error.
	// SubagentsModel names the default child model slug (FLEET_SUBAGENTS_MODEL); empty
	// inherits the parent's model. The child model is resolved HOST-SIDE (like the
	// phone-a-friend reviewer) so credentials stay host-side.
	SubagentsEnabled        bool
	SubagentsMaxDepth       int
	SubagentsMaxChildren    int
	SubagentsBudgetFraction float64
	SubagentsModel          string

	// ── agent self-improvement: persistent task memory (#198, #285) ──
	// A scheduled task that opts into Captain's Log (instruction_self_improve) gets
	// remember/recall tools backed by the task_memories table. These caps bound how
	// much a single task may accumulate so a long-lived recurring task cannot grow
	// its memory unbounded. TaskMemoryMaxKeys (>0) bounds the number of distinct
	// keys (oldest evicted LRU-style); TaskMemoryMaxValueBytes (>0) hard-rejects an
	// oversized value. Defaults: 100 keys, 4096 bytes.
	TaskMemoryMaxKeys       int
	TaskMemoryMaxValueBytes int

	// ── run-history retention (#252) ──
	// RunLogRetentionDays prunes terminal task runs (and their logs) older than
	// this many days in a daily sweep. <=0 disables pruning. Default 90.
	RunLogRetentionDays int
	// KeepRunsPerTask is the minimum number of most-recent terminal runs kept per
	// task regardless of age, so a task's last-known state is never pruned. Default 10.
	KeepRunsPerTask int
	// CleanupHour is the UTC hour (0–23) the daily retention sweep runs. Default 4.
	CleanupHour int

	// ── host reclamation + disk backpressure ──
	// WorktreePruneAge is how old an orphaned per-run git worktree directory
	// must be before the maintenance loop reclaims it
	// (FLEET_WORKTREE_PRUNE_AGE, default 24h; 0 disables the sweep).
	//
	// Age is the only signal available — a worktree belonging to a running task
	// is indistinguishable from one a crash abandoned — so the default is
	// deliberately longer than the default task wall-clock ceiling (4h). Lower
	// it only on a box whose tasks are known to be short.
	WorktreePruneAge time.Duration
	// DiskMinFreePercent is the free-space floor, as a percentage of the
	// filesystem holding the data dir, below which the box sheds BACKGROUND
	// load: scheduled tasks stop being claimed and /healthz reports degraded,
	// while interactive chat keeps working (FLEET_DISK_MIN_FREE_PERCENT,
	// default 5; 0 disables the check).
	//
	// Shedding scheduled work rather than everything is the point: a full disk
	// is usually produced by unattended runs, and stopping them is what gives
	// an operator room to intervene through the very chat interface they would
	// otherwise have lost.
	DiskMinFreePercent int

	// ── task priority queues: anti-starvation (#230) ──
	// TaskStarvationWindowMinutes promotes a pending task that has waited longer
	// than this (and is still less urgent than the High floor) up to High, so a
	// sustained stream of higher-priority work can never starve it. <=0 disables
	// promotion. Default 30.
	TaskStarvationWindowMinutes int
	// TaskPausedExpiryMinutes fails a task that has sat in
	// paused_awaiting_input longer than this (#510) — an unattended ask-pause
	// otherwise waits forever. FLEET_PAUSED_TASK_EXPIRY_MINUTES; <=0 disables
	// (the default, preserving the wait-forever behavior).
	TaskPausedExpiryMinutes int

	// ── process log file sink (#298) ──
	// Log is the OPT-IN rotating-file sink for fleet's process log. Default OFF
	// (LogFile empty): the process keeps writing to stderr exactly as before, which
	// journald already rotates under the shipped systemd unit (ADR-0004). Set
	// FLEET_LOG_FILE — typically a container/non-systemd deployment — to ALSO tee the
	// standard log lines to a size/age/backup-rotated file. It rotates the existing
	// log lines as-is; it does NOT convert them to structured JSON (slog migration
	// #178 is separate).
	Log LogConfig

	// ── log archival (#272) ──
	// LogArchiveAfterDays compresses (and optionally encrypts) the log payloads of
	// terminal tasks older than this many days IN PLACE, on the same daily sweep as
	// retention. Reads inflate archived payloads transparently. <=0 disables
	// archival. Default 0 (OFF) — conservative; opt in deliberately.
	LogArchiveAfterDays int
	// LogArchiveEncryptionKey is the optional 32-byte AES-256-GCM key (decoded from
	// the base64 FLEET_LOG_ARCHIVE_ENCRYPTION_KEY) used to encrypt archived log
	// payloads. nil/empty = archives are gzip-only. Held host-side; never logged.
	LogArchiveEncryptionKey []byte

	// ── remote (hosted) MCP servers + per-user OAuth (#443) ──
	// PublicBaseURL is the externally-reachable origin of the web app, e.g.
	// "https://fleet.example.com" (FLEET_PUBLIC_BASE_URL). The per-user remote-MCP
	// OAuth redirect URI is derived from it and must be byte-stable — it is NEVER
	// reconstructed from request headers. Required to enable the feature.
	PublicBaseURL string
	// MCPOAuthEncryptionKey is the optional 32-byte AES-256-GCM key (decoded from
	// the base64 FLEET_MCP_OAUTH_ENCRYPTION_KEY) that encrypts per-user remote-MCP
	// OAuth tokens at rest. nil/empty = the feature is OFF (fails closed). Held
	// host-side; never logged.
	MCPOAuthEncryptionKey []byte
	// RemoteMCPAllowInsecureHTTP permits adding http:// remote MCP servers
	// (FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP). Dev/test only; default false (https).
	RemoteMCPAllowInsecureHTTP bool

	// MCPServers is the runtime MCP server catalog. It is sourced from the
	// client bundle's manifest (internal/clientconfig) — cmd/fleet builds it via
	// Bundle.MCPServerConfigs() and assigns it here — NOT loaded from typed
	// credential fields. fleet itself ships no client connectors; the generic
	// default bundle's catalog is empty. Nil/empty means "no MCP servers".
	MCPServers map[string]MCPServerConfig

	// HTTPTools is the runtime inline HTTP-tool catalog (the manifest's http_tools:
	// section), sourced from the client bundle via Bundle.HTTPToolConfigs() and
	// assigned here, in manifest order. Header secrets are resolved host-side at
	// CALL time (Headers carries the resolved values), so this slice — like the MCP
	// catalog — is built in whichever process holds the connector credentials.
	// Empty (the generic default) means "no HTTP tools" and changes nothing.
	HTTPTools []HTTPToolConfig

	// ── TLS termination (chat server) ──
	// The standard deployment fronts the Next.js app (the ONLY public entrypoint)
	// with Caddy/Tailscale, which terminate TLS; the Go chat/orchestrator servers
	// bind loopback. These knobs let an operator instead terminate TLS directly at
	// the Fleet chat process. Default "off" — no behavior change. The orchestrator
	// stays loopback HTTP (it is impersonation-load-bearing and MUST stay 127.0.0.1).
	TLSMode      string // FLEET_TLS_MODE: off|manual|auto
	TLSCertFile  string // FLEET_TLS_CERT_FILE (manual)
	TLSKeyFile   string // FLEET_TLS_KEY_FILE (manual)
	TLSDomain    string // FLEET_TLS_DOMAIN (auto)
	TLSACMEDir   string // FLEET_TLS_ACME_DIR (auto cert cache)
	TLSACMEEmail string // FLEET_TLS_ACME_EMAIL (auto account contact)
	TLSHTTPAddr  string // FLEET_TLS_HTTP_ADDR: HTTP->HTTPS redirect + ACME challenge listener (default ":80")

	// ── attachments / uploads (generic infra) ──
	// EmailAttachmentDir is the host directory MCP tools write downloaded
	// attachments to and where uploads are staged for the sandbox bind mount.
	// Generic infrastructure, independent of any specific email connector.
	EmailAttachmentDir string

	// ── rate limit (interactive) ──
	RatePerMinute int
	RatePerDay    int
	// RateLimitEnabled is the master switch for chat rate limiting
	// (FLEET_CHAT_RATE_LIMIT_ENABLED, default true). When false, the RPM/day and
	// concurrent-turn limits are all bypassed without zeroing each counter.
	RateLimitEnabled bool
	// RateLimitConcurrent caps simultaneous in-flight turns per user
	// (FLEET_CHAT_RATE_LIMIT_CONCURRENT, default 5; 0 disables). Stops one user
	// from holding every worker slot with parallel long turns.
	RateLimitConcurrent int

	// ── admin (interactive) ──
	AdminEmails []string

	// ── sandbox ──
	SandboxImage   string
	SandboxRuntime string
	// PIIRedactionEnabled gates the OPTIONAL PII redaction pass (#450) applied to
	// tool output before it enters the model context. FLEET_PII_REDACTION_ENABLED,
	// default false (byte-for-byte unchanged when off). Provider-neutral; the
	// built-in redactor is deterministic (no model server).
	PIIRedactionEnabled bool
	// PIIRedactionMode is the strictness when enabled: "observe" (detect+audit,
	// pass through), "redact" (mask matches with [PII:<kind>] markers, the default
	// when enabled), or "block" (withhold the tool result from the model).
	// FLEET_PII_REDACTION_MODE. Validated + defaulted in cmd/fleet.
	PIIRedactionMode string
	// PIIRedactionEngine picks the detector: "pattern" (the built-in
	// deterministic regexes, the default) or "rampart" (the MiniLM ONNX
	// token-classification model behind an operator-deployed HTTP service —
	// see docs/PII-REDACTION.md). FLEET_PII_REDACTION_ENGINE. Validated +
	// defaulted in cmd/fleet; rampart additionally needs PIIRampartURL.
	PIIRedactionEngine string
	// PIIRampartURL is the Rampart detection service endpoint
	// (FLEET_PII_RAMPART_URL), e.g. http://127.0.0.1:8787/v1/redact. Empty =
	// the rampart engine cannot activate.
	PIIRampartURL string
	// GuardrailMode controls optional host-side prompt-injection screening at
	// untrusted ingress: off (default), observe, or block.
	GuardrailMode string
	// GuardrailProfile is the detector profile, default "prompt-injection".
	GuardrailProfile string
	// GuardrailURL is the operator-deployed detector endpoint.
	GuardrailURL string
	// ContextHandlesEnabled gates inline composer context handles (#517): a chat
	// message may contain `@url:<url>` (host-side SSRF-guarded fetch) and
	// `@file:"path"` (read from the conversation workspace, path-gated) that the
	// server expands into the turn context. FLEET_CONTEXT_HANDLES_ENABLED, default
	// false — @url makes the server fetch a user-supplied URL, so it is opt-in.
	ContextHandlesEnabled bool
	// ConnectorRecommendationsEnabled gates connector auto-recommendation (#512):
	// when a chat message matches an Optional MCP connector the user has NOT
	// enabled, a note is added to the turn so the agent can suggest connecting it
	// (via the existing /settings/connections OAuth flow — never auto-connecting).
	// FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED, default false.
	ConnectorRecommendationsEnabled bool
	// DefaultNetworkMode is the fleet-wide sandbox egress posture (#211):
	// "" / "open" (full slirp4netns egress for networked work — the default),
	// "allowlisted" (networked sandboxes route HTTP(S) through the host egress
	// proxy, limited to SandboxNetworkAllowlist), or "lockdown" (a fleet-wide
	// kill-switch: every sandbox is sealed regardless of a task's AllowNetwork).
	// From FLEET_DEFAULT_NETWORK_MODE.
	DefaultNetworkMode string
	// SandboxNetworkAllowlist is the default domain allowlist for allowlisted
	// mode, resolved from the bundle manifest's sandbox.network_allowlist at boot
	// (not an env var). Exact domains or "*."-prefixed wildcards.
	SandboxNetworkAllowlist []string
	// Per-container cgroup caps (empty/0 → sandbox defaults: 512m / 1.0 / 128).
	// Operators size these to the host the docs told them to provision.
	SandboxMemory string
	SandboxCPUs   string
	SandboxPids   int
	// Per-task override ceilings (#205): a scheduled task's SandboxLimits may
	// raise its container above the global SandboxMemory/CPUs/Pids, but never
	// past these operator-set maxima. 0 = no ceiling (any override accepted).
	// Defaults 8192 MiB / 16.0 CPUs / 1024 pids.
	SandboxMemoryMaxMB int
	SandboxCPUsMax     float64
	SandboxPidsMax     int
	// SandboxDiskGB is the sandbox disk quota, in GiB
	// (FLEET_SANDBOX_DISK_GB). 0 → sandbox default (5); negative disables the
	// quota. It bounds any SINGLE file written from INSIDE the sandbox, which
	// stops the classic `dd` bomb from filling the host disk (#216). It does
	// NOT bound total workspace bytes, and it does not apply to host-side
	// brokers that stage files into the workspace (attachments, download_url,
	// generate_image), which carry their own separate caps — the attachment
	// one is FLEET_UPLOAD_MAX_BYTES, default 1 GiB and operator-tunable. See
	// sandbox.ContainerConfig.DiskLimitGB.
	SandboxDiskGB int
	// SandboxWarmSize overrides the warm-pool depth (FLEET_SANDBOX_WARM_SIZE).
	// 0 (default) derives it from MaxConcurrentAgents (clamped 2..8); a positive
	// value pins the depth explicitly (#181).
	SandboxWarmSize int
	// SandboxWarmTTLSeconds bounds how long a warm container may sit idle before
	// it is reaped and replaced (FLEET_SANDBOX_WARM_TTL, default 300). 0 disables
	// TTL reaping (#181).
	SandboxWarmTTLSeconds int

	// ── python REPL (#213) ──
	// PythonREPLMode selects the run_python kernel lifetime
	// (FLEET_PYTHON_REPL_MODE). "per-turn" (the default) keeps the legacy
	// behaviour: a fresh sandbox + kernel each turn, destroyed at turn end.
	// "persistent" reuses ONE sandbox+kernel per conversation across turns so
	// variables/imports survive between turns. Persistent mode applies only to
	// non-lockdown interactive chat; lockdown and scheduled runs always stay
	// per-turn. Any other value falls back to "per-turn" with a warning.
	PythonREPLMode string
	// PythonCellTimeoutSeconds is a host-operator ceiling on a single run_python
	// cell (FLEET_PYTHON_CELL_TIMEOUT). 0 (default) disables the ceiling, leaving
	// the per-call timeout_seconds param (default 300) in charge. When >0, the
	// effective per-cell timeout is min(timeout_seconds, this).
	PythonCellTimeoutSeconds int
	// PythonREPLIdleTTLSeconds bounds how long a persistent per-conversation
	// sandbox may sit idle (no run_python call) before the reaper closes it
	// (FLEET_PYTHON_REPL_IDLE_TTL, default 1800 = 30m). Only meaningful when
	// PythonREPLMode == "persistent".
	PythonREPLIdleTTLSeconds int
	// PythonREPLMaxSessions bounds how many persistent per-conversation sandboxes
	// are kept (FLEET_PYTHON_REPL_MAX, default 32). SOFT cap: evaluated only when
	// a new one is created, evicting least-recently-used IDLE sessions, so the
	// live count can overshoot. Only meaningful when PythonREPLMode ==
	// "persistent". 0 disables the cap.
	PythonREPLMaxSessions int

	WorkspaceRoot         string
	LockdownOnly          bool
	LockdownAllowedModels []string

	// MockMode short-circuits LLM calls (e2e). FLEET_MOCK_MODE / CHAT_MOCK_MODE.
	MockMode bool

	// ── observability: Sentry error tracking (#193) ──
	// SentryDSN is the OPTIONAL Sentry-protocol ingest endpoint (Sentry or a
	// Better Stack Errors URL). Empty (the default) leaves Sentry completely
	// disabled — no SDK init, no transport, zero overhead. When set, cmd/fleet
	// initializes the SDK at boot and registers a deferred Flush. The DSN is a
	// public ingest identifier, not a secret; secrets never enter the SDK
	// because the BeforeSend hook scrubs every outbound event (see
	// internal/observability/sentry).
	SentryDSN string
	// Environment is the deployment tier used for Sentry scope tagging
	// ("production" | "staging" | "dev"). Empty falls back to "dev". Formalized
	// from the previously-implicit deploy convention so the field is a single
	// source of truth the Sentry init reads alongside other boot-time wiring.
	Environment string

	// reload backs config hot-reload (#286): synchronization for the Live* getters
	// plus the boot env snapshot Reload uses to reproduce boot precedence. Set by
	// Load; nil for a Config built directly in a test (the getters then read their
	// fields directly, which is safe because such a Config is never reloaded). See
	// reload.go.
	reload *reloadState
}

// MCPServerConfig is one scheduled-mode MCP server's spawn spec + allowlist.
type MCPServerConfig struct {
	Type          string
	Command       string
	Args          []string
	URL           string
	Env           map[string]string
	Headers       map[string]string
	Enabled       bool
	ToolAllowlist []string
	// Dir is the working directory a stdio server's subprocess launches in (the
	// client-config bundle root), so relative command args like `mcp/foo.py`
	// resolve against the bundle rather than the fleet process cwd. Empty for
	// HTTP servers and for catalogs that supply absolute args.
	Dir string
	// AccountVars are the base credential env-var names the account-suffix scan
	// uses to discover this server's provisioned `<VAR>_<ACCOUNT>` seats
	// (creds.AccountsFor). Surfaced (names only) in the MCP catalog + the
	// model-facing roster so a task/agent can discover valid account names.
	AccountVars []string

	// IdentityEnv names the env keys (of Env) that route identity or money
	// (manifest identity_env). A named-account variant spawn is refused when any
	// of these keeps its default-seat value un-overridden by the account suffix
	// — agentcore's inherited-routing-identity guard (see MCPServerBase).
	IdentityEnv []string

	// Optional-server metadata, carried from the bundle manifest (#205-adjacent
	// fix): Optional gates a server out of every turn unless the conversation
	// opted in (chat's Optional-server semantics); the rest drive the settings-UI
	// catalog. These MUST be propagated all the way to agent.MCPServerSpec or the
	// Gate-1 opt-in never fires and every connector's tools load on every turn,
	// blowing past the model's tool-count ceiling.
	Optional         bool
	DisplayName      string
	Description      string
	Beta             bool
	EnabledByDefault bool
	// DataSources: manifest-declared external data identifiers this connector
	// touches (display-only; see clientconfig.MCPServer.DataSources).
	DataSources []string

	// TLS hardens an http server's connection (CA pinning / mTLS / public-key
	// pin) when set in the manifest (#280); nil = default system TLS. Carried
	// through to agent.MCPServerSpec and agentcore.MCPServerBase so both the
	// scheduled and interactive registration paths apply it.
	TLS *mcp.TLSOptions

	// Probe is the manifest-declared read-only canary call for
	// `fleet mcp test --deep` (probe:); nil = none declared. Consumed only by
	// the probe CLI — the runtime registration paths ignore it.
	Probe *MCPProbeConfig
}

// MCPProbeConfig is one declared canary tool call for `fleet mcp test --deep`
// (the manifest probe: block after load). See clientconfig.ProbeDef for the
// authoring semantics; assertions stay minimal by design (call succeeds, not
// isError, optional Contains substring).
type MCPProbeConfig struct {
	Tool     string
	Args     map[string]interface{}
	Contains string
}

// HTTPToolConfig is one resolved inline HTTP tool (the manifest http_tools[]
// entry after env-header resolution). It is the credential-bearing runtime shape
// the MCP client registers as a synthetic-server tool: Headers holds the values
// already resolved from the host process env, so it — like MCPServerConfig — is
// built only in a process that legitimately holds the secrets and is never shipped
// to the sandbox or the model. The model sees only Name/Description/InputSchema
// and supplies the declared params; URL/BodyTemplate {param} substitution and the
// optional ResponseJQ filter run host-side at call time.
type HTTPToolConfig struct {
	Name         string
	Description  string
	Method       string
	URL          string
	Headers      map[string]string
	BodyTemplate string
	InputSchema  map[string]interface{}
	ResponseJQ   string
	Critical     bool
}

// applyEnvFile folds envFile's KEY=VALUE pairs into the process environment:
// only allowlisted keys (static allowlist + RegisterAllowedEnvVars names +
// prefixes + account-suffix shapes) are admitted, a pre-existing process-env
// value WINS over the file's, quotes are stripped, and a missing file is not
// an error. It returns the snapshot of allowlisted process-env winners taken
// BEFORE the file was applied — Load feeds that to the reload state so a
// hot-reload reproduces boot precedence exactly.
func applyEnvFile(envFile string) (map[string]string, error) {
	// Snapshot the process env so file-driven writes never clobber it. We walk
	// os.Environ() rather than the static allowlist because allowedEnvPrefixes
	// and the per-account suffix shape admit open-ended names. Strip quotes
	// immediately so restoration cannot re-introduce literal quotes that
	// podman/docker --env-file leaves in place.
	existing := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		if !isAllowedEnvVar(k) {
			continue
		}
		existing[k] = stripQuotes(kv[eq+1:])
	}

	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("load env file %s: %w", envFile, err)
		}
	}

	// Restore process-env winners.
	for key, v := range existing {
		_ = os.Setenv(key, v)
	}
	return existing, nil
}

// bootstrapEnvFileWritten records which allowlisted keys BootstrapEnvFile's
// early application introduced into the process env (i.e. keys sourced from
// the env FILE, not the operator's process env). Load subtracts them from its
// process-winner snapshot so the reload state keeps boot precedence identical
// to a boot without the early application: an env-file-sourced value must stay
// re-readable from the file on hot-reload, never frozen as a process winner.
// Guarded by registerMu (written at startup before Load, like the registered
// allowlist).
var bootstrapEnvFileWritten = map[string]bool{}

// BootstrapEnvFile applies the env file to the process environment EARLY —
// before the client bundle's manifest interpolation — with Load's exact
// admission and precedence rules (process env wins; missing file is not an
// error). clientconfig.Load calls it (once per process, gated there) so a
// ${VAR} reference outside the lazily-resolved connector env/header maps —
// url:, command:/args:, sandbox.image, providers[].base_url — resolves against
// the same environment the runtime uses (#1123). Load's own application
// afterwards is an idempotent re-read: process env wins, so it re-sets the
// same values and restores the same winners.
//
// Call contract: call before Load, and never with per-request/derived paths —
// this is a boot-time seam. Repeat calls are safe: the file-introduced-key
// bookkeeping MERGES (it never resets), so a second application can never
// promote an earlier application's file-sourced keys into "process winners"
// and silently freeze them against hot-reload.
func BootstrapEnvFile(envFile string) error {
	before, err := applyEnvFile(envFile)
	if err != nil {
		return err
	}
	// Record the keys this application introduced (allowlisted, present now,
	// absent from the pre-application winners): they are file-sourced, and Load
	// must not report them to the reload state as process-env boot winners.
	// Collected before taking registerMu — isAllowedEnvVar read-locks it.
	var written []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		if !isAllowedEnvVar(k) {
			continue
		}
		if _, winner := before[k]; !winner {
			written = append(written, k)
		}
	}
	registerMu.Lock()
	for _, k := range written {
		bootstrapEnvFileWritten[k] = true
	}
	registerMu.Unlock()
	return nil
}

// Load reads environment variables in this precedence order (highest wins):
//
//  1. Process environment (snapshotted and restored around file loads).
//  2. envFile (e.g. .env.local) — per-operator overrides.
//
// Missing files are NOT an error.
//
// Malformed values ARE an error (#1119): a numeric/bool/duration knob that is
// SET but does not parse (or falls outside its registered range, see knobs.go)
// makes Load fail with an error naming every offending variable, its value,
// and the expected format — never a silent fallback to the default. An unset
// knob still gets its default. The hot-reload path (reload.go) and
// `fleet validate-config` apply the same registry, so the three paths accept
// and reject identically.
func Load(envFile string) (*Config, error) {
	existing, err := applyEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	// Keys the BootstrapEnvFile early application wrote are in the process env
	// by now, but they came from the env FILE — drop them from the winner
	// snapshot so the reload state sees exactly the winners a boot without the
	// early application would have seen.
	registerMu.RLock()
	for k := range bootstrapEnvFileWritten {
		delete(existing, k)
	}
	registerMu.RUnlock()

	// lp collects every malformed numeric/bool/duration env value so boot can
	// refuse with ONE error naming all of them (#1119). The typed knobs parse
	// through the envKnobs registry (knobs.go) — the same table the hot-reload
	// path and `fleet validate-config` use — so the three paths agree on what
	// a given environment accepts or rejects.
	lp := &loadParser{}

	cfg := &Config{
		MCPServers: make(map[string]MCPServerConfig),

		// ── transport (interactive) ──
		Addr:        getenvFleetDefault("SERVER_ADDR", "127.0.0.1:8080"),
		SharedToken: getenvFleet("SERVER_TOKEN"),

		// ── TLS termination (chat server) ──
		TLSMode:      strings.ToLower(strings.TrimSpace(getenvDefault("FLEET_TLS_MODE", "off"))),
		TLSCertFile:  os.Getenv("FLEET_TLS_CERT_FILE"),
		TLSKeyFile:   os.Getenv("FLEET_TLS_KEY_FILE"),
		TLSDomain:    os.Getenv("FLEET_TLS_DOMAIN"),
		TLSACMEDir:   getenvDefault("FLEET_TLS_ACME_DIR", "/var/lib/fleet/acme-cache"),
		TLSACMEEmail: os.Getenv("FLEET_TLS_ACME_EMAIL"),
		TLSHTTPAddr:  getenvDefault("FLEET_TLS_HTTP_ADDR", ":80"),

		// ── data (interactive) ──
		DataDir:                 getenvFleetDefault("DATA_DIR", "./data"),
		ConversationTTL:         lp.getenvInt("CONVERSATION_TTL_DAYS", 14),
		UnpinnedCap:             lp.getenvInt("CONVERSATION_UNPINNED_CAP", 50),
		InputQueueRetentionDays: lp.getenvFleetInt("INPUT_QUEUE_RETENTION_DAYS", 30),
		TurnEventRetentionDays:  lp.getenvFleetInt("TURN_EVENT_RETENTION_DAYS", 14),
		UploadMaxBytes:          lp.getenvFleetInt64("UPLOAD_MAX_BYTES", 1<<30),
		AutoArchiveAfterDays:    lp.getenvFleetInt("AUTO_ARCHIVE_AFTER_DAYS", 0),
		SearchEnabled:           lp.getenvBool("FLEET_SEARCH_ENABLED", true),
		ConversationSoftDelete:  lp.getenvBool("FLEET_CONVERSATION_SOFT_DELETE", false),
		DatabaseURL:             buildDatabaseURL(),

		// DB connection pools (#276) — defaults reproduce the historical
		// hard-coded behavior exactly: 25 open / 5 idle, idle-time reaping OFF
		// (0 = unlimited, matching the prior code which never called
		// SetConnMaxIdleTime), 5m lifetime; chat pings at 5s, sched at 10s.
		ChatDBPool: DBPoolConfig{
			MaxOpenConns:    lp.getenvFleetInt("CHAT_DB_MAX_CONNS", 25),
			MaxIdleConns:    lp.getenvFleetInt("CHAT_DB_MIN_CONNS", 5),
			ConnMaxIdleTime: lp.getenvFleetDuration("CHAT_DB_MAX_CONN_IDLE_TIME", 0),
			ConnMaxLifetime: lp.getenvFleetDuration("CHAT_DB_MAX_CONN_LIFETIME", 5*time.Minute),
			ConnectTimeout:  lp.getenvFleetDuration("CHAT_DB_CONNECT_TIMEOUT", 5*time.Second),
		},
		SchedDBPool: DBPoolConfig{
			MaxOpenConns:    lp.getenvFleetInt("SCHED_DB_MAX_CONNS", 25),
			MaxIdleConns:    lp.getenvFleetInt("SCHED_DB_MIN_CONNS", 5),
			ConnMaxIdleTime: lp.getenvFleetDuration("SCHED_DB_MAX_CONN_IDLE_TIME", 0),
			ConnMaxLifetime: lp.getenvFleetDuration("SCHED_DB_MAX_CONN_LIFETIME", 5*time.Minute),
			ConnectTimeout:  lp.getenvFleetDuration("SCHED_DB_CONNECT_TIMEOUT", 10*time.Second),
		},

		// ── LLM (shared) ──
		OpenRouterAPIKey: stripQuotes(os.Getenv("OPENROUTER_API_KEY")),
		MaxIterations:    lp.getenvFleetInt("MAX_ITERATIONS", 300),
		MaxCostUSD:       lp.getenvFleetFloat("MAX_COST_USD", 50.0),
		MaxTotalTokens:   lp.getenvFleetInt("MAX_TOTAL_TOKENS", 10000000),

		DefaultThinkingBudgetTokens: lp.getenvFleetInt("DEFAULT_THINKING_BUDGET_TOKENS", 0),

		ShutdownGraceSeconds: lp.getenvFleetInt("SHUTDOWN_GRACE_SECONDS", 30),

		TurnTimeoutSeconds:     lp.getenvFleetInt("TURN_TIMEOUT_SECONDS", 1800),
		Temperature:            lp.getenvFleetFloat("TEMPERATURE", 0.3),
		LLMMaxTokens:           lp.getenvInt("LLM_MAX_TOKENS", 16384),
		TitleModel:             getenvFleetDefault("TITLE_MODEL", DefaultTitleModel),
		MetadataModel:          getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel)),
		ErrorAnalysisModel:     getenvFleetDefault("ERROR_ANALYSIS_MODEL", getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel))),
		ErrorAnalysisEnabled:   lp.getenvFleetBool("ERROR_ANALYSIS_ENABLED", true),
		SelfImproveEnabled:     lp.getenvFleetBool("SELF_IMPROVE_ENABLED", false),
		AutoTitle:              lp.getenvFleetBool("AUTO_TITLE", true),
		MemoryModel:            getenvFleetDefault("MEMORY_MODEL", getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel))),
		RecurringTaskModel:     getenvFleetDefault("RECURRING_TASK_MODEL", getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel))),
		LibraryPromptModel:     getenvFleetDefault("LIBRARY_PROMPT_MODEL", getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel))),
		MemoryAutoIndexEnabled: lp.getenvFleetBool("MEMORY_AUTOINDEX_ENABLED", false),
		MemoryGraphModel:       getenvFleetDefault("MEMORY_GRAPH_MODEL", getenvFleetDefault("MEMORY_MODEL", getenvFleetDefault("METADATA_MODEL", getenvFleetDefault("TITLE_MODEL", DefaultTitleModel)))),
		MemoryGraphEnabled:     lp.getenvFleetBool("MEMORY_GRAPH_ENABLED", false),
		ApprovalTimeoutSeconds: lp.getenvFleetInt("APPROVAL_TIMEOUT_SECONDS", 3600),
		AutoApproveInTest:      lp.getenvFleetBool("AUTO_APPROVE_IN_TEST", false),
		MaxConcurrentAgents:    lp.getenvFleetInt("MAX_CONCURRENT_AGENTS", 8),

		// ── personas ──
		PersonaDefault: getenvFleetOrBare("PERSONA_DEFAULT", "assistant"),

		// ── scheduled task (cutlass) ──
		// getenvFleet, not a bare os.Getenv: these resolve FLEET_TASK_MODEL →
		// CHAT_TASK_MODEL → CUTLASS_TASK_MODEL like every other knob here, and
		// like EnvAliases already advertises for them. Reading only the legacy
		// spelling silently dropped FLEET_TASK_MODEL on deployments that follow
		// the documented convention, leaving every scheduled task to dead-letter
		// with "no model configured" (#1015).
		TaskModel:         getenvFleet("TASK_MODEL"),
		TaskFallbackModel: getenvFleet("TASK_FALLBACK_MODEL"),

		// ── model tiers (#1187) ──
		DefaultModel:  getenvFleet("DEFAULT_MODEL"),
		AdvancedModel: getenvFleet("ADVANCED_MODEL"),

		// ── phone a friend: super-LLM review (#175) ──
		PhoneAFriendEnabled: lp.getenvFleetBool("PHONE_A_FRIEND_ENABLED", false),
		PhoneAFriendModel:   getenvFleet("PHONE_A_FRIEND_MODEL"),

		// ── sub-agents / delegation (#175, #264, #1043) ──
		// Default TRUE (#1043): the parent agent decides whether to delegate;
		// FLEET_SUBAGENTS_ENABLED=false is the fleet-wide kill switch.
		SubagentsEnabled:        lp.getenvFleetBool("SUBAGENTS_ENABLED", true),
		SubagentsMaxDepth:       lp.getenvFleetInt("SUBAGENTS_MAX_DEPTH", defaultSubagentsMaxDepth),
		SubagentsMaxChildren:    lp.getenvFleetInt("SUBAGENTS_MAX_CHILDREN", defaultSubagentsMaxChildren),
		SubagentsBudgetFraction: normalizeBudgetFraction(lp.getenvFleetFloat("SUBAGENTS_BUDGET_FRACTION", defaultSubagentsBudgetFraction)),
		SubagentsModel:          getenvFleet("SUBAGENTS_MODEL"),

		// ── agent self-improvement: persistent task memory (#198, #285) ──
		TaskMemoryMaxKeys:       lp.getenvFleetInt("TASK_MEMORY_MAX_KEYS", 100),
		TaskMemoryMaxValueBytes: lp.getenvFleetInt("TASK_MEMORY_MAX_VALUE_BYTES", 4096),

		// ── host reclamation + disk backpressure ──
		WorktreePruneAge:   lp.getenvFleetDuration("WORKTREE_PRUNE_AGE", 24*time.Hour),
		DiskMinFreePercent: lp.getenvFleetInt("DISK_MIN_FREE_PERCENT", 5),

		// ── run-history retention (#252) ──
		RunLogRetentionDays: lp.getenvFleetInt("RUN_LOG_RETENTION_DAYS", 90),
		KeepRunsPerTask:     lp.getenvFleetInt("KEEP_RUNS_PER_TASK", 10),
		CleanupHour:         lp.getenvFleetInt("CLEANUP_HOUR", 4),

		// ── task priority queues: anti-starvation (#230) ── default 30m; 0 = OFF.
		TaskStarvationWindowMinutes: lp.getenvFleetInt("TASK_STARVATION_WINDOW_MINUTES", 30),
		// Paused-task expiry (#510): default 0 = OFF (paused tasks wait forever).
		TaskPausedExpiryMinutes: lp.getenvFleetInt("PAUSED_TASK_EXPIRY_MINUTES", 0),

		// ── process log file sink (#298) ── default OFF (LogFile empty): opt in
		// with FLEET_LOG_FILE. The size/age/backup/compress knobs only apply once
		// the file sink is on.
		Log: LogConfig{
			File:       getenvFleet("LOG_FILE"),
			MaxSizeMB:  lp.getenvFleetInt("LOG_MAX_SIZE_MB", 100),
			MaxAgeDays: lp.getenvFleetInt("LOG_MAX_AGE_DAYS", 0),
			MaxBackups: lp.getenvFleetInt("LOG_MAX_BACKUPS", 7),
			Compress:   lp.getenvFleetBool("LOG_COMPRESS", true),
			Format:     getenvFleetDefault("LOG_FORMAT", "json"),
			Level:      getenvFleetDefault("LOG_LEVEL", "info"),
		},

		// ── log archival (#272) ── default OFF (0): opt in deliberately.
		LogArchiveAfterDays:     lp.getenvFleetInt("LOG_ARCHIVE_AFTER_DAYS", 0),
		LogArchiveEncryptionKey: logArchiveEncryptionKey(),

		PublicBaseURL:              strings.TrimRight(strings.TrimSpace(getenvFleet("PUBLIC_BASE_URL")), "/"),
		MCPOAuthEncryptionKey:      mcpOAuthEncryptionKey(),
		RemoteMCPAllowInsecureHTTP: lp.getenvFleetBool("REMOTE_MCP_ALLOW_INSECURE_HTTP", false),

		// ── attachments / uploads (generic infra) ──
		EmailAttachmentDir: getenvDefault("EMAIL_ATTACHMENT_DIR", "./data/attachments"),

		// ── rate limit (interactive) ──
		RatePerMinute:       lp.getenvInt("CHAT_RATE_PER_MIN", 40),
		RatePerDay:          lp.getenvInt("CHAT_RATE_PER_DAY", 2000),
		RateLimitEnabled:    lp.getenvBool("FLEET_CHAT_RATE_LIMIT_ENABLED", true),
		RateLimitConcurrent: lp.getenvInt("FLEET_CHAT_RATE_LIMIT_CONCURRENT", 5),

		// ── admin ──
		AdminEmails: splitEmails(os.Getenv("ADMIN_EMAILS")),

		// ── sandbox ──
		SandboxImage:       getenvFleet("SANDBOX_IMAGE"),
		SandboxRuntime:     getenvFleet("SANDBOX_RUNTIME"),
		DefaultNetworkMode: strings.ToLower(strings.TrimSpace(getenvFleet("DEFAULT_NETWORK_MODE"))),

		// PII redaction (#450) — optional, default off.
		PIIRedactionEnabled: lp.getenvFleetBool("PII_REDACTION_ENABLED", false),
		PIIRedactionMode:    strings.ToLower(strings.TrimSpace(getenvFleet("PII_REDACTION_MODE"))),
		PIIRedactionEngine:  strings.ToLower(strings.TrimSpace(getenvFleet("PII_REDACTION_ENGINE"))),
		PIIRampartURL:       strings.TrimSpace(getenvFleet("PII_RAMPART_URL")),
		GuardrailMode:       strings.ToLower(strings.TrimSpace(getenvFleet("GUARDRAIL_MODE"))),
		GuardrailProfile:    strings.TrimSpace(getenvFleet("GUARDRAIL_PROFILE")),
		GuardrailURL:        strings.TrimSpace(getenvFleet("GUARDRAIL_URL")),

		// Composer context handles (#517) — optional, default off.
		ContextHandlesEnabled: lp.getenvFleetBool("CONTEXT_HANDLES_ENABLED", false),

		// Connector auto-recommendation (#512) — optional, default off.
		ConnectorRecommendationsEnabled: lp.getenvFleetBool("CONNECTOR_RECOMMENDATIONS_ENABLED", false),
		SandboxMemory:                   getenvFleet("SANDBOX_MEMORY"),
		SandboxCPUs:                     getenvFleet("SANDBOX_CPUS"),
		SandboxPids:                     lp.getenvInt("FLEET_SANDBOX_PIDS", 0),
		SandboxDiskGB:                   lp.getenvInt("FLEET_SANDBOX_DISK_GB", 0),
		// Per-task override ceilings (#205).
		SandboxMemoryMaxMB:    lp.getenvFleetInt("SANDBOX_MEMORY_MAX_MB", 8192),
		SandboxCPUsMax:        lp.getenvFleetFloat("SANDBOX_CPUS_MAX", 16.0),
		SandboxPidsMax:        lp.getenvFleetInt("SANDBOX_PIDS_MAX", 1024),
		SandboxWarmSize:       lp.getenvFleetInt("SANDBOX_WARM_SIZE", 0),
		SandboxWarmTTLSeconds: lp.getenvFleetInt("SANDBOX_WARM_TTL", 300),

		PythonREPLMode:           normalizePythonREPLMode(getenvFleetDefault("PYTHON_REPL_MODE", pythonREPLModePerTurn)),
		PythonCellTimeoutSeconds: lp.getenvFleetInt("PYTHON_CELL_TIMEOUT", 0),
		PythonREPLIdleTTLSeconds: lp.getenvFleetInt("PYTHON_REPL_IDLE_TTL", 1800),
		PythonREPLMaxSessions:    lp.getenvFleetInt("PYTHON_REPL_MAX", 32),

		WorkspaceRoot:         getenvFleet("WORKSPACE_ROOT"),
		LockdownOnly:          lp.getenvFleetBool("LOCKDOWN_ONLY", false),
		LockdownAllowedModels: splitLockdownModels(getenvFleet("LOCKDOWN_ALLOWED_MODELS")),
		MockMode:              lp.getenvFleetBool("MOCK_MODE", false),

		// ── observability: Sentry error tracking (#193) ──
		SentryDSN:   stripQuotes(os.Getenv("FLEET_SENTRY_DSN")),
		Environment: getenvDefault("FLEET_ENVIRONMENT", "dev"),
	}

	// Fail LOUD on any malformed numeric/bool/duration value (#1119): a typo'd
	// security knob (FLEET_LOCKDOWN_ONLY=enabled) or spend ceiling
	// (FLEET_MAX_COST_USD=5O) must refuse to boot rather than silently run on
	// the default — silent defaulting was fail-OPEN for lockdown. This matches
	// the IP-list/TLS/network-mode posture below and the hot-reload path,
	// which already rejected the same values (boot and reload now agree). An
	// UNSET knob still gets its default; only a set-but-malformed value errors,
	// and every problem is reported in one pass.
	if err := lp.err(); err != nil {
		return nil, err
	}

	// ── IP access control (#314) ──
	// Parsed here (not in the struct literal) so a malformed entry is a FATAL
	// startup error rather than being silently skipped — a silently-dropped
	// allowlist entry could leave the instance more open than the operator
	// intended. The returned error propagates up to cmd/fleet's log.Fatalf.
	var ipErr error
	if cfg.IPAllowlist, ipErr = parseCIDRList(getenvFleet("IP_ALLOWLIST")); ipErr != nil {
		return nil, fmt.Errorf("FLEET_IP_ALLOWLIST: %w", ipErr)
	}
	if cfg.IPDenylist, ipErr = parseCIDRList(getenvFleet("IP_DENYLIST")); ipErr != nil {
		return nil, fmt.Errorf("FLEET_IP_DENYLIST: %w", ipErr)
	}
	if cfg.TrustedProxies, ipErr = parseIPList(getenvFleet("TRUSTED_PROXIES")); ipErr != nil {
		return nil, fmt.Errorf("FLEET_TRUSTED_PROXIES: %w", ipErr)
	}

	// ── personas / prompts (cutlass file-name normalization) ──
	// Defaults are the generic bundle's names; a deployment sets FLEET_PERSONA /
	// SYSTEM_PROMPT (and FLEET_PERSONA_DEFAULT) to its bundle's own. FLEET_PERSONA
	// names a FILE the drivers load by basename out of personas/; PERSONA_DEFAULT
	// names a persona.
	cfg.SystemPrompt = getEnvOrDefault("SYSTEM_PROMPT", "default.md")
	if !hasKnownPromptExtension(cfg.SystemPrompt) {
		cfg.SystemPrompt += ".md"
	}
	cfg.Persona = getenvFleetOrBare("PERSONA", "personas/assistant.yaml")
	if !hasKnownPromptExtension(cfg.Persona) {
		cfg.Persona += ".yaml"
	}

	// The MCP server catalog is NOT built here — it is sourced from the client
	// bundle's manifest (internal/clientconfig) and assigned to cfg.MCPServers by
	// cmd/fleet at startup. Leave it as the empty map initialized above.

	// Lockdown is a no-op without an image. Surface the misconfiguration loudly.
	if cfg.LockdownOnly && cfg.SandboxImage == "" {
		fmt.Fprintln(os.Stderr, "warn: FLEET_LOCKDOWN_ONLY=true but sandbox image is unset; cannot enforce — treating as disabled")
		cfg.LockdownOnly = false
	}

	// Validate the sandbox egress mode (#211): an unknown value must fail loudly
	// rather than silently fall through to open egress.
	switch cfg.DefaultNetworkMode {
	case "", "open", "allowlisted", "lockdown":
	default:
		return nil, fmt.Errorf("FLEET_DEFAULT_NETWORK_MODE must be one of open|allowlisted|lockdown, got %q", cfg.DefaultNetworkMode)
	}

	// Capture the boot environment last, once the process env is in its final
	// (file-loaded, winners-restored) state, so a later hot-reload (#286) can
	// reproduce boot precedence exactly and report changed non-reloadable settings.
	cfg.reload = newReloadState(existing)

	return cfg, nil
}

// LockdownAvailable reports whether the lockdown affordance should be exposed.
func (c *Config) LockdownAvailable() bool {
	return c.SandboxImage != ""
}

// LockdownAllows reports whether the slug is on the lockdown allow-list. The
// allow-list entries may be exact slugs or `*`-glob patterns (e.g.
// "anthropic/*", "google/gemini-*"), matched via ModelAllowed — a superset of
// the historical exact-string behavior, so an entry with no wildcard still
// matches exactly as before.
func (c *Config) LockdownAllows(slug string) bool {
	return ModelAllowed(slug, c.LockdownAllowedModels)
}

// ModelAllowed reports whether slug matches any pattern in the allow-list.
// Patterns support `*` wildcards via path.Match (a `/`-aware glob); an entry
// with no metacharacters degrades to exact-string equality. An empty slug never
// matches. An empty pattern list returns false — callers that want "no list =
// allow all" must check len(list)==0 themselves, so the meaning of an empty list
// stays explicit at each call site.
func ModelAllowed(slug string, patterns []string) bool {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return false
	}
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == slug {
			return true
		}
		// path.Match only errors on a malformed pattern (e.g. an unterminated
		// `[`); treat that as "no match" rather than letting it match anything.
		if ok, err := path.Match(pat, slug); err == nil && ok {
			return true
		}
	}
	return false
}

// Validate checks the interactive (chat) required values. Use ValidateScheduled
// for the one-shot scheduled (cutlass) run.
func (c *Config) Validate() error {
	if c.OpenRouterAPIKey == "" && !c.MockMode {
		return fmt.Errorf("OPENROUTER_API_KEY is required (or set FLEET_MOCK_MODE=1 for tests)")
	}
	if c.SharedToken == "" {
		return fmt.Errorf("FLEET_SERVER_TOKEN is required (shared secret between Next.js and chat-server)")
	}
	if c.ConversationTTL <= 0 {
		return fmt.Errorf("CONVERSATION_TTL_DAYS must be positive")
	}
	if c.UnpinnedCap <= 0 {
		return fmt.Errorf("CONVERSATION_UNPINNED_CAP must be positive")
	}
	if c.InputQueueRetentionDays < 0 {
		return fmt.Errorf("FLEET_INPUT_QUEUE_RETENTION_DAYS must be non-negative")
	}
	if c.TurnEventRetentionDays < 0 {
		return fmt.Errorf("FLEET_TURN_EVENT_RETENTION_DAYS must be non-negative")
	}
	if c.UploadMaxBytes <= 0 {
		return fmt.Errorf("FLEET_UPLOAD_MAX_BYTES must be positive")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL (or DB_HOST/DB_USER/DB_NAME parts) is required")
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	return nil
}

// validateTLS checks the chat-server TLS knobs (FLEET_TLS_MODE and friends).
func (c *Config) validateTLS() error {
	switch c.TLSMode {
	case "", "off":
		return nil
	case "manual":
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return fmt.Errorf("FLEET_TLS_MODE=manual requires FLEET_TLS_CERT_FILE and FLEET_TLS_KEY_FILE")
		}
		return nil
	case "auto":
		if c.TLSDomain == "" {
			return fmt.Errorf("FLEET_TLS_MODE=auto requires FLEET_TLS_DOMAIN")
		}
		return nil
	default:
		return fmt.Errorf("FLEET_TLS_MODE=%q is invalid (want off|manual|auto)", c.TLSMode)
	}
}

// ValidateScheduled checks the one-shot scheduled (cutlass) required values and
// returns an error describing all problems found. Called at startup to fail
// fast for the scheduled driver.
func (c *Config) ValidateScheduled() error {
	var errs []string

	if c.OpenRouterAPIKey == "" {
		errs = append(errs, "OPENROUTER_API_KEY is required")
	} else if !strings.HasPrefix(c.OpenRouterAPIKey, "sk-or-") {
		errs = append(errs, "OPENROUTER_API_KEY should start with 'sk-or-' (got '"+c.OpenRouterAPIKey[:min(6, len(c.OpenRouterAPIKey))]+"...')")
	}

	if c.MaxIterations < 1 || c.MaxIterations > 10000 {
		errs = append(errs, fmt.Sprintf("MAX_ITERATIONS must be between 1 and 10000 (got %d)", c.MaxIterations))
	}
	if c.MaxCostUSD < 0 {
		errs = append(errs, fmt.Sprintf("CUTLASS_MAX_COST_USD must be >= 0 (got %.2f)", c.MaxCostUSD))
	}
	if c.MaxTotalTokens < 0 {
		errs = append(errs, fmt.Sprintf("CUTLASS_MAX_TOTAL_TOKENS must be >= 0 (got %d)", c.MaxTotalTokens))
	}
	if c.LLMMaxTokens < 256 {
		errs = append(errs, fmt.Sprintf("LLM_MAX_TOKENS must be >= 256 (got %d)", c.LLMMaxTokens))
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// buildDatabaseURL returns the Postgres DSN. chat's URL-form builder is the
// standard. Precedence: DATABASE_URL, else the DB_* parts with localhost
// defaults.
func buildDatabaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	host := getenvDefault("DB_HOST", "localhost")
	port := getenvDefault("DB_PORT", "5432")
	user := getenvDefault("DB_USER", "chat")
	pass := os.Getenv("DB_PASSWORD")
	name := getenvDefault("DB_NAME", "chat")
	ssl := getenvDefault("DB_SSLMODE", "disable")

	auth := url.User(user).String()
	if pass != "" {
		auth = url.UserPassword(user, pass).String()
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=%s", auth, host, port, name, ssl)
}

// splitLockdownModels parses the lockdown allow-list. Empty input returns the
// default (one slug per product tier slot).
func splitLockdownModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{
			"google/gemini-3.7-flash", // recommended default
			"openai/gpt-5.6-sol",      // strong tier
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// parseCIDRList parses a comma-separated list of IPs/CIDRs into networks (#314).
// A bare host address (no "/") is coerced to a single-host network (/32 for
// IPv4, /128 for IPv6). An empty input returns a nil slice (no filter). A
// malformed entry returns an error so the caller can fail startup loudly — a
// silently-dropped allowlist entry could leave the instance wide open.
func parseCIDRList(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			_, network, err := net.ParseCIDR(p)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", p, err)
			}
			out = append(out, network)
			continue
		}
		ip := net.ParseIP(p)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", p)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}

// parseIPList parses a comma-separated list of bare IP addresses into net.IPs
// (#314), used for FLEET_TRUSTED_PROXIES. Empty input returns a nil slice. A
// malformed entry returns an error so startup fails loudly rather than trusting
// a proxy the operator never meant to name.
func parseIPList(raw string) ([]net.IP, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]net.IP, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ip := net.ParseIP(p)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", p)
		}
		out = append(out, ip)
	}
	return out, nil
}

// splitEmails parses a comma-separated ADMIN_EMAILS value, normalized.
func splitEmails(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadEnvFile parses a KEY=VALUE env file, respecting the allowlist. Always
// overwrites — Load snapshots/restores the pre-existing process env around
// this call so "process env wins" holds.
//
// Value semantics (shared byte-for-byte with internal/creds's parser, whose
// writer encodes values to survive this exact round-trip — #834):
//
//   - an `export ` prefix is tolerated;
//   - an UNQUOTED value ends at the first ` #` or `\t#` (inline comment) — a
//     value that must contain a literal ` #` (e.g. a password like `p@ss #1`)
//     MUST be quoted, or it is truncated at the marker;
//   - ONE layer of matching surrounding quotes (single or double) is stripped;
//     the interior — including ` #`, edge whitespace, and inner quotes — is
//     preserved verbatim.
func loadEnvFile(path string) error {
	return loadEnvFileFiltered(path, func(string) bool { return true })
}

func loadEnvFileFiltered(path string, include func(string) bool) error {
	f, err := os.Open(path) // #nosec G304 — path comes from trusted config.
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tolerate `export KEY=…` for parity with internal/creds/envfile.go,
		// whose docs promise the two parsers agree byte-for-byte.
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		if include != nil && !include(k) {
			continue
		}
		v := stripInlineComment(strings.TrimSpace(line[eq+1:]))
		v = stripQuotes(v)
		if !isAllowedEnvVar(k) {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return sc.Err()
}

// stripInlineComment trims a trailing `# comment` off an unquoted .env value.
// Quoted values are left intact (quote handling owns those). This is the
// standard dotenv rule and the contract internal/creds's writer/parser rely
// on: an unquoted value containing ` #` is DELIBERATELY truncated there —
// quote the value to keep a literal ` #` (creds.SetEnvKey does this
// automatically). See loadEnvFile's doc comment for the full value semantics.
func stripInlineComment(value string) string {
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, `'`) {
		return value
	}
	if i := strings.Index(value, " #"); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	if i := strings.Index(value, "\t#"); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	return value
}

// stripQuotes removes a single layer of matching surrounding quotes.
func stripQuotes(value string) string {
	if (strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
		(strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`)) {
		if len(value) >= 2 {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func hasKnownPromptExtension(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".md")
}

// ── env helpers ──
//
// Unified value semantics (#1119): every helper below strips ONE layer of
// matching surrounding quotes from a set value (podman/docker --env-file keep
// quotes in place; the env-file loader and Load's process-env snapshot already
// strip them, so this is defense-in-depth that keeps every read path
// consistent). Numeric/bool/duration knobs are NOT read through these string
// helpers — they parse through the envKnobs registry (knobs.go) via Load's
// loadParser, which fails boot loudly on a set-but-malformed value.

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return stripQuotes(v)
	}
	return def
}

// getEnvOrDefault is cutlass's quote-stripping default helper.
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return stripQuotes(value)
	}
	return defaultValue
}

// ── FLEET_-prefixed env helpers with CHAT_/CUTLASS_ back-compat ──
//
// These try FLEET_<suffix> first, then each legacy prefix in order. The first
// non-empty value wins. They power the canonical-prefix-with-back-compat
// behavior for shared knobs (timeouts, ceilings, sandbox image).

func lookupFleet(suffix string) (string, bool) {
	suffix = strings.TrimLeft(suffix, "_")
	if v := os.Getenv(canonicalPrefix + suffix); v != "" {
		return v, true
	}
	for _, p := range legacyPrefixes {
		if v := os.Getenv(p + suffix); v != "" {
			return v, true
		}
	}
	return "", false
}

func getenvFleet(suffix string) string {
	if v, ok := lookupFleet(suffix); ok {
		return stripQuotes(v)
	}
	return ""
}

func getenvFleetDefault(suffix, def string) string {
	if v, ok := lookupFleet(suffix); ok {
		// Strip quotes like getenvFleet does: the same quoted value must
		// resolve identically whichever helper reads it (#1119).
		return stripQuotes(v)
	}
	return def
}

// getenvFleetOrBare resolves a knob whose historical spelling is UNPREFIXED
// (PERSONA, PERSONA_DEFAULT): the prefixed spellings first in lookupFleet's
// order, then the bare name, then def. The bare fallback keeps existing
// deployments reading; the prefixed lookup is what makes the documented FLEET_
// convention work at all — without it FLEET_PERSONA was silently ignored and the
// built-in default applied (#956). Quotes are stripped from whichever spelling
// wins, matching getEnvOrDefault.
func getenvFleetOrBare(suffix, def string) string {
	if v, ok := lookupFleet(suffix); ok {
		return stripQuotes(v)
	}
	return getEnvOrDefault(suffix, def)
}

// EnvAliases returns every spelling the prefix alias machinery resolves for
// the same knob as name: when name carries the canonical FLEET_ prefix or a
// legacy prefix, the canonical spelling plus one spelling per legacy prefix,
// in lookup order (see lookupFleet). A name without a recognized prefix has
// no aliases and is returned alone. Callers that must reason about a knob's
// whole alias family (e.g. the MCP broker's connector/parent env-separation
// gate) derive it here rather than hand-enumerating legacy spellings.
func EnvAliases(name string) []string {
	suffix, ok := strings.CutPrefix(name, canonicalPrefix)
	if !ok {
		for _, p := range legacyPrefixes {
			if suffix, ok = strings.CutPrefix(name, p); ok {
				break
			}
		}
	}
	if !ok || suffix == "" {
		return []string{name}
	}
	names := make([]string, 0, len(legacyPrefixes)+1)
	names = append(names, canonicalPrefix+suffix)
	for _, p := range legacyPrefixes {
		names = append(names, p+suffix)
	}
	return names
}

// logArchiveEncryptionKey decodes the optional base64 AES-256-GCM key for log
// archival (#272) from FLEET_LOG_ARCHIVE_ENCRYPTION_KEY. It returns nil when the
// key is unset (encryption off). A present-but-malformed or wrong-length key is
// a misconfiguration: it logs a warning (WITHOUT the key material) and returns
// nil so archival falls back to gzip-only rather than crashing or silently using
// a bad key. The decoded bytes are never logged.
func logArchiveEncryptionKey() []byte {
	raw := getenvFleet("LOG_ARCHIVE_ENCRYPTION_KEY")
	if raw == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		log.Printf("Warning: FLEET_LOG_ARCHIVE_ENCRYPTION_KEY is not valid base64; archives will be gzip-only (unencrypted)")
		return nil
	}
	if len(key) != 32 {
		log.Printf("Warning: FLEET_LOG_ARCHIVE_ENCRYPTION_KEY must decode to 32 bytes (got %d); archives will be gzip-only (unencrypted)", len(key))
		return nil
	}
	return key
}

// mcpOAuthEncryptionKey decodes the optional base64 AES-256-GCM key for per-user
// remote-MCP OAuth tokens (#443) from FLEET_MCP_OAUTH_ENCRYPTION_KEY. Unlike the
// log-archive key, a malformed/wrong-length value still returns nil (the feature
// stays OFF — fails closed) but logs loudly so the operator knows their key was
// rejected. The decoded bytes are never logged.
func mcpOAuthEncryptionKey() []byte {
	raw := getenvFleet("MCP_OAUTH_ENCRYPTION_KEY")
	if raw == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		log.Printf("Warning: FLEET_MCP_OAUTH_ENCRYPTION_KEY is not valid base64; remote-MCP OAuth stays DISABLED")
		return nil
	}
	if len(key) != 32 {
		log.Printf("Warning: FLEET_MCP_OAUTH_ENCRYPTION_KEY must decode to 32 bytes (got %d); remote-MCP OAuth stays DISABLED", len(key))
		return nil
	}
	return key
}

// Python REPL mode values for FLEET_PYTHON_REPL_MODE (#213).
const (
	pythonREPLModePerTurn    = "per-turn"
	pythonREPLModePersistent = "persistent"
)

// PersistentPythonREPL reports whether run_python should reuse ONE kernel per
// conversation across turns (FLEET_PYTHON_REPL_MODE=persistent) rather than the
// default fresh-per-turn kernel.
func (c *Config) PersistentPythonREPL() bool {
	return c.PythonREPLMode == pythonREPLModePersistent
}

// normalizePythonREPLMode validates FLEET_PYTHON_REPL_MODE and fails closed to
// the conservative "per-turn" default on any unrecognized value (a typo must
// never silently keep a stale kernel alive across turns). Case/space tolerant.
func normalizePythonREPLMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", pythonREPLModePerTurn:
		return pythonREPLModePerTurn
	case pythonREPLModePersistent:
		return pythonREPLModePersistent
	default:
		log.Printf("Warning: FLEET_PYTHON_REPL_MODE=%q is not recognized (want %q or %q); falling back to %q",
			raw, pythonREPLModePerTurn, pythonREPLModePersistent, pythonREPLModePerTurn)
		return pythonREPLModePerTurn
	}
}

// The typed numeric/bool/duration readers live in knobs.go (#1119): Load
// resolves them through loadParser + the envKnobs registry so a
// set-but-malformed value refuses to boot instead of silently defaulting.

// normalizeBudgetFraction clamps the sub-agent budget fraction (#264) into the
// valid (0, 1] range, falling back to the package default on a nonsensical value
// so a misconfiguration can never mean "unbounded" (0 or negative → default;
// >1 → 1.0, the whole remaining budget). The fraction caps each child's slice of
// the parent's remaining budget; the parent ceiling remains the hard wall.
func normalizeBudgetFraction(f float64) float64 {
	if f <= 0 {
		return defaultSubagentsBudgetFraction
	}
	if f > 1 {
		return 1.0
	}
	return f
}
