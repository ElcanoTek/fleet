package clientconfig

import (
	"slices"
	"strings"
	"testing"
)

const categoryManifest = `
mcp_servers:
  - name: ix
    command: python3
    args: ["mcp/ix.py"]
    optional: true
    display_name: "Index Exchange"
    category: ssps
    enabled_env: [IX_TEST_TOKEN]
    tools: [ix_get_deal, ix_create_deal]
  - name: bq
    command: python3
    args: ["mcp/bq.py"]
    optional: true
    display_name: "BigQuery Reporting"
    category: data-storage
    enabled_env: [BQ_TEST_KEY_UNSET]
    tools: [bq_query]
  - name: notes
    command: python3
    args: ["mcp/notes.py"]
    always: true
`

func TestConnectorMetaCategoriesAndSetupState(t *testing.T) {
	t.Setenv("IX_TEST_TOKEN", "x")
	b, err := Load(writeManifest(t, categoryManifest))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	meta := map[string]ConnectorMeta{}
	for _, m := range b.ConnectorMeta() {
		meta[m.Name] = m
	}
	if m := meta["ix"]; m.Category != "ssps" || !m.Configured || !m.Optional {
		t.Errorf("ix meta = %+v, want ssps, configured, optional", m)
	}
	if m := meta["bq"]; m.Category != "data-storage" || m.Configured || !slices.Equal(m.Tools, []string{"bq_query"}) {
		t.Errorf("bq meta = %+v, want data-storage, unconfigured, allowlist tools", m)
	}
	if m := meta["notes"]; m.Category != MiscConnectorCategory || !m.Configured || m.Optional {
		t.Errorf("notes meta = %+v, want misc fallback, configured, always-on", m)
	}
	if got := b.UncategorizedConnectors(); !slices.Equal(got, []string{"notes"}) {
		t.Errorf("UncategorizedConnectors() = %v, want [notes]", got)
	}
	// The unconfigured connector still never reaches the runtime catalog.
	if _, ok := b.MCPServerConfigs()["bq"]; ok {
		t.Error("unconfigured bq must not be in MCPServerConfigs")
	}
}

func TestConnectorMetaSurvivesScrub(t *testing.T) {
	t.Setenv("IX_TEST_TOKEN", "x")
	b, err := Load(writeManifest(t, categoryManifest))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	before := b.ConnectorMeta()
	b.ScrubConnectorRuntimeDefinitions()
	after := b.ConnectorMeta()
	if len(after) != len(before) || after[1].Name != "bq" || after[1].Configured {
		t.Errorf("ConnectorMeta after scrub = %+v, want %+v", after, before)
	}
}

func TestUnknownConnectorCategoryFailsLoad(t *testing.T) {
	_, err := Load(writeManifest(t, `
mcp_servers:
  - name: ix
    command: python3
    args: ["mcp/ix.py"]
    always: true
    category: SSP
`))
	if err == nil || !strings.Contains(err.Error(), `unknown category "SSP"`) {
		t.Fatalf("load err = %v, want unknown category", err)
	}
}

func TestConnectorCategoriesMiscLast(t *testing.T) {
	cats := ConnectorCategories()
	if cats[len(cats)-1].ID != MiscConnectorCategory {
		t.Errorf("last category = %q, want misc", cats[len(cats)-1].ID)
	}
	if cats[0].ID != "ssps" {
		t.Errorf("first category = %q, want ssps", cats[0].ID)
	}
	if ConnectorCategoryOr("crm-sales") != "crm-sales" || ConnectorCategoryOr("ad-tech") != MiscConnectorCategory {
		t.Error("ConnectorCategoryOr must keep known slugs and send unknown ones to misc")
	}
}
