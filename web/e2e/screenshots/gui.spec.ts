import { test } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "../mocked/_session";
import { mockChatBoot, fulfillSse } from "../mocked/_mocks";

// GUI screenshot capture for the README / docs (#487). This is NOT a PR-gating
// test — it lives in its own `screenshots` Playwright project (E2E_SCREENSHOTS=1,
// see playwright.config.ts) so a capture hiccup never blocks the mocked PR lane.
// It drives the SAME deterministic mocked Next app the mocked suite uses (every
// /api/* call is route-intercepted — no backend, no LLM), navigates the key
// views, and writes a full-page PNG per view. The screenshot workflow runs this
// on push to main and commits the PNGs to docs/screenshots/web/.
//
// screenshot({path}) resolves relative to the Playwright process CWD (web/, the
// config dir), so ../docs/screenshots/web is the repo-root docs dir.
const OUT = "../docs/screenshots/web";

// settle waits for fonts + a short paint delay so a capture isn't mid-animation.
async function settle(page: Page) {
  await page.evaluate(() => document.fonts?.ready);
  await page.waitForTimeout(400);
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("chat view", async ({ page }) => {
  await mockChatBoot(page);
  // Seed a completed streamed turn so the shot shows a real conversation, not
  // just the empty composer. Mirrors the mocked chat spec's event vocabulary.
  await page.route("**/api/chat", (r: Route) =>
    fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-1", title: "Weekly revenue report", persona: "default" } },
      { event: "text.delta", id: 2, data: { text: "I'll pull the latest numbers and summarize them. " } },
      { event: "tool.call", id: 3, data: { id: "call-1", name: "run_python", input: JSON.stringify({ code: "df.groupby('region').revenue.sum()" }) } },
      { event: "tool.result", id: 4, data: { id: "call-1", name: "run_python", text: "NORAM  1.24M\nEMEA   0.88M\nAPAC   0.51M", is_err: false } },
      { event: "text.delta", id: 5, data: { text: "Revenue is up 12% WoW, led by NORAM. Full breakdown above." } },
      { event: "turn.completed", id: 6, data: { cost_usd: 0.014, model: "anthropic/claude-opus-4.8" } },
    ]),
  );
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  const composer = page.getByRole("textbox").first();
  await composer.fill("Summarize this week's revenue by region.");
  await composer.press("Enter");
  // Wait for the streamed assistant reply to land before capturing.
  await page.getByText(/Revenue is up 12% WoW/i).waitFor({ timeout: 15_000 });
  await settle(page);
  await page.screenshot({ path: `${OUT}/chat.png`, fullPage: true });
});

test("orchestrator dashboard", async ({ page }) => {
  const STATS = { pending_tasks: 2, running_tasks: 1, completed_tasks_today: 14, failed_tasks_today: 0 };
  const MCP_SERVERS = {
    servers: [
      { name: "github", description: "GitHub issues + PRs", tool_count: 9, accounts: ["acme"] },
      { name: "postgres", description: "Analytics warehouse", tool_count: 5, accounts: [] },
    ],
  };
  // The dashboard renders each task's PROMPT (not its name), so give them
  // presentable prompt text.
  const TASKS = [
    { id: "11111111-1111-1111-1111-111111111111", prompt: "Summarize this week's revenue by region", status: "success", created_at: new Date().toISOString(), agent_session_id: "sess-1" },
    { id: "22222222-2222-2222-2222-222222222222", prompt: "Sync the analytics warehouse from the source systems", status: "running", created_at: new Date().toISOString(), agent_session_id: "sess-2" },
    { id: "33333333-3333-3333-3333-333333333333", prompt: "Flag any daily spend anomalies over 20%", status: "scheduled", created_at: new Date().toISOString() },
  ];
  await page.route("**/api/orchestrator/**", (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats") return route.fulfill({ json: STATS });
    if (path === "/mcp-servers") return route.fulfill({ json: MCP_SERVERS });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
    if (path === "/concurrency") return route.fulfill({ json: { max_concurrent_agents: 4, warm_pool_size: 2 } });
    if (path === "/tasks") return route.fulfill({ json: { data: TASKS, total: TASKS.length, limit: 20, offset: 0 } });
    return route.fulfill({ json: {} });
  });
  await page.goto("/orchestrator");
  // Wait for the dashboard shell, then the seeded task list, before capturing.
  await page.getByTestId("orchestrator-dashboard").waitFor({ state: "visible", timeout: 15_000 });
  await page.getByText(/Summarize this week's revenue by region/i).first().waitFor({ timeout: 15_000 });
  await settle(page);
  await page.screenshot({ path: `${OUT}/orchestrator.png`, fullPage: true });
});
