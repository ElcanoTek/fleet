import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for the orchestrator view: dashboard load, task-create through the
// shared <McpServerPicker> (enable a server + pick a credential account),
// asserting the legacy target_node_name field is GONE, task-list refresh, and
// the react-markdown log viewer. Every /api/orchestrator/* call is intercepted
// by Playwright (no Go moc backend).

const STATS = {
  pending_tasks: 2,
  running_tasks: 0,
  completed_tasks_today: 3,
  failed_tasks_today: 0,
};

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

// A task only GET /tasks/{id} serves: a task link must open it even though the
// board's list does not include it.
const LINKED_TASK = {
  id: "3333abcd-3333-4333-8333-33333333abcd",
  prompt: "Pull the quarterly pacing report for the linked task",
  status: "success",
  created_at: new Date().toISOString(),
  agent_session_id: "sess-3",
};

// A task id whose by-id fetch fails with a server error: a failure that says
// nothing about the task, so the link stays in the address bar for a reload.
const UNAVAILABLE_TASK_ID = "55555555-5555-4555-8555-555555555555";

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
    if (path === "/mcp-servers") return route.fulfill({ json: MCP_SERVERS });
    if (path === "/config") return route.fulfill({ json: { timezone: "America/New_York" } });

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
    // One task by id: what a task link (/orchestrator/tasks/<id>) loads. The
    // board's own /tasks/tags, /tasks/upcoming and /tasks/estimate reads are
    // not task ids and fall through.
    const oneTask = /^\/tasks\/([^/]+)$/.exec(path);
    if (oneTask && !["tags", "upcoming", "estimate"].includes(oneTask[1]) && method === "GET") {
      if (oneTask[1] === UNAVAILABLE_TASK_ID) {
        return route.fulfill({ status: 503, json: { detail: "try again later" } });
      }
      // Like the server, match the id in any case and answer with its
      // canonical lowercase form.
      const task = [...SEED_TASKS, LINKED_TASK].find((t) => oneTask[1].toLowerCase() === t.id);
      return task
        ? route.fulfill({ json: task })
        : route.fulfill({ status: 404, json: { detail: "task not found" } });
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
  await expect(page.locator("#tasksTable").getByText("Run the optimization protocol")).toBeVisible();
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

  await page.getByLabel("Prompt", { exact: true }).fill("Pull yesterday's deal report");

  // The MCP picker lives in the collapsed "Tools & files" reveal, where the
  // legacy Target Agent / node-name input used to be — and that legacy input
  // is gone entirely.
  await page.getByRole("button", { name: /tools & files/i }).click();
  await expect(page.getByTestId("task-mcp-section")).toBeVisible();
  await expect(page.getByLabel(/target node|target agent/i)).toHaveCount(0);

  // Enable the xandr MCP server and pick the client_a credential account (the
  // account seat renders once the server is enabled).
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
  await expect(page.locator("#tasksTable").getByText("Pull yesterday's deal report")).toBeVisible();
});

test("opening a task renders its log viewer (react-markdown)", async ({ page }) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.locator("#tasksTable").getByText("Run the optimization protocol").click();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toBeVisible();
  await expect(page.getByTestId("log-modal-body")).toContainText("Run the optimization protocol");
  // The assistant message's **Report** markdown renders to <strong>Report</strong>
  // via react-markdown — assert the bolded text, proving the markdown pipeline ran.
  await expect(page.getByTestId("log-modal-body").getByText("Report", { exact: true })).toBeVisible();
});

// A task notification's link (the browser push, the email, the webhook's
// log_url) is /orchestrator/tasks/<id>. It lands on the board with that task's
// log open, and leaves the address bar on the board so a reload does not
// reopen it.
test("a task link opens that task's log", async ({ page }) => {
  await mockOrchestrator(page, {});
  await page.goto(`/orchestrator/tasks/${LINKED_TASK.id}`);

  // The record names its own task's prompt, which only the by-id answer has:
  // the board's list does not include this task.
  const dialog = page.getByRole("dialog", { name: "Task Logs" });
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText(LINKED_TASK.prompt);
  await expect(page.locator("#tasksTable")).not.toContainText(LINKED_TASK.prompt);
  await expect(page).toHaveURL(/\/orchestrator$/);
});

test("a link that is not a task id is not fetched, and says so", async ({ page }) => {
  // A one-task fetch, or the list path ".." would collapse it to. The board's
  // own /tasks/tags and /tasks/upcoming reads are not task fetches.
  const fetched: string[] = [];
  page.on("request", (r) => {
    const path = new URL(r.url()).pathname;
    const one = /^\/api\/orchestrator\/tasks\/([^/]+)$/.exec(path);
    if (path === "/api/orchestrator/" || (one && !["tags", "upcoming"].includes(one[1]))) {
      fetched.push(path);
    }
  });
  await mockOrchestrator(page, {});
  await page.goto("/orchestrator?task=..");

  await expect(page.getByText(/That task could not be opened/)).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toHaveCount(0);
  expect(fetched).toEqual([]);
});

test("a link to a task that cannot be loaded says so", async ({ page }) => {
  await mockOrchestrator(page, {});
  await page.goto("/orchestrator/tasks/99999999-9999-9999-9999-999999999999");

  await expect(page.getByText(/That task could not be opened/)).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toHaveCount(0);
  await expect(page).toHaveURL(/\/orchestrator$/);
});

test("a task link with an uppercased id still opens the task", async ({ page }) => {
  await mockOrchestrator(page, {});
  await page.goto(`/orchestrator/tasks/${LINKED_TASK.id.toUpperCase()}`);

  const dialog = page.getByRole("dialog", { name: "Task Logs" });
  await expect(dialog).toContainText(LINKED_TASK.prompt);
  await expect(page).toHaveURL(/\/orchestrator$/);
});

test("a task link the server could not answer for now stays for a reload", async ({ page }) => {
  await mockOrchestrator(page, {});
  await page.goto(`/orchestrator/tasks/${UNAVAILABLE_TASK_ID}`);

  await expect(page.getByText(/could not be loaded just now/)).toBeVisible();
  await expect(page.getByRole("dialog", { name: "Task Logs" })).toHaveCount(0);
  await expect(page).toHaveURL(new RegExp(`\\?task=${UNAVAILABLE_TASK_ID}$`));
});

test("the log viewer renders an agent-generated image inline (#271)", async ({ page }) => {
  const captured: { createBody?: Record<string, unknown> } = {};
  await mockOrchestrator(page, captured);
  await page.goto("/orchestrator");
  await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();

  await page.locator("#tasksTable").getByText("Run the optimization protocol").click();
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
