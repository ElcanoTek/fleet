package clientconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/usertoken"
	"github.com/itchyny/gojq"
)

// validate checks the MCP catalog for the structural invariants the spawn path
// relies on, plus the sandbox descriptor. Content dirs are NOT required to exist
// (a manifest-only bundle is valid); callers that read a specific file surface
// their own not-found errors.
func (b *Bundle) validate() error {
	if err := b.validateSandbox(); err != nil {
		return err
	}

	// Tool names share ONE namespace across mcp_servers[] (server names) and
	// http_tools[] (tool names): the agent addresses them as mcp_<server>_<tool>,
	// so a collision would make dispatch ambiguous. `seen` therefore tracks both.
	seen := map[string]bool{}
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("mcp_servers[%d]: name is required", i)
		}
		if s.Name == HTTPToolServerName {
			return fmt.Errorf("mcp_servers[%q]: name %q is reserved for inline http_tools", s.Name, HTTPToolServerName)
		}
		if s.Name == A2AToolServerName {
			return fmt.Errorf("mcp_servers[%q]: name %q is reserved for a2a_peers", s.Name, A2AToolServerName)
		}
		if seen[s.Name] {
			return fmt.Errorf("mcp_servers: duplicate server name %q", s.Name)
		}
		seen[s.Name] = true
		// Catalog copy (display_name/description) is what a user reads in the
		// Tools picker and on the connections page before enabling a
		// connector. It is not load-bearing for execution, so a gap warns
		// rather than failing the bundle — but it warns LOUDLY and once per
		// connector, because the alternative failure mode is a silent blank
		// row that looks like a broken connector. See connector_labels.go.
		if strings.TrimSpace(s.DisplayName) == "" {
			s.DisplayName = deriveDisplayName(s.Name)
			slog.Warn("clientconfig: warning: mcp_servers has no display_name; the Tools picker and connections page will show the derived label. Add a display_name to manifest.yaml (see docs/MCP-CATALOG.md, \"Connector copy\").", "name", s.Name, "display_name", s.DisplayName)
		}
		if strings.TrimSpace(s.Description) == "" {
			slog.Warn("clientconfig: warning: mcp_servers has no description; the Tools picker and connections page will render this connector with an empty body. Add a one-sentence description to manifest.yaml (see docs/MCP-CATALOG.md, \"Connector copy\").", "name", s.Name)
		}
		switch s.Type {
		case "stdio", "":
			s.Type = "stdio"
			if strings.TrimSpace(s.Command) == "" {
				return fmt.Errorf("mcp_servers[%q]: stdio server requires a command", s.Name)
			}
		case "http":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp_servers[%q]: http server requires a url", s.Name)
			}
		default:
			return fmt.Errorf("mcp_servers[%q]: unknown type %q (want stdio|http)", s.Name, s.Type)
		}
		// TLS placement is validated for EVERY server (after Type is normalized)
		// so a tls: block that could never apply fails the load loudly instead of
		// being silently dropped.
		if err := validateServerTLS(s.Name, s.Type, s.URL, s.TLS); err != nil {
			return err
		}
		if err := validateServerEnvContract(s); err != nil {
			return err
		}
		if err := validateCategory(s); err != nil {
			return err
		}
		if err := validateBatchSecondsPerDeal(s); err != nil {
			return err
		}
		if err := validateApprovedCallTimeout(s); err != nil {
			return err
		}
		// The account-suffix convention is purely lexical, so its base vars
		// must not be underscore-prefixes of one another — fail the load
		// loudly rather than cross-wire credentials at spawn time (#1124).
		if err := validateAccountSuffixBases(s); err != nil {
			return err
		}
		// A declared probe must name a callable tool: empty is a broken
		// declaration, and a tool outside the tools: allowlist would have the
		// probe exercising a call the runtime itself never exposes — both fail
		// loud at load rather than silently at test time.
		if s.Probe != nil {
			probeTool := strings.TrimSpace(s.Probe.Tool)
			if probeTool == "" {
				return fmt.Errorf("mcp_servers[%q]: probe.tool is required", s.Name)
			}
			if len(s.Tools) > 0 && !slices.Contains(s.Tools, probeTool) {
				return fmt.Errorf("mcp_servers[%q]: probe.tool %q is not in the server's tools allowlist", s.Name, probeTool)
			}
		}
	}
	if err := b.validateHTTPTools(seen); err != nil {
		return err
	}
	if err := b.validateA2APeers(seen); err != nil {
		return err
	}
	if err := b.validatePersonas(); err != nil {
		return err
	}
	if err := b.validateWebhookTriggers(); err != nil {
		return err
	}
	if err := b.validateRemoteMCPCatalog(); err != nil {
		return err
	}
	if err := b.validateProviders(); err != nil {
		return err
	}
	if err := validatePricing(b.PricingConfig); err != nil {
		return err
	}
	if err := validateHooks(b.HooksConfig); err != nil {
		return err
	}
	return nil
}

// validateAccountSuffixBases rejects a stdio server whose account-suffix base
// vars contain one var that is an underscore-boundary prefix of another
// (#1124). The base set is every key of the server's env map (each is probed
// as <VAR>_<ACCOUNT> by creds.ApplyClientSuffix on a named-account spawn) plus
// account_vars (scanned by creds.AccountsFor to derive the account catalog).
// That convention is purely lexical, so declaring both FOO and FOO_BAR means:
//
//   - account "bar" silently overlays FOO with FOO_BAR's value — a DIFFERENT
//     credential injected under the wrong key; and
//   - FOO_BAR's tail surfaces as a phantom "bar" account of FOO in the
//     account catalog.
//
// Both are wrong-credential hazards, so the load fails with instructions to
// rename rather than leaving the ambiguity to spawn time. Matching is
// case-insensitive, mirroring creds.AccountsFor. Scope is per server — the
// set one ApplyClientSuffix/AccountsFor call actually operates over; http
// servers reject account variants outright and are skipped.
// validateIdentityEnv checks a server's identity_env: entries must name keys
// of THIS server's env map (and the account overlay is stdio-only). A typo'd
// identity var would otherwise silently never guard anything — for a
// wrong-seat/wrong-revenue guard that is the dangerous direction, so fail loud
// at startup instead.
func validateIdentityEnv(s *ServerDef) error {
	for _, v := range s.IdentityEnv {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return fmt.Errorf("mcp_servers[%q]: identity_env entries must be non-empty", s.Name)
		}
		if s.Type != "stdio" {
			return fmt.Errorf("mcp_servers[%q]: identity_env is only valid on a stdio server (accounts are env-suffixed; http servers reject account variants)", s.Name)
		}
		val, ok := s.Env[trimmed]
		if !ok {
			return fmt.Errorf("mcp_servers[%q]: identity_env var %q is not a key of the server's env map", s.Name, trimmed)
		}
		// The signed-in user is data, never routing identity (ADR-0087): a
		// key that routes a seat or money must not be filled from whoever is
		// at the keyboard, and the variant guard would read the token as an
		// inherited default-seat value and refuse every named seat anyway.
		if usertoken.Mentions(val) {
			return fmt.Errorf("mcp_servers[%q]: identity_env var %q is filled from ${FLEET_USER_EMAIL}; the signed-in user must never route a seat or money — drop it from identity_env (see docs/MCP-BUNDLE-ENV.md)", s.Name, trimmed)
		}
	}
	return nil
}

// validateCategory normalizes the Tools-picker category and fails the load on
// an unknown ID ("SSP" vs "ssps" would otherwise split one group into two).
func validateCategory(s *ServerDef) error {
	s.Category = strings.TrimSpace(s.Category)
	if s.Category != "" && !IsConnectorCategory(s.Category) {
		return fmt.Errorf("mcp_servers[%q]: unknown category %q (want one of the connector category IDs, e.g. ssps, deal-management, data-storage, misc)", s.Name, s.Category)
	}
	return nil
}

// validateBatchSecondsPerDeal checks a server's per-record deal_ids batch
// budget. It is seconds per record and the whole call is capped at 30
// minutes, so a value above MaxBatchSecondsPerDeal (most likely milliseconds
// typed as seconds) could never apply; a negative one is meaningless.
func validateBatchSecondsPerDeal(s *ServerDef) error {
	if s.BatchSecondsPerDeal < 0 || s.BatchSecondsPerDeal > MaxBatchSecondsPerDeal {
		return fmt.Errorf("mcp_servers[%q]: batch_seconds_per_deal must be between 1 and %d seconds (or omitted for the default), got %d", s.Name, MaxBatchSecondsPerDeal, s.BatchSecondsPerDeal)
	}
	return nil
}

// validateApprovedCallTimeout checks a server's approved-card call budget. It
// is seconds for one call, capped at 30 minutes, so a value above
// MaxApprovedCallTimeoutSeconds (most likely milliseconds typed as seconds)
// could never apply; a negative one is meaningless.
func validateApprovedCallTimeout(s *ServerDef) error {
	if s.ApprovedCallTimeoutSeconds < 0 || s.ApprovedCallTimeoutSeconds > MaxApprovedCallTimeoutSeconds {
		return fmt.Errorf("mcp_servers[%q]: approved_call_timeout_seconds must be between 1 and %d seconds (or omitted for the default), got %d", s.Name, MaxApprovedCallTimeoutSeconds, s.ApprovedCallTimeoutSeconds)
	}
	return nil
}

func validateAccountSuffixBases(s *ServerDef) error {
	if s.Type != "stdio" {
		return nil
	}
	bases := make(map[string]string, len(s.Env)+len(s.AccountVars)) // UPPER(name) → declared spelling
	names := make([]string, 0, len(s.Env)+len(s.AccountVars))
	collect := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		upper := strings.ToUpper(name)
		if _, dup := bases[upper]; dup {
			return
		}
		bases[upper] = name
		names = append(names, upper)
	}
	for name := range s.Env {
		collect(name)
	}
	for _, name := range s.AccountVars {
		collect(name)
	}
	slices.Sort(names) // deterministic error when several pairs conflict
	for _, upper := range names {
		for i := 0; i < len(upper); i++ {
			if upper[i] != '_' {
				continue
			}
			prefix, ok := bases[upper[:i]]
			if !ok {
				continue
			}
			return fmt.Errorf(
				"mcp_servers[%q]: env var %q is an underscore-prefix of sibling var %q — the account-suffix convention (<VAR>_<ACCOUNT>) would inject %s's value over %s for account %q and list %q as a credential account of %s; rename one of the vars so neither is a prefix of the other",
				s.Name, prefix, bases[upper], bases[upper], prefix, strings.ToLower(upper[i+1:]), strings.ToLower(upper[i+1:]), prefix)
		}
	}
	return nil
}

// webhookSlugShape bounds a manifest webhook trigger slug to the same URL-safe
// shape the orchestrator's task-trigger CLI enforces, so the two inbound-webhook
// surfaces share one slug grammar.
var webhookSlugShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// validateWebhookTriggers fails the load on a malformed webhook_triggers[] entry
// (#268): a missing/duplicate/out-of-shape slug, a missing notify_user, or a
// missing authentication method. Like the rest of validate this is fail-loud at
// startup — a trigger with no secret env configured would otherwise silently
// reject every inbound call (fail-closed), and a trigger with no notify_user
// would have nowhere to create its conversation. An empty prompt_template is
// allowed (the raw payload still reaches the model via the default rendering).
func (b *Bundle) validateWebhookTriggers() error {
	seen := map[string]bool{}
	for i := range b.WebhookTriggers {
		t := &b.WebhookTriggers[i]
		slug := strings.TrimSpace(t.Slug)
		if slug == "" {
			return fmt.Errorf("webhook_triggers[%d]: slug is required", i)
		}
		if !webhookSlugShape.MatchString(slug) {
			return fmt.Errorf("webhook_triggers[%q]: slug must match %s", slug, webhookSlugShape.String())
		}
		if seen[slug] {
			return fmt.Errorf("webhook_triggers: duplicate slug %q", slug)
		}
		seen[slug] = true
		if strings.TrimSpace(t.NotifyUser) == "" {
			return fmt.Errorf("webhook_triggers[%q]: notify_user is required", slug)
		}
		hasHMAC := strings.TrimSpace(t.HMACSecretEnv) != ""
		hasToken := strings.TrimSpace(t.TokenSecretEnv) != ""
		if !hasHMAC && !hasToken {
			return fmt.Errorf("webhook_triggers[%q]: an authentication method is required (set hmac_secret_env or token_secret_env)", slug)
		}
		if hasHMAC && hasToken {
			return fmt.Errorf("webhook_triggers[%q]: set only one of hmac_secret_env or token_secret_env", slug)
		}
	}
	return nil
}

// validateRemoteMCPCatalog fails the load on a malformed remote_mcp_catalog[]
// entry (#538): a blank/duplicate name, a missing display_name/description
// (the UI renders these — a blank card is a curation bug), a non-https URL
// (the endpoint receives conversation-derived tool traffic; plaintext is never
// acceptable), or a name colliding with a bundled mcp_servers entry (the two
// classes render side by side and must stay distinguishable). Fail-loud at
// startup, like the rest of validate.
func (b *Bundle) validateRemoteMCPCatalog() error {
	seen := map[string]bool{}
	manualStems := map[string]string{}
	for i := range b.RemoteMCPCatalog {
		e := &b.RemoteMCPCatalog[i]
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return fmt.Errorf("remote_mcp_catalog[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("remote_mcp_catalog: duplicate name %q", name)
		}
		seen[name] = true
		for _, s := range b.MCPCatalog {
			if s.Name == name {
				return fmt.Errorf("remote_mcp_catalog[%q]: name collides with bundled mcp_servers entry %q", name, name)
			}
		}
		if strings.TrimSpace(e.DisplayName) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: display_name is required", name)
		}
		if strings.TrimSpace(e.Description) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: description is required", name)
		}
		u := strings.TrimSpace(e.URL)
		if u == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: url is required", name)
		}
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("remote_mcp_catalog[%q]: url must be https:// (got %q)", name, e.URL)
		}
		if err := validateRemoteMCPEntryMeta(e); err != nil {
			return err
		}
		if e.ClientRegistration == "manual" {
			stem := manualOAuthStem(name)
			if stem == "" {
				return fmt.Errorf("remote_mcp_catalog[%q]: client_registration: manual requires an alphanumeric name stem for OAuth env vars", name)
			}
			if prev, dup := manualStems[stem]; dup {
				return fmt.Errorf("remote_mcp_catalog[%q]: manual OAuth env stem %q collides with %q", name, stem, prev)
			}
			manualStems[stem] = name
		}
	}
	return nil
}

// remoteMCPProvenances is the closed set of hosting-operator trust tiers a
// catalog entry may declare; see RemoteMCPCatalogEntry.Provenance.
var remoteMCPProvenances = map[string]bool{"official": true, "third_party": true, "community": true}

// remoteMCPAuths is the closed set of connection-hint values; see
// RemoteMCPCatalogEntry.Auth.
var remoteMCPAuths = map[string]bool{"oauth": true, "api_key": true, "open": true, "tenant": true}

// remoteMCPHeaderShape bounds api_key_header to an HTTP header-name token, so a
// catalog entry can never smuggle whitespace/CRLF into the per-user connect
// request that later replays it.
var remoteMCPHeaderShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// remoteMCPKeyPrefixShape bounds api_key_prefix: printable ASCII only (it is
// a header-value fragment; CR/LF or non-ASCII would be rejected or mangled by
// the transport), spaces allowed and significant.
var remoteMCPKeyPrefixShape = regexp.MustCompile(`^[\x20-\x7e]{1,64}$`)

// remoteMCPVariantIDShape bounds a url_variants id: it becomes part of an
// environment-variable name (FLEET_CATALOG_KEY_<ENTRY>_<ID>) and a test name.
var remoteMCPVariantIDShape = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// remoteMCPCategoryShape bounds a category slug to lowercase kebab-case so the
// UI's grouping/filter keys stay uniform. The set is open (a bundle may invent
// its own grouping) but the shape is not — "CRM Sales" vs "crm-sales" silently
// splitting one group into two is exactly the typo class validate exists for.
var remoteMCPCategoryShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateRemoteMCPEntryMeta fails the load on malformed directory metadata
// (category/tags/provenance/auth/repo_url). Absent provenance normalizes to
// "official" — a legacy shim for pre-existing bundle manifests, whose entries
// were all vendor-official; the built-in catalog's own test requires it
// explicitly so a new entry can never inherit trust by omission.
func validateRemoteMCPEntryMeta(e *RemoteMCPCatalogEntry) error {
	name := strings.TrimSpace(e.Name)
	if e.Provenance == "" {
		e.Provenance = "official"
	}
	if !remoteMCPProvenances[e.Provenance] {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown provenance %q (want official|third_party|community)", name, e.Provenance)
	}
	if e.Auth != "" && !remoteMCPAuths[e.Auth] {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown auth %q (want oauth|api_key|open|tenant)", name, e.Auth)
	}
	if e.Category != "" && !remoteMCPCategoryShape.MatchString(e.Category) {
		return fmt.Errorf("remote_mcp_catalog[%q]: category %q must be lowercase kebab-case", name, e.Category)
	}
	if r := strings.TrimSpace(e.RepoURL); r != "" && !strings.HasPrefix(r, "https://") {
		return fmt.Errorf("remote_mcp_catalog[%q]: repo_url must be https:// (got %q)", name, e.RepoURL)
	}
	if u := strings.TrimSpace(e.SetupURL); u != "" && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("remote_mcp_catalog[%q]: setup_url must be https:// (got %q)", name, e.SetupURL)
	}
	// docs_url is the page the directory card sends a user to before they
	// connect; it gets the same bar as repo_url and setup_url (the built-in
	// catalog requires it on every entry, a bundle may omit it).
	if u := strings.TrimSpace(e.DocsURL); u != "" && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("remote_mcp_catalog[%q]: docs_url must be https:// (got %q)", name, e.DocsURL)
	}
	if h := strings.TrimSpace(e.APIKeyHeader); h != "" {
		if e.Auth != "api_key" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header is only meaningful with auth: api_key", name)
		}
		if !remoteMCPHeaderShape.MatchString(h) {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header is not a valid header name (supply the name, not a credential)", name)
		}
	}
	if q := strings.TrimSpace(e.APIKeyQuery); q != "" {
		if e.Auth != "api_key" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_query is only meaningful with auth: api_key", name)
		}
		if strings.TrimSpace(e.APIKeyHeader) != "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header and api_key_query are mutually exclusive", name)
		}
		if !remoteMCPHeaderShape.MatchString(q) {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_query is not a valid query-parameter name (supply the name, not a credential)", name)
		}
	}
	if pfx := e.APIKeyPrefix; pfx != "" {
		if e.Auth != "api_key" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_prefix is only meaningful with auth: api_key", name)
		}
		if strings.TrimSpace(e.APIKeyHeader) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_prefix needs api_key_header (the default Authorization: Bearer shape takes no prefix)", name)
		}
		if !remoteMCPKeyPrefixShape.MatchString(pfx) {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_prefix must be 1-64 printable ASCII characters (supply the scheme, not a credential)", name)
		}
	}
	if err := validateRemoteMCPURLVariants(name, e); err != nil {
		return err
	}
	if err := validateRemoteMCPOAuthClient(name, e); err != nil {
		return err
	}
	for _, tag := range e.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: empty tag", name)
		}
		if tag != strings.ToLower(tag) {
			return fmt.Errorf("remote_mcp_catalog[%q]: tag %q must be lowercase", name, tag)
		}
	}
	return nil
}

func validateRemoteMCPOAuthClient(name string, e *RemoteMCPCatalogEntry) error {
	if e.ClientRegistration != "" && e.ClientRegistration != "manual" {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown client_registration %q (want manual or empty)", name, e.ClientRegistration)
	}
	if cs := strings.TrimSpace(e.ClientSecret); cs != "" {
		if cs != "required" {
			return fmt.Errorf("remote_mcp_catalog[%q]: unknown client_secret %q (want required or empty)", name, e.ClientSecret)
		}
		if e.ClientRegistration != "manual" {
			return fmt.Errorf("remote_mcp_catalog[%q]: client_secret: required is only meaningful with client_registration: manual", name)
		}
	}
	cid := strings.TrimSpace(e.OAuthClientID)
	csec := strings.TrimSpace(e.OAuthClientSecret)
	if cid == "" && csec == "" {
		return nil
	}
	if e.Auth != "" && e.Auth != "oauth" && e.Auth != "tenant" {
		return fmt.Errorf("remote_mcp_catalog[%q]: oauth_client_id / oauth_client_secret are only valid on auth: oauth or auth: tenant entries", name)
	}
	if e.ClientRegistration != "manual" {
		return fmt.Errorf("remote_mcp_catalog[%q]: oauth_client_id / oauth_client_secret are only meaningful with client_registration: manual", name)
	}
	if cid == "" && !hasManualOAuthIDInEnv(name) {
		return fmt.Errorf("remote_mcp_catalog[%q]: oauth_client_secret requires oauth_client_id", name)
	}
	if strings.TrimSpace(e.ClientSecret) == "required" && csec == "" && !hasManualOAuthSecretInEnv(name) {
		return fmt.Errorf("remote_mcp_catalog[%q]: client_secret: required needs oauth_client_secret alongside oauth_client_id", name)
	}
	return nil
}

// validateRemoteMCPURLVariants checks an entry's url_variants: each needs a
// unique id (it names the nightly fixture's secret and a test), a unique
// label, and an https url that is neither a placeholder nor the entry's own.
func validateRemoteMCPURLVariants(name string, e *RemoteMCPCatalogEntry) error {
	if len(e.URLVariants) == 0 {
		return nil
	}
	if strings.Contains(e.URL, "{") {
		return fmt.Errorf("remote_mcp_catalog[%q]: url_variants cannot be combined with a {placeholder} url", name)
	}
	seen := map[string]bool{}
	ids := map[string]bool{}
	urls := map[string]bool{}
	entryURL := strings.TrimRight(strings.TrimSpace(e.URL), "/")
	if canon, err := mcpoauth.CanonicalResourceURI(e.URL); err == nil {
		entryURL = strings.TrimRight(canon, "/")
	}
	for i, v := range e.URLVariants {
		if !remoteMCPVariantIDShape.MatchString(v.ID) {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] needs an id of 1-16 lowercase letters, digits or hyphens (got %q)", name, i, v.ID)
		}
		if ids[v.ID] {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants id %q is listed twice", name, v.ID)
		}
		ids[v.ID] = true
		label := strings.TrimSpace(v.Label)
		if label == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] needs a label", name, i)
		}
		if seen[strings.ToLower(label)] {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants label %q is listed twice", name, label)
		}
		seen[strings.ToLower(label)] = true
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v.URL)), "https://") {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] url must be https:// (got %q)", name, i, v.URL)
		}
		if strings.Contains(v.URL, "{") {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] url carries a {placeholder}", name, i)
		}
		// The parser AddServer stores a URL through: a variant it refuses
		// ("https://", a non-numeric port) would load, show on the card,
		// and fail every add — so it fails the bundle load instead.
		u, err := mcpoauth.CanonicalResourceURI(v.URL)
		if err != nil {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] url is not a valid server URL: %w", name, i, err)
		}
		// Repeats are compared canonical (case, default port, trailing slash),
		// the identity the stored connection and the drift check use.
		u = strings.TrimRight(u, "/")
		if u == entryURL {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] repeats the entry's own url", name, i)
		}
		if urls[u] {
			return fmt.Errorf("remote_mcp_catalog[%q]: url_variants[%d] repeats another variant's url", name, i)
		}
		urls[u] = true
	}
	return nil
}

// validateProviders fails the load on a malformed providers[] entry (#289): a
// blank/duplicate name, an unknown type, a missing api_key_env for a type that
// requires a key, a missing base_url for a type with no default endpoint
// (openaicompat, azure), or a region or base_url its type does not accept
// (agentcore.CheckProviderFields). The per-type rules come from agentcore's provider
// type table, the same one the admin store and the resolver check. Like the
// rest of validate this is fail-loud at startup — a typo'd provider type or a
// missing credential env would otherwise only surface as a resolver error on
// the first turn that routed to it. An empty providers block is valid
// (single-OpenRouter default).
func (b *Bundle) validateProviders() error {
	seen := map[string]bool{}
	for i := range b.Providers {
		p := &b.Providers[i]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("providers: duplicate provider name %q", name)
		}
		seen[name] = true
		typ := strings.TrimSpace(p.Type)
		if typ == "" {
			return fmt.Errorf("providers[%q]: type is required (%s)", name, agentcore.ProviderTypeChoices())
		}
		spec, ok := agentcore.LookupProviderType(typ)
		if !ok {
			return fmt.Errorf("providers[%q]: unknown type %q (want %s)", name, typ, agentcore.ProviderTypeChoices())
		}
		if spec.APIKeyRequired && strings.TrimSpace(p.APIKeyEnv) == "" {
			return fmt.Errorf("providers[%q]: api_key_env is required for a %q provider", name, typ)
		}
		if spec.BaseURLRequired && strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("providers[%q]: type %q requires base_url", name, typ)
		}
		if err := agentcore.CheckProviderFields(spec, p.BaseURL, p.Region); err != nil {
			return fmt.Errorf("providers[%q]: %w", name, err)
		}
		if p.ContextWindowTokens < 0 {
			return fmt.Errorf("providers[%q]: context_window_tokens must be positive when set", name)
		}
	}
	chainSeen := map[string]bool{}
	for i, raw := range b.FallbackProviders {
		name := strings.TrimSpace(raw)
		if name == "" {
			return fmt.Errorf("fallback_providers[%d]: provider name is required", i)
		}
		if !seen[name] {
			return fmt.Errorf("fallback_providers[%d]: unknown provider %q", i, name)
		}
		if chainSeen[name] {
			return fmt.Errorf("fallback_providers: duplicate provider %q", name)
		}
		chainSeen[name] = true
		b.FallbackProviders[i] = name
	}
	return nil
}

// validateServerTLS rejects a per-server TLS block that is malformed OR could
// never take effect (#280). A non-empty block is only meaningful on an https
// http server: http.Transport applies a TLSClientConfig solely to https
// requests, and stdio has no TLS transport at all — so a block on a plaintext
// http:// url or a stdio server would otherwise be SILENTLY ignored, leaving the
// operator believing a connection is pinned/mTLS-protected when it is not. We
// fail the load loudly instead. An absent or empty block (toMCP == nil) is a
// no-op and is allowed on any server.
//
// mTLS needs BOTH client_cert and client_key, and a public-key pin must be a
// well-formed SHA-256. File existence/parse is deliberately deferred to connect
// time (cert/key/CA files may be provisioned on the box separately from the
// bundle), where mcp.TLSOptions.build surfaces a clear, named error.
func validateServerTLS(name, serverType, url string, d *ServerTLSDef) error {
	if d.toMCP() == nil {
		return nil // absent or all-empty → nothing to apply or validate
	}
	if serverType != "http" {
		return fmt.Errorf("mcp_servers[%q]: tls is only valid on an http server (got type %q)", name, serverType)
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "https://") {
		return fmt.Errorf("mcp_servers[%q]: tls hardening requires an https:// url (got %q) — it cannot apply to a plaintext connection", name, url)
	}
	cert := strings.TrimSpace(d.ClientCert)
	key := strings.TrimSpace(d.ClientKey)
	if (cert == "") != (key == "") {
		return fmt.Errorf("mcp_servers[%q]: tls mTLS requires both client_cert and client_key", name)
	}
	if p := strings.TrimSpace(d.PinnedSHA256); p != "" {
		if _, err := mcp.NormalizePinSHA256(p); err != nil {
			return fmt.Errorf("mcp_servers[%q]: tls %w", name, err)
		}
	}
	return nil
}

// validatePersonas fails the load on a malformed personas[] entry — a blank
// name or a duplicate name. A typo'd or duplicated persona entry would
// otherwise silently fail to bind its tool policy (leaving the persona on the
// permissive default), which for a least-privilege gate is the dangerous
// direction: fail loud at startup instead. Empty tool_permissions blocks are
// allowed (they are the explicit "no narrowing" case).
func (b *Bundle) validatePersonas() error {
	seen := map[string]bool{}
	for i := range b.Personas {
		name := strings.TrimSpace(b.Personas[i].Name)
		if name == "" {
			return fmt.Errorf("personas[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("personas: duplicate persona name %q", name)
		}
		seen[name] = true
	}
	return nil
}

// validateHTTPTools fails the load on a malformed http_tools[] entry — a missing
// name/method/url, an unsupported method, a url that is not http(s) with a fixed
// host (no userinfo, no {param} in the scheme or host), a name that collides with
// an MCP server or another http_tool, an input_schema that is not a JSON-Schema
// object, or a response_jq that does not parse. The jq syntax check runs HERE (at Load) so a
// typo'd jq program fails startup loudly rather than at the first model call. The
// already-populated `seen` set enforces the single shared name namespace.
func (b *Bundle) validateHTTPTools(seen map[string]bool) error {
	for i := range b.HTTPTools {
		t := &b.HTTPTools[i]
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Errorf("http_tools[%d]: name is required", i)
		}
		if name == HTTPToolServerName || name == A2AToolServerName {
			return fmt.Errorf("http_tools[%q]: name is reserved", name)
		}
		if seen[name] {
			return fmt.Errorf("http_tools: duplicate tool name %q (collides with an mcp_servers entry or another http_tool)", name)
		}
		seen[name] = true

		method := strings.ToUpper(strings.TrimSpace(t.Method))
		if method == "" {
			return fmt.Errorf("http_tools[%q]: method is required (GET|POST|PUT|PATCH|DELETE)", name)
		}
		if !httpToolMethods[method] {
			return fmt.Errorf("http_tools[%q]: unsupported method %q (want GET|POST|PUT|PATCH|DELETE)", name, t.Method)
		}
		t.Method = method // normalize so the executor sees a canonical verb
		// The URL gets the same shape check as an a2a_peers rpc_url: an http(s)
		// URL with a host, never file:// (or any non-network scheme) and never
		// userinfo (the credential goes in headers, resolved host-side). The
		// scheme and host must also be FIXED — {param} tokens are substituted
		// from model-supplied arguments at call time, so a token in either
		// would hand the model the destination of a credential-bearing request
		// made from the process that owns every connector secret (SSRF). Tokens
		// in the path and query are the feature and stay allowed; the executor
		// percent-encodes them.
		rawURL := strings.TrimSpace(t.URL)
		if rawURL == "" {
			return fmt.Errorf("http_tools[%q]: url is required", name)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("http_tools[%q]: url does not parse: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("http_tools[%q]: url scheme must be http or https, got %q", name, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("http_tools[%q]: url has no host", name)
		}
		if u.User != nil {
			return fmt.Errorf("http_tools[%q]: url must not carry userinfo; put the credential in headers as a ${ENV_VAR} reference", name)
		}
		if strings.ContainsAny(u.Scheme+u.Host, "{}") {
			return fmt.Errorf("http_tools[%q]: url scheme and host must be fixed; {param} tokens are allowed only in the path and query", name)
		}
		t.URL = rawURL
		// input_schema, when present, must be a JSON-Schema object so the model is
		// handed a well-formed tool parameter schema. An absent schema is allowed
		// (a no-parameter tool); the executor advertises an empty object schema.
		if t.InputSchema != nil {
			if typ, ok := t.InputSchema["type"].(string); ok && typ != "object" {
				return fmt.Errorf("http_tools[%q]: input_schema.type must be %q, got %q", name, "object", typ)
			}
		}
		if jq := strings.TrimSpace(t.ResponseJQ); jq != "" {
			if _, err := gojq.Parse(jq); err != nil {
				return fmt.Errorf("http_tools[%q]: response_jq does not parse: %w", name, err)
			}
		}
	}
	return nil
}

// validateA2APeers is the fail-fast Load-time check for a2a_peers[] (#1368):
// a valid MCP-safe name, an http(s) rpc_url with a host (never file:// or a
// URL with userinfo — the credential goes in headers, resolved host-side), a
// non-empty bundle-authored description (the only peer text the model sees),
// and no collision in the shared tool namespace — the peer name AND its four
// derived tool names are reserved there, so an http_tool or mcp_server cannot
// shadow mcp__a2a_<peer>_send.
func (b *Bundle) validateA2APeers(seen map[string]bool) error {
	for i := range b.A2APeers {
		p := &b.A2APeers[i]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("a2a_peers[%d]: name is required", i)
		}
		if !a2aPeerNamePattern.MatchString(name) {
			return fmt.Errorf("a2a_peers[%q]: name must match %s (lowercase letters, digits, underscore; at most 32 chars)", name, a2aPeerNamePattern.String())
		}
		if name == HTTPToolServerName || name == A2AToolServerName {
			return fmt.Errorf("a2a_peers[%q]: name is reserved", name)
		}
		if seen[name] {
			return fmt.Errorf("a2a_peers: duplicate name %q (collides with an mcp_servers entry, an http_tool, or another peer)", name)
		}
		seen[name] = true
		for _, tool := range mcp.A2AToolNames(name) {
			if seen[tool] {
				return fmt.Errorf("a2a_peers[%q]: derived tool name %q collides with an mcp_servers entry or an http_tool", name, tool)
			}
			seen[tool] = true
		}
		p.Name = name

		rawURL := strings.TrimSpace(p.RPCURL)
		if rawURL == "" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url is required", name)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("a2a_peers[%q]: rpc_url does not parse: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url scheme must be http or https, got %q", name, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url has no host", name)
		}
		if u.User != nil {
			return fmt.Errorf("a2a_peers[%q]: rpc_url must not carry userinfo; put the credential in headers as a ${ENV_VAR} reference", name)
		}
		p.RPCURL = rawURL
		if strings.TrimSpace(p.Description) == "" {
			return fmt.Errorf("a2a_peers[%q]: description is required (it is the only text about the peer the model sees)", name)
		}
	}
	return nil
}

// validatePricing fails the load on a malformed pricing block (#297): an unknown
// fallback mode, an override missing its model slug, or a negative rate. This is
// the same fail-loud-at-startup posture as the rest of validate — a typo'd
// fallback or a sign-flipped rate would otherwise silently mis-account cost (and
// the cost ceiling that rides on it). An absent block (zero value) is valid: an
// empty fallback resolves to the OpenRouter default downstream.
func validatePricing(p PricingConfig) error {
	switch strings.ToLower(strings.TrimSpace(p.Fallback)) {
	case "", "openrouter", "zero":
	default:
		return fmt.Errorf("pricing.fallback: unknown value %q (want openrouter|zero)", p.Fallback)
	}
	for i, o := range p.Overrides {
		if strings.TrimSpace(o.Model) == "" {
			return fmt.Errorf("pricing.overrides[%d]: model is required", i)
		}
		for _, r := range []struct {
			name string
			val  float64
		}{
			{"input_cost_per_million_tokens", o.InputCostPerMillionTokens},
			{"output_cost_per_million_tokens", o.OutputCostPerMillionTokens},
			{"cache_read_cost_per_million_tokens", o.CacheReadCostPerMillionTokens},
			{"cache_write_cost_per_million_tokens", o.CacheWriteCostPerMillionTokens},
		} {
			if r.val < 0 {
				return fmt.Errorf("pricing.overrides[%q]: %s must not be negative (got %g)", o.Model, r.name, r.val)
			}
		}
	}
	return nil
}

// validateSandbox checks the declared sandbox block's Containerfile.
func (b *Bundle) validateSandbox() error {
	// Sandbox: a Containerfile the manifest NAMES (sandbox.containerfile) must
	// exist — a bundle pointing at a missing file is broken, and the generic
	// image is never used in its place. A bundle that names none may ship one
	// at the conventional sandbox/Containerfile (then it must be a file) or
	// ship none at all: it customizes nothing, so its sandbox is the release's
	// signed generic image, and an on-box build uses fleet's config/default
	// Containerfile (scripts/build-sandbox-image.sh, docs/RELEASE-ARTIFACTS.md).
	// With a prebuilt sandbox.image override the Containerfile is irrelevant.
	if b.sandboxDeclared && b.SandboxConfig.Image == "" {
		cf := b.SandboxConfig.ContainerfileAbsPath
		info, err := os.Stat(cf)
		switch {
		case err == nil && info.IsDir():
			return fmt.Errorf("sandbox: containerfile %s is a directory", cf)
		case err != nil && b.sandboxContainerfileNamed:
			return fmt.Errorf("sandbox: containerfile %s: %w (set sandbox.image to use a prebuilt image instead)", cf, err)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("sandbox: containerfile %s: %w", cf, err)
		}
	}
	return nil
}
