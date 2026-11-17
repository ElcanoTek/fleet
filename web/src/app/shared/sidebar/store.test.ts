import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { addShortcut, BUILTIN_LAYOUT, normalizeLayout } from "./layout";
import { setBootstrapForTest } from "@/app/lib/bootstrap";
import { _resetMeForTests } from "@/app/shared/lib/account";
import {
  flushSidebarSaveForTests,
  getSidebarState,
  loadSidebar,
  resetSidebarLayout,
  resetSidebarStoreForTests,
  setSidebarLayout,
} from "./store";
import { resetFallbackTargetsForTests } from "./targets";
import { entryHomeEligible, captureEntryAddress, markEntryHomeUsed, resetEntryHomeForTests, resolveEntryHome } from "./home";

const WORKSPACE = normalizeLayout({ items: [{ id: "tasks", kind: "builtin" }], home: "tasks" });
const OWN = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Board" });

function mockFetch(answers: { get?: unknown; status?: number }) {
  const calls: { method: string; body?: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      if (answers.status && answers.status >= 400) return new Response("down", { status: answers.status });
      if (method === "GET") return Response.json(answers.get ?? { layout: null, workspace_default: null });
      if (method === "PUT") return Response.json({ layout: calls[calls.length - 1].body });
      return Response.json({ layout: null });
    }),
  );
  return calls;
}

beforeEach(() => {
  setBootstrapForTest({ session: { email: "ana@example.com" } });
  resetSidebarStoreForTests();
  window.localStorage.clear();
});
afterEach(() => {
  setBootstrapForTest(null);
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("sidebar store", () => {
  it("follows own ?? workspace ?? built-in", async () => {
    mockFetch({ get: { layout: null, workspace_default: WORKSPACE } });
    await loadSidebar();
    expect(getSidebarState()).toMatchObject({ status: "ready", source: "workspace", own: null });
    expect(getSidebarState().layout.items[0].id).toBe("tasks");
  });

  it("the cached layout belongs to the account that saved it", () => {
    window.localStorage.setItem("fleet.sidebar.layout.v2", JSON.stringify({ email: "ana@example.com", layout: OWN }));
    resetSidebarStoreForTests();
    expect(getSidebarState().layout.items).toHaveLength(4);
    // The next person on this browser never paints (or is sent Home by) it.
    setBootstrapForTest({ session: { email: "bo@example.com" } });
    resetSidebarStoreForTests();
    expect(getSidebarState().layout.items).toHaveLength(3);
  });

  it("an edit still waiting when the page is hidden is sent with keepalive", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      Response.json({ layout: null, workspace_default: null }),
    );
    vi.stubGlobal("fetch", fetchMock);
    await loadSidebar();
    setSidebarLayout(OWN);
    flushSidebarSaveForTests();
    const put = fetchMock.mock.calls.find((c) => c[1]?.method === "PUT");
    expect(put?.[1]?.keepalive).toBe(true);
    // It carries the edit's number, so the server can order it against a
    // save still in flight.
    expect(JSON.parse(String(put?.[1]?.body)).rev).toBeGreaterThan(0);
    await vi.advanceTimersByTimeAsync(600);
    expect(fetchMock.mock.calls.filter((c) => c[1]?.method === "PUT")).toHaveLength(1);
  });

  it("an unreachable server renders the built-in layout and says it cannot save", async () => {
    window.localStorage.setItem("fleet.sidebar.layout.v2", JSON.stringify({ email: "ana@example.com", layout: OWN }));
    resetSidebarStoreForTests();
    expect(getSidebarState().layout.items).toHaveLength(4); // painted from the cache first
    mockFetch({ status: 502 });
    await loadSidebar();
    expect(getSidebarState()).toMatchObject({ status: "unavailable", source: "builtin" });
    expect(getSidebarState().layout.items).toHaveLength(3);
  });

  it("an edit shows at once and is saved once after a burst", async () => {
    vi.useFakeTimers();
    const calls = mockFetch({});
    await loadSidebar();
    setSidebarLayout(OWN);
    setSidebarLayout(addShortcut(OWN, { kind: "project", ref: "p1" }));
    expect(getSidebarState().layout.items).toHaveLength(5);
    expect(getSidebarState().saving).toBe(true);
    await vi.advanceTimersByTimeAsync(600);
    expect(calls.filter((c) => c.method === "PUT")).toHaveLength(1);
    expect(getSidebarState()).toMatchObject({ saving: false, saveError: false, source: "user" });
  });

  it("reset drops the person's own layout and deletes it on the server", async () => {
    vi.useFakeTimers();
    const calls = mockFetch({ get: { layout: OWN, workspace_default: WORKSPACE } });
    await loadSidebar();
    expect(getSidebarState().source).toBe("user");
    resetSidebarLayout();
    expect(getSidebarState().source).toBe("workspace");
    await vi.advanceTimersByTimeAsync(600);
    const del = calls.find((c) => c.method === "DELETE");
    expect((del?.body as { rev?: number } | undefined)?.rev).toBeGreaterThan(0);
  });

  it("saves run one at a time, so an older one can never land after a newer one", async () => {
    vi.useFakeTimers();
    const sent: string[] = [];
    let releaseFirst: () => void = () => {};
    let puts = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init?: RequestInit) => {
        if ((init?.method ?? "GET") === "GET") return Response.json({ layout: null, workspace_default: null });
        const body = String(init?.body ?? "");
        puts++;
        if (puts === 1) await new Promise<void>((r) => (releaseFirst = r));
        sent.push(body);
        return Response.json({ layout: JSON.parse(body) });
      }),
    );
    await loadSidebar();
    setSidebarLayout(OWN);
    await vi.advanceTimersByTimeAsync(600); // first PUT starts and hangs
    const newer = addShortcut(OWN, { kind: "project", ref: "p9", title: "Newer" });
    setSidebarLayout(newer);
    await vi.advanceTimersByTimeAsync(600); // second save is due, but waits its turn
    expect(puts).toBe(1);
    releaseFirst();
    await vi.advanceTimersByTimeAsync(10);
    expect(sent).toHaveLength(2);
    expect(JSON.parse(sent[1]).items).toHaveLength(5); // the newer layout committed last
    expect(JSON.parse(sent[1]).rev).toBeGreaterThan(JSON.parse(sent[0]).rev); // and is numbered after it
    expect(getSidebarState()).toMatchObject({ saving: false, saveError: false });
  });

  it("an edit made while the first read is in flight is not overwritten by it", async () => {
    let release: (v: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init?: RequestInit) =>
        (init?.method ?? "GET") === "GET"
          ? new Promise<Response>((r) => (release = r))
          : Promise.resolve(Response.json({ layout: null })),
      ),
    );
    const read = loadSidebar();
    setSidebarLayout(OWN);
    release(Response.json({ layout: null, workspace_default: WORKSPACE }));
    await read;
    expect(getSidebarState().layout.items).toHaveLength(4);
    expect(getSidebarState().workspace?.home).toBe("tasks");
  });
});

describe("Home on entry", () => {
  beforeEach(() => resetEntryHomeForTests());

  it("applies only to a bare /chat entry, once", () => {
    window.history.replaceState(null, "", "/chat");
    captureEntryAddress();
    expect(entryHomeEligible()).toBe(true);
    markEntryHomeUsed();
    expect(entryHomeEligible()).toBe(false);
  });

  it("never applies to a link that asks for something", () => {
    window.history.replaceState(null, "", "/chat?c=abc");
    captureEntryAddress();
    expect(entryHomeEligible()).toBe(false);
    resetEntryHomeForTests();
    window.history.replaceState(null, "", "/settings");
    captureEntryAddress();
    expect(entryHomeEligible()).toBe(false);
    resetEntryHomeForTests();
    window.history.replaceState(null, "", "/chat?lang=es");
    captureEntryAddress();
    expect(entryHomeEligible()).toBe(true);
  });

  it("resolves the Home item from the layout", async () => {
    mockFetch({ get: { layout: null, workspace_default: WORKSPACE } });
    expect((await resolveEntryHome(1000))?.id).toBe("tasks");
  });

  describe("only somewhere this account can open", () => {
    const chatHome = (ref: string) =>
      normalizeLayout({ items: [{ id: "s-1", kind: "chat", ref, title: "Board" }], home: "s-1" });
    function stubLists(own: string[], opts: { failLists?: boolean } = {}) {
      vi.stubGlobal(
        "fetch",
        vi.fn(async (url: string) => {
          if (url === "/api/sidebar") return Response.json(listsAnswer.current);
          if (opts.failLists) return new Response("down", { status: 502 });
          if (url.startsWith("/api/projects")) return Response.json({ projects: [] });
          if (url.includes("archived") || url.includes("scope=shared")) return Response.json({ conversations: [] });
          return Response.json({ conversations: own.map((id) => ({ id, title: id })) });
        }),
      );
    }
    const listsAnswer: { current: unknown } = { current: null };
    beforeEach(() => resetFallbackTargetsForTests());

    it("skips a workspace Home this account cannot open", async () => {
      listsAnswer.current = { layout: null, workspace_default: chatHome("admins-own") };
      stubLists(["c1"]);
      expect(await resolveEntryHome(1000)).toBeNull();
    });

    it("skips a workspace Home it cannot confirm, but keeps the person's own", async () => {
      listsAnswer.current = { layout: null, workspace_default: chatHome("c1") };
      stubLists(["c1"], { failLists: true });
      expect(await resolveEntryHome(1000)).toBeNull();
      resetSidebarStoreForTests();
      resetFallbackTargetsForTests();
      listsAnswer.current = { layout: chatHome("c1"), workspace_default: null };
      stubLists(["c1"], { failLists: true });
      expect((await resolveEntryHome(1000))?.ref).toBe("c1");
    });

    it("skips the person's own Home once it is gone, and opens it while it is there", async () => {
      listsAnswer.current = { layout: chatHome("deleted"), workspace_default: null };
      stubLists(["c1"]);
      expect(await resolveEntryHome(1000)).toBeNull();
      resetSidebarStoreForTests();
      resetFallbackTargetsForTests();
      listsAnswer.current = { layout: chatHome("c1"), workspace_default: null };
      stubLists(["c1"]);
      expect((await resolveEntryHome(1000))?.ref).toBe("c1");
    });

    it("follows a personal Analytics Home only while the account is an admin", async () => {
      const analyticsHome = normalizeLayout({ items: [{ id: "s-a", kind: "page", ref: "analytics" }], home: "s-a" });
      for (const [admin, expected] of [
        [false, null],
        [true, "analytics"],
      ] as const) {
        resetSidebarStoreForTests();
        _resetMeForTests();
        vi.stubGlobal(
          "fetch",
          vi.fn(async (url: string) =>
            url === "/api/me"
              ? Response.json({ email: "ana@example.com", role: admin ? "admin" : "member", admin })
              : Response.json({ layout: analyticsHome, workspace_default: null }),
          ),
        );
        expect((await resolveEntryHome(1000))?.ref ?? null).toBe(expected);
      }
    });

    it("never takes a task as a workspace Home", async () => {
      listsAnswer.current = {
        layout: null,
        workspace_default: normalizeLayout({ items: [{ id: "s-t", kind: "task", ref: "t1" }], home: "s-t" }),
      };
      stubLists([]);
      expect(await resolveEntryHome(1000)).toBeNull();
    });
  });
});
