package main

// Admin-managed workspace feature settings (internal/settings): this file is
// the boot-side wiring — the env-derived DEFAULTS and the per-key APPLY hooks
// the service pushes effective values through. Everything registered here
// takes effect live (consumers re-read per turn / per run / per tool call);
// see docs/ADMIN-SETTINGS.md for the registry contract and the inventory of
// settings that deliberately stay env-only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/notifyadmin"
	"github.com/ElcanoTek/fleet/internal/piiredact"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/settings"
	"github.com/ElcanoTek/fleet/internal/store"
)

// buildWorkspaceSettings constructs the settings service over the chat store.
// Defaults are snapshotted from the env-derived Config BEFORE any override
// applies, so "Reset to default" always reverts to what this deployment's env
// file configures.
func buildWorkspaceSettings(cfg *config.Config, st *store.Store) (*settings.Service, *piiRedactorState, error) {
	defaults := map[string]string{
		"pii_redaction_mode":                defaultPIIRedactionMode(cfg),
		"pii_redaction_engine":              defaultPIIRedactionEngine(cfg),
		"pii_rampart_url":                   cfg.PIIRampartURL,
		"tool_disclosure_threshold":         strconv.Itoa(agentcore.EnvToolDisclosureThreshold()),
		"max_tool_output_bytes":             strconv.Itoa(agentcore.EnvMaxToolOutputBytes()),
		"phone_a_friend_enabled":            strconv.FormatBool(cfg.PhoneAFriendEnabled),
		"subagents_enabled":                 strconv.FormatBool(cfg.SubagentsEnabled),
		"memory_autoindex_enabled":          strconv.FormatBool(cfg.MemoryAutoIndexEnabled),
		"error_analysis_enabled":            strconv.FormatBool(cfg.ErrorAnalysisEnabled),
		"auto_title_enabled":                strconv.FormatBool(cfg.AutoTitle),
		"connector_recommendations_enabled": strconv.FormatBool(cfg.ConnectorRecommendationsEnabled),
		"context_handles_enabled":           strconv.FormatBool(cfg.ContextHandlesEnabled),
	}
	pii := newPIIRedactorState(cfg)
	hooks := map[string]settings.ApplyFunc{
		// The three PII keys feed one redactor: each hook updates its slice of
		// the shared state and rebuilds. Rebuild errors (rampart without a URL)
		// surface to the admin and roll the write back.
		"pii_redaction_mode":   pii.applyMode,
		"pii_redaction_engine": pii.applyEngine,
		"pii_rampart_url":      pii.applyURL,
		// The two agentcore knobs shadow a PER-USE env read: with no admin
		// override the holder is CLEARED (not pinned to the boot env value), so
		// an env-file edit + #286 reload — or any process-env change — keeps
		// taking effect live exactly as before this feature existed.
		"tool_disclosure_threshold":         applyEnvShadowedInt(agentcore.SetToolDisclosureThreshold, 0),
		"max_tool_output_bytes":             applyEnvShadowedInt(agentcore.SetMaxToolOutputBytes, -1),
		"phone_a_friend_enabled":            applyBoolSetting(cfg.SetPhoneAFriendEnabled),
		"subagents_enabled":                 applyBoolSetting(cfg.SetSubagentsEnabled),
		"memory_autoindex_enabled":          applyBoolSetting(cfg.SetMemoryAutoIndexEnabled),
		"error_analysis_enabled":            applyBoolSetting(cfg.SetErrorAnalysisEnabled),
		"auto_title_enabled":                applyBoolSetting(cfg.SetAutoTitle),
		"connector_recommendations_enabled": applyBoolSetting(cfg.SetConnectorRecommendationsEnabled),
		"context_handles_enabled":           applyBoolSetting(cfg.SetContextHandlesEnabled),
	}
	svc, err := settings.NewService(st, defaults, hooks)
	return svc, pii, err
}

// defaultPIIRedactionMode mirrors configurePIIRedaction's env semantics as a
// registry value: disabled → "off"; enabled with an invalid or "off" mode →
// "redact" (a misconfiguration keeps the control ON, #450).
func defaultPIIRedactionMode(cfg *config.Config) string {
	if !cfg.PIIRedactionEnabled {
		return string(piiredact.ModeOff)
	}
	mode, err := piiredact.ParseMode(cfg.PIIRedactionMode)
	if err != nil || mode == piiredact.ModeOff {
		return string(piiredact.ModeRedact)
	}
	return string(mode)
}

// defaultPIIRedactionEngine maps FLEET_PII_REDACTION_ENGINE onto the registry
// value: "rampart" only when explicitly asked for AND a service URL is set;
// anything else (unset, junk, or rampart with no URL — which could never
// activate and must not fail redaction OPEN at boot) is the deterministic
// "pattern" engine. The URL-less degrade is logged by configurePIIRedaction.
func defaultPIIRedactionEngine(cfg *config.Config) string {
	if strings.EqualFold(strings.TrimSpace(cfg.PIIRedactionEngine), "rampart") {
		if strings.TrimSpace(cfg.PIIRampartURL) == "" {
			return "pattern"
		}
		return "rampart"
	}
	return "pattern"
}

// piiRedactorState is the shared state behind the three PII settings hooks
// (mode, engine, rampart URL). Any change rebuilds the ONE process-wide
// redactor from the full trio, so the hooks compose regardless of apply
// order. The settings Service serializes hook calls, but the probe endpoint
// reads concurrently — hence the mutex.
type piiRedactorState struct {
	mu     sync.Mutex
	mode   string
	engine string
	url    string
	// current is what rebuild installed — kept for the admin Test probe, which
	// must exercise exactly what tool calls run through.
	current piiredact.Redactor
}

func newPIIRedactorState(cfg *config.Config) *piiRedactorState {
	return &piiRedactorState{
		mode:   defaultPIIRedactionMode(cfg),
		engine: defaultPIIRedactionEngine(cfg),
		url:    cfg.PIIRampartURL,
	}
}

// rebuild is the exported-shape wrapper for boot-time installs.
func (p *piiRedactorState) rebuild() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rebuildLocked()
}

func (p *piiRedactorState) applyMode(value string, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.mode
	p.mode = value
	if err := p.rebuildLocked(); err != nil {
		p.mode = prev
		return err
	}
	return nil
}

func (p *piiRedactorState) applyEngine(value string, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.engine
	p.engine = value
	if err := p.rebuildLocked(); err != nil {
		p.engine = prev
		return err
	}
	return nil
}

func (p *piiRedactorState) applyURL(value string, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.url
	p.url = value
	if err := p.rebuildLocked(); err != nil {
		p.url = prev
		return err
	}
	return nil
}

// rebuildLocked constructs and installs the redactor for the current trio.
// "off" installs nil (the tool-output pass is a byte-for-byte no-op); the
// rampart engine without a URL is a configuration error the admin sees (and
// the settings write rolls back). Callers hold p.mu.
func (p *piiRedactorState) rebuildLocked() error {
	mode, err := piiredact.ParseMode(p.mode)
	if err != nil {
		return err
	}
	// Engine/URL consistency holds REGARDLESS of mode: accepting
	// engine=rampart with no URL while the mode is off would boobytrap the
	// later mode change with a confusing rampart error (observed live).
	if p.engine == "rampart" && strings.TrimSpace(p.url) == "" {
		return fmt.Errorf("the rampart engine needs a detection service URL — set pii_rampart_url (or FLEET_PII_RAMPART_URL) first; see docs/PII-REDACTION.md for deploying the service")
	}
	if mode == piiredact.ModeOff {
		p.current = nil
		agentcore.SetPIIRedactor(nil)
		return nil
	}
	var r piiredact.Redactor
	if p.engine == "rampart" {
		r = piiredact.NewRampart(mode, p.url)
	} else {
		r = piiredact.New(mode)
	}
	p.current = r
	agentcore.SetPIIRedactor(r)
	return nil
}

// Probe runs the CURRENT redactor over a fixed synthetic sample for the admin
// "Test detection" button (POST /admin/pii-redaction/test). For the rampart
// engine it goes through ProbeService, so a dead detection service reports as
// a failure instead of silently falling back — surfacing connectivity is the
// point of the button. The sample is synthetic; the response carries kinds +
// counts and the redacted preview, never operator data.
func (p *piiRedactorState) Probe(ctx context.Context) httpapi.PIIProbeResult {
	p.mu.Lock()
	current := p.current
	mode, engine := p.mode, p.engine
	p.mu.Unlock()

	res := httpapi.PIIProbeResult{Mode: mode, Engine: engine}
	if current == nil {
		res.Detail = "PII redaction is off — set a mode to test detection"
		return res
	}
	const sample = "Contact Alex Rivera at alex.rivera@example.com or (415) 555-0134, SSN 123-45-6789, 12 Main St, Springfield."
	start := time.Now()
	var out piiredact.Result
	if rr, ok := current.(*piiredact.RampartRedactor); ok {
		out, err := rr.ProbeService(ctx, sample)
		res.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			res.Detail = fmt.Sprintf("rampart service unreachable: %v (tool calls fall back to the pattern engine)", err)
			return res
		}
		res.OK = true
		res.Detail = out.Summary()
		res.Redacted = out.Text
		return res
	}
	out = current.Redact(sample)
	res.LatencyMS = time.Since(start).Milliseconds()
	res.OK = true
	res.Detail = out.Summary()
	res.Redacted = out.Text
	return res
}

// applyBoolSetting adapts a config live setter to an ApplyFunc. The value was
// registry-validated, so a parse failure is a wiring bug worth surfacing.
// Config fields don't re-read the env after boot, so applying the default is
// the same as applying an override of the same value.
func applyBoolSetting(set func(bool)) settings.ApplyFunc {
	return func(value string, _ bool) error {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("not a boolean: %q", value)
		}
		set(b)
		return nil
	}
}

// applyEnvShadowedInt adapts an agentcore override holder to an ApplyFunc.
// These holders SHADOW an env var that the runtime otherwise re-reads per use,
// so a default (override=false) must CLEAR the holder with the given sentinel
// — pinning the boot env value would freeze later env/reload changes that
// worked before this feature existed.
func applyEnvShadowedInt(set func(int), clearSentinel int) settings.ApplyFunc {
	return func(value string, override bool) error {
		if !override {
			set(clearSentinel)
			return nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("not an integer: %q", value)
		}
		set(n)
		return nil
	}
}

// appendWorkspaceSettingsOption builds the settings service, pushes every
// effective value (override or env default) into the running system before the
// listeners come up, and appends the httpapi option that serves the admin
// endpoints. Failure degrades rather than bricking boot, mirroring the
// LLM-provider overlay — but it degrades to the 501 panel, never to a lying
// one: if the boot apply cannot load the override rows (after a short retry),
// the endpoints are NOT registered, because a panel that reports an override
// as in effect when it never applied would be worse than no panel. Env-derived
// behavior serves either way. A construction failure (a registry key without a
// default or hook — a programming error, also caught by
// TestBuildWorkspaceSettingsCoversRegistry) degrades the same way.
func appendWorkspaceSettingsOption(opts []httpapi.Option, cfg *config.Config, st *store.Store) []httpapi.Option {
	svc, pii, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		log.Printf("workspace settings: DISABLED — service construction failed (this is a wiring bug): %v", err)
		return opts
	}
	// The store just served migrations, so a load failure here is a transient
	// blip at worst — retry briefly. Only a LOAD failure degrades the panel to
	// 501 (it cannot render truthfully): a per-key apply failure keeps the
	// panel up with that row carrying its error (Resolved.ApplyError), so the
	// admin can fix or Reset it from the UI instead of being stranded.
	var applyErr error
	for attempt := 0; attempt < 3; attempt++ {
		applyErr = svc.ApplyAll(context.Background())
		if applyErr == nil || !errors.Is(applyErr, settings.ErrLoadFailed) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if applyErr != nil && errors.Is(applyErr, settings.ErrLoadFailed) {
		log.Printf("workspace settings: DISABLED — boot load failed, serving env-derived defaults (admin overrides NOT in effect): %v", applyErr)
		return opts
	}
	if applyErr != nil {
		log.Printf("workspace settings: some overrides NOT in effect (env-derived behavior serves for those keys; fix or Reset them from the admin panel): %v", applyErr)
	}
	// The PII probe rides with the panel: it exercises the same state the
	// hooks maintain, so it is only honest when the panel is live too.
	return append(opts,
		httpapi.WithWorkspaceSettings(svc),
		httpapi.WithPIIRedactionProbe(pii.Probe))
}

// gatedErrorAnalyzer wraps the Manager's post-failure diagnosis (#317) so the
// admin error-analysis toggle is consulted LIVE at failure time — not frozen
// into a nil-vs-wired seam at boot. Disabled → (nil, nil), which the runner
// treats as "nothing to persist".
type gatedErrorAnalyzer struct {
	cfg *config.Config
	mgr *agent.Manager
}

func (g gatedErrorAnalyzer) AnalyzeTaskFailure(ctx context.Context, taskPrompt, errMsg, sessionTail string) (json.RawMessage, error) {
	if !g.cfg.LiveErrorAnalysisEnabled() {
		return nil, nil
	}
	return g.mgr.AnalyzeTaskFailure(ctx, taskPrompt, errMsg, sessionTail)
}

// errorAnalyzerFor returns the runner's post-failure diagnosis seam (#317),
// gated live on the error_analysis_enabled setting (admin override > env).
func errorAnalyzerFor(cfg *config.Config, mgr *agent.Manager) runner.ErrorAnalyzer {
	if cfg == nil || mgr == nil {
		return nil
	}
	return gatedErrorAnalyzer{cfg: cfg, mgr: mgr}
}

// appendNotifySettingsOption wires the admin Notifications panel
// (internal/notifyadmin): a persisted admin row hot-swaps the shared
// notifier's config at boot; edits keep swapping it live. Same degrade posture
// as the Features panel — if the boot apply can't read the row, the endpoints
// are NOT registered (501 panel, never a lying one) and the env-derived config
// already in the notifier keeps serving.
func appendNotifySettingsOption(opts []httpapi.Option, st *store.Store, notifier *notify.Notifier) []httpapi.Option {
	svc := notifyadmin.NewService(st, notify.Load(), notifier)
	if err := svc.ApplyBoot(context.Background()); err != nil {
		log.Printf("notify settings: DISABLED — boot apply failed, serving env-derived config (admin settings NOT in effect): %v", err)
		return opts
	}
	return append(opts, httpapi.WithNotifySettings(svc))
}
