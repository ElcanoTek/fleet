package agentcore

import "strings"

// MergeLLMProviders composes the model resolver's routing table from its two
// sources (admin-managed LLM providers, #289 follow-on):
//
//   - base: the client bundle's providers: block (env-keyed, boot-resolved) —
//     or, when the bundle declares none and an OpenRouter key is set, the
//     implicit single catch-all OpenRouter provider (the historical default).
//   - admin: the DB-backed, admin-edited rows.
//
// Admin rows OVERLAY the base: a row whose name matches a base provider
// replaces it in place (so an admin can rotate the bundle's OpenRouter key or
// re-point "openrouter" wholesale); otherwise the row is appended. Appending
// keeps the base catch-all's precedence for unlisted slugs, while an admin
// provider's explicit models list still wins over any catch-all (a listed
// model always beats a catch-all regardless of order — see selectProvider).
//
// The result may be empty (no bundle providers, no env key, no admin rows) —
// callers keep the existing "OPENROUTER_API_KEY required" boot error for that.
func MergeLLMProviders(base []ProviderConfig, admin []ProviderConfig, openRouterKey string) []ProviderConfig {
	merged := make([]ProviderConfig, 0, len(base)+len(admin)+1)
	var fallbackProviders []string
	for i := range base {
		if len(base[i].FallbackProviders) > 0 {
			fallbackProviders = append([]string(nil), base[i].FallbackProviders...)
			break
		}
	}
	if len(base) > 0 {
		merged = append(merged, base...)
	} else if strings.TrimSpace(openRouterKey) != "" {
		merged = append(merged, ProviderConfig{
			Name:   "openrouter",
			Type:   ProviderTypeOpenRouter,
			APIKey: openRouterKey,
		})
	}
	for _, a := range admin {
		replaced := false
		for i := range merged {
			if merged[i].Name == a.Name {
				merged[i] = a
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	for i := range merged {
		merged[i].FallbackProviders = append([]string(nil), fallbackProviders...)
	}
	return merged
}

// ValidateProvider eagerly builds the provider client handle (no network) and
// reports the construction error, so admin edits are checked BEFORE they are
// persisted — a row that cannot build must never reach the routing table (a
// broken row would otherwise fail every future resolver rebuild).
func ValidateProvider(cfg ProviderConfig, headers ProviderHeaders) error {
	_, err := buildProvider(cfg, headers)
	return err
}
