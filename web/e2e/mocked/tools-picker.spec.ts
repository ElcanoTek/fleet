import { test, expect, type Page, type Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The composer's Tools picker end to end against a mocked catalog shaped like
// the design's sample data: the wrench badge, the popover (On section,
// category headings, search, needs-setup rows), the account menu, and the
// library modal. Screenshots land in test-results/ for a visual check.

const CATEGORIES = [
  { id: "house", name: "Elcano Tools" },
  { id: "ssps", name: "SSPs" },
  { id: "deal-management", name: "Deal Management" },
  { id: "data-storage", name: "Data & Storage" },
  { id: "misc", name: "Misc Tools" },
];

const SERVERS = [
  { name: "dealsheet", display_name: "Deal Sheet", category: "house", always_on: true, enabled: true,
    description: "Build a branded XLSX deal sheet from a batch of created deals, ready to forward to the trader and the client.",
    tools: [], tool_count: 12 },
  { name: "lens", display_name: "Lens", category: "house", enabled: false,
    description: "Classify websites, apps and CTV apps into ad quality tiers with Lens, then read and download the results.",
    tools: ["lens_classify", "lens_list_runs", "lens_download"], tool_count: 3 },
  { name: "pages", display_name: "Pages", category: "house", enabled: false,
    description: "Build, deploy and publish versioned client dashboards from templates.",
    tools: ["pages_deploy", "pages_publish"], tool_count: 2 },
  { name: "deallib", display_name: "Deal Library", category: "deal-management", enabled: false,
    description: "Search, read and update deals stored in the Manifest Deal Library.",
    tools: ["search_deals", "get_deal", "update_deal"], tool_count: 3 },
  { name: "indexexchange", display_name: "Index Exchange", category: "ssps", enabled: true,
    description: "Create, edit and pause private marketplace deals on Index Exchange.",
    tools: ["ix_create_deal", "ix_update_deal", "ix_clone_deal"], tool_count: 3,
    accounts: ["OMG_USD", "OMG_GBP", "OMG_EUR"] },
  { name: "magnite", display_name: "Magnite", category: "ssps", enabled: false,
    description: "Manage Magnite Streaming and DV+ deals: create, update, pause, and pull delivery reports.",
    tools: ["mg_create_deal", "mg_clone_deal"], tool_count: 2, accounts: ["OMG_EMEA"] },
  { name: "pubmatic", display_name: "PubMatic", category: "ssps", enabled: false,
    description: "Create and manage PubMatic PMP deals and curated packages.",
    tools: ["pm_create_deal"], tool_count: 1 },
  { name: "triplelift", display_name: "TripleLift", category: "ssps", enabled: false, status: "unavailable",
    description: "Native and CTV deal management on TripleLift.", tools: [], tool_count: 0 },
  { name: "fastio", display_name: "Fast.io", category: "data-storage", always_on: true, enabled: true,
    description: "Browse, download, upload and share files in the Fast.io workspace where client reporting lands.",
    tools: [], tool_count: 7 },
  { name: "bigquery", display_name: "BigQuery Reporting", category: "data-storage", enabled: false, status: "needs_setup",
    description: "Query the Elcano reporting warehouse for spend, delivery and win-rate by exchange.",
    tools: ["bq_query", "bq_list_tables"], tool_count: 2 },
  { name: "image_generation", display_name: "Image generation", category: "misc", enabled: false,
    description: "Turn this on, then ask for an image.", tools: [], tool_count: 0 },
];

async function boot(page: Page, { admin }: { admin: boolean }) {
  await page.route("**/api/**", (route) => route.fulfill({ json: {} }));
  await mockChatBoot(page);
  await page.route("**/api/mcp-servers", (r: Route) =>
    r.fulfill({ json: { servers: SERVERS, categories: CATEGORIES } }),
  );
  await page.route("**/api/admin/settings", (r: Route) =>
    admin ? r.fulfill({ json: {} }) : r.fulfill({ status: 403, body: "forbidden" }),
  );
  await page.route("**/api/client-config", (r: Route) =>
    r.fulfill({ json: { branding: { app_name: "Elcano" } } }),
  );
  await page.goto("/chat");
}

test("tools popover: badge, On section, headings, search and account menu", async ({ page, context }) => {
  await page.setViewportSize({ width: 1280, height: 860 });
  await loginViaCookie(context);
  await boot(page, { admin: false });

  const wrench = page.getByRole("button", { name: "Tools", exact: true });
  // Index Exchange + the two always-on connections.
  await expect(wrench).toHaveAttribute("data-tip-top", "Tools, 3 on");
  await wrench.click();
  const popover = page.getByTestId("chat-tools-popover");
  await expect(popover).toBeVisible();
  await expect(popover).toHaveCSS("opacity", "1");
  await expect(page.getByRole("textbox", { name: "Search tools" })).toBeFocused();
  await expect(popover.getByRole("group")).toHaveText([/^On/, /^Elcano Tools/, /^SSPs/, /^Deal Management/, /^Misc Tools/]);
  await expect(popover.getByTestId("chat-tools-row-bigquery")).toHaveCount(0);
  await expect(popover.getByTestId("chat-tools-row-triplelift")).toHaveCount(0);
  await expect(page.getByTestId("mcp-seat-indexexchange")).toHaveText(/Elcano/);
  await page.screenshot({ path: "test-results/tools-popover.png" });

  await page.getByTestId("mcp-seat-indexexchange").click();
  const seatList = page.getByRole("listbox", { name: "Account for Index Exchange" });
  await expect(seatList).toBeVisible();
  await expect(seatList).toHaveCSS("opacity", "1");
  await page.screenshot({ path: "test-results/tools-seat-menu.png" });
  await page.getByRole("option", { name: /OMG_GBP/ }).click();
  await expect(page.getByTestId("mcp-seat-indexexchange")).toHaveText(/OMG_GBP/);
  await expect(popover).toBeVisible();

  const search = page.getByRole("textbox", { name: "Search tools" });
  await search.fill("deal");
  // Deal Sheet and Deal Library tie on a name prefix; the house group wins
  // the tie on category order.
  await expect(popover.getByRole("group").first()).toHaveText(/^Elcano Tools/);
  await expect(popover.getByRole("group").nth(1)).toHaveText(/^Deal Management/);
  await page.screenshot({ path: "test-results/tools-popover-search.png" });

  await search.fill("clone");
  await expect(popover.getByTestId("chat-tools-row-magnite")).toContainText("↳ mg_clone_deal");

  await search.fill("bigquery");
  await expect(popover.getByTestId("chat-tools-row-bigquery")).toContainText("Needs admin setup");

  await search.fill("ssp");
  await expect(popover.getByRole("button", { name: "All 3 on" })).toBeVisible();
  await page.screenshot({ path: "test-results/tools-popover-ssp.png" });

  await search.fill("xyz");
  await expect(popover).toContainText("No tools match ‘xyz’");

  await search.press("Escape");
  await expect(search).toHaveValue("");
  await search.press("Escape");
  await expect(popover).toHaveCount(0);
  await expect(wrench).toBeFocused();
});

test("tools library: counts, filters, detail and admin setup link", async ({ page, context }) => {
  await page.setViewportSize({ width: 1280, height: 860 });
  await loginViaCookie(context);
  await boot(page, { admin: true });

  await page.getByRole("button", { name: "Tools", exact: true }).click();
  await page.getByRole("textbox", { name: "Search tools" }).fill("bigquery");
  await page.getByRole("button", { name: /Set up/ }).click();
  const library = page.getByTestId("chat-tools-library");
  await expect(library).toBeVisible();
  await expect(library).toContainText("3 of 11 on");
  const detail = page.getByTestId("chat-tools-library-detail");
  await expect(detail).toContainText("Needs setup before it can be used.");
  await expect(detail.getByRole("link", { name: /Set up in Settings/ })).toHaveAttribute(
    "href",
    "/settings/connections#connector-bigquery",
  );
  await page.screenshot({ path: "test-results/tools-library-setup.png" });

  await library.getByRole("navigation", { name: "Categories" }).getByRole("button", { name: /SSPs/ }).click();
  await library.getByTestId("chat-tools-library-row-indexexchange").getByRole("button").first().click();
  await expect(detail).toContainText("On for this chat");
  await expect(detail.getByTestId("mcp-seat-indexexchange")).toBeVisible();
  await expect(library.getByTestId("chat-tools-library-bulk")).toContainText("Applies to 3 connections in SSPs");
  await detail.getByRole("button", { name: /Show 3 tools/ }).click();
  await expect(detail).toContainText("ix_clone_deal");
  await page.screenshot({ path: "test-results/tools-library-ssps.png" });

  await page.keyboard.press("Escape");
  await expect(library).toHaveCount(0);
});

test("tools picker renders in the dark theme", async ({ page, context }) => {
  await page.setViewportSize({ width: 1280, height: 860 });
  await page.emulateMedia({ colorScheme: "dark" });
  await loginViaCookie(context);
  await boot(page, { admin: true });
  await page.getByRole("button", { name: "Tools", exact: true }).click();
  await expect(page.getByTestId("chat-tools-popover")).toHaveCSS("opacity", "1");
  await page.screenshot({ path: "test-results/tools-popover-dark.png" });
  await page.getByRole("textbox", { name: "Search tools" }).fill("bigquery");
  await page.getByRole("button", { name: /Set up/ }).click();
  await expect(page.getByTestId("chat-tools-library")).toBeVisible();
  await page.screenshot({ path: "test-results/tools-library-dark.png" });
});

test("tools picker fits a phone without sideways scroll", async ({ page, context }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await loginViaCookie(context);
  await boot(page, { admin: false });
  await page.getByRole("button", { name: "Tools", exact: true }).click();
  const popover = page.getByTestId("chat-tools-popover");
  await expect(popover).toHaveCSS("opacity", "1");
  const box = await popover.boundingBox();
  expect(box && box.x >= 0 && box.x + box.width <= 390).toBe(true);
  await page.screenshot({ path: "test-results/tools-popover-phone.png" });
  await page.getByRole("button", { name: "Open library" }).click();
  const library = page.getByTestId("chat-tools-library");
  await expect(library).toBeVisible();
  expect(await library.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await page.screenshot({ path: "test-results/tools-library-phone.png" });
});

test.describe("on a touch phone", () => {
  test.use({ hasTouch: true, isMobile: true });

  test("tablet rail filters keep 44px touch targets", async ({ page, context }) => {
    await page.setViewportSize({ width: 820, height: 1180 });
    await loginViaCookie(context);
    await boot(page, { admin: true });
    await page.getByRole("button", { name: "Tools", exact: true }).tap();
    await page.getByRole("button", { name: "Open library" }).tap();
    const rail = page.getByTestId("chat-tools-library").getByRole("navigation", { name: "Status" });
    const heights = await rail.getByRole("button").evaluateAll((b) => b.map((n) => n.getBoundingClientRect().height));
    expect(Math.min(...heights)).toBeGreaterThanOrEqual(44);
  });

  test("library: close top-right, detail is its own view, no keyboard on open", async ({ page, context }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await loginViaCookie(context);
    await boot(page, { admin: true });
    await page.getByRole("button", { name: "Tools", exact: true }).tap();
    const popover = page.getByTestId("chat-tools-popover");
    await expect(popover).toHaveCSS("opacity", "1");
    // Focusing the search field would open the keyboard over the list.
    await expect(page.getByRole("textbox", { name: "Search tools" })).not.toBeFocused();
    // The bulk strip is 44px tall, so All on / All off stay out of the search row.
    const strip = (await page.getByTestId("chat-mcp-all-actions").boundingBox())!;
    expect(strip.height).toBeGreaterThanOrEqual(44);
    // The footer is tall enough that "Open library"'s 44px touch box is not clipped.
    const footerLink = (await page.getByRole("button", { name: "Open library" }).boundingBox())!;
    const pop = (await popover.boundingBox())!;
    expect(pop.y + pop.height - (footerLink.y + footerLink.height / 2)).toBeGreaterThanOrEqual(22);
    await page.getByRole("button", { name: "Open library" }).tap();

    const library = page.getByTestId("chat-tools-library");
    await expect(library).toBeVisible();
    await expect(library.getByLabel("Search tools")).not.toBeFocused();
    await expect(library.getByLabel("Search tools")).toBeVisible();
    // A hardware keyboard's Shift+Tab from the focused sheet stays inside it.
    await page.keyboard.press("Shift+Tab");
    expect(await library.evaluate((el) => el.contains(document.activeElement))).toBe(true);
    // Rows are at least 44px tall, so neighbouring switches' touch boxes never overlap.
    const heights = await library
      .locator('[data-testid^="chat-tools-library-row-"]')
      .evaluateAll((rows) => rows.map((r) => r.getBoundingClientRect().height));
    expect(Math.min(...heights)).toBeGreaterThanOrEqual(44);
    // The row's tap target is its full height, and the filter chips are 44px.
    const tapHeights = await library
      .locator('[data-testid^="chat-tools-library-row-"] > button')
      .evaluateAll((btns) => btns.map((b) => b.getBoundingClientRect().height));
    expect(Math.min(...tapHeights)).toBeGreaterThanOrEqual(44);
    const chip = (await library.getByRole("navigation", { name: "Categories" }).getByRole("button").first().boundingBox())!;
    expect(chip.height).toBeGreaterThanOrEqual(44);
    // The close button stays in the top-right corner when the admin link wraps.
    const close = (await page.getByRole("button", { name: "Close tools library" }).boundingBox())!;
    expect(390 - (close.x + close.width)).toBeLessThan(24);
    expect(close.y).toBeLessThan(24);

    // Tapping a row opens its detail in place of the list, with a way back.
    await library.getByTestId("chat-tools-library-row-magnite").getByRole("button").first().tap();
    const detail = page.getByTestId("chat-tools-library-detail");
    await expect(detail).toBeInViewport();
    await expect(library.getByRole("region", { name: "Connections" })).toBeHidden();
    await page.screenshot({ path: "test-results/tools-library-phone-detail.png" });
    await page.getByRole("button", { name: "All connections" }).tap();
    await expect(library.getByTestId("chat-tools-library-row-magnite")).toBeInViewport();

    // Escape steps back from the detail before it closes the library.
    await library.getByTestId("chat-tools-library-row-indexexchange").getByRole("button").first().tap();
    await page.keyboard.press("Escape");
    await expect(library.getByRole("region", { name: "Connections" })).toBeVisible();
    await expect(library).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(library).toHaveCount(0);
  });

  test("account menu options are 44px touch targets", async ({ page, context }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await loginViaCookie(context);
    await boot(page, { admin: true });
    await page.getByRole("button", { name: "Tools", exact: true }).tap();
    await page.getByTestId("mcp-seat-indexexchange").tap();
    const list = page.getByRole("listbox", { name: "Account for Index Exchange" });
    await expect(list).toBeVisible();
    const heights = await list.getByRole("option").evaluateAll((o) => o.map((n) => n.getBoundingClientRect().height));
    expect(heights.length).toBeGreaterThan(1);
    expect(Math.min(...heights)).toBeGreaterThanOrEqual(44);
  });

  test("a hardware keyboard on a touch device still lands in search", async ({ page, context }) => {
    await page.setViewportSize({ width: 820, height: 1180 });
    await loginViaCookie(context);
    await boot(page, { admin: true });
    await page.getByRole("button", { name: "Tools", exact: true }).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("textbox", { name: "Search tools" })).toBeFocused();
  });

  test("switching a connector out of the status filter keeps its detail open", async ({ page, context }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await loginViaCookie(context);
    await boot(page, { admin: true });
    await page.getByRole("button", { name: "Tools", exact: true }).tap();
    await page.getByRole("button", { name: "Open library" }).tap();
    const library = page.getByTestId("chat-tools-library");
    await library.getByRole("navigation", { name: "Status" }).getByRole("button", { name: /^On/ }).tap();
    await library.getByTestId("chat-tools-library-row-indexexchange").locator("> button").tap();
    const detail = page.getByTestId("chat-tools-library-detail");
    await expect(detail).toContainText("Index Exchange");
    await detail.getByRole("switch", { name: "Index Exchange" }).tap();
    await expect(detail).toContainText("Index Exchange");
    await expect(detail).toContainText("Off for this chat");
  });
});

test("narrow window: opening a connector from the menu focuses the detail's back button", async ({ page, context }) => {
  await page.setViewportSize({ width: 600, height: 800 });
  await loginViaCookie(context);
  await boot(page, { admin: true });
  await page.getByRole("button", { name: "Tools", exact: true }).click();
  await page.getByRole("textbox", { name: "Search tools" }).fill("bigquery");
  await page.getByRole("button", { name: /Set up/ }).click();
  const library = page.getByTestId("chat-tools-library");
  await expect(library.getByTestId("chat-tools-library-detail")).toBeInViewport();
  await expect(page.getByRole("button", { name: "All connections" })).toBeFocused();
});

test("tablet: a rail filter beside an open detail returns to the filtered list", async ({ page, context }) => {
  await page.setViewportSize({ width: 820, height: 1180 });
  await loginViaCookie(context);
  await boot(page, { admin: true });
  await page.getByRole("button", { name: "Tools", exact: true }).click();
  await page.getByRole("button", { name: "Open library" }).click();
  const library = page.getByTestId("chat-tools-library");
  await library.getByTestId("chat-tools-library-row-magnite").getByRole("button").first().click();
  await expect(library.getByRole("region", { name: "Connections" })).toBeHidden();
  // Magnite is off, so "On" filters it out: the list comes back, filtered.
  await library.getByRole("navigation", { name: "Status" }).getByRole("button", { name: /^On/ }).click();
  await expect(library.getByRole("region", { name: "Connections" })).toBeVisible();
  await expect(library.getByTestId("chat-tools-library-row-magnite")).toHaveCount(0);
  await expect(library.getByTestId("chat-tools-library-row-indexexchange")).toBeVisible();
  // A search from the rail does the same, even one that matches nothing.
  await library.getByTestId("chat-tools-library-row-indexexchange").getByRole("button").first().click();
  await expect(library.getByRole("region", { name: "Connections" })).toBeHidden();
  await library.getByLabel("Search tools").fill("zzz");
  await expect(library.getByRole("region", { name: "Connections" })).toBeVisible();
  await expect(library).toContainText("No tools match ‘zzz’");
});
