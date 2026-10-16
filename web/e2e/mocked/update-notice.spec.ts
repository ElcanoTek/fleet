import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The "was updated — Reload" notice is app-wide: a deploy is noticed on every
// signed-in page, and the notice is visible on a phone, where the chat
// sidebar that used to carry it is a closed drawer. Phone-sized viewport as in
// mobile.spec.ts (chromium with isMobile/hasTouch, not a webkit device
// descriptor, because the suite only installs chromium).
test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 3 });

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

// A build id no real bundle carries, so the probe always sees drift.
async function serveNewerBuild(page: Page) {
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { buildId: "e2e-newer-build" } }));
}

async function mockSettingsShell(page: Page) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r: Route) =>
    r.fulfill({ status: 501, json: { error: "not configured" } }),
  );
  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ status: 403, body: "forbidden" }));
}

test("a deploy is announced on a non-chat page, on a phone, and can be dismissed", async ({ page }) => {
  await mockSettingsShell(page);
  await serveNewerBuild(page);
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible({ timeout: 15_000 });

  const notice = page.getByTestId("update-notice");
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("was updated");
  await expect(notice.getByRole("button", { name: "Reload" })).toBeVisible();

  // It fits the phone's width.
  const box = await notice.boundingBox();
  expect(box).not.toBeNull();
  if (box) {
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(390 + 1);
  }

  await notice.getByRole("button", { name: "Dismiss update notice" }).click();
  await expect(notice).toHaveCount(0);
});

test("chat shows the same notice, outside the sidebar drawer", async ({ page }) => {
  await mockChatBoot(page);
  // Registered after mockChatBoot's /api/version stub, so it takes precedence.
  await serveNewerBuild(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await expect(page.getByTestId("update-notice")).toBeVisible();
  // The old footer button inside the (closed) drawer is gone.
  await expect(page.getByText("Chat has been updated — click to refresh")).toHaveCount(0);
});

test("no notice while the server runs the same build", async ({ page }) => {
  await mockSettingsShell(page);
  // The default mocked shape carries no buildId, so nothing differs.
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("update-notice")).toHaveCount(0);
});
