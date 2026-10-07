import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useOutputShareStates } from "./useOutputShareStates";

// B17's markers must cover the file the JUST-FINISHED reply presented. The
// message count moves at send time — before the reply exists — so the read
// has to happen again when the turn settles, not only when the count moves.

type Props = {
  conversationId: string | null;
  enabled: boolean;
  paused: boolean;
  turnActive: boolean;
  messageCount: number;
};

function stubOutputs(paths: () => string[]) {
  const fetchMock = vi.fn(async () =>
    new Response(
      JSON.stringify({
        outputs: paths().map((p) => ({ path: p, name: p, size: 1, modified_at: 1, shared: true })),
        total: paths().length,
        shared_count: paths().length,
        team_visible: true,
      }),
      { status: 200 },
    ),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("useOutputShareStates", () => {
  it("re-reads when the turn settles, so the just-presented file gets its marker", async () => {
    let onDisk = ["old.csv"];
    const fetchMock = stubOutputs(() => onDisk);
    const base: Props = { conversationId: "c1", enabled: true, paused: false, turnActive: false, messageCount: 2 };
    const { result, rerender } = renderHook((p: Props) => useOutputShareStates(p), { initialProps: base });
    await waitFor(() => expect(result.current?.shared.has("old.csv")).toBe(true));
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Send: the user message lands and the turn starts. No read mid-turn —
    // the reply (and its file) does not exist yet.
    rerender({ ...base, messageCount: 3, turnActive: true });
    // The reply streams in and presents new.csv; the count may not move again
    // before the turn ends (the reply was already counted as a placeholder).
    onDisk = ["old.csv", "new.csv"];
    rerender({ ...base, messageCount: 4, turnActive: true });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // The turn settles with the count unchanged: the states are re-read.
    rerender({ ...base, messageCount: 4, turnActive: false });
    await waitFor(() => expect(result.current?.shared.has("new.csv")).toBe(true));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("reads nothing for a private chat, and re-reads when a dialog closes", async () => {
    const fetchMock = stubOutputs(() => ["a.csv"]);
    const base: Props = { conversationId: "c1", enabled: false, paused: false, turnActive: false, messageCount: 1 };
    const { result, rerender } = renderHook((p: Props) => useOutputShareStates(p), { initialProps: base });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(result.current).toBeNull();

    rerender({ ...base, enabled: true, paused: true });
    expect(fetchMock).not.toHaveBeenCalled();
    rerender({ ...base, enabled: true, paused: false });
    await waitFor(() => expect(result.current?.id).toBe("c1"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
