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

async function setup(page: Page, saved: string[]) {
  const puts: string[][] = [];
  await mockChatBoot(page);
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
    return r.fulfill({ json: { conversation_ids: saved } });
  });
  await page.goto("/chat");
  await page.getByTestId("pinned-section").waitFor({ timeout: 15_000 });
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
