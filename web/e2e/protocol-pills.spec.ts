import { test, expect } from "./fixtures";

// Empty-state quick-start pills. The catalog is now CONFIG-DRIVEN: the live set
// comes from /api/client-config. To keep this spec client-agnostic, we stub that
// endpoint to return no cards, which forces the neutral DEFAULT_PILLS fallback
// (Summarize a document / Analyze a dataset / Draft something). Against the
// MOCKED chat-server the canned reply is "Mock reply to: <prompt>", so we assert
// a pill submitted the expected prompt by matching the echoed text. These cover
// the form↔conversation hybrid: a form pill (Summarize) and the
// conversation-first pill with its optional form (Analyze a dataset).

test.describe("protocol pills", () => {
  // Stub /api/client-config → empty cards so the UI falls back to DEFAULT_PILLS.
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/client-config", (route) =>
      route.fulfill({ json: { branding: {}, empty_state: { cards: [], protocol_pills: [] } } }),
    );
  });

  test("empty state surfaces the neutral fallback pills", async ({ page, login }) => {
    await login();
    await expect(page.getByRole("button", { name: /summarize a document/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /analyze a dataset/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /draft something/i })).toBeVisible();
  });

  test("summarize form templates its prompt and sends it", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /summarize a document/i }).click();

    // The inline form replaces the card grid; its prompt preview shows the
    // static template (no required fields, so it's ready immediately).
    await expect(page.getByText(/Summarize the attached\/pasted document\./i)).toBeVisible();

    await page.getByRole("button", { name: /^summarize$/i }).click();

    // Mock server echoes the submitted prompt back.
    await expect(
      page.getByText(/Mock reply to:\s*Summarize the attached\/pasted document\./i),
    ).toBeVisible({ timeout: 15_000 });
  });

  test("analyze-a-dataset starts the conversational intake", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /analyze a dataset/i }).click();

    // Conversation-first pill: the "Skip the form, start in Chat" link is the
    // chat path and sends the starterPrompt.
    await page.getByRole("button", { name: /skip the form, start in chat/i }).click();

    await expect(
      page.getByText(/I'd like to analyze a dataset/i).first(),
    ).toBeVisible({ timeout: 15_000 });
  });

  test("draft form gates on its required field then sends the template", async ({ page, login }) => {
    await login();
    await page.getByRole("button", { name: /draft something/i }).click();

    await page.getByLabel(/what should i draft/i).fill("a follow-up email");

    await page.getByRole("button", { name: /draft it/i }).click();

    await expect(
      page.getByText(/Mock reply to:\s*Draft the following for me/i),
    ).toBeVisible({ timeout: 15_000 });
  });
});
