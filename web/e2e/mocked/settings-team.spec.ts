import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for Settings → Team (#1157): a member with no team can create one
// from the UI (the fix for "team is not editable, so projects are not usable or
// sharable"), and a name that belongs to someone else's trust group comes back
// as the upstream 409 rather than a silent success — joining stays an admin
// grant (ADR-0047).

async function mockShell(page: Page) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r: Route) =>
    r.fulfill({ status: 501, json: { error: "not configured" } }),
  );
  // Not an admin: the nav's Admin parent stays hidden, and the page shows the
  // "ask an admin" wording.
  await page.route("**/api/admin/settings", (r: Route) =>
    r.fulfill({ status: 403, body: "forbidden — not an admin" }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("a member creates their own team from Settings → Team", async ({ page }) => {
  await mockShell(page);

  let team = "";
  const writes: string[] = [];
  await page.route("**/api/me/team", async (r: Route) => {
    if (r.request().method() === "PUT") {
      writes.push(r.request().postData() ?? "");
      team = "platform";
    }
    await r.fulfill({
      json: { email: "e2e@example.com", role: "member", team_id: team, admin: false },
    });
  });

  await page.goto("/settings/team");

  // The nav exposes the new section, and the empty state explains the model.
  const nav = page.getByRole("navigation", { name: "Settings sections" });
  await expect(nav.getByRole("link", { name: "Team" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByText(/not in a team yet/i)).toBeVisible({ timeout: 15_000 });

  await page.getByLabel("Team name").fill("platform");
  await page.getByRole("button", { name: "Create team" }).click();

  await expect(page.getByTestId("team-current")).toHaveText("platform", { timeout: 15_000 });
  await expect(page.getByText(/You are now in team/)).toBeVisible();
  expect(writes.map((w) => JSON.parse(w))).toEqual([{ team_id: "platform" }]);
});

test("claiming another team's name surfaces the upstream conflict", async ({ page }) => {
  await mockShell(page);

  await page.route("**/api/me/team", async (r: Route) => {
    if (r.request().method() === "PUT") {
      await r.fulfill({
        status: 409,
        body: "that team already exists — ask an admin to add you to it",
      });
      return;
    }
    await r.fulfill({
      json: { email: "e2e@example.com", role: "member", team_id: "", admin: false },
    });
  });

  await page.goto("/settings/team");
  await expect(page.getByText(/not in a team yet/i)).toBeVisible({ timeout: 15_000 });

  await page.getByLabel("Team name").fill("leadership");
  await page.getByRole("button", { name: "Create team" }).click();

  await expect(page.getByText(/ask an admin to add you to it/i)).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("team-current")).toHaveCount(0);
});
