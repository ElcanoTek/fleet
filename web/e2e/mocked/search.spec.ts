import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for full-text search (#308): Cmd/Ctrl+K opens the search palette, a
// debounced query renders ranked results with <mark> highlights, Escape closes
// it, and clicking a result loads that conversation. /api/search is intercepted
// so the suite is deterministic (no Go chat-server).

async function mockSearch(page: Page) {
  await page.route("**/api/search**", (r: Route) =>
    r.fulfill({
      json: {
        results: [
          {
            conversation_id: "conv-search-1",
            title: "Python async patterns",
            match_preview: "the async <mark>function</mark> you asked about",
            matched_at: 1719432000,
          },
        ],
        total: 1,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("Ctrl+K opens search, shows highlighted results, Escape closes", async ({ page }) => {
  await mockChatBoot(page);
  await mockSearch(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.keyboard.press("Control+k");
  await expect(page.getByTestId("search-input")).toBeVisible();

  await page.getByTestId("search-input").fill("python");
  const result = page.getByTestId("search-result").first();
  await expect(result).toBeVisible({ timeout: 5_000 });
  await expect(result).toContainText("Python async patterns");
  // The preview's matched term renders as a real <mark> element (sanitized HTML).
  await expect(result.locator("mark")).toHaveText("function");

  await page.keyboard.press("Escape");
  await expect(page.getByTestId("search-input")).toBeHidden();
});

test("clicking a search result loads that conversation", async ({ page }) => {
  await mockChatBoot(page);
  await mockSearch(page);
  // The result click calls loadConversation → GET /api/conversations/<id>.
  await page.route("**/api/conversations/conv-search-1", (r: Route) =>
    r.fulfill({
      json: {
        conversation: {
          id: "conv-search-1",
          title: "Python async patterns",
          persona: "default",
          model: "",
          pinned: false,
          created_at: 1719432000,
          updated_at: 1719432000,
        },
        history: [{ role: "user", type: "text", content: { text: "tell me about async functions" } }],
      },
    }),
  );
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.keyboard.press("Control+k");
  await page.getByTestId("search-input").fill("python");
  await page.getByTestId("search-result").first().click();

  // Palette closes and the chosen conversation's content renders (scope to the
  // conversation body — the same text also appears in the title-rename button).
  await expect(page.getByTestId("search-input")).toBeHidden();
  await expect(
    page.getByLabel("Conversation", { exact: true }).getByText("tell me about async functions"),
  ).toBeVisible({ timeout: 10_000 });
});
