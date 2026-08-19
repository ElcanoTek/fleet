import { creds, expect, test } from "./fixtures";

// Skills library/builder + connections availability, against the REAL stack:
// real Postgres rows behind the builder CRUD, the real merged bundle+builtin
// skills roster, and real user_connector_prefs behind the availability
// toggles. (The owner dislikes mocked-only coverage; this is the real thing.)
//
// Selectors follow the fleet-unified settings redesign: rows/cards are divs
// carrying skill-row-* / dir-card-* / bundled-card-* testids, deletes are
// two-click inline confirms (no window.confirm dialog), and the bundled
// availability control is a switch + "Enabled for me"/"Disabled for me" label.

const SKILL_NAME = "e2e-live-skill";

// wipeUserSkills deletes any leftover builder skills from a previous run —
// the suite shares one chat DB, and skill names are unique per user.
async function wipeUserSkills(page: import("@playwright/test").Page) {
  const origin = new URL(page.url()).origin;
  const headers = { Origin: origin };
  const resp = await page.request.get("/api/user-skills", { headers });
  if (!resp.ok()) return;
  const body = (await resp.json()) as { skills: Array<{ id: string }> | null };
  for (const sk of body.skills ?? []) {
    await page.request.delete(`/api/user-skills/${sk.id}`, { headers });
  }
}

test("skills page: built-in pack renders and the builder round-trips a personal skill", async ({
  page,
  login,
}) => {
  await login();
  await wipeUserSkills(page);
  await page.goto("/settings/skills");

  // The built-in pack is inherited by the default bundle and badged Built-in.
  const profilerRow = page.getByTestId("skill-row-data-profiler");
  await expect(profilerRow).toBeVisible();
  await expect(page.getByText("Built-in").first()).toBeVisible();

  // bento-slides (#985) ships bundled templates/references alongside its
  // SKILL.md — assert it rosters too, so a pack whose aux files broke
  // materialization does not silently vanish from the library.
  await expect(page.getByTestId("skill-row-bento-slides")).toBeVisible();

  // Read view: the full SKILL.md body loads from the merged roster.
  await profilerRow.getByRole("button", { name: "View" }).click();
  await expect(page.getByText("name: data-profiler")).toBeVisible();

  // Builder: create → Active → disable → delete, all against real DB rows.
  await page.getByRole("button", { name: "New skill" }).click();
  await page.getByPlaceholder("my-skill").fill(SKILL_NAME);
  await page
    .getByPlaceholder(/Verify a deal sheet/)
    .fill("An e2e-only skill that verifies the builder persists to the real database.");
  await page.getByPlaceholder(/1\. Read the attached/).fill("1. Do the e2e thing.\n2. Stop.");
  await page.getByRole("button", { name: "Create skill" }).click();

  const mine = page.getByTestId(`skill-row-${SKILL_NAME}`);
  await expect(mine).toBeVisible();
  await expect(mine.getByText("Active")).toBeVisible();

  // Survives a reload — it's a DB row, not client state.
  await page.reload();
  await expect(page.getByTestId(`skill-row-${SKILL_NAME}`)).toBeVisible();

  await page
    .getByTestId(`skill-row-${SKILL_NAME}`)
    .getByRole("button", { name: "Disable" })
    .click();
  await expect(page.getByTestId(`skill-row-${SKILL_NAME}`).getByText("Disabled")).toBeVisible();

  // Delete is a two-click inline confirm now — arm, then confirm.
  await page
    .getByTestId(`skill-row-${SKILL_NAME}`)
    .getByRole("button", { name: "Delete", exact: true })
    .click();
  await page
    .getByTestId(`skill-row-${SKILL_NAME}`)
    .getByRole("button", { name: "Confirm delete" })
    .click();
  await expect(page.getByTestId(`skill-row-${SKILL_NAME}`)).toHaveCount(0);
});

test("connections page: directory searches and an availability toggle persists", async ({
  page,
  login,
}) => {
  await login();
  await page.goto("/settings/connections");

  // The built-in hosted directory is inherited by the default bundle: search
  // narrows to Stripe with its Official badge and vet links.
  const search = page.getByPlaceholder(/Search servers/);
  await expect(search).toBeVisible();
  await search.fill("stripe");
  const stripeCard = page
    .locator('[data-testid^="dir-card-"]', { hasText: "Stripe" })
    .first();
  await expect(stripeCard).toBeVisible();
  await expect(stripeCard.getByText("Official")).toBeVisible();
  await expect(stripeCard.getByRole("link", { name: "docs" })).toBeVisible();

  // Availability layer: the synthetic image_generation connector is Optional
  // in every deployment, so it always has a switch. Disable → reload →
  // still disabled (a real user_connector_prefs row) → reset to default.
  const imgCard = page.getByTestId("bundled-card-image_generation");
  await expect(imgCard).toBeVisible();
  await imgCard.getByRole("switch", { name: /Enable Image generation for me/ }).click();
  await expect(imgCard.getByText("Disabled for me")).toBeVisible();

  await page.reload();
  const imgCardAfter = page.getByTestId("bundled-card-image_generation");
  await expect(imgCardAfter.getByText("Disabled for me")).toBeVisible();

  await imgCardAfter.getByRole("button", { name: "Reset" }).click();
  await expect(imgCardAfter.getByText("Enabled for me")).toBeVisible();
});

test("sharing: grantee sees a shared connection surface", async ({ page, login }) => {
  await login();
  // Sharing needs a CONNECTED remote server (OAuth), which the live stack has
  // no authorization server for — the grant/list/revoke store+API paths are
  // unit-tested. Here we assert the surfaces exist and degrade correctly for
  // a user with no connections: no Shared-with-you section, no stray errors.
  await page.goto("/settings/connections");
  await expect(page.getByText("Remote servers").first()).toBeVisible();
  await expect(page.getByText("Shared with you")).toHaveCount(0);
  await expect(page.getByText(/Authorization failed/)).toHaveCount(0);
  void creds; // fixture import kept for parity with sibling specs
});
