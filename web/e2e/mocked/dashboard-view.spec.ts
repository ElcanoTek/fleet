import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for dashboard view (docs/DASHBOARD-VIEW.md): the chat as a live
// board. Panels are derived from the conversation's show_ui cards, so the
// whole board is seeded through the persisted history — there is no board
// endpoint to mock.

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

const card = (id: string, spec: unknown) => [
  { role: "assistant", type: "tool_call", content: { id, name: "show_ui", input: JSON.stringify(spec) } },
  {
    role: "tool",
    type: "tool_result",
    content: { id, name: "show_ui", text: `UI_DISPLAYED card_id=${id}: the user can now see the card.`, is_err: false },
  },
];

const REVENUE_V1 = {
  title: "Revenue",
  panel: "revenue",
  components: [{ type: "stat", label: "Month to date", value: "$1.02M" }],
};
const REVENUE_V2 = {
  title: "Revenue",
  panel: "revenue",
  components: [
    {
      type: "columns",
      children: [
        { type: "stat", label: "Month to date", value: "$1.24M", caption: "+12% vs last month", tone: "success" },
        { type: "stat", label: "Forecast", value: "$1.9M", caption: "on track" },
      ],
    },
    {
      type: "chart",
      kind: "line",
      labels: ["W1", "W2", "W3", "W4", "W5", "W6", "W7", "W8"],
      unit: "$",
      series: [
        { name: "This year", values: [210, 260, 240, 310, 330, 360, 352, 401] },
        { name: "Last year", values: [190, 200, 230, 240, 250, 270, 280, 300] },
      ],
    },
  ],
};
const PIPELINE = {
  title: "Open deals",
  panel: "pipeline",
  components: [
    {
      type: "table",
      columns: [
        { key: "deal", label: "Deal" },
        { key: "stage", label: "Stage" },
        { key: "value", label: "Value", align: "right" },
      ],
      rows: [
        { deal: "Acme renewal", stage: "Negotiation", value: "$240k" },
        { deal: "Globex pilot", stage: "Proposal", value: "$85k" },
        { deal: "Initech expansion", stage: "Discovery", value: "$410k" },
      ],
    },
  ],
};
const HEALTH = {
  title: "Systems",
  components: [
    {
      type: "status_list",
      items: [
        { status: "pass", label: "Billing sync", detail: "2 min ago" },
        { status: "warn", label: "CRM import", detail: "3 records skipped" },
        { status: "pass", label: "Email delivery" },
      ],
    },
  ],
};
const FOLLOW_UP = {
  title: "Send a follow-up",
  components: [{ type: "text_input", id: "note", label: "Note to Acme", required: true }],
  actions: [{ id: "draft", label: "Draft email" }],
};

const HISTORY = [
  { role: "user", type: "text", content: { text: "build me a revenue dashboard" } },
  ...card("call-rev-1", REVENUE_V1),
  ...card("call-pipe-1", PIPELINE),
  ...card("call-health-1", HEALTH),
  { role: "assistant", type: "text", content: { text: "Three panels are up. Want a follow-up to Acme?" } },
  { role: "user", type: "text", content: { text: "refresh revenue and help me follow up" } },
  ...card("call-rev-2", REVENUE_V2),
  ...card("call-follow-1", FOLLOW_UP),
  { role: "assistant", type: "text", content: { text: "Revenue is refreshed. Fill in the note and I'll draft it." } },
].map((entry, i) => ({ id: i + 1, ...entry }));

async function openDashboardChat(page: Page) {
  await page.addInitScript(() => {
    try {
      window.localStorage.removeItem("fleet.dashboard.view.v1");
      window.localStorage.removeItem("fleet.dashboard.board.v2");
    } catch {
      // storage unavailable: nothing to clear
    }
  });
  await mockChatBoot(page, { conversations: [{ id: "conv-dash", title: "Revenue cockpit" }] });
  await page.route("**/api/conversations/conv-dash", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: { id: "conv-dash", title: "Revenue cockpit", persona: "default", model: "test-model", pinned: false },
        history: HISTORY,
        pending_approvals: [
          {
            approval_id: "ap-email-1",
            tool: "mcp_sendgrid_send_email",
            summary: {
              tool: "mcp_sendgrid_send_email",
              to: "ops@example.com",
              subject: "Weekly revenue snapshot",
              content: "<p>Revenue is up 12% month to date.</p>",
              content_type: "text/html",
            },
          },
        ],
        resolved_approvals: [],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator('[data-conversation-id="conv-dash"]');
  await row.waitFor({ timeout: 15_000 });
  await row.click();
  await expect(page.getByText("Revenue is refreshed.")).toBeVisible();
}

test("dashboard view puts panel cards on a board beside the chat rail", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openDashboardChat(page);

  await page.getByTestId("dashboard-enter").click();
  await expect(page.getByTestId("dashboard-hud")).toContainText("Revenue cockpit");
  // The pending email approval lights the HUD; it stays in the chat rail.
  await expect(page.getByTestId("dashboard-status")).toHaveAttribute("data-status", "approval");
  await expect(page.locator('[data-approval-id="ap-email-1"]')).toBeVisible();

  // Three panels: revenue (refreshed in place, so v2), the pipeline table, and
  // the display-only systems card. The form stays inline in the rail.
  const panels = page.getByTestId("dashboard-panel");
  await expect(panels).toHaveCount(3);
  await expect(page.getByTestId("dashboard-panel-count")).toHaveText("3");
  const revenue = page.locator('[data-panel-key="p:revenue"]');
  await expect(revenue).toContainText("v2");
  await expect(revenue).toContainText("$1.24M");
  await expect(revenue).not.toContainText("$1.02M");
  // Both revenue cards (current and earlier) point at the board instead of
  // drawing a second copy.
  await expect(page.getByTestId("dashboard-card-pointer")).toHaveCount(4);
  const form = page.locator('[data-testid="genui-card"][data-card-id="call-follow-1"]');
  await expect(form).toBeVisible();

  // Pin the form: it becomes a fourth panel, and the rail points at it.
  await page.getByTestId("dashboard-pin-card").click();
  await expect(panels).toHaveCount(4);
  await expect(page.locator('[data-panel-key="c:call-follow-1"]')).toContainText("Send a follow-up");

  // Remove the systems panel: its card returns to the chat rail.
  const systems = page.locator('[data-panel-key="c:call-health-1"]');
  await systems.getByRole("button", { name: "More for Systems" }).click();
  await page.getByRole("menuitem", { name: "Remove from board" }).click();
  await expect(panels).toHaveCount(3);
  await expect(page.locator('[data-testid="genui-card"][data-card-id="call-health-1"]')).toBeVisible();

  // A message sent from dashboard view tells the server which view it is.
  const chat = page.waitForRequest((req) => req.url().includes("/api/chat") && req.method() === "POST");
  await page.locator("#promptInput").fill("add a churn panel");
  await page.keyboard.press("Enter");
  const body = JSON.parse((await chat).postData() ?? "{}") as { view?: string };
  expect(body.view).toBe("dashboard");
});

test("dashboard view: focus a panel, keyboard reorder, and Esc to leave", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openDashboardChat(page);
  // D (outside a text field) turns the view on.
  await page.locator("body").click({ position: { x: 5, y: 5 } });
  await page.keyboard.press("d");
  await expect(page.getByTestId("dashboard-hud")).toBeVisible();

  // The help button opens and closes its popover.
  await page.getByTestId("dashboard-help-button").click();
  await expect(page.getByTestId("dashboard-help")).toBeVisible();
  await page.getByTestId("dashboard-help-button").click();
  await expect(page.getByTestId("dashboard-help")).toHaveCount(0);

  const keys = () => page.getByTestId("dashboard-panel").evaluateAll((els) => els.map((e) => e.getAttribute("data-panel-key")));
  expect(await keys()).toEqual(["p:revenue", "p:pipeline", "c:call-health-1"]);
  // Arrow keys on a panel's grip move it.
  await page.locator('[data-panel-key="p:revenue"]').getByTestId("dashboard-panel-grip").focus();
  await page.keyboard.press("ArrowRight");
  await expect.poll(keys).toEqual(["p:pipeline", "p:revenue", "c:call-health-1"]);

  // Focus a panel full-board; Esc closes the focus first, then the view.
  await page.locator('[data-panel-key="p:pipeline"]').getByTestId("dashboard-panel-focus").click();
  await expect(page.getByTestId("dashboard-focus-layer")).toContainText("Initech expansion");
  // The keyboard focus moves into the focused panel, and comes back to the
  // panel's Focus button when it closes.
  await expect(page.getByTestId("dashboard-panel-restore")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("dashboard-focus-layer")).toHaveCount(0);
  await expect(page.locator('[data-panel-key="p:pipeline"]').getByTestId("dashboard-panel-focus")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("dashboard-hud")).toHaveCount(0);
  await expect(page.getByTestId("dashboard-enter")).toBeVisible();

  // The layout and the view are remembered for this chat in this browser.
  await page.keyboard.press("d");
  expect(await keys()).toEqual(["p:pipeline", "p:revenue", "c:call-health-1"]);

  // The rail is hidden, so Ctrl+K leaves the view and focuses its search.
  await page.keyboard.press("Control+k");
  await expect(page.getByTestId("dashboard-hud")).toHaveCount(0);
  await expect(page.getByTestId("search-input")).toBeFocused();
});

test("moving between dashboard view and the normal chat keeps drafts, cards and layout", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openDashboardChat(page);
  const prompt = page.locator("#promptInput");
  await prompt.fill("half-written question");
  const inline = (id: string) => page.locator(`[data-testid="genui-card"][data-card-id="${id}"]`);

  for (let round = 0; round < 3; round++) {
    await page.getByTestId("dashboard-enter").click();
    await expect(page.getByTestId("dashboard-panel")).toHaveCount(round === 0 ? 3 : 4);
    if (round === 0) {
      // Pin the form and start answering it on the board.
      await page.getByTestId("dashboard-pin-card").click();
      await page.locator('[data-panel-key="c:call-follow-1"]').getByLabel(/Note to Acme/).fill("Renewal looks good");
    }
    await expect(page.locator('[data-panel-key="c:call-follow-1"]').getByLabel(/Note to Acme/)).toHaveValue("Renewal looks good");
    await expect(prompt).toHaveValue("half-written question");
    await page.getByTestId("dashboard-exit").click();

    // Back in the normal chat: full cards again, no board pointers, the same
    // draft in the composer and in the form.
    await expect(page.getByTestId("dashboard-hud")).toHaveCount(0);
    await expect(page.getByTestId("dashboard-card-pointer")).toHaveCount(0);
    await expect(page.locator('[data-conversation-id="conv-dash"]')).toBeVisible();
    await expect(inline("call-rev-2")).toContainText("$1.24M");
    await expect(inline("call-follow-1").getByLabel(/Note to Acme/)).toHaveValue("Renewal looks good");
    await expect(prompt).toHaveValue("half-written question");
  }
});

test("the board tells the agent which card each panel is, pinned ones included", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await openDashboardChat(page);
  await page.getByTestId("dashboard-enter").click();
  await page.getByTestId("dashboard-pin-card").click();
  await expect(page.getByTestId("dashboard-panel")).toHaveCount(4);
  const chat = page.waitForRequest((req) => req.url().includes("/api/chat") && req.method() === "POST");
  // Pinning moves the focus to the new panel; type into the composer itself.
  await expect(page.locator('[data-panel-key="c:call-follow-1"]')).toBeFocused();
  await page.locator("#promptInput").fill("update the follow-up panel");
  await page.locator("#promptInput").press("Enter");
  const body = JSON.parse((await chat).postData() ?? "{}") as { view?: string; board?: unknown[] };
  expect(body.view).toBe("dashboard");
  expect(body.board).toEqual([
    { title: "Revenue", card_id: "call-rev-2", panel: "revenue" },
    { title: "Open deals", card_id: "call-pipe-1", panel: "pipeline" },
    { title: "Systems", card_id: "call-health-1" },
    { title: "Send a follow-up", card_id: "call-follow-1", pinned: true },
  ]);
});

test("dashboard view on a phone switches between the board and the chat", async ({ page }) => {
  // Open the chat from the desktop rail, then go phone-sized.
  await page.setViewportSize({ width: 1280, height: 844 });
  await openDashboardChat(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByTestId("dashboard-enter").click();
  await expect(page.getByTestId("dashboard-board")).toBeVisible();
  await expect(page.getByTestId("dashboard-panel")).toHaveCount(3);
  await page.getByRole("button", { name: "Chat", exact: true }).click();
  await expect(page.getByTestId("dashboard-board")).toBeHidden();
  await expect(page.getByText("Revenue is refreshed.")).toBeVisible();
  // A pointer in the chat jumps back to its panel on the board.
  await page.getByTestId("dashboard-card-pointer").first().click();
  await expect(page.getByTestId("dashboard-board")).toBeVisible();
  // The approvals light switches to the chat and brings the card into view.
  await page.getByTestId("dashboard-status").click();
  await expect(page.getByTestId("dashboard-board")).toBeHidden();
  await expect(page.locator('[data-approval-id="ap-email-1"]')).toBeInViewport();
});

// axe over the HUD and the board (the chat rail's own components are covered
// by their specs), in both colour schemes: contrast is per theme.
for (const scheme of ["light", "dark"] as const) {
  test(`dashboard view has no axe violations (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await page.setViewportSize({ width: 1440, height: 900 });
    await openDashboardChat(page);
    await page.getByTestId("dashboard-enter").click();
    await expect(page.getByTestId("dashboard-panel")).toHaveCount(3);
    await expect(page.locator('[data-panel-key="p:revenue"]')).toContainText("$1.24M");
    // Let the entrance animation settle so contrast is measured at rest.
    await page.waitForTimeout(400);
    const results = await new AxeBuilder({ page })
      .include('[data-testid="dashboard-hud"]')
      .include('[data-testid="dashboard-board"]')
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();
    const summary = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    );
    expect(summary, summary.join("\n")).toEqual([]);
  });
}
