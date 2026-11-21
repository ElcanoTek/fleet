import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the warning before a temporary chat is deleted
// (docs/CHAT-RETENTION.md). A temporary chat is deleted after a period without
// activity; for the last days of that period its row's one status is the days
// left, such chats are counted under the heading, and the chat itself carries
// a notice with the keep action. A chat with months left says none of this:
// a temporary chat's row is just its title until then.

const DAY = 86_400;
// A row's one status when it is the days left before the chat is deleted.
const EXPIRING = '[data-testid="row-status"][data-status="expiring"]';
const idleFor = (days: number) => Math.floor(Date.now() / 1000) - days * DAY - 3600;

const CONVERSATIONS = [
  { id: "fresh", title: "Started today", updated_at: idleFor(0) },
  { id: "soon", title: "Idle for months", updated_at: idleFor(85) },
  { id: "kept", title: "Pinned and idle", pinned: true, updated_at: idleFor(88) },
];

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("a temporary chat near the end of its window says so in the rail and in the chat, and Pin keeps it", async ({ page }) => {
  await mockChatBoot(page, { conversations: CONVERSATIONS });
  // The deployment's policy (the most recent route wins): 90 days, flagged
  // for the last 14.
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: {
        lockdown_available: false,
        lockdown_only: false,
        lockdown_allowed_models: [],
        conversation_ttl_days: 90,
        conversation_expiry_warning_days: 14,
        conversation_unpinned_cap: 500,
      },
    }),
  );
  const pins: unknown[] = [];
  await page.route("**/api/conversations/soon/pin", (r: Route) => {
    pins.push(r.request().postDataJSON());
    return r.fulfill({ json: {} });
  });
  await page.goto("/chat");

  // The rail: one chat is counted, and only that row carries the countdown.
  const notice = page.getByTestId("expiring-chats-notice");
  await expect(notice).toContainText("1 chat will be deleted soon.");
  await expect(page.locator(EXPIRING)).toHaveCount(1);
  const row = page.locator("aside").getByRole("button", { name: "Idle for months", exact: true });
  await expect(row.locator(EXPIRING)).toHaveText("5d");
  await expect(row).toHaveAccessibleDescription(/deleted in 5 days/);
  // The other temporary chat has months left: its row says nothing.
  await expect(
    page.locator("aside").getByRole("button", { name: "Started today", exact: true }).getByTestId("row-status"),
  ).toHaveCount(0);

  // The count leads to the chat; the chat says how long it has left.
  await notice.click();
  await expect(row).toBeFocused();
  await row.click();
  const banner = page.getByTestId("expiring-chat-banner");
  await expect(banner).toContainText("This temporary chat will be deleted in 5 days.");
  await expect(banner).toContainText("another 90 days");

  const results = await new AxeBuilder({ page })
    .include('[data-testid="expiring-chat-banner"]')
    .include('[data-testid="unified-chat-list"]')
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`)).toEqual([]);

  // Pin to keep: the chat is no longer temporary, so every warning goes.
  await banner.getByRole("button", { name: "Pin to keep" }).click();
  await expect(banner).toHaveCount(0);
  await expect(page.locator(EXPIRING)).toHaveCount(0);
  await expect(notice).toHaveCount(0);
  expect(pins).toEqual([{ pinned: true }]);

  // Unpinning is activity too: the chat is temporary again with a full
  // window, so the old countdown must not come back.
  await page.locator("aside").getByRole("button", { name: "Conversation options for Idle for months" }).click();
  await page.getByRole("menuitem", { name: "Unpin", exact: true }).click();
  await expect.poll(() => pins.length).toBe(2);
  // Back in Chats with the other temporary chat, and no countdown on either.
  await expect(page.getByTestId("unified-chat-list").getByRole("button", { name: "Idle for months", exact: true })).toBeVisible();
  await expect(page.getByTestId("unified-chat-list").getByTestId("row-status")).toHaveCount(0);
  await expect(page.locator(EXPIRING)).toHaveCount(0);
  await expect(banner).toHaveCount(0);
});

test("a chat with time left carries no warning", async ({ page }) => {
  await mockChatBoot(page, { conversations: [CONVERSATIONS[0]] });
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: {
        lockdown_available: false,
        lockdown_only: false,
        lockdown_allowed_models: [],
        conversation_ttl_days: 90,
        conversation_expiry_warning_days: 14,
      },
    }),
  );
  await page.goto("/chat");
  await page.locator("aside").getByRole("button", { name: "Started today", exact: true }).click();
  await expect(page.getByTestId("row-status")).toHaveCount(0);
  await expect(page.getByTestId("expiring-chats-notice")).toHaveCount(0);
  await expect(page.getByTestId("expiring-chat-banner")).toHaveCount(0);
});
