import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

// Mocked e2e for the empty-state protocol pills. The catalog is config-driven
// via /api/client-config; stubbing it to return NO cards forces the neutral
// DEFAULT_PILLS fallback (Summarize a document / Analyze a dataset / Draft
// something). The pills render only under the "victoria" persona. We then drive
// a form pill end-to-end: submitting its templated prompt posts to /api/chat,
// which we mock to echo the prompt back as the assistant reply.

// Mocks /api/chat to echo whatever prompt was submitted, so a spec can assert
// "the pill sent the prompt I expected".
async function mockEchoChat(page: Page) {
  await page.route("**/api/chat", (r: Route) => {
    const body = JSON.parse(r.request().postData() ?? "{}") as { message?: string };
    return fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-1", title: "New chat", persona: "victoria" } },
      { event: "text.delta", id: 2, data: { text: `Mock reply to: ${body.message ?? ""}` } },
      { event: "turn.completed", id: 3, data: { cost_usd: 0, model: "anthropic/claude-opus-4.8" } },
    ]);
  });
}

test.beforeEach(async ({ context, page }) => {
  await loginViaCookie(context);
  await mockChatBoot(page, { personaDefault: "victoria" });
  // Empty cards → neutral DEFAULT_PILLS fallback.
  await page.route("**/api/client-config", (r: Route) =>
    r.fulfill({ json: { branding: {}, empty_state: { cards: [] } } }),
  );
  await mockEchoChat(page);
});

test("empty state surfaces the neutral fallback pills", async ({ page }) => {
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await expect(page.getByRole("button", { name: /summarize a document/i })).toBeVisible();
  await expect(page.getByRole("button", { name: /analyze a dataset/i })).toBeVisible();
  await expect(page.getByRole("button", { name: /draft something/i })).toBeVisible();
});

test("the summarize form templates its prompt and sends it", async ({ page }) => {
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.getByRole("button", { name: /summarize a document/i }).click();

  // The inline form shows the static template (no required fields → ready now).
  await expect(page.getByText(/Summarize the attached\/pasted document\./i)).toBeVisible();
  await page.getByRole("button", { name: /^summarize$/i }).click();

  // The mocked chat echoes the submitted prompt back as the assistant reply.
  await expect(
    page.getByText(/Mock reply to:\s*Summarize the attached\/pasted document\./i),
  ).toBeVisible({ timeout: 15_000 });
});

test("the draft form gates on its required field then sends the template", async ({ page }) => {
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.getByRole("button", { name: /draft something/i }).click();
  await page.getByLabel(/what should i draft/i).fill("a follow-up email");
  await page.getByRole("button", { name: /draft it/i }).click();

  await expect(
    page.getByText(/Mock reply to:\s*Draft the following for me/i),
  ).toBeVisible({ timeout: 15_000 });
});
