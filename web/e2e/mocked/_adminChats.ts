import type { Page, Route } from "@playwright/test";
import { mockShellBasics } from "./_mocks";

// A small in-memory stand-in for the chat administration routes
// (/api/admin/chats, /api/admin/chat-overview, …/note, …/notice, …/requests),
// enough for Settings → Admin → Chats to be driven end to end: three people,
// one of them stuck behind an approval card, one with a failed turn, one with
// a running patrol. Changes a spec makes (pin a note, post a notice) are kept
// and show up in the next read, as they would against the server.

const NOW = Math.floor(Date.now() / 1000);

export const DANA = "dana.whitfield@northwind.example";
export const LI = "li.chen@northwind.example";
export const SAM = "sam.okafor@northwind.example";
export const ADMIN = "alex@northwind.example";

type Chat = Record<string, unknown> & { id: string; owner_email: string; title: string };

function chat(id: string, owner: string, title: string, extra: Record<string, unknown> = {}): Chat {
  return {
    id,
    title,
    owner_email: owner,
    model: "anthropic/claude-sonnet-5.5",
    approval_mode: "manual",
    persona: "default",
    lockdown: false,
    archived: false,
    multiplayer: false,
    team_shared: false,
    created_at: NOW - 9 * 86400,
    updated_at: NOW - 3600,
    active_patrols: 0,
    pending_approvals: 0,
    queued_inputs: 0,
    turn_running: false,
    has_note: false,
    ...extra,
  };
}

export const STUCK_ID = "7c1e5b0a-3f6d-4c1b-9d55-0a2f4e8b1c01";

export function seedChats(): Chat[] {
  return [
    chat(STUCK_ID, DANA, "Weekly client report — Northwind", {
      updated_at: NOW - 4 * 60,
      pending_approvals: 1,
      waiting_since: NOW - 3 * 3600,
      last_turn_status: "completed",
      attention: ["cards_waiting"],
      active_patrols: 1,
      approval_mode: "manual",
    }),
    chat("2a9d4c7e-8b13-4f60-a1c2-5d6e7f809a02", DANA, "Budget import from the finance sheet", {
      updated_at: NOW - 2 * 3600,
      last_turn_status: "error",
      attention: ["turn_failed"],
    }),
    chat("9f3b2a1c-4d5e-4f70-8a9b-0c1d2e3f4a03", DANA, "Scratch: wording for the renewal email", {
      updated_at: NOW - 26 * 3600,
      last_turn_status: "completed",
    }),
    chat("5b6c7d8e-9f0a-4b1c-8d2e-3f4a5b6c7d04", LI, "Pacing sweep — all campaigns", {
      updated_at: NOW - 40,
      turn_running: true,
      last_turn_status: "running",
      active_patrols: 2,
      approval_mode: "auto",
    }),
    chat("0d1e2f3a-4b5c-4d6e-9f70-8a9b0c1d2e05", LI, "Q4 media plan", {
      updated_at: NOW - 5 * 3600,
      last_turn_status: "completed",
      has_note: true,
      note_seen: true,
    }),
    chat("c3d4e5f6-a7b8-4c9d-8e0f-1a2b3c4d5e06", SAM, "Onboarding checklist", {
      updated_at: NOW - 3 * 86400,
      last_turn_status: "completed",
    }),
  ];
}

const TRANSCRIPT = [
  { id: 101, created_at: NOW - 4 * 3600, role: "user", type: "text", text: "Can you put together this week's client report for Northwind and email it to Priya?" },
  {
    id: 102,
    created_at: NOW - 4 * 3600 + 40,
    role: "assistant",
    type: "text",
    text:
      "Here is this week's summary for **Northwind**:\n\n| Channel | Spend | Pacing |\n|---|---|---|\n| Search | $18,420 | 98% |\n| Social | $9,310 | 104% |\n| Display | $4,075 | 91% |\n\n- Search is on plan; no changes needed.\n- Display is under-pacing — I'd raise the daily cap by about 8%.\n\nI've drafted the email to Priya. It needs your approval before it is sent.",
  },
  { id: 103, created_at: NOW - 3 * 3600 - 300, role: "user", type: "text", kind: "chat_patrol", text: "[Scheduled Patrol — Run #12: Monday report]\nBuild the weekly client report and email it to the account lead." },
  {
    id: 104,
    created_at: NOW - 3 * 3600,
    role: "assistant",
    type: "text",
    text: "The Monday run is ready. I'm waiting for you to approve the email to **priya@northwind.example** before I send it.",
  },
  { id: 105, created_at: NOW - 3000, role: "assistant", type: "notice", kind: "admin_updated", text: "An administrator changed the schedule of the patrol \"Monday report\" from 30 6 * * 1 to 0 7 * * 1. Reason: Dana asked for a later start." },
];

const PATROL = {
  id: "5ee926c2-1f76-4221-b7a7-45a23b547742",
  title: "Monday report",
  prompt: "Build the weekly client report for Northwind.\n- Pull spend and pacing for every channel.\n- Email the summary to the account lead.",
  prompt_sha256: "abc123",
  cron: "0 7 * * 1",
  timezone: "America/New_York",
  status: "active",
  next_run_at: NOW + 2 * 86400,
  user_email: DANA,
  fire_in_flight: false,
  def_version: 4,
};

export type AdminChatsMock = {
  /** Every request the page made to an admin chat route: method, path, body. */
  calls: Array<{ method: string; path: string; body?: unknown }>;
  note: () => Record<string, unknown> | null;
};

export async function mockAdminChats(page: Page): Promise<AdminChatsMock> {
  await mockShellBasics(page, { admin: true, email: ADMIN });
  const chats = seedChats();
  const transcript: Array<Record<string, unknown>> = [...TRANSCRIPT];
  const actions: Array<Record<string, unknown>> = [
    { id: 1, created_at: NOW - 3000, actor: ADMIN, via: "web", action: "patrol_schedule", reason: "Dana asked for a later start.", detail: 'schedule "30 6 * * 1" (America/New_York) -> "0 7 * * 1" (America/New_York)' },
    { id: 2, created_at: NOW - 2 * 86400, actor: "ops-agent@fleet-box", via: "cli", action: "request_message", reason: "seed the shared log", detail: "Publish the team log now." },
  ];
  let note: Record<string, unknown> | null = null;
  let nextId = 200;
  const calls: AdminChatsMock["calls"] = [];

  const matches = (c: Chat, q: URLSearchParams): boolean => {
    const state = q.get("state") ?? "";
    const owner = (q.get("owner") ?? "").toLowerCase();
    const text = (q.get("q") ?? "").toLowerCase();
    if (owner && c.owner_email.toLowerCase() !== owner) return false;
    if (text && !`${c.title} ${c.owner_email} ${c.id}`.toLowerCase().includes(text)) return false;
    if (state === "attention") return Array.isArray(c.attention) && c.attention.length > 0;
    if (state === "running") return c.turn_running === true;
    if (state === "patrols") return Number(c.active_patrols) > 0;
    if (state === "noted") return c.has_note === true;
    if (state === "archived") return c.archived === true;
    return true;
  };
  const counts = (list: Chat[]) => ({
    chats: list.length,
    attention: list.filter((c) => Array.isArray(c.attention) && c.attention.length > 0).length,
    running: list.filter((c) => c.turn_running).length,
    patrols: list.filter((c) => Number(c.active_patrols) > 0).length,
    noted: list.filter((c) => c.has_note).length,
    cards_waiting: list.reduce((n, c) => n + Number(c.pending_approvals), 0),
  });

  await page.route("**/api/admin/chat-overview**", (r: Route) => {
    const q = new URL(r.request().url()).searchParams;
    q.delete("state");
    const shown = chats.filter((c) => matches(c, q));
    const owners = [...new Set(shown.map((c) => c.owner_email))];
    const people = owners
      .map((email) => {
        const mine = shown.filter((c) => c.owner_email === email);
        return { email, ...counts(mine), last_active: Math.max(...mine.map((c) => Number(c.updated_at))) };
      })
      .sort((a, b) => b.attention - a.attention || b.last_active - a.last_active);
    return r.fulfill({ json: { overview: { ...counts(shown), archived: 2, deleted: 1, people } } });
  });

  await page.route("**/api/admin/chats?**", (r: Route) => {
    const q = new URL(r.request().url()).searchParams;
    if (q.get("deleted") === "1" || q.get("state") === "archived") return r.fulfill({ json: { chats: [] } });
    return r.fulfill({ json: { chats: chats.filter((c) => matches(c, q)) } });
  });

  await page.route("**/api/admin/chat-requests/**", (r: Route) =>
    r.fulfill({ json: { request: { id: "req-1", kind: "message", state: "done", input_state: "completed" } } }),
  );

  await page.route("**/api/admin/chats/*/**", (r: Route) => {
    const req = r.request();
    const url = new URL(req.url());
    const [, id, sub] = /\/api\/admin\/chats\/([^/]+)\/(.+)$/.exec(url.pathname) ?? [];
    const body = req.postData() ? (JSON.parse(req.postData() ?? "{}") as Record<string, unknown>) : undefined;
    calls.push({ method: req.method(), path: url.pathname, body });
    const target = chats.find((c) => c.id === id);
    if (sub === "history") {
      const withTools = url.searchParams.get("tools") === "1";
      const entries = withTools
        ? [
            ...transcript.slice(0, 3),
            { id: 1031, created_at: NOW - 3 * 3600 - 200, role: "assistant", type: "tool_call", tool: "send_email", text: '{"to":"priya@northwind.example","subject":"Northwind weekly report"}' },
            ...transcript.slice(3),
          ]
        : transcript;
      return r.fulfill({ json: { entries: id === STUCK_ID ? entries : [] } });
    }
    if (sub === "note" && req.method() === "PUT") {
      note = { text: String(body?.text ?? ""), author: ADMIN, via: "web", created_at: NOW, updated_at: Math.floor(Date.now() / 1000), version: nextId++ };
      if (target) Object.assign(target, { has_note: true, note_seen: false });
      actions.unshift({ id: nextId++, created_at: Math.floor(Date.now() / 1000), actor: ADMIN, via: "web", action: "note_set", detail: note.text });
      return r.fulfill({ json: { note } });
    }
    if (sub === "note" && req.method() === "DELETE") {
      note = null;
      if (target) Object.assign(target, { has_note: false, note_seen: false });
      return r.fulfill({ status: 204 });
    }
    if (sub === "notice" && req.method() === "POST") {
      transcript.push({ id: nextId++, created_at: Math.floor(Date.now() / 1000), role: "assistant", type: "notice", kind: "admin_updated", text: String(body?.text ?? "") });
      return r.fulfill({ status: 204 });
    }
    if (sub === "requests" && req.method() === "POST") {
      if (body?.kind === "message") {
        transcript.push({
          id: nextId++,
          created_at: Math.floor(Date.now() / 1000),
          role: "user",
          type: "text",
          kind: "admin_message",
          sender: ADMIN,
          text: String(body.message ?? ""),
        });
      }
      return r.fulfill({ status: 202, json: { request: { id: "req-1", kind: body?.kind, state: "done", input_state: "queued" } } });
    }
    return r.fulfill({ status: 404, body: "not found" });
  });

  await page.route("**/api/admin/chats/*", (r: Route) => {
    const req = r.request();
    const url = new URL(req.url());
    const id = url.pathname.split("/").pop() ?? "";
    const target = chats.find((c) => c.id === id);
    if (!target) return r.fulfill({ status: 404, body: "not found" });
    if (req.method() === "PATCH") {
      const body = JSON.parse(req.postData() ?? "{}") as Record<string, unknown>;
      calls.push({ method: "PATCH", path: url.pathname, body });
      if (typeof body.title === "string") target.title = body.title;
      return r.fulfill({ json: { chat: target } });
    }
    const stuck = id === STUCK_ID;
    return r.fulfill({
      json: {
        chat: target,
        patrols: stuck ? [PATROL] : [],
        queue: [],
        actions: stuck ? actions : [],
        pending_cards: stuck
          ? [{ id: "ap-1", tool: "send_email", mcp_server: "gmail", for_email: DANA, created_at: NOW - 3 * 3600, expires_at: NOW + 21 * 3600 }]
          : [],
        note: stuck ? note : null,
      },
    });
  });

  return { calls, note: () => note };
}
