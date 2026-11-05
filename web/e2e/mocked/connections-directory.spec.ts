import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// The directory starts with eight popular connectors. Searching must still
// reach entries outside that preview, with the count and cards below the
// search input instead of hidden beneath a sticky filter bar.

// A synthetic catalog big enough that the directory genuinely scrolls: three
// categories × 20 hosted servers each, plus the eight popular picks.
const CATEGORIES = ["development", "cloud-infrastructure", "databases"];
const POPULAR = [
  "microsoft-workiq-mail", "github", "slack", "google-gmail",
  "google-drive", "notion", "linear", "salesforce",
];
const catalog = {
  bundled: [],
  remote_mcp_enabled: true,
  third_party: [...POPULAR].reverse().map((name) => ({
    name,
    display_name: name,
    description: `Popular connector ${name}.`,
    url: `https://mcp.example.com/${name}`,
    vendor: "Example, Inc.",
    provenance: "official",
    category: "productivity",
  })).concat(CATEGORIES.flatMap((category) =>
    Array.from({ length: 20 }, (_, i) => ({
      name: `${category}-${i}`,
      display_name: `${category} server ${i}`,
      description: `Synthetic ${category} entry #${i} outside the popular preview.`,
      url: `https://mcp.example.com/${category}/${i}`,
      vendor: "Example, Inc.",
      provenance: "official",
      category,
    })),
  )),
};

async function mockConnectionsBoot(page: Page) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/admin/settings", (r: Route) =>
    r.fulfill({ status: 403, body: "forbidden — not an admin" }),
  );
  await page.route("**/api/mcp-servers", (r: Route) => r.fulfill({ json: { servers: [] } }));
  await page.route("**/api/remote-mcp-servers", (r: Route) =>
    r.fulfill({ json: { servers: [] } }),
  );
  await page.route("**/api/connector-prefs", (r: Route) => r.fulfill({ json: { prefs: [] } }));
  await page.route("**/api/mcp-catalog", (r: Route) => r.fulfill({ json: catalog }));
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("popular preview stays compact and search reaches the full directory", async ({ page }) => {
  await mockConnectionsBoot(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/settings/connections");

  const search = page.getByRole("textbox", { name: "Search connector directory" });
  await expect(search).toBeVisible({ timeout: 15_000 });
  const cards = page.locator('[data-testid^="dir-card-"]');
  await expect(cards).toHaveCount(8);
  expect(await cards.evaluateAll((els) => els.map((el) => el.getAttribute("data-testid"))))
    .toEqual(POPULAR.map((name) => `dir-card-${name}`));
  await expect(cards.getByText("Popular", { exact: true })).toHaveCount(8);
  await expect(page.getByRole("tablist")).toHaveCount(0);
  await expect(cards.getByText("Official", { exact: true })).toHaveCount(0);

  await search.fill("databases");
  await expect(page.getByRole("status")).toHaveText("20 connectors match");
  await expect(cards).toHaveCount(20);
  await expect(cards.getByText("Popular", { exact: true })).toHaveCount(0);

  // Visibility alone doesn't catch overlapping content. Search results must
  // remain below the search input as the full catalog replaces the preview.
  await expect(async () => {
    const searchBox = await search.boundingBox();
    expect(searchBox).not.toBeNull();
    const searchBottom = searchBox!.y + searchBox!.height;
    const tops = [
      page.getByRole("status"),
      page.getByTestId("dir-card-databases-0"),
    ];
    for (const target of tops) {
      const box = await target.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.y).toBeGreaterThanOrEqual(searchBottom);
    }
  }).toPass();

  await search.fill("missing-connector");
  await expect(cards).toHaveCount(0);
  await expect(page.getByText(/No connectors match/)).toBeVisible();
  await expect(page.getByRole("link", { name: "Contact Elcano" })).toHaveAttribute(
    "href", "mailto:hello@elcanotek.com?subject=Connector%20request",
  );
  await search.clear();
  await expect(cards).toHaveCount(8);
});
