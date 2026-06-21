import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked P7 e2e for the chat view: an authenticated /chat load reaches the
// empty composer state, and sending a message streams a mocked SSE turn that
// renders the assistant reply. Every /api/* call the chat shell makes is
// intercepted by Playwright (no Go chat-server; that's P8). CHAT_MOCK_MODE=1 is
// set on the dev server so the same mock contract the P8 harness relies on
// stays wired.

function sse(frames: Array<{ event: string; data: unknown; id?: number }>): string {
  return (
    frames
      .map((f) => {
        const lines = [`event: ${f.event}`];
        if (f.id !== undefined) lines.push(`id: ${f.id}`);
        lines.push(`data: ${JSON.stringify(f.data)}`);
        return lines.join("\n");
      })
      .join("\n\n") + "\n\n"
  );
}

async function mockChat(page: Page) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/personas", (r: Route) =>
    r.fulfill({ json: { personas: [{ id: "default", name: "Default" }], default: "default" } }),
  );
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: {
        lockdown_available: false,
        lockdown_only: false,
        lockdown_allowed_models: [],
        julesEnabled: false,
      },
    }),
  );
  await page.route("**/api/mcp-servers", (r: Route) => r.fulfill({ json: { servers: [] } }));
  await page.route("**/api/conversations", (r: Route) => {
    if (r.request().method() === "GET") return r.fulfill({ json: { conversations: [] } });
    return r.fulfill({ json: {} });
  });
  await page.route("**/api/model-rankings", (r: Route) => r.fulfill({ json: { rankings: [] } }));
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: { models: [] } }));
  await page.route("**/api/model-check**", (r: Route) => r.fulfill({ json: { ok: true } }));

  // The streaming turn: a conversation frame (assigns the real id), a couple of
  // text deltas, then turn.completed.
  await page.route("**/api/chat", (r: Route) => {
    const body = sse([
      { event: "conversation", id: 1, data: { id: "conv-1", title: "New chat", persona: "default" } },
      { event: "text.delta", id: 2, data: { text: "Hello" } },
      { event: "text.delta", id: 3, data: { text: " from the mock." } },
      { event: "turn.completed", id: 4, data: { cost_usd: 0, model: "anthropic/claude-opus-4.8" } },
    ]);
    return r.fulfill({
      status: 200,
      headers: { "Content-Type": "text/event-stream; charset=utf-8", "Cache-Control": "no-cache" },
      body,
    });
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("authenticated /chat reaches the empty composer state", async ({ page }) => {
  await mockChat(page);
  await page.goto("/chat");
  await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
    timeout: 15_000,
  });
});

test("sending a message streams a mocked assistant reply", async ({ page }) => {
  await mockChat(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await composer.fill("Hi there");
  await composer.press("Enter");

  await expect(page.getByText("Hello from the mock.")).toBeVisible({ timeout: 15_000 });
});
