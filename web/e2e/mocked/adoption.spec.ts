import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the Operations Center Adoption tab (the exec per-user
// AI-adoption audit): an admin-role session sees the tab (and can deep-link it
// via ?tab=adoption), the panel renders KPI tiles with previous-period deltas,
// the two daily small multiples, the token-first leaderboard with engagement
// tiers, and the inactive-seat roster. A non-admin never sees the tab. Every
// /api/orchestrator/* call is intercepted by Playwright (no Go backend).

const DAYS = Array.from({ length: 14 }, (_, i) => `2026-06-${String(i + 1).padStart(2, "0")}`);

// Deterministic per-user daily series (index-aligned to DAYS).
function series(seed: number, activeEvery: number): number[] {
  return DAYS.map((_, i) => (i % activeEvery === 0 ? seed * (1 + (i % 5)) : 0));
}

const ALICE_DAILY = series(9000, 1);
const BOB_DAILY = series(5000, 3);
const CAROL_DAILY = series(800, 7);

const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);
const active = (xs: number[]) => xs.filter((v) => v > 0).length;

function userRow(user: string, daily: number[], overrides: Record<string, unknown>) {
  return {
    user,
    cost_usd: 0,
    task_cost_usd: 0,
    chat_cost_usd: 0,
    prompt_tokens: Math.round(sum(daily) * 0.85),
    completion_tokens: sum(daily) - Math.round(sum(daily) * 0.85),
    cached_tokens: 0,
    task_iterations: 0,
    chat_turns: 0,
    active_days: active(daily),
    last_active: DAYS.filter((_, i) => daily[i] > 0).at(-1),
    prev_cost_usd: 0,
    prev_tokens: 0,
    daily_tokens: daily,
    ...overrides,
  };
}

const ADOPTION_REPORT = {
  from: "2026-06-01T00:00:00Z",
  to: "2026-06-15T00:00:00Z",
  prev_from: "2026-05-18T00:00:00Z",
  days: DAYS,
  users: [
    userRow("alice@example.com", ALICE_DAILY, {
      cost_usd: 41.2,
      task_cost_usd: 28.4,
      chat_cost_usd: 12.8,
      task_iterations: 46,
      chat_turns: 118,
      cached_tokens: 61_000,
      prev_cost_usd: 30.1,
      prev_tokens: Math.round(sum(ALICE_DAILY) * 0.8),
    }),
    userRow("bob@example.com", BOB_DAILY, {
      cost_usd: 9.75,
      chat_cost_usd: 9.75,
      chat_turns: 42,
      prev_cost_usd: 12.4,
      prev_tokens: Math.round(sum(BOB_DAILY) * 1.4),
    }),
    userRow("carol@example.com", CAROL_DAILY, {
      cost_usd: 0.62,
      chat_cost_usd: 0.62,
      chat_turns: 5,
    }),
  ],
  inactive_users: [
    { email: "dan@example.com", created_at: "2026-05-20T00:00:00Z" },
    { email: "erin@example.com", created_at: "2026-02-11T00:00:00Z" },
  ],
  daily: DAYS.map((day, i) => ({
    day,
    cost_usd: 3.2 + (i % 4),
    tokens: ALICE_DAILY[i] + BOB_DAILY[i] + CAROL_DAILY[i],
    actions: 8 + (i % 6),
    active_users: [ALICE_DAILY[i], BOB_DAILY[i], CAROL_DAILY[i]].filter((v) => v > 0).length,
  })),
  totals: {
    active_users: 3,
    prev_active_users: 2,
    new_active_users: 1,
    registered_users: 5,
    cost_usd: 51.57,
    prev_cost_usd: 42.5,
    tokens: sum(ALICE_DAILY) + sum(BOB_DAILY) + sum(CAROL_DAILY),
    prev_tokens: Math.round((sum(ALICE_DAILY) + sum(BOB_DAILY)) * 0.9),
    cached_tokens: 61_000,
    chat_turns: 165,
    task_iterations: 46,
  },
  sources: ["tasks", "chat", "accounts"],
  note:
    "Dollar figures cover only runs with model pricing available; token totals are complete " +
    "regardless. Token volume measures how much someone uses the agents, not the quality of " +
    "what they produce with them — read this as an adoption signal, not a performance grade.",
};

const STATS = { pending_tasks: 0, running_tasks: 0, completed_tasks_today: 0, failed_tasks_today: 0 };

export async function mockOrchestratorAdmin(page: Page, opts: { role?: string } = {}) {
  await page.route("**/api/orchestrator/**", async (route: Route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace("/api/orchestrator", "");
    if (path === "/me") {
      return route.fulfill({
        json: { authenticated: true, username: "e2e", role: opts.role ?? "admin" },
      });
    }
    if (path === "/stats") return route.fulfill({ json: STATS });
    if (path === "/mcp-servers") return route.fulfill({ json: { servers: [] } });
    if (path === "/admin/usage/adoption") return route.fulfill({ json: ADOPTION_REPORT });
    if (path === "/tasks") return route.fulfill({ json: { tasks: [], total: 0 } });
    if (path === "/prompts") return route.fulfill({ json: [] });
    if (path === "/task-templates") return route.fulfill({ json: [] });
    return route.fulfill({ json: {} });
  });
}

test("admin deep-links ?tab=adoption and the exec audit renders end to end", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockOrchestratorAdmin(page);

  await page.goto("/orchestrator?tab=adoption");

  // The deep link lands on the Adoption tab directly.
  await expect(page.getByRole("tab", { name: "Adoption", selected: true })).toBeVisible();

  // KPI tiles: headcount with the seat denominator, and trend deltas.
  const totals = page.getByTestId("adoption-totals");
  await expect(totals).toContainText("of 5 seats · 60% adoption");
  await expect(totals).toContainText("▲ 50%"); // active users 3 vs 2

  // Two small multiples over the shared day axis (never a dual-axis chart).
  await expect(page.getByRole("img", { name: /Tokens per day, 14 days/ })).toBeVisible();
  await expect(page.getByRole("img", { name: /Active users per day, 14 days/ })).toBeVisible();

  // Leaderboard: token order + engagement tiers + the delta column.
  const rows = page.getByTestId("adoption-row");
  await expect(rows).toHaveCount(3);
  await expect(rows.nth(0)).toContainText("alice@example.com");
  await expect(rows.nth(0)).toContainText("power");
  await expect(rows.nth(1)).toContainText("bob@example.com");
  await expect(rows.nth(2)).toContainText("carol@example.com");
  // carol has no previous-window baseline → the "new" chip.
  await expect(rows.nth(2)).toContainText("new");

  // The inactive-seat roster names who hasn't adopted yet.
  const inactive = page.getByTestId("adoption-inactive");
  await expect(inactive).toContainText("Not yet active (2)");
  await expect(inactive).toContainText("dan@example.com");

  // The honest-scope note travels with the data, verbatim.
  await expect(page.getByTestId("adoption-note")).toContainText("adoption signal");
});

test("non-admins see neither the Adoption tab nor the panel via deep link", async ({
  page,
  context,
}) => {
  await loginViaCookie(context);
  await mockOrchestratorAdmin(page, { role: "client" });

  await page.goto("/orchestrator?tab=adoption");

  // The dashboard renders, but the admin tabs are absent and the deep link
  // falls back to the Recent Tasks view.
  await expect(page.getByRole("tab", { name: "Recent Tasks" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Adoption" })).toHaveCount(0);
  await expect(page.getByTestId("adoption-totals")).toHaveCount(0);
});
