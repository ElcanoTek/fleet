package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// Admin-managed LLM providers (migration 034, #289 follow-on). Rows here are
// merged with the client bundle's providers: block into the agentcore model
// resolver's routing table; the merge itself lives in cmd/fleet (the store only
// persists rows).
//
// CRITICAL SECURITY INVARIANT: the API-key VALUE is write-only. It is sealed
// under the store cipher on write and decrypted ONLY by LLMProviderConfigs —
// the internal read used to (re)build the resolver. Every admin/UI-facing read
// goes through ListLLMProviders, which reports only whether a key is stored
// (HasAPIKey), never the value. This mirrors the MCP credential-account rule:
// secrets go in, never come back out.

// aadPurposeLLMKey domain-separates provider-key ciphertexts from the MCP
// OAuth secrets sealed under the same cipher. Bound to the row id (immutable),
// not the name (renamable), so a rename never orphans the ciphertext.
const aadPurposeLLMKey = "fleet:llm-provider-api-key:v1"

// ErrLLMProviderNotFound is returned when an id matches no row.
var ErrLLMProviderNotFound = errors.New("llm provider not found")

// llmProviderTypes mirrors agentcore's supported provider backends. Kept as a
// plain string set (not an agentcore import) so the store stays a leaf package.
var llmProviderTypes = map[string]bool{
	"openrouter": true,
	"anthropic":  true,
	"openai":     true,
	"ollama":     true,
}

// llmProviderNameRE — the name doubles as the "<name>/<model>" routing prefix,
// so it must be a clean slug with no "/" or whitespace.
var llmProviderNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LLMProvider is one admin-managed provider as every UI-facing read returns
// it: metadata + whether a key is stored, NEVER the key value.
type LLMProvider struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BaseURL   string   `json:"base_url"`
	Models    []string `json:"models"`
	Enabled   bool     `json:"enabled"`
	HasAPIKey bool     `json:"has_api_key"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// LLMProviderConfig is the resolver-building read: LLMProvider plus the
// decrypted key. Never serialize this type.
type LLMProviderConfig struct {
	LLMProvider
	APIKey string `json:"-"`
}

// LLMProviderInput carries a create or update. APIKey semantics: nil = leave
// the stored key unchanged (or none on create); non-nil = replace with the
// given value ("" clears it).
type LLMProviderInput struct {
	Name    string
	Type    string
	BaseURL string
	Models  []string
	Enabled bool
	APIKey  *string
}

func (in *LLMProviderInput) normalize() error {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if !llmProviderNameRE.MatchString(in.Name) {
		return fmt.Errorf("invalid provider name %q (lowercase slug, no slashes)", in.Name)
	}
	if !llmProviderTypes[in.Type] {
		return fmt.Errorf("invalid provider type %q (want openrouter|anthropic|openai|ollama)", in.Type)
	}
	if in.BaseURL != "" {
		// http/https only (no file:, unix:, …), a host, and no embedded
		// credentials. localhost/private hosts stay allowed — Ollama and
		// self-hosted gateways are the point of base_url.
		u, err := url.Parse(in.BaseURL)
		if err != nil {
			return fmt.Errorf("invalid base_url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("base_url must use http or https (got %q)", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("base_url must include a host")
		}
		if u.User != nil {
			return fmt.Errorf("base_url must not embed credentials")
		}
	}
	models := make([]string, 0, len(in.Models))
	seen := make(map[string]bool, len(in.Models))
	for _, m := range in.Models {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	in.Models = models
	return nil
}

const llmProviderColumns = `id, name, provider_type, base_url,
	(api_key_sealed IS NOT NULL) AS has_key, models, enabled, created_at, updated_at`

func scanLLMProvider(row interface{ Scan(...any) error }) (*LLMProvider, error) {
	var p LLMProvider
	var modelsJSON []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.HasAPIKey,
		&modelsJSON, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(modelsJSON, &p.Models); err != nil {
		return nil, fmt.Errorf("decode models for provider %s: %w", p.ID, err)
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	return &p, nil
}

// ListLLMProviders returns every provider row (enabled and disabled, so the
// admin UI can show toggles), ordered by creation. No secret values.
func (s *Store) ListLLMProviders(ctx context.Context) ([]LLMProvider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+llmProviderColumns+` FROM llm_providers ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LLMProvider{}
	for rows.Next() {
		p, err := scanLLMProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// LLMProviderConfigs returns the ENABLED providers with decrypted API keys —
// the one read that yields secret values, used only to (re)build the model
// resolver host-side. Never expose its result over HTTP.
func (s *Store) LLMProviderConfigs(ctx context.Context) ([]LLMProviderConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, provider_type, base_url, api_key_sealed, models, enabled, created_at, updated_at
		FROM llm_providers WHERE enabled ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LLMProviderConfig{}
	for rows.Next() {
		var c LLMProviderConfig
		var sealed []byte
		var modelsJSON []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &sealed,
			&modelsJSON, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(modelsJSON, &c.Models); err != nil {
			return nil, fmt.Errorf("decode models for provider %s: %w", c.ID, err)
		}
		if len(sealed) > 0 {
			if s.tokenCipher == nil {
				return nil, secretbox.ErrNoCipher
			}
			pt, err := s.tokenCipher.Open(sealed, secretbox.AAD(aadPurposeLLMKey, c.ID))
			if err != nil {
				return nil, fmt.Errorf("decrypt api key for provider %q: %w", c.Name, err)
			}
			c.APIKey = string(pt)
		}
		c.HasAPIKey = c.APIKey != ""
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetLLMProviderConfig returns ONE row with its decrypted key — the
// test-connection probe's read. Unlike LLMProviderConfigs it includes disabled
// rows (testing before enabling is the point). Same rule as the list variant:
// host-side use only, never expose the result over HTTP.
func (s *Store) GetLLMProviderConfig(ctx context.Context, id string) (*LLMProviderConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, provider_type, base_url, api_key_sealed, models, enabled, created_at, updated_at
		FROM llm_providers WHERE id=$1`, id)
	var c LLMProviderConfig
	var sealed []byte
	var modelsJSON []byte
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &sealed,
		&modelsJSON, &c.Enabled, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLLMProviderNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(modelsJSON, &c.Models); err != nil {
		return nil, fmt.Errorf("decode models for provider %s: %w", c.ID, err)
	}
	if len(sealed) > 0 {
		if s.tokenCipher == nil {
			return nil, secretbox.ErrNoCipher
		}
		pt, err := s.tokenCipher.Open(sealed, secretbox.AAD(aadPurposeLLMKey, c.ID))
		if err != nil {
			return nil, fmt.Errorf("decrypt api key for provider %q: %w", c.Name, err)
		}
		c.APIKey = string(pt)
	}
	c.HasAPIKey = c.APIKey != ""
	return &c, nil
}

// CreateLLMProvider inserts a provider. A non-nil, non-empty APIKey is sealed
// under the store cipher; creating a keyed provider without a cipher configured
// fails closed.
func (s *Store) CreateLLMProvider(ctx context.Context, in LLMProviderInput) (*LLMProvider, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	sealed, err := s.sealLLMKey(id, in.APIKey)
	if err != nil {
		return nil, err
	}
	modelsJSON, err := json.Marshal(in.Models)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO llm_providers (id, name, provider_type, base_url, api_key_sealed, models, enabled, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
		id, in.Name, in.Type, in.BaseURL, sealed, modelsJSON, in.Enabled, now)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("a provider named %q already exists", in.Name)
		}
		return nil, err
	}
	return s.getLLMProvider(ctx, id)
}

// UpdateLLMProvider updates a provider. in.APIKey nil leaves the stored key
// untouched; non-nil replaces it ("" clears). The NAME is immutable: it is the
// "<name>/<model>" routing prefix baked into saved conversations' and
// scheduled tasks' model slugs — a rename would silently break every one of
// them. Delete + recreate is the explicit way to change a prefix.
func (s *Store) UpdateLLMProvider(ctx context.Context, id string, in LLMProviderInput) (*LLMProvider, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	existing, err := s.getLLMProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.Name != in.Name {
		return nil, fmt.Errorf("provider name cannot be changed (it is the routing prefix in saved model slugs) — delete and recreate instead")
	}
	modelsJSON, err := json.Marshal(in.Models)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if in.APIKey == nil {
		res, err := s.db.ExecContext(ctx, `
			UPDATE llm_providers SET name=$2, provider_type=$3, base_url=$4, models=$5, enabled=$6, updated_at=$7
			WHERE id=$1`,
			id, in.Name, in.Type, in.BaseURL, modelsJSON, in.Enabled, now)
		if err != nil {
			if pgUniqueViolation(err) {
				return nil, fmt.Errorf("a provider named %q already exists", in.Name)
			}
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrLLMProviderNotFound
		}
		return s.getLLMProvider(ctx, id)
	}
	sealed, err := s.sealLLMKey(id, in.APIKey)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE llm_providers SET name=$2, provider_type=$3, base_url=$4, api_key_sealed=$5, models=$6, enabled=$7, updated_at=$8
		WHERE id=$1`,
		id, in.Name, in.Type, in.BaseURL, sealed, modelsJSON, in.Enabled, now)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("a provider named %q already exists", in.Name)
		}
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrLLMProviderNotFound
	}
	return s.getLLMProvider(ctx, id)
}

// DeleteLLMProvider removes a provider row (and with it the sealed key).
func (s *Store) DeleteLLMProvider(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM llm_providers WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLLMProviderNotFound
	}
	return nil
}

func (s *Store) getLLMProvider(ctx context.Context, id string) (*LLMProvider, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+llmProviderColumns+` FROM llm_providers WHERE id=$1`, id)
	p, err := scanLLMProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLLMProviderNotFound
	}
	return p, err
}

// sealLLMKey seals a (possibly nil/empty) key under the row-id-bound AAD.
// nil or empty → NULL column (no key stored).
func (s *Store) sealLLMKey(id string, key *string) ([]byte, error) {
	if key == nil || *key == "" {
		return nil, nil
	}
	if s.tokenCipher == nil {
		return nil, secretbox.ErrNoCipher
	}
	return s.tokenCipher.Seal([]byte(*key), secretbox.AAD(aadPurposeLLMKey, id))
}
