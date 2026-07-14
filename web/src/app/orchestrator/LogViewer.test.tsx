import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
import { LogViewer } from "./LogViewer";
import type { LogSession, Task } from "@/app/shared/lib/orchestratorApi";

// LogViewer renders a scheduled task's stored log. #271 adds inline rendering of
// agent-generated images: a relative workspace path in the log markdown is
// rewritten to the task workspace file proxy and shown as an <img>; an absolute
// remote URL is left untouched (no SSRF / remote-fetch); a workspace image that
// fails to load degrades to a download link.

const taskLogs = vi.fn();
const rerunTask = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    taskLogs: (...args: unknown[]) => taskLogs(...args),
    rerunTask: (...args: unknown[]) => rerunTask(...args),
  },
}));

const TASK_ID = "11111111-1111-1111-1111-111111111111";
const TASK: Task = { id: TASK_ID, prompt: "Generate a weekly infographic" };

function mockSession(session: LogSession) {
  taskLogs.mockReset();
  taskLogs.mockResolvedValue(session);
}

afterEach(() => cleanup());

describe("LogViewer inline images (#271)", () => {
  it("rewrites a relative agent-image reference to the task workspace proxy", async () => {
    mockSession({
      id: "sess-1",
      title: "Weekly run",
      messages: [
        { id: "u1", role: "user", content: "Generate a weekly infographic" },
        {
          id: "a1",
          role: "assistant",
          content: "Here is the infographic:\n\n![weekly infographic](weekly.png)",
        },
      ],
    });

    render(<LogViewer task={TASK} onClose={() => {}} />);

    const img = await screen.findByTestId("log-image");
    expect(img).toHaveAttribute(
      "src",
      `/api/orchestrator/tasks/${TASK_ID}/workspace/weekly.png`,
    );
    expect(img).toHaveAttribute("alt", "weekly infographic");
  });

  it("does NOT rewrite an absolute remote URL (no SSRF / remote fetch)", async () => {
    mockSession({
      id: "sess-2",
      messages: [
        {
          id: "a1",
          role: "assistant",
          content: "![tracker](https://evil.example/track.png)",
        },
      ],
    });

    render(<LogViewer task={TASK} onClose={() => {}} />);

    const img = await screen.findByTestId("log-image");
    // The remote URL is passed straight through — never rewritten to the
    // workspace proxy, and never fetched as if it were a local file.
    expect(img).toHaveAttribute("src", "https://evil.example/track.png");
    expect(img.getAttribute("src")).not.toContain("/api/orchestrator/");
  });

  it("degrades a broken workspace image to a download link", async () => {
    mockSession({
      id: "sess-3",
      messages: [
        {
          id: "a1",
          role: "assistant",
          content: "![chart](chart.png)",
        },
      ],
    });

    render(<LogViewer task={TASK} onClose={() => {}} />);

    const img = await screen.findByTestId("log-image");
    // Simulate the file being unavailable (GC'd / wrong type): the onError
    // handler swaps the <img> for a link so the user still sees a download
    // affordance instead of a broken-image icon.
    fireEvent.error(img);

    await waitFor(() => {
      const link = screen.getByRole("link", { name: "chart" });
      expect(link).toHaveAttribute(
        "href",
        `/api/orchestrator/tasks/${TASK_ID}/workspace/chart.png`,
      );
      expect(link).toHaveAttribute("download", "chart.png");
    });
  });

  it("rewrites a relative <a> link to the workspace proxy with a download attr", async () => {
    mockSession({
      id: "sess-4",
      messages: [
        {
          id: "a1",
          role: "assistant",
          content: "[report.png](report.png)",
        },
      ],
    });

    render(<LogViewer task={TASK} onClose={() => {}} />);

    const link = await screen.findByRole("link", { name: "report.png" });
    expect(link).toHaveAttribute(
      "href",
      `/api/orchestrator/tasks/${TASK_ID}/workspace/report.png`,
    );
    expect(link).toHaveAttribute("download", "report.png");
  });
});

// ── task-detail modal (moc parity): summary strip, metrics, filters,
// resubmit, download ─────────────────────────────────────────────────────────

const DONE_TASK: Task = {
  id: TASK_ID,
  prompt: "Generate a weekly infographic",
  status: "success",
  created_by_username: "junhao",
  created_at: "2026-07-14T09:00:00",
  recurrence: "0 9 * * 6,0",
};

const RICH_SESSION: LogSession = {
  id: "sess-9",
  title: "Weekly run",
  prompt_tokens: 507240,
  completion_tokens: 58225,
  cached_tokens: 2412434,
  cost: 1.0645,
  messages: [
    { id: "u1", role: "user", content: "Generate a weekly infographic", created_at: 1752500000 },
    {
      id: "a1",
      role: "assistant",
      content: "Working on it.",
      model: "z-ai/glm-5.2",
      provider: "StreamLake",
      created_at: 1752500010,
      tool_calls: [{ id: "c1", name: "run_python", arguments: "{\"code\":\"1+1\"}" }],
    },
    { id: "t1", role: "tool", content: "2", tool_call_id: "c1", created_at: 1752500020 },
    {
      id: "a2",
      role: "assistant",
      content: "Done — chart attached.",
      model: "z-ai/glm-5.2",
      provider: "StreamLake",
      created_at: 1752500030,
    },
  ],
};

describe("LogViewer task-detail modal", () => {
  it("renders the task summary strip mirroring the table row", async () => {
    mockSession(RICH_SESSION);
    render(<LogViewer task={DONE_TASK} onClose={() => {}} />);
    const summary = await screen.findByTestId("task-summary");
    expect(summary).toHaveTextContent("11111111…");
    expect(summary).toHaveTextContent("success");
    expect(summary).toHaveTextContent("junhao");
    expect(summary).toHaveTextContent(/9:00 AM · Sat, Sun/);
  });

  it("renders session token totals and cost in the metrics strip", async () => {
    mockSession(RICH_SESSION);
    render(<LogViewer task={DONE_TASK} onClose={() => {}} />);
    const metrics = await screen.findByTestId("log-metrics");
    expect(metrics).toHaveTextContent("507,240");
    expect(metrics).toHaveTextContent("58,225");
    expect(metrics).toHaveTextContent("2,412,434");
    expect(metrics).toHaveTextContent("$1.0645");
    expect(metrics).toHaveTextContent("4"); // messages
  });

  it("filters the transcript by interaction chips (union) and clears", async () => {
    mockSession(RICH_SESSION);
    render(<LogViewer task={DONE_TASK} onClose={() => {}} />);
    const filters = await screen.findByTestId("log-filters");
    expect(filters).toHaveTextContent("Showing all 4 messages");
    // tool chip resolves the tool-result's name via tool_call_id
    const chip = screen.getByRole("button", { name: /run_python/ });
    fireEvent.click(chip);
    await waitFor(() =>
      expect(screen.getByTestId("log-filters")).toHaveTextContent(
        "Showing 2 of 4 messages",
      ),
    );
    // union with responses highlight widens the selection
    fireEvent.click(screen.getByRole("button", { name: /^Responses/ }));
    await waitFor(() =>
      expect(screen.getByTestId("log-filters")).toHaveTextContent(
        "Showing 3 of 4 messages",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Clear all" }));
    await waitFor(() =>
      expect(screen.getByTestId("log-filters")).toHaveTextContent(
        "Showing all 4 messages",
      ),
    );
  });

  it("resubmits a terminal task and notifies the dashboard", async () => {
    mockSession(RICH_SESSION);
    rerunTask.mockReset();
    rerunTask.mockResolvedValue({ id: "99999999-9999-9999-9999-999999999999" });
    const onResubmitted = vi.fn();
    render(
      <LogViewer task={DONE_TASK} onClose={() => {}} onResubmitted={onResubmitted} />,
    );
    fireEvent.click(await screen.findByTestId("resubmit-task-button"));
    await waitFor(() => expect(rerunTask).toHaveBeenCalledWith(TASK_ID));
    await waitFor(() => expect(onResubmitted).toHaveBeenCalled());
  });

  it("hides Resubmit for a scheduled task and downloads the session JSON", async () => {
    mockSession(RICH_SESSION);
    render(
      <LogViewer
        task={{ ...DONE_TASK, status: "scheduled" }}
        onClose={() => {}}
      />,
    );
    const download = await screen.findByTestId("download-logs-button");
    expect(screen.queryByTestId("resubmit-task-button")).toBeNull();

    const createObjectURL = vi.fn(() => "blob:mock");
    const revokeObjectURL = vi.fn();
    Object.assign(URL, { createObjectURL, revokeObjectURL });
    fireEvent.click(download);
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:mock");
  });
});
