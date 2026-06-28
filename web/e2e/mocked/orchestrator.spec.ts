import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the orchestrator view: dashboard load, task-create through the
// shared <McpServerPicker> (enable a server + pick a credential account),
// asserting the legacy target_node_name field is GONE, task-list refresh, and
// the react-markdown log viewer. Every /api/orchestrator/* call is intercepted
// by Playwright (no Go moc backend).

const STATS = {
  total_nodes: 1,
  active_nodes: 1,
  pending_tasks: 2,
  running_tasks: 0,
  completed_tasks_today: 3,
  failed_tasks_today: 0,
};

const NODES = { data: [], total: 0, limit: 100, offset: 0 };

// A 1x1 transparent PNG, used as a stand-in for an agent-generated image served
// through the task workspace proxy (#271).
const ONE_PX_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64",
);

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

async function mockOrchestrator(page: Page, captured: { createBody?: Record<string, unknown> }) {
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
      const body = JSON.parse(route.request().postData() ?? "{}") as Record<string, unknown>;
      captured.createBody = body;
      return route.fulfill({ json: createdTask(body) });
    }
    // Agent-generated image fetched through the task workspace proxy (#271):
    // return a real 1x1 PNG so the <img> loads and stays mounted (a 404 would
    // correctly degrade it to the download-link fallback instead).
    if (/^\/tasks\/[^/]+\/workspace\//.test(path)) {
      return route.fulfill({ contentType: "image/png", body: ONE_PX_PNG });
    }
    if (path.startsWith("/logs/")) {
      return route.fulfill({
        json: {
          id: "sess-1",
          title: "Optimization run",
          messages: [
            { id: "m1", role: "user", content: "Run the optimization protocol" },
            {
              id: "m2",
              role: "assistant",
              // The agent references a generate_image artifact the same way it
              // does in chat (#271). The log viewer must rewrite the relative
              // path to the task workspace proxy and render it inline.
              content: "Done. **Report** attached.\n\n![chart](spend_chart.png)",
            },
          ],
        },
      });
    }
    return route.fulfill({ json: {} });
  });
}

function createdTask(body: Record<string, unknown>) {
  return {
    id: "22222222-2222-2222-2222-222222222222",
    prompt: (body.prompt as string) ?? "",
    status: "pending",
    created_at: new Date().toISOString(),
  };
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the dashboard loads stats and the task list", async ({ page }) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");

  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
  await expect(page.getByText("Run the optimization protocol")).toBeVisible();
});

test("create a task via the MCP picker (enable a server + select an account); target_node_name is gone", async ({
  page,
}) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.getByTestId("new-task-btn").click();
  await expect(page.getByRole("dialog", { name: "Create New Task" })).toBeVisible();

  // The MCP picker section exists where the legacy Target Agent / node-name
  // input used to be — and that legacy input is gone entirely.
  await expect(page.getByTestId("task-mcp-section")).toBeVisible();
  await expect(page.getByLabel(/target node|target agent/i)).toHaveCount(0);

  await page.getByLabel("Prompt / Command").fill("Pull yesterday's deal report");

  // Enable the xandr MCP server and pick the client_a credential account.
  await page.getByTestId("mcp-toggle-xandr").check();
  await page.getByTestId("mcp-account-xandr").selectOption("client_a");

  await page.getByRole("button", { name: "Launch task" }).click();

  await expect.poll(() => captured.createBody).toBeTruthy();
  const body = captured.createBody as {
    prompt: string;
    mcp_selection: Array<{ server: string; account?: string }>;
    target_node_name?: unknown;
  };
  expect(body.prompt).toContain("Pull yesterday's deal report");
  expect(body.mcp_selection).toEqual([{ server: "xandr", account: "client_a" }]);
  // The migration replaced target_node_name with the MCP selection — assert the
  // payload never carries the legacy field.
  expect(body.target_node_name).toBeUndefined();

  // The new task shows up in the refreshed list.
  await expect(page.getByText("Pull yesterday's deal report")).toBeVisible();
});

test("opening a task renders its log viewer (react-markdown)", async ({ page }) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.getByText("Run the optimization protocol").click();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toBeVisible();
  await expect(page.getByTestId("log-modal-body")).toContainText("Run the optimization protocol");
  // The assistant message's **Report** markdown renders to <strong>Report</strong>
  // via react-markdown — assert the bolded text, proving the markdown pipeline ran.
  await expect(page.getByTestId("log-modal-body").getByText("Report", { exact: true })).toBeVisible();
});

test("the log viewer renders an agent-generated image inline (#271)", async ({ page }) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.getByText("Run the optimization protocol").click();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toBeVisible();

  // The relative `![chart](spend_chart.png)` reference is rewritten to the task
  // workspace file proxy and rendered as an <img>, mirroring chat's inline
  // image handling. The src must be the task-scoped workspace URL — never the
  // bare relative path (which would 404) and never an arbitrary remote URL.
  const img = page.getByTestId("log-image");
  await expect(img).toBeVisible();
  await expect(img).toHaveAttribute(
    "src",
    /\/api\/orchestrator\/tasks\/[^/]+\/workspace\/spend_chart\.png$/,
  );
});
