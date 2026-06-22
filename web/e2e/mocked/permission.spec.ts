import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

// Mocked e2e for the EXTERNAL-agent permission UI. An external ACP agent
// (Claude Code / Goose) self-executes in a locked sandbox and, when it wants to
// do something sensitive, calls session/request_permission — which fleet surfaces
// as a `permission.requested` SSE event. The chat shell renders an inline
// allow/deny prompt; the user's decision POSTs to
// /api/conversations/{id}/permissions/{requestId}. There is no "approve all";
// default-deny on timeout is enforced server-side.
//
// Every /api/* call is intercepted by Playwright (no Go chat-server), so the
// suite is deterministic.

// A turn where the external agent streams an opening line, requests permission,
// and the SSE stays open until we observe the user's decision via the mocked
// permission endpoint, then completes.
async function mockPermissionTurn(page: Page, onDecision: (body: unknown) => void) {
  await page.route("**/api/chat", (r: Route) =>
    fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-1", title: "New chat", persona: "default" } },
      { event: "governance", id: 2, data: { tier: "delegated", image: "localhost/ext:latest" } },
      { event: "text.delta", id: 3, data: { text: "I'll update the config. " } },
      {
        event: "permission.requested",
        id: 4,
        data: {
          request_id: "perm-1",
          tool_call_id: "call_edit",
          title: "Modifying critical configuration file",
          kind: "edit",
          locations: ["/workspace/config.json"],
          options: [
            { optionId: "allow", name: "Allow this change", kind: "allow_once" },
            { optionId: "reject", name: "Skip this change", kind: "reject_once" },
          ],
        },
      },
      // In a real turn the agent's reply continues after the decision; for the
      // mocked stream we close the turn so the assistant message settles. The
      // card's own POST + optimistic flip is what we assert.
      { event: "turn.completed", id: 5, data: { cost_usd: 0.0, model: "claude-code" } },
    ]),
  );

  await page.route("**/api/conversations/*/permissions/*", (r: Route) => {
    onDecision(JSON.parse(r.request().postData() ?? "{}"));
    return r.fulfill({ json: { resolved: true } });
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("an external agent's permission request renders an inline allow/deny prompt", async ({ page }) => {
  await mockChatBoot(page);
  await mockPermissionTurn(page, () => {});
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("update the config");
  await composer.press("Enter");

  // The permission card surfaces with the agent's description + the affected path.
  const card = page.getByTestId("permission-card");
  await expect(card).toBeVisible({ timeout: 15_000 });
  await expect(card).toContainText("Modifying critical configuration file");
  await expect(card).toContainText("/workspace/config.json");

  // Allow + Deny actions are present; there is exactly ONE allow button (the
  // single allow_once option) — no one-click "approve all".
  await expect(page.getByTestId("permission-allow")).toHaveCount(1);
  await expect(page.getByTestId("permission-deny")).toBeVisible();
});

test("allowing the request POSTs the decision with the chosen option id", async ({ page }) => {
  let decision: unknown = null;
  await mockChatBoot(page);
  await mockPermissionTurn(page, (b) => {
    decision = b;
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("update the config");
  await composer.press("Enter");

  await page.getByTestId("permission-allow").click();

  // The card flips to its allowed terminal state, and the decision POSTed the
  // allow + the agent's option id.
  await expect(page.getByTestId("permission-card")).toContainText("Permission allowed", {
    timeout: 15_000,
  });
  expect(decision).toEqual({ allowed: true, option_id: "allow" });
});

test("denying the request POSTs a deny decision", async ({ page }) => {
  let decision: unknown = null;
  await mockChatBoot(page);
  await mockPermissionTurn(page, (b) => {
    decision = b;
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("update the config");
  await composer.press("Enter");

  await page.getByTestId("permission-deny").click();

  await expect(page.getByTestId("permission-card")).toContainText("Permission denied", {
    timeout: 15_000,
  });
  expect(decision).toEqual({ allowed: false, option_id: "" });
});
