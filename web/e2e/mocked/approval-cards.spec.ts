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
