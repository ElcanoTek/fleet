import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import AdminChatsPage, { formatRunTime, lineDiff, type AdminChat, type AdminPatrol } from "./page";
import { toRows } from "./AdminTranscript";
import { mergeLatest } from "./ChatDetail";
import { attentionLabel, initials, type AdminNote, type ChatOverview } from "./api";
import { jsonResponse } from "@/test/fetch";

// Settings → Admin → Chats (ADR-0115, ADR-0117): the overview that says who
// needs help (counts, people, filters), one chat opened from it (the
// conversation as its owner sees it, the pinned note, a message followed to
// the turn's end), and the patrol, settings and delete panels with the
// compare-and-swap values the page read.

// A stand-in for the in-house router: the query string is the page's only
// navigation state (?chat=<id>).
const nav = vi.hoisted(() => {
  let search = "";
  const listeners = new Set<() => void>();
  return {
    get: () => search,
    set: (next: string) => {
      search = next;
      for (const l of listeners) l();
    },
    subscribe: (l: () => void) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
  };
});

vi.mock("@/app/lib/navigation", async () => {
  const { useSyncExternalStore } = await import("react");
  const router = {
    push: (href: string) => nav.set(href.includes("?") ? href.slice(href.indexOf("?")) : ""),
    replace: vi.fn(),
  };
  return {
    useRouter: () => router,
    useSearch: () => useSyncExternalStore(nav.subscribe, nav.get, nav.get),
  };
});

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

const OVERVIEW: ChatOverview = {
  chats: 1,
  attention: 0,
  running: 0,
  patrols: 1,
  noted: 0,
  cards_waiting: 0,
  archived: 2,
  deleted: 1,
  people: [{ email: CHAT.owner_email, chats: 1, attention: 0, running: 0, patrols: 1, noted: 0, cards_waiting: 0, last_active: NOW - 60 }],
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
  def_version: 4,
};

type Call = { url: string; method: string; body?: unknown };
let calls: Call[];
let requestPolls: unknown[];
let note: AdminNote | null;
let detailChat: AdminChat;
let purged: boolean;

beforeEach(() => {
  calls = [];
  requestPolls = [];
  note = null;
  detailChat = CHAT;
  purged = false;
  nav.set("");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      if (url.startsWith("/api/admin/chat-overview")) return jsonResponse({ overview: OVERVIEW });
      if (url.startsWith("/api/admin/chats?")) return jsonResponse({ chats: [CHAT] });
      if (url.endsWith("/note") && method === "PUT") {
        note = { text: (JSON.parse(String(init?.body)) as { text: string }).text, author: "admin@x", via: "web", created_at: NOW, updated_at: NOW };
        return jsonResponse({ note });
      }
      if (url.endsWith("/note") && method === "DELETE") {
        note = null;
        return jsonResponse(null, 204);
      }
      if (purged && url.startsWith(`/api/admin/chats/${CHAT.id}`) && method === "GET") return new Response("not found", { status: 404 });
      if (url.includes("/history")) {
        return jsonResponse({
          entries: [
            { id: 1, created_at: NOW - 100, role: "user", type: "text", text: "what did we do on Acme?" },
            { id: 2, created_at: NOW - 90, role: "user", type: "text", kind: "admin_message", sender: "robbie@ops", text: "Publish the log." },
          ],
        });
      }
      if (url === `/api/admin/chats/${CHAT.id}` && method === "GET") {
        return jsonResponse({
          chat: detailChat,
          patrols: [PATROL],
          queue: [],
          actions: [{ id: 7, created_at: NOW - 30, actor: "robbie@ops", via: "cli", action: "request_message", reason: "seed the shared log" }],
          pending_cards:
            detailChat.pending_approvals > 0
              ? [{ id: "ap-1", tool: "send_email", for_email: CHAT.owner_email, created_at: NOW - 3 * 3600, expires_at: NOW + 3600 }]
              : [],
          note,
        });
      }
      if (url.endsWith("/requests") && method === "POST") {
        if ((JSON.parse(String(init?.body)) as { kind: string }).kind === "purge") purged = true;
        return jsonResponse({ request: { id: "req-1", kind: "message", state: "done", input_state: "queued" } }, 202);
      }
      if (url.startsWith("/api/admin/chat-requests/")) {
        return jsonResponse({ request: requestPolls.shift() ?? { id: "req-1", kind: "message", state: "done", input_state: "completed" } });
      }
      if (url === `/api/admin/chats/${CHAT.id}` && method === "PATCH") {
        return jsonResponse({ chat: CHAT });
      }
      if (url.startsWith("/api/admin/patrols/") && method === "PATCH") {
        const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {};
        if (typeof body.cron === "string" && body.cron.split(" ").length !== 5) {
          return new Response("invalid chat schedule: invalid cron expression", { status: 400 });
        }
        if (body.dry_run) {
          const cron = body.cron as string;
          return jsonResponse({
            patrol: PATROL,
            preview: { ...PATROL, cron, timezone: body.timezone },
            changed: cron !== PATROL.cron || body.timezone !== PATROL.timezone,
            // Sat 2026-11-21 and Sun 2026-11-22, 06:30 in New York (EST).
            next_runs: [1795779000, 1795865400],
          });
        }
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

async function openChat(tab?: "patrols" | "settings" | "activity") {
  render(<AdminChatsPage />);
  // One person in view: their chats are listed without a click.
  const row = await screen.findByRole("button", { name: /Pre-Loaded Opti Workbench/ });
  fireEvent.click(row);
  await screen.findByTestId("admin-chat-panel");
  if (tab) fireEvent.click(screen.getByTestId(`admin-tab-${tab}`));
}

describe("overview", () => {
  it("counts the workspace's chats and groups them by person", async () => {
    render(<AdminChatsPage />);
    const person = await screen.findByTestId("admin-person");
    expect(person.textContent).toContain("owner@client.example");
    expect(person.textContent).toContain("1 with patrols");
    expect(screen.getByTestId("tile-patrols").textContent).toContain("1");
    expect(calls.some((c) => c.url.startsWith("/api/admin/chat-overview") && c.url.includes("active_days=30"))).toBe(true);
    // Their chats are read with the owner as the filter.
    await screen.findByTestId("admin-chat-row");
    expect(calls.some((c) => c.url.startsWith("/api/admin/chats?") && c.url.includes("owner=owner%40client.example"))).toBe(true);
  });

  it("narrows to a state from a tile, and to the recycle bin from its chip", async () => {
    render(<AdminChatsPage />);
    await screen.findByTestId("admin-person");
    const tile = screen.getByTestId("tile-patrols");
    fireEvent.click(tile);
    expect(tile.getAttribute("aria-pressed")).toBe("true");
    await waitFor(() => expect(calls.some((c) => c.url.startsWith("/api/admin/chats?") && c.url.includes("state=patrols"))).toBe(true));
    // A state nobody is in says so instead of listing nothing.
    fireEvent.click(screen.getByTestId("tile-attention"));
    expect(await screen.findByText(/Nobody is stuck/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Recycle bin/ }));
    await waitFor(() => expect(calls.some((c) => c.url.startsWith("/api/admin/chats?") && c.url.includes("deleted=1"))).toBe(true));
    // The bin is a flat list: grouping by person is not offered there.
    expect(screen.queryByRole("group", { name: "Group by" })).toBeNull();
  });

  it("keeps its filters behind an open chat", async () => {
    await openChat();
    fireEvent.click(screen.getByTestId("admin-chat-back"));
    await waitFor(() => expect(screen.queryByTestId("admin-chat-panel")).toBeNull());
    expect(screen.getByTestId("admin-person")).toBeTruthy();
  });
});

describe("a chat", () => {
  it("shows the conversation as its owner sees it, read-only", async () => {
    await openChat();
    const transcript = await screen.findByTestId("admin-chat-transcript");
    expect(transcript.textContent).toContain("what did we do on Acme?");
    // An administrator's message is labelled as theirs, as in the chat.
    expect(screen.getByTestId("admin-message-notice").textContent).toContain("robbie@ops");
    expect(screen.getByText(/As owner@client.example sees it · read-only/)).toBeTruthy();
    // Entries are asked for whole enough to read, and can include tool calls.
    expect(calls.some((c) => c.url.includes("/history?") && c.url.includes("max_chars=20000") && !c.url.includes("tools=1"))).toBe(true);
    fireEvent.click(screen.getByTestId("admin-chat-tools"));
    await waitFor(() => expect(calls.some((c) => c.url.includes("/history?") && c.url.includes("tools=1"))).toBe(true));
  });

  it("says what the owner is stuck behind", async () => {
    detailChat = { ...CHAT, pending_approvals: 1, waiting_since: NOW - 3 * 3600, attention: ["cards_waiting"] };
    await openChat();
    const callout = await screen.findByTestId("admin-chat-attention");
    expect(callout.textContent).toContain("Waiting on owner@client.example");
    expect(callout.textContent).toContain("send_email");
    expect(callout.textContent).toContain("waiting 3h");
  });

  it("pins a note, shows it unacknowledged, and unpins it on a second click", async () => {
    await openChat();
    const pin = screen.getByTestId("admin-note-pin") as HTMLButtonElement;
    expect(pin.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-note-text"), { target: { value: "  Approve the card under the last reply.  " } });
    expect(pin.disabled).toBe(false);
    fireEvent.click(pin);
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const put = calls.find((c) => c.method === "PUT");
    expect(put?.url).toBe(`/api/admin/chats/${CHAT.id}/note`);
    expect(put?.body).toEqual({ text: "Approve the card under the last reply." });
    const card = await screen.findByTestId("admin-pinned-note");
    expect(card.textContent).toContain("Approve the card under the last reply.");
    expect(screen.getByTestId("admin-note-unseen")).toBeTruthy();

    const unpin = await screen.findByTestId("admin-note-unpin");
    fireEvent.click(unpin);
    expect(calls.some((c) => c.method === "DELETE")).toBe(false); // armed, not fired
    fireEvent.click(unpin);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/note"))).toBe(true));
    await waitFor(() => expect(screen.queryByTestId("admin-pinned-note")).toBeNull());
  });

  it("shows who acknowledged the note", async () => {
    note = { text: "Welcome.", author: "admin@x", via: "web", created_at: NOW - 600, updated_at: NOW - 600, seen_at: NOW - 60, seen_by: "owner@client.example" };
    await openChat();
    expect((await screen.findByTestId("admin-note-seen")).textContent).toContain("Seen by owner@client.example");
  });

  it("sends a message with a reason and follows it to the end of the turn", async () => {
    await openChat();
    fireEvent.click(screen.getByRole("button", { name: "Message the assistant" }));
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

  it("takes no administrator message in a Lockdown chat, and says what to do instead", async () => {
    detailChat = { ...CHAT, lockdown: true };
    await openChat();
    fireEvent.click(screen.getByRole("button", { name: "Message the assistant" }));
    expect(screen.getByTestId("step-in-message-off").textContent).toContain("Pin a note");
  });

  it("offers Stop only while there is work to stop, and asks for a reason", async () => {
    await openChat();
    expect(screen.queryByTestId("admin-chat-stop")).toBeNull();
    cleanup();
    nav.set("");
    detailChat = { ...CHAT, turn_running: true };
    await openChat();
    fireEvent.click(await screen.findByTestId("admin-chat-stop"));
    const stop = screen.getByRole("button", { name: "Stop now" }) as HTMLButtonElement;
    expect(stop.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-stop-reason"), { target: { value: "runaway loop" } });
    fireEvent.click(stop);
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/requests"))).toBe(true));
    expect(calls.find((c) => c.url.endsWith("/requests"))?.body).toEqual({ kind: "stop", reason: "runaway loop" });
  });

  it("lists what administrators have done to it", async () => {
    await openChat("activity");
    const list = await screen.findByTestId("admin-chat-activity");
    expect(list.textContent).toContain("Sent a message");
    expect(list.textContent).toContain("robbie@ops · from the command line");
    expect(list.textContent).toContain("Reason: seed the shared log");
  });
});

describe("patrol instructions", () => {
  it("saves patrol instructions with the sha256 it read, and shows the diff first", async () => {
    await openChat("patrols");
    fireEvent.click(await screen.findByRole("button", { name: "Edit instructions" }));
    const box = screen.getByTestId("admin-patrol-prompt");
    fireEvent.change(box, { target: { value: "13. LOGGING\n- Append to the shared team log." } });
    const diff = screen.getByTestId("admin-patrol-diff");
    expect(diff.textContent).toContain("- - Offer a memory card.");
    expect(diff.textContent).toContain("+ - Append to the shared team log.");
    const save = screen.getByRole("button", { name: "Save instructions" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-patrol-reason"), { target: { value: "log moved" } });
    fireEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    const patch = calls.find((c) => c.method === "PATCH");
    expect(patch?.url).toBe(`/api/admin/patrols/${PATROL.id}`);
    expect(patch?.body).toEqual({ prompt: "13. LOGGING\n- Append to the shared team log.", expect_sha256: "abc123", reason: "log moved" });
  });
});

describe("patrol schedule", () => {
  it("previews the next runs in the patrol's timezone and saves with the def_version it read", async () => {
    await openChat("patrols");
    fireEvent.click(await screen.findByRole("button", { name: "Change schedule" }));
    const cron = screen.getByTestId("admin-patrol-cron") as HTMLInputElement;
    expect(cron.value).toBe("30 6 * * 1-5");
    expect((screen.getByTestId("admin-patrol-timezone") as HTMLSelectElement).value).toBe("America/New_York");
    // The current schedule previews as no change.
    expect(await screen.findByText("No changes yet. Next runs:")).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save schedule" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    // An invalid cron is refused by the server's check, and cannot be saved.
    fireEvent.change(cron, { target: { value: "30 6 * *" } });
    expect(await screen.findByText(/invalid cron expression/)).toBeTruthy();
    expect(save.disabled).toBe(true);

    fireEvent.change(cron, { target: { value: "30 6 * * *" } });
    const preview = await screen.findByTestId("admin-patrol-schedule-preview");
    await waitFor(() => expect(preview.textContent).toContain("Daily 06:30"));
    expect(preview.textContent).toContain(formatRunTime(1795779000, "America/New_York"));
    expect(formatRunTime(1795779000, "America/New_York")).toMatch(/06:30/);
    const dry = calls.filter((c) => c.method === "PATCH" && (c.body as { dry_run?: boolean }).dry_run);
    expect(dry.at(-1)?.body).toEqual({ cron: "30 6 * * *", timezone: "America/New_York", expect_def_version: 4, dry_run: true });

    expect(save.disabled).toBe(true); // no reason yet
    fireEvent.change(screen.getByTestId("admin-patrol-reason"), { target: { value: "the team works weekends" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && !(c.body as { dry_run?: boolean }).dry_run)).toBe(true));
    const patch = calls.find((c) => c.method === "PATCH" && !(c.body as { dry_run?: boolean }).dry_run);
    expect(patch?.url).toBe(`/api/admin/patrols/${PATROL.id}`);
    expect(patch?.body).toEqual({ cron: "30 6 * * *", timezone: "America/New_York", expect_def_version: 4, reason: "the team works weekends" });
    expect(await screen.findByText("Schedule changed and noted in the chat.")).toBeTruthy();
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
    await openChat("settings");
    const save = (await screen.findByRole("button", { name: "Save settings" })) as HTMLButtonElement;
    expect(save.disabled).toBe(true); // nothing changed
    fireEvent.change(screen.getByTestId("admin-chat-model"), { target: { value: "provider/other-model" } });
    expect(save.disabled).toBe(true); // no reason yet
    fireEvent.change(screen.getByTestId("admin-settings-reason"), { target: { value: "client asked" } });
    fireEvent.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.url === `/api/admin/chats/${CHAT.id}`)).toBe(true));
    const patch = calls.find((c) => c.method === "PATCH");
    expect(patch?.body).toEqual({ reason: "client asked", model: "provider/other-model" });
  });

  it("archives with a reason", async () => {
    await openChat("settings");
    fireEvent.change(await screen.findByTestId("admin-delete-reason"), { target: { value: "project ended" } });
    fireEvent.click(screen.getByTestId("admin-archive"));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ archived: true, reason: "project ended" });
  });

  it("transfers to a new owner with a reason", async () => {
    await openChat("settings");
    const transfer = (await screen.findByRole("button", { name: "Transfer" })) as HTMLButtonElement;
    fireEvent.change(screen.getByTestId("admin-transfer-email"), { target: { value: "new@client.example" } });
    expect(transfer.disabled).toBe(true);
    fireEvent.change(screen.getByTestId("admin-transfer-reason"), { target: { value: "left the team" } });
    fireEvent.click(transfer);
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/requests"))).toBe(true));
    expect(calls.find((c) => c.url.endsWith("/requests"))?.body).toEqual({ kind: "transfer", to_email: "new@client.example", reason: "left the team" });
  });
});

describe("delete", () => {
  it("moves a chat to the recycle bin, and only purges after the title is typed", async () => {
    await openChat("settings");
    fireEvent.change(await screen.findByTestId("admin-delete-reason"), { target: { value: "forked by mistake" } });
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
    // The chat is gone: the view says so instead of showing a chat that no
    // longer exists, and offers the way back.
    expect((await screen.findByTestId("admin-chat-gone")).textContent).toContain("no longer exists");
    expect(screen.queryByTestId("admin-chat-panel")).toBeNull();
    fireEvent.click(screen.getByTestId("admin-chat-back"));
    await waitFor(() => expect(screen.queryByTestId("admin-chat-gone")).toBeNull());
  });
});

describe("helpers", () => {
  it("shapes stored entries into what the chat draws", () => {
    const rows = toRows([
      { id: 1, created_at: 1, role: "user", type: "text", text: "hi" },
      { id: 2, created_at: 2, role: "assistant", type: "text", text: "Hello " },
      { id: 3, created_at: 3, role: "assistant", type: "text", text: "there." },
      { id: 4, created_at: 4, role: "assistant", type: "tool_call", tool: "run_python", text: "{}" },
      { id: 5, created_at: 5, role: "assistant", type: "text", text: "Done." },
      { id: 6, created_at: 6, role: "user", type: "text", kind: "chat_patrol", text: "[Scheduled Patrol]" },
      { id: 7, created_at: 7, role: "assistant", type: "notice", text: "Heads up." },
    ]);
    expect(rows.map((r) => r.kind)).toEqual(["user", "assistant", "tool", "assistant", "patrol", "notice"]);
    // One reply stored in two pieces reads as one.
    expect(rows[1]).toMatchObject({ kind: "assistant", text: "Hello there." });
  });

  it("joins a refreshed latest page onto earlier pages, and drops them rather than show a hole", () => {
    const e = (id: number, text = String(id)) => ({ id, created_at: id, role: "user", type: "text", text });
    const ids = (r: { entries: { id: number }[] }) => r.entries.map((x) => x.id);
    // The fresh page overlaps what is shown: it replaces the tail.
    const joined = mergeLatest([e(1), e(2), e(3), e(4)], [e(3, "edited"), e(4), e(5)]);
    expect(ids(joined)).toEqual([1, 2, 3, 4, 5]);
    expect(joined.joined).toBe(true);
    expect(joined.entries[2].text).toBe("edited");
    // An entry removed since the last read does not linger.
    expect(ids(mergeLatest([e(1), e(2), e(3)], [e(2), e(4)]))).toEqual([1, 2, 4]);
    // More than a page arrived between reads: 5..9 were never read, so the
    // earlier pages are dropped, not shown with 4 followed by 10.
    const hole = mergeLatest([e(1), e(2), e(3), e(4)], [e(10), e(11)]);
    expect(ids(hole)).toEqual([10, 11]);
    expect(hole.joined).toBe(false);
    // The first read.
    expect(mergeLatest([], [e(1)])).toEqual({ entries: [e(1)], joined: true });
  });

  it("names why a chat needs attention, and an avatar's letters", () => {
    const now = 10_000;
    expect(attentionLabel({ ...CHAT, attention: ["cards_waiting", "turn_failed"], pending_approvals: 2, waiting_since: now - 7200 }, now)).toEqual([
      "2 cards waiting 2h",
      "last turn failed",
    ]);
    expect(initials("dana.whitfield@northwind.example")).toBe("DW");
    expect(initials("li@x")).toBe("LI");
  });
});
