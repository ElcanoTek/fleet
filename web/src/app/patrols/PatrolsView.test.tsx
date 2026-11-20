import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { PatrolsView } from "./PatrolsView";
import { resetPatrolCacheForTests, type PatrolBrief } from "@/app/shared/sidebar/targets";

// The Patrols page (docs/SIDEBAR.md): the person's patrols and the ones
// teammates run in chats shared with them, steered and started from one
// place.

const navigate = vi.hoisted(() => vi.fn());
vi.mock("@/app/lib/navigation", async (original) => ({
  ...(await original<typeof import("@/app/lib/navigation")>()),
  navigate,
}));

const api = vi.hoisted(() => ({
  createSchedule: vi.fn(),
  deleteSchedule: vi.fn(),
  runScheduleNow: vi.fn(),
  setScheduleAction: vi.fn(),
  updateSchedule: vi.fn(),
}));
vi.mock("@/app/shared/lib/patrolApi", async (original) => ({
  ...(await original<typeof import("@/app/shared/lib/patrolApi")>()),
  ...api,
}));

const openPatrolWizard = vi.hoisted(() => vi.fn());
vi.mock("@/app/launchpad/patrolWizard", async (original) => ({
  ...(await original<typeof import("@/app/launchpad/patrolWizard")>()),
  openPatrolWizard,
}));

const NOW = Math.floor(Date.now() / 1000);

const patrol = (over: Partial<PatrolBrief>): PatrolBrief => ({
  id: "pt-1",
  conversation_id: "c-1",
  title: "Deal delivery check",
  conversation_title: "North Peak Deal Updates",
  cron: "0 */2 * * *",
  timezone: "America/Detroit",
  status: "active",
  next_run_at: NOW + 19 * 3600,
  run_count: 44,
  approval_mode: "auto",
  mine: true,
  ...over,
});

const MINE = patrol({});
const PAUSED = patrol({ id: "pt-2", conversation_id: "c-2", title: "Backfill scan", conversation_title: "Basis pacing", status: "paused", next_run_at: undefined });
const THEIRS = patrol({
  id: "pt-3",
  conversation_id: "t-1",
  title: "Hy-Vee deal insights",
  conversation_title: "Raptive Hy-Vee",
  mine: false,
  owner_email: "jeanne@example.com",
  team_visible: true,
});

type Call = { method: string; url: string; body?: unknown };
let calls: Call[] = [];
let list: PatrolBrief[] | "fail" = [];
let detail: Record<string, unknown> = {};
let createChat: () => Response = () => new Response(JSON.stringify({ id: "new-chat" }), { status: 200 });

beforeEach(() => {
  calls = [];
  list = [MINE, PAUSED, THEIRS];
  detail = {
    "pt-1": {
      patrol: { ...MINE, prompt: "Goal: keep an eye on delivery." },
      runs: [
        { at: NOW - 3600, trigger: "schedule", outcome: "queued", run_number: 44 },
        { at: NOW - 7200, trigger: "manual", outcome: "queued", run_number: 43 },
        { at: NOW - 10800, trigger: "schedule", outcome: "busy", reason: "approval_pending" },
      ],
    },
    "pt-3": { patrol: THEIRS, runs: [{ at: NOW - 3600, trigger: "schedule", outcome: "queued", run_number: 20 }] },
  };
  createChat = () => new Response(JSON.stringify({ id: "new-chat" }), { status: 200 });
  for (const fn of Object.values(api)) fn.mockReset().mockResolvedValue({ schedule: { id: "pt-new" } });
  navigate.mockClear();
  openPatrolWizard.mockClear();
  resetPatrolCacheForTests();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({ method, url, body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined });
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (url === "/api/patrols") return list === "fail" ? json({}, 500) : json({ patrols: list });
      if (url.startsWith("/api/patrols/")) {
        const d = detail[decodeURIComponent(url.slice("/api/patrols/".length))];
        return d ? json(d) : json({}, 404);
      }
      if (url === "/api/conversations" && method === "POST") return createChat();
      if (url === "/api/conversations") return json({ conversations: [{ id: "c-9", title: "Existing chat" }, { id: "c-8", title: "Sealed", lockdown: true }] });
      return json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Page({ startAdding = false }: { startAdding?: boolean }) {
  const [adding, setAdding] = useState(startAdding);
  return (
    <>
      <button type="button" data-testid="open-form" onClick={() => setAdding(true)}>
        New patrol
      </button>
      <PatrolsView adding={adding} setAdding={setAdding} />
    </>
  );
}

const card = (id: string) => document.querySelector<HTMLElement>(`[data-patrol-id="${id}"]`)!;
const expand = (id: string) => fireEvent.click(card(id).querySelector<HTMLElement>("button[aria-expanded]")!);

describe("the Patrols page", () => {
  it("lists my patrols, then the ones shared with me, and counts them", async () => {
    render(<Page />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    expect(screen.getByTestId("patrols-count")).toHaveTextContent("3 patrols");
    expect(screen.getAllByTestId("patrol-card").map((c) => c.getAttribute("data-patrol-id"))).toEqual(["pt-1", "pt-2", "pt-3"]);
    expect(screen.getByText("Shared with you")).toBeInTheDocument();
    // Each row says its rhythm, its run and how much it may do alone, and its chat.
    expect(card("pt-1")).toHaveTextContent("Every 2h · Run #44 · Auto-approve");
    expect(card("pt-1")).toHaveTextContent("North Peak Deal Updates");
    expect(within(card("pt-1")).getByTestId("patrol-next")).toHaveTextContent("in 19h");
    expect(within(card("pt-2")).getByTestId("patrol-next")).toHaveTextContent("paused");
  });

  it("a teammate's patrol is there to read: one way into its chat, nothing to steer", async () => {
    render(<Page />);
    await waitFor(() => expect(card("pt-3")).toBeTruthy());
    const theirs = within(card("pt-3"));
    expect(theirs.queryByTestId("patrol-run-now")).toBeNull();
    expect(theirs.queryByTestId("patrol-pause")).toBeNull();
    fireEvent.click(theirs.getByRole("button", { name: "Open chat" }));
    // Shared with the team: the team link decides between the viewer and the chat.
    expect(navigate).toHaveBeenLastCalledWith("/chat?team=t-1");

    expand("pt-3");
    await waitFor(() => expect(theirs.getByTestId("patrol-runs")).toBeInTheDocument());
    expect(card("pt-3")).toHaveTextContent("jeanne@example.com set this patrol up.");
    expect(theirs.queryByTestId("patrol-prompt")).toBeNull();
    expect(theirs.queryByTestId("patrol-edit")).toBeNull();
    expect(theirs.queryByTestId("patrol-remove")).toBeNull();
  });

  it("opening a row loads what each run is asked to do and what recently happened", async () => {
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    expect(calls.some((c) => c.url === "/api/patrols/pt-1")).toBe(false);
    expand("pt-1");
    const mine = within(card("pt-1"));
    await waitFor(() => expect(mine.getByTestId("patrol-prompt")).toHaveTextContent("Goal: keep an eye on delivery."));
    const runs = within(mine.getByTestId("patrol-runs")).getAllByRole("listitem");
    expect(runs).toHaveLength(3);
    expect(runs[0]).toHaveTextContent("Ran on schedule · Run #44");
    expect(runs[1]).toHaveTextContent("Ran when asked (Run now) · Run #43");
    expect(runs[2]).toHaveTextContent("Skipped: an approval was waiting");
    expect(calls.filter((c) => c.url === "/api/patrols/pt-1")).toHaveLength(1);
  });

  it("says so, with a way to retry, when a patrol's history cannot be read", async () => {
    delete detail["pt-1"];
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    expand("pt-1");
    expect(await within(card("pt-1")).findByText(/Couldn't load this patrol's history\./)).toBeInTheDocument();
    // Editing needs the task, which did not load.
    expect(within(card("pt-1")).getByTestId("patrol-edit")).toBeDisabled();
  });

  it("Run now and Pause go to the patrol's own chat, then the list is read again", async () => {
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    const reads = () => calls.filter((c) => c.url === "/api/patrols").length;
    const before = reads();
    fireEvent.click(within(card("pt-1")).getByTestId("patrol-run-now"));
    await waitFor(() => expect(api.runScheduleNow).toHaveBeenCalledWith("c-1", "pt-1"));
    await waitFor(() => expect(reads()).toBeGreaterThan(before));

    fireEvent.click(within(card("pt-1")).getByTestId("patrol-pause"));
    await waitFor(() => expect(api.setScheduleAction).toHaveBeenCalledWith("c-1", "pt-1", "pause"));
    fireEvent.click(within(card("pt-2")).getByTestId("patrol-pause"));
    await waitFor(() => expect(api.setScheduleAction).toHaveBeenCalledWith("c-2", "pt-2", "resume"));
  });

  it("a refused action is said on the page", async () => {
    api.runScheduleNow.mockRejectedValueOnce(new Error("The chat is busy."));
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    fireEvent.click(within(card("pt-1")).getByTestId("patrol-run-now"));
    expect(await screen.findByTestId("patrols-page-error")).toHaveTextContent("The chat is busy.");
  });

  it("removing a patrol takes a second click", async () => {
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    expand("pt-1");
    const remove = await within(card("pt-1")).findByTestId("patrol-remove");
    fireEvent.click(remove);
    expect(api.deleteSchedule).not.toHaveBeenCalled();
    expect(remove).toHaveTextContent("Confirm remove");
    fireEvent.click(remove);
    await waitFor(() => expect(api.deleteSchedule).toHaveBeenCalledWith("c-1", "pt-1"));
  });

  it("a patrol whose runs are being skipped says so once, and still says when it checks next", async () => {
    list = [patrol({ skipped: { reason: "approval_pending", count: 3 } }), patrol({ id: "pt-9", skipped: { reason: "approval_pending", count: 1 } })];
    render(<Page />);
    await waitFor(() => expect(card("pt-1")).toBeTruthy());
    expect(within(card("pt-1")).getByTestId("patrol-skipped")).toHaveTextContent("3 runs skipped · approval waiting");
    expect(within(card("pt-1")).getByTestId("patrol-next")).toHaveTextContent("in 19h");
    expect(within(card("pt-9")).getByTestId("patrol-skipped")).toHaveTextContent("1 run skipped · approval waiting");
  });

  it("search finds a patrol by its name, its chat, or whose it is", async () => {
    render(<Page />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    const search = screen.getByTestId("patrols-search");
    fireEvent.change(search, { target: { value: "basis" } });
    expect(screen.getAllByTestId("patrol-card").map((c) => c.getAttribute("data-patrol-id"))).toEqual(["pt-2"]);
    expect(screen.getByTestId("patrols-count")).toHaveTextContent("1 of 3");
    fireEvent.change(search, { target: { value: "jeanne" } });
    expect(screen.getAllByTestId("patrol-card").map((c) => c.getAttribute("data-patrol-id"))).toEqual(["pt-3"]);
  });

  it("with none yet, the guided setup is the way in and the plain form is a click away", async () => {
    list = [];
    render(<Page />);
    const empty = await screen.findByTestId("patrols-empty");
    expect(empty).toHaveTextContent("Set up your first patrol");
    fireEvent.click(screen.getByTestId("patrols-first"));
    expect(openPatrolWizard).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByTestId("patrols-write"));
    expect(screen.queryByTestId("patrols-empty")).toBeNull();
    expect(screen.getByRole("button", { name: "Start patrol" })).toBeInTheDocument();
  });

  it('"could not load" is never shown as "you have none"', async () => {
    list = "fail";
    render(<Page />);
    expect(await screen.findByTestId("patrols-load-failed")).toHaveTextContent("Couldn’t load your patrols.");
    expect(screen.queryByTestId("patrols-empty")).toBeNull();
    list = [MINE];
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(1));
  });
});

describe("starting a patrol from the page", () => {
  const fillAndStart = async () => {
    const form = screen.getByRole("button", { name: "Start patrol" }).closest("form") as HTMLElement;
    const task = within(form).getAllByRole("textbox")[0];
    fireEvent.change(task, { target: { value: "Check delivery and flag anything under 90%." } });
    await waitFor(() => expect(within(form).getByRole("button", { name: "Start patrol" })).toBeEnabled());
    fireEvent.click(within(form).getByRole("button", { name: "Start patrol" }));
  };

  it("in a new chat: makes the chat, then arms the patrol in it", async () => {
    render(<Page startAdding />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    await fillAndStart();
    await waitFor(() => expect(api.createSchedule).toHaveBeenCalledTimes(1));
    const made = calls.find((c) => c.method === "POST" && c.url === "/api/conversations");
    expect(made?.body).toMatchObject({ title: expect.any(String) });
    const [conversationId, body] = api.createSchedule.mock.calls[0];
    expect(conversationId).toBe("new-chat");
    expect(body).toMatchObject({ prompt: "Check delivery and flag anything under 90%.", approval_mode: "auto", auto_approve: true });
    // A patrol in its own new chat is that chat's only one.
    expect(body.allow_multiple).toBeUndefined();
    // The form closes and the list is read again.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Start patrol" })).toBeNull());
  });

  it("a chat made for a patrol that could not be armed is taken back out", async () => {
    api.createSchedule.mockRejectedValueOnce(new Error("Schedules are full."));
    render(<Page startAdding />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    await fillAndStart();
    expect(await screen.findByTestId("patrols-page-error")).toHaveTextContent("Schedules are full.");
    expect(calls.some((c) => c.method === "DELETE" && c.url === "/api/conversations/new-chat")).toBe(true);
    // The form stays, with what was typed, for another try.
    expect(screen.getByRole("button", { name: "Start patrol" })).toBeInTheDocument();
  });

  it("in an existing chat: adds a patrol beside the ones already there, and a sealed chat is not offered", async () => {
    render(<Page startAdding />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    fireEvent.click(screen.getByRole("button", { name: "An existing chat" }));
    const picker = await screen.findByTestId("patrols-new-chat");
    await waitFor(() => expect(within(picker).getByRole("option", { name: "Existing chat" })).toBeInTheDocument());
    expect(within(picker).queryByRole("option", { name: "Sealed" })).toBeNull();
    fireEvent.change(picker, { target: { value: "c-9" } });
    await fillAndStart();
    await waitFor(() => expect(api.createSchedule).toHaveBeenCalledTimes(1));
    expect(api.createSchedule.mock.calls[0][0]).toBe("c-9");
    expect(api.createSchedule.mock.calls[0][1].allow_multiple).toBe(true);
    expect(calls.some((c) => c.method === "POST" && c.url === "/api/conversations")).toBe(false);
  });

  it("the guided setup is one click from the form", async () => {
    render(<Page startAdding />);
    await waitFor(() => expect(screen.getAllByTestId("patrol-card")).toHaveLength(3));
    fireEvent.click(screen.getByTestId("patrols-guided"));
    expect(openPatrolWizard).toHaveBeenCalledTimes(1);
  });
});
