import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { BulkDeleteConfirmModal } from "./BulkDeleteConfirmModal";

// The multi-select delete (#279) must not be the quiet way around B33: when
// the selection holds team-shared chats, it names the team's loss with the
// summed shared-file count.

function stubOutputs(counts: Record<string, number | "fail">) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    const id = /conversations\/([^/]+)\/outputs/.exec(url)?.[1] ?? "";
    const n = counts[id];
    if (n === undefined || n === "fail") return new Response("nope", { status: 500 });
    return new Response(
      JSON.stringify({ outputs: [], total: n, shared_count: n, team_visible: true }),
      { status: 200 },
    );
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("BulkDeleteConfirmModal", () => {
  it("without shared chats keeps today's copy and fetches nothing", () => {
    const fetchMock = stubOutputs({});
    render(<BulkDeleteConfirmModal count={3} onCancel={() => {}} onConfirm={() => {}} />);
    const dialog = screen.getByRole("dialog", { name: "Delete 3 conversations?" });
    expect(dialog).toHaveTextContent("3 conversations will be removed. This cannot be undone.");
    expect(screen.queryByTestId("bulk-delete-shared-loss")).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("names the team and the summed shared files of the shared chats", async () => {
    stubOutputs({ a: 2, b: 3 });
    render(
      <BulkDeleteConfirmModal
        count={4}
        sharedLoss={[{ conversationIds: ["a", "b"], team: "Elcano" }]}
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("bulk-delete-shared-loss")).toHaveTextContent(
        "2 of these are shared with Elcano. Elcano loses access to them and their 5 shared files. Teammates who branched them keep their copies.",
      ),
    );
  });

  it("names every team a selection spans, with the files summed across all", async () => {
    stubOutputs({ a: 1, b: 2, c: 0, d: 4 });
    render(
      <BulkDeleteConfirmModal
        count={5}
        sharedLoss={[
          { conversationIds: ["a", "b", "c"], team: "Quant" },
          { conversationIds: ["d"], team: "Ops" },
        ]}
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("bulk-delete-shared-loss")).toHaveTextContent(
        "3 of these are shared with Quant and 1 with Ops. They lose access to them and their 7 shared files. Teammates who branched them keep their copies.",
      ),
    );
  });

  it("lists three teams with commas", async () => {
    stubOutputs({ a: 0, b: 0, c: 0 });
    render(
      <BulkDeleteConfirmModal
        count={3}
        sharedLoss={[
          { conversationIds: ["a"], team: "Quant" },
          { conversationIds: ["b"], team: "Ops" },
          { conversationIds: ["c"], team: "Risk" },
        ]}
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("bulk-delete-shared-loss")).toHaveTextContent(
        "1 of these is shared with Quant, 1 with Ops and 1 with Risk. They lose access to them. Teammates who branched them keep their copies.",
      ),
    );
  });

  it("drops the number when a count fails", async () => {
    stubOutputs({ a: 2, b: "fail" });
    render(
      <BulkDeleteConfirmModal
        count={3}
        sharedLoss={[{ conversationIds: ["a", "b"], team: "Elcano" }]}
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("bulk-delete-shared-loss")).toHaveTextContent(
        "2 of these are shared with Elcano. Elcano loses access to them and their shared files.",
      ),
    );
    expect(screen.getByTestId("bulk-delete-shared-loss")).not.toHaveTextContent(/\d shared file/);
  });

  it("one shared chat, no shared files: singular copy, no file clause", async () => {
    stubOutputs({ a: 0 });
    render(
      <BulkDeleteConfirmModal
        count={2}
        sharedLoss={[{ conversationIds: ["a"], team: "Elcano" }]}
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByTestId("bulk-delete-shared-loss")).toHaveTextContent(
        "1 of these is shared with Elcano. Elcano loses access to it. Teammates who branched it keep their copies.",
      ),
    );
  });

  it("confirms once the countdown ends and the count is known", async () => {
    stubOutputs({ a: 1 });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const onConfirm = vi.fn();
    render(
      <BulkDeleteConfirmModal
        count={2}
        sharedLoss={[{ conversationIds: ["a"], team: "Elcano" }]}
        onCancel={() => {}}
        onConfirm={onConfirm}
      />,
    );
    await act(async () => {
      vi.advanceTimersByTime(3200);
    });
    const btn = await screen.findByRole("button", { name: "Delete 2" });
    expect(btn).toBeEnabled();
    fireEvent.click(btn);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
