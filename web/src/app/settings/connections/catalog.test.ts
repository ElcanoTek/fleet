import { describe, expect, it } from "vitest";

import {
  authHint,
  categoriesOf,
  categoryIcon,
  categoryLabel,
  connectorParamOf,
  consentRequired,
  FEATURED_SLUG,
  fillPlaceholders,
  filterCatalog,
  groupByCategory,
  needsTenantURL,
  placeholderLabel,
  placeholdersOf,
  placeholderValueOK,
  provenanceBadge,
  setupLink,
  toolCountSuffix,
  type CatalogThirdParty,
} from "./catalog";

const entry = (over: Partial<CatalogThirdParty>): CatalogThirdParty => ({
  name: "github",
  display_name: "GitHub",
  description: "Repos and issues.",
  url: "https://api.githubcopilot.com/mcp/",
  vendor: "GitHub, Inc.",
  provenance: "official",
  ...over,
});

const CATALOG: CatalogThirdParty[] = [
  entry({ name: "github", category: "development", tags: ["git", "issues"] }),
  entry({
    name: "stripe",
    display_name: "Stripe",
    description: "Payments and invoices.",
    vendor: "Stripe, Inc.",
    url: "https://mcp.stripe.com",
    category: "commerce-payments",
    tags: ["payments"],
  }),
  entry({
    name: "zapier",
    display_name: "Zapier",
    description: "Actions across thousands of apps.",
    vendor: "Zapier Inc.",
    url: "https://mcp.zapier.com/api/v1/connect",
    category: "automation",
    provenance: "third_party",
    tags: ["aggregator"],
  }),
  entry({
    name: "quirky",
    display_name: "Quirky",
    description: "A bundle-invented category.",
    url: "https://mcp.quirky.test/mcp",
    category: "ad-tech",
    provenance: "community",
  }),
];

describe("categoryIcon", () => {
  it("maps every curated slug to a sprite symbol and falls back to plug", () => {
    expect(categoryIcon("development")).toBe("wrench");
    expect(categoryIcon("databases")).toBe("database");
    expect(categoryIcon("finance")).toBe("dollar-sign");
    expect(categoryIcon("ad-tech")).toBe("plug");
    expect(categoryIcon("")).toBe("plug");
  });
});

describe("categoryLabel", () => {
  it("maps curated slugs and falls back to title case", () => {
    expect(categoryLabel("development")).toBe("Developer Tools");
    expect(categoryLabel("crm-sales")).toBe("CRM & Sales");
    expect(categoryLabel("ad-tech")).toBe("Ad Tech");
    expect(categoryLabel("")).toBe("Other");
  });
});

describe("categoriesOf", () => {
  it("returns curated order first, unknown categories after, with counts", () => {
    const cats = categoriesOf(CATALOG);
    expect(cats.map((c) => c.slug)).toEqual([
      "development",
      "commerce-payments",
      "automation",
      "ad-tech",
    ]);
    expect(cats[0]).toEqual({ slug: "development", label: "Developer Tools", count: 1 });
  });

  it("counts missing category as other, listed last", () => {
    const cats = categoriesOf([entry({ name: "x", category: undefined })]);
    expect(cats).toEqual([{ slug: "other", label: "Other", count: 1 }]);
  });
});

describe("filterCatalog", () => {
  it("returns everything for an empty query and no category", () => {
    expect(filterCatalog(CATALOG, "", "")).toHaveLength(4);
  });

  it("narrows to featured entries across categories via the reserved slug", () => {
    const withFeatured = [
      ...CATALOG,
      entry({ name: "linear", display_name: "Linear", category: "productivity", featured: true }),
      entry({ name: "notion", display_name: "Notion", category: "productivity", featured: true }),
    ];
    expect(filterCatalog(withFeatured, "", FEATURED_SLUG).map((e) => e.name)).toEqual([
      "linear",
      "notion",
    ]);
    // The free-text query still composes with the featured filter.
    expect(filterCatalog(withFeatured, "notion", FEATURED_SLUG).map((e) => e.name)).toEqual([
      "notion",
    ]);
  });

  it("narrows by category slug", () => {
    expect(filterCatalog(CATALOG, "", "automation").map((e) => e.name)).toEqual(["zapier"]);
  });

  it("matches tags, vendor, and category label case-insensitively", () => {
    expect(filterCatalog(CATALOG, "AGGREGATOR", "").map((e) => e.name)).toEqual(["zapier"]);
    expect(filterCatalog(CATALOG, "stripe, inc", "").map((e) => e.name)).toEqual(["stripe"]);
    expect(filterCatalog(CATALOG, "developer tools", "").map((e) => e.name)).toEqual(["github"]);
  });

  it("combines query and category", () => {
    expect(filterCatalog(CATALOG, "payments", "development")).toHaveLength(0);
  });
});

describe("groupByCategory", () => {
  it("splits a filtered list into ordered labeled sections", () => {
    const groups = groupByCategory(CATALOG);
    expect(groups.map((g) => g.slug)).toEqual([
      "development",
      "commerce-payments",
      "automation",
      "ad-tech",
    ]);
    expect(groups[2].entries.map((e) => e.name)).toEqual(["zapier"]);
  });
});

describe("provenanceBadge", () => {
  it("labels the three tiers and never inflates trust on bad input", () => {
    expect(provenanceBadge("official")).toEqual({ label: "Official", tone: "success" });
    expect(provenanceBadge("third_party")).toEqual({ label: "Aggregator", tone: "warning" });
    expect(provenanceBadge("community")).toEqual({ label: "Community", tone: "warning" });
    expect(provenanceBadge("")).toEqual({ label: "Community", tone: "warning" });
    expect(provenanceBadge("vendor")).toEqual({ label: "Community", tone: "warning" });
  });
});

describe("auth + tenant + consent", () => {
  it("flags {placeholder} endpoints as needing the user's URL", () => {
    const tenant = entry({ url: "https://{workspace}.example.com/mcp", auth: "tenant" });
    expect(needsTenantURL(tenant)).toBe(true);
    expect(authHint(tenant)).toBe("Needs your URL");
  });

  it("hints open and api_key auth, stays quiet for oauth", () => {
    expect(authHint(entry({ auth: "open" }))).toBe("No sign-in needed");
    expect(authHint(entry({ auth: "api_key" }))).toBe("Needs an API key");
    expect(authHint(entry({ auth: "oauth" }))).toBeNull();
    expect(authHint(entry({}))).toBeNull();
  });

  it("requires consent for any endpoint not run by the service's own vendor", () => {
    expect(consentRequired(entry({}))).toBe(false);
    expect(consentRequired(entry({ provenance: "third_party" }))).toBe(true);
    expect(consentRequired(entry({ provenance: "community" }))).toBe(true);
  });
});

describe("guided tenant-URL form helpers", () => {
  it("extracts placeholders deduped, in order", () => {
    expect(placeholdersOf("https://{tenantId}.example.com/{tenantId}/{region}/mcp")).toEqual([
      "tenantId",
      "region",
    ]);
    expect(placeholdersOf("https://mcp.example.com/mcp")).toEqual([]);
  });

  it("humanizes placeholder tokens", () => {
    expect(placeholderLabel("tenantId")).toBe("tenant id");
    expect(placeholderLabel("instance-name")).toBe("instance name");
    expect(placeholderLabel("workspace_hostname")).toBe("workspace hostname");
  });

  it("fills placeholders with trimmed values, leaving empties visible", () => {
    const url = "https://{store}.myshopify.com/api/mcp";
    expect(fillPlaceholders(url, { store: " acme " })).toBe("https://acme.myshopify.com/api/mcp");
    expect(fillPlaceholders(url, {})).toBe(url);
  });

  it("rejects placeholder values that would corrupt the URL", () => {
    expect(placeholderValueOK("acme")).toBe(true);
    expect(placeholderValueOK("acme/eu")).toBe(true);
    expect(placeholderValueOK("")).toBe(false);
    expect(placeholderValueOK("has space")).toBe(false);
    expect(placeholderValueOK("{nested}")).toBe(false);
  });
});

describe("toolCountSuffix", () => {
  it("pluralizes and stays silent on absent/zero counts", () => {
    expect(toolCountSuffix(12)).toBe(" — 12 tools available");
    expect(toolCountSuffix(1)).toBe(" — 1 tool available");
    expect(toolCountSuffix(0)).toBe("");
    expect(toolCountSuffix(undefined)).toBe("");
  });
});

describe("setupLink", () => {
  it("prefers the explicit setup walkthrough, falls back to docs", () => {
    expect(
      setupLink(entry({ setup_url: "https://vendor.test/setup", docs_url: "https://vendor.test/docs" })),
    ).toBe("https://vendor.test/setup");
    expect(setupLink(entry({ docs_url: "https://vendor.test/docs" }))).toBe(
      "https://vendor.test/docs",
    );
    expect(setupLink(entry({}))).toBeNull();
  });
});

describe("connectorParamOf", () => {
  it("parses the ?connector= deep link, normalized to the catalog's lowercase names", () => {
    expect(connectorParamOf("?connector=browserbase")).toBe("browserbase");
    expect(connectorParamOf("?connector=Browserbase")).toBe("browserbase");
    expect(connectorParamOf("?connector=%20browserbase%20")).toBe(
      "browserbase",
    );
    expect(connectorParamOf("?foo=1&connector=exa")).toBe("exa");
  });

  it("returns null when absent or empty", () => {
    expect(connectorParamOf("")).toBeNull();
    expect(connectorParamOf("?connected=1")).toBeNull();
    expect(connectorParamOf("?connector=")).toBeNull();
    expect(connectorParamOf("?connector=%20")).toBeNull();
  });
});
