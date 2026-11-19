import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The chat list with a lot in it (docs/SIDEBAR.md, docs/CHAT-ORDER.md): a
// teammate's chat can be pinned for oneself from its menu and joins the one
// Pinned group, which is put in order by dragging (a mouse on the row, a
// finger on the handle) or with arrows; the sort is a compact menu button the
// same size on a phone as on a desktop; and on a phone the drawer scrolls as
// one, so the list is not squeezed into a few rows.

const TOPICS = [
  "Q4 pipeline review",
  "Acme renewal",
  "Omnicom pacing",
  "Deal desk daily",
  "Incident room",
  "Budget notes",
  "Weekly campaign health check for the EMEA region and its partners",
  "Onboarding plan",
  "Churn analysis",
  "Pricing experiment",
];
const OWNERS = ["maria.fernandez-oliveira", "tom", "priya.k"];

type Row = Record<string, unknown> & { id: string; title: string; pinned: boolean };

// 120 chats: two thirds the person's own (the first three pinned), one third
// teammates' (shared with the team; every other one Multiplayer too).
function seed(): { own: Row[]; team: Row[] } {
  const own: Row[] = [];
  const team: Row[] = [];
  for (let i = 0; i < 120; i++) {
    const n = Math.floor(i / TOPICS.length);
    const title = `${TOPICS[i % TOPICS.length]}${n ? ` ${n + 1}` : ""}`;
    const times = { updated_at: 1_700_000_000 - i * 60, created_at: 1_690_000_000 + i };
    if (i % 3 === 1) {
      team.push({
        id: `t${i}`,
        title,
        persona: "default",
        model: "test-model",
        user_email: `${OWNERS[i % OWNERS.length]}@example.com`,
        team_visible: true,
        multiplayer: i % 2 === 0,
        pinned: false,
        ...times,
      });
    } else {
      own.push({ id: `c${i}`, title, persona: "default", model: "test-model", pinned: i < 6, ...times });
    }
  }
  return { own, team };
}

type Opts = {
  role?: string;
  pinFails?: boolean;
  separateLists?: boolean;
  teamPinned?: string[];
  // Pin every one of the person's own chats (80 of them).
  pinAllOwn?: boolean;
};

async function boot(page: Page, opts: Opts = {}) {
  const { own, team } = seed();
  for (const id of opts.teamPinned ?? []) team.find((c) => c.id === id)!.pinned = true;
  if (opts.pinAllOwn) for (const c of own) c.pinned = true;
  const pins: Array<{ id: string; pinned: boolean }> = [];
  const orders: Array<{ scope: string; ids: string[] }> = [];
  await mockChatBoot(page, { conversations: own });
  await page.route("**/api/me", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com", role: opts.role ?? "member" } }),
  );
  if (opts.separateLists) {
    await page.route("**/api/sidebar", (r: Route) =>
      r.fulfill({
        json: {
          layout: {
            version: 1,
            items: [
              { id: "chat", kind: "builtin" },
              { id: "patrols", kind: "builtin" },
              { id: "tasks", kind: "builtin" },
            ],
            sections: [{ id: "projects" }, { id: "chats" }, { id: "team" }],
            separate_lists: true,
          },
          workspace_default: null,
        },
      }),
    );
  }
  // As the server lists them: the caller's pinned first, then most recent.
  const sharedList = () => [...team].sort((a, b) => Number(b.pinned) - Number(a.pinned));
  await page.route("**/api/conversations?*", (r: Route) => {
    const url = new URL(r.request().url());
    if (url.searchParams.get("scope") === "shared") return r.fulfill({ json: { conversations: sharedList() } });
    if (url.searchParams.get("scope") === "team") return r.fulfill({ json: { conversations: [] } });
    if (url.searchParams.get("archived") === "true") return r.fulfill({ json: { conversations: [] } });
    return r.fulfill({ json: { conversations: own } });
  });
  await page.route("**/api/conversations/order", (r: Route) => {
    if (r.request().method() !== "PUT") return r.fulfill({ json: { orders: {} } });
    const body = r.request().postDataJSON() as { scope: string; conversation_ids: string[] };
    orders.push({ scope: body.scope, ids: body.conversation_ids });
    return r.fulfill({ json: body });
  });
  await page.route("**/api/conversations/*/pin", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").at(-2) ?? "";
    const body = r.request().postDataJSON() as { pinned: boolean };
    pins.push({ id, pinned: body.pinned });
    if (opts.pinFails) return r.fulfill({ status: 500, body: "down" });
    const row = [...own, ...team].find((c) => c.id === id);
    if (row) row.pinned = body.pinned;
    return r.fulfill({ status: 204, body: "" });
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  return { pins, orders, own, team };
}

const titlesIn = (page: Page, testId: string) =>
  page
    .getByTestId(testId)
    .locator("[data-rail-row] .truncate:not(.font-\\[family-name\\:var\\(--font-code\\)\\])")
    .allInnerTexts();

const teamRow = (page: Page, title: string) =>
  page.getByTestId("team-shared-conv-row").filter({ hasText: title }).first().locator("..");

async function pinFromMenu(page: Page, title: string, item: "Pin" | "Unpin" = "Pin") {
  const row = teamRow(page, title);
  await row.hover();
  await row.getByTestId("team-chat-options").click();
  await page.getByRole("menuitem", { name: item, exact: true }).click();
}

// Two animation frames: long enough for a response the page just received to
// be read, applied and committed, so "still there" is not checked too early.
async function settledFrames(page: Page) {
  await page.evaluate(
    () => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => r()))),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("a teammate's chat is pinned from its menu, joins Pinned, and unpins back into the list", async ({ page }) => {
  const { pins } = await boot(page);
  await expect(page.getByTestId("pinned-section").locator("[data-rail-row]")).toHaveCount(4);
  await expect(page.getByTestId("unified-chat-list").locator("[data-rail-row]")).toHaveCount(116);

  await pinFromMenu(page, "Acme renewal");
  await expect.poll(() => pins).toEqual([{ id: "t1", pinned: true }]);
  // In Pinned, still a teammate's row (whose it is stays on it), listed once.
  const pinned = page.getByTestId("pinned-section");
  await expect(pinned.getByTestId("team-shared-conv-row").filter({ hasText: "Acme renewal" })).toHaveCount(1);
  await expect(pinned.getByTestId("team-shared-conv-row").first()).toContainText("tom");
  await expect(page.getByTestId("unified-chat-list").getByText("Acme renewal", { exact: true })).toHaveCount(0);
  await expect(page.getByTestId("unified-chat-list").locator("[data-rail-row]")).toHaveCount(115);
  // The heading says Pinned; the row needs no pin of its own.
  await expect(pinned.getByTestId("team-chat-pinned-mark")).toHaveCount(0);

  // It survives a reload (the server keeps the pin, per person).
  await page.reload();
  await expect(
    page.getByTestId("pinned-section").getByTestId("team-shared-conv-row").filter({ hasText: "Acme renewal" }),
  ).toHaveCount(1);

  await pinFromMenu(page, "Acme renewal", "Unpin");
  await expect.poll(() => pins.at(-1)).toEqual({ id: "t1", pinned: false });
  await expect(page.getByTestId("pinned-section").getByTestId("team-shared-conv-row")).toHaveCount(0);
  await expect(page.getByTestId("unified-chat-list").getByText("Acme renewal", { exact: true })).toHaveCount(1);
});

test("a pin the server refuses is put back, and the person is told why", async ({ page }) => {
  const { pins } = await boot(page, { pinFails: true });
  await pinFromMenu(page, "Acme renewal");
  await expect.poll(() => pins.length).toBe(1);
  await expect(page.getByText("Couldn’t pin “Acme renewal”. Try again.")).toBeVisible();
  await expect(page.getByTestId("pinned-section").getByTestId("team-shared-conv-row")).toHaveCount(0);
  await expect(page.getByTestId("unified-chat-list").getByText("Acme renewal", { exact: true })).toHaveCount(1);

  // The Pinned group is full (409): the message says what to do about it.
  await page.route("**/api/conversations/t4/pin", (r: Route) => r.fulfill({ status: 409, body: "too many pinned chats" }));
  await pinFromMenu(page, "Incident room");
  await expect(page.getByText("Your pinned chats are full. Unpin one to pin “Incident room”.")).toBeVisible();
  await expect(page.getByTestId("pinned-section").getByTestId("team-shared-conv-row")).toHaveCount(0);
});

test("a list read that started before a pin was saved does not undo the pin", async ({ page }) => {
  const { pins, team } = await boot(page);
  // The next read of the shared list is answered late, with what the server
  // held when the read arrived (before the pin).
  let release: () => void = () => {};
  const held = new Promise<void>((r) => (release = r));
  let hold = true;
  let reads = 0;
  let arrived: () => void = () => {};
  const readArrived = new Promise<void>((r) => (arrived = r));
  await page.route("**/api/conversations?scope=shared", async (r: Route) => {
    reads++;
    const sorted = () => [...team].sort((a, b) => Number(b.pinned) - Number(a.pinned));
    if (!hold) return r.fulfill({ json: { conversations: sorted() } });
    hold = false;
    const stale = JSON.stringify({ conversations: sorted() });
    arrived();
    await held;
    return r.fulfill({ contentType: "application/json", body: stale });
  });
  // A refused pin on one of the person's own chats makes the page re-read
  // its lists, the shared one among them.
  await page.route("**/api/conversations/c6/pin", (r: Route) => r.fulfill({ status: 500, body: "down" }));
  const own = page.locator('[data-conversation-id="c6"]').first();
  await own.hover();
  await own.getByRole("button", { name: /Conversation options/ }).click();
  await page.getByRole("menuitem", { name: "Pin", exact: true }).click();
  await readArrived;

  await pinFromMenu(page, "Acme renewal");
  await expect.poll(() => pins.some((p) => p.id === "t1" && p.pinned)).toBe(true);
  const pinnedRow = page.getByTestId("pinned-section").getByTestId("team-shared-conv-row").filter({ hasText: "Acme renewal" });
  await expect(pinnedRow).toHaveCount(1);
  const before = reads;
  release();
  // The stale answer is dropped and the list read again; the pin stays.
  await expect.poll(() => reads).toBeGreaterThan(before);
  await settledFrames(page);
  await expect(pinnedRow).toHaveCount(1);
});

test("a Viewer account is offered no pin on a teammate's chat", async ({ page }) => {
  await boot(page, { role: "viewer" });
  const row = teamRow(page, "Acme renewal");
  await row.hover();
  await row.getByTestId("team-chat-options").click();
  await expect(page.getByRole("menu")).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Pin", exact: true })).toHaveCount(0);
});

test("the Pinned group is ordered by hand, a teammate's chat with the person's own", async ({ page }) => {
  const { orders } = await boot(page, { teamPinned: ["t1"] });
  // Before any order: by recent activity, the teammate's chat among them.
  await expect.poll(() => titlesIn(page, "pinned-section")).toEqual([
    "Q4 pipeline review",
    "Acme renewal",
    "Omnicom pacing",
    "Deal desk daily",
    "Budget notes",
  ]);
  await page.getByTestId("pinned-reorder-toggle").click();
  const list = page.getByRole("list", { name: "Pinned chats, in order" });
  await list.getByRole("button", { name: "Move Acme renewal up" }).click();
  await expect.poll(() => orders.at(-1)).toEqual({ scope: "pinned", ids: ["t1", "c0", "c2", "c3", "c5"] });

  // A mouse drags a row by any part of it: the last chat to the top.
  await page
    .getByTestId("chat-reorder-item-c5")
    .locator("[data-reorder-row]")
    .dragTo(page.getByTestId("chat-reorder-item-t1").locator("[data-reorder-row]"), {
      sourcePosition: { x: 90, y: 12 },
      targetPosition: { x: 90, y: 12 },
    });
  await expect.poll(() => orders.at(-1)?.ids).toEqual(["c5", "t1", "c0", "c2", "c3"]);
  // Done is in the Pinned heading; the rows come back in the new order.
  await page.getByTestId("pinned-section").getByTestId("chat-reorder-done").click();
  await expect.poll(() => titlesIn(page, "pinned-section")).toEqual([
    "Budget notes",
    "Acme renewal",
    "Q4 pipeline review",
    "Omnicom pacing",
    "Deal desk daily",
  ]);
});

test("a long Pinned list scrolls under a held drag, and the drop lands where it shows", async ({ page }) => {
  const { orders, own } = await boot(page, { pinAllOwn: true });
  await page.getByTestId("pinned-reorder-toggle").click();
  const items = page.getByTestId("chat-reorder-list").locator("li");
  await expect(items).toHaveCount(80);
  // The list is far taller than the rail: its scroller is the chat list.
  const scroller = page.getByTestId("chat-reorder-list").locator("xpath=ancestor::div[contains(@class,'overflow-y-auto')][1]");
  const box = (await scroller.boundingBox())!;
  expect(await scroller.evaluate((el) => el.scrollHeight > el.clientHeight * 3)).toBe(true);
  expect(await scroller.evaluate((el) => el.scrollTop)).toBe(0);

  // Pick the first chat up and hold it at the bottom edge.
  const first = (await page.getByTestId(`chat-reorder-item-${own[0].id}`).locator("[data-reorder-row]").boundingBox())!;
  const x = first.x + 90;
  await page.mouse.move(x, first.y + first.height / 2);
  await page.mouse.down();
  await page.mouse.move(x, box.y + box.height - 6, { steps: 8 });
  // Nothing more from the pointer: the list scrolls by itself until the row
  // is in the last place, and stops there (not on into the chats below).
  let last = -1;
  await expect
    .poll(
      async () => {
        const now = await scroller.evaluate((el) => el.scrollTop);
        const still = now === last && now > 0;
        last = now;
        return still;
      },
      { timeout: 20_000, intervals: [400] },
    )
    .toBe(true);
  expect(last).toBeGreaterThan(1500);
  expect(orders.length).toBe(0);
  // The carried row is in the last place and on screen.
  const carried = page.locator("li[data-dragging]");
  await expect(carried).toHaveCount(1);
  await expect(carried).toBeInViewport();
  await page.mouse.up();
  // Dropped at the end of the list: one save, the first chat now last.
  await expect.poll(() => orders.length).toBe(1);
  const ids = own.map((c) => c.id);
  expect(orders[0]).toEqual({ scope: "pinned", ids: [...ids.slice(1), ids[0]] });
  await expect(items.last()).toContainText("Q4 pipeline review");
  await expect(page.locator("li[data-dragging]")).toHaveCount(0);
});

test("with separate lists a pinned teammate's chat stays in Team chats, first and marked", async ({ page }) => {
  await boot(page, { separateLists: true, teamPinned: ["t31"] });
  const section = page.getByTestId("team-chats-section");
  const rows = section.getByTestId("team-shared-conv-row");
  await expect(rows).toHaveCount(40);
  await expect(rows.first()).toContainText("Acme renewal 4");
  await expect(rows.first().getByTestId("team-chat-pinned-mark")).toHaveCount(1);
  await expect(section.getByTestId("team-chat-pinned-mark")).toHaveCount(1);
  await expect(page.getByTestId("pinned-section").getByTestId("team-shared-conv-row")).toHaveCount(0);
});

for (const scheme of ["light", "dark"] as const) {
  test(`the list, its sort menu and a pinned teammate's chat pass axe (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await boot(page, { teamPinned: ["t1"] });
    await page.getByTestId("recent-sort").click();
    await expect(page.getByRole("menu", { name: "Sort chats" })).toBeVisible();
    const results = await new AxeBuilder({ page })
      .include('[data-testid="pinned-section"]')
      .include('[data-testid="recent-sort"]')
      .include('[role="menu"]')
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();
    const summary = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    );
    expect(summary, summary.join("\n")).toEqual([]);
  });
}

test.describe("on a phone", () => {
  // As mobile.spec.ts: a phone viewport with touch, on the suite's Chromium.
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  async function openDrawer(page: Page) {
    await page.getByRole("button", { name: /open sidebar/i }).click();
    await expect(page.getByTestId("pinned-section")).toBeVisible();
  }

  // A real finger: touch events through the browser's input pipeline, which
  // become the pointer events (pointerType "touch") the list listens for.
  async function fingerDrag(page: Page, from: { x: number; y: number }, dy: number) {
    const cdp = await page.context().newCDPSession(page);
    const at = (y: number) => [{ x: from.x, y }];
    await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: at(from.y) });
    const steps = 8;
    for (let i = 1; i <= steps; i++) {
      await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: at(from.y + (dy * i) / steps) });
    }
    await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await cdp.detach();
  }

  test("the sort is the heading's size, on one row, and its menu sorts the list", async ({ page }) => {
    await boot(page);
    await openDrawer(page);
    const sort = page.getByTestId("recent-sort");
    await sort.scrollIntoViewIfNeeded();
    // It was a native select, forced to 16px on touch screens: twice the
    // 11px heading beside it, and wide enough to wrap under it.
    const size = await sort.evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
    expect(size).toBeLessThanOrEqual(12);
    const heading = sort.locator("..");
    expect((await heading.boundingBox())!.height).toBeLessThanOrEqual(40);
    const box = (await sort.boundingBox())!;
    expect(box.x + box.width).toBeLessThanOrEqual(390 * 0.85);

    await sort.tap();
    const choice = page.getByRole("menuitemradio", { name: "Title A–Z" });
    // A full-size tap target, inside the screen.
    const cb = (await choice.boundingBox())!;
    expect(cb.height).toBeGreaterThanOrEqual(44);
    expect(cb.x).toBeGreaterThanOrEqual(0);
    expect(cb.x + cb.width).toBeLessThanOrEqual(390);
    await choice.tap();
    await expect(sort).toHaveAccessibleName("Sort chats: Title A–Z");
    const titles = await titlesIn(page, "unified-chat-list");
    expect(titles.length).toBe(116);
    expect(titles.slice(0, 3)).toEqual(["Acme renewal", "Acme renewal 2", "Acme renewal 3"]);
  });

  test("a teammate's chat is pinned with a tap, and Pinned is reordered with a finger", async ({ page }) => {
    const { pins, orders } = await boot(page);
    await openDrawer(page);
    const row = teamRow(page, "Acme renewal");
    await row.getByTestId("team-chat-options").tap();
    await page.getByRole("menuitem", { name: "Pin", exact: true }).tap();
    await expect.poll(() => pins).toEqual([{ id: "t1", pinned: true }]);
    await expect(page.getByTestId("pinned-section").locator("[data-rail-row]")).toHaveCount(5);
    // No tooltip is left over the list after the menu closes on a touch screen.
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    await page.getByTestId("pinned-reorder-toggle").tap();
    const items = page.getByTestId("chat-reorder-list").locator("li");
    await expect(items).toHaveText([/Q4 pipeline review/, /Acme renewal/, /Omnicom pacing/, /Deal desk daily/, /Budget notes/]);

    // Drag the last chat up two places by its handle.
    const handle = page.getByTestId("chat-reorder-handle-c5");
    const hb = (await handle.boundingBox())!;
    const step =
      (await items.nth(1).boundingBox())!.y - (await items.nth(0).boundingBox())!.y;
    await fingerDrag(page, { x: hb.x + hb.width / 2, y: hb.y + hb.height / 2 }, -2 * step);
    await expect.poll(() => orders.at(-1)).toEqual({ scope: "pinned", ids: ["c0", "t1", "c5", "c2", "c3"] });
    expect(orders.length).toBe(1);
    await expect(items).toHaveText([/Q4 pipeline review/, /Acme renewal/, /Budget notes/, /Omnicom pacing/, /Deal desk daily/]);

    // A finger on the row itself (not the handle) scrolls; it moves nothing.
    const title = items.nth(0).locator(".truncate");
    const tb = (await title.boundingBox())!;
    await fingerDrag(page, { x: tb.x + 20, y: tb.y + tb.height / 2 }, 2 * step);
    await page.waitForTimeout(200);
    expect(orders.length).toBe(1);

    // The arrows are full-size tap targets and still work.
    const up = page.getByRole("button", { name: "Move Budget notes up" });
    expect((await up.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await up.tap();
    await expect.poll(() => orders.at(-1)?.ids).toEqual(["c0", "c5", "t1", "c2", "c3"]);
    await page.getByTestId("chat-reorder-done").tap();
    await expect(page.getByTestId("chat-reorder-list")).toHaveCount(0);
  });

  test("a long Pinned list scrolls under a finger held at the edge", async ({ page }) => {
    // On a phone the scroller is the drawer's body, not the chat list (which
    // still computes to overflow-y: auto but has nothing to scroll).
    const { orders, own } = await boot(page, { pinAllOwn: true });
    await openDrawer(page);
    await page.getByTestId("pinned-reorder-toggle").tap();
    const items = page.getByTestId("chat-reorder-list").locator("li");
    await expect(items).toHaveCount(80);
    const body = page.getByTestId("rail-body");
    const box = (await body.boundingBox())!;
    const start = await body.evaluate((el) => el.scrollTop);
    const handle = page.getByTestId(`chat-reorder-handle-${own[0].id}`);
    await handle.scrollIntoViewIfNeeded();
    const hb = (await handle.boundingBox())!;
    const x = hb.x + hb.width / 2;
    const from = hb.y + hb.height / 2;
    const edge = box.y + box.height - 8;
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y: from }] });
    for (let i = 1; i <= 8; i++) {
      await cdp.send("Input.dispatchTouchEvent", {
        type: "touchMove",
        touchPoints: [{ x, y: from + ((edge - from) * i) / 8 }],
      });
    }
    // The finger holds still; the drawer scrolls on by itself.
    await expect.poll(() => body.evaluate((el) => el.scrollTop), { timeout: 15_000 }).toBeGreaterThan(start + 600);
    expect(orders.length).toBe(0);
    await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await cdp.detach();
    // One save; the chat went well down the list (it started first).
    await expect.poll(() => orders.length).toBe(1);
    expect(orders[0].ids.indexOf(own[0].id)).toBeGreaterThan(12);
    expect(orders[0].ids.length).toBe(80);
  });

  test("the drawer scrolls as one, so a long list gets the screen", async ({ page }) => {
    await boot(page);
    await openDrawer(page);
    const body = page.getByTestId("rail-body");
    // The one scroller: nothing inside it is a scroll box of its own (the
    // Projects and Team chats sections are capped only on wider screens).
    const nested = await body.evaluate((el) =>
      [...el.querySelectorAll<HTMLElement>("*")].filter((n) => {
        const o = getComputedStyle(n).overflowY;
        return (o === "auto" || o === "scroll") && n.scrollHeight > n.clientHeight + 1;
      }).length,
    );
    expect(nested).toBe(0);
    const { scrollHeight, clientHeight } = await body.evaluate((el) => ({
      scrollHeight: el.scrollHeight,
      clientHeight: el.clientHeight,
    }));
    // 120 chats at a row each: the body is the one scroller, and a tall one.
    expect(scrollHeight).toBeGreaterThan(clientHeight * 4);
    await body.evaluate((el) => el.scrollTo(0, 1200));
    // Rows on screen at once: it used to be four or five.
    const visible = await page.getByTestId("unified-chat-list").locator("[data-rail-row]").evaluateAll((rows) => {
      const vh = window.innerHeight;
      return rows.filter((r) => {
        const b = r.getBoundingClientRect();
        return b.top >= 0 && b.bottom <= vh;
      }).length;
    });
    expect(visible).toBeGreaterThanOrEqual(12);
    // Nothing sticks out sideways, and every row keeps a 44px tap height.
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(1);
    expect(await body.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    const short = await page.getByTestId("unified-chat-list").locator("[data-rail-row]").evaluateAll(
      (rows) => rows.filter((r) => r.getBoundingClientRect().height < 44).length,
    );
    expect(short).toBe(0);
    // The last chat is reachable.
    const last = page.getByTestId("unified-chat-list").locator("[data-rail-row]").last();
    await last.scrollIntoViewIfNeeded();
    await expect(last).toBeInViewport();
  });
});
