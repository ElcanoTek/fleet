package agentcore

import (
	"strings"
	"sync"
)

// Live model-tier holders (admin settings "default_model"/"fallback_model",
// with "advanced_model" retained as the historical alias key).
//
// DefaultCoreModel / DefaultFallbackModel stay the compiled-in tier slugs, but a
// deployment must not need a code push to move a tier. The effective slugs
// therefore live here behind a RWMutex, seeded from the constants, and
// cmd/fleet's workspace-settings hooks push the env-derived default at boot and
// any admin override live. Consumers that mean "the CURRENT tier" read
// CurrentDefaultModel/CurrentFallbackModel; the constants remain only as the
// built-in seed and for tests that pin the shipped values.
//
// Fleet puts the smartest frontier model first as the Default Model
// (DefaultCoreModel = google/gemini-4-argon) so users get the best experience
// out of the box, pairs it with a second smart frontier Fallback Model from a
// different provider family (DefaultFallbackModel = anthropic/claude-opus-5.5)
// for automatic task/patrol failover and chat fallback, and exposes operator-
// pinned free/cost-efficient models (`FLEET_PINNED_MODELS`) when users want to
// optimize spend.
//
// The third slot, the task tier, is what an Operations Center task runs on by
// default. Unset, it FOLLOWS the default frontier tier (CurrentDefaultModel),
// so both interactive chat and scheduled work run on the primary frontier model
// unless overridden (docs/TASK-MODEL-TIER.md).
var modelTiers = struct {
	mu            sync.RWMutex
	def, fallback string
	// task is the task-tier override; "" means "follow def".
	task string
}{def: DefaultCoreModel, fallback: DefaultFallbackModel}

// CurrentDefaultModel returns the effective default frontier-tier slug — what a
// new conversation starts on and what the web pins first in its picker.
func CurrentDefaultModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.def
}

// CurrentFallbackModel returns the effective fallback frontier-tier slug — the
// second pinned picker slot, automatic task/patrol fallback, and
// suggest_advanced_model target.
func CurrentFallbackModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.fallback
}

// CurrentAdvancedModel is the historical alias for CurrentFallbackModel.
func CurrentAdvancedModel() string {
	return CurrentFallbackModel()
}

// CurrentTaskModel returns the effective task-tier slug: what a scheduled task
// with no pinned model runs on and what the Operations Center create form
// pre-fills. It is the task-tier override when one is set, otherwise the
// CURRENT default frontier tier (Gemini 4 Argon by default), so it is never empty.
func CurrentTaskModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	if modelTiers.task != "" {
		return modelTiers.task
	}
	return modelTiers.def
}

// SetDefaultModel installs the effective default frontier-tier slug. An empty
// value reverts to the compiled-in DefaultCoreModel, so a hook applying an
// unset env default cannot blank the tier.
func SetDefaultModel(slug string) {
	v := strings.TrimSpace(slug)
	if v == "" {
		v = DefaultCoreModel
	}
	modelTiers.mu.Lock()
	modelTiers.def = v
	modelTiers.mu.Unlock()
}

// SetFallbackModel installs the effective fallback frontier-tier slug. Empty
// reverts to the compiled-in DefaultFallbackModel, mirroring SetDefaultModel.
func SetFallbackModel(slug string) {
	v := strings.TrimSpace(slug)
	if v == "" {
		v = DefaultFallbackModel
	}
	modelTiers.mu.Lock()
	modelTiers.fallback = v
	modelTiers.mu.Unlock()
}

// SetAdvancedModel is the historical alias for SetFallbackModel.
func SetAdvancedModel(slug string) {
	SetFallbackModel(slug)
}

// SetTaskModel installs the task-tier override. Empty clears it, and the task
// tier then follows the default tier (CurrentTaskModel).
func SetTaskModel(slug string) {
	v := strings.TrimSpace(slug)
	modelTiers.mu.Lock()
	modelTiers.task = v
	modelTiers.mu.Unlock()
}
