import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
  test(`ordinary chat has no transcript fade at ${viewport.width}px`, async ({ page, context }) => {
    await page.setViewportSize(viewport);
    await loginViaCookie(context);
    await page.route("**/api/**", route => route.fulfill({ json: {} }));
    await mockChatBoot(page);
    await page.route("**/api/chat", route => fulfillSse(route, [
      { event: "conversation", data: { id: "clearance", title: "Weekly plan", persona: "default" } },
      { event: "text.delta", data: { text: Array.from({ length: 16 }, (_, i) => `Week ${i + 1}: review progress with the team.`).join("\n\n") + "\n\nFinal next step: confirm the owners." } },
      { event: "turn.completed", data: { cost_usd: 0, model: "demo-model" } },
    ]));
    await page.goto("/chat");
    const composer = page.getByRole("textbox").first();
    await composer.fill("Plan the weekly review.");
    await composer.press("Enter");
    const conversation = page.getByRole("region", { name: "Conversation" });
    const action = conversation.getByRole("button", { name: "Regenerate", exact: true });
    await expect(action).toBeVisible();
    await expect(page.locator('[data-testid="composer-fade"]')).toHaveCount(0);
    // The team viewer owns its CTA treatment. Ordinary chat must have no
    // gradient/mask overlay, and its final actions remain inside the scroller.
    await expect(page.locator('[class*="sticky-fade"]')).toHaveCount(0);
    await expect.poll(async () => {
      await conversation.evaluate(el => { el.scrollTop = el.scrollHeight; });
      const [actionBox, transcriptBox] = await Promise.all([action.boundingBox(), conversation.boundingBox()]);
      return actionBox && transcriptBox ? transcriptBox.y + transcriptBox.height - (actionBox.y + actionBox.height) : -1;
    }).toBeGreaterThanOrEqual(8);
  });
}
