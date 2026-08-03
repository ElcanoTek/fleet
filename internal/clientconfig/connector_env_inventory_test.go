package clientconfig

import (
	"slices"
	"testing"
)

func TestConnectorEnvInventorySurvivesInterpolation(t *testing.T) {
	t.Setenv("EXPORTED_SECRET", "connector-secret")
	t.Setenv("OWNER_ID", "owner-7")
	t.Setenv("HTTP_TOKEN", "http-secret")
	t.Setenv("URL_TOKEN", "url-secret")
	t.Setenv("PROVIDER_KEY", "provider-secret")

	b, err := Load(writeManifest(t, `
branding: {}
mcp_servers:
  - name: mail
    type: stdio
    command: /bin/true
    always: true
    env:
      API_TOKEN: "${EXPORTED_SECRET}"
      OWNER: "${OWNER_ID:-owner-default}"
      WORKDIR: "${FLEET_WORKSPACE}"
      LITERAL: "$${NOT_AN_ENV_REF}"
    account_vars: [API_TOKEN]
    identity_env: [OWNER]
http_tools:
  - name: lookup
    description: lookup
    method: GET
    url: "https://example.test/${URL_TOKEN}"
    headers:
      Authorization: "Bearer ${HTTP_TOKEN}"
    input_schema:
      type: object
providers:
  - name: direct
    type: anthropic
    api_key_env: PROVIDER_KEY
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	names := b.ConnectorEnvVarNames()
	for _, want := range []string{"API_TOKEN", "EXPORTED_SECRET", "HTTP_TOKEN", "LITERAL", "OWNER", "OWNER_ID", "URL_TOKEN", "WORKDIR"} {
		if !slices.Contains(names, want) {
			t.Errorf("ConnectorEnvVarNames = %v, want %q", names, want)
		}
	}
	for _, unwanted := range []string{"FLEET_WORKSPACE", "NOT_AN_ENV_REF", "PROVIDER_KEY"} {
		if slices.Contains(names, unwanted) {
			t.Errorf("ConnectorEnvVarNames = %v, must exclude %q", names, unwanted)
		}
	}
	if got := b.MCPServerConfigs()["mail"].Env["API_TOKEN"]; got != "connector-secret" {
		t.Fatalf("runtime interpolation changed: API_TOKEN = %q", got)
	}
}

func TestConnectorEnvironmentKeysIncludesAccountOverrides(t *testing.T) {
	b := &Bundle{
		connectorEnvVarNames:     []string{"API_TOKEN", "OWNER_ID", "HTTP_TOKEN"},
		connectorAccountVarNames: []string{"API_TOKEN", "OWNER_ID"},
	}
	environ := []string{
		"API_TOKEN=default",
		"API_TOKEN_CLIENT_A=seat-a",
		"api_token_client_b=seat-b",
		"OWNER_ID_CLIENT_A=owner-a",
		"HTTP_TOKEN=http",
		"HTTP_TOKEN_COPY=not-an-account-var",
		"API_TOKENIZER=unrelated",
		"PROVIDER_KEY=keep",
	}
	want := []string{"API_TOKEN", "API_TOKEN_CLIENT_A", "HTTP_TOKEN", "OWNER_ID_CLIENT_A", "api_token_client_b"}
	if got := b.ConnectorEnvironmentKeys(environ); !slices.Equal(got, want) {
		t.Fatalf("ConnectorEnvironmentKeys = %v, want %v", got, want)
	}
}

func TestSourceEnvRefsHandlesDefaultsRequiredAndEscapes(t *testing.T) {
	got := sourceEnvRefs(`a=${ A } b=${B:-${IGNORED}} c=${C:?required} d=$${LITERAL}`)
	want := []string{"A", "B", "C"}
	if !slices.Equal(got, want) {
		t.Fatalf("sourceEnvRefs = %v, want %v", got, want)
	}
}

func TestScrubConnectorRuntimeDefinitionsPreservesParentBundleState(t *testing.T) {
	b := &Bundle{
		Branding:        Branding{AppName: "Example"},
		MCPCatalog:      []ServerDef{{Name: "connector", Always: true, DisplayName: "Connector", Env: map[string]string{"TOKEN": "secret"}}},
		HTTPTools:       []HTTPToolDef{{Name: "lookup", Headers: map[string]string{"Authorization": "secret"}}},
		WebhookTriggers: []WebhookTriggerDef{{Slug: "events"}},
		Providers:       []ProviderDef{{Name: "models", APIKeyEnv: "MODEL_KEY"}},
		TaskTemplates:   []TaskTemplate{{Name: "template"}},
	}
	b.ScrubConnectorRuntimeDefinitions()
	if b.MCPCatalog != nil || b.HTTPTools != nil {
		t.Fatalf("connector definitions survived scrub: servers=%+v tools=%+v", b.MCPCatalog, b.HTTPTools)
	}
	if b.Branding.AppName != "Example" || len(b.WebhookTriggers) != 1 || len(b.Providers) != 1 || len(b.TaskTemplates) != 1 {
		t.Fatalf("parent bundle state was damaged: %+v", b)
	}
	if got := b.AlwaysOnServers(); len(got) != 1 || got[0].Name != "connector" || got[0].DisplayName != "Connector" {
		t.Fatalf("public always-on catalog was damaged: %+v", got)
	}
}
