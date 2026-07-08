import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Regression for the connector directory's category-chip scroll: the sticky
// search + chip bar overlays the top of the settings scrollport, and a bare
// scrollIntoView used to park the results count, the category heading, and the
// first row of cards UNDERNEATH it — invisible until the user scrolled back
// up. The page now measures the bar and sets scroll-margin-top on the results
// container, so after a chip click the top of the filtered results must sit
// fully below the bar's bottom edge.

// A synthetic catalog big enough that the directory genuinely scrolls: three
// categories × 20 hosted servers each.
const CATEGORIES = ["development", "cloud-infrastructure", "databases"];
const catalog = {
  bundled: [],
  remote_mcp_enabled: true,
  third_party: CATEGORIES.flatMap((category) =>
    Array.from({ length: 20 }, (_, i) => ({
      name: `${category}-${i}`,
      display_name: `${category} server ${i}`,
      description: `Synthetic ${category} entry #${i} for the chip-scroll regression spec.`,
      url: `https://mcp.example.com/${category}/${i}`,
      vendor: "Example, Inc.",
      provenance: "official",
      category,
    })),
  ),
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

test("category chip scroll lands the results below the sticky filter bar", async ({ page }) => {
  await mockConnectionsBoot(page);
  // Reduced motion makes the chip-click scroll instant (behavior: "auto"), so
  // the geometry assertions don't race a smooth-scroll animation.
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/settings/connections");

  const bar = page.getByTestId("dir-filter-bar");
  await expect(bar).toBeVisible({ timeout: 15_000 });

  await page.getByRole("tab", { name: /^Databases/ }).click();
  await expect(page.getByText("20 servers match")).toBeVisible();

  // The occlusion check toBeVisible can't do: the filtered results' top — the
  // match count, the category heading, and the first card — must sit BELOW
  // the sticky bar's bottom edge, not underneath the bar.
  await expect(async () => {
    const barBox = await bar.boundingBox();
    expect(barBox).not.toBeNull();
    const barBottom = barBox!.y + barBox!.height;
    const tops = [
      page.getByText("20 servers match"),
      page.getByText("Databases", { exact: true }),
      page.getByTestId("dir-card-databases-0"),
    ];
    for (const target of tops) {
      const box = await target.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.y).toBeGreaterThanOrEqual(barBottom);
    }
  }).toPass();
});
