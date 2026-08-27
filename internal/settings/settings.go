// Package settings is the admin-managed workspace feature settings registry
// and service (the "Features" section of the web admin page).
//
// It closes the gap the #450/#506/#175-era features shared: each shipped
// governed and tested but configurable only through an env flag, so in
// practice nobody could find or administer them. The registry curates exactly
// the settings that can take effect LIVE — each one's consumer re-reads its
// value per turn / per run / per tool call — so the admin page never has to
// lie about a change requiring a restart. Boot-bound settings (listener
// addresses, sandbox sizing, FLEET_MAX_CONCURRENT_AGENTS, …) and
// secret-bearing config (SMTP/webhook credentials) are deliberately NOT here;
// see docs/ADMIN-SETTINGS.md for the full inventory and why.
//
// Precedence: admin override (workspace_settings row) > env var > built-in
// default. The env var keeps working as the deployment default; an admin
// override wins until it is reset (row deleted), which reverts to the
// env-derived value. Values are validated against the registry BEFORE they are
// persisted, so a bad value can never poison the store or the running process
// (the same discipline as config hot-reload #286).
//
// The service itself is dependency-light: how each value is APPLIED to the
// running system (agentcore.SetPIIRedactor, config Live setters, …) is
// injected by cmd/fleet as per-key hooks, mirroring how the LLM-provider
// resolver reload is injected via WithLLMProvidersChanged.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Kind is a setting's value type. It drives both validation here and the
// control the admin UI renders (toggle / segmented control / number field).
type Kind string

const (
	KindBool Kind = "bool"
	KindInt  Kind = "int"
	KindEnum Kind = "enum"
	// KindURL is a free-text http(s) URL, empty allowed ("not configured").
	// Used for operator-deployed service endpoints (never credentials — the
	// registry stays secret-free by construction).
	KindURL Kind = "url"
	// KindModel is a model slug: "<provider-or-lab>/<model>", the shape both
	// the OpenRouter catalog and admin-configured workspace providers
	// ("<provider>/<model>", GET /llm-provider-models) route on. Free text —
	// the catalog changes weekly and admin providers name arbitrary models, so
	// an enum would be stale by construction. Case is preserved (slugs are
	// case-sensitive upstream); the one shape rule is a "/" separating two
	// non-empty halves with no whitespace, which every routable slug has.
	KindModel Kind = "model"
)

// Spec declares one admin-configurable setting: its stable key (DB + API), its
// type and bounds, and the env var that supplies the default — provenance the
// UI surfaces so an operator can connect the setting back to the deployment's
// env file. Presentation copy (labels, grouping, help text) lives client-side.
type Spec struct {
	Key  string   `json:"key"`
	Kind Kind     `json:"kind"`
	Enum []string `json:"enum,omitempty"` // legal values when Kind == KindEnum
	// Min/Max bound KindInt values (inclusive). MinZeroOK additionally admits
	// exactly 0 below Min for "0 = disabled/unlimited" semantics.
	Min       int  `json:"min,omitempty"`
	Max       int  `json:"max,omitempty"`
	MinZeroOK bool `json:"min_zero_ok,omitempty"`
	// EnvVar names the env var(s) the default comes from, for provenance only —
	// resolution happens at boot in cmd/fleet, not here.
	EnvVar string `json:"env_var"`
}

// Registry returns the curated setting specs, in stable display order. Every
// entry MUST be live-applicable (its consumer re-reads per use); adding a
// boot-bound setting here would make the admin page dishonest.
func Registry() []Spec {
	return []Spec{
		// Privacy & data protection. The three PII keys feed ONE redactor: the
		// cmd/fleet apply hooks rebuild it from the current trio on any change.
		{Key: "pii_redaction_mode", Kind: KindEnum,
			Enum:   []string{"off", "observe", "redact", "block"},
			EnvVar: "FLEET_PII_REDACTION_ENABLED / FLEET_PII_REDACTION_MODE"},
		// ORDER MATTERS: the URL must apply BEFORE the engine. Boot ApplyAll
		// runs hooks in this order, and the engine hook validates that a
		// rampart selection has a URL — url-after-engine would reject a
		// perfectly good persisted config at every boot (caught live).
		// TestPIIRegistryOrderBootSafe guards this.
		{Key: "pii_rampart_url", Kind: KindURL,
			EnvVar: "FLEET_PII_RAMPART_URL"},
		{Key: "pii_redaction_engine", Kind: KindEnum,
			Enum:   []string{"pattern", "rampart"},
			EnvVar: "FLEET_PII_REDACTION_ENGINE"},
		{Key: "guardrail_url", Kind: KindURL,
			EnvVar: "FLEET_GUARDRAIL_URL"},
		{Key: "guardrail_mode", Kind: KindEnum,
			Enum:   []string{"off", "observe", "block"},
			EnvVar: "FLEET_GUARDRAIL_MODE"},

		// Agent runtime.
		{Key: "tool_disclosure_threshold", Kind: KindInt, Min: 1, Max: 100000,
			EnvVar: "FLEET_TOOL_DISCLOSURE_THRESHOLD"},
		// Zero is retained as a backwards-compatible spelling of the safe 64KiB
		// default. Agentcore also clamps every source to the non-disableable 128KiB
		// hard maximum, but rejecting larger admin writes keeps the persisted/UI
		// value honest about what the runtime enforces.
		{Key: "max_tool_output_bytes", Kind: KindInt, Min: 1024, Max: 128 * 1024, MinZeroOK: true,
			EnvVar: "FLEET_MAX_TOOL_OUTPUT_BYTES"},
		// The approval default-deny window (#225). Bounds mirror the
		// per-conversation override's: at least a minute (a shorter window is a
		// deny in disguise) and at most 24h (a typo must not leave cards
		// effectively un-expiring). The stager reads the live value once per
		// turn, so an edit governs the next staged card without a restart.
		{Key: "approval_timeout_seconds", Kind: KindInt, Min: 60, Max: 86400,
			EnvVar: "FLEET_APPROVAL_TIMEOUT_SECONDS"},
		{Key: "phone_a_friend_enabled", Kind: KindBool,
			EnvVar: "FLEET_PHONE_A_FRIEND_ENABLED"},
		{Key: "subagents_enabled", Kind: KindBool,
			EnvVar: "FLEET_SUBAGENTS_ENABLED"},

		// Model tiers (#1187): the two role slots the chat UI pins — what a new
		// conversation starts on, and the suggest_advanced_model escalation
		// target. Live: the web re-reads them from /client-config on every
		// shell mount, and the Go escalation path reads the agentcore holder
		// per call. Deliberately NOT here: the scheduled-task default model
		// (FLEET_TASK_MODEL) — the scheduler snapshots it at boot, so admitting
		// it would violate the live-apply rule this registry is built on.
		{Key: "default_model", Kind: KindModel,
			EnvVar: "FLEET_DEFAULT_MODEL"},
		{Key: "advanced_model", Kind: KindModel,
			EnvVar: "FLEET_ADVANCED_MODEL"},

		// Workspace features.
		{Key: "memory_autoindex_enabled", Kind: KindBool,
			EnvVar: "FLEET_MEMORY_AUTOINDEX_ENABLED"},
		{Key: "error_analysis_enabled", Kind: KindBool,
			EnvVar: "FLEET_ERROR_ANALYSIS_ENABLED"},
		{Key: "auto_title_enabled", Kind: KindBool,
			EnvVar: "FLEET_AUTO_TITLE"},
		{Key: "connector_recommendations_enabled", Kind: KindBool,
			EnvVar: "FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED"},
		{Key: "context_handles_enabled", Kind: KindBool,
			EnvVar: "FLEET_CONTEXT_HANDLES_ENABLED"},
		// Shared file library total-size cap in MB (docs/SHARED-FILES.md).
		// MinZeroOK: 0 = unlimited, for deployments that genuinely want a very
		// large library and accept the disk cost. Live: the upload handler
		// reads it per request. Max 16 TiB — far past any sane library, but a
		// finite bound keeps a fat-fingered value from overflowing the
		// bytes conversion.
		{Key: "shared_files_max_total_mb", Kind: KindInt, Min: 1, Max: 16 * 1024 * 1024, MinZeroOK: true,
			EnvVar: "FLEET_SHARED_FILES_MAX_TOTAL_MB"},
	}
}

// Validate checks value against spec and returns its normalized form (trimmed,
// lowercased booleans/enums, canonical integer). It never mutates state.
func Validate(spec Spec, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch spec.Kind {
	case KindBool:
		switch strings.ToLower(v) {
		case "true":
			return "true", nil
		case "false":
			return "false", nil
		}
		return "", fmt.Errorf("%s: want true or false, got %q", spec.Key, value)
	case KindEnum:
		lv := strings.ToLower(v)
		for _, e := range spec.Enum {
			if lv == e {
				return lv, nil
			}
		}
		return "", fmt.Errorf("%s: want one of %s, got %q", spec.Key, strings.Join(spec.Enum, "|"), value)
	case KindURL:
		if v == "" {
			return "", nil
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("%s: want an http(s) URL (or empty), got %q", spec.Key, value)
		}
		return v, nil
	case KindModel:
		if v == "" || len(v) > 200 || strings.ContainsAny(v, " \t\n") {
			return "", fmt.Errorf("%s: want a model slug like provider/model, got %q", spec.Key, value)
		}
		if i := strings.Index(v, "/"); i <= 0 || i == len(v)-1 {
			return "", fmt.Errorf("%s: want a model slug like provider/model (a %q separating two non-empty halves), got %q", spec.Key, "/", value)
		}
		return v, nil
	case KindInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("%s: invalid integer %q", spec.Key, value)
		}
		if n == 0 && spec.MinZeroOK {
			return "0", nil
		}
		if n < spec.Min || n > spec.Max {
			bounds := fmt.Sprintf("between %d and %d", spec.Min, spec.Max)
			if spec.MinZeroOK {
				bounds += " (or exactly 0)"
			}
			return "", fmt.Errorf("%s: %d is out of range (must be %s)", spec.Key, n, bounds)
		}
		return strconv.Itoa(n), nil
	default:
		return "", fmt.Errorf("%s: unknown kind %q", spec.Key, spec.Kind)
	}
}

// Resolved is one setting as the admin API reports it: the spec plus the
// effective value, where it came from, and the default it would revert to.
type Resolved struct {
	Spec
	// Value is the EFFECTIVE value (the admin override when set, else Default).
	Value string `json:"value"`
	// Source is "admin" when an override row exists, else "default".
	Source string `json:"source"`
	// Default is the env-derived boot default the setting reverts to on reset.
	Default string `json:"default"`
	// UpdatedAt/UpdatedBy describe the override row (zero values when Source is
	// "default" and no stale row exists).
	UpdatedAt int64  `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	// Stale is true when an override row EXISTS but no longer validates (e.g. a
	// bound tightened in a later release): the default serves, but the row is
	// surfaced — with its attribution — so the admin can see and Reset it.
	// Without this the ignored row would be invisible and undeletable from the
	// panel, and could silently spring back to life if the bounds loosen again.
	Stale bool `json:"stale,omitempty"`
	// ApplyError, when non-empty, says this setting's last apply FAILED (e.g. a
	// persisted rampart engine whose service URL disappeared from the env
	// across a restart) — the stored value is NOT in effect. Surfaced so the
	// panel is honest per-row and the admin can fix or Reset from the UI.
	ApplyError string `json:"apply_error,omitempty"`
}

// SourceAdmin / SourceDefault are the Resolved.Source values.
const (
	SourceAdmin   = "admin"
	SourceDefault = "default"
)

// Store is the persistence seam (satisfied by *store.Store).
type Store interface {
	WorkspaceSettings(ctx context.Context) (map[string]store.WorkspaceSetting, error)
	SetWorkspaceSetting(ctx context.Context, key, value, updatedBy string) (store.WorkspaceSetting, error)
	DeleteWorkspaceSetting(ctx context.Context, key string) error
}

// ApplyFunc pushes one setting's effective value into the running system
// (swap the PII redactor, set a live config field, …). Hooks are injected by
// cmd/fleet. override says whether value comes from an admin row (true) or is
// the env-derived default (false) — hooks that shadow a per-use env read (the
// agentcore holders) must CLEAR their override on false rather than pin the
// boot env value, so `unset = env keeps working live` stays true after boot.
type ApplyFunc func(value string, override bool) error

// ErrUnknownKey is returned for a key not in the registry; ErrInvalidValue
// wraps a validation failure on Set. Both are reported BEFORE anything is
// persisted or applied, so the API layer can map them to 404/400 while every
// other error (persist, apply) is a 500.
var (
	ErrUnknownKey   = errors.New("unknown setting key")
	ErrInvalidValue = errors.New("invalid setting value")
	// ErrLoadFailed wraps an ApplyAll that could not even READ the override
	// rows — the one case where the panel cannot render truthfully and should
	// degrade to 501. Per-key APPLY failures are not this: the panel stays up
	// and the affected row carries its error (Resolved.ApplyError), so the
	// admin can fix or Reset it from the UI instead of being stranded.
	ErrLoadFailed = errors.New("workspace settings load failed")
)

// Service resolves, persists, and applies admin overrides. Set/Reset/ApplyAll
// serialize under one mutex so two admin edits can't interleave their store
// write and apply steps (the same discipline as the LLM-provider reloader).
type Service struct {
	st       Store
	specs    map[string]Spec
	order    []string
	defaults map[string]string // env-derived boot defaults, snapshotted before any override applies
	hooks    map[string]ApplyFunc
	mu       sync.Mutex
	// applyErrs remembers, per key, the last boot-apply failure so Snapshot can
	// surface it (Resolved.ApplyError). Cleared by a successful Set/Reset.
	applyErrs map[string]string
}

// NewService builds a Service. defaults maps every registry key to its
// env-derived boot default; hooks maps keys to their apply functions. Both
// must cover the registry — a MISSING default or hook is a programming error
// reported at construction so it can't ship silently. A default that fails
// registry validation is NOT an error: the registry bounds constrain what an
// admin may write, while some settings accept legacy env spellings outside
// those bounds — such a default is kept verbatim
// (with a log) as the display/reset target so one out-of-bounds env value can
// never disable the whole panel. cmd/fleet derives every default from typed
// sources, so a kept-verbatim default is always parseable by its hook.
func NewService(st Store, defaults map[string]string, hooks map[string]ApplyFunc) (*Service, error) {
	s := &Service{
		st:        st,
		specs:     map[string]Spec{},
		defaults:  map[string]string{},
		hooks:     hooks,
		applyErrs: map[string]string{},
	}
	for _, spec := range Registry() {
		s.specs[spec.Key] = spec
		s.order = append(s.order, spec.Key)
		d, ok := defaults[spec.Key]
		if !ok {
			return nil, fmt.Errorf("settings: no boot default for %q", spec.Key)
		}
		nd, err := Validate(spec, d)
		if err != nil {
			// Out-of-bounds env default: keep it verbatim. It is what the runtime
			// actually does and what a reset must revert to; only NEW admin writes
			// are held to the registry bounds.
			log.Printf("workspace settings: env default for %s is outside the admin-settable bounds (%v); keeping it as the default", spec.Key, err)
			nd = strings.TrimSpace(d)
		}
		s.defaults[spec.Key] = nd
		if _, ok := hooks[spec.Key]; !ok {
			return nil, fmt.Errorf("settings: no apply hook for %q", spec.Key)
		}
	}
	return s, nil
}

// Snapshot returns every setting's resolved state in registry order — the
// admin GET. No secret material: registry values are feature toggles and
// bounds, never credentials.
func (s *Service) Snapshot(ctx context.Context) ([]Resolved, error) {
	overrides, err := s.st.WorkspaceSettings(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Resolved, 0, len(s.order))
	for _, key := range s.order {
		out = append(out, s.resolve(key, overrides))
	}
	return out, nil
}

// resolve computes one key's Resolved from the override rows. An override that
// no longer validates (e.g. a bound tightened in a later release) does not
// serve — the default does — but it is surfaced as Stale with its attribution
// so the panel can show and Reset it (an invisible row could otherwise spring
// back to life when the bounds change again).
func (s *Service) resolve(key string, overrides map[string]store.WorkspaceSetting) Resolved {
	spec := s.specs[key]
	r := Resolved{Spec: spec, Value: s.defaults[key], Source: SourceDefault, Default: s.defaults[key]}
	if row, ok := overrides[key]; ok {
		v, err := Validate(spec, row.Value)
		if err != nil {
			r.Stale = true
			r.UpdatedAt = row.UpdatedAt
			r.UpdatedBy = row.UpdatedBy
			return r
		}
		r.Value = v
		r.Source = SourceAdmin
		r.UpdatedAt = row.UpdatedAt
		r.UpdatedBy = row.UpdatedBy
	}
	r.ApplyError = s.applyErrs[key]
	return r
}

// Set validates, persists, and applies one override. The value is validated
// BEFORE the store write (a bad value is a 400, never persisted). If the apply
// hook fails, the just-written row is deleted again (compensation) so the DB
// can never disagree with the running system across a restart — the admin sees
// the error and the previous state, live and persisted, keeps serving.
func (s *Service) Set(ctx context.Context, key, value, updatedBy string) (Resolved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[key]
	if !ok {
		return Resolved{}, ErrUnknownKey
	}
	v, err := Validate(spec, value)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %w", ErrInvalidValue, err)
	}
	row, err := s.st.SetWorkspaceSetting(ctx, key, v, updatedBy)
	if err != nil {
		return Resolved{}, fmt.Errorf("persist %s: %w", key, err)
	}
	if err := s.apply(key, v, true); err != nil {
		// Compensate: a value that did not take effect must not lie in wait in
		// the DB to be silently activated by the next boot's ApplyAll.
		if delErr := s.st.DeleteWorkspaceSetting(ctx, key); delErr != nil {
			return Resolved{}, fmt.Errorf("%w (and rolling back the persisted row failed: %w — run Reset)", err, delErr)
		}
		return Resolved{}, err
	}
	delete(s.applyErrs, key)
	// Audit line: key and value are registry-validated constants (never raw
	// input); updatedBy is the authenticated admin identity.
	log.Printf("workspace settings: %s = %s (set by %s)", key, v, updatedBy)
	s.retryFailedLocked(ctx)
	return Resolved{
		Spec: spec, Value: v, Source: SourceAdmin, Default: s.defaults[key],
		UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy,
	}, nil
}

// retryFailedLocked re-applies any key whose last apply failed (ApplyError),
// after some other setting changed — settings can depend on each other (the
// rampart engine needs the rampart URL), so fixing the dependency should heal
// the dependent without a reboot or a redundant re-save. Best-effort: a key
// that still fails keeps its error. Callers hold s.mu.
func (s *Service) retryFailedLocked(ctx context.Context) {
	if len(s.applyErrs) == 0 {
		return
	}
	overrides, err := s.st.WorkspaceSettings(ctx)
	if err != nil {
		return // keep existing errors; next change or boot retries
	}
	for _, key := range s.order {
		if _, failed := s.applyErrs[key]; !failed {
			continue
		}
		r := s.resolve(key, overrides)
		if err := s.apply(key, r.Value, r.Source == SourceAdmin); err == nil {
			delete(s.applyErrs, key)
			log.Printf("workspace settings: %s recovered and is now in effect", key)
		}
	}
}

// Reset deletes one override and re-applies the env-derived default. If the
// default cannot apply (e.g. resetting a rampart URL that the engine setting
// still needs), the deleted row is re-inserted (compensation) so the DB never
// disagrees with the running system across a restart.
func (s *Service) Reset(ctx context.Context, key, updatedBy string) (Resolved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[key]
	if !ok {
		return Resolved{}, ErrUnknownKey
	}
	// Snapshot the row for compensation before deleting it.
	overrides, err := s.st.WorkspaceSettings(ctx)
	if err != nil {
		return Resolved{}, fmt.Errorf("reset %s: %w", key, err)
	}
	prev, hadRow := overrides[key]
	if err := s.st.DeleteWorkspaceSetting(ctx, key); err != nil {
		return Resolved{}, fmt.Errorf("reset %s: %w", key, err)
	}
	if err := s.apply(key, s.defaults[key], false); err != nil {
		if hadRow {
			if _, restoreErr := s.st.SetWorkspaceSetting(ctx, key, prev.Value, prev.UpdatedBy); restoreErr != nil {
				return Resolved{}, fmt.Errorf("%w (and restoring the previous value failed: %w)", err, restoreErr)
			}
		}
		return Resolved{}, err
	}
	delete(s.applyErrs, key)
	log.Printf("workspace settings: %s reset to default %s (by %s)", key, s.defaults[key], updatedBy)
	s.retryFailedLocked(ctx)
	return Resolved{Spec: spec, Value: s.defaults[key], Source: SourceDefault, Default: s.defaults[key]}, nil
}

// ApplyAll pushes every setting's effective value into the running system —
// called once at boot after the store is ready. Defaults apply with
// override=false so env-shadowing hooks clear rather than pin the boot env
// value. Overrides that fail to apply are collected (not short-circuited) so
// one bad hook can't stop the rest; the caller decides how to degrade.
func (s *Service) ApplyAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	overrides, err := s.st.WorkspaceSettings(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLoadFailed, err)
	}
	var errs []string
	for _, key := range s.order {
		r := s.resolve(key, overrides)
		if err := s.apply(key, r.Value, r.Source == SourceAdmin); err != nil {
			s.applyErrs[key] = err.Error()
			errs = append(errs, fmt.Sprintf("%s: %v", key, err))
		} else {
			delete(s.applyErrs, key)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("apply workspace settings: %s", strings.Join(errs, "; "))
	}
	return nil
}

// apply runs the key's hook. Callers hold s.mu.
func (s *Service) apply(key, value string, override bool) error {
	hook := s.hooks[key]
	if hook == nil {
		return fmt.Errorf("no apply hook wired for %s", key)
	}
	if err := hook(value, override); err != nil {
		return fmt.Errorf("apply %s: %w", key, err)
	}
	return nil
}
