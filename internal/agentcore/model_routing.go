package agentcore

import (
	"fmt"
	"slices"
	"strings"
)

// ModelProviderInfo is the public routing table, deliberately separate from
// ProviderConfig: credentials and endpoint URLs must never reach the picker.
type ModelProviderInfo struct {
	Name     string       `json:"name"`
	Type     ProviderType `json:"type"`
	Models   []string     `json:"models"`
	CatchAll bool         `json:"catch_all"`
}

// ModelProviders snapshots the ACTIVE resolver, including bundle/env providers
// and successfully applied admin overlays. Reading DB rows instead advertises
// unapplied edits and omits bundle-only deployments.
func (r *ModelResolver) ModelProviders() []ModelProviderInfo {
	out := make([]ModelProviderInfo, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, ModelProviderInfo{
			Name: p.Name, Type: p.Type, Models: slices.Clone(p.Models), CatchAll: len(p.Models) == 0,
		})
	}
	return out
}

// CheckModelRoute checks local routing only, without loading a model or calling
// its provider. It uses the same precedence as execution, not a public catalog.
func (r *ModelResolver) CheckModelRoute(slug string) (ProviderType, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", fmt.Errorf("choose a model from the workspace model picker")
	}
	p, _, err := selectProvider(r.providers, slug)
	if err != nil {
		return "", fmt.Errorf("%w; choose a workspace model using <provider-name>/<model>, or ask an admin to configure its provider in Settings → Admin → Model providers", err)
	}
	return p.Type, nil
}

// CatalogModelSlug returns the underlying OpenRouter catalog identifier for a
// routed model. Native/gateway routes keep their identity: stripping a prefix
// from those would borrow another provider's price or context metadata.
func (r *ModelResolver) CatalogModelSlug(slug string) string {
	p, model, err := selectProvider(r.providers, strings.TrimSpace(slug))
	if err == nil && p.Type == ProviderTypeOpenRouter {
		return model
	}
	return slug
}
