import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { addShortcut, BUILTIN_LAYOUT, normalizeLayout } from "./layout";
import {
  getSidebarState,
  loadSidebar,
  resetSidebarLayout,
  resetSidebarStoreForTests,
  setSidebarLayout,
} from "./store";
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
  resetSidebarStoreForTests();
  window.localStorage.clear();
});
afterEach(() => {
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

  it("an unreachable server renders the built-in layout and says it cannot save", async () => {
    window.localStorage.setItem("fleet.sidebar.layout.v1", JSON.stringify(OWN));
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
    expect(calls.some((c) => c.method === "DELETE")).toBe(true);
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
});
