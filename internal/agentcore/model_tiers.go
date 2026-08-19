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
var modelTiers = struct {
	mu       sync.RWMutex
	def, adv string
}{def: DefaultCoreModel, adv: DefaultMaxModel}

// CurrentDefaultModel returns the effective everyday-tier slug — what a new
// conversation starts on and what the web pins first in its picker.
func CurrentDefaultModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.def
}

// CurrentAdvancedModel returns the effective strong-tier slug — the
// suggest_advanced_model escalation target.
func CurrentAdvancedModel() string {
	modelTiers.mu.RLock()
	defer modelTiers.mu.RUnlock()
	return modelTiers.adv
}

// SetDefaultModel installs the effective everyday-tier slug. An empty value
// reverts to the compiled-in DefaultCoreModel, so a hook applying an unset
// env default cannot blank the tier.
func SetDefaultModel(slug string) {
	v := strings.TrimSpace(slug)
	if v == "" {
		v = DefaultCoreModel
	}
	modelTiers.mu.Lock()
	modelTiers.def = v
	modelTiers.mu.Unlock()
}

// SetAdvancedModel installs the effective strong-tier slug. Empty reverts to
// the compiled-in DefaultMaxModel, mirroring SetDefaultModel.
func SetAdvancedModel(slug string) {
	v := strings.TrimSpace(slug)
	if v == "" {
		v = DefaultMaxModel
	}
	modelTiers.mu.Lock()
	modelTiers.adv = v
	modelTiers.mu.Unlock()
}
