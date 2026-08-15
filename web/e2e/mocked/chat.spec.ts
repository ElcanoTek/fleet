import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

// Mocked e2e for the chat view. An authenticated /chat load reaches the empty
// composer state, and sending a message streams a mocked SSE turn — text deltas,
// a tool.call, its tool.result, and a final assistant message — that the shell
// renders. Every /api/* call is intercepted by Playwright (no Go chat-server),
// so the suite is deterministic. CHAT_MOCK_MODE=1 is set on the server so the
// same mock contract the live harness relies on stays wired.

// A streaming turn that exercises the full event vocabulary the chat shell
// handles: conversation (assigns the id), two text deltas, a tool.call + its
// tool.result, a closing text delta, then turn.completed.
async function mockStreamingTurn(page: Page) {
  await page.route("**/api/chat", (r: Route) =>
    fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-1", title: "New chat", persona: "default" } },
      { event: "text.delta", id: 2, data: { text: "Let me check the weather. " } },
      { event: "tool.call", id: 3, data: { id: "call-1", name: "get_weather", input: JSON.stringify({ city: "Boston" }) } },
      { event: "tool.result", id: 4, data: { id: "call-1", name: "get_weather", text: "Boston: 72F and sunny.", is_err: false } },
      { event: "text.delta", id: 5, data: { text: "It is 72F and sunny in Boston." } },
      { event: "turn.completed", id: 6, data: { cost_usd: 0.001, model: "anthropic/claude-opus-4.8" } },
    ]),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("authenticated /chat reaches the empty composer state", async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
  // The composer is mounted and ready for input.
  await expect(page.getByRole("textbox").first()).toBeVisible();
});

test("a Git-backed library prompt can be selected into the chat composer", async ({ page }) => {
  const content = "name: Weekly brief\ngoal: Summarize the week\n";
  await mockChatBoot(page);
  await page.route("**/api/orchestrator/prompts", (r: Route) =>
    r.fulfill({
      json: [
        {
          id: "git:weekly.yaml",
          name: "Weekly brief",
          description: "Summarize the week",
          content,
          source: "git",
          visibility: "workspace",
          read_only: true,
          owned_by_caller: false,
          path: "prompts/weekly.yaml",
        },
      ],
    }),
  );

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "Open prompt library" }).click();
  await expect(page.getByRole("dialog", { name: "Prompt library" })).toBeVisible();
  await page.getByRole("button", { name: "Use prompt" }).click();
  await expect(page.getByRole("textbox").first()).toHaveValue(content);
});

test("a sent turn streams text deltas and a final assistant message", async ({ page }) => {
  await mockChatBoot(page);
  await mockStreamingTurn(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("What's the weather in Boston?");
  await composer.press("Enter");

  // The user's message renders inside the conversation (scoped to avoid the
  // conversation-title button, which echoes the same text), then the streamed
  // assistant text (deltas concatenated) lands as the final message.
  const conversation = page.getByRole("region", { name: "Conversation" });
  await expect(conversation.getByText("What's the weather in Boston?")).toBeVisible();
  await expect(page.getByText("It is 72F and sunny in Boston.")).toBeVisible({ timeout: 15_000 });
});

test("the streamed tool.call and tool.result render in the execution trail", async ({ page, context }) => {
  // The tool-call chips live behind the "Show details" toggle (showStats),
  // persisted in localStorage. Pre-seed it ON so the execution trail renders.
  await context.addInitScript(() => window.localStorage.setItem("chat-show-stats", "1"));
  await mockChatBoot(page);
  await mockStreamingTurn(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("What's the weather in Boston?");
  await composer.press("Enter");

  // The tool.call surfaces as a chip labeled with the tool name.
  const toolChip = page.getByRole("button", { name: /get_weather/i });
  await expect(toolChip).toBeVisible({ timeout: 15_000 });

  // Expanding the chip reveals the tool.result text (the is_err=false branch).
  await toolChip.click();
  await expect(page.getByText("Boston: 72F and sunny.")).toBeVisible();

  // And the final assistant message is still present.
  await expect(page.getByText("It is 72F and sunny in Boston.")).toBeVisible();
});

// A delegation turn: the spawn tool call, the child's live subagent.progress
// steps, then the spawn's JSON result. The progress events are what make a
// running sub-agent visible — before them the chip showed the task text and
// nothing else for the child's whole (multi-minute) life (#1043 follow-up).
async function mockDelegationTurn(page: Page) {
  await page.route("**/api/chat", (r: Route) =>
    fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-1", title: "New chat", persona: "default" } },
      {
        event: "tool.call",
        id: 2,
        data: {
          id: "call-sub-1",
          name: "spawn_subagent",
          input: JSON.stringify({ task: "Summarize the last 3 emails", role: "explore" }),
        },
      },
      {
        event: "subagent.progress",
        id: 3,
        data: {
          tool_call_id: "call-sub-1",
          child_session_id: "subagent-11111111-2222-3333-4444-555555555555",
          role: "explore",
          phase: "started",
          task: "Summarize the last 3 emails",
        },
      },
      {
        event: "subagent.progress",
        id: 4,
        data: {
          tool_call_id: "call-sub-1",
          child_session_id: "subagent-11111111-2222-3333-4444-555555555555",
          role: "explore",
          phase: "tool",
          tool: "outlook_email_search",
          detail: "query=is:unread",
          step: 1,
        },
      },
      {
        event: "subagent.progress",
        id: 5,
        data: {
          tool_call_id: "call-sub-1",
          child_session_id: "subagent-11111111-2222-3333-4444-555555555555",
          role: "explore",
          phase: "finished",
          success: true,
          cost_usd: 0.0042,
          tokens: 3100,
          steps: 2,
          tools_used: ["outlook_email_search"],
          duration_ms: 9100,
        },
      },
      {
        event: "tool.result",
        id: 6,
        data: {
          id: "call-sub-1",
          name: "spawn_subagent",
          text: JSON.stringify({
            result: "Three emails: a renewal, a standup note, and an invoice.",
            success: true,
            role: "explore",
            child_session_id: "subagent-11111111-2222-3333-4444-555555555555",
            cost_usd: 0.0042,
            tokens: 3100,
            steps: 2,
            tools_used: ["outlook_email_search"],
          }),
          is_err: false,
        },
      },
      { event: "text.delta", id: 7, data: { text: "Here is the summary of your last 3 emails." } },
      { event: "turn.completed", id: 8, data: { cost_usd: 0.006, model: "anthropic/claude-opus-4.8" } },
    ]),
  );
}

test("a delegation streams live sub-agent activity, then settles into a child card", async ({
  page,
  context,
}) => {
  await context.addInitScript(() => window.localStorage.setItem("chat-show-stats", "1"));
  await mockChatBoot(page);
  await mockDelegationTurn(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("Summarize my last 3 emails with a sub-agent");
  await composer.press("Enter");

  // The delegation's chip opens itself, so the child's task and its work trail
  // are visible without hunting for a disclosure.
  await expect(page.getByText("Summarize the last 3 emails")).toBeVisible({ timeout: 15_000 });

  // Once the result lands, the live panel retires and the child card carries
  // the durable trail: status, spend, steps, and the tools the child used.
  const card = page.getByTestId("chat-subagent-card");
  await expect(card).toBeVisible({ timeout: 15_000 });
  await expect(card).toContainText("done");
  await expect(card).toContainText("explore");
  await expect(card).toContainText("2 steps");
  await expect(card).toContainText("outlook_email_search");
  await expect(page.getByTestId("chat-subagent-activity")).toHaveCount(0);
  await expect(page.getByText("Here is the summary of your last 3 emails.")).toBeVisible();
});

test("config-driven empty-state cards render from a stubbed /api/client-config", async ({ page }) => {
  // The protocol-pill empty-state cards render whenever the client config
  // supplies any (persona-AGNOSTIC — regression guard for #80, where the gate
  // was hardcoded to the legacy "victoria" persona and so never showed on a
  // stock install whose default persona is "assistant"). Their catalog is
  // config-driven via /api/client-config; stub a bespoke client catalog and
  // assert those cards (not the neutral defaults) render under the DEFAULT
  // persona.
  await mockChatBoot(page);
  await page.route("**/api/client-config", (r: Route) =>
    r.fulfill({
      json: {
        branding: { app_name: "Acme AI" },
        empty_state: {
          cards: [
            {
              id: "deal-report",
              type: "form",
              icon: "bar-chart",
              title: "Pull a deal report",
              desc: "Fetch yesterday's programmatic deal performance.",
              cta: "Pull report",
              promptTemplate: "Pull yesterday's deal report.",
            },
            {
              id: "draft-memo",
              type: "form",
              icon: "edit",
              title: "Draft a client memo",
              desc: "Summarize the week for a client.",
              cta: "Draft memo",
              promptTemplate: "Draft a weekly client memo.",
            },
          ],
        },
      },
    }),
  );

  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });

  // The config-driven cards render…
  await expect(page.getByRole("button", { name: /pull a deal report/i })).toBeVisible();
  await expect(page.getByRole("button", { name: /draft a client memo/i })).toBeVisible();
  // …and the neutral fallback pills (DEFAULT_PILLS) do NOT, proving the catalog
  // came from the stubbed config rather than the hardcoded default.
  await expect(page.getByRole("button", { name: /analyze a dataset/i })).toHaveCount(0);
});
