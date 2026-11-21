import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ConversationSummary } from "@/app/chat/ui/chat-experience";
import type { Project } from "@/app/chat/ui/ProjectsModal";
import { rememberSidebarLibrary, resetSidebarLibraryForTests, sidebarListsChanged, useSidebarLibrary } from "./useSidebarLibrary";

// What the sidebar lists on a page that is not Chat (docs/SIDEBAR.md): the
// same lists, read from the same endpoints, with the few changes that are
// one request made in place.

const me = vi.hoisted(() => ({ role: "member" as string, admin: false }));
vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: me.role, team_id: "", admin: me.admin }),
}));

const conv = (over: Partial<ConversationSummary>): ConversationSummary => ({
  id: "c",
  title: "Untitled",
  persona: "default",
  model: "",
  pinned: false,
  updated_at: 1_700_000_000,
  ...over,
});
const project = (over: Partial<Project>): Project => ({
  id: "p",
  owner_email: "me@example.com",
  name: "Project",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
  ...over,
});

type Call = { method: string; url: string; body?: unknown };

function stubApi(overrides: Record<string, (call: Call) => Response | undefined> = {}) {
  const calls: Call[] = [];
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const call: Call = {
        method: init?.method ?? "GET",
        url: String(input),
        body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      };
      calls.push(call);
      for (const [key, answer] of Object.entries(overrides)) {
        const [method, url] = key.split(" ");
        if (call.method === method && call.url === url) {
          const res = answer(call);
          if (res) return res;
        }
      }
      if (call.method !== "GET") return json({});
      switch (call.url) {
        case "/api/conversations":
          return json({ conversations: [conv({ id: "own-1", title: "Mine" }), conv({ id: "own-2", title: "Other", updated_at: 5 })] });
        case "/api/conversations?archived=true":
          return json({ conversations: [conv({ id: "arch-1", title: "Old", archived_at: 9 })] });
        case "/api/conversations?scope=shared":
          return json({
            conversations: [
              conv({ id: "team-1", title: "Theirs", user_email: "dana@example.com", team_visible: true }),
              // The shared scope can list the caller's own shared chat: it is in Chats already.
              conv({ id: "own-1", title: "Mine", user_email: "Me@Example.com", team_visible: true }),
            ],
          });
        case "/api/projects":
          return json({ projects: [project({ id: "p-1", name: "Playbook" })] });
        case "/api/conversations/order":
          return json({ orders: { pinned: ["own-1"] } });
        case "/api/server-config":
          return json({ lockdown_available: true, conversation_ttl_days: 30, conversation_expiry_warning_days: 0, conversation_unpinned_cap: -1 });
        default:
          return json({}, 404);
      }
    }),
  );
  return calls;
}

beforeEach(() => {
  me.role = "member";
  me.admin = false;
  resetSidebarLibraryForTests();
  window.localStorage.clear();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("useSidebarLibrary", () => {
  it("reads the sidebar's lists once, and leaves the caller's own chats out of the teammates' list", async () => {
    const calls = stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    expect(result.current.loading).toBe(true);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.conversations.map((c) => c.id)).toEqual(["own-1", "own-2"]);
    // The archived list follows the lists the sidebar draws first.
    await waitFor(() => expect(result.current.archivedLoaded).toBe(true));
    expect(result.current.archivedConversations.map((c) => c.id)).toEqual(["arch-1"]);
    expect(result.current.teamSharedConversations.map((c) => c.id)).toEqual(["team-1"]);
    expect(result.current.projects.map((p) => p.id)).toEqual(["p-1"]);
    expect(result.current.chatOrders).toEqual({ pinned: ["own-1"] });
    expect(result.current.projectsLoaded && result.current.teamSharedLoaded && result.current.archivedLoaded).toBe(true);
    // Retention numbers: a zero-day warning is a real answer, a non-positive cap is "none".
    expect(result.current.serverConfig).toMatchObject({
      lockdownAvailable: true,
      conversationTtlDays: 30,
      conversationExpiryWarningDays: 0,
      conversationUnpinnedCap: undefined,
    });
    expect(calls.filter((c) => c.method === "GET").map((c) => c.url).sort()).toEqual([
      "/api/conversations",
      "/api/conversations/order",
      "/api/conversations?archived=true",
      "/api/conversations?scope=shared",
      "/api/projects",
      "/api/server-config",
    ]);
  });

  it("starts from what the last page showed, so moving between pages never flashes an empty sidebar", async () => {
    stubApi();
    rememberSidebarLibrary({ conversations: [conv({ id: "seen", title: "Seen before" })], projects: [project({ id: "p-0" })] });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    expect(result.current.loading).toBe(false);
    expect(result.current.conversations.map((c) => c.id)).toEqual(["seen"]);
    expect(result.current.projectsLoaded).toBe(true);
    // …and is replaced by the fresh read.
    await waitFor(() => expect(result.current.conversations.map((c) => c.id)).toEqual(["own-1", "own-2"]));
  });

  it("a list that could not be read keeps what was there", async () => {
    stubApi({
      "GET /api/conversations": () => new Response("{}", { status: 500 }),
      "GET /api/projects": () => new Response("{}", { status: 500 }),
    });
    rememberSidebarLibrary({ conversations: [conv({ id: "seen" })], projects: [project({ id: "p-0" })] });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.teamSharedLoaded).toBe(true));
    expect(result.current.conversations.map((c) => c.id)).toEqual(["seen"]);
    expect(result.current.projects.map((p) => p.id)).toEqual(["p-0"]);
  });

  it("pins in place: at once on screen, one request to the server", async () => {
    const calls = stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    let ok = false;
    await act(async () => {
      ok = await result.current.togglePin(result.current.conversations[1]);
    });
    expect(ok).toBe(true);
    expect(result.current.conversations.find((c) => c.id === "own-2")?.pinned).toBe(true);
    // A pin counts as activity: the chat moves to the top of the recent order.
    expect(result.current.conversations[0].id).toBe("own-2");
    expect(calls.find((c) => c.method === "POST")).toEqual({ method: "POST", url: "/api/conversations/own-2/pin", body: { pinned: true } });
  });

  it("a pin the server refuses is said, and the lists are read again", async () => {
    stubApi({ "POST /api/conversations/own-2/pin": () => new Response("{}", { status: 500 }) });
    const onError = vi.fn();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", onError));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      expect(await result.current.togglePin(result.current.conversations[1])).toBe(false);
    });
    expect(onError).toHaveBeenCalledWith("Couldn’t pin “Other”. Try again.");
    expect(result.current.conversations.find((c) => c.id === "own-2")?.pinned).toBe(false);
  });

  it("reads the archived list once per page, after the others, and not again after a change", async () => {
    const calls = stubApi({ "POST /api/conversations/own-2/pin": () => new Response("{}", { status: 500 }) });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.archivedLoaded).toBe(true));
    const gets = () => calls.filter((c) => c.method === "GET").map((c) => c.url);
    expect(gets().indexOf("/api/conversations?archived=true")).toBe(gets().length - 1);
    // A refused pin reads the lists again; the archived one is not among them.
    await act(async () => {
      await result.current.togglePin(result.current.conversations[1]);
    });
    await waitFor(() => expect(gets().filter((u) => u === "/api/conversations")).toHaveLength(2));
    expect(gets().filter((u) => u === "/api/conversations?archived=true")).toHaveLength(1);
    expect(result.current.archivedConversations.map((c) => c.id)).toEqual(["arch-1"]);
  });

  it("reads its chats again when something outside the sidebar made one (the patrol wizard)", async () => {
    const calls = stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    const reads = () => calls.filter((c) => c.method === "GET" && c.url === "/api/conversations").length;
    expect(reads()).toBe(1);
    act(() => sidebarListsChanged());
    await waitFor(() => expect(reads()).toBe(2));
  });

  it("a read started from outside (or on coming back to the tab) is not overwritten by an older one still out", async () => {
    let release: () => void = () => {};
    let n = 0;
    stubApi({
      "GET /api/conversations": () => {
        n++;
        return new Response(JSON.stringify({ conversations: [conv({ id: "patrol-chat", title: "New patrol" })] }), { status: 200 });
      },
    });
    // The first (pre-launch) read is slow and answers last.
    const base = vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      if (String(input) === "/api/conversations" && n === 0) {
        n++;
        await new Promise<void>((resolve) => (release = resolve));
        return new Response(JSON.stringify({ conversations: [conv({ id: "old", title: "Before" })] }), { status: 200 });
      }
      return base(input as string, init);
    });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(n).toBe(1));
    act(() => sidebarListsChanged());
    await waitFor(() => expect(result.current.conversations.map((c) => c.id)).toEqual(["patrol-chat"]));
    await act(async () => {
      release();
      await new Promise((r) => setTimeout(r, 0));
    });
    await waitFor(() => expect(result.current.conversations.map((c) => c.id)).toEqual(["patrol-chat"]));

    // Coming back to the tab reads again.
    const reads = n;
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(n).toBeGreaterThan(reads));
  });

  it("a read from outside waits for a change still being saved, then reads once it lands", async () => {
    let releasePin: () => void = () => {};
    const calls = stubApi({
      "POST /api/conversations/own-2/pin": () => undefined,
    });
    const base = vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      if (String(input) === "/api/conversations/own-2/pin") {
        calls.push({ method: "POST", url: String(input), body: undefined });
        await new Promise<void>((resolve) => (releasePin = resolve));
        return new Response("{}", { status: 200 });
      }
      return base(input as string, init);
    });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    const reads = () => calls.filter((c) => c.method === "GET" && c.url === "/api/conversations").length;
    let pinned: Promise<unknown> = Promise.resolve();
    act(() => {
      pinned = result.current.togglePin(result.current.conversations[1]);
    });
    act(() => sidebarListsChanged());
    // Nothing read while the pin is out: its lists would undo it on screen.
    await new Promise((r) => setTimeout(r, 20));
    expect(reads()).toBe(1);
    expect(result.current.conversations.find((c) => c.id === "own-2")?.pinned).toBe(true);
    await act(async () => {
      releasePin();
      await pinned;
    });
    await waitFor(() => expect(reads()).toBe(2));
  });

  it("a first read overtaken by a quick pin is read again, so no list is left unread", async () => {
    // The last page's chats are on screen while the first read is out.
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    let projectReads = 0;
    const calls = stubApi();
    const base = globalThis.fetch as unknown as (input: string, init?: RequestInit) => Promise<Response>;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: string, init?: RequestInit) => {
        if (String(input) === "/api/projects" && ++projectReads === 1) await held;
        return base(input, init);
      }),
    );
    rememberSidebarLibrary({ conversations: [conv({ id: "own-2", title: "Other" })] });
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(projectReads).toBe(1));
    await act(async () => {
      await result.current.togglePin(result.current.conversations[0]);
    });
    // The first read comes back carrying the state from before the pin: it
    // is dropped, and the lists are read again.
    await act(async () => {
      release();
    });
    await waitFor(() => expect(result.current.projects.map((p) => p.id)).toEqual(["p-1"]));
    expect(result.current.chatOrders).toEqual({ pinned: ["own-1"] });
    expect(projectReads).toBe(2);
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(1);
  });

  it("order saves go one at a time, and only the latest order waits behind a save that is out", async () => {
    const releases: (() => void)[] = [];
    const calls = stubApi();
    const base = globalThis.fetch as unknown as (input: string, init?: RequestInit) => Promise<Response>;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: string, init?: RequestInit) => {
        if (init?.method === "PUT") await new Promise<void>((resolve) => releases.push(resolve));
        return base(input, init);
      }),
    );
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => {
      result.current.reorderChats("pinned", ["a", "b", "c"]);
      result.current.reorderChats("pinned", ["b", "a", "c"]);
      result.current.reorderChats("pinned", ["b", "c", "a"]);
    });
    // The screen has the last move; one save is out.
    expect(result.current.chatOrders.pinned).toEqual(["b", "c", "a"]);
    await waitFor(() => expect(releases).toHaveLength(1));
    await act(async () => {
      releases[0]();
    });
    await waitFor(() => expect(releases).toHaveLength(2));
    await act(async () => {
      releases[1]();
    });
    const saved = () => calls.filter((c) => c.method === "PUT").map((c) => (c.body as { conversation_ids: string[] }).conversation_ids);
    await waitFor(() => expect(saved()).toEqual([["a", "b", "c"], ["b", "c", "a"]]));
    expect(result.current.chatOrders.pinned).toEqual(["b", "c", "a"]);
  });

  it("pinning a teammate's chat changes the teammates' list, not the caller's own", async () => {
    stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      await result.current.togglePin(result.current.teamSharedConversations[0]);
    });
    expect(result.current.teamSharedConversations[0].pinned).toBe(true);
    expect(result.current.conversations.map((c) => c.id)).toEqual(["own-1", "own-2"]);
  });

  it("renames, labels, reorders and pins a project with the same requests Chat sends", async () => {
    const calls = stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      expect(await result.current.renameConversation("own-1", "  Renamed  ")).toBe(true);
      // Nothing to rename to, and an id that is not a safe path segment: no request.
      expect(await result.current.renameConversation("own-1", "   ")).toBe(false);
      expect(await result.current.renameConversation("../x", "Name")).toBe(false);
      result.current.setConversationLabels("own-1", ["q4"]);
      result.current.reorderChats("pinned", ["own-2", "own-1"]);
      result.current.pinProject("p-1", true);
      result.current.renameProject("p-1", "  Runbooks ");
      result.current.renameProject("p-1", "  ");
    });
    await waitFor(() => expect(result.current.projects[0]).toMatchObject({ pinned: true, name: "Runbooks" }));
    expect(result.current.conversations.find((c) => c.id === "own-1")).toMatchObject({ title: "Renamed", labels: ["q4"] });
    expect(result.current.chatOrders.pinned).toEqual(["own-2", "own-1"]);
    expect(calls.filter((c) => c.method !== "GET")).toEqual([
      { method: "POST", url: "/api/conversations/own-1/rename", body: { title: "Renamed" } },
      { method: "PATCH", url: "/api/conversations/bulk", body: { conversation_ids: ["own-1"], changes: { labels: ["q4"] } } },
      { method: "PUT", url: "/api/conversations/order", body: { scope: "pinned", conversation_ids: ["own-2", "own-1"] } },
      { method: "PATCH", url: "/api/projects/p-1", body: { pinned: true } },
      { method: "PATCH", url: "/api/projects/p-1", body: { name: "Runbooks" } },
    ]);
  });

  it("offers writes only once the account is known not to be a read-only Viewer", async () => {
    stubApi();
    const member = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    // Before GET /api/me answers, the role is unknown: nothing is offered yet.
    expect(member.result.current.canWrite).toBe(false);
    await waitFor(() => expect(member.result.current.canWrite).toBe(true));
    member.unmount();
    me.role = "viewer";
    const viewer = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(viewer.result.current.loading).toBe(false));
    expect(viewer.result.current.canWrite).toBe(false);
  });

  it("filters by the search text and keeps the sort as this device's preference", async () => {
    stubApi();
    const { result } = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => result.current.setQuery("oth"));
    expect(result.current.filteredConversations.map((c) => c.id)).toEqual(["own-2"]);
    expect(result.current.recentSort).toBe("activity");
    act(() => result.current.setRecentSort("title"));
    const again = renderHook(() => useSidebarLibrary("me@example.com", () => {}));
    expect(again.result.current.recentSort).toBe("title");
  });
});
