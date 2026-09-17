import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

test("native-only chat repairs an unavailable default and retries with the newly selected model", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockChatBoot(page);
  await page.route("**/api/llm-provider-models", (route) => route.fulfill({ json: {
    routing_known: true,
    providers: [{ name: "bundle-openai", type: "openai", models: ["gpt-4o", "gpt-4o-mini"], catch_all: false }],
    models: ["gpt-4o", "gpt-4o-mini"].map((model) => ({ id: `bundle-openai/${model}`, name: `Workspace ${model}` })),
  } }));
  const publicModels = { models: [
    { slug: "google/gemini-3.8-flash", name: "Unavailable Gemini" },
    { slug: "openai/gpt-4o", name: "Unavailable OpenRouter GPT" },
  ] };
  await page.route("**/api/model-catalog", (route) => route.fulfill({ json: publicModels }));
  await page.route("**/api/model-rankings", (route) => route.fulfill({ json: publicModels }));
  await page.route("**/api/model-check**", (route) => route.fulfill({ json: { allowed: true } }));
  await page.route("**/api/conversations/*/truncate?*", (route) => route.fulfill({ json: {} }));
  const sent: string[] = [];
  await page.route("**/api/chat", (route) => {
    sent.push(route.request().postDataJSON().model);
    return fulfillSse(route, [
      { event: "conversation", data: { id: "native-chat", title: "Native chat", model: sent.at(-1), persona: "default" } },
      ...(sent.length === 1
        ? [{ event: "turn.error", data: { message: "Test provider temporarily unavailable" } }]
        : [{ event: "text.delta", data: { text: "Retried through the selected workspace provider." } },
           { event: "turn.completed", data: { model: sent.at(-1) } }]),
    ]);
  });

  await page.goto("/chat");
  await expect(page.getByRole("alert").filter({ hasText: "not available through this workspace" })).toBeVisible();
  await page.getByRole("button", { name: "Choose a model", exact: true }).click();
  const list = page.locator("#composer-model-listbox");
  await expect(list.getByRole("option")).toHaveCount(2);
  await expect(list).not.toContainText("Unavailable");
  await list.getByRole("option", { name: "Workspace gpt-4o workspace", exact: true }).click();
  const composer = page.getByRole("textbox").first();
  await composer.fill("Create a test deal");
  await composer.press("Enter");
  await expect(page.getByText("Test provider temporarily unavailable")).toBeVisible();
  await page.locator("button[aria-haspopup='listbox']").first().click();
  await list.getByRole("option", { name: /Workspace gpt-4o-mini/ }).click();
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(page.getByText("Retried through the selected workspace provider.")).toBeVisible();
  expect(sent).toEqual(["bundle-openai/gpt-4o", "bundle-openai/gpt-4o-mini"]);
});
