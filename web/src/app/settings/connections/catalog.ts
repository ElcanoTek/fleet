// Pure helpers for the connector directory on the connections page (#538 +
// the directory expansion): category labels/ordering, search filtering, and
// provenance/auth presentation. Kept out of the page component so vitest can
// cover the logic without rendering.

import type { StatusTone } from "@/app/shared/ui/StatusChip";

export type CatalogThirdParty = {
  name: string;
  display_name: string;
  description: string;
  url: string;
  vendor?: string;
  docs_url?: string;
  repo_url?: string;
  category?: string;
  tags?: string[];
  // Hosting-operator trust tier — who runs the endpoint (distinct from the
  // bundled/third_party CLASS): "official" (the service's own vendor),
  // "third_party" (an aggregator/integrator hosting access to other vendors'
  // services), "community" (an identifiable maintainer who is neither).
  provenance: string;
  auth?: string;
  // Onboarding guidance (connector-directory onboarding): a visible sentence
  // on where the URL/key comes from, the vendor's connect walkthrough URL,
  // and — for api_key entries — the header name the key is sent under.
  setup_hint?: string;
  setup_url?: string;
  api_key_header?: string;
  // "manual" = the vendor's authorization server has no dynamic client
  // registration; the guided form collects a bring-your-own OAuth client ID
  // (+ optional secret) up front.
  client_registration?: string;
  // Curated Featured-shelf pick, rendered before the category listing.
  featured?: boolean;
};

export type CatalogBundled = {
  name: string;
  display_name?: string;
  description: string;
  tool_count: number;
  beta?: boolean;
  // Credential-account seat names (never secret values) provisioned for this
  // connector; the availability UI offers them as the user's default seat.
  accounts?: string[];
  enabled_by_default?: boolean;
  // false = an always-on connector: wired into every turn by the operator,
  // rendered as a visible-but-locked row (no per-user toggle).
  optional?: boolean;
};

// A user's explicit availability choice for one connector (absence = operator
// default). Mirrors the Go store.ConnectorPref wire shape.
export type ConnectorPref = {
  kind: "bundled" | "remote";
  connector_id: string;
  enabled: boolean;
  default_account?: string;
};

// prefFor looks up an explicit pref; effectiveEnabled collapses the tri-state
// for display: explicit choice wins, otherwise a connector is available.
export function prefFor(
  prefs: ConnectorPref[],
  kind: "bundled" | "remote",
  id: string,
): ConnectorPref | undefined {
  return prefs.find((p) => p.kind === kind && p.connector_id === id);
}

export function effectiveEnabled(
  prefs: ConnectorPref[],
  kind: "bundled" | "remote",
  id: string,
): boolean {
  return prefFor(prefs, kind, id)?.enabled ?? true;
}

export type CatalogResponse = {
  bundled: CatalogBundled[];
  third_party: CatalogThirdParty[];
  remote_mcp_enabled: boolean;
};

// Display names for the curated category slugs the built-in directory uses.
// An unknown slug (a bundle can invent its own) falls back to title-case, so
// nothing breaks when a bundle adds e.g. `ad-tech`.
const CATEGORY_LABELS: Record<string, string> = {
  development: "Developer Tools",
  "cloud-infrastructure": "Cloud & Infrastructure",
  databases: "Databases",
  "data-analytics": "Data & Analytics",
  "web-search": "Web Search & Scraping",
  productivity: "Productivity & Projects",
  communication: "Communication & Meetings",
  "crm-sales": "CRM & Sales",
  "customer-support": "Customer Support",
  "commerce-payments": "Commerce & Payments",
  finance: "Finance",
  "design-media": "Design & Media",
  observability: "Observability",
  security: "Security",
  "ai-ml": "AI & ML",
  automation: "Automation & Integrations",
  "marketing-social": "Marketing & Social",
  "knowledge-docs": "Docs & Knowledge",
  "travel-local": "Travel & Local",
  other: "Other",
};

// Directory section order — curated slugs in a deliberate reading order,
// unknown categories appended alphabetically, "other" always last.
const CATEGORY_ORDER = Object.keys(CATEGORY_LABELS);

export function categoryLabel(slug: string): string {
  const s = slug || "other";
  const known = CATEGORY_LABELS[s];
  if (known) return known;
  return s
    .split("-")
    .map((w) => (w ? w[0].toUpperCase() + w.slice(1) : w))
    .join(" ");
}

// categoriesOf returns the distinct categories present, in directory order,
// with counts — the source for the filter chips.
export function categoriesOf(
  entries: CatalogThirdParty[],
): { slug: string; label: string; count: number }[] {
  const counts = new Map<string, number>();
  for (const e of entries) {
    const slug = e.category || "other";
    counts.set(slug, (counts.get(slug) ?? 0) + 1);
  }
  const known = CATEGORY_ORDER.filter((s) => counts.has(s));
  const unknown = [...counts.keys()]
    .filter((s) => !CATEGORY_ORDER.includes(s))
    .sort();
  return [...known.filter((s) => s !== "other"), ...unknown, ...(counts.has("other") ? ["other"] : [])].map(
    (slug) => ({ slug, label: categoryLabel(slug), count: counts.get(slug) ?? 0 }),
  );
}

// filterCatalog narrows by a category slug ("" = all) and a free-text query
// matched case-insensitively against name, display name, description, vendor,
// category, and tags.
export function filterCatalog(
  entries: CatalogThirdParty[],
  query: string,
  category: string,
): CatalogThirdParty[] {
  const q = query.trim().toLowerCase();
  return entries.filter((e) => {
    if (category && (e.category || "other") !== category) return false;
    if (!q) return true;
    return (
      e.display_name.toLowerCase().includes(q) ||
      e.name.includes(q) ||
      e.description.toLowerCase().includes(q) ||
      (e.vendor ?? "").toLowerCase().includes(q) ||
      categoryLabel(e.category || "other")
        .toLowerCase()
        .includes(q) ||
      (e.tags ?? []).some((t) => t.includes(q))
    );
  });
}

// groupByCategory splits a (filtered) list into ordered sections.
export function groupByCategory(
  entries: CatalogThirdParty[],
): { slug: string; label: string; entries: CatalogThirdParty[] }[] {
  return categoriesOf(entries).map(({ slug, label }) => ({
    slug,
    label,
    entries: entries.filter((e) => (e.category || "other") === slug),
  }));
}

// provenanceBadge maps the hosting-operator trust tier to its chip. Official
// = the vendor itself operates the endpoint; Aggregator = an identifiable
// platform hosting access to OTHER vendors' services (it sees the traffic and
// often holds the delegated tokens); Community = an identifiable maintainer
// who is neither. Anything unexpected renders as Community — never inflate
// trust on bad input.
export function provenanceBadge(provenance: string): {
  label: string;
  tone: StatusTone;
} {
  switch (provenance) {
    case "official":
      return { label: "Official", tone: "success" };
    case "third_party":
      return { label: "Aggregator", tone: "warning" };
    default:
      return { label: "Community", tone: "warning" };
  }
}

// needsTenantURL — a {placeholder} endpoint is per org/workspace/store and
// can't be one-click added; the user supplies the placeholder values through
// the card's guided form.
export function needsTenantURL(e: CatalogThirdParty): boolean {
  return e.url.includes("{");
}

// placeholdersOf extracts the {placeholder} tokens from a tenant-scoped URL
// template, deduped, in order of appearance — one guided-form input each.
export function placeholdersOf(url: string): string[] {
  const out: string[] = [];
  for (const m of url.matchAll(/\{([^{}]+)\}/g)) {
    if (!out.includes(m[1])) out.push(m[1]);
  }
  return out;
}

// placeholderLabel humanizes a {placeholder} token for the guided form:
// "tenantId" / "tenant_id" / "tenant-id" all render "tenant id".
export function placeholderLabel(ph: string): string {
  return ph
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/[-_]+/g, " ")
    .toLowerCase()
    .trim();
}

// fillPlaceholders substitutes the user's values into the URL template. Values
// are trimmed; slashes are allowed (some templates take a host or a path
// segment) but whitespace and braces are not — the preview + backend URL
// validation catch the rest.
export function fillPlaceholders(url: string, values: Record<string, string>): string {
  return url.replace(/\{([^{}]+)\}/g, (whole, ph: string) => {
    const v = (values[ph] ?? "").trim();
    return v === "" ? whole : v;
  });
}

// placeholderValueOK — a single placeholder value is usable when present and
// free of characters that would corrupt the URL.
export function placeholderValueOK(v: string): boolean {
  const t = v.trim();
  return t !== "" && !/[\s{}]/.test(t);
}

// setupLink is the best "how do I connect this?" destination: the explicit
// setup walkthrough when the entry has one, else the vendor docs.
export function setupLink(e: CatalogThirdParty): string | null {
  return e.setup_url || e.docs_url || null;
}

// authHint is the small secondary label describing what connecting takes.
// OAuth is the default expectation and renders no hint.
export function authHint(e: CatalogThirdParty): string | null {
  if (needsTenantURL(e)) return "Needs your URL";
  switch (e.auth) {
    case "open":
      return "No sign-in needed";
    case "api_key":
      return "Needs an API key";
    default:
      return null;
  }
}

// consentRequired — adding a server whose endpoint is NOT operated by the
// service's own vendor gets an explicit, operator-named consent step: that
// operator receives tool-call arguments (which can include conversation
// content) and, for OAuth flows, often holds the delegated access token.
export function consentRequired(e: CatalogThirdParty): boolean {
  return e.provenance !== "official";
}
