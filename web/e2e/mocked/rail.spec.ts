import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the unified navigation rail (#169) + conversation organization
// (#258/#279) + the collapsible rail / select mode (UI polish): the rail shows
// the Chat/Operations Center nav with the active surface marked, derives the
// Labels section from the conversation list, filters by label,
// exposes the per-row kebab (whose "Select…" is the only way into select
// mode), collapses to a 4.25rem icon strip with a persisted preference,
// auto-collapses at ≤900px with an overlay+scrim expansion, explains the
// sealed-row lock on hover/focus, and the account menu carries the unified
// settings navigation (Settings · Connections · Skills · Admin) plus Theme +
// Sign out identically on both surfaces. All /api/* calls are intercepted.

const CONVERSATIONS = [
  { id: "c1", title: "Acme Renewal", persona: "default", model: "", pinned: true, updated_at: 40, labels: ["client", "urgent"] },
  { id: "c2", title: "Omnicom Pacing", persona: "default", model: "", pinned: true, updated_at: 30, labels: ["client"] },
  { id: "c3", title: "Schema Notes", persona: "default", model: "", pinned: false, updated_at: 20, labels: ["research"] },
  { id: "c4", title: "Loose Recent", persona: "default", model: "", pinned: false, updated_at: 10 },
  { id: "c5", title: "Sealed Deal", persona: "default", model: "", pinned: false, updated_at: 5, lockdown: true },
];

async function mockConversations(page: Page) {
  await page.route("**/api/conversations", (r: Route) => {
    if (r.request().method() === "GET") return r.fulfill({ json: { conversations: CONVERSATIONS } });
    return r.fulfill({ json: {} });
  });
  // The shell auto-loads the most-recent conversation on boot; return a minimal
  // detail payload so that load resolves rather than 502-ing.
  await page.route("**/api/conversations/*", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "c1";
    const conv = CONVERSATIONS.find((c) => c.id === id) ?? CONVERSATIONS[0];
    return r.fulfill({ json: { conversation: conv, history: [] } });
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the rail marks the active surface and links to the other", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await expect(page.getByRole("link", { name: "Chat" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("link", { name: "Operations Center" })).toBeVisible();
});

test("the account menu carries Settings (+subtext) + Theme + Sign out on chat", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.getByTestId("account-menu-button").click();
  const menu = page.getByRole("menu", { name: "Account" });
  await expect(menu).toBeVisible();
  await expect(menu).toContainText("e2e@example.com");
  await expect(page.getByRole("group", { name: "Theme" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Sign out" })).toBeVisible();
  // The settings redesign: one Settings item with its subtext line; the
  // sections (Connections/Skills/Admin) live in the settings area's own
  // sub-nav, not the menu.
  const settingsItem = page.getByRole("menuitem", { name: /Settings/ });
  await expect(settingsItem).toBeVisible();
  await expect(settingsItem).toContainText("Connections, skills & workspace settings");
  // exact: Playwright's name matching is substring by default, and the
  // Settings item's accessible name contains "Connections, skills…".
  await expect(page.getByRole("menuitem", { name: "Connections", exact: true })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "Skills", exact: true })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "Admin", exact: true })).toHaveCount(0);
});

test("the rail derives the Labels section and filters by label", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();

  // The Labels section materializes from the conversation list; pinned chats
  // (including previously folder-filed ones — folders are gone) show under
  // Pinned, loose unpinned ones under Temporary.
  await expect(bar.getByRole("button", { name: /client/ }).first()).toBeVisible();
  await expect(bar.getByText("Loose Recent", { exact: true })).toBeVisible();
  await expect(bar.getByText("Acme Renewal", { exact: true })).toBeVisible();

  // Filtering by a label reveals matches and a removable filter chip.
  await bar.getByRole("button", { name: "client 2" }).click();
  await expect(bar.getByText(/Label:/)).toBeVisible();
  await expect(bar.getByText("Acme Renewal", { exact: true })).toBeVisible();
  await expect(bar.getByText("Omnicom Pacing", { exact: true })).toBeVisible();
  await expect(bar.getByText("Loose Recent", { exact: true })).toHaveCount(0);

  // Clearing restores the sectioned view.
  await bar.getByRole("button", { name: "Clear" }).click();
  await expect(bar.getByText("Loose Recent", { exact: true })).toBeVisible();
});

test("the per-row kebab exposes pin / rename / labels / archive / delete", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  // Boot settle, then open the kebab with a self-healing retry: under heavy
  // CI load a late boot re-render can swallow the click's state toggle (the
  // pointer events dispatch, but the row subtree remounts mid-click and the
  // menu never opens). toPass() re-clicks until the menu is actually there —
  // each attempt is click→verify, so it converges regardless of which side
  // of the race an attempt lands on.
  await expect(page.locator("main").getByText("Acme Renewal")).toBeVisible();
  const menu = page.getByRole("menu", { name: "Options for Loose Recent" });
  await expect(async () => {
    await bar.getByRole("button", { name: "Conversation options for Loose Recent" }).click();
    await expect(menu).toBeVisible({ timeout: 1500 });
  }).toPass({ timeout: 15_000 });
  // Exact set + order (#169 audit fix #3): plain verbs, no conversation title.
  await expect(menu.getByRole("menuitem", { name: "Pin", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Rename", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Labels", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Download as JSON", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Share", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Select…", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Archive", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Delete", exact: true })).toBeVisible();
  // Exactly two dividers (after Labels, after Share).
  await expect(menu.getByRole("separator")).toHaveCount(2);
  // No menu item carries the conversation's name.
  await expect(menu.getByRole("menuitem", { name: /Loose Recent/ })).toHaveCount(0);

  // Regression guard: an open menu must survive re-renders of the rail (the
  // conversation list polls/refreshes). The popover renders visibility:hidden in
  // JSX and is revealed imperatively, so it must re-reveal on every commit —
  // otherwise the next refresh blinks it out from under the user.
  await page.waitForTimeout(800);
  await expect(menu).toBeVisible();

  // Regression guard: the menu is portaled to <body> and positioned in viewport
  // coordinates; it must land fully on-screen (a kebab sits near the left edge,
  // and the rail <aside>'s transform would otherwise make `fixed` resolve
  // against the rail, flinging the menu off-screen).
  const box = await menu.boundingBox();
  const vp = page.viewportSize();
  expect(box).not.toBeNull();
  if (box && vp) {
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(vp.width + 1);
    expect(box.y + box.height).toBeLessThanOrEqual(vp.height + 1);
  }
});

test("the kebab labels flyout opens beside the menu, both visible (#169 audit #4)", async ({
  page,
}) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await expect(page.locator("main").getByText("Acme Renewal")).toBeVisible();
  const menu = page.getByRole("menu", { name: "Options for Loose Recent" });
  // Self-healing open (see the kebab test above for why).
  await expect(async () => {
    await bar.getByRole("button", { name: "Conversation options for Loose Recent" }).click();
    await expect(menu).toBeVisible({ timeout: 1500 });
  }).toPass({ timeout: 15_000 });

  // "Labels" opens a flyout BESIDE the menu — the parent stays visible.
  await menu.getByRole("menuitem", { name: "Labels", exact: true }).click();
  const labelsFlyout = page.getByRole("menu", { name: "Labels" });
  await expect(labelsFlyout).toBeVisible();
  await expect(menu).toBeVisible();
  // The flyout sits to the side of the menu, not overlapping it.
  const menuBox = await menu.boundingBox();
  const flyBox = await labelsFlyout.boundingBox();
  expect(menuBox && flyBox).toBeTruthy();
  if (menuBox && flyBox) {
    const disjoint = flyBox.x >= menuBox.x + menuBox.width - 1 || flyBox.x + flyBox.width <= menuBox.x + 1;
    expect(disjoint).toBe(true);
  }

  // Escape closes the flyout but leaves the main menu open.
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu", { name: "Labels" })).toHaveCount(0);
  await expect(menu).toBeVisible();
});

test("the rail collapses to an icon strip, persists, and repositions the account menu", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const rail = page.locator("aside").first();
  await expect(page.getByRole("searchbox", { name: "Search", exact: true })).toBeVisible();

  // Collapse: a 4.25rem (68px) icon strip; wide-only content hides.
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeLessThan(80);
  await expect(page.getByRole("searchbox", { name: "Search", exact: true })).toBeHidden();

  // The account menu still opens — avatar-only anchor; the menu takes the
  // design's 15rem base width, growing to its content (the Theme segmented
  // control) so nothing clips, and lands fully on-screen.
  //
  // Local `next dev` only: the Next.js dev-overlay indicator (<nextjs-portal>,
  // the floating "N" chip) sits bottom-left — exactly over the collapsed
  // rail's account button — and intercepts the click, timing the test out.
  // CI runs `next start` (no overlay), so strip it rather than force-click:
  // force would also mask a REAL overlap regression.
  await page.evaluate(() => document.querySelector("nextjs-portal")?.remove());
  await page.getByTestId("account-menu-button").click();
  const menu = page.getByRole("menu", { name: "Account" });
  await expect(menu).toBeVisible();
  const menuBox = await menu.boundingBox();
  expect(menuBox).not.toBeNull();
  if (menuBox) {
    expect(menuBox.width).toBeGreaterThanOrEqual(239);
    expect(menuBox.width).toBeLessThanOrEqual(321);
    expect(menuBox.x).toBeGreaterThanOrEqual(0);
  }
  // No content overflows the menu box (the collapsed-menu clipping bug).
  const themeBox = await page.getByRole("group", { name: "Theme" }).boundingBox();
  if (menuBox && themeBox) {
    expect(themeBox.x + themeBox.width).toBeLessThanOrEqual(menuBox.x + menuBox.width + 1);
  }
  await page.keyboard.press("Escape");

  // The preference persists across reload.
  await page.reload();
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await expect.poll(async () => (await page.locator("aside").first().boundingBox())?.width ?? 0).toBeLessThan(80);

  // Expand restores the full rail.
  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect(page.getByRole("searchbox", { name: "Search", exact: true })).toBeVisible();
});

test("Select… enters select mode with checkboxes + the bulk icon bar; Escape exits", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await expect(page.locator("main").getByText("Acme Renewal")).toBeVisible();
  // Self-healing open (see the kebab test above for why).
  await expect(async () => {
    await bar.getByRole("button", { name: "Conversation options for Loose Recent" }).click();
    await expect(
      page.getByRole("menu", { name: "Options for Loose Recent" }),
    ).toBeVisible({ timeout: 1500 });
  }).toPass({ timeout: 15_000 });
  await page
    .getByRole("menu", { name: "Options for Loose Recent" })
    .getByRole("menuitem", { name: "Select…", exact: true })
    .click();

  // The bulk bar appears with the seed row selected; actions are enabled.
  await expect(bar.getByText("1 selected")).toBeVisible();
  const deleteBtn = bar.getByRole("button", { name: "Delete selected" });
  await expect(deleteBtn).toBeEnabled();

  // Row clicks toggle selection instead of opening the conversation.
  await bar.getByRole("button", { name: /Schema Notes/ }).click();
  await expect(bar.getByText("2 selected")).toBeVisible();

  // Deselecting everything disables the actions (Cancel stays live).
  await bar.getByRole("button", { name: /Schema Notes/ }).click();
  await bar.getByRole("button", { name: /Loose Recent/ }).click();
  await expect(bar.getByText("0 selected")).toBeVisible();
  await expect(deleteBtn).toBeDisabled();
  await expect(bar.getByRole("button", { name: "Exit selection" })).toBeEnabled();

  // Add-label opens ABOVE the bar (the bulk bar's remaining panel; the
  // Move-to-folder action left with the folders UI).
  await bar.getByRole("button", { name: /Loose Recent/ }).click();
  await bar.getByRole("button", { name: "Add label" }).click();
  const labelsMenu = page.getByRole("menu", { name: "Add label to selected" });
  await expect(labelsMenu).toBeVisible();
  const anchorBox = await bar.getByRole("button", { name: "Add label" }).boundingBox();
  const popBox = await labelsMenu.boundingBox();
  expect(anchorBox && popBox).toBeTruthy();
  if (anchorBox && popBox) expect(popBox.y + popBox.height).toBeLessThanOrEqual(anchorBox.y + 1);

  // Escape closes the popover; a second Escape exits select mode entirely.
  await page.keyboard.press("Escape");
  await expect(labelsMenu).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(bar.getByText(/selected/)).toHaveCount(0);
  await expect(bar.getByRole("button", { name: "Conversation options for Loose Recent" })).toBeAttached();
});

test("the sealed new-chat button explains itself on hover and keyboard focus", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  // Re-stub server-config (the most recent route wins) to expose the sealed
  // affordance the explainer tooltip lives on.
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({ json: { lockdown_available: true, lockdown_only: false, lockdown_allowed_models: [] } }),
  );
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const sealedButton = page.getByRole("button", { name: /New sealed chat/ });
  await expect(sealedButton).toBeVisible();

  await sealedButton.hover();
  await expect(page.getByRole("tooltip")).toContainText(/sealed chat/i);
  await page.mouse.move(600, 400);
  await expect(page.getByRole("tooltip")).toHaveCount(0);

  await sealedButton.focus();
  await expect(page.getByRole("tooltip")).toBeVisible();
});

test("at ≤900px the rail auto-collapses and expands as an overlay with a scrim", async ({ page }) => {
  await page.setViewportSize({ width: 840, height: 800 });
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // Auto-collapsed to the icon strip (the stored preference is untouched).
  const rail = page.locator("aside").first();
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeLessThan(80);

  // Expanding opens the rail as an overlay (full 300px) above a scrim.
  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeGreaterThan(290);
  const scrim = page.locator('button[aria-label="Close navigation"]').last();
  await expect(scrim).toBeVisible();

  // Clicking the scrim dismisses the overlay back to the strip.
  await scrim.click({ position: { x: 700, y: 400 } });
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeLessThan(80);
});
