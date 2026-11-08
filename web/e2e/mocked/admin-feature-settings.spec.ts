import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the admin Features page (settings redesign): /settings/admin/
// features renders every workspace feature setting from GET /api/admin/settings,
// a toggle PUTs the override and re-renders with the Overridden badge + Reset,
// and Reset DELETEs back to the default. The Notifications page renders from
// its own endpoint at /settings/admin/notifications.

type Resolved = {
  key: string;
  kind: "bool" | "int" | "enum";
  enum?: string[];
  env_var: string;
  value: string;
  source: "admin" | "default";
  default: string;
  updated_by?: string;
};

const SETTINGS: Resolved[] = [
  {
    key: "pii_redaction_mode",
    kind: "enum",
    enum: ["off", "observe", "redact", "block"],
    env_var: "FLEET_PII_REDACTION_ENABLED / FLEET_PII_REDACTION_MODE",
    value: "off",
    source: "default",
    default: "off",
  },
  {
    key: "subagents_enabled",
    kind: "bool",
    env_var: "FLEET_SUBAGENTS_ENABLED",
    value: "false",
    source: "default",
    default: "false",
  },
];

async function mockAdmin(page: Page) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "admin@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/admin/stats", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/users", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/llm-providers", (r: Route) =>
    r.fulfill({ json: { providers: [] } }),
  );
  await page.route("**/api/admin/notify-settings", (r: Route) =>
    r.fulfill({
      json: {
        source: "env",
        settings: {
          notify_on: "",
          smtp_host: "",
          smtp_port: "587",
          smtp_username: "",
          has_smtp_password: false,
          smtp_from: "",
          email_to: "",
          webhook_url: "https://hooks.example.com/x",
          webhook_method: "POST",
          webhook_body_template: "",
          has_webhook_secret: true,
        },
        email_enabled: false,
        webhook_enabled: true,
      },
    }),
  );

  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ json: { settings: SETTINGS } }));
  await page.route("**/api/admin/settings/subagents_enabled", (r: Route) => {
    if (r.request().method() === "PUT") {
      return r.fulfill({
        json: {
          ...SETTINGS[1],
          value: "true",
          source: "admin",
          updated_by: "admin@example.com",
        },
      });
    }
    if (r.request().method() === "DELETE") {
      return r.fulfill({ json: SETTINGS[1] });
    }
    return r.fallback();
  });
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the Features page renders, toggles live, and resets to default", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/features");

  const panel = page.getByTestId("feature-settings-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("PII redaction");
  await expect(panel).toContainText(/Privacy & data protection/i);
  await expect(panel).toContainText("FLEET_SUBAGENTS_ENABLED");

  // Toggle sub-agents on: PUT round-trip flips the switch, and the row's
  // badge flips to the design's Overridden state with a Reset affordance.
  const toggle = page.getByTestId("toggle-subagents_enabled");
  await expect(toggle).toHaveAttribute("aria-checked", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-checked", "true");
  await expect(panel).toContainText("Overridden");
  await expect(panel).toContainText("set by admin@example.com");

  // Reset: DELETE reverts to the env default.
  await page.getByTestId("reset-subagents_enabled").click();
  await expect(toggle).toHaveAttribute("aria-checked", "false");
  await expect(panel).not.toContainText("Overridden");
});

test("the Features filter narrows rows and hides empty groups", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/features");

  const panel = page.getByTestId("feature-settings-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });

  await page.getByRole("textbox", { name: "Filter settings" }).fill("sub-agent");
  await expect(panel).toContainText("Sub-agent delegation");
  await expect(panel).not.toContainText("PII redaction");

  await page.getByRole("textbox", { name: "Filter settings" }).fill("zzz-no-match");
  await expect(page.getByText(/No settings match/)).toBeVisible();
});

test("the Notifications page renders channel status without secret material", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/settings/admin/notifications");

  const notifications = page.getByTestId("notifications-panel");
  await expect(notifications).toBeVisible({ timeout: 15_000 });
  await expect(notifications).toContainText("Env config");
  await expect(page.getByTestId("notify-webhook-url")).toHaveValue("https://hooks.example.com/x");
  await expect(page.getByTestId("notify-webhook-secret")).toHaveValue("");
});
