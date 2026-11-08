package agent

import (
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/redact"
)

// RegisterConnectorSecrets hands every credential the connectors about to be
// initialised carry — resolved auth headers of HTTP MCP servers, inline HTTP
// tools and A2A peers, and secret-named env values of stdio servers — to the
// secret redactors (tool output and the process log) BEFORE any of them is
// contacted. A connector that answers its first initialize with a 401 echoing
// the opaque key it was sent is logged right then, at "MCP server failed to
// connect"; registering lazily after that left the key to the shape patterns,
// which an opaque token does not match. Duplicates are free.
func RegisterConnectorSecrets(specs map[string]MCPServerSpec, httpTools []config.HTTPToolConfig, a2aPeers []config.A2APeerConfig) {
	var values []string
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		values = appendHeaderSecrets(values, spec.Headers)
		for name, v := range spec.Env {
			if redact.IsSecretEnvName(name) {
				values = append(values, v)
			}
		}
	}
	for _, t := range httpTools {
		values = appendHeaderSecrets(values, t.Headers)
	}
	for _, p := range a2aPeers {
		values = appendHeaderSecrets(values, p.Headers)
	}
	if len(values) > 0 {
		agentcore.RegisterSecretLiterals("", false, values...)
	}
}

// appendHeaderSecrets appends the values of headers whose NAME marks a
// credential (Authorization, Cookie, X-Api-Key, X-Auth-Token, …). A value
// with an auth scheme ("Bearer abc") is registered whole and as the bare
// credential after the scheme, since an upstream may echo either. Other
// headers (Accept, a tenant id) are not secrets and are left out: as
// literals they would redact ordinary text.
func appendHeaderSecrets(values []string, headers map[string]string) []string {
	for name, v := range headers {
		lower := strings.ToLower(name)
		if !strings.Contains(lower, "auth") && lower != "cookie" && !redact.IsSecretEnvName(name) {
			continue
		}
		v = strings.TrimSpace(v)
		values = append(values, v)
		if _, cred, ok := strings.Cut(v, " "); ok {
			values = append(values, strings.TrimSpace(cred))
		}
	}
	return values
}
