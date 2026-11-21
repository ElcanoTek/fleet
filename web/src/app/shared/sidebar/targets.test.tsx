import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import {
  fetchPatrolList,
  freshPatrols,
  isOwnPatrol,
  patrolsByConversation,
  patrolsChanged,
  refreshPatrols,
  resetPatrolCacheForTests,
  usePatrols,
  type PatrolBrief,
} from "./targets";

afterEach(() => {
  cleanup();
  resetPatrolCacheForTests();
  vi.unstubAllGlobals();
});

const patrol = (over: Partial<PatrolBrief>): PatrolBrief => ({
  id: "p1",
  conversation_id: "c1",
  title: "Watch",
  conversation_title: "Chat",
  status: "active",
  ...over,
});

function stubPatrols(answers: (PatrolBrief[] | "fail" | number)[]) {
  let i = 0;
  const fetchMock = vi.fn(async () => {
    const a = answers[Math.min(i++, answers.length - 1)];
    if (a === "fail") throw new Error("offline");
    if (typeof a === "number") return new Response("{}", { status: a });
    return new Response(JSON.stringify({ patrols: a }), { status: 200 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function Names() {
  const list = usePatrols(true);
  return <p data-testid="names">{list.map((p) => p.title).join(",")}</p>;
}

describe("the patrols a sidebar row is marked from", () => {
  it("a failed read is null, never an empty list", async () => {
    stubPatrols(["fail"]);
    expect(await fetchPatrolList()).toBeNull();
    stubPatrols([500]);
    expect(await fetchPatrolList()).toBeNull();
    stubPatrols([[]]);
    expect(await fetchPatrolList()).toEqual([]);
  });

  it("is read once per page however many parts ask, and again on refresh", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "First" })], [patrol({ title: "Second" })]]);
    render(
      <>
        <Names />
        <Names />
      </>,
    );
    await waitFor(() => expect(screen.getAllByTestId("names")[0]).toHaveTextContent("First"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await act(async () => {
      await refreshPatrols();
    });
    expect(screen.getAllByTestId("names")[1]).toHaveTextContent("Second");
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("a read overtaken by a refresh never puts its older list back", async () => {
    // The first read (say, on focus) is slow; a change's refresh starts after
    // it and answers first. The slow one must not land on top.
    let releaseFirst: (r: Response) => void = () => {};
    let n = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        n++;
        if (n === 1) return new Promise<Response>((resolve) => (releaseFirst = resolve));
        return Promise.resolve(new Response(JSON.stringify({ patrols: [patrol({ title: "After the change" })] }), { status: 200 }));
      }),
    );
    render(<Names />);
    await act(async () => {
      await refreshPatrols();
    });
    expect(screen.getByTestId("names")).toHaveTextContent("After the change");
    await act(async () => {
      releaseFirst(new Response(JSON.stringify({ patrols: [patrol({ title: "Before the change" })] }), { status: 200 }));
      await Promise.resolve();
    });
    expect(screen.getByTestId("names")).toHaveTextContent("After the change");
  });

  it("a change made elsewhere (Chat's Patrol bar) is read again for whoever shows the list", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "Old" })], [patrol({ title: "New" })]]);
    render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Old"));
    act(() => patrolsChanged());
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("New"));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("with nothing showing the list, a change only drops it, and the next read is fresh", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "Old" })], [patrol({ title: "New" })]]);
    expect((await refreshPatrols())?.map((p) => p.title)).toEqual(["Old"]);
    patrolsChanged();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("New"));
  });

  it("a failed read is not kept: the next part of the page that asks reads again", async () => {
    const fetchMock = stubPatrols(["fail", [patrol({ title: "Back" })]]);
    const first = render(<Names />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByTestId("names")).toHaveTextContent("");
    first.unmount();
    render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Back"));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("freshPatrols shares a read on its way, and reads again over an answer already in hand", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "One" })], [patrol({ title: "Two" })]]);
    render(<Names />);
    // The sidebar's read is on its way: the page shares it.
    expect((await freshPatrols())?.map((p) => p.title)).toEqual(["One"]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    // Answered: a later fresh read asks again.
    expect((await freshPatrols())?.map((p) => p.title)).toEqual(["Two"]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("a list read over a minute ago is read again when a part of the page that shows it mounts", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "Then" })], [patrol({ title: "Now" })]]);
    const first = render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Then"));
    first.unmount();
    // Within the minute: the answer in hand is used.
    render(<Names />);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    cleanup();
    const realNow = Date.now();
    const now = vi.spyOn(Date, "now").mockReturnValue(realNow + 61_000);
    try {
      render(<Names />);
      await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Now"));
      expect(fetchMock).toHaveBeenCalledTimes(2);
    } finally {
      now.mockRestore();
    }
  });

  it("coming back to the tab reads the list again while something shows it", async () => {
    const fetchMock = stubPatrols([[patrol({ title: "Before" })], [patrol({ title: "After" })]]);
    render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Before"));
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("After"));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("once the soonest patrol was due, the list is read again, so a row never sits at due now", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const due = Math.floor(Date.now() / 1000) + 60;
      const fetchMock = stubPatrols([[patrol({ title: "Due", next_run_at: due })], [patrol({ title: "Moved on", next_run_at: due + 3600 })]]);
      render(<Names />);
      await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Due"));
      await act(async () => {
        vi.advanceTimersByTime(60_000 + 31_000);
      });
      await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("Moved on"));
      expect(fetchMock).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps what it knew when a refresh fails", async () => {
    stubPatrols([[patrol({ title: "First" })], "fail"]);
    render(<Names />);
    await waitFor(() => expect(screen.getByTestId("names")).toHaveTextContent("First"));
    await act(async () => {
      expect(await refreshPatrols()).toBeNull();
    });
    expect(screen.getByTestId("names")).toHaveTextContent("First");
  });

  it("picks one patrol per chat: the active one that runs soonest, else a paused one", () => {
    const by = patrolsByConversation([
      patrol({ id: "a", conversation_id: "c1", status: "paused" }),
      patrol({ id: "b", conversation_id: "c1", next_run_at: 500 }),
      patrol({ id: "c", conversation_id: "c1", next_run_at: 100 }),
      patrol({ id: "d", conversation_id: "c2", status: "paused" }),
      patrol({ id: "e", conversation_id: "" }),
    ]);
    expect(by.get("c1")?.id).toBe("c");
    expect(by.get("c2")?.id).toBe("d");
    expect(by.size).toBe(2);
  });

  it("a patrol is the caller's own unless the server says it is a teammate's", () => {
    expect(isOwnPatrol(patrol({}))).toBe(true);
    expect(isOwnPatrol(patrol({ mine: true }))).toBe(true);
    expect(isOwnPatrol(patrol({ mine: false, owner_email: "jeanne@example.com" }))).toBe(false);
  });
});
