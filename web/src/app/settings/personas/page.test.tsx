import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

const navigateMock = vi.fn();
vi.mock("@/app/lib/navigation", () => ({ navigate: (href: string) => navigateMock(href) }));

import PersonasPage from "./page";

// Settings → Personas (docs/USER-PERSONAS.md): drafts wait for approval, the
// editor saves the server's shape, Start patrol creates the chat + schedule
// and opens it, and the twin builder only prefills a chat — it never sends.

type Call = { url: string; method: string; body?: unknown };

function mockApi(personas: unknown[]) {
  const calls: Call[] = [];
  const fn = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (url === "/api/user-personas" && method === "GET") {
      return {
        ok: true,
        status: 200,
        json: async () => ({ personas, workspace_personas: ["assistant", "trader"], default: "assistant" }),
      };
    }
    if (url === "/api/conversations" && method === "POST") {
      return { ok: true, status: 200, json: async () => ({ id: "conv-1" }) };
    }
    if (url.endsWith("/schedules") && method === "POST") {
      return { ok: true, status: 200, json: async () => ({ schedule: { id: "s-1" } }) };
    }
    return { ok: true, status: 200, json: async () => ({}), text: async () => "" };
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

const twin = {
  id: "p1",
  name: "my-twin",
  ref: "user:my-twin",
  display_name: "My Twin",
  description: "Thinks like me",
  instructions: "Lead with the number.",
  base_persona: "trader",
  kind: "twin",
  autonomy: "act",
  status: "active",
  patrols: [{ title: "Morning sweep", cron: "0 9 * * 1-5", timezone: "UTC", prompt: "Check pacing." }],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  navigateMock.mockReset();
});

describe("PersonasPage", () => {
  it("lists personas with their kind, autonomy, base and patrol cadence", async () => {
    mockApi([twin]);
    render(<PersonasPage />);
    const row = await screen.findByTestId("persona-row-my-twin");
    expect(within(row).getByText("My Twin")).toBeInTheDocument();
    expect(within(row).getByText("Digital twin")).toBeInTheDocument();
    expect(within(row).getByText("Acts for you")).toBeInTheDocument();
    expect(within(row).getByText("Builds on Trader")).toBeInTheDocument();
    expect(within(row).getByText(/Weekdays at 9:00/)).toBeInTheDocument();
  });

  it("starts a patrol: new chat on the persona, schedule with auto-approve for an acting persona, first run, then opens it", async () => {
    const calls = mockApi([twin]);
    render(<PersonasPage />);
    const row = await screen.findByTestId("persona-row-my-twin");
    fireEvent.click(within(row).getByRole("button", { name: "Start patrol" }));
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith("/chat?c=conv-1"));
    const create = calls.find((c) => c.url === "/api/conversations");
    expect(create?.body).toMatchObject({ persona: "user:my-twin", title: "Morning sweep" });
    const sched = calls.find((c) => c.url === "/api/conversations/conv-1/schedules");
    expect(sched?.body).toMatchObject({ cron: "0 9 * * 1-5", prompt: "Check pacing.", auto_approve: true });
    expect(calls.some((c) => c.url === "/api/conversations/conv-1/schedules/s-1/run-now" && c.method === "POST")).toBe(true);
  });

  it("offers Approve (not Start chat or Start patrol) on a proposed draft", async () => {
    const calls = mockApi([{ ...twin, id: "p2", name: "my-twin-draft", status: "proposed", replaces_name: "my-twin" }]);
    render(<PersonasPage />);
    const row = await screen.findByTestId("persona-row-my-twin-draft");
    expect(within(row).getByText("Proposed edit to my-twin")).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "Start patrol" })).toBeNull();
    expect(within(row).queryByRole("button", { name: "Start chat" })).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(calls.some((c) => c.url === "/api/user-personas/p2/approve" && c.method === "POST")).toBe(true),
    );
  });

  it("creates a persona from a template, deriving the short name from the display name", async () => {
    const calls = mockApi([]);
    render(<PersonasPage />);
    fireEvent.click(await screen.findByTestId("new-persona"));
    const editor = screen.getByTestId("persona-editor");
    fireEvent.click(within(editor).getByRole("button", { name: "Patrol analyst" }));
    fireEvent.change(within(editor).getByPlaceholderText("My Twin"), { target: { value: "Deal Desk Watcher" } });
    expect((within(editor).getByPlaceholderText("my-twin") as HTMLInputElement).value).toBe("deal-desk-watcher");
    fireEvent.click(within(editor).getByRole("button", { name: "Create persona" }));
    await waitFor(() => expect(calls.some((c) => c.url === "/api/user-personas" && c.method === "POST")).toBe(true));
    const post = calls.find((c) => c.url === "/api/user-personas" && c.method === "POST");
    expect(post?.body).toMatchObject({ name: "deal-desk-watcher", display_name: "Deal Desk Watcher", kind: "assistant" });
    expect((post?.body as { patrols: unknown[] } | undefined)?.patrols).toHaveLength(1);
  });

  it("the twin builder opens a chat with the request in the composer", async () => {
    mockApi([]);
    render(<PersonasPage />);
    fireEvent.change(await screen.findByPlaceholderText(/I run programmatic deals/), {
      target: { value: "I manage retail deals." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Build with the assistant" }));
    expect(navigateMock).toHaveBeenCalledTimes(1);
    const href = String(navigateMock.mock.calls[0][0]);
    expect(href.startsWith("/chat?draft=")).toBe(true);
    const draft = decodeURIComponent(href.slice("/chat?draft=".length));
    expect(draft).toContain("I manage retail deals.");
    expect(draft).toContain('action "history"');
  });
});
