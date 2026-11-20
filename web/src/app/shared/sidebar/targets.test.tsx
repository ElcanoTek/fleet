import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import {
  fetchPatrolList,
  isOwnPatrol,
  patrolsByConversation,
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
