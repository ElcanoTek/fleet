import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The rail's pinned-chat order (docs/CHAT-PIN-ORDER.md): the Pinned section
// follows the order saved on the server, "Reorder" turns it into a list that
// can be dragged or moved with arrows, each move is saved with PUT
// /api/conversations/pin-order, and the list passes axe in both themes.

const CONVERSATIONS = [
  { id: "c1", title: "Acme Renewal", persona: "default", model: "", pinned: true, updated_at: 40 },
  { id: "c2", title: "Omnicom Pacing", persona: "default", model: "", pinned: true, updated_at: 30 },
  { id: "c3", title: "Deal Desk Daily", persona: "default", model: "", pinned: true, updated_at: 25 },
  { id: "c4", title: "Loose Recent", persona: "default", model: "", pinned: false, updated_at: 10 },
];

async function setup(page: Page, saved: string[], opts: { role?: string; holdFirstRead?: Promise<void>; waitForPinned?: boolean } = {}) {
  const puts: string[][] = [];
  await mockChatBoot(page);
  await page.route("**/api/me", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com", role: opts.role ?? "member" } }),
  );
  await page.route("**/api/conversations", (r: Route) =>
    r.fulfill({ json: r.request().method() === "GET" ? { conversations: CONVERSATIONS } : {} }),
  );
  await page.route("**/api/conversations/*", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "c1";
    const conv = CONVERSATIONS.find((c) => c.id === id) ?? CONVERSATIONS[0];
    return r.fulfill({ json: { conversation: conv, history: [] } });
  });
  await page.route("**/api/conversations/pin-order", (r: Route) => {
    if (r.request().method() === "PUT") {
      const ids = (r.request().postDataJSON() as { conversation_ids: string[] }).conversation_ids;
      puts.push(ids);
      return r.fulfill({ json: { conversation_ids: ids } });
    }
    if (opts.holdFirstRead) {
      const hold = opts.holdFirstRead;
      opts.holdFirstRead = undefined;
      return hold.then(() => r.fulfill({ json: { conversation_ids: saved } }));
    }
    return r.fulfill({ json: { conversation_ids: saved } });
  });
  await page.goto("/chat");
  if (opts.waitForPinned === false) {
    await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  } else {
    await page.getByTestId("pinned-section").waitFor({ timeout: 15_000 });
  }
  return puts;
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
  const list = page.getByTestId("pinned-reorder-list");
  await expect(list).toBeVisible();

  // Arrows: Omnicom Pacing up one place.
  await list.getByRole("button", { name: "Move Omnicom Pacing up" }).click();
  await expect.poll(() => puts.at(-1)).toEqual(["c3", "c2", "c1"]);
  await expect(page.getByRole("status").filter({ hasText: "moved to position" })).toHaveText(
    "Omnicom Pacing moved to position 2 of 3.",
  );

  // Drag: Acme Renewal to the top.
  await page
    .getByTestId("pinned-reorder-item-c1")
    .locator("div[draggable]")
    .dragTo(page.getByTestId("pinned-reorder-item-c3").locator("div[draggable]"));
  await expect.poll(() => puts.at(-1)).toEqual(["c1", "c3", "c2"]);

  await page.getByTestId("pinned-reorder-done").click();
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
  await expect.poll(() => puts.at(-1)).toEqual(["c2", "c1", "c3"]);
  release(); // the stale read now answers with the old saved order
  await page.waitForTimeout(300);
  const items = page.getByTestId("pinned-reorder-list").locator("li");
  await expect(items).toHaveText([/Omnicom Pacing/, /Acme Renewal/, /Deal Desk Daily/]);
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
  await expect(page.getByTestId("pinned-reorder-list")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("pinned-reorder-list")).toHaveCount(0);
  await expect(page.getByTestId("pinned-reorder-toggle")).toBeVisible();
  await expect(page.getByTestId("pinned-reorder-toggle")).toBeFocused();
  // The overlay is still open: Escape belonged to reorder mode alone.
  await expect.poll(async () => (await rail.boundingBox())?.width ?? 0).toBeGreaterThan(290);
});

for (const scheme of ["light", "dark"] as const) {
  test(`reorder mode has no axe violations (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await setup(page, []);
    await page.getByTestId("pinned-reorder-toggle").click();
    await expect(page.getByTestId("pinned-reorder-list")).toBeVisible();
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
