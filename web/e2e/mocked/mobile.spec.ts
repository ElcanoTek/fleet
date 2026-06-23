import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mobile-layout smoke under a phone-sized viewport. Pure presentation/responsive
// behaviour (no backend), so it runs in the mocked lane: the composer must fit
// the narrow viewport with no horizontal overflow, and the sidebar must behave
// as a hamburger-toggled drawer rather than a fixed rail.
//
// We set the mobile viewport + touch explicitly rather than spreading a device
// descriptor like devices["iPhone 13"], because those pin defaultBrowserType to
// webkit — and the suite (and CI) only installs the chromium browser. isMobile +
// hasTouch are chromium-supported, so this keeps the mocked "chromium" project.
test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 });

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the composer fits the mobile viewport with no horizontal overflow", async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const composer = page.getByRole("textbox").first();
  await expect(composer).toBeVisible();

  const box = await composer.boundingBox();
  const vp = page.viewportSize();
  expect(box).not.toBeNull();
  expect(vp).not.toBeNull();
  if (box && vp) {
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(vp.width + 1);
  }

  // The page itself must not scroll horizontally at the mobile width.
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
  );
  expect(overflow).toBeLessThanOrEqual(1);
});

test("the sidebar is a hamburger-toggled drawer on mobile", async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  // Off-canvas to start (translated fully off-screen), revealed by the hamburger,
  // and dismissable via the in-drawer close button.
  const sidebar = page.locator("aside").first();
  await expect(sidebar).toHaveClass(/-translate-x-full/);

  await page.getByRole("button", { name: /open sidebar/i }).click();
  await expect(sidebar).toHaveClass(/translate-x-0/);

  await sidebar.getByRole("button", { name: /close sidebar/i }).first().click();
  await expect(sidebar).toHaveClass(/-translate-x-full/);
});
