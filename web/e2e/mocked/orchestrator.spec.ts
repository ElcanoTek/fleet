import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked P7 e2e for the orchestrator view: task-create (with MCP enable +
// account select), task list, and log view — all driven against the Next dev
// server with every /api/orchestrator/* call intercepted by Playwright (no Go
// backend; that's P8).

const STATS = {
  total_nodes: 1,
  active_nodes: 1,
  pending_tasks: 2,
  running_tasks: 0,
  completed_tasks_today: 3,
  failed_tasks_today: 0,
};

const NODES = { data: [], total: 0, limit: 100, offset: 0 };

const MCP_SERVERS = {
  servers: [
    { name: "xandr", description: "Xandr DSP", tool_count: 7, accounts: ["client_a", "client_b"] },
    { name: "magnite", description: "Magnite SSP", tool_count: 4, accounts: [] },
  ],
};

const SEED_TASKS = [
  {
    id: "11111111-1111-1111-1111-111111111111",
    prompt: "Run the optimization protocol",
    status: "success",
    created_at: new Date().toISOString(),
    agent_session_id: "sess-1",
  },
];

async function mockOrchestrator(page: Page, captured: { createBody?: unknown }) {
  await page.route("**/api/orchestrator/**", async (route: Route) => {
    const url = new URL(route.request().url());
    const path = url.pathname.replace("/api/orchestrator", "");
    const method = route.request().method();

    if (path === "/me") return route.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats") return route.fulfill({ json: STATS });
    if (path === "/nodes") return route.fulfill({ json: NODES });
    if (path === "/mcp-servers") return route.fulfill({ json: MCP_SERVERS });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });
    if (path === "/concurrency")
      return route.fulfill({ json: { max_concurrent_agents: 4, warm_pool_size: 2 } });

    if (path === "/tasks" && method === "GET") {
      const tasks = [...SEED_TASKS, ...(captured.createBody ? [createdTask(captured.createBody)] : [])];
      return route.fulfill({ json: { data: tasks, total: tasks.length, limit: 20, offset: 0 } });
    }
    if (path === "/tasks" && method === "POST") {
      captured.createBody = JSON.parse(route.request().postData() ?? "{}");
      return route.fulfill({ json: createdTask(captured.createBody) });
    }
    if (path.startsWith("/logs/")) {
      return route.fulfill({
        json: {
          id: "sess-1",
          title: "Optimization run",
          messages: [
            { id: "m1", role: "user", content: "Run the optimization protocol" },
            { id: "m2", role: "assistant", content: "Done. **Report** attached." },
          ],
        },
      });
    }
    return route.fulfill({ json: {} });
  });
}

function createdTask(body: unknown) {
  const b = body as { prompt?: string };
  return {
    id: "22222222-2222-2222-2222-222222222222",
    prompt: b.prompt ?? "",
    status: "pending",
    created_at: new Date().toISOString(),
  };
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("orchestrator dashboard loads stats and the task list", async ({ page }) => {
  const captured: { createBody?: unknown } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");

  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
  await expect(page.getByText("Run the optimization protocol")).toBeVisible();
});

test("create a task with an MCP server enabled + account selected", async ({ page }) => {
  const captured: { createBody?: unknown } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.getByTestId("new-task-btn").click();
  await expect(page.getByRole("dialog", { name: "Create New Task" })).toBeVisible();

  await page.getByLabel("Prompt / Command").fill("Pull yesterday's deal report");

  // Enable the xandr MCP server and pick the client_a credential account.
  await page.getByTestId("mcp-toggle-xandr").check();
  await page.getByTestId("mcp-account-xandr").selectOption("client_a");

  await page.getByRole("button", { name: "Launch task" }).click();

  await expect.poll(() => captured.createBody).toBeTruthy();
  const body = captured.createBody as { prompt: string; mcp_selection: Array<{ server: string; account?: string }> };
  expect(body.prompt).toContain("Pull yesterday's deal report");
  expect(body.mcp_selection).toEqual([{ server: "xandr", account: "client_a" }]);

  // The new task shows up in the refreshed list.
  await expect(page.getByText("Pull yesterday's deal report")).toBeVisible();
});

test("open a task's log viewer", async ({ page }) => {
  const captured: { createBody?: unknown } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.getByText("Run the optimization protocol").click();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toBeVisible();
  await expect(page.getByTestId("log-modal-body")).toContainText("Run the optimization protocol");
  await expect(page.getByTestId("log-modal-body")).toContainText("Report");
});
