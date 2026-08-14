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

// The composer toolbar used to wrap on a phone: the model chip carried the full
// vendor-prefixed name plus the four cost glyphs, so the trailing controls (the
// context ring) dropped onto a second line. The chip is now the only elastic
// item — it truncates — and everything else stays on one row.
test("the composer toolbar keeps every control on one row", async ({ page }) => {
  await mockChatBoot(page);
  // Priced so the chip's cost indicator exists in the DOM — the point of the
  // assertion below is that it is hidden at this width, not merely absent.
  await page.route("**/api/model-catalog", (r) =>
    r.fulfill({
      json: {
        models: [
          {
            slug: "deepseek/deepseek-v4-flash-0731",
            name: "DeepSeek: DeepSeek V4 Flash 0731",
            price_prompt: 0.0000004,
            price_completion: 0.0000016,
            context_length: 200000,
          },
        ],
      },
    }),
  );
  await page.goto("/chat");
  await expect(page.getByRole("textbox").first()).toBeVisible({ timeout: 20_000 });

  const chip = page.locator("button[aria-haspopup='listbox']").first();
  await expect(chip).toBeVisible();

  // Vendor prefix dropped on mobile so the model still reads in the space left
  // after the icon buttons; the full label stays available to assistive tech.
  await expect(chip.getByTestId("composer-model-label-short")).toHaveText("DeepSeek V4 Flash 0731");
  await expect(chip.getByTestId("composer-model-label-full")).toBeHidden();
  await expect(chip).toHaveAccessibleName(/DeepSeek: DeepSeek V4 Flash 0731/);
  // Cost glyphs are picker-only at this width.
  await expect(chip.locator(".model-cost")).toBeHidden();

  // Every toolbar control shares one row: same vertical centre as the chip, and
  // nothing spills past the composer's right edge.
  const toolbar = chip.locator("xpath=ancestor::div[contains(@class,'justify-between')][1]");
  const rows = await toolbar.evaluate((el) => {
    const parent = el.getBoundingClientRect();
    const controls = Array.from(el.querySelectorAll("button")).filter((b) => {
      const r = b.getBoundingClientRect();
      // Skip anything collapsed or rendered inside an open popover.
      return r.width > 0 && r.height > 0 && r.top >= parent.top - 1 && r.bottom <= parent.bottom + 1;
    });
    return {
      centres: controls.map((b) => Math.round(b.getBoundingClientRect().top + b.getBoundingClientRect().height / 2)),
      overflowRight: Math.max(
        ...controls.map((b) => b.getBoundingClientRect().right - parent.right),
      ),
    };
  });
  expect(rows.centres.length).toBeGreaterThan(3);
  expect(Math.max(...rows.centres) - Math.min(...rows.centres)).toBeLessThanOrEqual(2);
  expect(rows.overflowRight).toBeLessThanOrEqual(1);
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
