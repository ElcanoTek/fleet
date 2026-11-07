import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary, type MCPServerInfo } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// The Tools picker's catalog loads race (#2025): opening chat A then chat B
// (or the new-chat preview landing after a conversation's catalog) used to
// let whichever response arrived LAST paint the picker. A response now applies
// only while it is the latest load and its conversation is still open. The
// picker's badge counts the connectors that are on, so each catalog below
// turns on a different number of them.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225600 },
  { id: "conv-b", title: "Beta chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225500 },
];

function catalog(on: number): { servers: MCPServerInfo[] } {
  return {
    servers: Array.from({ length: 4 }, (_, i) => ({
      name: `srv${i}`,
      description: "",
      tools: [],
      tool_count: 1,
      enabled: i < on,
    })),
  };
}

type Deferred = { url: string; resolve: (body: unknown) => void };

let held: Deferred[] = [];

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/mcp-servers" || /^\/api\/conversations\/conv-[ab]\/mcp-servers$/.test(url)) {
      return new Promise<Response>((resolve) => {
        held.push({ url, resolve: (body) => resolve(json(body)) });
      });
    }
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/conversations/conv-b") return json({ conversation: CONVS[1], history: [] });
    if (/\/api\/conversations\/conv-[ab]\/inflight$/.test(url)) return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
}

async function release(url: string, body: unknown) {
  await waitFor(() => expect(held.some((d) => d.url === url)).toBe(true));
  const i = held.findIndex((d) => d.url === url);
  const [d] = held.splice(i, 1);
  await act(async () => {
    d.resolve(body);
  });
}

const badge = () => screen.queryByTestId("chat-tools-badge")?.textContent ?? "0";
const headerTitle = () => screen.getByTitle("Click to rename");

beforeEach(() => {
  held = [];
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => ({
      matches: false,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
    })),
  );
  Element.prototype.scrollIntoView = vi.fn();
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  clearChatSession();
});

describe("Tools picker catalog loads", () => {
  it("keeps the open chat's catalog when an older chat's load lands after it", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await release("/api/mcp-servers", catalog(1));

    // A's catalog is still in flight when the user opens B.
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-a/mcp-servers")).toBe(true));
    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));

    await release("/api/conversations/conv-b/mcp-servers", catalog(3));
    await waitFor(() => expect(badge()).toBe("3"));

    // A answers last: it must not repaint B's picker.
    await release("/api/conversations/conv-a/mcp-servers", catalog(2));
    expect(badge()).toBe("3");
  });

  it("ignores the new-chat preview when it lands after the conversation's catalog", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));

    await release("/api/conversations/conv-a/mcp-servers", catalog(2));
    await waitFor(() => expect(badge()).toBe("2"));

    await release("/api/mcp-servers", catalog(4));
    expect(badge()).toBe("2");
  });
});

// Connector saves race (#2046): every POST carries the whole selection, so
// two saves in flight at once could land out of order and the older answer
// switched off a connector the user had just turned on. Saves now run one at
// a time per conversation, and only the newest one reconciles the picker. The
// mock answers each POST with exactly what it was sent, and the helper below
// always answers the NEWEST POST it can see first.
describe("Tools picker connector saves", () => {
  let posts: Array<{ body: { enabled_optional: string[] }; resolve: () => void }> = [];
  let catalogLoads = 0;
  // When set, conversation catalog loads after the first are held here.
  let heldCatalog: Array<() => void> | null = null;
  const checked = (name: string) => screen.getByRole("switch", { name }).getAttribute("aria-checked");

  function mockSaveBackend() {
    posts = [];
    catalogLoads = 0;
    heldCatalog = null;
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (init?.method === "POST" && url === "/api/conversations/conv-a/mcp-servers") {
          const body = JSON.parse(String(init.body)) as { enabled_optional: string[] };
          return new Promise<Response>((resolve) => {
            posts.push({ body, resolve: () => resolve(json({ enabled_optional: body.enabled_optional })) });
          });
        }
        if (url === "/api/conversations/conv-a/mcp-servers") {
          catalogLoads++;
          const hold = heldCatalog;
          if (hold && catalogLoads > 1) {
            return new Promise<Response>((resolve) => {
              hold.push(() => resolve(json(catalog(0))));
            });
          }
        }
        if (url === "/api/mcp-servers" || /\/mcp-servers$/.test(url)) return json(catalog(0));
        if (url === "/api/conversations") return json({ conversations: CONVS });
        if (url === "/api/conversations?archived=true") return json({ conversations: [] });
        if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
        if (/\/inflight$/.test(url)) return json({ inflight: false });
        return json({}, 404);
      }),
    );
  }

  async function answerNewestPost() {
    await waitFor(() => expect(posts.length).toBeGreaterThan(0));
    const p = posts.pop()!;
    await act(async () => {
      p.resolve();
    });
    return p.body.enabled_optional;
  }

  it("ends on the last selection asked for when two saves overlap", async () => {
    mockSaveBackend();
    const sent: string[][] = [];
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await waitFor(() => expect(catalogLoads).toBe(1));
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    // Opening the picker refreshes the catalog; let that land first.
    await waitFor(() => expect(catalogLoads).toBe(2));
    await act(async () => {});

    fireEvent.click(screen.getByRole("switch", { name: "srv0" }));
    fireEvent.click(screen.getByRole("switch", { name: "srv1" }));
    expect(checked("srv0")).toBe("true");
    expect(checked("srv1")).toBe("true");

    sent.push(await answerNewestPost());
    // Whichever save answered first, it must not switch srv1 back off.
    expect(checked("srv1")).toBe("true");
    sent.push(await answerNewestPost());
    await waitFor(() => expect(posts.length).toBe(0));

    // The last POST carried both connectors, and the picker shows both on.
    expect(sent[sent.length - 1]).toEqual(["srv0", "srv1"]);
    expect(checked("srv0")).toBe("true");
    expect(checked("srv1")).toBe("true");
  });

  it("a catalog reload that started before a toggle does not switch it back", async () => {
    mockSaveBackend();
    heldCatalog = [];
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await waitFor(() => expect(catalogLoads).toBe(1));

    // Opening the picker starts a reload, held while the user toggles.
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    await waitFor(() => expect(heldCatalog).toHaveLength(1));
    fireEvent.click(screen.getByRole("switch", { name: "srv0" }));
    expect(checked("srv0")).toBe("true");

    // The reload answers with the state from before the toggle: ignored.
    await act(async () => {
      heldCatalog![0]();
    });
    expect(checked("srv0")).toBe("true");

    expect(await answerNewestPost()).toEqual(["srv0"]);
    expect(checked("srv0")).toBe("true");
  });

  it("a seat change survives a catalog read that landed while its save was in flight", async () => {
    const seated = {
      servers: [
        {
          name: "gamma",
          display_name: "Gamma",
          description: "",
          tools: [],
          tool_count: 1,
          enabled: true,
          accounts: ["personal", "work"],
          default_account: "work",
          remote: true,
        },
      ],
    };
    let post: { body: { accounts: Record<string, string> }; resolve: () => void } | null = null;
    let catalogGets = 0;
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (init?.method === "POST" && url === "/api/conversations/conv-a/mcp-servers") {
          const body = JSON.parse(String(init.body)) as { enabled_optional: string[]; accounts: Record<string, string> };
          return new Promise<Response>((resolve) => {
            post = { body, resolve: () => resolve(json({ enabled_optional: body.enabled_optional, accounts: body.accounts })) };
          });
        }
        if (url === "/api/conversations/conv-a/mcp-servers") {
          catalogGets++;
          // Every read answers with the seat as persisted BEFORE the save.
          return json(seated);
        }
        if (url === "/api/mcp-servers" || /\/mcp-servers$/.test(url)) return json(seated);
        if (url === "/api/conversations") return json({ conversations: CONVS });
        if (url === "/api/conversations?archived=true") return json({ conversations: [] });
        if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
        if (/\/inflight$/.test(url)) return json({ inflight: false });
        return json({}, 404);
      }),
    );
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await waitFor(() => expect(catalogGets).toBe(1));
    const toolsButton = screen.getByRole("button", { name: "Tools" });
    fireEvent.click(toolsButton);
    await waitFor(() => expect(catalogGets).toBe(2));
    await act(async () => {});

    fireEvent.click(await screen.findByTestId("mcp-seat-gamma"));
    fireEvent.mouseDown(screen.getByRole("option", { name: /personal/ }));
    fireEvent.click(screen.getByRole("option", { name: /personal/ }));
    await waitFor(() => expect(post).not.toBeNull());
    expect(post!.body.accounts).toEqual({ gamma: "personal" });

    // Close and reopen the picker: its read lands with the old seat.
    fireEvent.click(toolsButton);
    fireEvent.click(toolsButton);
    await waitFor(() => expect(catalogGets).toBe(3));
    await waitFor(() => expect(screen.getByTestId("mcp-seat-gamma")).toHaveTextContent("work"));

    // The save answers: the seat it persisted is what the picker shows.
    await act(async () => {
      post!.resolve();
    });
    await waitFor(() => expect(screen.getByTestId("mcp-seat-gamma")).toHaveTextContent("personal"));
  });

  it("after a failed save the picker shows what the server holds, and each save sent only its own edit", async () => {
    // A stateful server: a POST it accepts replaces the selection; a failed
    // POST leaves it alone. Catalog reads report what it holds.
    let serverOn = new Set<string>();
    const sentBodies: string[][] = [];
    const queue: Array<{ body: string[]; answer: (ok: boolean) => void }> = [];
    let catalogReads = 0;
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    const serverCatalog = () => ({
      servers: Array.from({ length: 4 }, (_, i) => ({
        name: `srv${i}`,
        description: "",
        tools: [],
        tool_count: 1,
        enabled: serverOn.has(`srv${i}`),
      })),
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (init?.method === "POST" && url === "/api/conversations/conv-a/mcp-servers") {
          const body = (JSON.parse(String(init.body)) as { enabled_optional: string[] }).enabled_optional;
          sentBodies.push(body);
          return new Promise<Response>((resolve) => {
            queue.push({
              body,
              answer: (ok) => {
                if (!ok) return resolve(json({ error: "boom" }, 500));
                serverOn = new Set(body);
                resolve(json({ enabled_optional: body, accounts: {} }));
              },
            });
          });
        }
        if (url === "/api/conversations/conv-a/mcp-servers") {
          catalogReads++;
          return json(serverCatalog());
        }
        if (url === "/api/mcp-servers") return json(serverCatalog());
        if (url === "/api/conversations") return json({ conversations: CONVS });
        if (url === "/api/conversations?archived=true") return json({ conversations: [] });
        if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
        if (/\/inflight$/.test(url)) return json({ inflight: false });
        return json({}, 404);
      }),
    );
    const answerNext = async (ok: boolean) => {
      await waitFor(() => expect(queue.length).toBe(1));
      const q = queue.shift()!;
      await act(async () => {
        q.answer(ok);
      });
    };
    const checked = (name: string) => screen.getByRole("switch", { name }).getAttribute("aria-checked");

    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await waitFor(() => expect(catalogReads).toBe(1));
    fireEvent.click(screen.getByRole("button", { name: "Tools" }));
    await waitFor(() => expect(catalogReads).toBe(2));
    await act(async () => {});

    // Three quick edits; saves go one at a time.
    fireEvent.click(screen.getByRole("switch", { name: "srv0" }));
    fireEvent.click(screen.getByRole("switch", { name: "srv1" }));
    fireEvent.click(screen.getByRole("switch", { name: "srv2" }));

    await answerNext(true);
    await answerNext(true);
    await answerNext(false);
    await waitFor(() => expect(catalogReads).toBe(3));

    // Each request carried exactly its own edit on top of the one before.
    expect(sentBodies).toEqual([["srv0"], ["srv0", "srv1"], ["srv0", "srv1", "srv2"]]);
    // The picker shows the server's state: the third edit did not persist.
    await waitFor(() => expect(checked("srv2")).toBe("false"));
    expect(checked("srv0")).toBe("true");
    expect(checked("srv1")).toBe("true");
    expect(checked("srv3")).toBe("false");
    expect([...serverOn].sort()).toEqual(["srv0", "srv1"]);
  });
});
