package clientconfig

import (
	"reflect"
	"slices"
	"strings"
)

// EnvVarNames returns every process-env var name the manifest references —
// across enable gates, account vars, identity_env, webhook/provider key names,
// and every ${VAR} interpolation site anywhere in the manifest (env/header
// values, url:, command:/args:, sandbox.image, providers, … — see
// manifestEnvVarNames, #1123). cmd/fleet passes these to
// config.RegisterAllowedEnvVars so the bundle's connector credentials survive
// the .env-file load while fleet's static allowlist stays client-agnostic.
func (b *Bundle) EnvVarNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || reservedRuntimeVar(name) || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	b.literalEnvVarNames(add)
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		for _, v := range s.Env {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
		for _, v := range s.Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// Inline http_tools' header secrets must survive the .env-file allowlist too:
	// they are resolved host-side at call time exactly like an MCP server's headers.
	for i := range b.HTTPTools {
		for _, v := range b.HTTPTools[i].Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// And a2a peers' header credentials (#1368), same seam.
	for i := range b.A2APeers {
		for _, v := range b.A2APeers[i].Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// Every ${VAR} reference anywhere else in the manifest (#1123) — url:,
	// command:/args:, sandbox.image, providers[].base_url, … interpolate at
	// load against the process env, so an env-file value for them must survive
	// the allowlist exactly like an env/header reference. Captured from the
	// raw manifest at Load (an exported value is substituted out of the parsed
	// fields, but its NAME must still be admitted).
	for _, name := range b.manifestEnvVarNames {
		add(name)
	}
	return out
}

// literalEnvVarNames feeds add() every env-var name the manifest names
// LITERALLY rather than via ${...} interpolation. Shared by EnvVarNames and
// EnvVarNamesDefaultOnly so the two views can never drift.
func (b *Bundle) literalEnvVarNames(add func(string)) {
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		for _, v := range s.EnabledEnv {
			add(v)
		}
		for _, group := range s.EnabledGroups {
			for _, v := range group {
				add(v)
			}
		}
		for _, v := range s.AccountVars {
			add(v)
		}
		// identity_env keys must survive the .env allowlist: the variant guard
		// reads their <VAR>_<ACCOUNT> forms from the process env (suffix
		// admission requires the BASE name to be registered).
		for _, v := range s.IdentityEnv {
			add(v)
		}
	}
	// Webhook trigger signing secrets (#268) are named directly (not ${VAR}
	// references) and are read host-side from the process env at request time, so
	// their env-var names must survive the .env-file allowlist exactly like an MCP
	// connector credential.
	for i := range b.WebhookTriggers {
		add(b.WebhookTriggers[i].HMACSecretEnv)
		add(b.WebhookTriggers[i].TokenSecretEnv)
	}
	// LLM provider API-key env vars (#289) are named directly (not ${VAR}
	// references) and read host-side at boot, so their names must survive the
	// .env-file allowlist exactly like an MCP connector credential.
	for i := range b.Providers {
		add(b.Providers[i].APIKeyEnv)
	}
}

// EnvVarNamesDefaultOnly returns the subset of EnvVarNames whose EVERY
// manifest appearance is a default-carrying ${VAR:-...} reference — config
// knobs with a manifest fallback (e.g. the generic bundle's
// `image: "${FLEET_SANDBOX_IMAGE:-}"`), never a credential something waits
// on. A name that is also referenced bare or ${VAR:?}-required anywhere, or
// named literally (enable gates, account_vars, identity_env, webhook
// secrets, provider keys), is excluded. `fleet validate-config` uses this to
// report an absent default-carrying name as "manifest defaults in effect"
// instead of a missing credential (#1123 widened EnvVarNames to every
// interpolated field, which would otherwise warn forever on a pristine
// install). Names only, never values.
func (b *Bundle) EnvVarNamesDefaultOnly() []string {
	literal := map[string]bool{}
	b.literalEnvVarNames(func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			literal[name] = true
		}
	})
	out := make([]string, 0, len(b.manifestDefaultOnlyEnvVarNames))
	for _, name := range b.manifestDefaultOnlyEnvVarNames {
		if !literal[name] {
			out = append(out, name)
		}
	}
	return out
}

// ConnectorEnvVarNames returns the base process-env names referenced by bundle
// MCP servers and inline HTTP tools. The inventory is captured from the raw
// manifest, before interpolation can replace an already-exported ${VAR} token
// with its value. Values are never retained here.
//
// This is narrower than EnvVarNames: provider keys and webhook signing secrets
// remain parent-owned and are deliberately excluded. Account-suffixed variants
// are represented by every stdio env key that ApplyClientSuffix may probe; use
// ConnectorEnvironmentKeys to expand those bases against a process environment.
func (b *Bundle) ConnectorEnvVarNames() []string {
	return append([]string(nil), b.connectorEnvVarNames...)
}

// ConnectorAccountEnvVarNames returns the stdio env keys whose
// <KEY>_<ACCOUNT> process-env variants may be consumed by account binding.
// These names are public identifiers only; values are never retained here.
func (b *Bundle) ConnectorAccountEnvVarNames() []string {
	return append([]string(nil), b.connectorAccountVarNames...)
}

// ConnectorEnvironmentKeys returns the exact keys in environ that belong to
// bundle connectors: every declared base name plus every <stdio-env-key>_<seat>
// variant recognized by the account suffix convention. Entries and returned
// values contain names only, never secret values. Matching account variants is
// case-insensitive, mirroring creds.AccountsFor.
func (b *Bundle) ConnectorEnvironmentKeys(environ []string) []string {
	bases := make(map[string]bool, len(b.connectorEnvVarNames))
	for _, name := range b.connectorEnvVarNames {
		bases[strings.ToUpper(name)] = true
	}
	accountBases := make([]string, 0, len(b.connectorAccountVarNames))
	for _, name := range b.connectorAccountVarNames {
		accountBases = append(accountBases, strings.ToUpper(name)+"_")
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		matched := bases[upper]
		if !matched {
			for _, prefix := range accountBases {
				if strings.HasPrefix(upper, prefix) && len(upper) > len(prefix) {
					matched = true
					break
				}
			}
		}
		if matched && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// PinConnectorMetadata records the public connector views (the always-on list
// and the picker metadata) from the live bundle and environment. Both read each
// connector's enable gate from the environment, so the broker parent calls this
// BEFORE it unsets the connector variables: pinned afterwards, every gated
// connector reads as unconfigured, and the Tools picker lists it as Needs setup
// next to its running row. ScrubConnectorRuntimeDefinitions keeps a pinned
// snapshot.
func (b *Bundle) PinConnectorMetadata() {
	if b == nil || b.MCPCatalog == nil {
		return
	}
	b.scrubbedAlwaysOnServers = append([]AlwaysOnServer{}, b.AlwaysOnServers()...)
	b.scrubbedConnectorMeta = append([]ConnectorMeta{}, b.ConnectorMeta()...)
	b.connectorMetaPinned = true
}

// ScrubConnectorRuntimeDefinitions removes the interpolated connector sections
// from an already loaded bundle. Call only after all public metadata has been
// extracted (PinConnectorMetadata, while the connector environment is still
// set) and the credential-owning child has inherited the environment.
// Branding, providers, webhook triggers, remote directory entries, policies,
// templates, and content paths remain intact for parent-side consumers.
func (b *Bundle) ScrubConnectorRuntimeDefinitions() {
	if b == nil {
		return
	}
	if !b.connectorMetaPinned {
		b.PinConnectorMetadata()
	}
	for i := range b.MCPCatalog {
		b.MCPCatalog[i] = ServerDef{}
	}
	for i := range b.HTTPTools {
		b.HTTPTools[i] = HTTPToolDef{}
	}
	for i := range b.A2APeers {
		b.A2APeers[i] = A2APeerDef{}
	}
	b.MCPCatalog = nil
	b.HTTPTools = nil
	b.A2APeers = nil
}

func connectorEnvInventory(servers []ServerDef, httpTools []HTTPToolDef, a2aPeers []A2APeerDef) ([]string, []string) {
	seen := map[string]bool{}
	accountSeen := map[string]bool{}
	var names, accountNames []string
	add := func(dst *[]string, index map[string]bool, name string) {
		name = strings.TrimSpace(name)
		if name == "" || reservedRuntimeVar(name) || index[name] {
			return
		}
		index[name] = true
		*dst = append(*dst, name)
	}
	addRef := func(value string) {
		for _, name := range sourceEnvRefs(value) {
			add(&names, seen, name)
		}
	}
	for i := range servers {
		s := &servers[i]
		for _, name := range s.EnabledEnv {
			add(&names, seen, name)
		}
		for _, group := range s.EnabledGroups {
			for _, name := range group {
				add(&names, seen, name)
			}
		}
		for _, name := range s.AccountVars {
			add(&names, seen, name)
			add(&accountNames, accountSeen, name)
		}
		for _, name := range s.IdentityEnv {
			add(&names, seen, name)
		}
		// ApplyClientSuffix probes <ENV_KEY>_<ACCOUNT> for every stdio env key,
		// not only the smaller account_vars discovery list. Retain those keys as
		// account bases so the parent can scrub identity/config overrides too.
		for name := range s.Env {
			add(&names, seen, name)
			add(&accountNames, accountSeen, name)
		}
		walkSourceStrings(reflect.ValueOf(*s), addRef)
	}
	for i := range httpTools {
		walkSourceStrings(reflect.ValueOf(httpTools[i]), addRef)
	}
	for i := range a2aPeers {
		walkSourceStrings(reflect.ValueOf(a2aPeers[i]), addRef)
	}
	slices.Sort(names)
	slices.Sort(accountNames)
	return names, accountNames
}

// walkSourceStrings visits every string in the raw connector definitions,
// including nested probe args/input schemas and map keys. The manifest
// interpolator operates over the entire YAML section, so an inventory limited
// to today's documented header/env fields could silently miss a future
// interpolated connector field.
func walkSourceStrings(value reflect.Value, visit func(string)) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			walkSourceStrings(value.Elem(), visit)
		}
	case reflect.String:
		visit(value.String())
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			walkSourceStrings(value.Field(i), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkSourceStrings(value.Index(i), visit)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			walkSourceStrings(iter.Key(), visit)
			walkSourceStrings(iter.Value(), visit)
		}
	default:
		// Scalar non-strings and executable/channel/unsafe kinds carry no
		// manifest text and therefore cannot contain an env reference.
	}
}

// sourceEnvRefs extracts source variable names from ${VAR}, ${VAR:-default},
// and ${VAR:?message} without consulting the environment. It shares the
// scanner with the manifest-wide reference inventory (envRefsIn), so it
// mirrors the interpolator's brace/escape rules and expandExpr's operator
// parse exactly.
func sourceEnvRefs(value string) []string {
	refs := envRefsIn(value)
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.name != "" {
			out = append(out, ref.name)
		}
	}
	return out
}

// WebhookSecretEnvNames returns the env-var names holding the bundle's webhook
// trigger signing secrets (GitHub-style HMAC + Slack). cmd/fleet passes these
// to config.RegisterNonReloadableSecretEnvVars so a config hot-reload pins them
// to their boot values (#584): they are per-request AUTH secrets, read via
// os.Getenv at verification time, and a drifted .env-file copy must be reported
// as Skipped rather than silently rotating a live secret mid-run.
func (b *Bundle) WebhookSecretEnvNames() []string {
	seen := map[string]bool{}
	var out []string
	for i := range b.WebhookTriggers {
		for _, name := range []string{b.WebhookTriggers[i].HMACSecretEnv, b.WebhookTriggers[i].TokenSecretEnv} {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
