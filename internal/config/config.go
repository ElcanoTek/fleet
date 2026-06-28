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
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
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

// DefaultTitleModel is the fallback for FLEET_TITLE_MODEL / CHAT_TITLE_MODEL:
// the "default" product tier. Mirrors the frontend's DEFAULT_MODEL.
const DefaultTitleModel = "google/gemini-3.5-flash"

// DefaultFromEmail is the fallback From address for outgoing mail. Neutral by
// default; a deployment overrides via SENDGRID_FROM_EMAIL / MAILBUX_FROM_EMAIL.
const DefaultFromEmail = "noreply@example.com"

// allowedEnvVars is the union allowlist of keys that may be set from a .env
// file. Anything else in the file is ignored. The process environment wins
// over the file so operators can override individual values per invocation.
var allowedEnvVars = map[string]bool{
	// ── chat transport / data ──
	"CHAT_SERVER_ADDR":              true,
	"CHAT_SERVER_TOKEN":             true,
	"CHAT_DATA_DIR":                 true,
	"CONVERSATION_TTL_DAYS":         true,
	"CONVERSATION_UNPINNED_CAP":     true,
	"FLEET_AUTO_ARCHIVE_AFTER_DAYS": true,

	// ── fleet transport / data (canonical) ──
	"FLEET_SERVER_ADDR":  true,
	"FLEET_SERVER_TOKEN": true,
	"FLEET_DATA_DIR":     true,

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
	"OPENROUTER_API_KEY":           true,
	"OPENROUTER_BASE_URL":          true,
	"FLEET_OPENROUTER_BASE_URL":    true,
	"CHAT_MAX_ITERATIONS":          true,
	"CHAT_MAX_COST_USD":            true,
	"CHAT_MAX_TOTAL_TOKENS":        true,
	"CHAT_TURN_TIMEOUT_SECONDS":    true,
	"CHAT_TEMPERATURE":             true,
	"CHAT_TITLE_MODEL":             true,
	"FLEET_MAX_ITERATIONS":         true,
	"FLEET_MAX_COST_USD":           true,
	"FLEET_MAX_TOTAL_TOKENS":       true,
	"FLEET_TEMPERATURE":            true,
	"FLEET_TITLE_MODEL":            true,
	"FLEET_MAX_CONCURRENT_AGENTS":  true,
	"FLEET_RUN_LOG_RETENTION_DAYS": true,
	"FLEET_KEEP_RUNS_PER_TASK":     true,
	"FLEET_CLEANUP_HOUR":           true,
	"LLM_MAX_TOKENS":               true,
	"REASONING_ENABLED":            true,
	"REASONING_EFFORT":             true,
	"FLEET_MAX_TOOL_OUTPUT_BYTES":  true,

	// ── personas / protocols ──
	"PERSONA_DEFAULT": true,
	"PERSONA":         true,
	"SYSTEM_PROMPT":   true,

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
	"CUTLASS_TASK_MODEL":          true,
	"CUTLASS_TASK_FALLBACK_MODEL": true,
	"CUTLASS_TASK_MAX_ITERATIONS": true,
	"CUTLASS_INPUT_DIR":           true,
	"CUTLASS_INPUT_FILES":         true,
	"CUTLASS_ALLOWED_DIRS":        true,
	"GH_TOKEN":                    true,

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

	// ── admin ──
	"ADMIN_EMAILS": true,

	// ── sandbox ──
	"CHAT_SANDBOX_IMAGE":           true,
	"CHAT_SANDBOX_RUNTIME":         true,
	"CHAT_WORKSPACE_ROOT":          true,
	"FLEET_SANDBOX_IMAGE":          true,
	"FLEET_SANDBOX_RUNTIME":        true,
	"FLEET_SANDBOX_MEMORY":         true,
	"FLEET_SANDBOX_CPUS":           true,
	"FLEET_SANDBOX_PIDS":           true,
	"FLEET_SANDBOX_DISK_GB":        true,
	"FLEET_SANDBOX_WARM_SIZE":      true,
	"FLEET_SANDBOX_WARM_TTL":       true,
	"FLEET_WORKSPACE_ROOT":         true,
	"CHAT_LOCKDOWN_ONLY":           true,
	"CHAT_LOCKDOWN_ALLOWED_MODELS": true,

	// ── test harness ──
	"CHAT_MOCK_MODE":  true,
	"FLEET_MOCK_MODE": true,
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

// Config holds the union runtime configuration for fleet (interactive +
// scheduled). Interactive-only fields are inert for scheduled runs and vice
// versa.
type Config struct {
	// ── transport (interactive) ──
	Addr        string
	SharedToken string

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
	// AutoArchiveAfterDays soft-archives unpinned conversations untouched for
	// this many days (#282). 0 (the default) disables it — a conversation is
	// then only ever archived by an explicit user action. FLEET_AUTO_ARCHIVE_AFTER_DAYS.
	AutoArchiveAfterDays int

	// SearchEnabled gates full-text search (#308): the GET /search endpoint plus
	// the message-content index maintenance + startup backfill.
	// FLEET_SEARCH_ENABLED, default true; set false to drop the GIN index upkeep
	// on a write-heavy deployment (the endpoint then returns 404).
	SearchEnabled bool

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
	OpenRouterAPIKey   string
	MaxIterations      int
	MaxCostUSD         float64
	MaxTotalTokens     int
	TurnTimeoutSeconds int
	Temperature        float64
	LLMMaxTokens       int
	ReasoningEnabled   bool
	ReasoningEffort    string
	TitleModel         string

	// MaxConcurrentAgents is the configured concurrency cap (FLEET_MAX_CONCURRENT_AGENTS,
	// default 8). It bounds simultaneous SCHEDULED tasks (the worker-pool semaphore)
	// and sizes the sandbox warm pool; interactive chat turns are NOT gated by it —
	// each takes a sandbox on demand, bounded only by host resources. 0 means the
	// default applied by the runner.
	MaxConcurrentAgents int

	// ── personas ──
	PersonaDefault string

	// ── scheduled task config (cutlass) ──
	TaskModel         string
	TaskFallbackModel string
	TaskMaxIterations int
	LLMTemperature    float64
	SystemPrompt      string
	Persona           string

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

	// ── run-history retention (#252) ──
	// RunLogRetentionDays prunes terminal task runs (and their logs) older than
	// this many days in a daily sweep. <=0 disables pruning. Default 90.
	RunLogRetentionDays int
	// KeepRunsPerTask is the minimum number of most-recent terminal runs kept per
	// task regardless of age, so a task's last-known state is never pruned. Default 10.
	KeepRunsPerTask int
	// CleanupHour is the UTC hour (0–23) the daily retention sweep runs. Default 4.
	CleanupHour int

	InputDir   string
	InputFiles []string

	// MCPServers is the runtime MCP server catalog. It is sourced from the
	// client bundle's manifest (internal/clientconfig) — cmd/fleet builds it via
	// Bundle.MCPServerConfigs() and assigns it here — NOT loaded from typed
	// credential fields. fleet itself ships no client connectors; the generic
	// default bundle's catalog is empty. Nil/empty means "no MCP servers".
	MCPServers map[string]MCPServerConfig

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

	// ── web search ──
	TavilyAPIKey string

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
	// Per-container cgroup caps (empty/0 → sandbox defaults: 512m / 1.0 / 128).
	// Operators size these to the host the docs told them to provision.
	SandboxMemory string
	SandboxCPUs   string
	SandboxPids   int
	// SandboxDiskGB caps each sandbox's writable disk usage, in GiB
	// (FLEET_SANDBOX_DISK_GB). 0 → sandbox default (5); negative disables the
	// quota. Stops an agent from filling the host disk (#216).
	SandboxDiskGB int
	// SandboxWarmSize overrides the warm-pool depth (FLEET_SANDBOX_WARM_SIZE).
	// 0 (default) derives it from MaxConcurrentAgents (clamped 2..8); a positive
	// value pins the depth explicitly (#181).
	SandboxWarmSize int
	// SandboxWarmTTLSeconds bounds how long a warm container may sit idle before
	// it is reaped and replaced (FLEET_SANDBOX_WARM_TTL, default 300). 0 disables
	// TTL reaping (#181).
	SandboxWarmTTLSeconds int
	WorkspaceRoot         string
	LockdownOnly          bool
	LockdownAllowedModels []string

	// MockMode short-circuits LLM calls (e2e). FLEET_MOCK_MODE / CHAT_MOCK_MODE.
	MockMode bool
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
}

// Load reads environment variables in this precedence order (highest wins):
//
//  1. Process environment (snapshotted and restored around file loads).
//  2. envFile (e.g. .env.local) — per-operator overrides.
//
// Missing files are NOT an error.
func Load(envFile string) (*Config, error) {
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
		DataDir:              getenvFleetDefault("DATA_DIR", "./data"),
		ConversationTTL:      getenvInt("CONVERSATION_TTL_DAYS", 14),
		UnpinnedCap:          getenvInt("CONVERSATION_UNPINNED_CAP", 50),
		AutoArchiveAfterDays: getenvFleetInt("AUTO_ARCHIVE_AFTER_DAYS", 0),
		SearchEnabled:        getenvBool("FLEET_SEARCH_ENABLED", true),
		DatabaseURL:          buildDatabaseURL(),

		// DB connection pools (#276) — defaults reproduce the historical
		// hard-coded behavior exactly: 25 open / 5 idle, idle-time reaping OFF
		// (0 = unlimited, matching the prior code which never called
		// SetConnMaxIdleTime), 5m lifetime; chat pings at 5s, sched at 10s.
		ChatDBPool: DBPoolConfig{
			MaxOpenConns:    getenvFleetInt("CHAT_DB_MAX_CONNS", 25),
			MaxIdleConns:    getenvFleetInt("CHAT_DB_MIN_CONNS", 5),
			ConnMaxIdleTime: getenvFleetDuration("CHAT_DB_MAX_CONN_IDLE_TIME", 0),
			ConnMaxLifetime: getenvFleetDuration("CHAT_DB_MAX_CONN_LIFETIME", 5*time.Minute),
			ConnectTimeout:  getenvFleetDuration("CHAT_DB_CONNECT_TIMEOUT", 5*time.Second),
		},
		SchedDBPool: DBPoolConfig{
			MaxOpenConns:    getenvFleetInt("SCHED_DB_MAX_CONNS", 25),
			MaxIdleConns:    getenvFleetInt("SCHED_DB_MIN_CONNS", 5),
			ConnMaxIdleTime: getenvFleetDuration("SCHED_DB_MAX_CONN_IDLE_TIME", 0),
			ConnMaxLifetime: getenvFleetDuration("SCHED_DB_MAX_CONN_LIFETIME", 5*time.Minute),
			ConnectTimeout:  getenvFleetDuration("SCHED_DB_CONNECT_TIMEOUT", 10*time.Second),
		},

		// ── LLM (shared) ──
		OpenRouterAPIKey: stripQuotes(os.Getenv("OPENROUTER_API_KEY")),
		MaxIterations:    getenvFleetInt("MAX_ITERATIONS", 300),
		MaxCostUSD:       getenvFleetFloat("MAX_COST_USD", 50.0),
		MaxTotalTokens:   getenvFleetInt("MAX_TOTAL_TOKENS", 10000000),

		ShutdownGraceSeconds: getenvFleetInt("SHUTDOWN_GRACE_SECONDS", 30),

		TurnTimeoutSeconds:  getenvFleetInt("TURN_TIMEOUT_SECONDS", 1800),
		Temperature:         getenvFleetFloat("TEMPERATURE", 0.3),
		LLMMaxTokens:        getenvInt("LLM_MAX_TOKENS", 16384),
		ReasoningEnabled:    getenvBool("REASONING_ENABLED", true),
		ReasoningEffort:     getenvDefault("REASONING_EFFORT", "medium"),
		TitleModel:          getenvFleetDefault("TITLE_MODEL", DefaultTitleModel),
		MaxConcurrentAgents: getenvFleetInt("MAX_CONCURRENT_AGENTS", 8),

		// ── personas ──
		PersonaDefault: getenvDefault("PERSONA_DEFAULT", "assistant"),

		// ── scheduled task (cutlass) ──
		TaskModel:         stripQuotes(os.Getenv("CUTLASS_TASK_MODEL")),
		TaskFallbackModel: stripQuotes(os.Getenv("CUTLASS_TASK_FALLBACK_MODEL")),
		TaskMaxIterations: getEnvOrDefaultInt("CUTLASS_TASK_MAX_ITERATIONS", 0),
		LLMTemperature:    getEnvOrDefaultFloat("CUTLASS_TEMPERATURE", 0.3),

		// ── phone a friend: super-LLM review (#175) ──
		PhoneAFriendEnabled: getenvFleetBool("PHONE_A_FRIEND_ENABLED", false),
		PhoneAFriendModel:   getenvFleet("PHONE_A_FRIEND_MODEL"),

		// ── run-history retention (#252) ──
		RunLogRetentionDays: getenvFleetInt("RUN_LOG_RETENTION_DAYS", 90),
		KeepRunsPerTask:     getenvFleetInt("KEEP_RUNS_PER_TASK", 10),
		CleanupHour:         getenvFleetInt("CLEANUP_HOUR", 4),

		// ── attachments / uploads (generic infra) ──
		EmailAttachmentDir: getenvDefault("EMAIL_ATTACHMENT_DIR", "./data/attachments"),

		// ── web search ──
		TavilyAPIKey: stripQuotes(os.Getenv("TAVILY_API_KEY")),

		// ── rate limit (interactive) ──
		RatePerMinute:       getenvInt("CHAT_RATE_PER_MIN", 40),
		RatePerDay:          getenvInt("CHAT_RATE_PER_DAY", 2000),
		RateLimitEnabled:    getenvBool("FLEET_CHAT_RATE_LIMIT_ENABLED", true),
		RateLimitConcurrent: getenvInt("FLEET_CHAT_RATE_LIMIT_CONCURRENT", 5),

		// ── admin ──
		AdminEmails: splitEmails(os.Getenv("ADMIN_EMAILS")),

		// ── sandbox ──
		SandboxImage:          getenvFleet("SANDBOX_IMAGE"),
		SandboxRuntime:        getenvFleet("SANDBOX_RUNTIME"),
		SandboxMemory:         getenvFleet("SANDBOX_MEMORY"),
		SandboxCPUs:           getenvFleet("SANDBOX_CPUS"),
		SandboxPids:           getEnvOrDefaultInt("FLEET_SANDBOX_PIDS", 0),
		SandboxDiskGB:         getEnvOrDefaultInt("FLEET_SANDBOX_DISK_GB", 0),
		SandboxWarmSize:       getenvFleetInt("SANDBOX_WARM_SIZE", 0),
		SandboxWarmTTLSeconds: getenvFleetInt("SANDBOX_WARM_TTL", 300),
		WorkspaceRoot:         getenvFleet("WORKSPACE_ROOT"),
		LockdownOnly:          getenvBool("CHAT_LOCKDOWN_ONLY", false),
		LockdownAllowedModels: splitLockdownModels(os.Getenv("CHAT_LOCKDOWN_ALLOWED_MODELS")),
		MockMode:              getenvFleetBool("MOCK_MODE", false),
	}

	// ── personas / prompts (cutlass file-name normalization) ──
	// Defaults are the generic bundle's names; a client bundle sets PERSONA /
	// SYSTEM_PROMPT (and PERSONA_DEFAULT) to its own.
	cfg.SystemPrompt = getEnvOrDefault("SYSTEM_PROMPT", "default.md")
	if !hasKnownPromptExtension(cfg.SystemPrompt) {
		cfg.SystemPrompt += ".md"
	}
	cfg.Persona = getEnvOrDefault("PERSONA", "personas/assistant.yaml")
	if !hasKnownPromptExtension(cfg.Persona) {
		cfg.Persona += ".yaml"
	}

	// ── scheduled-mode input files (cutlass) ──
	cfg.InputDir = stripQuotes(os.Getenv("CUTLASS_INPUT_DIR"))
	if inputFiles := stripQuotes(os.Getenv("CUTLASS_INPUT_FILES")); inputFiles != "" {
		cfg.InputFiles = strings.Split(inputFiles, ",")
	}

	// The MCP server catalog is NOT built here — it is sourced from the client
	// bundle's manifest (internal/clientconfig) and assigned to cfg.MCPServers by
	// cmd/fleet at startup. Leave it as the empty map initialized above.

	// Lockdown is a no-op without an image. Surface the misconfiguration loudly.
	if cfg.LockdownOnly && cfg.SandboxImage == "" {
		fmt.Fprintln(os.Stderr, "warn: CHAT_LOCKDOWN_ONLY=true but sandbox image is unset; cannot enforce — treating as disabled")
		cfg.LockdownOnly = false
	}
	return cfg, nil
}

// LockdownAvailable reports whether the lockdown affordance should be exposed.
func (c *Config) LockdownAvailable() bool {
	return c.SandboxImage != ""
}

// LockdownAllows reports whether the slug is on the lockdown allow-list.
func (c *Config) LockdownAllows(slug string) bool {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return false
	}
	for _, allowed := range c.LockdownAllowedModels {
		if allowed == slug {
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
			"google/gemini-3.5-flash",   // default
			"anthropic/claude-opus-4.8", // advanced
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

// loadEnvFile parses a KEY=VALUE env file, respecting the allowlist. It strips
// inline comments off unquoted values (cutlass behavior) and a single layer of
// surrounding quotes. Always overwrites — Load snapshots/restores the
// pre-existing process env around this call so "process env wins" holds.
func loadEnvFile(path string) error {
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
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
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
// Quoted values are left intact (quote handling owns those).
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

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
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

func getEnvOrDefaultInt(key string, defaultValue int) int {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		// strconv (not Sscanf) so trailing garbage like "12abc" is REJECTED and
		// falls back to the default, rather than being silently parsed as 12.
		if result, err := strconv.Atoi(value); err == nil {
			return result
		}
	}
	return defaultValue
}

func getEnvOrDefaultFloat(key string, defaultValue float64) float64 {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if result, err := strconv.ParseFloat(value, 64); err == nil {
			return result
		}
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
		return v
	}
	return def
}

func getenvFleetInt(suffix string, def int) int {
	if v, ok := lookupFleet(suffix); ok {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return def
}

// getenvFleetDuration reads FLEET_<suffix> (with CHAT_/CUTLASS_ back-compat) as
// a Go duration string (e.g. "5m", "10s"). Falls back to def on absence or parse
// error. Used for the DB-pool timeout knobs (#276).
func getenvFleetDuration(suffix string, def time.Duration) time.Duration {
	if v, ok := lookupFleet(suffix); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

func getenvFleetFloat(suffix string, def float64) float64 {
	if v, ok := lookupFleet(suffix); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func getenvFleetBool(suffix string, def bool) bool {
	if v, ok := lookupFleet(suffix); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}
