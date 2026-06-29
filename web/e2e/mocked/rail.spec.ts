import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the unified navigation rail (#169) + conversation organization
// (#258/#279): the rail shows the Chat/Operations Center nav with the active
// surface marked, derives Folders/Labels sections from the conversation list,
// filters by folder, exposes the per-row kebab, and the chat account menu
// carries Theme + Sign out but NOT Settings (Settings lives only in the
// Operations Center account menu). All /api/* calls are intercepted.

const CONVERSATIONS = [
  { id: "c1", title: "Acme Renewal", persona: "default", model: "", pinned: true, updated_at: 40, folder: "Clients", labels: ["client", "urgent"] },
  { id: "c2", title: "Omnicom Pacing", persona: "default", model: "", pinned: true, updated_at: 30, folder: "Clients", labels: ["client"] },
  { id: "c3", title: "Schema Notes", persona: "default", model: "", pinned: false, updated_at: 20, labels: ["research"] },
  { id: "c4", title: "Loose Recent", persona: "default", model: "", pinned: false, updated_at: 10 },
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

test("the account menu carries Theme + Sign out but not Settings on chat", async ({ page }) => {
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
  await expect(page.getByRole("menuitem", { name: "Settings" })).toHaveCount(0);
});

test("the rail derives Folders + Labels sections and filters by folder", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();

  // Folders + Labels sections materialize from the conversation list. Filed
  // conversations live in their folder, so Recent shows only the loose ones.
  await expect(bar.getByRole("button", { name: /Clients/ })).toBeVisible();
  await expect(bar.getByText("Loose Recent", { exact: true })).toBeVisible();
  await expect(bar.getByText("Acme Renewal", { exact: true })).toHaveCount(0);

  // Filtering by the folder reveals its conversations and a removable filter chip.
  await bar.getByRole("button", { name: /Clients/ }).first().click();
  await expect(bar.getByText(/Folder:/)).toBeVisible();
  await expect(bar.getByText("Acme Renewal", { exact: true })).toBeVisible();
  await expect(bar.getByText("Omnicom Pacing", { exact: true })).toBeVisible();
  await expect(bar.getByText("Loose Recent", { exact: true })).toHaveCount(0);

  // Clearing restores the sectioned view.
  await bar.getByRole("button", { name: "Clear" }).click();
  await expect(bar.getByText("Loose Recent", { exact: true })).toBeVisible();
});

test("the per-row kebab exposes pin / rename / folder / labels / archive / delete", async ({ page }) => {
  await mockChatBoot(page);
  await mockConversations(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const bar = page.locator("aside").first();
  await bar.getByRole("button", { name: "Conversation options for Loose Recent" }).click();
  const menu = page.getByRole("menu", { name: "Options for Loose Recent" });
  await expect(menu).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Pin Loose Recent" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Rename" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Add to folder" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Labels" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Share Loose Recent" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Archive Loose Recent" })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Delete Loose Recent" })).toBeVisible();

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
