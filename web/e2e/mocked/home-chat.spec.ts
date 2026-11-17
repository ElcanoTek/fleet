import { test, expect, type Page, type Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The Home chat (docs/HOME-CHAT.md): the first row of the chat list, marked
// Home; a tab's first visit lands on it; a reload in the same tab opens the
// most recent chat as before; a link outranks Home; and "Set as home" in a
// row's menu saves the choice with PUT /api/me/home.

const CONVERSATIONS = [
  { id: "recent", title: "Most Recent", persona: "default", model: "", pinned: false, updated_at: 50 },
  { id: "home", title: "Morning Patrol", persona: "default", model: "", pinned: true, updated_at: 40 },
  { id: "other", title: "Budget Notes", persona: "default", model: "", pinned: false, updated_at: 30 },
];

const HOME = { conversation_id: "home", access: "owner", title: "Morning Patrol", owner_email: "e2e@example.com" };

async function setup(page: Page, opts: { home?: typeof HOME | null } = {}) {
  const opened: string[] = [];
  const puts: string[] = [];
  let home = opts.home === undefined ? HOME : opts.home;
  await mockChatBoot(page);
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  // The active list (bare path) carries "home"; the query-string reads stay
  // with mockChatBoot.
  await page.route("**/api/conversations", (r: Route) =>
    r.fulfill({ json: r.request().method() === "GET" ? { conversations: CONVERSATIONS, home } : {} }),
  );
  await page.route("**/api/conversations/*", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "";
    if (id === "order") return r.fulfill({ json: { orders: {} } });
    opened.push(id);
    const conv = CONVERSATIONS.find((c) => c.id === id) ?? CONVERSATIONS[0];
    return r.fulfill({ json: { conversation: conv, history: [] } });
  });
  await page.route("**/api/me/home", (r: Route) => {
    if (r.request().method() === "PUT") {
      const id = (r.request().postDataJSON() as { conversation_id: string }).conversation_id;
      puts.push(id);
      const conv = CONVERSATIONS.find((c) => c.id === id);
      home = { conversation_id: id, access: "owner", title: conv?.title ?? "", owner_email: "e2e@example.com" };
    } else if (r.request().method() === "DELETE") {
      home = null;
    }
    return r.fulfill({ json: { home } });
  });
  return { opened, puts };
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("a tab's first visit lands on Home; a reload opens the most recent chat", async ({ page }) => {
  const { opened } = await setup(page);
  await page.goto("/chat");
  const homeSection = page.getByTestId("home-section");
  await expect(homeSection).toContainText("Morning Patrol");
  await expect(homeSection.getByTestId("home-badge")).toBeVisible();
  await expect(page.getByTestId("nav-home")).toHaveCount(0);
  await expect.poll(() => opened[0]).toBe("home");

  opened.length = 0;
  await page.reload();
  await expect.poll(() => opened[0]).toBe("recent");

  // The Home row opens it again.
  await homeSection.getByText("Morning Patrol", { exact: true }).click();
  await expect.poll(() => opened.at(-1)).toBe("home");
});

test("a link to a chat outranks Home", async ({ page }) => {
  const { opened } = await setup(page);
  await page.goto("/chat?c=other");
  await expect.poll(() => opened[0]).toBe("other");
});

test("Set as home in a row's menu saves the choice and moves the Home row", async ({ page }) => {
  const { puts } = await setup(page, { home: null });
  await page.goto("/chat");
  await expect(page.getByTestId("home-section")).toHaveCount(0);

  const row = page.locator('[data-conversation-id="other"]');
  await row.hover();
  await row.getByRole("button", { name: "Conversation options for Budget Notes" }).click();
  await page.getByRole("menuitem", { name: "Set as home" }).click();
  await expect.poll(() => puts).toEqual(["other"]);
  await expect(page.getByTestId("home-section")).toContainText("Budget Notes");
  await expect(page.getByText("Budget Notes is your home chat now. Fleet opens it first.")).toBeVisible();
});
