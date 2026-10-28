import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";

// Mocked e2e for Settings → Personas (docs/USER-PERSONAS.md): the page is in
// the settings nav, a proposed draft is approved in one click, and the page
// passes axe in both colour schemes (the frontend standard's a11y bar).

const ACTIVE = {
  id: "p1",
  name: "my-twin",
  ref: "user:my-twin",
  display_name: "My Twin",
  description: "Thinks and writes the way I do.",
  instructions: "Lead with the number.",
  base_persona: "",
  kind: "twin",
  autonomy: "advise",
  status: "active",
  patrols: [{ title: "Morning briefing", cron: "0 8 * * 1-5", timezone: "UTC", prompt: "Brief me." }],
};
const DRAFT = {
  ...ACTIVE,
  id: "p2",
  name: "deal-watch",
  ref: "user:deal-watch",
  display_name: "Deal Watch",
  kind: "assistant",
  status: "proposed",
  patrols: [],
};

async function mockShell(page: Page, approvals: string[]) {
  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r: Route) =>
    r.fulfill({ status: 501, json: { error: "not configured" } }),
  );
  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ status: 403, body: "forbidden — not an admin" }));
  await page.route("**/api/user-personas", (r: Route) =>
    r.fulfill({
      json: {
        personas: approvals.length > 0 ? [ACTIVE, { ...DRAFT, status: "active" }] : [ACTIVE, DRAFT],
        workspace_personas: ["assistant"],
        default: "assistant",
      },
    }),
  );
  await page.route("**/api/user-personas/*/approve", async (r: Route) => {
    approvals.push(r.request().url());
    await r.fulfill({ json: { ...DRAFT, status: "active" } });
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("a proposed persona is approved from Settings → Personas", async ({ page }) => {
  const approvals: string[] = [];
  await mockShell(page, approvals);
  await page.goto("/settings/personas");

  const nav = page.getByRole("navigation", { name: "Settings sections" });
  await expect(nav.getByRole("link", { name: "Personas" })).toHaveAttribute("aria-current", "page");
  const draft = page.getByTestId("persona-row-deal-watch");
  await expect(draft.getByText("Proposed — review it")).toBeVisible({ timeout: 15_000 });
  await expect(draft.getByRole("button", { name: "Start patrol" })).toHaveCount(0);

  await draft.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("“Deal Watch” is ready to use.")).toBeVisible();
  expect(approvals).toHaveLength(1);
  expect(approvals[0]).toContain("/api/user-personas/p2/approve");
  await expect(page.getByTestId("persona-row-my-twin").getByRole("button", { name: "Start patrol" })).toBeVisible();
});

for (const scheme of ["light", "dark"] as const) {
  test.describe(`${scheme} theme`, () => {
    test.use({ colorScheme: scheme });

    test("/settings/personas (with the editor open) has no axe violations", async ({ page }) => {
      await mockShell(page, []);
      await page.goto("/settings/personas");
      await expect(page.getByTestId("persona-row-my-twin")).toBeVisible({ timeout: 15_000 });
      await page.getByTestId("new-persona").click();
      await page.getByRole("button", { name: "Digital twin", exact: true }).click();
      await expect(page.getByTestId("persona-editor")).toBeVisible();
      const results = await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
        .analyze();
      const summary = results.violations.map(
        (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
      );
      expect(summary, summary.join("\n")).toEqual([]);
    });
  });
}
