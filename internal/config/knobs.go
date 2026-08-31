package config

// ── Typed env-knob registry (#1119, extended by #1273) ──
//
// One table describes every numeric/bool/duration env knob the BINARY parses,
// wherever it parses it: the knob's canonical name, how it resolves (the
// FLEET_→CHAT_→CUTLASS_ alias chain vs a direct key), its kind, and any range
// constraints. The table is the SINGLE parse implementation behind the three
// paths that read these knobs — boot (Load, via the loadParser collector),
// hot-reload (reload.go's reloadFleet* helpers), and the `fleet
// validate-config` preflight (ValidateEnvKnobs) — so the three can never
// disagree about what a given env accepts or rejects.
//
// Semantics, shared by all three paths:
//
//   - An UNSET knob (or one that is blank after whitespace/quote stripping)
//     gets its default — never an error.
//   - A SET-but-malformed or out-of-range value is an ERROR: boot refuses to
//     start, reload keeps the running value and reports a ReloadError, and
//     validate-config reports a blocking env_vars failure. This matches the
//     loader's IP-list/TLS/network-mode posture; before #1119 these knobs
//     silently fell back to their defaults, which was fail-OPEN for security
//     knobs (FLEET_LOCKDOWN_ONLY=enabled left lockdown off).
//   - Values are cleaned before parsing: surrounding whitespace trimmed, ONE
//     layer of matching quotes stripped (podman/docker --env-file keep them),
//     then trimmed again.
//
// ── The three knob classes (#1273) ──
//
// #1119 covered only what Load itself consumes; a knob parsed at its point of
// use somewhere else in the tree stayed silently (or warn-)defaulting, and
// `fleet validate-config` could not preflight it at all. Every knob now
// carries a scope and a strictness, so there is no silent class left:
//
//  1. scopeLoader (the #1119 population) — Load consumes the value through a
//     loadParser call. Registry↔loader agreement is asserted BOTH ways from
//     source by knobs_coverage_test.go.
//  2. scopeExternal — the value is consumed OUTSIDE Load (deep in
//     internal/sandbox, internal/agentcore, internal/httpapi, cmd/fleet …),
//     usually because it is needed where no *Config is in reach. Load does
//     not consume these, but it VALIDATES them (externalKnobProblems, called
//     from Load) — so a malformed value still refuses to boot, in the same
//     one-pass error as every loader knob. Where a read happens in a verb
//     that never calls Load (`fleet backup`), the point of use resolves the
//     value through the exported EnvKnob* helpers below, which parse through
//     this same table.
//  3. lenient — a documented-lenient knob (today only the OTEL sample ratio):
//     the consumer deliberately absorbs a bad value rather than failing, so
//     boot does NOT refuse. It is still registered, so validate-config
//     preflights the syntax and reports it as an ADVISORY. Every lenient row
//     must carry the rationale in why (a registry test enforces that).
//
// Adding a knob: add a registry entry here. For a loader knob, give Load a
// loadParser call too — the pairing cannot drift silently, since a loader call
// without a registry entry (or with a mismatched kind) records a load error,
// and knobs_coverage_test.go cross-checks both directions from source. For an
// out-of-loader knob, knobs_sweep_test.go walks the whole repo for ad-hoc
// os.Getenv+parse reads and fails until the key has a row here.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// knobKind is the value type an env knob parses to.
type knobKind int

const (
	kindInt knobKind = iota
	kindInt64
	kindFloat
	kindBool
	kindDuration
	// kindStrconvBool is a bool whose CONSUMER parses it with
	// strconv.ParseBool, which accepts a NARROWER token set than kindBool
	// (1/0/t/f/true/false and the TRUE/True case variants — but not
	// yes/no/on/off). Registered separately so the registry describes what
	// the reader actually honors: validating `=yes` as fine while the reader
	// silently resolves it to false would be a worse assurance than none
	// (#1273).
	kindStrconvBool
)

// label names the kind in registry-bug messages.
func (k knobKind) label() string {
	switch k {
	case kindInt:
		return "int"
	case kindInt64:
		return "int64"
	case kindFloat:
		return "float"
	case kindBool:
		return "bool"
	case kindDuration:
		return "duration"
	case kindStrconvBool:
		return "strconv-bool"
	default:
		return "unknown"
	}
}

// knobScope says which code path consumes a knob's value — see the three
// classes in the package comment above.
type knobScope int

const (
	// scopeLoader: consumed by Load through a loadParser call (the #1119 set).
	scopeLoader knobScope = iota
	// scopeExternal: consumed outside Load, at its point of use (#1273).
	scopeExternal
)

// envKnob is one registered numeric/bool/duration env knob.
type envKnob struct {
	// key is the canonical env-var name, as reported in error messages
	// (e.g. "FLEET_MAX_COST_USD", "CONVERSATION_TTL_DAYS").
	key string
	// fleet marks a knob that resolves through the FLEET_→CHAT_→CUTLASS_
	// alias chain (lookupFleet on the suffix after "FLEET_"). A non-fleet
	// knob reads exactly key from the process env. Set it ONLY when the
	// consumer honors the aliases too (the loader always does; an external
	// consumer does when it reads through agentcore's EnvPrefix machinery,
	// and does NOT when it calls os.Getenv("FLEET_…") directly) — otherwise
	// boot would refuse over a spelling nothing ever reads.
	fleet bool
	kind  knobKind
	// min/max are optional INCLUSIVE bounds (nil = unbounded), expressed as
	// float64 so one pair covers every numeric kind. They mirror the bounds
	// the hot-reload path has always enforced (plus the two validate-config
	// enforced), so boot and reload reject the same ranges.
	min, max *float64

	// scope says who consumes the value (see knobScope).
	scope knobScope
	// readBy names the package/file that consumes an external knob, so a boot
	// failure points at the code that reads it. Required on scopeExternal rows.
	readBy string
	// lenient marks a knob whose consumer DELIBERATELY absorbs a malformed
	// value. Boot does not refuse; validate-config still preflights the syntax
	// and reports an advisory. why is the rationale and is REQUIRED here.
	lenient bool
	why     string
}

// bound builds a *float64 for the registry literals below.
func bound(v float64) *float64 { return &v }

// envKnobs is the registry: every numeric/bool/duration knob Load consumes.
// Grouping and order mirror Load's struct literal for reviewability.
var envKnobs = []envKnob{
	// ── data (interactive) ──
	{key: "CONVERSATION_TTL_DAYS", kind: kindInt},
	{key: "CONVERSATION_UNPINNED_CAP", kind: kindInt},
	{key: "FLEET_INPUT_QUEUE_RETENTION_DAYS", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_TURN_EVENT_RETENTION_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_UPLOAD_MAX_BYTES", fleet: true, kind: kindInt64},
	{key: "FLEET_SHARED_FILES_MAX_TOTAL_MB", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_AUTO_ARCHIVE_AFTER_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_SEARCH_ENABLED", kind: kindBool},
	{key: "FLEET_CONVERSATION_SOFT_DELETE", kind: kindBool},

	// ── DB connection pools (#276) ──
	{key: "FLEET_CHAT_DB_MAX_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_CHAT_DB_MIN_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_CHAT_DB_MAX_CONN_IDLE_TIME", fleet: true, kind: kindDuration},
	{key: "FLEET_CHAT_DB_MAX_CONN_LIFETIME", fleet: true, kind: kindDuration},
	{key: "FLEET_CHAT_DB_CONNECT_TIMEOUT", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_SCHED_DB_MIN_CONNS", fleet: true, kind: kindInt},
	{key: "FLEET_SCHED_DB_MAX_CONN_IDLE_TIME", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_MAX_CONN_LIFETIME", fleet: true, kind: kindDuration},
	{key: "FLEET_SCHED_DB_CONNECT_TIMEOUT", fleet: true, kind: kindDuration},

	// ── LLM (shared) ── bounds on the four hot-reloadable ceilings match
	// reload.go, so boot and reload agree (#1119).
	{key: "FLEET_MAX_ITERATIONS", fleet: true, kind: kindInt, min: bound(1), max: bound(10000)},
	{key: "FLEET_MAX_COST_USD", fleet: true, kind: kindFloat, min: bound(0)},
	{key: "FLEET_MAX_TOTAL_TOKENS", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_DEFAULT_THINKING_BUDGET_TOKENS", fleet: true, kind: kindInt},
	{key: "FLEET_SHUTDOWN_GRACE_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_TURN_TIMEOUT_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_TEMPERATURE", fleet: true, kind: kindFloat, min: bound(0)},
	{key: "LLM_MAX_TOKENS", kind: kindInt},
	{key: "FLEET_ERROR_ANALYSIS_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SELF_IMPROVE_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_AUTO_TITLE", fleet: true, kind: kindBool},
	{key: "FLEET_MEMORY_AUTOINDEX_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_MEMORY_GRAPH_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_APPROVAL_TIMEOUT_SECONDS", fleet: true, kind: kindInt},
	{key: "FLEET_AUTO_APPROVE_IN_TEST", fleet: true, kind: kindBool},
	// min 1: `fleet serve` hands this straight to admission.New, which floors
	// a 0 to total=1 with no interactive reserve — a box-wide concurrency cap
	// of ONE, not "use a default". (Only the standalone runner pool treats
	// n<1 as "fall back to 8".) A set 0 is a misconfiguration; refuse it.
	{key: "FLEET_MAX_CONCURRENT_AGENTS", fleet: true, kind: kindInt, min: bound(1)},

	// ── phone a friend / sub-agents (#175, #264, #1043) ──
	{key: "FLEET_PHONE_A_FRIEND_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SUBAGENTS_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SUBAGENTS_MAX_DEPTH", fleet: true, kind: kindInt},
	{key: "FLEET_SUBAGENTS_MAX_CHILDREN", fleet: true, kind: kindInt},
	// Out-of-range numeric fractions are CLAMPED by normalizeBudgetFraction
	// (documented: a misconfiguration must never mean "unbounded"), so no
	// bounds here — only a non-numeric value errors.
	{key: "FLEET_SUBAGENTS_BUDGET_FRACTION", fleet: true, kind: kindFloat},

	// ── task memory (#198, #285) ──
	{key: "FLEET_TASK_MEMORY_MAX_KEYS", fleet: true, kind: kindInt},
	{key: "FLEET_TASK_MEMORY_MAX_VALUE_BYTES", fleet: true, kind: kindInt},

	// ── run-history retention (#252) + task pacing (#230, #510) ──
	{key: "FLEET_RUN_LOG_RETENTION_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_KEEP_RUNS_PER_TASK", fleet: true, kind: kindInt},
	{key: "FLEET_CLEANUP_HOUR", fleet: true, kind: kindInt},
	{key: "FLEET_TASK_STARVATION_WINDOW_MINUTES", fleet: true, kind: kindInt},
	{key: "FLEET_PAUSED_TASK_EXPIRY_MINUTES", fleet: true, kind: kindInt},

	// ── host reclamation + disk backpressure ──
	// The prune age is floored at zero (which disables the sweep) rather than
	// allowed negative, and the free-space floor is a percentage, so it is
	// bounded to [0, 100] — a typo like 500 would otherwise shed all scheduled
	// work forever.
	{key: "FLEET_WORKTREE_PRUNE_AGE", fleet: true, kind: kindDuration, min: bound(0)},
	{key: "FLEET_DISK_MIN_FREE_PERCENT", fleet: true, kind: kindInt, min: bound(0), max: bound(100)},

	// ── process log file sink (#298) + log archival (#272) ──
	{key: "FLEET_LOG_MAX_SIZE_MB", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_MAX_AGE_DAYS", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_MAX_BACKUPS", fleet: true, kind: kindInt},
	{key: "FLEET_LOG_COMPRESS", fleet: true, kind: kindBool},
	{key: "FLEET_LOG_ARCHIVE_AFTER_DAYS", fleet: true, kind: kindInt},

	// ── remote MCP OAuth (#443) ──
	{key: "FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP", fleet: true, kind: kindBool},

	// ── rate limit (interactive) ──
	{key: "CHAT_RATE_PER_MIN", kind: kindInt},
	{key: "CHAT_RATE_PER_DAY", kind: kindInt},
	{key: "FLEET_CHAT_RATE_LIMIT_ENABLED", kind: kindBool},
	{key: "FLEET_CHAT_RATE_LIMIT_CONCURRENT", kind: kindInt},

	// ── sandbox ──
	{key: "FLEET_PII_REDACTION_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_CONTEXT_HANDLES_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED", fleet: true, kind: kindBool},
	// Waives the boot requirement for the open-egress NetworkPolicy, so it is
	// a security posture rather than a preference: registered like any other
	// bool so a typo ("ture") is refused at the seam instead of silently
	// reading as false and — here — as the SAFE value, which would hide the
	// operator's intent behind an error they never see.
	{key: "FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED", kind: kindBool},
	{key: "FLEET_A2A_ENABLED", fleet: true, kind: kindBool},
	{key: "FLEET_SANDBOX_PIDS", kind: kindInt},
	{key: "FLEET_SANDBOX_DISK_GB", kind: kindInt},
	{key: "FLEET_SANDBOX_MEMORY_MAX_MB", fleet: true, kind: kindInt},
	{key: "FLEET_SANDBOX_CPUS_MAX", fleet: true, kind: kindFloat},
	{key: "FLEET_SANDBOX_PIDS_MAX", fleet: true, kind: kindInt},
	// min 0: 0 is a real value (no warm pool, #1264) and unset means "derive",
	// so every explicit negative — the -1 sentinel spelled out included — is a
	// misconfiguration to refuse at the seam, not something for the pool to
	// absorb (#1299).
	{key: "FLEET_SANDBOX_WARM_SIZE", fleet: true, kind: kindInt, min: bound(0)},
	{key: "FLEET_SANDBOX_WARM_TTL", fleet: true, kind: kindInt},

	// ── python REPL (#213) ──
	{key: "FLEET_PYTHON_CELL_TIMEOUT", fleet: true, kind: kindInt},
	{key: "FLEET_PYTHON_REPL_IDLE_TTL", fleet: true, kind: kindInt},
	{key: "FLEET_PYTHON_REPL_MAX", fleet: true, kind: kindInt},

	// ── lockdown / test harness ── FLEET_LOCKDOWN_ONLY is the security knob
	// whose silent fail-open motivated #1119: an unrecognized token used to
	// leave lockdown OFF; now it refuses to boot.
	{key: "FLEET_LOCKDOWN_ONLY", fleet: true, kind: kindBool},
	{key: "FLEET_MOCK_MODE", fleet: true, kind: kindBool},

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

	// ── backup retention (`fleet backup`, a verb that never calls Load —
	// internal/admincli resolves it through EnvKnobInt below).
	{key: "FLEET_BACKUP_RETENTION_DAYS", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/admincli (backup prune)"},

	// ── sandbox runtime sizing ── the Kata guest-memory overhead only ever
	// ADDS memory, but a typo silently sizing every VM off the default is the
	// #1119 bug class; the consumer requires a POSITIVE integer.
	{key: "FLEET_SANDBOX_KATA_OVERHEAD_MB", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/sandbox (kataOverheadMB)"},

	// ── agent runtime (internal/agentcore) ── the fleet:true rows here resolve
	// through the EnvPrefix machinery, which honors the CHAT_/CUTLASS_ aliases;
	// the two fleet:false rows are read with a plain os.Getenv on the canonical
	// name, so the registry must not accept an alias spelling nothing reads.
	{key: "FLEET_TOOL_DISCLOSURE_THRESHOLD", kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/agentcore (deferred tool disclosure)"},
	{key: "FLEET_MODEL_CACHE_TTL_MINUTES", fleet: true, kind: kindInt, min: bound(1),
		scope: scopeExternal, readBy: "internal/agentcore (OpenRouter catalog TTL)"},
	{key: "FLEET_RETRY_MAX_ATTEMPTS", fleet: true, kind: kindInt, min: bound(0),
		scope: scopeExternal, readBy: "internal/agentcore (provider retry budget)"},
	// Clamped by the consumer to [MinMaxToolOutputBytes, HardMaxToolOutputBytes]
	// and to (0,1] respectively — a documented clamp, like the subagent budget
	// fraction above — so only a non-numeric value is refused.
	{key: "FLEET_MAX_TOOL_OUTPUT_BYTES", kind: kindInt,
		scope: scopeExternal, readBy: "internal/agentcore (model-output boundary)"},
	{key: "FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD", fleet: true, kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (context pressure)"},
	{key: "FLEET_CONTEXT_COMPACTION_THRESHOLD", fleet: true, kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (context compaction)"},
	// Clamped by the consumer to (0,1] like the two context thresholds above;
	// 1 disables in practice (the hard ceiling fires first at 100%).
	{key: "FLEET_BUDGET_WINDDOWN_FRACTION", fleet: true, kind: kindFloat,
		scope: scopeExternal, readBy: "internal/agentcore (budget wind-down notice)"},
	// The three agentcore kill-switches are read with strconv.ParseBool, so
	// they take its narrower token set (see kindStrconvBool).
	{key: "FLEET_DISABLE_PROMPT_CACHE", fleet: true, kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (prompt-cache breakpoints)"},
	{key: "FLEET_DISABLE_OPENROUTER_MODELS", fleet: true, kind: kindStrconvBool,
		scope: scopeExternal, readBy: "internal/agentcore (live model catalog)"},
	{key: "FLEET_SCHEDULED_AUTO_COMPACT", fleet: true, kind: kindStrconvBool,
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

	// ── documented-lenient (#1273) ── the ONE knob whose leniency is
	// deliberate. Registered so validate-config preflights the syntax as an
	// advisory; boot does not refuse.
	{key: "FLEET_OTEL_SAMPLE_RATIO", kind: kindFloat,
		scope: scopeExternal, readBy: "internal/otelsetup (head sampler)",
		lenient: true,
		why:     "tracing dial with a documented total order over bad input: unset/unparseable/NaN/±Inf/≥1 all mean AlwaysSample and ≤0 means never. Refusing to boot the whole service over an observability knob is worse than sampling everything, so validate-config reports a malformed ratio as an advisory instead"},
}

// envKnobByKey indexes the registry by canonical name for the loader and the
// reload path.
var envKnobByKey = func() map[string]*envKnob {
	m := make(map[string]*envKnob, len(envKnobs))
	for i := range envKnobs {
		k := &envKnobs[i]
		if _, dup := m[k.key]; dup {
			panic("config: duplicate envKnobs entry " + k.key)
		}
		m[k.key] = k
	}
	return m
}()

// lookup resolves the knob's raw value from the environment; ok is false when
// no spelling is set (non-empty).
func (k *envKnob) lookup() (string, bool) {
	if k.fleet {
		return lookupFleet(strings.TrimPrefix(k.key, canonicalPrefix))
	}
	v := os.Getenv(k.key)
	return v, v != ""
}

// cleanEnvValue normalizes a raw env value before typed parsing: surrounding
// whitespace is trimmed, ONE layer of matching quotes is stripped (podman/
// docker --env-file keep them in place), then the result is trimmed again. A
// blank result is treated as UNSET (the default applies) rather than
// malformed.
func cleanEnvValue(raw string) string {
	return strings.TrimSpace(stripQuotes(strings.TrimSpace(raw)))
}

// checkBounds enforces the knob's optional inclusive bounds. The message names
// the offending value and the full expected range; the caller prefixes the env
// var name.
func (k *envKnob) checkBounds(v float64) error {
	if (k.min != nil && v < *k.min) || (k.max != nil && v > *k.max) {
		val := strconv.FormatFloat(v, 'f', -1, 64)
		switch {
		case k.min != nil && k.max != nil:
			return fmt.Errorf("%s is out of range (must be between %s and %s)",
				val, formatBound(*k.min), formatBound(*k.max))
		case k.min != nil:
			return fmt.Errorf("%s is out of range (must be >= %s)", val, formatBound(*k.min))
		default:
			return fmt.Errorf("%s is out of range (must be <= %s)", val, formatBound(*k.max))
		}
	}
	return nil
}

func formatBound(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// The typed parsers below share one contract: (value, set, err). set is false
// when the cleaned value is blank (treat as unset → default); err is non-nil
// for a non-blank value that fails to parse or falls outside the bounds. Error
// messages name the offending value and the expected format; callers prefix
// the env-var name (Load's collector and validate-config inline it, the
// reload path carries it in ReloadError.Key).

func (k *envKnob) parseInt(raw string) (int, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, fmt.Errorf("invalid integer %q (expected a whole number like \"30\")", v)
	}
	if err := k.checkBounds(float64(i)); err != nil {
		return 0, false, err
	}
	return i, true, nil
}

func (k *envKnob) parseInt64(raw string) (int64, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid integer %q (expected a whole number like \"1073741824\")", v)
	}
	if err := k.checkBounds(float64(i)); err != nil {
		return 0, false, err
	}
	return i, true, nil
}

func (k *envKnob) parseFloat(raw string) (float64, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid number %q (expected a decimal number like \"12.5\")", v)
	}
	if err := k.checkBounds(f); err != nil {
		return 0, false, err
	}
	return f, true, nil
}

func (k *envKnob) parseBool(raw string) (bool, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return false, false, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	}
	return false, false, fmt.Errorf("invalid boolean %q (expected one of: 1, 0, true, false, yes, no, on, off)", v)
}

// parseStrconvBool mirrors a consumer that calls strconv.ParseBool directly
// (#1273): the accepted tokens are exactly ParseBool's, so the registry never
// certifies a spelling the reader would resolve to the zero value.
func (k *envKnob) parseStrconvBool(raw string) (bool, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return false, false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false, fmt.Errorf("invalid boolean %q (this knob is read with Go's strconv.ParseBool, which accepts only: 1, 0, t, f, true, false, TRUE, FALSE, True, False — not yes/no/on/off)", v)
	}
	return b, true, nil
}

func (k *envKnob) parseDuration(raw string) (time.Duration, bool, error) {
	v := cleanEnvValue(raw)
	if v == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, fmt.Errorf("invalid duration %q (expected a Go duration like \"30s\", \"5m\", or \"1h30m\")", v)
	}
	if err := k.checkBounds(float64(d)); err != nil {
		return 0, false, err
	}
	return d, true, nil
}

// check parses raw for the knob's kind and reports the first problem, or nil
// when the value is well-formed (or blank = unset). validate-config's
// registry walk uses it; the typed callers use the parse* funcs directly.
func (k *envKnob) check(raw string) error {
	var err error
	switch k.kind {
	case kindInt:
		_, _, err = k.parseInt(raw)
	case kindInt64:
		_, _, err = k.parseInt64(raw)
	case kindFloat:
		_, _, err = k.parseFloat(raw)
	case kindBool:
		_, _, err = k.parseBool(raw)
	case kindStrconvBool:
		_, _, err = k.parseStrconvBool(raw)
	case kindDuration:
		_, _, err = k.parseDuration(raw)
	}
	return err
}

// ValidateEnvKnobs preflights every registered STRICT numeric/bool/duration
// env knob — the loader's own (#1119) and, since #1273, the out-of-loader ones
// too: each knob that is SET (non-blank after quote stripping, any alias
// spelling it is actually read under) gets the exact parse + range check boot
// enforces, and each problem string names the env var, the offending value,
// and the expected format. Unset knobs are fine — the default applies.
// `fleet validate-config` calls this so its preflight is table-driven from the
// same registry the boot and hot-reload paths parse through, and can never
// drift from them.
//
// Documented-lenient knobs are excluded here and reported by
// ValidateLenientEnvKnobs instead, so a blocking failure means "this refuses
// to boot" and nothing weaker.
func ValidateEnvKnobs() []string {
	return validateKnobs(func(k *envKnob) bool { return !k.lenient })
}

// ValidateLenientEnvKnobs preflights the documented-lenient knobs (#1273):
// their consumer deliberately absorbs a malformed value, so boot does not
// refuse — but an operator who typo'd one still deserves to be told, so
// `fleet validate-config` reports these as ADVISORIES (warn, never blocking).
// Each problem names the variable, the offending value, and what the consumer
// will do with it instead.
func ValidateLenientEnvKnobs() []string {
	problems := validateKnobs(func(k *envKnob) bool { return k.lenient })
	for i, p := range problems {
		problems[i] = p + " — not a boot failure: " + lenientWhyByProblem(p)
	}
	return problems
}

// lenientWhyByProblem recovers the rationale for the knob a problem string
// names (problems are formatted "KEY: …"), so the advisory carries the WHY
// from the registry row rather than a second copy of it here.
func lenientWhyByProblem(problem string) string {
	key, _, _ := strings.Cut(problem, ":")
	if k := envKnobByKey[key]; k != nil && k.why != "" {
		return k.why
	}
	return "documented-lenient knob"
}

// validateKnobs walks the registry, keeping the rows want selects.
func validateKnobs(want func(*envKnob) bool) []string {
	var problems []string
	for i := range envKnobs {
		k := &envKnobs[i]
		if !want(k) {
			continue
		}
		raw, ok := k.lookup()
		if !ok {
			continue
		}
		if err := k.check(raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", k.key, err))
		}
	}
	return problems
}

// externalKnobProblems is the boot gate for scopeExternal knobs (#1273). Load
// calls it and folds the result into the SAME one-pass error the loader knobs
// produce, so a malformed knob refuses to boot whether Load consumes the value
// or some package deep in the tree does. Lenient rows are skipped by design.
//
// Each message names the consumer, because the operator cannot infer it from a
// config-load failure the way they can for a loader knob.
func externalKnobProblems() []string {
	var problems []string
	for i := range envKnobs {
		k := &envKnobs[i]
		if k.scope != scopeExternal || k.lenient {
			continue
		}
		raw, ok := k.lookup()
		if !ok {
			continue
		}
		if err := k.check(raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v (read by %s)", k.key, err, k.readBy))
		}
	}
	return problems
}

// ── the exported point-of-use readers (#1273) ──
//
// A `fleet` verb that never calls Load (`fleet backup`) gets no boot gate, so
// it resolves its knob through these helpers instead: same registry row, same
// parser, same bounds, and a real error the verb can refuse on. They are
// deliberately narrow — one lookup, no collection — since a CLI verb wants to
// fail on the knob it actually needs, not on an unrelated malformed one.
//
// Calling one with a key that is not a scopeExternal row of the matching kind
// is a programming error and is reported as such (the sweep test and the
// registry tests both catch it, but a runtime error beats a silent default).

// EnvKnobInt resolves a registered external int knob, returning def when it is
// unset (or blank) and an error when it is set but malformed/out of range.
func EnvKnobInt(key string, def int) (int, error) {
	k, err := externalKnob(key, kindInt)
	if err != nil {
		return def, err
	}
	raw, ok := k.lookup()
	if !ok {
		return def, nil
	}
	v, set, err := k.parseInt(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", k.key, err)
	}
	if !set {
		return def, nil
	}
	return v, nil
}

// externalKnob resolves the registry row a point-of-use reader expects.
func externalKnob(key string, kind knobKind) (*envKnob, error) {
	k := envKnobByKey[key]
	if k == nil {
		return nil, fmt.Errorf("%s: BUG — read at its point of use but not in the envKnobs registry; add a scopeExternal entry in knobs.go so boot and `fleet validate-config` validate it (#1273)", key)
	}
	if k.scope != scopeExternal {
		return nil, fmt.Errorf("%s: BUG — envKnobs registry has it as a loader knob; read it off the *Config instead", key)
	}
	if k.kind != kind {
		return nil, fmt.Errorf("%s: BUG — envKnobs registry kind mismatch (registry %s, caller reads %s); fix the entry in knobs.go", key, k.kind.label(), kind.label())
	}
	return k, nil
}

// ── the Load-side collector ──

// loadParser resolves typed env knobs for Load, collecting every malformed
// value so boot can refuse with ONE error naming all of them (instead of
// failing one var at a time). Its methods keep the historical helper names so
// Load's call sites read unchanged; each returns the default for an
// unset/blank knob and records an error (still returning the default, so the
// struct literal stays total) for a set-but-malformed one.
type loadParser struct {
	problems []string
}

// knob resolves the registry entry the caller expects, recording a loud
// internal error when it is missing or mismatched: this is what makes the
// loader↔registry pairing impossible to drift silently — a new getenv* call
// without a registry entry fails every Load (and therefore every test that
// boots a config).
func (p *loadParser) knob(key string, kind knobKind) *envKnob {
	k := envKnobByKey[key]
	if k == nil {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — read by the loader but not in the envKnobs registry; add an entry in knobs.go so boot, hot-reload, and `fleet validate-config` all validate it (#1119)", key))
		return nil
	}
	if k.kind != kind {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — envKnobs registry kind mismatch (registry %s, loader reads %s); fix the entry in knobs.go", key, k.kind.label(), kind.label()))
		return nil
	}
	if k.scope != scopeLoader {
		p.problems = append(p.problems, fmt.Sprintf(
			"%s: BUG — envKnobs registry marks it scopeExternal but the loader consumes it; drop the scope/readBy fields from its entry in knobs.go (#1273)", key))
		return nil
	}
	return k
}

// collectExternalKnobs folds the scopeExternal boot gate (#1273) into the same
// one-pass error the loader knobs produce, so `fleet serve` refuses on every
// malformed knob at once — whoever ends up consuming the value.
func (p *loadParser) collectExternalKnobs() {
	p.problems = append(p.problems, externalKnobProblems()...)
}

// fail records one malformed knob, prefixed with its canonical env-var name.
func (p *loadParser) fail(k *envKnob, err error) {
	p.problems = append(p.problems, fmt.Sprintf("%s: %v", k.key, err))
}

// err reports every problem collected during Load, or nil when the
// environment parsed clean. The message enumerates each offending variable so
// an operator fixes the whole file in one pass.
func (p *loadParser) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment configuration (unset variables get defaults; set variables must parse):\n  - %s",
		strings.Join(p.problems, "\n  - "))
}

func (p *loadParser) getenvInt(key string, def int) int {
	k := p.knob(key, kindInt)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseInt(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvBool(key string, def bool) bool {
	k := p.knob(key, kindBool)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseBool(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetInt(suffix string, def int) int {
	return p.getenvInt(canonicalPrefix+strings.TrimLeft(suffix, "_"), def)
}

func (p *loadParser) getenvFleetBool(suffix string, def bool) bool {
	return p.getenvBool(canonicalPrefix+strings.TrimLeft(suffix, "_"), def)
}

func (p *loadParser) getenvFleetInt64(suffix string, def int64) int64 {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindInt64)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseInt64(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetFloat(suffix string, def float64) float64 {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindFloat)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseFloat(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}

func (p *loadParser) getenvFleetDuration(suffix string, def time.Duration) time.Duration {
	k := p.knob(canonicalPrefix+strings.TrimLeft(suffix, "_"), kindDuration)
	if k == nil {
		return def
	}
	raw, ok := k.lookup()
	if !ok {
		return def
	}
	v, set, err := k.parseDuration(raw)
	if err != nil {
		p.fail(k, err)
		return def
	}
	if !set {
		return def
	}
	return v
}
