import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the restaurant-style model cost indicators ($ … $$$$) in both
// model pickers. The catalog/rankings proxies are stubbed with a priced fixture
// so the tier arithmetic (blended 3 prompt : 1 completion, see
// shared/lib/modelCost.ts) is exercised end-to-end through the real UI:
//
//   glm-5.2      $0.40/$1.60 → $0.70/M  → $
//   gpt-5.6-sol  $1.25/$10   → $3.44/M  → $$
//   sonnet       $3/$15      → $6.00/M  → $$$
//   opus         $15/$75     → $30.00/M → $$$$
//
// The default mocks return EMPTY model lists (see _mocks.ts), so every other
// spec keeps exercising the no-pricing path where no indicator renders.

const PRICED = {
  models: [
    {
      slug: "z-ai/glm-5.2",
      name: "Z.AI: GLM 5.2",
      price_prompt: 0.0000004,
      price_completion: 0.0000016,
      context_length: 200000,
    },
    {
      slug: "openai/gpt-5.6-sol",
      name: "OpenAI: GPT-5.6 Sol",
      price_prompt: 0.00000125,
      price_completion: 0.00001,
      context_length: 400000,
    },
    {
      slug: "anthropic/claude-sonnet-4.5",
      name: "Anthropic: Claude Sonnet 4.5",
      price_prompt: 0.000003,
      price_completion: 0.000015,
    },
    {
      slug: "anthropic/claude-opus-4.8",
      name: "Anthropic: Claude Opus 4.8",
      price_prompt: 0.000015,
      price_completion: 0.000075,
    },
  ],
};

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the chat composer shows a cost tier per model and on the chip", async ({ page }) => {
  await mockChatBoot(page);
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: PRICED }));
  await page.route("**/api/model-rankings", (r: Route) => r.fulfill({ json: PRICED }));
  await page.goto("/chat");
  await expect(page.getByRole("textbox").first()).toBeVisible({ timeout: 20_000 });

  const chip = page.locator("button[aria-haspopup='listbox']").first();
  await chip.click();
  const listbox = page.locator("#composer-model-listbox");
  await expect(listbox.locator(".model-cost").first()).toBeVisible();

  // One indicator per row, each carrying the tier as data + accessible text.
  const rows = listbox.locator("[role='option']");
  const tierOf = (name: string) =>
    rows.filter({ hasText: name }).locator(".model-cost").first();
  await expect(tierOf("Z.AI: GLM 5.2")).toHaveAttribute("data-cost-tier", "1");
  await expect(tierOf("OpenAI: GPT-5.6 Sol")).toHaveAttribute("data-cost-tier", "2");
  await expect(tierOf("Claude Sonnet 4.5")).toHaveAttribute("data-cost-tier", "3");
  await expect(tierOf("Claude Opus 4.8")).toHaveAttribute("data-cost-tier", "4");
  await expect(tierOf("Claude Sonnet 4.5")).toHaveAttribute(
    "aria-label",
    /premium cost — about \$6\.00\/M tokens blended/,
  );

  // Picking a model moves its tier onto the collapsed chip, so the price band
  // of the model about to run is visible without reopening the picker.
  await rows.filter({ hasText: "Claude Opus 4.8" }).first().click();
  await expect(listbox).toHaveCount(0);
  await expect(chip.locator(".model-cost")).toHaveAttribute("data-cost-tier", "4");
});

test("the task form's model picker shows a cost tier per option", async ({ page }) => {
  await page.route("**/api/orchestrator/**", (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/tasks")
      return route.fulfill({ json: { data: [], total: 0, limit: 20, offset: 0 } });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
    return route.fulfill({ json: {} });
  });
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: PRICED }));
  await page.route("**/api/llm-provider-models", (r: Route) =>
    r.fulfill({ json: { models: [], providers: [] } }),
  );

  await page.goto("/orchestrator");
  await page.getByTestId("new-task-btn").click();
  await expect(page.getByRole("dialog", { name: "Create New Task" })).toBeVisible();
  await page.getByRole("button", { name: /advanced/i }).first().click();
  await page.locator("#taskModelInput").click();

  const options = page.locator(".model-picker-dropdown [role='option']");
  const tierOf = (slug: string) =>
    options.filter({ has: page.locator(`text=${slug}`) }).locator(".model-cost").first();
  await expect(tierOf("z-ai/glm-5.2")).toHaveAttribute("data-cost-tier", "1");
  await expect(tierOf("anthropic/claude-opus-4.8")).toHaveAttribute("data-cost-tier", "4");
});
