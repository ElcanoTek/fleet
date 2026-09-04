import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for Settings → Team (#1157, copy corrected by ADR-0057): a member
// with no team can create one from the UI (the fix for "team is not editable,
// so projects are not usable or sharable"); a name that belongs to someone
// else's trust group comes back as a conflict rather than a silent success —
// joining stays an admin grant (ADR-0047); and leaving, which now unshares the
// chats you shared with the team, confirms before it acts.

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
  // The confirmation names the one path that actually adds teammates — the old
  // copy told them to "join the same name", which the server refuses.
  await expect(
    page.getByText(/Teammates get added by an admin in Settings → Admin → Users/),
  ).toBeVisible();
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

  // The server cannot say whether a user or a team-shared PROJECT holds the
  // name (ADR-0047), so the copy does not claim "that team exists" — it says
  // the name is in use and names the fix.
  await expect(
    page.getByText(/That name is already in use\. An admin can add you/i),
  ).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("team-current")).toHaveCount(0);
});

test("leaving confirms first, stating what it costs", async ({ page }) => {
  await mockShell(page);

  const writes: string[] = [];
  await page.route("**/api/me/team", async (r: Route) => {
    if (r.request().method() === "PUT") {
      writes.push(r.request().postData() ?? "");
      await r.fulfill({
        json: { email: "e2e@example.com", role: "member", team_id: "", admin: false },
      });
      return;
    }
    await r.fulfill({
      json: {
        email: "e2e@example.com",
        role: "member",
        team_id: "platform",
        admin: false,
        shared_projects: 3,
        shared_chats: 2,
      },
    });
  });

  await page.goto("/settings/team");
  await expect(page.getByTestId("team-current")).toHaveText("platform", { timeout: 15_000 });

  await page.getByRole("button", { name: "Leave team" }).click();

  // Nothing is written until the consequences have been shown and accepted —
  // leaving unshares the chats this user shared with the team (ADR-0057).
  const dialog = page.getByRole("dialog", { name: "Leave platform?" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("3 team-shared projects");
  await expect(dialog).toContainText("2 chats you shared with the team");
  await expect(dialog).toContainText("Projects you own stay yours");
  expect(writes).toEqual([]);

  await dialog.getByRole("button", { name: "Leave team" }).click();
  await expect(page.getByText(/You left your team/)).toBeVisible({ timeout: 15_000 });
  expect(writes.map((w) => JSON.parse(w))).toEqual([{ team_id: "" }]);
});
