import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The rail's hand-set chat orders and the Temporary sort
// (docs/CHAT-ORDER.md): the Pinned section and each project follow the
// order saved on the server, "Reorder" turns them into a list that can be
// dragged or moved with arrows, each move is saved with PUT
// /api/conversations/order for its scope, Temporary chats follow the chosen
// sort (remembered on the device), and the list passes axe in both themes.

const CONVERSATIONS = [
  { id: "c1", title: "Acme Renewal", persona: "default", model: "", pinned: true, updated_at: 40 },
  { id: "c2", title: "Omnicom Pacing", persona: "default", model: "", pinned: true, updated_at: 30 },
  { id: "c3", title: "Deal Desk Daily", persona: "default", model: "", pinned: true, updated_at: 25 },
  { id: "c4", title: "Loose Recent", persona: "default", model: "", pinned: false, updated_at: 10, created_at: 1 },
  { id: "c7", title: "Budget Notes", persona: "default", model: "", pinned: false, updated_at: 9, created_at: 8 },
  { id: "c5", title: "Roadmap Kickoff", persona: "default", model: "", pinned: false, updated_at: 8, project_id: "p1" },
  { id: "c6", title: "Roadmap Q3", persona: "default", model: "", pinned: false, updated_at: 7, project_id: "p1" },
];

const PROJECT = {
  id: "p1",
  owner_email: "e2e@example.com",
  name: "Roadmap",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
};

type Put = { scope: string; ids: string[] };

async function setup(
  page: Page,
  saved: string[],
  opts: {
    role?: string;
    holdFirstRead?: Promise<void>;
    waitForPinned?: boolean;
    projectOrder?: string[];
    // /api/me answers 500, so the role stays unknown.
    meFails?: boolean;
    // The first PUT waits for this, then fails with a 500.
    holdThenFailFirstPut?: Promise<void>;
  } = {},
) {
  const puts: Put[] = [];
  const reads = { n: 0 };
  await mockChatBoot(page);
  await page.route("**/api/me", (r: Route) =>
    opts.meFails
      ? r.fulfill({ status: 500, body: "down" })
      : r.fulfill({ json: { email: "e2e@example.com", role: opts.role ?? "member" } }),
  );
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [PROJECT] } }));
  await page.route("**/api/conversations", (r: Route) =>
    r.fulfill({ json: r.request().method() === "GET" ? { conversations: CONVERSATIONS } : {} }),
  );
  await page.route("**/api/conversations/*", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "c1";
    const conv = CONVERSATIONS.find((c) => c.id === id) ?? CONVERSATIONS[0];
    return r.fulfill({ json: { conversation: conv, history: [] } });
  });
  const orders = () => ({
    orders: {
      ...(saved.length ? { pinned: saved } : {}),
      ...(opts.projectOrder ? { "project:p1": opts.projectOrder } : {}),
    },
  });
  await page.route("**/api/conversations/order", (r: Route) => {
    if (r.request().method() === "PUT") {
      const body = r.request().postDataJSON() as { scope: string; conversation_ids: string[] };
      puts.push({ scope: body.scope, ids: body.conversation_ids });
      if (opts.holdThenFailFirstPut) {
        const hold = opts.holdThenFailFirstPut;
        opts.holdThenFailFirstPut = undefined;
        return hold.then(() => r.fulfill({ status: 500, body: "down" }));
      }
      return r.fulfill({ json: { scope: body.scope, conversation_ids: body.conversation_ids } });
    }
    if (opts.holdFirstRead) {
      const hold = opts.holdFirstRead;
      opts.holdFirstRead = undefined;
      return hold.then(() => r.fulfill({ json: orders() }));
    }
    reads.n++;
    return r.fulfill({ json: orders() });
  });
  await page.goto("/chat");
  if (opts.waitForPinned === false) {
    await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  } else {
    await page.getByTestId("pinned-section").waitFor({ timeout: 15_000 });
  }
  return Object.assign(puts, { reads });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("pinned chats follow the saved order and can be reordered with arrows and drag", async ({ page }) => {
  const puts = await setup(page, ["c3", "c1", "c2"]);
  const section = page.getByTestId("pinned-section");
  // Saved order: Deal Desk Daily, Acme Renewal, Omnicom Pacing.
  const order = async () => {
    const text = (await section.innerText()).replace(/\s+/g, " ");
    return ["Deal Desk Daily", "Acme Renewal", "Omnicom Pacing"].sort((a, b) => text.indexOf(a) - text.indexOf(b));
  };
  await expect.poll(order).toEqual(["Deal Desk Daily", "Acme Renewal", "Omnicom Pacing"]);

  await page.getByTestId("pinned-reorder-toggle").click();
  const list = page.getByTestId("chat-reorder-list");
  await expect(list).toBeVisible();

  // Arrows: Omnicom Pacing up one place.
  await list.getByRole("button", { name: "Move Omnicom Pacing up" }).click();
  await expect.poll(() => puts.at(-1)?.ids).toEqual(["c3", "c2", "c1"]);
  expect(puts.at(-1)?.scope).toBe("pinned");
  await expect(page.getByRole("status").filter({ hasText: "moved to position" })).toHaveText(
    "Omnicom Pacing moved to position 2 of 3.",
  );

  // Drag: Acme Renewal to the top.
  await page
    .getByTestId("chat-reorder-item-c1")
    .locator("div[draggable]")
    .dragTo(page.getByTestId("chat-reorder-item-c3").locator("div[draggable]"));
  await expect.poll(() => puts.at(-1)?.ids).toEqual(["c1", "c3", "c2"]);

  await page.getByTestId("chat-reorder-done").click();
  await expect(list).toBeHidden();
  await expect.poll(order).toEqual(["Acme Renewal", "Deal Desk Daily", "Omnicom Pacing"]);
});

test("a slow first read of the saved order does not undo a move made meanwhile", async ({ page }) => {
  let release: () => void = () => {};
  const held = new Promise<void>((r) => (release = r));
  const puts = await setup(page, ["c3", "c2", "c1"], { holdFirstRead: held });
  await page.getByTestId("pinned-reorder-toggle").click();
  // Server order unknown yet: the rail shows newest first (c1, c2, c3).
  await page.getByRole("button", { name: "Move Omnicom Pacing up" }).click();
  await expect.poll(() => puts.at(-1)?.ids).toEqual(["c2", "c1", "c3"]);
  release(); // the stale read now answers with the old saved order
  await page.waitForTimeout(300);
  const items = page.getByTestId("chat-reorder-list").locator("li");
  await expect(items).toHaveText([/Omnicom Pacing/, /Acme Renewal/, /Deal Desk Daily/]);
});

test("Reorder waits for the account role: an unreadable role offers no mode", async ({ page }) => {
  await setup(page, [], { meFails: true });
  await expect(page.getByTestId("pinned-section")).toContainText("Acme Renewal");
  await page.waitForTimeout(300);
  await expect(page.getByTestId("pinned-reorder-toggle")).toHaveCount(0);
});

test("a failed save followed by a queued move keeps the later move (no stale recovery)", async ({ page }) => {
  let release: () => void = () => {};
  const held = new Promise<void>((r) => (release = r));
  const puts = await setup(page, [], { holdThenFailFirstPut: held });
  const readsBefore = puts.reads.n;
  await page.getByTestId("pinned-reorder-toggle").click();
  const list = page.getByTestId("chat-reorder-list");
  // Newest first: Acme, Omnicom, Deal Desk. Move 1 (its PUT will fail):
  await list.getByRole("button", { name: "Move Omnicom Pacing up" }).click();
  // Move 2, queued behind move 1's save:
  await list.getByRole("button", { name: "Move Deal Desk Daily up" }).click();
  release(); // move 1's save fails now; move 2's save then succeeds
  await expect.poll(() => puts.length).toBe(2);
  await page.waitForTimeout(300);
  // The rail keeps move 2's order — the one the server now holds — and no
  // recovery read was made, since the last save succeeded.
  await expect(list.locator("li")).toHaveText([/Omnicom Pacing/, /Deal Desk Daily/, /Acme Renewal/]);
  expect(puts.at(-1)?.ids).toEqual(["c2", "c3", "c1"]);
  expect(puts.reads.n).toBe(readsBefore);
});

test("a read-only viewer is not offered Reorder", async ({ page }) => {
  await setup(page, [], { role: "viewer" });
  await expect(page.getByTestId("pinned-section")).toContainText("Acme Renewal");
  await page.waitForTimeout(300);
  await expect(page.getByTestId("pinned-reorder-toggle")).toHaveCount(0);
});

test("Escape leaves reorder mode without closing the rail on a narrow screen", async ({ page }) => {
  await page.setViewportSize({ width: 840, height: 800 });
  await setup(page, [], { waitForPinned: false });
  // At ≤900px the rail starts as an icon strip; open it as an overlay.
  const rail = page.locator("aside").first();
  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeGreaterThan(290);
  await page.getByTestId("pinned-reorder-toggle").click();
  await expect(page.getByTestId("chat-reorder-list")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("chat-reorder-list")).toHaveCount(0);
  await expect(page.getByTestId("pinned-reorder-toggle")).toBeVisible();
  await expect(page.getByTestId("pinned-reorder-toggle")).toBeFocused();
  // The overlay is still open: Escape belonged to reorder mode alone.
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeGreaterThan(290);
});

test("a project's chats follow its saved order and reorder from the project's menu", async ({ page }) => {
  const puts = await setup(page, [], { projectOrder: ["c6", "c5"] });
  const options = page.getByRole("button", { name: "Project options for Roadmap" });
  await options.click();
  await page.getByRole("menuitem", { name: "Reorder chats" }).click();
  const list = page.getByRole("list", { name: "Chats in Roadmap, in order" });
  // The saved order applies: Q3 above Kickoff.
  await expect(list.locator("li")).toHaveText([/Roadmap Q3/, /Roadmap Kickoff/]);
  await list.getByRole("button", { name: "Move Roadmap Kickoff up" }).click();
  await expect.poll(() => puts.at(-1)).toEqual({ scope: "project:p1", ids: ["c5", "c6"] });
  await expect(list.locator("li")).toHaveText([/Roadmap Kickoff/, /Roadmap Q3/]);
  await page.getByTestId("chat-reorder-done").click();
  await expect(list).toHaveCount(0);
  await expect(options).toBeFocused();
});

test("Temporary chats follow the chosen sort, remembered on this device", async ({ page }) => {
  await setup(page, []);
  const y = async (id: string) => (await page.locator(`[data-conversation-id="${id}"]`).first().boundingBox())?.y ?? 0;
  const loose = "c4"; // "Loose Recent": more recent activity, started earlier
  const budget = "c7"; // "Budget Notes"
  await expect.poll(async () => (await y(loose)) < (await y(budget))).toBe(true); // Recent activity (default)
  const sort = page.getByRole("combobox", { name: "Sort temporary chats" });
  await sort.selectOption("title");
  await expect.poll(async () => (await y(budget)) < (await y(loose))).toBe(true); // Title A–Z
  await sort.selectOption("created");
  await expect.poll(async () => (await y(budget)) < (await y(loose))).toBe(true); // Newest first
  await page.reload();
  await expect(page.getByRole("combobox", { name: "Sort temporary chats" })).toHaveValue("created");
});

for (const scheme of ["light", "dark"] as const) {
  test(`reorder mode has no axe violations (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await setup(page, []);
    await page.getByTestId("pinned-reorder-toggle").click();
    await expect(page.getByTestId("chat-reorder-list")).toBeVisible();
    const results = await new AxeBuilder({ page })
      .include('[data-testid="pinned-section"]')
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();
    const summary = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    );
    expect(summary, summary.join("\n")).toEqual([]);
  });
}
