package agent

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// A connector the bundle declares but whose credentials are not set on the
// box ("Needs setup" in the Tools picker) is named in the system prompt, so the
// model tells the user it is not connected yet instead of sending them to the
// Tools picker to switch on something that has no switch (Raptive, PubMatic
// publisher, 2026-11-19). The list comes from the connector snapshot, never a
// hand-kept list.
func TestBuildSystemPrompt_NeedsSetupConnectors(t *testing.T) {
	m := fixtureManager(t)
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{
		{Name: "ix_publisher_mcp", DisplayName: "Index Exchange (Publisher)", Configured: true, Tools: []string{"ixpub_list_deals"}},
		{Name: "pubmatic_publisher_mcp", DisplayName: "PubMatic (Publisher)", Description: "Manage PubMatic publisher deals.", Tools: []string{"pmpub_list_deals", "pmpub_update_deal"}},
		{Name: "alpha_mcp", DisplayName: "Alpha", Tools: []string{"alpha_read"}},
		// The snapshot says unconfigured but the server is running: running wins.
		{Name: "stale_mcp", DisplayName: "Stale", Tools: []string{"stale_read"}},
	})
	m.alwaysOnServerMetadata = []AlwaysOnServerInfo{{Name: "stale_mcp"}}

	got, err := m.buildSystemPrompt("victoria", nil, "conv-x", nil, "", nil, nil, nil, hostedMCPRoster{}, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	idx := strings.Index(got, "## Connectors not set up on this server")
	if idx < 0 {
		t.Fatalf("section missing:\n%s", got)
	}
	section := got[idx:]
	if want := "- **PubMatic (Publisher)** (`pubmatic_publisher_mcp`): Manage PubMatic publisher deals."; !strings.Contains(section, want) {
		t.Errorf("missing PubMatic row %q in:\n%s", want, section)
	}
	if strings.Index(section, "alpha_mcp") > strings.Index(section, "pubmatic_publisher_mcp") {
		t.Errorf("rows not sorted by name:\n%s", section)
	}
	for _, not := range []string{"ix_publisher_mcp", "stale_mcp"} {
		if strings.Contains(section, not) {
			t.Errorf("%s is configured or running and must not be listed:\n%s", not, section)
		}
	}
	if !strings.Contains(section, "Do **NOT** tell the user to switch it on in the Tools picker") {
		t.Errorf("missing the do-not-switch-on rule:\n%s", section)
	}

	// Once the credentials are set (the reload's snapshot says Configured),
	// the section is gone with no other change.
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{
		{Name: "pubmatic_publisher_mcp", DisplayName: "PubMatic (Publisher)", Configured: true, Tools: []string{"pmpub_list_deals"}},
	})
	got, err = m.buildSystemPrompt("victoria", nil, "conv-x", nil, "", nil, nil, nil, hostedMCPRoster{}, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if strings.Contains(got, "## Connectors not set up on this server") {
		t.Errorf("section should be absent when every connector is configured")
	}
}

// A connector whose every declared tool the chat cannot use (hidden from
// interactive chat, or outside the persona's policy) is not listed: the model
// would offer something the user could not use even once it is set up.
func TestNeedsSetupForPrompt_RespectsHiddenAndPersona(t *testing.T) {
	m := fixtureManager(t)
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{
		{Name: "pubmatic_publisher_mcp", Tools: []string{"pmpub_update_deal"}},
		{Name: "beta_mcp", Tools: []string{"beta_read"}},
		{Name: "notools_mcp"},
	})
	got := m.needsSetupForPrompt("victoria", []string{"pmpub_update_deal"})
	names := []string{}
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "beta_mcp,notools_mcp" {
		t.Fatalf("hidden filter: got %v, want [beta_mcp notools_mcp]", names)
	}

	m.personaPolicies = map[string]agentcore.PersonaToolPermissions{"victoria": {Deny: []string{"mcp_beta_mcp_beta_read"}}}
	got = m.needsSetupForPrompt("victoria", nil)
	names = names[:0]
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "notools_mcp,pubmatic_publisher_mcp" {
		t.Fatalf("persona filter: got %v, want [notools_mcp pubmatic_publisher_mcp]", names)
	}
}

// Running state comes from the picker's base-name catalogs, never from
// parsing registered names: a running `jira_prod` (or one on account `acme`,
// registered as `jira_prod_acme`) hides neither `jira` nor `jira_prod_acme`.
func TestNeedsSetupForPrompt_RunningIsTheBaseNameCatalog(t *testing.T) {
	m := fixtureManager(t)
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{
		{Name: "jira", Tools: []string{"search"}},
		{Name: "jira_prod", Configured: true, Tools: []string{"search"}},
		{Name: "jira_prod_acme", Tools: []string{"search"}},
		{Name: "opt_mcp", Tools: []string{"read"}},
	})
	m.mcpCatalog = []mcp.ServerTool{{ServerName: "jira_prod_acme", Tool: mcp.Tool{Name: "search"}}}
	m.alwaysOnServerMetadata = []AlwaysOnServerInfo{{Name: "jira_prod"}}
	m.optionalServerMetadata = []OptionalServerInfo{{Name: "opt_mcp"}}
	got := m.needsSetupForPrompt("victoria", nil)
	names := make([]string, 0, len(got))
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "jira,jira_prod_acme" {
		t.Fatalf("got %v, want [jira jira_prod_acme]", names)
	}
}

// The persona policy is matched against the runtime tool name, which keeps
// the connector name's case.
func TestNeedsSetupForPrompt_PersonaMatchKeepsCase(t *testing.T) {
	m := fixtureManager(t)
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{{Name: "Salesforce", Tools: []string{"read"}}})
	m.personaPolicies = map[string]agentcore.PersonaToolPermissions{"victoria": {Deny: []string{"mcp_Salesforce_read"}}}
	if got := m.needsSetupForPrompt("victoria", nil); len(got) != 0 {
		t.Fatalf("denied connector listed: %+v", got)
	}
}

// Connector names are case-sensitive, so two that differ only by case are
// both listed.
func TestNeedsSetupForPrompt_CaseDistinctNamesBothListed(t *testing.T) {
	m := fixtureManager(t)
	m.SetConnectorMeta([]clientconfig.ConnectorMeta{
		{Name: "Salesforce", Tools: []string{"read"}},
		{Name: "salesforce", Tools: []string{"read"}},
	})
	got := m.needsSetupForPrompt("victoria", nil)
	names := make([]string, 0, len(got))
	for _, c := range got {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "Salesforce,salesforce" {
		t.Fatalf("got %v, want [Salesforce salesforce]", names)
	}
}
