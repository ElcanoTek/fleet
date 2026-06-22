import { test, expect, creds } from "./fixtures";

// LIVE scheduled-task journey against the fully real stack. A task created
// through the orchestrator UI is leased by the REAL worker pool and run to
// `success` through the SAME real Podman sandbox the chat path uses — driven by
// the fake LLM (scenario "sched-task": run_python → confirm_audit → final text,
// which clears the scheduled-mode self-audit enforcement gate so the task can
// reach success). Logs are then retrievable through the real log viewer.
//
// The orchestrator's task-create + status auth is the moc username/password
// (Bearer-token) path, so this spec logs into the orchestrator view with the
// seeded sched admin user rather than relying on the chat cookie (which the
// orchestrator's CreateTask handler does not honor for write auth).

test.describe("live scheduled task → real worker pool + sandbox", () => {
  test("create a task, the real worker runs it to success, logs are retrievable", async ({
    page,
    login,
  }) => {
    // The worker pool polls on a tick and the cold container + self-audit loop
    // take time; give this journey room well beyond the 60s default.
    test.setTimeout(240_000);

    // TWO auth layers are required for the orchestrator write path:
    //  1. the app SESSION cookie (login fixture) — the Next middleware gates
    //     /orchestrator and redirects to /login without it;
    //  2. the orchestrator MOC bearer (username/password) — the orchestrator's
    //     CreateTask handler authorizes writes by Bearer token, not the cookie.
    await login();
    await page.goto("/orchestrator");

    // Orchestrator login card (OrchestratorLogin). Its inputs carry an explicit
    // aria-label; scope by it (exact) to avoid the show/hide-password button.
    await page.getByLabel("Username", { exact: true }).fill(creds.schedUsername);
    await page.getByLabel("Password", { exact: true }).fill(creds.password);
    await page.getByRole("button", { name: "Login with username and password" }).click();

    await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible({ timeout: 20_000 });

    // Open the create-task modal and submit a real task. The scenario marker in
    // the prompt steers the fake LLM through the audit-clearing tool sequence.
    // The unique tag goes FIRST so it survives the 80-char prompt-cell truncate.
    const tag = `e2e-${Date.now()}`;
    const prompt = `${tag} run scheduled sandbox task [[scenario:sched-task]]`;
    await page.getByTestId("new-task-btn").click();
    await expect(page.getByRole("dialog", { name: "Create New Task" })).toBeVisible();
    await page.getByLabel("Prompt / Command").fill(prompt);
    await page.getByRole("button", { name: /launch task/i }).click();

    // The new task appears in the list, matched on the unique leading tag.
    const taskRow = page.locator("tr[data-task-id]", { hasText: tag });
    await expect(taskRow.first()).toBeVisible({ timeout: 20_000 });

    // Bounded polling for the REAL worker pool to lease + run the task to
    // success through the sandbox. The pool polls every 30s but bursts on
    // startup; allow generous time for the cold container + audit loop.
    await expect(async () => {
      await page.reload();
      await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
      const row = page.locator("tr[data-task-id]", { hasText: tag }).first();
      await expect(row.locator(".status-badge.status-success")).toBeVisible();
    }).toPass({ timeout: 150_000, intervals: [3_000] });

    // Open the task's logs (real LogViewer, fetched from /api/orchestrator/logs)
    // and assert the agent's final report rendered.
    await page.locator("tr[data-task-id]", { hasText: tag }).first().click();
    await expect(page.getByRole("dialog", { name: "Task Logs" })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("log-modal-body")).toContainText(/SCHED_TASK_OK 45/, {
      timeout: 15_000,
    });
  });
});
