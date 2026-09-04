import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, fulfillSse } from "./_mocks";

for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
  test(`finished reply clears the composer fade at ${viewport.width}px`, async ({ page, context }) => {
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
    // Even at the absolute bottom, the old 16/24px padding leaves the last
    // response controls behind the composer's 64px gradient. Visibility alone
    // cannot detect that translucent overlay; compare their actual geometry.
    await expect.poll(async () => {
      await conversation.evaluate(el => { el.scrollTop = el.scrollHeight; });
      const fade = page.locator('[data-testid="composer-fade"]');
      const [actionBox, fadeBox] = await Promise.all([action.boundingBox(), fade.boundingBox()]);
      return actionBox && fadeBox ? fadeBox.y - (actionBox.y + actionBox.height) : -1;
    }).toBeGreaterThanOrEqual(8);
  });
}
