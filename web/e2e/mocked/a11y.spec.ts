import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Automated accessibility checks (the Elcano frontend standard asks for
// Playwright + axe wherever a browser suite exists). axe runs the WCAG 2.x A
// and AA rule sets against the rendered page — the real SPA served by the Go
// gateway — in both colour schemes, because contrast is a per-theme property.
//
// What this does and does not prove: axe catches the mechanical failures
// (missing names and labels, broken ARIA, contrast, landmark and heading
// structure). It cannot judge reading order, focus management or whether a
// label is meaningful; the keyboard and focus specs beside this one cover
// those paths by hand.

async function expectNoViolations(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  const summary = results.violations.map(
    (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
  );
  expect(summary, summary.join("\n")).toEqual([]);
}

for (const scheme of ["light", "dark"] as const) {
  test.describe(`${scheme} theme`, () => {
    test.use({ colorScheme: scheme });

    test("/login has no axe violations", async ({ page }) => {
      await page.goto("/login");
      await expect(page.getByRole("button", { name: /sign in/i })).toBeVisible();
      await expectNoViolations(page);
    });

    test("/chat (empty state) has no axe violations", async ({ page, context }) => {
      await loginViaCookie(context);
      await mockChatBoot(page);
      await page.goto("/chat");
      await expect(page.getByTestId("nav-to-help")).toBeVisible({ timeout: 15_000 });
      await expectNoViolations(page);
    });

    test("/help has no axe violations", async ({ page, context }) => {
      await loginViaCookie(context);
      await mockChatBoot(page);
      await page.goto("/help");
      await expect(page.getByTestId("guide-card-chat")).toBeVisible({ timeout: 15_000 });
      await expectNoViolations(page);
    });
  });
}
