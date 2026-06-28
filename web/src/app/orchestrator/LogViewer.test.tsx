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
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    taskLogs: (...args: unknown[]) => taskLogs(...args),
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
