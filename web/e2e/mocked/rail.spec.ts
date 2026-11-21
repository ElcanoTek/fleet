import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the sidebar frame (#169) + conversation organization
// (#258/#279) + the collapsible rail / select mode: the sidebar's top items
// (Patrols, Background tasks) mark the page that is open, the Chats menu
// filters by label and starts select mode, the per-row ⋯ menu holds a chat's
// actions, the sidebar collapses to an icon strip with a persisted
// preference, auto-collapses at ≤900px with an overlay+scrim expansion, the +
// menu offers a sealed chat where the deployment has them, and the account
// menu carries the unified settings navigation (Settings · Connections ·
// Skills · Admin) plus Theme + Sign out identically on every page. All
// /api/* calls are intercepted.

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

test("the sidebar's top items lead to Patrols and Background tasks and mark the open page", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // The top of the sidebar names the other ways of working: Patrols (a page
  // of patrol chats) and Background tasks (the Operations Center). Chat has
  // no row of its own: the pen beside the search field starts one, and every
  // chat row leads back to it.
  await expect(page.getByTestId("nav-to-chat")).toHaveCount(0);
  await expect(page.getByTestId("new-chat-button")).toBeVisible();
  await expect(page.getByTestId("nav-to-patrols")).toHaveText(/Patrols/);
  await expect(page.getByTestId("nav-to-orchestrator")).toContainText("Background tasks");
  // On Chat neither is the current page.
  await expect(page.getByTestId("nav-to-patrols")).not.toHaveAttribute("aria-current", "page");
  await expect(page.getByTestId("nav-to-orchestrator")).not.toHaveAttribute("aria-current", "page");

  // Patrols is a page, with the same sidebar and its row marked.
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: [] } }));
  await page.getByTestId("nav-to-patrols").click();
  await expect(page).toHaveURL(/\/patrols$/);
  await expect(page.getByTestId("nav-to-patrols")).toHaveAttribute("aria-current", "page");
  await expect(page.getByTestId("new-chat-button")).toBeVisible();
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
  // The account menu groups Workspace (Settings, Guides) and Model Context & Tools
  // (Context & Connections), while Administration is hidden for non-admins.
  const settingsItem = page.getByTestId("nav-to-settings");
  await expect(settingsItem).toBeVisible();
  await expect(settingsItem).toContainText("General preferences & team settings");
  const contextItem = page.getByTestId("nav-to-context-settings");
  await expect(contextItem).toBeVisible();
  await expect(contextItem).toContainText("Connections, memories, skills, personas & files");
  await expect(page.getByRole("menuitem", { name: "Connections", exact: true })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "Skills", exact: true })).toHaveCount(0);
  await expect(page.getByTestId("nav-to-admin-settings")).toHaveCount(0);
});

test("Sign out asks first in a centred dialog, then signs out", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const openConfirm = async () => {
    await page.getByTestId("account-menu-button").click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    const dialog = page.getByRole("dialog", { name: "Sign out?" });
    await expect(dialog).toBeVisible();
    return dialog;
  };

  // Centred in the viewport, not pinned to the rail it was opened from.
  const dialog = await openConfirm();
  // Measured after the enter animation, which slides the panel in.
  await dialog.evaluate((el) => Promise.all(el.getAnimations().map((a) => a.finished)));
  const box = await dialog.boundingBox();
  const viewport = page.viewportSize();
  expect(box && viewport).toBeTruthy();
  if (box && viewport) {
    expect(Math.abs(box.x + box.width / 2 - viewport.width / 2)).toBeLessThan(2);
    expect(Math.abs(box.y + box.height / 2 - viewport.height / 2)).toBeLessThan(2);
  }

  // Cancel keeps the session.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByRole("dialog", { name: "Sign out?" })).toHaveCount(0);
  await expect(page).toHaveURL(/\/chat/);

  // Confirming posts the logout form; with no central Auth configured the
  // gateway lands on the manual sign-in page.
  const confirm = await openConfirm();
  await Promise.all([
    page.waitForURL((url) => url.pathname === "/login" && url.searchParams.get("manual") === "1" && url.searchParams.get("lang") === "en"),
    confirm.getByRole("button", { name: "Sign out" }).click(),
  ]);
});

// The confirm must sit above the rail wherever the rail floats over the page:
// the 640-900px overlay (scrim z-[380], rail z-[390]) and the <640px drawer.
// The hit test proves a click on Sign out reaches the button, not the scrim.
for (const layout of [
  { name: "the expanded overlay rail at 800px", width: 800, height: 900, open: "Expand sidebar", openState: "Collapse sidebar" },
  { name: "the phone drawer", width: 390, height: 844, open: "Open sidebar", openState: "Close sidebar" },
]) {
  test(`the Sign out confirm sits above ${layout.name}`, async ({ page }) => {
    await page.setViewportSize({ width: layout.width, height: layout.height });
    await mockChatBoot(page);
    await mockConversations(page);
    await page.goto("/chat");
    await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
    await page.getByRole("button", { name: layout.open, exact: true }).click();
    await page.getByTestId("account-menu-button").click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    const dialog = page.getByRole("dialog", { name: "Sign out?" });
    await expect(dialog).toBeVisible();
    await dialog.evaluate((el) => Promise.all(el.getAnimations().map((a) => a.finished)));

    const confirm = dialog.getByRole("button", { name: "Sign out" });
    const hit = await confirm.evaluate((button) => {
      const r = button.getBoundingClientRect();
      return document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2) === button;
    });
    expect(hit).toBe(true);

    // Escape closes only the confirm; the rail it came from stays open.
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByTestId("account-menu-button")).toBeFocused();
    await expect(page.getByTestId("account-menu-button")).toBeInViewport();
    await expect(page.getByRole("button", { name: layout.openState, exact: true })).toBeVisible();
    if (layout.openState === "Collapse sidebar") {
      await expect(page.getByRole("button", { name: layout.openState, exact: true })).toHaveAttribute("aria-expanded", "true");
    }

    await page.getByTestId("account-menu-button").click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await Promise.all([
      page.waitForURL((url) => url.pathname === "/login" && url.searchParams.get("manual") === "1" && url.searchParams.get("lang") === "en"),
      page.getByRole("dialog", { name: "Sign out?" }).getByRole("button", { name: "Sign out" }).click(),
    ]);
  });
}

test("the Chats menu filters the list by label", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await expect(bar.getByText("Loose Recent", { exact: true })).toBeVisible();
  await expect(bar.getByText("Acme Renewal", { exact: true })).toBeVisible();

  // The labels the person uses are derived from the conversation list and
  // offered in the Chats menu, each with its count. (There is no standing
  // Labels section in the sidebar.)
  await page.getByTestId("recent-sort").click();
  await page.getByTestId("chats-menu-labels").click();
  const labels = page.getByRole("menu", { name: "Filter by label" });
  await expect(labels.getByRole("menuitemcheckbox")).toHaveText([/client\s*2/, /research\s*1/, /urgent\s*1/]);

  // Filtering by a label keeps the matches and shows a removable filter chip.
  await labels.getByRole("menuitemcheckbox", { name: /client/ }).click();
  await expect(labels.getByRole("menuitemcheckbox", { name: /client/ })).toHaveAttribute("aria-checked", "true");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
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
  // A member whose saved sidebar has loaded: the menu then also offers the
  // two sidebar items (Move to the top, Set as start page).
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  await page.route("**/api/sidebar", (r: Route) => r.fulfill({ json: { layout: null, workspace_default: null } }));
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await expect(page.locator("main").getByText("Acme Renewal")).toBeVisible();
  const menu = page.getByRole("menu", { name: "Options for Loose Recent" });
  await bar.getByRole("button", { name: "Conversation options for Loose Recent" }).click();
  await expect(menu).toBeVisible();
  // Exact set + order (#169 audit fix #3): plain verbs, no conversation
  // title, grouped: where the chat sits · start page · what it can become ·
  // who else gets it · putting it away. One Share entry, not three: the
  // dialog it opens holds both audiences — share by link and share with team
  // (ADR-0057) — so the menu does not fork on whether a public link happens
  // to exist. Select is the Chats menu's, not a row's. (No "Move to project"
  // here: this account has no projects.)
  await expect(menu.getByRole("menuitem")).toHaveText([
    "Pin",
    "Move to the top",
    "Rename",
    "Labels",
    "Set as start page",
    "Add a patrol…",
    "Make a background task…",
    "Save as workflow…",
    "Share…",
    "Download chat…",
    "Transfer ownership…",
    "Archive",
    "Delete…",
  ]);
  await expect(menu.getByRole("menuitem", { name: "Select…", exact: true })).toHaveCount(0);
  // Four dividers, one between each group.
  await expect(menu.getByRole("separator")).toHaveCount(4);
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
  const anchor = bar.getByRole("button", { name: "Conversation options for Loose Recent" });
  await anchor.click();
  await expect(menu).toBeVisible();

  // The click can scroll an off-screen row into view before opening its menu,
  // with the resulting scroll event delivered afterward. Reproduce that
  // delayed delivery deterministically: geometry is already current, so this
  // event must not close the newly positioned menu. Real subsequent scrolling
  // that moves the anchor still dismisses it (Menu's unit regression).
  await anchor.evaluate((button) => {
    for (let parent = button.parentElement; parent; parent = parent.parentElement) {
      if (getComputedStyle(parent).overflowY === "auto") {
        parent.dispatchEvent(new Event("scroll"));
        return;
      }
    }
    throw new Error("conversation row has no scrolling ancestor");
  });
  await expect(menu).toBeVisible();

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

test("Select chats… enters select mode with checkboxes + the bulk icon bar; Escape exits", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await expect(page.locator("main").getByText("Acme Renewal")).toBeVisible();
  // Select mode starts from the Chats menu, with nothing selected yet.
  await page.getByTestId("recent-sort").click();
  await page.getByTestId("chats-menu-select").click();

  // The bulk bar appears; with nothing selected its actions are off.
  await expect(bar.getByText("0 selected")).toBeVisible();
  const deleteBtn = bar.getByRole("button", { name: "Delete selected" });
  await expect(deleteBtn).toBeDisabled();
  await bar.getByRole("button", { name: /Loose Recent/ }).click();
  await expect(bar.getByText("1 selected")).toBeVisible();
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

test("the + menu offers a sealed chat where the deployment has them, and says what sealed means", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // Without sealed chats the menu does not mention them.
  await page.getByTestId("new-menu-button").click();
  const menu = page.getByRole("menu", { name: "New" });
  await expect(menu.getByRole("menuitem")).toHaveText([/^New chat/, /^New patrol/, /^New background task/, "New project"]);
  await page.keyboard.press("Escape");

  // Re-stub server-config (the most recent route wins) to expose them.
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({ json: { lockdown_available: true, lockdown_only: false, lockdown_allowed_models: [] } }),
  );
  await page.reload();
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByTestId("new-menu-button").click();
  const sealed = page.getByTestId("new-menu-sealed");
  await expect(sealed).toHaveText("New sealed chat");
  // The explanation rides the item (its description for a pointer and for
  // assistive tech), where the standalone sealed button used to carry it.
  await expect(sealed).toHaveAttribute("title", /Locked down and private/);
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
