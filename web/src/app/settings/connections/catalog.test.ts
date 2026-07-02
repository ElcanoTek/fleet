import { describe, expect, it } from "vitest";

import {
  authHint,
  categoriesOf,
  categoryLabel,
  consentRequired,
  filterCatalog,
  groupByCategory,
  needsTenantURL,
  provenanceBadge,
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
    expect(authHint(entry({ auth: "api_key" }))).toBe("API key");
    expect(authHint(entry({ auth: "oauth" }))).toBeNull();
    expect(authHint(entry({}))).toBeNull();
  });

  it("requires consent for any endpoint not run by the service's own vendor", () => {
    expect(consentRequired(entry({}))).toBe(false);
    expect(consentRequired(entry({ provenance: "third_party" }))).toBe(true);
    expect(consentRequired(entry({ provenance: "community" }))).toBe(true);
  });
});
