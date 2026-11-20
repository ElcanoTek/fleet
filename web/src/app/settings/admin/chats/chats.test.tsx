import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import AdminChatsPage, { lineDiff, type AdminChat, type AdminPatrol } from "./page";
import { jsonResponse } from "@/test/fetch";

// Settings → Admin → Chats (ADR-0115): list every chat, open one, send it a
// message (followed to the turn's end), and edit a patrol's instructions with
// the sha256 the page read (compare-and-swap).

vi.mock("@/app/lib/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

vi.mock("../../useIsAdmin", () => ({
  useIsAdmin: () => "admin",
  retryAdminProbe: () => Promise.resolve("admin"),
}));

const NOW = Math.floor(Date.now() / 1000);

const CHAT: AdminChat = {
  id: "dfe4adc7-6fd8-43cf-829c-0c723b2d42e6",
  title: "Pre-Loaded Opti Workbench",
  owner_email: "owner@client.example",
  model: "provider/model",
  approval_mode: "auto",
  persona: "victoria",
  lockdown: false,
  archived: false,
  multiplayer: false,
  team_shared: false,
  created_at: NOW - 86400,
  updated_at: NOW - 60,
  active_patrols: 1,
  pending_approvals: 0,
  queued_inputs: 0,
  turn_running: false,
};

const PATROL: AdminPatrol = {
  id: "5ee926c2-1f76-4221-b7a7-45a23b547742",
  title: "Morning sweep",
  prompt: "13. LOGGING\n- Offer a memory card.",
  prompt_sha256: "abc123",
  cron: "30 6 * * 1-5",
  timezone: "America/New_York",
  status: "active",
  next_run_at: NOW + 3600,
  user_email: "owner@client.example",
  fire_in_flight: false,
};

type Call = { url: string; method: string; body?: unknown };
let calls: Call[];
let requestPolls: unknown[];

beforeEach(() => {
  calls = [];
  requestPolls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      if (url.startsWith("/api/admin/chats?")) return jsonResponse({ chats: [CHAT] });
      if (url.includes("/history")) {
        return jsonResponse({
          entries: [
            { id: 1, created_at: NOW - 100, role: "user", type: "text", text: "what did we do on Acme?" },
            { id: 2, created_at: NOW - 90, role: "user", type: "text", kind: "admin_message", sender: "robbie@ops", text: "Publish the log." },
          ],
        });
      }
      if (url === `/api/admin/chats/${CHAT.id}` && method === "GET") {
        return jsonResponse({ chat: CHAT, patrols: [PATROL], queue: [], actions: [] });
      }
      if (url.endsWith("/requests") && method === "POST") {
        return jsonResponse({ request: { id: "req-1", kind: "message", state: "done", input_state: "queued" } }, 202);
      }
      if (url.startsWith("/api/admin/chat-requests/")) {
        return jsonResponse({ request: requestPolls.shift() ?? { id: "req-1", kind: "message", state: "done", input_state: "completed" } });
      }
      if (url === `/api/admin/chats/${CHAT.id}` && method === "PATCH") {
        return jsonResponse({ chat: CHAT });
      }
      if (url.startsWith("/api/admin/patrols/") && method === "PATCH") {
        return jsonResponse({ patrol: { ...PATROL } });
      }
      return new Response("not found", { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

async function openChat() {
  render(<AdminChatsPage />);
  const row = await screen.findByRole("button", { name: /Pre-Loaded Opti Workbench/ });
  fireEvent.click(row);
  await screen.findByTestId("admin-chat-panel");
}

describe("AdminChatsPage", () => {
  it("lists chats and opens one with its transcript and patrol", async () => {
    await openChat();
    expect(calls.some((c) => c.url.startsWith("/api/admin/chats?") && c.url.includes("active_days=30"))).toBe(true);
    const transcript = await screen.findByTestId("admin-chat-transcript");
    expect(transcript.textContent).toContain("Administrator · robbie@ops");
    expect(transcript.textContent).toContain("owner@client.example");
    expect(screen.getByText("Patrol · Morning sweep")).toBeTruthy();
  });

  it("sends a message with a reason and follows it to the end of the turn", async () => {
    await openChat();
    const send = screen.getByRole("button", { name: "Send message" }) as HTMLButtonElement;
    fireEvent.change(screen.getByTestId("admin-chat-message"), { target: { value: "Publish the team log." } });
    expect(send.disabled).toBe(true); // no reason yet
    fireEvent.change(screen.getByTestId("admin-chat-reason"), { target: { value: "seed the shared log" } });
    expect(send.disabled).toBe(false);
    requestPolls = [{ id: "req-1", kind: "message", state: "done", input_state: "running" }];
    fireEvent.click(send);
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/requests"))).toBe(true));
    const post = calls.find((c) => c.url.endsWith("/requests"));
    expect(post?.body).toEqual({ kind: "message", message: "Publish the team log.", reason: "seed the shared log" });
    expect(await screen.findByText(/Queued in the chat/)).toBeTruthy();
    expect(await screen.findByText(/The turn ended \(completed\)/, {}, { timeout: 6000 })).toBeTruthy();
  }, 10000);

  it("saves patrol instructions with the sha256 it read, and shows the diff first", async () => {
    await openChat();
    fireEvent.click(screen.getByRole("button", { name: "Edit instructions" }));
    const box = screen.getByTestId("admin-patrol-prompt");
    fireEvent.change(box, { target: { value: "13. LOGGING\n- Append to the shared team log." } });
    const diff = screen.getByTestId("admin-patrol-diff");
    expect(diff.textContent).toContain("- - Offer a memory card.");
    expect(diff.textContent).toContain("+ - Append to the shared team log.");
    const save = screen.getByRole("button", { name: "Save instructions" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    // Message, settings, patrol, delete: the patrol's reason is the third.
    const reasons = screen.getAllByLabelText("Reason (recorded and shown in the chat)");
    fireEvent.change(reasons[2], { target: { value: "log moved" } });
    fireEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    const patch = calls.find((c) => c.method === "PATCH");
    expect(patch?.url).toBe(`/api/admin/patrols/${PATROL.id}`);
    expect(patch?.body).toEqual({ prompt: "13. LOGGING\n- Append to the shared team log.", expect_sha256: "abc123", reason: "log moved" });
  });
});

describe("lineDiff", () => {
  it("prints removals before additions", () => {
    expect(lineDiff("a\nb\nc", "a\nB\nc")).toEqual([
      { kind: " ", text: "a" },
      { kind: "-", text: "b" },
      { kind: "+", text: "B" },
      { kind: " ", text: "c" },
    ]);
  });
});

describe("chat settings", () => {
  it("sends a changed model with the reason", async () => {
    await openChat();
    fireEvent.change(screen.getByTestId("admin-chat-model"), { target: { value: "provider/other-model" } });
    const reasons = screen.getAllByLabelText("Reason (recorded and shown in the chat)");
    fireEvent.change(reasons[1], { target: { value: "client asked" } });
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.url === `/api/admin/chats/${CHAT.id}`)).toBe(true));
    const patch = calls.find((c) => c.method === "PATCH");
    expect(patch?.body).toEqual({ reason: "client asked", model: "provider/other-model" });
  });
});

describe("delete", () => {
  it("moves a chat to the recycle bin, and only purges after the title is typed", async () => {
    await openChat();
    fireEvent.change(screen.getByTestId("admin-delete-reason"), { target: { value: "forked by mistake" } });
    fireEvent.click(screen.getByRole("button", { name: "Move to recycle bin" }));
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/requests"))).toBe(true));
    expect(calls.find((c) => c.url.endsWith("/requests"))?.body).toEqual({ kind: "delete", reason: "forked by mistake" });

    const purge = screen.getByRole("button", { name: "Delete permanently" }) as HTMLButtonElement;
    expect(purge.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-delete-confirm"), { target: { value: "wrong" } });
    expect(purge.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-delete-confirm"), { target: { value: CHAT.title } });
    expect(purge.disabled).toBe(false);
    fireEvent.click(purge);
    await waitFor(() => expect(calls.filter((c) => c.url.endsWith("/requests")).length).toBe(2));
    expect(calls.filter((c) => c.url.endsWith("/requests"))[1].body).toEqual({ kind: "purge", reason: "forked by mistake", confirm: CHAT.title });
  });
});
