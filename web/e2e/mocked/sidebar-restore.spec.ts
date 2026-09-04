import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

test("finishing a delayed restore keeps an opened mobile drawer; user navigation closes it", async ({ page, context }) => {
  await page.setViewportSize({ width: 390, height: 900 });
  await loginViaCookie(context);
  await page.route("**/api/**", route => route.fulfill({ json: {} }));
  const first = { id: "restore-first", title: "Restored chat", persona: "default", model: "test-model" };
  const second = { ...first, id: "restore-second", title: "Another chat" };
  await mockChatBoot(page, { conversations: [first, second] });
  let release!: () => void;
  const held = new Promise<void>(resolve => { release = resolve; });
  let requested = false;
  await page.route("**/api/conversations/restore-first", async route => {
    requested = true;
    await held;
    await route.fulfill({ json: {
      conversation: first,
      history: [{ id: 1, role: "assistant", type: "text", content: { text: "The restored response." } }],
    } });
  });
  await page.goto("/chat");
  await expect.poll(() => requested).toBe(true);
  await page.getByRole("button", { name: "Open sidebar", exact: true }).click();
  const sidebar = page.getByRole("complementary", { name: "Primary navigation" });
  await expect.poll(async () => (await sidebar.boundingBox())?.x).toBe(0);
  release();
  await expect(page.getByText("The restored response.", { exact: true })).toBeAttached();
  // Assert state as well as geometry so the check cannot pass in the first
  // animation frame of an incorrectly closing drawer.
  await expect(sidebar).toHaveClass(/(?:^|\s)translate-x-0(?:\s|$)/);
  await sidebar.getByRole("button", { name: "Another chat", exact: true }).click();
  await expect(sidebar).toHaveClass(/(?:^|\s)-translate-x-full(?:\s|$)/);
});
