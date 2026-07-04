import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the admin Features panel: /admin renders every workspace
// feature setting from GET /api/admin/settings, a toggle PUTs the override and
// re-renders with admin provenance, and Reset DELETEs back to the default.

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
  await page.route("**/api/admin/stats", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/users", (r: Route) => r.fulfill({ json: { users: [] } }));
  await page.route("**/api/admin/llm-providers", (r: Route) =>
    r.fulfill({ json: { providers: [] } }),
  );
  await page.route("**/api/admin/health-summary", (r: Route) =>
    r.fulfill({
      json: {
        fleet_version: "test",
        uptime_seconds: 60,
        db: { chat: "healthy", pool_size: 5, in_use: 1, idle: 4 },
        workers: {
          total: 1,
          active: 0,
          idle: 1,
          queued_tasks: 0,
          running_tasks: 0,
          completed_today: 0,
          failed_today: 0,
        },
        llm: { calls_today: 0, cost_today_usd: 0, avg_cost_per_call: 0 },
        mcp_servers: [],
        conversations_active: 0,
        sandbox_pool: { size: 0, available: 0 },
        memory_mb: 1,
        goroutines: 1,
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

test("the features panel renders, toggles live, and resets to default", async ({ page }) => {
  await mockAdmin(page);
  await page.goto("/admin");

  const panel = page.getByTestId("feature-settings-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("PII redaction");
  await expect(panel).toContainText("Privacy & data protection");
  await expect(panel).toContainText("FLEET_SUBAGENTS_ENABLED");

  // Toggle sub-agents on: PUT round-trip flips the switch + provenance chip.
  const toggle = page.getByTestId("toggle-subagents_enabled");
  await expect(toggle).toHaveAttribute("aria-checked", "false");
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-checked", "true");
  await expect(panel).toContainText("Customized");
  await expect(panel).toContainText("set by admin@example.com");

  // Reset: DELETE reverts to the env default.
  await page.getByTestId("reset-subagents_enabled").click();
  await expect(toggle).toHaveAttribute("aria-checked", "false");
  await expect(panel).not.toContainText("Customized");
});
