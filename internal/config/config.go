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
)

// Canonical + legacy env prefixes. FLEET_ wins; the two legacy prefixes are
// tried in order for back-compat. The two front-ends never set the same suffix
// to conflicting values, so order between the legacy prefixes is irrelevant.
const (
	canonicalPrefix = "FLEET_"
)

var legacyPrefixes = []string{"CHAT_", "CUTLASS_"}

// Environment-variable names shared by the allowlist, the loader, and the
// per-server env builders. Defined as constants so a typo in one place can't
// silently disable a server.
const (
	envSendGridAPIKey                    = "SENDGRID_API_KEY"    //nolint:gosec // G101: env var name, not a credential value
	envSendGridFromEmail                 = "SENDGRID_FROM_EMAIL" //nolint:gosec // G101: env var name, not a credential value
	envOpenXAPIKey                       = "OPENX_API_KEY"       //nolint:gosec // G101: env var name, not a credential value
	envPubMaticOwnerID                   = "PUBMATIC_OWNER_ID"
	envIndexExchangeBaseURL              = "INDEXEXCHANGE_BASE_URL"
	envIndexExchangeMarketplaceAccountID = "INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID"
	envEmailS3Bucket                     = "EMAIL_S3_BUCKET"
	envEmailS3Prefix                     = "EMAIL_S3_PREFIX"
)

// MCP server transport / command literals.
const (
	mcpServerTypeStdio = "stdio"
	mcpServerTypeHTTP  = "http"
	mcpCommandPython3  = "python3"
)

// DefaultTitleModel is the fallback for FLEET_TITLE_MODEL / CHAT_TITLE_MODEL:
// the "default" product tier. Mirrors the frontend's DEFAULT_MODEL.
const DefaultTitleModel = "google/gemini-3.5-flash"

// DefaultFromEmail is the fallback From address for outgoing mail.
const DefaultFromEmail = "victoria@elcanotek.com"

// allowedEnvVars is the union allowlist of keys that may be set from a .env
// file. Anything else in the file is ignored. The process environment wins
// over the file so operators can override individual values per invocation.
var allowedEnvVars = map[string]bool{
	// ── chat transport / data ──
	"CHAT_SERVER_ADDR":          true,
	"CHAT_SERVER_TOKEN":         true,
	"CHAT_DATA_DIR":             true,
	"CONVERSATION_TTL_DAYS":     true,
	"CONVERSATION_UNPINNED_CAP": true,

	// ── fleet transport / data (canonical) ──
	"FLEET_SERVER_ADDR":  true,
	"FLEET_SERVER_TOKEN": true,
	"FLEET_DATA_DIR":     true,

	// ── database (chat) ──
	"DATABASE_URL": true,
	"DB_HOST":      true,
	"DB_PORT":      true,
	"DB_USER":      true,
	"DB_PASSWORD":  true,
	"DB_NAME":      true,
	"DB_SSLMODE":   true,

	// ── LLM (shared) ──
	"OPENROUTER_API_KEY":          true,
	"CHAT_MAX_ITERATIONS":         true,
	"CHAT_MAX_COST_USD":           true,
	"CHAT_MAX_TOTAL_TOKENS":       true,
	"CHAT_TURN_TIMEOUT_SECONDS":   true,
	"CHAT_TEMPERATURE":            true,
	"CHAT_TITLE_MODEL":            true,
	"FLEET_MAX_ITERATIONS":        true,
	"FLEET_MAX_COST_USD":          true,
	"FLEET_MAX_TOTAL_TOKENS":      true,
	"FLEET_TEMPERATURE":           true,
	"FLEET_TITLE_MODEL":           true,
	"FLEET_MAX_CONCURRENT_AGENTS": true,
	"LLM_MAX_TOKENS":              true,
	"REASONING_ENABLED":           true,
	"REASONING_EFFORT":            true,

	// ── personas / protocols ──
	"PERSONA_DEFAULT": true,
	"PERSONA":         true,
	"SYSTEM_PROMPT":   true,

	// ── cutlass agent knobs ──
	"MAX_ITERATIONS":           true,
	"CUTLASS_TEMPERATURE":      true,
	"CUTLASS_MAX_COST_USD":     true,
	"CUTLASS_MAX_TOTAL_TOKENS": true,
	"DEAL_SHEET_OUTPUT_DIR":    true,

	// ── cutlass image gen ──
	"CUTLASS_IMAGE_OUTPUT":    true,
	"CUTLASS_IMAGE_MODEL":     true,
	"OPENROUTER_HTTP_REFERER": true,
	"OPENROUTER_X_TITLE":      true,

	// ── cutlass task input (set by runner) ──
	"CUTLASS_TASK_MODEL":                   true,
	"CUTLASS_TASK_FALLBACK_MODEL":          true,
	"CUTLASS_TASK_MAX_ITERATIONS":          true,
	"CUTLASS_INPUT_DIR":                    true,
	"CUTLASS_INPUT_FILES":                  true,
	"CUTLASS_ALLOWED_DIRS":                 true,
	"CUTLASS_CAPTAINS_LOG_ROOT":            true,
	"CUTLASS_CAPTAINS_LOG_URL":             true,
	"CUTLASS_CAPTAINS_LOG_BRANCH":          true,
	"CUTLASS_INSTRUCTION_REPO_ROOT":        true,
	"CUTLASS_INSTRUCTION_REPO_BASE_BRANCH": true,
	"CUTLASS_GIT_AUTHOR_NAME":              true,
	"CUTLASS_GIT_AUTHOR_EMAIL":             true,
	"GH_TOKEN":                             true,

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

	// ── fast.io ──
	"FAST_IO_MCP_TOKEN": true,

	// ── gamma (chat-only) ──
	"GAMMA_API_KEY": true,

	// ── OpenX (cutlass) ──
	envOpenXAPIKey: true,

	// ── PubMatic ──
	"PUBMATIC_API_KEY":             true,
	"PUBMATIC_BASE_URL":            true,
	"PUBMATIC_MCP_BASE_URL":        true,
	"PUBMATIC_USERNAME":            true,
	"PUBMATIC_PASSWORD":            true,
	"PUBMATIC_API_PRODUCT":         true,
	"PUBMATIC_ACCESS_TOKEN":        true,
	"PUBMATIC_DSP_ID":              true,
	"PUBMATIC_BUYER_ID":            true,
	"PUBMATIC_SEAT_ID":             true,
	"PUBMATIC_TARGETING_ID":        true,
	envPubMaticOwnerID:             true,
	"PUBMATIC_REPORT_DOWNLOAD_DIR": true,

	// ── Media.net ──
	"MEDIANET_SELECT_BASE_URL":     true,
	"MEDIANET_SELECT_EMAIL":        true,
	"MEDIANET_SELECT_PASSWORD":     true,
	"MEDIANET_SELECT_TOKEN":        true,
	"MEDIANET_REPORT_BASE_URL":     true,
	"MEDIANET_REPORT_TOKEN":        true,
	"MEDIANET_REPORT_DOWNLOAD_DIR": true,

	// ── Index Exchange ──
	"INDEXEXCHANGE_USERNAME":             true,
	"INDEXEXCHANGE_PASSWORD":             true,
	"INDEXEXCHANGE_SERVICE_ID":           true,
	"INDEXEXCHANGE_SERVICE_SECRET":       true,
	envIndexExchangeBaseURL:              true,
	"INDEXEXCHANGE_TIMEOUT_SECONDS":      true,
	"INDEXEXCHANGE_DOWNLOAD_DIR":         true,
	envIndexExchangeMarketplaceAccountID: true,

	// ── Magnite ──
	"MAGNITE_BASE_URL":     true,
	"MAGNITE_ACCESS_KEY":   true,
	"MAGNITE_SECRET_KEY":   true,
	"MAGNITE_SEAT_ID":      true,
	"MAGNITE_ACCOUNT_ID":   true,
	"MAGNITE_DV_BASE_URL":  true,
	"MAGNITE_DMG_BASE_URL": true,
	"MAGNITE_DOWNLOAD_DIR": true,

	// ── Xandr ──
	"XANDR_BASE_URL":            true,
	"XANDR_USERNAME":            true,
	"XANDR_PASSWORD":            true,
	"XANDR_SEAT_ID":             true,
	"XANDR_REPORT_DOWNLOAD_DIR": true,

	// ── TripleLift ──
	"TRIPLELIFT_CLIENT_ID":           true,
	"TRIPLELIFT_CLIENT_SECRET":       true,
	"TRIPLELIFT_MEMBER_ID":           true,
	"TRIPLELIFT_TOKEN_URL":           true,
	"TRIPLELIFT_BASE_URL":            true,
	"TRIPLELIFT_AUDIENCE":            true,
	"TRIPLELIFT_ORGANIZATION":        true,
	"TRIPLELIFT_SCOPE":               true,
	"TRIPLELIFT_REPORTING_BASE_URL":  true,
	"TRIPLELIFT_REPORT_DOWNLOAD_DIR": true,

	// ── rate limiting (chat) ──
	"CHAT_RATE_PER_MIN": true,
	"CHAT_RATE_PER_DAY": true,

	// ── admin ──
	"ADMIN_EMAILS": true,

	// ── sandbox ──
	"CHAT_SANDBOX_IMAGE":           true,
	"CHAT_SANDBOX_RUNTIME":         true,
	"CHAT_WORKSPACE_ROOT":          true,
	"FLEET_SANDBOX_IMAGE":          true,
	"FLEET_SANDBOX_RUNTIME":        true,
	"FLEET_WORKSPACE_ROOT":         true,
	"CHAT_LOCKDOWN_ONLY":           true,
	"CHAT_LOCKDOWN_ALLOWED_MODELS": true,

	// ── test harness ──
	"CHAT_MOCK_MODE":  true,
	"FLEET_MOCK_MODE": true,
}

// allowedEnvPrefixes admits open-ended user/account suffixes the operator names
// at provisioning time. A key matching one of these is treated like an exact
// allowlist entry. Currently:
//   - GAMMA_API_KEY_<NAME>: chat's per-user Gamma keys.
var allowedEnvPrefixes = []string{
	"GAMMA_API_KEY_",
}

// isAllowedEnvVar returns true when k may flow from a .env file into the
// process environment. A key is allowed when:
//
//  1. It is literally present in allowedEnvVars, OR
//  2. It matches a prefix in allowedEnvPrefixes (chat's open-ended Gamma keys), OR
//  3. It matches "<BASE>_<UPPERCASE_ALPHANUMERIC_SUFFIX>" where BASE is an
//     allowlisted key (cutlass's per-account credential-variant suffixing,
//     e.g. PUBMATIC_OWNER_ID_REKLAIM). The suffix MUST be uppercase
//     alphanumeric so LD_PRELOAD / PATH_PRELOAD style keys cannot match.
func isAllowedEnvVar(k string) bool {
	if allowedEnvVars[k] {
		return true
	}
	for _, p := range allowedEnvPrefixes {
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
	if !allowedEnvVars[base] {
		return false
	}
	for _, r := range suffix {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Config holds the union runtime configuration for fleet (interactive +
// scheduled). Interactive-only fields are inert for scheduled runs and vice
// versa.
type Config struct {
	// ── transport (interactive) ──
	Addr        string
	SharedToken string

	// ── data (interactive) ──
	DataDir         string
	ConversationTTL int // days
	UnpinnedCap     int // per-user

	// DatabaseURL is the Postgres DSN. URL-form (url.UserPassword) DSN builder.
	DatabaseURL string

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

	// MaxConcurrentAgents bounds simultaneous agents across both modes (plan
	// §6.4). 0 means the default (4) applied by the runner.
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

	InputDir                  string
	InputFiles                []string
	CaptainsLogEnabled        bool
	CaptainsLogURL            string
	InstructionRepoRoot       string
	InstructionRepoBaseBranch string
	CaptainsLogGitAuthorName  string
	CaptainsLogGitAuthorEmail string

	// MCPServers is the scheduled-mode credential-gated server catalog
	// (cutlass). configureMCPServers populates it from the loaded credentials.
	MCPServers map[string]MCPServerConfig

	// ── MCP: email ──
	AWSAccessKeyID           string
	AWSSecretAccessKey       string
	AWSRegion                string
	EmailS3Bucket            string
	EmailS3Prefix            string
	EmailS3DatePrefixFormat  string
	EmailS3MaxDatePrefixDays string
	EmailAttachmentDir       string
	EmailLastCheckFile       string
	SendGridAPIKey           string
	SendGridFromEmail        string

	// ── MCP: mailbux (chat-only) ──
	MailbuxUsername           string
	MailbuxPassword           string
	MailbuxFromEmail          string
	MailbuxJMAPBaseURL        string
	MailbuxSMTPHost           string
	MailbuxSMTPPort           string
	MailbuxDownloadDir        string
	MailbuxJMAPTimeoutSeconds string
	MailbuxQueryPageLimit     string
	MailbuxSearchMaxScan      string

	// ── web search ──
	TavilyAPIKey string

	// ── fast.io ──
	FastIOMCPToken string

	// ── gamma (chat-only) ──
	GammaAPIKey string

	// ── OpenX (cutlass) ──
	OpenXAPIKey string

	// ── deal sheet (cutlass) ──
	DealSheetOutputDir string

	// ── PubMatic ──
	PubMaticAPIKey            string
	PubMaticBaseURL           string
	PubMaticMCPBaseURL        string
	PubMaticUsername          string
	PubMaticPassword          string
	PubMaticAPIProduct        string
	PubMaticAccessToken       string
	PubMaticDSPID             string
	PubMaticBuyerID           string
	PubMaticSeatID            string
	PubMaticTargetingID       string
	PubMaticOwnerID           string
	PubMaticReportDownloadDir string

	// ── Index Exchange ──
	IndexExchangeUsername             string
	IndexExchangePassword             string
	IndexExchangeServiceID            string
	IndexExchangeServiceSecret        string
	IndexExchangeBaseURL              string
	IndexExchangeTimeoutSeconds       string
	IndexExchangeDownloadDir          string
	IndexExchangeMarketplaceAccountID string

	// ── Magnite ──
	MagniteBaseURL     string
	MagniteAccessKey   string
	MagniteSecretKey   string
	MagniteSeatID      string
	MagniteAccountID   string
	MagniteDVBaseURL   string
	MagniteDMGBaseURL  string
	MagniteDownloadDir string

	// ── Xandr ──
	XandrBaseURL           string
	XandrUsername          string
	XandrPassword          string
	XandrSeatID            string
	XandrReportDownloadDir string

	// ── Media.net ──
	MediaNetSelectBaseURL     string
	MediaNetSelectEmail       string
	MediaNetSelectPassword    string
	MediaNetSelectToken       string
	MediaNetReportBaseURL     string
	MediaNetReportToken       string
	MediaNetReportDownloadDir string

	// ── TripleLift ──
	TripleLiftClientID          string
	TripleLiftClientSecret      string
	TripleLiftMemberID          string
	TripleLiftTokenURL          string
	TripleLiftBaseURL           string
	TripleLiftAudience          string
	TripleLiftOrganization      string
	TripleLiftScope             string
	TripleLiftReportingBaseURL  string
	TripleLiftReportDownloadDir string

	// ── rate limit (interactive) ──
	RatePerMinute int
	RatePerDay    int

	// ── admin (interactive) ──
	AdminEmails []string

	// ── sandbox ──
	SandboxImage          string
	SandboxRuntime        string
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

		// ── data (interactive) ──
		DataDir:         getenvFleetDefault("DATA_DIR", "./data"),
		ConversationTTL: getenvInt("CONVERSATION_TTL_DAYS", 14),
		UnpinnedCap:     getenvInt("CONVERSATION_UNPINNED_CAP", 50),
		DatabaseURL:     buildDatabaseURL(),

		// ── LLM (shared) ──
		OpenRouterAPIKey: stripQuotes(os.Getenv("OPENROUTER_API_KEY")),
		MaxIterations:    getenvFleetInt("MAX_ITERATIONS", 300),
		MaxCostUSD:       getenvFleetFloat("MAX_COST_USD", 50.0),
		MaxTotalTokens:   getenvFleetInt("MAX_TOTAL_TOKENS", 10000000),

		TurnTimeoutSeconds:  getenvFleetInt("TURN_TIMEOUT_SECONDS", 1800),
		Temperature:         getenvFleetFloat("TEMPERATURE", 0.3),
		LLMMaxTokens:        getenvInt("LLM_MAX_TOKENS", 16384),
		ReasoningEnabled:    getenvBool("REASONING_ENABLED", true),
		ReasoningEffort:     getenvDefault("REASONING_EFFORT", "medium"),
		TitleModel:          getenvFleetDefault("TITLE_MODEL", DefaultTitleModel),
		MaxConcurrentAgents: getenvFleetInt("MAX_CONCURRENT_AGENTS", 4),

		// ── personas ──
		PersonaDefault: getenvDefault("PERSONA_DEFAULT", "victoria"),

		// ── scheduled task (cutlass) ──
		TaskModel:         stripQuotes(os.Getenv("CUTLASS_TASK_MODEL")),
		TaskFallbackModel: stripQuotes(os.Getenv("CUTLASS_TASK_FALLBACK_MODEL")),
		TaskMaxIterations: getEnvOrDefaultInt("CUTLASS_TASK_MAX_ITERATIONS", 0),
		LLMTemperature:    getEnvOrDefaultFloat("CUTLASS_TEMPERATURE", 0.3),

		// ── MCP: email ──
		AWSAccessKeyID:           stripQuotes(os.Getenv("AWS_ACCESS_KEY_ID")),
		AWSSecretAccessKey:       stripQuotes(os.Getenv("AWS_SECRET_ACCESS_KEY")),
		AWSRegion:                getEnvOrDefault("AWS_REGION", "us-east-2"),
		EmailS3Bucket:            stripQuotes(os.Getenv(envEmailS3Bucket)),
		EmailS3Prefix:            getEnvOrDefault(envEmailS3Prefix, "emails/"),
		EmailS3DatePrefixFormat:  stripQuotes(os.Getenv("EMAIL_S3_DATE_PREFIX_FORMAT")),
		EmailS3MaxDatePrefixDays: stripQuotes(os.Getenv("EMAIL_S3_MAX_DATE_PREFIX_DAYS")),
		EmailAttachmentDir:       getenvDefault("EMAIL_ATTACHMENT_DIR", "./data/attachments"),
		EmailLastCheckFile:       getenvDefault("EMAIL_LAST_CHECK_FILE", "./data/email_last_checked.txt"),
		SendGridAPIKey:           stripQuotes(os.Getenv(envSendGridAPIKey)),
		SendGridFromEmail:        getEnvOrDefault(envSendGridFromEmail, DefaultFromEmail),

		// ── mailbux (chat-only) ──
		MailbuxUsername:           os.Getenv("MAILBUX_USERNAME"),
		MailbuxPassword:           os.Getenv("MAILBUX_PASSWORD"),
		MailbuxFromEmail:          getEnvOrDefault("MAILBUX_FROM_EMAIL", DefaultFromEmail),
		MailbuxJMAPBaseURL:        os.Getenv("MAILBUX_JMAP_BASE_URL"),
		MailbuxSMTPHost:           os.Getenv("MAILBUX_SMTP_HOST"),
		MailbuxSMTPPort:           os.Getenv("MAILBUX_SMTP_PORT"),
		MailbuxDownloadDir:        getenvDefault("MAILBUX_DOWNLOAD_DIR", "./data/attachments"),
		MailbuxJMAPTimeoutSeconds: os.Getenv("MAILBUX_JMAP_TIMEOUT_SECONDS"),
		MailbuxQueryPageLimit:     os.Getenv("MAILBUX_QUERY_PAGE_LIMIT"),
		MailbuxSearchMaxScan:      os.Getenv("MAILBUX_SEARCH_MAX_SCAN"),

		// ── web search ──
		TavilyAPIKey: stripQuotes(os.Getenv("TAVILY_API_KEY")),

		// ── fast.io ──
		FastIOMCPToken: stripQuotes(os.Getenv("FAST_IO_MCP_TOKEN")),

		// ── gamma (chat-only) ──
		GammaAPIKey: os.Getenv("GAMMA_API_KEY"),

		// ── OpenX (cutlass) ──
		OpenXAPIKey: stripQuotes(os.Getenv(envOpenXAPIKey)),

		// ── deal sheet ──
		DealSheetOutputDir: stripQuotes(os.Getenv("DEAL_SHEET_OUTPUT_DIR")),

		// ── PubMatic ──
		PubMaticAPIKey:            stripQuotes(os.Getenv("PUBMATIC_API_KEY")),
		PubMaticUsername:          stripQuotes(os.Getenv("PUBMATIC_USERNAME")),
		PubMaticPassword:          stripQuotes(os.Getenv("PUBMATIC_PASSWORD")),
		PubMaticAPIProduct:        getEnvOrDefault("PUBMATIC_API_PRODUCT", "PUBLISHER"),
		PubMaticAccessToken:       stripQuotes(os.Getenv("PUBMATIC_ACCESS_TOKEN")),
		PubMaticDSPID:             stripQuotes(os.Getenv("PUBMATIC_DSP_ID")),
		PubMaticBuyerID:           stripQuotes(os.Getenv("PUBMATIC_BUYER_ID")),
		PubMaticSeatID:            stripQuotes(os.Getenv("PUBMATIC_SEAT_ID")),
		PubMaticTargetingID:       stripQuotes(os.Getenv("PUBMATIC_TARGETING_ID")),
		PubMaticOwnerID:           getEnvOrDefault(envPubMaticOwnerID, "60067"),
		PubMaticReportDownloadDir: stripQuotes(os.Getenv("PUBMATIC_REPORT_DOWNLOAD_DIR")),

		// ── Index Exchange ──
		IndexExchangeUsername:             stripQuotes(os.Getenv("INDEXEXCHANGE_USERNAME")),
		IndexExchangePassword:             stripQuotes(os.Getenv("INDEXEXCHANGE_PASSWORD")),
		IndexExchangeServiceID:            stripQuotes(os.Getenv("INDEXEXCHANGE_SERVICE_ID")),
		IndexExchangeServiceSecret:        stripQuotes(os.Getenv("INDEXEXCHANGE_SERVICE_SECRET")),
		IndexExchangeBaseURL:              getEnvOrDefault(envIndexExchangeBaseURL, "https://app.indexexchange.com"),
		IndexExchangeTimeoutSeconds:       getEnvOrDefault("INDEXEXCHANGE_TIMEOUT_SECONDS", ""),
		IndexExchangeDownloadDir:          getEnvOrDefault("INDEXEXCHANGE_DOWNLOAD_DIR", ""),
		IndexExchangeMarketplaceAccountID: getEnvOrDefault(envIndexExchangeMarketplaceAccountID, "1491166"),

		// ── Magnite ──
		MagniteBaseURL:     os.Getenv("MAGNITE_BASE_URL"),
		MagniteAccessKey:   stripQuotes(os.Getenv("MAGNITE_ACCESS_KEY")),
		MagniteSecretKey:   stripQuotes(os.Getenv("MAGNITE_SECRET_KEY")),
		MagniteSeatID:      stripQuotes(os.Getenv("MAGNITE_SEAT_ID")),
		MagniteAccountID:   stripQuotes(os.Getenv("MAGNITE_ACCOUNT_ID")),
		MagniteDVBaseURL:   getEnvOrDefault("MAGNITE_DV_BASE_URL", "https://api.rubiconproject.com"),
		MagniteDMGBaseURL:  getEnvOrDefault("MAGNITE_DMG_BASE_URL", "https://dmg.rubiconproject.com"),
		MagniteDownloadDir: getEnvOrDefault("MAGNITE_DOWNLOAD_DIR", ""),

		// ── Xandr ──
		XandrBaseURL:           os.Getenv("XANDR_BASE_URL"),
		XandrUsername:          stripQuotes(os.Getenv("XANDR_USERNAME")),
		XandrPassword:          stripQuotes(os.Getenv("XANDR_PASSWORD")),
		XandrSeatID:            stripQuotes(os.Getenv("XANDR_SEAT_ID")),
		XandrReportDownloadDir: stripQuotes(os.Getenv("XANDR_REPORT_DOWNLOAD_DIR")),

		// ── Media.net ──
		MediaNetSelectBaseURL:     getEnvOrDefault("MEDIANET_SELECT_BASE_URL", "https://select.media.net"),
		MediaNetSelectEmail:       stripQuotes(os.Getenv("MEDIANET_SELECT_EMAIL")),
		MediaNetSelectPassword:    stripQuotes(os.Getenv("MEDIANET_SELECT_PASSWORD")),
		MediaNetSelectToken:       stripQuotes(os.Getenv("MEDIANET_SELECT_TOKEN")),
		MediaNetReportBaseURL:     getEnvOrDefault("MEDIANET_REPORT_BASE_URL", "https://select-analytics.media.net"),
		MediaNetReportToken:       stripQuotes(os.Getenv("MEDIANET_REPORT_TOKEN")),
		MediaNetReportDownloadDir: stripQuotes(os.Getenv("MEDIANET_REPORT_DOWNLOAD_DIR")),

		// ── TripleLift ──
		TripleLiftClientID:          stripQuotes(os.Getenv("TRIPLELIFT_CLIENT_ID")),
		TripleLiftClientSecret:      stripQuotes(os.Getenv("TRIPLELIFT_CLIENT_SECRET")),
		TripleLiftMemberID:          stripQuotes(os.Getenv("TRIPLELIFT_MEMBER_ID")),
		TripleLiftTokenURL:          getEnvOrDefault("TRIPLELIFT_TOKEN_URL", ""),
		TripleLiftBaseURL:           getEnvOrDefault("TRIPLELIFT_BASE_URL", "https://api.triplelift.net"),
		TripleLiftAudience:          stripQuotes(os.Getenv("TRIPLELIFT_AUDIENCE")),
		TripleLiftOrganization:      stripQuotes(os.Getenv("TRIPLELIFT_ORGANIZATION")),
		TripleLiftScope:             stripQuotes(os.Getenv("TRIPLELIFT_SCOPE")),
		TripleLiftReportingBaseURL:  stripQuotes(os.Getenv("TRIPLELIFT_REPORTING_BASE_URL")),
		TripleLiftReportDownloadDir: stripQuotes(os.Getenv("TRIPLELIFT_REPORT_DOWNLOAD_DIR")),

		// ── rate limit (interactive) ──
		RatePerMinute: getenvInt("CHAT_RATE_PER_MIN", 40),
		RatePerDay:    getenvInt("CHAT_RATE_PER_DAY", 2000),

		// ── admin ──
		AdminEmails: splitEmails(os.Getenv("ADMIN_EMAILS")),

		// ── sandbox ──
		SandboxImage:          getenvFleet("SANDBOX_IMAGE"),
		SandboxRuntime:        getenvFleet("SANDBOX_RUNTIME"),
		WorkspaceRoot:         getenvFleet("WORKSPACE_ROOT"),
		LockdownOnly:          getenvBool("CHAT_LOCKDOWN_ONLY", false),
		LockdownAllowedModels: splitLockdownModels(os.Getenv("CHAT_LOCKDOWN_ALLOWED_MODELS")),
		MockMode:              getenvFleetBool("MOCK_MODE", false),
	}

	// PubMatic base URL: PUBMATIC_BASE_URL, else PUBMATIC_MCP_BASE_URL, else default.
	cfg.PubMaticBaseURL = getEnvOrDefault("PUBMATIC_BASE_URL", "")
	if cfg.PubMaticBaseURL == "" {
		cfg.PubMaticBaseURL = getEnvOrDefault("PUBMATIC_MCP_BASE_URL", "https://api.pubmatic.com")
	}
	cfg.PubMaticMCPBaseURL = cfg.PubMaticBaseURL

	// ── personas / prompts (cutlass file-name normalization) ──
	cfg.SystemPrompt = getEnvOrDefault("SYSTEM_PROMPT", "default.md")
	if !hasKnownPromptExtension(cfg.SystemPrompt) {
		cfg.SystemPrompt += ".md"
	}
	cfg.Persona = getEnvOrDefault("PERSONA", "personas/victoria.yaml")
	if !hasKnownPromptExtension(cfg.Persona) {
		cfg.Persona += ".yaml"
	}

	// ── captain's log / instruction repo (cutlass) ──
	cfg.InputDir = stripQuotes(os.Getenv("CUTLASS_INPUT_DIR"))
	if inputFiles := stripQuotes(os.Getenv("CUTLASS_INPUT_FILES")); inputFiles != "" {
		cfg.InputFiles = strings.Split(inputFiles, ",")
	}
	cfg.CaptainsLogEnabled = strings.TrimSpace(os.Getenv("CUTLASS_CAPTAINS_LOG_ROOT")) != ""
	cfg.CaptainsLogURL = firstNonEmptyEnv("CUTLASS_CAPTAINS_LOG_URL")
	cfg.InstructionRepoRoot = firstNonEmptyEnv("CUTLASS_CAPTAINS_LOG_ROOT", "CUTLASS_INSTRUCTION_REPO_ROOT")
	cfg.InstructionRepoBaseBranch = firstNonEmptyEnvDefault("main", "CUTLASS_CAPTAINS_LOG_BRANCH", "CUTLASS_INSTRUCTION_REPO_BASE_BRANCH")
	cfg.CaptainsLogGitAuthorName = firstNonEmptyEnv("CUTLASS_GIT_AUTHOR_NAME")
	cfg.CaptainsLogGitAuthorEmail = firstNonEmptyEnv("CUTLASS_GIT_AUTHOR_EMAIL")

	// Scheduled-mode credential-gated MCP server catalog.
	cfg.configureMCPServers()

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
	return nil
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

// EmailMCPEnv returns the env-var map the ses_s3_email and sendgrid MCP
// subprocesses expect.
func (c *Config) EmailMCPEnv() map[string]string {
	env := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	add("AWS_ACCESS_KEY_ID", c.AWSAccessKeyID)
	add("AWS_SECRET_ACCESS_KEY", c.AWSSecretAccessKey)
	add("AWS_REGION", c.AWSRegion)
	add("EMAIL_S3_BUCKET", c.EmailS3Bucket)
	add("EMAIL_S3_PREFIX", c.EmailS3Prefix)
	add("EMAIL_S3_DATE_PREFIX_FORMAT", c.EmailS3DatePrefixFormat)
	add("EMAIL_S3_MAX_DATE_PREFIX_DAYS", c.EmailS3MaxDatePrefixDays)
	add("EMAIL_ATTACHMENT_DIR", c.EmailAttachmentDir)
	add("EMAIL_LAST_CHECK_FILE", c.EmailLastCheckFile)
	add("SENDGRID_API_KEY", c.SendGridAPIKey)
	add("SENDGRID_FROM_EMAIL", c.SendGridFromEmail)
	return env
}

// MailbuxMCPEnv returns the env-var map the mailbux MCP subprocess expects.
func (c *Config) MailbuxMCPEnv() map[string]string {
	env := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	add("MAILBUX_USERNAME", c.MailbuxUsername)
	add("MAILBUX_PASSWORD", c.MailbuxPassword)
	add("MAILBUX_FROM_EMAIL", c.MailbuxFromEmail)
	add("MAILBUX_JMAP_BASE_URL", c.MailbuxJMAPBaseURL)
	add("MAILBUX_SMTP_HOST", c.MailbuxSMTPHost)
	add("MAILBUX_SMTP_PORT", c.MailbuxSMTPPort)
	add("MAILBUX_DOWNLOAD_DIR", c.MailbuxDownloadDir)
	add("MAILBUX_JMAP_TIMEOUT_SECONDS", c.MailbuxJMAPTimeoutSeconds)
	add("MAILBUX_QUERY_PAGE_LIMIT", c.MailbuxQueryPageLimit)
	add("MAILBUX_SEARCH_MAX_SCAN", c.MailbuxSearchMaxScan)
	return env
}

// ProviderMCPEnv returns the subset of env vars a provider reporting MCP needs.
// This is the base (default-seat) env; per-account variants are applied by
// creds.ApplyClientSuffix at bind time.
func (c *Config) ProviderMCPEnv(provider string) map[string]string {
	env := map[string]string{}
	add := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}

	switch provider {
	case "magnite", "magnite_mcp":
		add("MAGNITE_BASE_URL", c.MagniteBaseURL)
		add("MAGNITE_ACCESS_KEY", c.MagniteAccessKey)
		add("MAGNITE_SECRET_KEY", c.MagniteSecretKey)
		add("MAGNITE_SEAT_ID", c.MagniteSeatID)
		add("MAGNITE_ACCOUNT_ID", c.MagniteAccountID)
		add("MAGNITE_DV_BASE_URL", c.MagniteDVBaseURL)
		add("MAGNITE_DMG_BASE_URL", c.MagniteDMGBaseURL)
		add("MAGNITE_DOWNLOAD_DIR", c.MagniteDownloadDir)
	case "indexexchange", "indexexchange_mcp":
		add("INDEXEXCHANGE_BASE_URL", c.IndexExchangeBaseURL)
		add("INDEXEXCHANGE_MARKETPLACE_ACCOUNT_ID", c.IndexExchangeMarketplaceAccountID)
		add("INDEXEXCHANGE_TIMEOUT_SECONDS", c.IndexExchangeTimeoutSeconds)
		add("INDEXEXCHANGE_DOWNLOAD_DIR", c.IndexExchangeDownloadDir)
		add("INDEXEXCHANGE_SERVICE_ID", c.IndexExchangeServiceID)
		add("INDEXEXCHANGE_SERVICE_SECRET", c.IndexExchangeServiceSecret)
		add("INDEXEXCHANGE_USERNAME", c.IndexExchangeUsername)
		add("INDEXEXCHANGE_PASSWORD", c.IndexExchangePassword)
	case "pubmatic", "pubmatic_mcp":
		add("PUBMATIC_API_KEY", c.PubMaticAPIKey)
		add("PUBMATIC_BASE_URL", c.PubMaticMCPBaseURL)
		add("PUBMATIC_MCP_BASE_URL", c.PubMaticMCPBaseURL)
		add("PUBMATIC_USERNAME", c.PubMaticUsername)
		add("PUBMATIC_PASSWORD", c.PubMaticPassword)
		add("PUBMATIC_API_PRODUCT", c.PubMaticAPIProduct)
		add("PUBMATIC_ACCESS_TOKEN", c.PubMaticAccessToken)
		add("PUBMATIC_DSP_ID", c.PubMaticDSPID)
		add("PUBMATIC_BUYER_ID", c.PubMaticBuyerID)
		add("PUBMATIC_SEAT_ID", c.PubMaticSeatID)
		add("PUBMATIC_TARGETING_ID", c.PubMaticTargetingID)
		add("PUBMATIC_OWNER_ID", c.PubMaticOwnerID)
		add("PUBMATIC_REPORT_DOWNLOAD_DIR", c.PubMaticReportDownloadDir)
	case "xandr", "xandr_mcp":
		add("XANDR_BASE_URL", c.XandrBaseURL)
		add("XANDR_USERNAME", c.XandrUsername)
		add("XANDR_PASSWORD", c.XandrPassword)
		add("XANDR_SEAT_ID", c.XandrSeatID)
		add("XANDR_REPORT_DOWNLOAD_DIR", c.XandrReportDownloadDir)
	case "medianet", "medianet_mcp":
		add("MEDIANET_SELECT_BASE_URL", c.MediaNetSelectBaseURL)
		add("MEDIANET_SELECT_EMAIL", c.MediaNetSelectEmail)
		add("MEDIANET_SELECT_PASSWORD", c.MediaNetSelectPassword)
		add("MEDIANET_SELECT_TOKEN", c.MediaNetSelectToken)
		add("MEDIANET_REPORT_BASE_URL", c.MediaNetReportBaseURL)
		add("MEDIANET_REPORT_TOKEN", c.MediaNetReportToken)
		add("MEDIANET_REPORT_DOWNLOAD_DIR", c.MediaNetReportDownloadDir)
	case "openx", "openx_mcp":
		add("OPENX_API_KEY", c.OpenXAPIKey)
	case "triplelift", "triplelift_mcp":
		for k, v := range buildTripleLiftEnv(c) {
			add(k, v)
		}
	}
	return env
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

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := stripQuotes(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyEnvDefault(defaultValue string, keys ...string) string {
	if value := firstNonEmptyEnv(keys...); value != "" {
		return value
	}
	return defaultValue
}

func getEnvOrDefaultInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err == nil {
			return result
		}
	}
	return defaultValue
}

func getEnvOrDefaultFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		var result float64
		if _, err := fmt.Sscanf(value, "%f", &result); err == nil {
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
