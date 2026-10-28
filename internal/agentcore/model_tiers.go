package agentcore

import (
	"strings"
	"sync"
)

// Live model-tier holders (admin settings "default_model"/"advanced_model").
//
// DefaultCoreModel / DefaultMaxModel stay the compiled-in tier slugs, but a
// deployment must not need a code push to move a tier (the pain that filed
// the setting: every lab refresh meant editing constants in two repos). The
// effective slugs therefore live here behind a RWMutex, seeded from the
// constants, and cmd/fleet's workspace-settings hooks push the env-derived
// default at boot and any admin override live. Consumers that mean "the
// CURRENT tier" read CurrentDefaultModel/CurrentAdvancedModel; the constants
// remain only as the built-in seed and for tests that pin the shipped values.
//
// The third slot, the task tier, is what an Operations Center task runs on by
// default. It has no compiled-in constant of its own: unset, it FOLLOWS the
// advanced tier, so a deployment names its strong model once and both chat
// escalation and scheduled work pick it up. Chat is where people pick fast
// models to play with; a scheduled task is a long unattended run, which is the
// job the strong model is for (docs/TASK-MODEL-TIER.md).
var modelTiers = struct {
	mu       sync.RWMutex
	def, adv string
	// task is the task-tier override; "" means "follow def".
	task string
}{def: DefaultCoreModel, adv: DefaultMaxModel}

// CurrentDefaultModel returns the effective default frontier-tier slug — what a
// new conversation starts on and what the web pins first in its picker.
func CurrentDefaultModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.def
}

// CurrentAdvancedModel returns the effective fallback frontier-tier slug — the
// second pinned picker slot and suggest_advanced_model target.
func CurrentAdvancedModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.adv
}

// CurrentFallbackModel returns the effective fallback frontier-tier slug
// (alias of CurrentAdvancedModel).
func CurrentFallbackModel() string {
	return CurrentAdvancedModel()
}

// CurrentTaskModel returns the effective task-tier slug: what a scheduled task
// with no pinned model runs on and what the Operations Center create form
// pre-fills. It is the task-tier override when one is set, otherwise the
// CURRENT default frontier tier (Opus 5.5 by default), so it is never empty.
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

// SetAdvancedModel installs the effective fallback frontier-tier slug. Empty
// reverts to the compiled-in DefaultMaxModel, mirroring SetDefaultModel.
func SetAdvancedModel(slug string) {
	v := strings.TrimSpace(slug)
	if v == "" {
		v = DefaultMaxModel
	}
	modelTiers.mu.Lock()
	modelTiers.adv = v
	modelTiers.mu.Unlock()
}

// SetFallbackModel installs the effective fallback frontier-tier slug (alias
// of SetAdvancedModel).
func SetFallbackModel(slug string) {
	SetAdvancedModel(slug)
}

// SetTaskModel installs the task-tier override. Empty clears it, and the task
// tier then follows the default tier (CurrentTaskModel).
func SetTaskModel(slug string) {
	v := strings.TrimSpace(slug)
	modelTiers.mu.Lock()
	modelTiers.task = v
	modelTiers.mu.Unlock()
}
