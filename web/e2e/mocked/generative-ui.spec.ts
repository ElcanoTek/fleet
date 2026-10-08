import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for generative-UI cards (show_ui, docs/GENERATIVE-UI.md):
//
//   1. A show_ui tool call in the conversation history renders as an
//      interactive card on reload — the card's spec IS the persisted tool
//      call input, so there is no separate staging payload to mock.
//   2. Templates recompute as the user edits (a repeater count drives a stat).
//   3. Submit validates, confirms, and sends ONE ordinary user turn carrying
//      "[UI submission] card=<id> action=<id>" + the values — the agent acts
//      on it through the normal governed turn.

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

const CARD = {
  title: "New campaign",
  description: "{{ count(lines) * len(exchanges) }} deal(s) will be created",
  components: [
    {
      type: "columns",
      children: [
        { type: "text_input", id: "advertiser", label: "Advertiser", required: true },
        { type: "text_input", id: "agency", label: "Agency", placeholder: "NA if none" },
      ],
    },
    {
      type: "columns",
      children: [
        { type: "date", id: "start", label: "Start", value: "2026-11-01" },
        { type: "date", id: "end", label: "End", value: "2026-12-31" },
      ],
    },
    {
      type: "multi_select",
      id: "exchanges",
      label: "Exchanges",
      options: ["Exchange A", "Exchange B", "Exchange C"],
      value: ["Exchange A", "Exchange B"],
      help: "Options were fetched from the connected exchange tools.",
    },
    {
      type: "repeater",
      id: "lines",
      label: "Lines",
      item_label: "Line {{ index }} · {{ channel }} · {{ cpm ? '$' + fixed(cpm, 2) : 'default floor' }}",
      add_label: "Add line",
      max_items: 10,
      value: [{ channel: "Display", cpm: 3 }],
      fields: [
        { type: "choice", id: "channel", label: "Channel", options: ["Display", "Video", "CTV"], required: true },
        { type: "number", id: "cpm", label: "Floor CPM", prefix: "$", min: 0.1, step: 0.01 },
        {
          type: "include_exclude",
          id: "geo",
          label: "Geography",
          options: ["US", "CA", "MX", "GB"],
          include_label: "Allow",
          exclude_label: "Block",
          value: { include: ["US"], exclude: [] },
        },
        { type: "list_input", id: "domains", label: "Domain block list", visible_if: "channel != 'CTV'" },
      ],
    },
    {
      type: "columns",
      children: [
        { type: "stat", label: "Deals", value: "{{ count(lines) * len(exchanges) }}" },
        { type: "stat", label: "Avg floor", value: "{{ avg(lines.cpm) ? '$' + fixed(avg(lines.cpm), 2) : '—' }}" },
      ],
    },
    {
      type: "status_list",
      items: [
        { status: "pass", label: "Seats found for every exchange" },
        { status: "warn", label: "Line 1 floor is below the usual $5 for Display", field: "lines[0].cpm" },
      ],
    },
  ],
  actions: [
    { id: "check", label: "Check against live accounts", style: "secondary" },
    { id: "create", label: "Create deals", style: "primary", confirm: "Create these deals now?" },
  ],
};

async function openConversationWithCard(page: Page) {
  await mockChatBoot(page, { conversations: [{ id: "conv-genui", title: "Campaign setup" }] });
  await page.route("**/api/conversations/conv-genui", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: { id: "conv-genui", title: "Campaign setup", persona: "default", model: "test-model", pinned: false },
        history: [
          { id: 1, role: "user", type: "text", content: { text: "set up deals for the new Acme campaign" } },
          { id: 2, role: "assistant", type: "text", content: { text: "Here's a builder with the exchanges you have connected." } },
          {
            id: 3,
            role: "assistant",
            type: "tool_call",
            content: { id: "call-ui-1", name: "show_ui", input: JSON.stringify(CARD) },
          },
          {
            id: 4,
            role: "tool",
            type: "tool_result",
            content: { id: "call-ui-1", name: "show_ui", text: 'UI_DISPLAYED card_id=call-ui-1: the user can now see the card "New campaign".', is_err: false },
          },
        ],
        pending_approvals: [],
        resolved_approvals: [],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator('[data-conversation-id="conv-genui"]');
  await row.waitFor({ timeout: 15_000 });
  await row.click();
}

test("a show_ui card renders from history, recomputes live, and submits as a user turn", async ({ page }) => {
  await openConversationWithCard(page);
  const card = page.getByTestId("genui-card");
  await expect(card).toBeVisible();
  await expect(card).toContainText("New campaign");
  await expect(card).toContainText("2 deal(s) will be created");

  // Add a CTV line: the deal count follows, and the CTV line hides the
  // domain list (visible_if).
  await card.getByRole("button", { name: "+ Add line" }).click();
  const line2 = card.locator('[data-repeater-item="1"]');
  await line2.getByRole("radio", { name: "CTV" }).click();
  await expect(line2.getByLabel("Domain block list")).toHaveCount(0);
  await line2.getByLabel("Floor CPM").fill("22");
  await expect(card).toContainText("4 deal(s) will be created");

  // Submit without the required advertiser: blocked, with the field flagged.
  await card.getByRole("button", { name: "Create deals" }).click();
  await expect(card.getByRole("status")).toContainText("Fix 1 field");
  await card.getByLabel(/Advertiser/).fill("Acme");

  const chat = page.waitForRequest((req) => req.url().includes("/api/chat") && req.method() === "POST");
  await card.getByRole("button", { name: "Create deals" }).click();
  await card.getByRole("button", { name: "Yes, create deals" }).click();
  const body = JSON.parse((await chat).postData() ?? "{}") as { message?: string };
  expect(body.message ?? "").toMatch(/^\[UI submission\] card=call-ui-1 action=create\n```json\n/);
  const values = JSON.parse((body.message ?? "").split("\n").slice(2, -1).join("\n"));
  expect(values.advertiser).toBe("Acme");
  expect(values.lines).toHaveLength(2);
  expect(values.lines[1]).toMatchObject({ channel: "CTV", cpm: 22, geo: { include: ["US"], exclude: [] } });
  expect(values.lines[1]).not.toHaveProperty("domains");
});
