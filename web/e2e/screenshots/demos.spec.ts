import { test, expect } from "@playwright/test";
import { mkdir } from "node:fs/promises";
import { loginViaCookie } from "../mocked/_session";
import type { UpcomingRun } from "../../src/app/shared/lib/orchestratorApi";
import { mockChatBoot, fulfillSse } from "../mocked/_mocks";

// Opt-in recordings of the shipped UI with fictional, deterministic data.
// Assertions are intentional: a missing view must fail instead of publishing
// an old or empty dashboard. No model, credentials, or live backend required.
test.skip(process.env.FLEET_RECORD_DEMOS !== "1", "Run docs/scripts/record-web-demos.mjs");
const out = process.env.DEMO_OUT_DIR || "/tmp/fleet-web-demos";
const viewport = { width: 1280, height: 800 };

test("record chat demo", async ({ browser, baseURL }) => {
  await mkdir(out, { recursive: true });
  const context = await browser.newContext({ baseURL, viewport, colorScheme: "dark", timezoneId: "America/New_York", recordVideo: { dir: out, size: viewport } });
  await loginViaCookie(context);
  const page = await context.newPage();
  try {
    await page.route("**/api/**", route => route.fulfill({ json: {} }));
    await mockChatBoot(page);
    await page.route("**/api/chat", route => fulfillSse(route, [
      { event: "conversation", data: { id: "demo-kickoff", title: "A clear plan for kickoff", persona: "default" } },
      { event: "tool.call", data: { id: "revenue", name: "run_python", input: JSON.stringify({ code: "18500 * 12 * 1.12" }) } },
      { event: "tool.result", data: { id: "revenue", name: "run_python", text: "248640.00", is_err: false } },
      { event: "text.delta", data: { text: "## Your six-week kickoff plan\n\nFirst-year revenue: **$248,640**, including the 12% uplift.\n\n| Week | Milestone | Owner |\n| --- | --- | --- |\n| 1 | Access and data onboarding | Priya |\n| 2–3 | Pilot workspace live | Marcus |\n| 4 | First automation in production | Priya |\n| 5 | Team training and playbooks | Dana |\n| 6 | Executive review and rollout | You |\n\n**Next step:** confirm the owners, then turn the weekly review into a recurring task in the Operations Center." } },
      { event: "turn.completed", data: { cost_usd: 0.014, model: "demo-model" } },
    ]));
    await page.goto("/chat");
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.waitForTimeout(1200);
    const composer = page.getByRole("textbox").first();
    await composer.pressSequentially("Plan a six-week customer kickoff with owners. Calculate first-year revenue at $18.5k/month plus a 12% uplift.", { delay: 35 });
    await page.waitForTimeout(600);
    await composer.press("Enter");
    await expect(page.getByText("Your six-week kickoff plan")).toBeVisible();
    await expect(page.getByText("$248,640", { exact: false })).toBeVisible();
    await page.getByText("Your six-week kickoff plan").hover();
    await page.mouse.wheel(0, 600);
    await page.waitForTimeout(6500);
    await page.screenshot({ path: `${out}/chat-final.png` });
  } finally {
    await context.close();
  }
  await page.video()!.saveAs(`${out}/chat.webm`);
});

test("record operations demo", async ({ browser, baseURL }) => {
  await mkdir(out, { recursive: true });
  const context = await browser.newContext({ baseURL, viewport, colorScheme: "dark", timezoneId: "America/New_York", recordVideo: { dir: out, size: viewport } });
  await loginViaCookie(context);
  const page = await context.newPage();
  const now = new Date("2026-09-07T10:00:00Z");
  await page.clock.setFixedTime(now);
  const tasks = [
    { id: "11111111-1111-1111-1111-111111111111", title: "Weekly revenue review", prompt: "Summarize revenue by region and flag changes over 10%", status: "success", created_at: now.toISOString(), recurrence: "0 9 * * 1" },
    { id: "22222222-2222-2222-2222-222222222222", title: "Customer kickoff brief", prompt: "Review onboarding milestones and highlight blockers", status: "running", created_at: now.toISOString() },
    { id: "33333333-3333-3333-3333-333333333333", title: "Daily progress brief", prompt: "Prepare a concise progress brief before standup", status: "scheduled", created_at: now.toISOString(), recurrence: "0 7 * * 1-5" },
  ];
  const upcoming: UpcomingRun[] = [2, 1, 0].map((i, offset) => ({
    task_id: tasks[i].id,
    title: tasks[i].title,
    prompt: tasks[i].prompt,
    recurring: Boolean(tasks[i].recurrence),
    recurrence: tasks[i].recurrence,
    next_run: new Date(now.getTime() + (offset + 1) * 3600000).toISOString(),
  }));
  try {
    await page.route("**/api/**", route => route.fulfill({ json: {} }));
    await mockChatBoot(page);
    await page.route("**/api/orchestrator/**", route => {
      const path = new URL(route.request().url()).pathname.replace("/api/orchestrator", "");
      if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "demo" } });
      if (path === "/stats") return route.fulfill({ json: { pending_tasks: 1, running_tasks: 1, completed_tasks_today: 14, failed_tasks_today: 0, active_agents: 1, agent_slots: 4 } });
      if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
      if (path === "/tasks") return route.fulfill({ json: { data: tasks, total: tasks.length, limit: 20, offset: 0 } });
      if (path === "/tasks/upcoming") return route.fulfill({ json: { upcoming } });
      return route.fulfill({ json: { data: [], servers: [] } });
    });
    await page.goto("/orchestrator");
    await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
    await expect(page.getByText("Weekly revenue review", { exact: true }).first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.waitForTimeout(4500);
    await page.getByRole("tab", { name: "Upcoming" }).click();
    await expect(page.getByRole("heading", { name: "Upcoming Runs" })).toBeVisible();
    await expect(page.getByText("Daily progress brief", { exact: true }).first()).toBeVisible();
    await page.waitForTimeout(4500);
    await page.screenshot({ path: `${out}/ops-final.png` });
  } finally {
    await context.close();
  }
  await page.video()!.saveAs(`${out}/ops.webm`);
});
