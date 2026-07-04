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

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
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
		"pii_redaction_mode":                applyPIIRedactionMode,
		"tool_disclosure_threshold":         applyIntSetting(agentcore.SetToolDisclosureThreshold),
		"max_tool_output_bytes":             applyIntSetting(agentcore.SetMaxToolOutputBytes),
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
// next tool call.
func applyPIIRedactionMode(value string) error {
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
func applyBoolSetting(set func(bool)) settings.ApplyFunc {
	return func(value string) error {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("not a boolean: %q", value)
		}
		set(b)
		return nil
	}
}

// applyIntSetting adapts an int live setter to an ApplyFunc.
func applyIntSetting(set func(int)) settings.ApplyFunc {
	return func(value string) error {
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
// LLM-provider overlay: a construction failure (a registry key without a
// default or hook — a programming error, also caught by
// TestBuildWorkspaceSettingsCoversRegistry) leaves the endpoints answering 501
// with a loud log; a boot-apply failure keeps env-derived behavior for the
// affected keys.
func appendWorkspaceSettingsOption(opts []httpapi.Option, cfg *config.Config, st *store.Store) []httpapi.Option {
	svc, err := buildWorkspaceSettings(cfg, st)
	if err != nil {
		log.Printf("workspace settings: DISABLED — service construction failed (this is a wiring bug): %v", err)
		return opts
	}
	if err := svc.ApplyAll(context.Background()); err != nil {
		log.Printf("workspace settings: boot apply degraded to env defaults for some keys: %v", err)
	}
	return append(opts, httpapi.WithWorkspaceSettings(svc))
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
