import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "../mocked/_session";
import { mockChatBoot, fulfillSse } from "../mocked/_mocks";

// GUI screenshot capture for the README / docs (#487). This is NOT a PR-gating
// test — it lives in its own `screenshots` Playwright project (E2E_SCREENSHOTS=1,
// see playwright.config.ts) so a capture hiccup never blocks the mocked PR lane.
// It drives the SAME deterministic mocked browser tier the mocked suite uses (every
// /api/* call is route-intercepted — no backend, no LLM), navigates the key
// views, and writes a PNG per view. The screenshot workflow runs this on push to
// main and commits the PNGs to docs/screenshots/web/.
//
// screenshot({path}) resolves relative to the Playwright process CWD (web/, the
// config dir), so ../docs/screenshots/web is the repo-root docs dir.
const OUT = "../docs/screenshots/web";

test.use({ colorScheme: "dark", timezoneId: "America/New_York" });

const SEEDED_CONVERSATIONS = [
  {
    id: "conv-kickoff",
    title: "Six-week customer kickoff plan",
    model: "anthropic/claude-opus-5.5",
    pinned: true,
    folder: "Customer Onboarding",
  },
  {
    id: "conv-patrol-deals",
    title: "Deals & spend patrol",
    model: "anthropic/claude-opus-5.5",
    pinned: true,
    folder: "Revenue Ops",
  },
  {
    id: "conv-inbox-patrol",
    title: "Morning inbox triage",
    model: "anthropic/claude-opus-5.5",
    pinned: false,
    folder: "Revenue Ops",
  },
  {
    id: "conv-emea",
    title: "EMEA Q4 pipeline review",
    model: "anthropic/claude-opus-5.5",
    pinned: false,
    folder: null,
  },
];

const SEEDED_PATROLS = [
  {
    id: "sched-deals",
    conversation_id: "conv-patrol-deals",
    title: "Deals & spend patrol",
    conversation_title: "Deals & spend patrol",
    cron: "0 9 * * 1-5",
    status: "active",
  },
  {
    id: "sched-inbox",
    conversation_id: "conv-inbox-patrol",
    title: "Morning inbox triage",
    conversation_title: "Morning inbox triage",
    cron: "0 */2 * * *",
    status: "active",
  },
  {
    id: "sched-sla",
    conversation_id: "conv-emea",
    title: "SLA breach watch",
    conversation_title: "EMEA Q4 pipeline review",
    cron: "0 8 * * 1-5",
    status: "paused",
  },
];

// settle waits for fonts + a short paint delay so a capture isn't mid-animation.
async function settle(page: Page) {
  await page.evaluate(() => document.fonts?.ready);
  await page.waitForTimeout(400);
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("chat view", async ({ page }) => {
  await mockChatBoot(page, { conversations: SEEDED_CONVERSATIONS });
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: SEEDED_PATROLS } }));
  await page.route("**/api/chat", (r: Route) =>
    fulfillSse(r, [
      { event: "conversation", id: 1, data: { id: "conv-kickoff", title: "Six-week customer kickoff plan", persona: "default" } },
      { event: "tool.call", id: 2, data: { id: "call-rev", name: "run_python", input: JSON.stringify({ code: "18500 * 12 * 1.12" }) } },
      { event: "tool.result", id: 3, data: { id: "call-rev", name: "run_python", text: "248640.00", is_err: false } },
      {
        event: "text.delta",
        id: 4,
        data: {
          text: "## Your six-week kickoff plan\n\nFirst-year revenue: **$248,640**, including the 12% expansion uplift.\n\n| Week | Milestone | Owner | Status |\n| --- | --- | --- | --- |\n| 1 | Access and data onboarding | Priya | Ready |\n| 2–3 | Pilot workspace live | Marcus | Scoped |\n| 4 | First automation in production | Priya | Planned |\n| 5 | Team training and playbooks | Dana | Planned |\n| 6 | Executive review and rollout | You | Target |\n\n**Next step:** confirm the milestone owners, or turn the weekly check-in into an automated **Patrol**.",
        },
      },
      { event: "turn.completed", id: 5, data: { cost_usd: 0.014, model: "anthropic/claude-opus-5.5" } },
    ]),
  );
  await page.goto("/chat");
  await page.getByText("Six-week customer kickoff plan").first().click();
  await page.route("**/api/conversations/conv-kickoff", (r: Route) =>
    r.fulfill({
      json: {
        conversation: {
          id: "conv-kickoff",
          title: "Six-week customer kickoff plan",
          persona: "default",
          model: "anthropic/claude-opus-5.5",
          pinned: true,
          folder: "Customer Onboarding",
        },
        history: [],
      },
    }),
  );
  const composer = page.getByRole("textbox").first();
  await composer.fill("Plan a six-week customer kickoff with owners and calculate first-year revenue at $18.5k/month plus a 12% uplift.");
  await composer.press("Enter");
  await page.getByText("Your six-week kickoff plan").waitFor({ timeout: 15_000 });
  await settle(page);
  await page.screenshot({ path: `${OUT}/chat.png` });
});

test("patrols view", async ({ page }) => {
  const nowSec = Math.floor(new Date("2026-11-04T12:00:00Z").getTime() / 1000);
  await page.clock.setFixedTime(new Date("2026-11-04T12:00:00Z"));
  await mockChatBoot(page, { conversations: SEEDED_CONVERSATIONS });
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: SEEDED_PATROLS } }));
  await page.route("**/api/conversations/conv-patrol-deals", (r: Route) =>
    r.fulfill({
      json: {
        conversation: {
          id: "conv-patrol-deals",
          title: "Deals & spend patrol",
          persona: "default",
          model: "anthropic/claude-opus-5.5",
          pinned: true,
          folder: "Revenue Ops",
          auto_approve: false,
        },
        schedules: [
          {
            id: "sched-deals",
            conversation_id: "conv-patrol-deals",
            kind: "cron",
            title: "Deals & spend patrol",
            prompt: "Sweep active PMP deals and campaign spend for pacing anomalies over 15%.",
            cron: "0 9 * * 1-5",
            timezone: "America/New_York",
            status: "active",
            run_count: 12,
            next_run_at: nowSec + 7200,
          },
        ],
        history: [
          {
            id: 101,
            type: "text",
            role: "user",
            content: {
              kind: "chat_patrol",
              text: "Deals & spend patrol — Sweep active PMP deals and campaign spend for pacing anomalies over 15% and flag items needing action.",
            },
          },
          {
            id: 102,
            type: "tool_call",
            role: "assistant",
            content: {
              id: "tc-pacing",
              name: "dealdesk__list_pacing_alerts",
              input: JSON.stringify({ threshold_pct: 15, window: "24h" }),
            },
          },
          {
            id: 103,
            type: "tool_result",
            role: "tool",
            content: {
              id: "tc-pacing",
              name: "dealdesk__list_pacing_alerts",
              text: "PMP-4821 (Retail Q4 Holiday): +24% ahead of cap\nPMP-4790 (Auto Tier-1 Video): -31% under-delivery (floor mismatch)\n14 other active deals within ±6% target.",
              is_err: false,
            },
          },
          {
            id: 104,
            type: "text",
            role: "assistant",
            content: {
              text: "### Morning deal & spend check-in\n\n14 of 16 active deals are pacing within target. Two deals crossed your **15% threshold** overnight:\n\n| Deal | Advertiser | Pacing (24h) | Recommended action |\n| --- | --- | --- | --- |\n| `PMP-4821` | Retail Q4 Holiday | **+24% over cap** | Shift $4.2k daily cap to even pacing |\n| `PMP-4790` | Auto Tier-1 Video | **-31% under** | Lower CPM floor from $18.50 → $16.00 |\n\nReply **apply both** if you'd like me to stage those deal updates for approval.",
            },
          },
        ],
      },
    }),
  );

  await page.goto("/chat");
  await page.getByText("Deals & spend patrol").first().click();
  await expect(page.getByTestId("patrol-banner")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Morning deal & spend check-in")).toBeVisible({ timeout: 15_000 });
  await settle(page);
  await page.screenshot({ path: `${OUT}/patrols.png` });
});

test("orchestrator dashboard", async ({ page }) => {
  const now = new Date("2026-11-04T12:00:00Z");
  await page.clock.setFixedTime(now);
  await mockChatBoot(page, { conversations: SEEDED_CONVERSATIONS });
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: SEEDED_PATROLS } }));
  const STATS = {
    agent_slots: 4,
    active_agents: 1,
    pending_tasks: 2,
    running_tasks: 1,
    completed_tasks_today: 14,
    failed_tasks_today: 0,
  };
  const MCP_SERVERS = {
    servers: [
      { name: "github", description: "GitHub issues + PRs", tool_count: 9, accounts: ["acme"] },
      { name: "postgres", description: "Analytics warehouse", tool_count: 5, accounts: [] },
    ],
  };
  const TASKS = [
    {
      id: "11111111-1111-1111-1111-111111111111",
      title: "Weekly regional revenue roll-up",
      prompt: "Summarize this week's revenue by region and export the executive CSV",
      status: "success",
      recurrence: "0 9 * * 1",
      created_at: now.toISOString(),
      agent_session_id: "sess-1",
    },
    {
      id: "22222222-2222-2222-2222-222222222222",
      title: "Warehouse partition sync",
      prompt: "Sync the analytics warehouse partitions from the upstream source systems",
      status: "running",
      created_at: now.toISOString(),
      agent_session_id: "sess-2",
    },
    {
      id: "33333333-3333-3333-3333-333333333333",
      title: "Nightly CloudTrail audit export",
      prompt: "Archive daily audit events and verify S3 manifest checksums",
      status: "scheduled",
      recurrence: "0 2 * * *",
      created_at: now.toISOString(),
    },
  ];
  await page.route("**/api/orchestrator/**", (route: Route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats") return route.fulfill({ json: STATS });
    if (path === "/mcp-servers") return route.fulfill({ json: MCP_SERVERS });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
    if (path === "/tasks") return route.fulfill({ json: { data: TASKS, total: TASKS.length, limit: 20, offset: 0 } });
    return route.fulfill({ json: {} });
  });
  await page.goto("/orchestrator");
  await page.getByTestId("orchestrator-dashboard").waitFor({ state: "visible", timeout: 15_000 });
  await page.getByText(/Summarize this week's revenue by region/i).first().waitFor({ timeout: 15_000 });
  await settle(page);
  await page.screenshot({ path: `${OUT}/orchestrator.png` });
});
