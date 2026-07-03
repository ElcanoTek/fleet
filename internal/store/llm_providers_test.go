package store

import (
	"context"
	"errors"
	"testing"
)

// Admin-managed LLM providers (migration 034). The load-bearing assertions:
// keys are write-only (list reports has_api_key, never a value), the
// resolver-building read decrypts, nil-key updates leave the stored key
// untouched, and name/type validation rejects junk before it reaches SQL.

func TestLLMProviderCRUDAndKeySealing(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()

	key := "sk-ant-test-123"
	created, err := s.CreateLLMProvider(ctx, LLMProviderInput{
		Name:    "Claude-Direct", // normalized to lowercase
		Type:    "anthropic",
		Models:  []string{" claude-sonnet-4-5 ", ""},
		Enabled: true,
		APIKey:  &key,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "claude-direct" || !created.HasAPIKey || created.Type != "anthropic" {
		t.Fatalf("created = %+v, want normalized name + has_api_key", created)
	}
	if len(created.Models) != 1 || created.Models[0] != "claude-sonnet-4-5" {
		t.Fatalf("models = %v, want trimmed single entry", created.Models)
	}

	// The UI-facing list never carries the key value.
	list, err := s.ListLLMProviders(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || !list[0].HasAPIKey {
		t.Fatalf("list = %+v, want one row with has_api_key", list)
	}

	// The resolver-building read decrypts the sealed key.
	cfgs, err := s.LLMProviderConfigs(ctx)
	if err != nil {
		t.Fatalf("configs: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].APIKey != key {
		t.Fatalf("configs[0].APIKey = %q, want the stored key", cfgs[0].APIKey)
	}

	// nil APIKey on update = leave the stored key untouched.
	updated, err := s.UpdateLLMProvider(ctx, created.ID, LLMProviderInput{
		Name: "claude-direct", Type: "anthropic",
		Models: []string{"claude-sonnet-4-5", "claude-opus-4-8"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.HasAPIKey || len(updated.Models) != 2 {
		t.Fatalf("updated = %+v, want key kept + 2 models", updated)
	}
	cfgs, _ = s.LLMProviderConfigs(ctx)
	if cfgs[0].APIKey != key {
		t.Fatalf("key after nil-key update = %q, want unchanged", cfgs[0].APIKey)
	}

	// Explicit "" clears the key.
	empty := ""
	updated, err = s.UpdateLLMProvider(ctx, created.ID, LLMProviderInput{
		Name: "claude-direct", Type: "anthropic",
		Models: updated.Models, Enabled: true, APIKey: &empty,
	})
	if err != nil {
		t.Fatalf("clear key: %v", err)
	}
	if updated.HasAPIKey {
		t.Fatalf("key not cleared: %+v", updated)
	}

	// Disabled rows drop out of the resolver-building read but stay listed.
	if _, err := s.UpdateLLMProvider(ctx, created.ID, LLMProviderInput{
		Name: "claude-direct", Type: "anthropic", Models: updated.Models, Enabled: false,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	cfgs, _ = s.LLMProviderConfigs(ctx)
	if len(cfgs) != 0 {
		t.Fatalf("configs after disable = %d rows, want 0", len(cfgs))
	}
	list, _ = s.ListLLMProviders(ctx)
	if len(list) != 1 {
		t.Fatalf("list after disable = %d rows, want 1", len(list))
	}

	if err := s.DeleteLLMProvider(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteLLMProvider(ctx, created.ID); !errors.Is(err, ErrLLMProviderNotFound) {
		t.Fatalf("double delete = %v, want ErrLLMProviderNotFound", err)
	}
}

func TestLLMProviderValidation(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()

	for _, tc := range []LLMProviderInput{
		{Name: "has/slash", Type: "openai", Enabled: true},
		{Name: "", Type: "openai", Enabled: true},
		{Name: "ok", Type: "grok", Enabled: true},
		{Name: "badurl", Type: "openai", BaseURL: "file:///etc/passwd", Enabled: true},
		{Name: "badurl2", Type: "openai", BaseURL: "not a url at all://", Enabled: true},
		{Name: "badurl3", Type: "openai", BaseURL: "https://user:pw@host/v1", Enabled: true},
	} {
		if _, err := s.CreateLLMProvider(ctx, tc); err == nil {
			t.Errorf("create %+v: want validation error, got nil", tc)
		}
	}

	// The name is the routing prefix baked into saved model slugs — immutable.
	row, err := s.CreateLLMProvider(ctx, LLMProviderInput{Name: "fixed", Type: "ollama", Enabled: true})
	if err != nil {
		t.Fatalf("create fixed: %v", err)
	}
	if _, err := s.UpdateLLMProvider(ctx, row.ID, LLMProviderInput{Name: "renamed", Type: "ollama", Enabled: true}); err == nil {
		t.Fatalf("rename: want error, got nil")
	}

	// Duplicate names are rejected via the unique constraint.
	if _, err := s.CreateLLMProvider(ctx, LLMProviderInput{Name: "dup", Type: "ollama", Enabled: true}); err != nil {
		t.Fatalf("create dup 1: %v", err)
	}
	if _, err := s.CreateLLMProvider(ctx, LLMProviderInput{Name: "dup", Type: "ollama", Enabled: true}); err == nil {
		t.Fatalf("create dup 2: want already-exists error")
	}
}

// A keyed create without a cipher configured must fail closed, never store
// plaintext.
func TestLLMProviderKeyRequiresCipher(t *testing.T) {
	s := newTestStore(t) // no cipher
	key := "sk-secret"
	if _, err := s.CreateLLMProvider(context.Background(), LLMProviderInput{
		Name: "nokey", Type: "openai", Enabled: true, APIKey: &key,
	}); err == nil {
		t.Fatalf("keyed create without cipher: want error, got nil")
	}
}
