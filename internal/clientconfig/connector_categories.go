package clientconfig

import "strings"

// Connector categories group connections in the chat Tools picker (the
// popover's static headings and the library's rail). A manifest connector
// names one by ID in its `category:` field — an ID, never free text, so "SSP"
// and "SSPs" can never split one group into two. The list is closed: an unknown
// ID fails the load. A connector without one lands in Misc Tools and startup
// logs a warning so it gets tagged.
//
// The house category comes first: the deployment's own tools (Lens, Pages, …),
// named per bundle (branding.house_tools_name, default "<app_name> Tools") so
// fleet carries no client name. Then the ad-ops categories, then the remote
// directory's curated slugs
// (builtin_remote_catalog.yaml) so a hosted connection groups under the same
// name the Connections directory shows. Misc Tools is always last.

// ConnectorCategory is one picker group: a stable ID and its display name.
type ConnectorCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MiscConnectorCategory is the fallback group for anything untagged.
const MiscConnectorCategory = "misc"

// HouseConnectorCategory groups the deployment's own tools, listed first.
const HouseConnectorCategory = "house"

var connectorCategories = []ConnectorCategory{
	{ID: HouseConnectorCategory, Name: "House Tools"},
	{ID: "ssps", Name: "SSPs"},
	{ID: "deal-management", Name: "Deal Management"},
	{ID: "data-storage", Name: "Data & Storage"},
	{ID: "development", Name: "Developer Tools"},
	{ID: "cloud-infrastructure", Name: "Cloud & Infrastructure"},
	{ID: "databases", Name: "Databases"},
	{ID: "data-analytics", Name: "Data & Analytics"},
	{ID: "web-search", Name: "Web Search & Scraping"},
	{ID: "productivity", Name: "Productivity & Projects"},
	{ID: "communication", Name: "Communication & Meetings"},
	{ID: "crm-sales", Name: "CRM & Sales"},
	{ID: "customer-support", Name: "Customer Support"},
	{ID: "commerce-payments", Name: "Commerce & Payments"},
	{ID: "finance", Name: "Finance"},
	{ID: "design-media", Name: "Design & Media"},
	{ID: "observability", Name: "Observability"},
	{ID: "security", Name: "Security"},
	{ID: "ai-ml", Name: "AI & ML"},
	{ID: "automation", Name: "Automation & Integrations"},
	{ID: "marketing-social", Name: "Marketing & Social"},
	{ID: "knowledge-docs", Name: "Docs & Knowledge"},
	{ID: "travel-local", Name: "Travel & Local"},
	{ID: MiscConnectorCategory, Name: "Misc Tools"},
}

// ConnectorCategories returns every picker category in display order, the
// house category first and Misc Tools last, with the generic house name. The
// picker hides the ones with no connection in them; serve
// Bundle.ConnectorCategories, which names the house category.
func ConnectorCategories() []ConnectorCategory {
	return connectorCategoriesNamed("")
}

// connectorCategoriesNamed is the list with the house category renamed
// (empty keeps "House Tools").
func connectorCategoriesNamed(house string) []ConnectorCategory {
	out := append([]ConnectorCategory(nil), connectorCategories...)
	if house != "" {
		out[0].Name = house
	}
	return out
}

// IsConnectorCategory reports whether id names a known category.
func IsConnectorCategory(id string) bool {
	for _, c := range connectorCategories {
		if c.ID == id {
			return true
		}
	}
	return false
}

// ConnectorCategoryOr returns id when it names a known category, else Misc
// Tools. Used for directory slugs, whose set is open (a bundle may invent its
// own), so an unknown one still lands somewhere.
func ConnectorCategoryOr(id string) string {
	id = strings.TrimSpace(id)
	if IsConnectorCategory(id) {
		return id
	}
	return MiscConnectorCategory
}
