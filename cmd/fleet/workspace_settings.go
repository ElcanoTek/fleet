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
	"fmt"
	"log"
	"strconv"
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
func buildWorkspaceSettings(cfg *config.Config, st *store.Store) (*settings.Service, error) {
	defaults := map[string]string{
		"pii_redaction_mode":                defaultPIIRedactionMode(cfg),
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
	hooks := map[string]settings.ApplyFunc{
		"pii_redaction_mode": applyPIIRedactionMode,
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
	return settings.NewService(st, defaults, hooks)
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

// applyPIIRedactionMode hot-swaps the process-wide PII redactor (#450). "off"
// installs nil (the tool-output pass becomes a byte-for-byte no-op); any other
// validated mode installs a fresh PatternRedactor. Takes effect on the very
// next tool call. The redactor holder is not env-shadowed (nothing re-reads
// the env after boot), so override and default apply identically.
func applyPIIRedactionMode(value string, _ bool) error {
	mode, err := piiredact.ParseMode(value)
	if err != nil {
		return err
	}
	if mode == piiredact.ModeOff {
		agentcore.SetPIIRedactor(nil)
		return nil
	}
	agentcore.SetPIIRedactor(piiredact.New(mode))
	return nil
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
	svc, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		log.Printf("workspace settings: DISABLED — service construction failed (this is a wiring bug): %v", err)
		return opts
	}
	// The store just served migrations, so a load failure here is a transient
	// blip at worst — retry briefly before degrading.
	var applyErr error
	for attempt := 0; attempt < 3; attempt++ {
		if applyErr = svc.ApplyAll(context.Background()); applyErr == nil {
			return append(opts, httpapi.WithWorkspaceSettings(svc))
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	log.Printf("workspace settings: DISABLED — boot apply failed, serving env-derived defaults (admin overrides NOT in effect): %v", applyErr)
	return opts
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
