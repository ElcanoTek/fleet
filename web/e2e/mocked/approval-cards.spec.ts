import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the approval-card cluster after the cards rework:
//
//   1. A non-email critical tool (a pages deploy) renders the GENERIC action
//      card — never the email chrome it used to fall through to.
//   2. RESOLVED approvals re-hydrate on conversation open: the notify-mode
//      "ran without asking" record (with its undo hint) and a timed-out card
//      survive a reload instead of vanishing with the SSE stream.
//   3. A timed-out card offers one-click "Ask again", which submits a user
//      turn asking the agent to re-stage.
//
// Every /api/* call is Playwright-intercepted; the conversation GET payload
// mirrors the server's pending_approvals / resolved_approvals contract.

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

// One conversation whose history holds the deploy's tool_call, so the reload
// path can anchor the resolved card to the message that staged it.
async function mockConversationWithApprovals(page: Page) {
  await mockChatBoot(page, {
    conversations: [{ id: "conv-cards", title: "Pages deploy thread" }],
  });
  await page.route("**/api/conversations/conv-cards", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: {
          id: "conv-cards",
          title: "Pages deploy thread",
          persona: "default",
          model: "test-model",
          pinned: false,
        },
        history: [
          { id: 1, role: "user", type: "text", content: { text: "publish the q3 page and email me" } },
          { id: 2, role: "assistant", type: "text", content: { text: "Deploying the page now." } },
          {
            id: 3,
            role: "assistant",
            type: "tool_call",
            content: { id: "call-deploy-1", name: "mcp_pages_deploy_page", input: `{"slug":"q3-report"}` },
          },
          {
            id: 4,
            role: "tool",
            type: "tool_result",
            content: {
              id: "call-deploy-1",
              name: "mcp_pages_deploy_page",
              text: `{"version":{"id":"143"},"status":"approved"}`,
              is_err: false,
            },
          },
        ],
        pending_approvals: [],
        resolved_approvals: [
          {
            approval_id: "ap-record-1",
            tool: "mcp_pages_deploy_page",
            summary: {
              tool: "mcp_pages_deploy_page",
              args: [{ key: "slug", value: "q3-report" }],
            },
            status: "approved",
            is_err: false,
            result_text:
              "Ran without asking: this tool is declared notify-mode in the client bundle. Undo with mcp_pages_rollback_page(slug, version_id).",
            recorded: true,
            tool_call_id: "call-deploy-1",
          },
          {
            approval_id: "ap-timeout-1",
            tool: "mcp_sendgrid_send_email",
            summary: {
              tool: "mcp_sendgrid_send_email",
              to: "brad@example.com",
              subject: "Q3 page is live",
              content: "<p>done</p>",
              content_type: "text/html",
            },
            status: "rejected",
            result_text: "Approval timed out — auto-denied. The action was not taken.",
          },
        ],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator('[data-conversation-id="conv-cards"]');
  await row.waitFor({ timeout: 15_000 });
  await row.click();
}

test("resolved cards survive a reload: the notify record and its undo hint re-hydrate on the generic card", async ({
  page,
}) => {
  await mockConversationWithApprovals(page);

  const record = page.getByTestId("generic-action-card");
  await expect(record).toBeVisible();
  await expect(record).toContainText("Deploy page · ran without asking");
  await expect(record).toContainText("mcp_pages_rollback_page");
  // The deploy is anchored to the assistant message that staged it, and it
  // never wears the email chrome.
  await expect(record).not.toContainText(/email sent/i);
});

test("a timed-out email card re-hydrates and Ask again submits a re-stage turn", async ({ page }) => {
  await mockConversationWithApprovals(page);

  const timedOut = page.locator('[data-approval-id="ap-timeout-1"]');
  await expect(timedOut).toBeVisible();
  await expect(timedOut).toContainText("Send cancelled");
  await expect(timedOut).toContainText("Approval timed out");

  // Ask again fires a new user turn asking the agent to re-stage.
  const chat = page.waitForRequest(
    (req) => req.url().includes("/api/chat") && req.method() === "POST",
  );
  await timedOut.getByTestId("approval-ask-again").click();
  const req = await chat;
  expect(req.postData() ?? "").toContain("timed out");
});

// A bundle can require a decision per call for a critical tool
// (agent_policy.critical_tool_no_session_approval). The pending card arrives
// with no_session_approval: true on reload and offers no apply-all checkbox;
// a card for any other tool keeps it.
test("a per-call tool's pending card has no apply-all checkbox; other cards keep it", async ({ page }) => {
  await mockChatBoot(page, {
    conversations: [{ id: "conv-per-call", title: "Per-call thread" }],
  });
  await page.route("**/api/conversations/conv-per-call", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: { id: "conv-per-call", title: "Per-call thread", persona: "default", model: "test-model", pinned: false },
        history: [
          { id: 1, role: "user", type: "text", content: { text: "create the deal and publish the page" } },
          { id: 2, role: "assistant", type: "text", content: { text: "Both need your approval." } },
        ],
        pending_approvals: [
          {
            approval_id: "ap-per-call",
            tool: "mcp_deals_create_deal",
            summary: { tool: "mcp_deals_create_deal", args: [{ key: "deal_name", value: "Q4 Video" }] },
            no_session_approval: true,
          },
          {
            approval_id: "ap-batchable",
            tool: "mcp_pages_deploy_page",
            summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3-report" }] },
            no_session_approval: false,
          },
        ],
        resolved_approvals: [],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator('[data-conversation-id="conv-per-call"]');
  await row.waitFor({ timeout: 15_000 });
  await row.click();

  const perCall = page.locator('[data-approval-id="ap-per-call"]');
  await expect(perCall).toBeVisible();
  await expect(perCall.getByRole("button", { name: "Approve & run" })).toBeVisible();
  await expect(perCall.getByTestId("approval-apply-all")).toHaveCount(0);

  const batchable = page.locator('[data-approval-id="ap-batchable"]');
  await expect(batchable.getByTestId("approval-apply-all")).toBeVisible();
});

// A bundle-declared describer's readable card (docs/APPROVAL-CARD-DESCRIBERS.md)
// re-hydrates from the conversation GET on both the pending and the resolved
// card: plain-words title, the record with its id, link and flag, before →
// after, and the raw arguments collapsed under Details. The resolved one
// collapses to a one-line outcome.
test("a described card renders in plain words, pending and resolved", async ({ page }) => {
  const card = {
    title: "Update 1 deal",
    subtitle: "Raise the floor",
    items: [
      {
        label: "Q4 Video",
        id: "PM-123",
        link: "https://ssp.example.com/deals/123",
        changes: [{ label: "Floor", before: "$2.00", after: "$2.50" }],
        flags: [{ code: "deal_active", label: "Deal is Active" }],
      },
    ],
  };
  await mockChatBoot(page, {
    conversations: [{ id: "conv-readable", title: "Readable thread" }],
  });
  await page.route("**/api/conversations/conv-readable", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: { id: "conv-readable", title: "Readable thread", persona: "default", model: "test-model", pinned: false },
        history: [
          { id: 1, role: "user", type: "text", content: { text: "raise the floor on Q4 Video" } },
          { id: 2, role: "assistant", type: "text", content: { text: "Here is the change." } },
        ],
        pending_approvals: [
          {
            approval_id: "ap-readable",
            tool: "mcp_deals_update_deal",
            summary: { tool: "mcp_deals_update_deal", args: [{ key: "etag", value: 'W/"77"' }] },
            card,
          },
        ],
        resolved_approvals: [
          {
            approval_id: "ap-readable-done",
            tool: "mcp_deals_update_deal",
            summary: { tool: "mcp_deals_update_deal", args: [{ key: "etag", value: 'W/"76"' }] },
            status: "approved",
            is_err: false,
            result_text: '{"updated":1}',
            card: { ...card, title: "Update 1 deal (earlier)" },
          },
        ],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator('[data-conversation-id="conv-readable"]');
  await row.waitFor({ timeout: 15_000 });
  await row.click();

  const pending = page.locator('[data-approval-id="ap-readable"]');
  await expect(pending.getByTestId("generic-action-title")).toHaveText("Update 1 deal");
  const item = pending.getByTestId("approval-card-item");
  await expect(item).toContainText("Q4 Video");
  await expect(item).toContainText("PM-123");
  await expect(item.getByRole("link", { name: /Open/ })).toHaveAttribute("href", "https://ssp.example.com/deals/123");
  await expect(item.getByTestId("approval-card-change")).toContainText("$2.50");
  await expect(item.getByTestId("approval-card-flag")).toHaveText("Deal is Active");
  // The raw arguments are behind Details, collapsed.
  const details = pending.getByTestId("approval-card-details");
  await expect(details.getByText('W/"77"')).toBeHidden();
  await details.locator("summary").click();
  await expect(details.getByText('W/"77"')).toBeVisible();
  await expect(pending.getByRole("button", { name: "Approve & run" })).toBeVisible();

  const done = page.locator('[data-approval-id="ap-readable-done"]');
  await expect(done.getByTestId("generic-action-title")).toHaveText("Applied · Update 1 deal (earlier)");
  await expect(done.getByTestId("approval-card-readable")).toHaveCount(0);
  await expect(done.getByTestId("approval-result")).toContainText('{"updated":1}');
});

// Grouped approvals (docs/GROUPED-APPROVALS.md): two pending cards one turn
// staged for tools in agent_policy.critical_tool_group_approval share a
// group_id and render as ONE card. Unchecking a row and pressing Approve all
// sends one group decision (approve the checked, decline the unchecked); the
// answers settle each call on its own card. "One at a time" falls back to the
// individual cards.
async function mockGroupedConversation(page: Page, conv: string) {
  await mockChatBoot(page, {
    conversations: [{ id: conv, title: "Plan thread" }],
  });
  await page.route(`**/api/conversations/${conv}`, (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: { id: conv, title: "Plan thread", persona: "default", model: "test-model", pinned: false },
        history: [
          { id: 1, role: "user", type: "text", content: { text: "book the plan" } },
          { id: 2, role: "assistant", type: "text", content: { text: "Two systems need your approval." } },
        ],
        pending_approvals: [
          {
            approval_id: "ap-grp-pm",
            tool: "mcp_pubmatic_execute_plan",
            summary: { tool: "mcp_pubmatic_execute_plan", args: [{ key: "plan_id", value: "p1" }] },
            group_id: "grp-1",
            mcp_server: "pubmatic",
            mcp_account: "client_a",
            card: { title: "Create 12 deals on PubMatic", items: [{ label: "Q4 Video", id: "PM-1" }] },
          },
          {
            approval_id: "ap-grp-mg",
            tool: "mcp_magnite_execute_plan",
            summary: { tool: "mcp_magnite_execute_plan", args: [{ key: "plan_id", value: "p1" }] },
            group_id: "grp-1",
          },
        ],
        resolved_approvals: [],
        pending_memory_proposals: [],
      },
    });
  });
  await page.goto("/chat");
  const row = page.locator(`[data-conversation-id="${conv}"]`);
  await row.waitFor({ timeout: 15_000 });
  await row.click();
}

test("a turn's grouped cards render as one card and Approve all sends one decision", async ({ page }) => {
  await mockGroupedConversation(page, "conv-group");
  let posted: unknown = null;
  await page.route("**/api/conversations/conv-group/approval-groups/grp-1", (r: Route) => {
    posted = r.request().postDataJSON();
    return r.fulfill({
      json: {
        group_id: "grp-1",
        results: [
          { approval_id: "ap-grp-pm", decision: "approve", status_code: 200, result: { status: "approved", is_err: false, result_text: "12 deals created" } },
          { approval_id: "ap-grp-mg", decision: "decline", status_code: 200, result: { status: "rejected" } },
        ],
      },
    });
  });

  const group = page.getByTestId("approval-group-card");
  await expect(group).toBeVisible();
  await expect(group.getByTestId("approval-group-title")).toHaveText("2 actions to approve");
  await expect(group.getByTestId("approval-group-row")).toHaveCount(2);
  await expect(group).toContainText("Create 12 deals on PubMatic");
  await expect(group).toContainText("Runs as client_a on pubmatic");
  // The individual cards are not rendered beside the group.
  await expect(page.getByTestId("generic-action-card")).toHaveCount(0);

  await group.getByTestId("approval-group-check").nth(1).uncheck();
  await group.getByRole("button", { name: "Approve all (1)" }).click();

  // Each call now shows its own outcome card.
  await expect(page.locator('[data-approval-id="ap-grp-pm"]').getByTestId("generic-action-title")).toHaveText(
    "Applied · Create 12 deals on PubMatic",
  );
  await expect(page.locator('[data-approval-id="ap-grp-mg"]').getByTestId("generic-action-title")).toHaveText(
    "Execute plan · cancelled",
  );
  await expect(page.getByTestId("approval-group-card")).toHaveCount(0);
  expect(posted).toEqual({ approve: ["ap-grp-pm"], decline: ["ap-grp-mg"] });
});

test("One at a time falls back to the individual approval cards", async ({ page }) => {
  await mockGroupedConversation(page, "conv-group-split");
  const group = page.getByTestId("approval-group-card");
  await expect(group).toBeVisible();
  await group.getByRole("button", { name: "One at a time" }).click();
  await expect(page.getByTestId("approval-group-card")).toHaveCount(0);
  await expect(page.locator('[data-approval-id="ap-grp-pm"]').getByRole("button", { name: "Approve & run" })).toBeVisible();
  await expect(page.locator('[data-approval-id="ap-grp-mg"]').getByRole("button", { name: "Approve & run" })).toBeVisible();
});
