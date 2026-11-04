import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const navigateMock = vi.fn();
vi.mock("@/app/lib/navigation", () => ({ navigate: (href: string) => navigateMock(href) }));

import { activeMemories, LAUNCHPAD_LAUNCHED_ITEM, PatrolLaunchpad, progressItem, recoveryItem, withMemory } from "./PatrolLaunchpad";

// The Patrol Launchpad end to end against a stubbed API
// (docs/PATROL-LAUNCHPAD.md): it walks the six steps, saves a non-negotiable
// as a pinned memory, launches through the same endpoints "Start patrol"
// uses — in order, with the picked connectors and autonomy — and opens the
// patrol chat. "Draft it with AI" replaces the template draft, and a failed
// draft keeps it.

type Call = { url: string; method: string; body?: unknown };

type Opts = {
  userPersonasNull?: boolean;
  memoriesRefreshHang?: boolean;
  alwaysOnLater?: boolean;
  memoriesFail?: boolean;
  hangDraft?: boolean;
  draft?: "ok" | "fail";
  catalog?: "ok" | "fail";
  // Results of the catalog reads after the first one (focus refreshes, the
  // launch-time re-read); beyond the list, reads succeed.
  catalogLater?: ("ok" | "fail")[];
  pin?: "ok" | "fail";
  // "drop": the request never gets an answer (a lost connection).
  runNow?: "ok" | "fail" | "drop";
  // How many times DELETE of the half-created chat fails before it works.
  deleteFailures?: number;
  tools?: "ok" | "fail";
  memories?: unknown[];
  hangConversation?: boolean;
  // How many creates commit on the server but lose their reply.
  createRepliesLost?: number;
  // The memory save commits but its reply is lost.
  memoryReplyLost?: boolean;
  role?: string;
  // Holds the memory POST until this resolves.
  memoryPostGate?: Promise<void>;
};

function mockApi(opts: Opts = {}) {
  const calls: Call[] = [];
  let memoryGets = 0;
  let catalogGets = 0;
  const keyed = new Map<string, string>();
  const savedMemories: { id: string; content: string }[] = [];
  const fn = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const json = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => "" });
    if (url === "/api/mcp-servers") {
      if (opts.alwaysOnLater && catalogGets > 0) {
        return json({
          servers: [
            { name: "gmail", display_name: "Gmail", description: "Read and search mail", enabled: true },
            { name: "dealdesk", display_name: "Deal Desk", description: "PMP deal management", enabled: false },
            { name: "files", display_name: "Workspace files", description: "Shared reports", always_on: true, enabled: true },
          ],
        });
      }
      const n = catalogGets++;
      if (n > 0 && opts.catalogLater?.[n - 1] === "fail") {
        return { ok: false, status: 500, json: async () => ({}), text: async () => "down" };
      }
      if (opts.catalog === "fail") return { ok: false, status: 500, json: async () => ({}), text: async () => "down" };
      return json({
        servers: [
          { name: "gmail", display_name: "Gmail", description: "Read and search mail", enabled: true },
          { name: "dealdesk", display_name: "Deal Desk", description: "PMP deal management", enabled: false },
          { name: "trade", display_name: "Trade Desk", description: "DSP", status: "needs_setup" },
        ],
      });
    }
    if (url === "/api/memories" && method === "GET" && opts.memoriesRefreshHang && memoryGets++ > 0) {
      return new Promise(() => {});
    }
    if (url === "/api/memories" && method === "GET" && opts.memoriesFail) {
      return { ok: false, status: 500, json: async () => ({}), text: async () => "" };
    }
    if (url === "/api/memories" && method === "GET") return json({ memories: [...(opts.memories ?? []), ...savedMemories] });
    if (url === "/api/memories/m1" && method === "PATCH" && opts.pin === "fail") {
      return { ok: false, status: 500, json: async () => ({}), text: async () => "" };
    }
    if (url === "/api/memories" && method === "POST" && opts.memoryReplyLost) {
      opts.memoryReplyLost = false;
      savedMemories.push({ id: "m1", content: (JSON.parse(String(init?.body)) as { content: string }).content });
      throw new TypeError("Failed to fetch");
    }
    if (url === "/api/memories" && method === "POST") {
      if (opts.memoryPostGate) await opts.memoryPostGate;
      return json({ id: "m1", content: "x" });
    }
    if (url === "/api/me") return json({ email: "e2e@example.com", role: opts.role ?? "member" });
    if (url === "/api/personas") {
      return json({ personas: ["assistant", "trader"], default: "assistant", user_personas: opts.userPersonasNull ? null : [] });
    }
    if (url === "/api/patrol-draft" && opts.hangDraft) return new Promise(() => {});
    if (url === "/api/patrol-draft") {
      if (opts.draft === "fail") return { ok: false, status: 502, json: async () => ({}), text: async () => "busy" };
      return json({ title: "Deal & inbox sweep", prompt: "**Mission**\nAI-written.", summary: "Every morning I check." });
    }
    if (url === "/api/conversations" && method === "POST") {
      if (opts.hangConversation) return new Promise(() => {});
      // Like the server: a create keyed with a client_request_id it has seen
      // answers with that chat instead of making another.
      const key = (JSON.parse(String(init?.body)) as { client_request_id?: string }).client_request_id ?? "";
      if (key && !keyed.has(key)) keyed.set(key, "conv-9");
      if ((opts.createRepliesLost ?? 0) > 0) {
        opts.createRepliesLost = (opts.createRepliesLost ?? 0) - 1;
        throw new TypeError("Failed to fetch");
      }
      return json({ id: "conv-9" });
    }
    if (url.endsWith("/mcp-servers") && method === "POST" && opts.tools === "fail") {
      return { ok: false, status: 500, json: async () => ({}), text: async () => "tools down" };
    }
    if (url.endsWith("/run-now") && opts.runNow === "drop") throw new TypeError("Failed to fetch");
    if (url === "/api/conversations/conv-9" && method === "DELETE" && (opts.deleteFailures ?? 0) > 0) {
      opts.deleteFailures = (opts.deleteFailures ?? 0) - 1;
      return { ok: false, status: 500, json: async () => ({}), text: async () => "" };
    }
    if (url.endsWith("/run-now") && opts.runNow === "fail") {
      return { ok: false, status: 429, json: async () => ({}), text: async () => "queue full" };
    }
    if (url.endsWith("/schedules") && method === "POST") return json({ schedule: { id: "s-1" } });
    return json({});
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

const next = () => fireEvent.click(screen.getByTestId("launchpad-next"));

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  navigateMock.mockReset();
});

async function walkToLaunch(calls: Call[], onClose: () => void = () => {}) {
  render(<PatrolLaunchpad onClose={onClose} />);
  void calls;
  // The preamble: the expanding-mind ladder and the trust path.
  expect(screen.getByTestId("launchpad-mind").querySelectorAll("li")).toHaveLength(4);
  expect(screen.getByTestId("launchpad-trust-path").textContent).toContain("Suggest & ask");
  // Each rung has its own hand-drawn picture: the meme's brain, one level per rung.
  const levels = screen.getAllByTestId("launchpad-mind-art").map((svg) => svg.getAttribute("data-level"));
  expect(levels).toEqual(["1", "2", "3", "4"]);
  const mollick = screen.getByRole("link", { name: "Dr. Ethan Mollick" });
  expect(mollick.getAttribute("href")).toBe("https://www.oneusefulthing.org/");
  next();
  await screen.findByTestId("launchpad-readiness");
  await waitFor(() => expect(screen.getByTestId("launchpad-readiness").textContent).not.toMatch(/Checking tools/));
  next();

  // Tools: Gmail is pre-ticked (on by default), Deal Desk is ticked by hand,
  // and the connector that needs setup links to its setup page.
  if (screen.queryByTestId("launchpad-tool-gmail")) {
    const gmail = screen.getByTestId("launchpad-tool-gmail") as HTMLInputElement;
    expect(gmail.checked).toBe(true);
    fireEvent.click(screen.getByTestId("launchpad-tool-dealdesk"));
    expect(screen.getByRole("link", { name: /Set up/ }).getAttribute("href")).toBe(
      "/settings/connections?connector=trade",
    );
  }
  next();

  // Ground rules: a persona, and a non-negotiable saved as a pinned memory.
  fireEvent.change(screen.getByTestId("launchpad-persona"), { target: { value: "trader" } });
  fireEvent.change(screen.getByTestId("launchpad-memory-input"), {
    target: { value: "Never pause a campaign without asking me" },
  });
  fireEvent.click(screen.getByTestId("launchpad-memory-add"));
  await waitFor(() => expect(calls.some((c) => c.url === "/api/memories/m1" && c.method === "PATCH")).toBe(true));
  expect(calls.find((c) => c.url === "/api/memories" && c.method === "POST")?.body).toEqual({
    content: "Never pause a campaign without asking me",
    kind: "constraint",
  });
  expect(calls.find((c) => c.url === "/api/memories/m1")?.body).toEqual({ pinned: true });
  next();

  // Watch list: nothing picked means no Next.
  expect((screen.getByTestId("launchpad-next") as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByTestId("launchpad-goal-deals"));
  fireEvent.click(screen.getByTestId("launchpad-goal-inbox"));
  expect(screen.getByTestId("launchpad-mode-nudge").textContent).toContain("turn on ⚡ Auto-approve");
  fireEvent.click(screen.getByTestId("launchpad-mode-act"));
  expect(screen.getByTestId("launchpad-mode-nudge").textContent).toContain("turn ⚡ Auto-approve off");
  next();

  // Schedule: pick a preset.
  fireEvent.click(screen.getByTestId("launchpad-cadence-every-2h"));
  next();
  await screen.findByTestId("launchpad-step-launch");
}

describe("PatrolLaunchpad", () => {
  it("walks from nothing to a launched patrol and opens its chat", async () => {
    const calls = mockApi();
    await walkToLaunch(calls);

    expect((screen.getByTestId("launchpad-title") as HTMLInputElement).value).toBe("Deals & inbox patrol");
    const prompt = (screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value;
    expect(prompt).toMatch(/\(use Deal Desk\)/);
    expect(prompt).toMatch(/\(use Gmail\)/);
    expect(screen.getByTestId("launchpad-summary").textContent).toContain("Gmail, Deal Desk");
    // Honest about what already exists: the non-negotiable saved earlier.
    expect(screen.getByTestId("launchpad-step-launch").textContent).toContain(
      "the non-negotiable you saved on Ground rules is already in your memories",
    );
    // It says where the patrol will live: in Chat, under its name.
    expect(screen.getByTestId("launchpad-where").textContent).toMatch(/In Chat.*as Deals & inbox patrol/);

    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-done");
    // Already acting on its own: no "next rung" nudge toward auto-approve.
    expect(screen.queryByText(/Next rung of the ladder/)).toBeNull();

    const writes = calls.filter((c) => c.method === "POST" && !c.url.startsWith("/api/memories"));
    expect(writes.map((c) => c.url)).toEqual([
      "/api/conversations",
      "/api/conversations/conv-9/mcp-servers",
      "/api/conversations/conv-9/schedules",
      "/api/conversations/conv-9/schedules/s-1/run-now",
    ]);
    expect(writes[0].body).toMatchObject({ title: "Deals & inbox patrol", persona: "trader" });
    expect(writes[1].body).toEqual({ enabled_optional: ["gmail", "dealdesk"], accounts: {} });
    expect(writes[2].body).toMatchObject({ cron: "0 */2 * * *", auto_approve: true, title: "Deals & inbox patrol" });
    expect((writes[2].body as { prompt: string }).prompt).toContain("You may take routine, reversible actions");

    // Launch clears the saved progress and remembers that a patrol launched.
    expect(window.sessionStorage.getItem(progressItem())).toBeNull();
    expect(window.localStorage.getItem(LAUNCHPAD_LAUNCHED_ITEM)).toBe("1");

    fireEvent.click(screen.getByTestId("launchpad-open"));
    expect(navigateMock).toHaveBeenCalledWith("/chat?c=conv-9");
  });

  it("lets the assistant write the instructions, and keeps the template when it cannot", async () => {
    const calls = mockApi();
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-draft-ai"));
    await screen.findByTestId("launchpad-ai-summary");
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toBe("**Mission**\nAI-written.");
    expect((screen.getByTestId("launchpad-title") as HTMLInputElement).value).toBe("Deal & inbox sweep");
    const req = calls.find((c) => c.url === "/api/patrol-draft")?.body as Record<string, unknown>;
    expect(req).toMatchObject({ mode: "act", persona: "trader", cadence: expect.stringContaining("Every 2 hours") });
    expect(req.connectors).toEqual([
      { name: "Gmail", description: "Read and search mail" },
      { name: "Deal Desk", description: "PMP deal management" },
    ]);
    expect((req.goals as string[])[0]).toMatch(/^Catch misconfigured deals: /);
  });

  it("keeps the template draft when the assistant cannot draft", async () => {
    const calls = mockApi({ draft: "fail" });
    await walkToLaunch(calls);
    const before = (screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value;
    fireEvent.click(screen.getByTestId("launchpad-draft-ai"));
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", expect.stringContaining("still here"));
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toBe(before);
  });

  it("remembers progress across a close and reopen", async () => {
    mockApi();
    render(<PatrolLaunchpad onClose={() => {}} />);
    next();
    await screen.findByTestId("launchpad-step-welcome");
    next();
    await screen.findByTestId("launchpad-step-tools");
    next();
    await screen.findByTestId("launchpad-step-rules");
    cleanup();
    render(<PatrolLaunchpad onClose={() => {}} />);
    expect(await screen.findByTestId("launchpad-step-rules")).toBeTruthy();
  });

  it("reports a failed launch and offers the way back", async () => {
    const calls = mockApi();
    await walkToLaunch(calls);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 400, json: async () => ({}), text: async () => "cron too frequent" }),
    );
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("cron too frequent");
    fireEvent.click(screen.getByRole("button", { name: "Back to the plan" }));
    expect(await screen.findByTestId("launchpad-step-launch")).toBeTruthy();
  });

  // ── Codex review findings (PR #1917) ──

  it("adds a new pinned memory at the front, so the drafter's 50-memory window keeps it", () => {
    const roster = [
      { id: "p1", content: "pinned", pinned: true },
      ...Array.from({ length: 60 }, (_, i) => ({ id: `u${i}`, content: `unpinned ${i}` })),
    ];
    const pinned = withMemory(roster, { id: "new", content: "Never pause", pinned: true });
    expect(pinned[0].id).toBe("new");
    expect(pinned.slice(0, 50).some((m) => m.id === "new")).toBe(true);
    const unpinned = withMemory(roster, { id: "n2", content: "loose" });
    expect(unpinned[1].id).toBe("n2"); // after the pinned block
    expect(withMemory(pinned, { id: "new", content: "dup", pinned: true })).toBe(pinned);
  });

  it("leaves memories to the server: the draft request carries none", async () => {
    expect(
      activeMemories([
        { id: "a", content: "keep" },
        { id: "b", content: "retired", retired_at: 5 },
        { id: "c", content: "proposal", source: "proposed" },
      ]).map((m) => m.id),
    ).toEqual(["a"]);
    const calls = mockApi({ memories: [{ id: "a", content: "Report in USD" }] });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-draft-ai"));
    await screen.findByTestId("launchpad-ai-summary");
    const req = calls.find((c) => c.url === "/api/patrol-draft")?.body as Record<string, unknown>;
    expect(req).not.toHaveProperty("memories");
    expect(req.goals).toBeTruthy();
  });

  it("says when the first check could not start, instead of claiming it is running", async () => {
    const calls = mockApi({ runNow: "fail" });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect(await screen.findByTestId("launchpad-first-run-refused")).toBeTruthy();
    expect(screen.queryByText(/is running its first check right now/)).toBeNull();
  });

  it("reports a non-negotiable that saved but could not be pinned", async () => {
    const calls = mockApi({ pin: "fail" });
    render(<PatrolLaunchpad onClose={() => {}} />);
    next();
    next();
    await screen.findByTestId("launchpad-step-tools");
    next();
    fireEvent.change(screen.getByTestId("launchpad-memory-input"), { target: { value: "Ask before pausing" } });
    fireEvent.click(screen.getByTestId("launchpad-memory-add"));
    expect(await screen.findByText(/could not be pinned/)).toBeTruthy();
    expect(calls.some((c) => c.url === "/api/memories/m1" && c.method === "PATCH")).toBe(true);
  });

  it("keeps the instructions live when only the name is edited, and flags custom ones the plan outgrew", async () => {
    const calls = mockApi();
    await walkToLaunch(calls);
    fireEvent.change(screen.getByTestId("launchpad-title"), { target: { value: "My sweep" } });
    // Back to the watch list: add a goal.
    fireEvent.click(screen.getByTestId("launchpad-back"));
    fireEvent.click(screen.getByTestId("launchpad-back"));
    fireEvent.click(screen.getByTestId("launchpad-goal-reports"));
    next();
    next();
    expect((screen.getByTestId("launchpad-title") as HTMLInputElement).value).toBe("My sweep");
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toMatch(/recurring reports/);
    expect(screen.queryByTestId("launchpad-prompt-stale")).toBeNull();

    // Custom instructions, then a plan change: the mismatch is pointed out.
    fireEvent.change(screen.getByTestId("launchpad-prompt"), { target: { value: "Just check deals." } });
    fireEvent.click(screen.getByTestId("launchpad-back"));
    fireEvent.click(screen.getByTestId("launchpad-back"));
    fireEvent.click(screen.getByTestId("launchpad-goal-optimize"));
    next();
    next();
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toBe("Just check deals.");
    expect(screen.getByTestId("launchpad-prompt-stale")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Rewrite from the plan" }));
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toMatch(/optimizations/);
  });

  it("rolls back a half-finished launch before returning to the plan", async () => {
    const calls = mockApi({ tools: "fail" });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("tools down");
    fireEvent.click(screen.getByRole("button", { name: "Back to the plan" }));
    await screen.findByTestId("launchpad-step-launch");
    expect(calls.some((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toBe(true);
    expect(calls.some((c) => c.url.endsWith("/schedules") && c.method === "POST")).toBe(false);
  });

  it("cannot be dismissed while a launch is in flight", async () => {
    const onClose = vi.fn();
    const calls = mockApi({ hangConversation: true });
    await walkToLaunch(calls, onClose);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-launching");
    const [closeButton, ...scrims] = screen
      .getAllByRole("button", { name: "Close the Patrol Launchpad" })
      .sort((a) => (a.closest("header") ? -1 : 1)) as HTMLButtonElement[];
    expect(closeButton.closest("header")).not.toBeNull();
    expect(closeButton.disabled).toBe(true);
    for (const scrim of scrims) fireEvent.click(scrim);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
  });

  it("keeps the server's default connectors when the catalog never loaded", async () => {
    const calls = mockApi({ catalog: "fail" });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-done");
    expect(calls.some((c) => c.url.endsWith("/mcp-servers") && c.method === "POST")).toBe(false);
    expect(calls.some((c) => c.url.endsWith("/schedules") && c.method === "POST")).toBe(true);
  });

  it("keeps saved answers to the account that wrote them", () => {
    window.sessionStorage.setItem("fleet.launchpad.v1:someone-else@example.com", JSON.stringify({ step: "launch", notes: "secret" }));
    mockApi();
    render(<PatrolLaunchpad onClose={() => {}} />);
    expect(screen.getByTestId("launchpad-step-why")).toBeTruthy();
    expect(progressItem()).not.toBe("fleet.launchpad.v1:someone-else@example.com");
  });

  it("stops a read-only viewer at the preamble with an explanation", async () => {
    const calls = mockApi({ role: "viewer" });
    const onClose = vi.fn();
    render(<PatrolLaunchpad onClose={onClose} />);
    expect(await screen.findByTestId("launchpad-readonly")).toBeTruthy();
    expect(screen.queryByTestId("launchpad-next")).toBeNull();
    fireEvent.click(screen.getByTestId("launchpad-readonly-close"));
    expect(onClose).toHaveBeenCalled();
    expect(calls.some((c) => c.method !== "GET")).toBe(false);
  });

  it("does not launch while the assistant is still drafting", async () => {
    const calls = mockApi({ hangDraft: true });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-draft-ai"));
    await waitFor(() => expect((screen.getByTestId("launchpad-launch") as HTMLButtonElement).disabled).toBe(true));
    // The plan cannot change under a draft in flight.
    expect((screen.getByTestId("launchpad-back") as HTMLButtonElement).disabled).toBe(true);
    // The fields the draft will replace are read-only meanwhile.
    expect((screen.getByTestId("launchpad-title") as HTMLInputElement).readOnly).toBe(true);
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).readOnly).toBe(true);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect(calls.some((c) => c.url === "/api/conversations" && c.method === "POST")).toBe(false);
  });

  it("Use a preset leaves a preset selected rather than the custom schedule", async () => {
    mockApi();
    render(<PatrolLaunchpad onClose={() => {}} />);
    for (let i = 0; i < 4; i++) next();
    fireEvent.click(screen.getByTestId("launchpad-goal-inbox"));
    next();
    await screen.findByTestId("launchpad-step-schedule");
    fireEvent.click(screen.getByTestId("launchpad-cadence-custom"));
    // Make the schedule a custom one (weekdays 07:15 is no preset)…
    fireEvent.change(screen.getByTestId("launchpad-schedule-time"), { target: { value: "07:15" } });
    expect(screen.getAllByRole("radio").filter((r) => r.getAttribute("aria-checked") === "true")).toHaveLength(0);
    // …then go back to presets: one must be selected, and it is what launches.
    fireEvent.click(screen.getByTestId("launchpad-cadence-custom"));
    expect(screen.getByTestId("launchpad-cadence-weekday-morning").getAttribute("aria-checked")).toBe("true");
    expect(screen.getByTestId("launchpad-cadence-custom").textContent).toContain("Custom schedule");
    const checked = screen.getAllByRole("radio").filter((r) => r.getAttribute("aria-checked") === "true");
    expect(checked).toHaveLength(1);
  });

  it("never reads a failed memories load as zero memories, and pauses the AI draft", async () => {
    const calls = mockApi({ memoriesFail: true });
    render(<PatrolLaunchpad onClose={() => {}} />);
    next();
    expect(await screen.findByText("Couldn’t check memories")).toBeTruthy();
    expect(screen.queryByText("0 memories saved")).toBeNull();
    for (let i = 0; i < 3; i++) next();
    fireEvent.click(screen.getByTestId("launchpad-goal-inbox"));
    next();
    next();
    await screen.findByTestId("launchpad-step-launch");
    expect((screen.getByTestId("launchpad-draft-ai") as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId("launchpad-memories-failed")).toBeTruthy();
    expect(calls.some((c) => c.url === "/api/patrol-draft")).toBe(false);
  });

  it("Reset to template rewrites the instructions but keeps a name the person chose", async () => {
    const calls = mockApi();
    await walkToLaunch(calls);
    fireEvent.change(screen.getByTestId("launchpad-title"), { target: { value: "My own name" } });
    fireEvent.change(screen.getByTestId("launchpad-prompt"), { target: { value: "custom" } });
    fireEvent.click(screen.getByRole("button", { name: "Reset to template" }));
    expect((screen.getByTestId("launchpad-title") as HTMLInputElement).value).toBe("My own name");
    expect((screen.getByTestId("launchpad-prompt") as HTMLTextAreaElement).value).toContain("**Mission**");
  });

  it("forgets a saved persona that no longer exists, instead of launching a stale ref", async () => {
    window.sessionStorage.setItem(progressItem(), JSON.stringify({ step: "rules", persona: "user:deleted" }));
    mockApi();
    render(<PatrolLaunchpad onClose={() => {}} />);
    await screen.findByTestId("launchpad-step-rules");
    await waitFor(() => expect((screen.getByTestId("launchpad-persona") as HTMLSelectElement).value).toBe(""));
  });

  it("cannot be dismissed while an AI draft is in flight", async () => {
    const onClose = vi.fn();
    const calls = mockApi({ hangDraft: true });
    await walkToLaunch(calls, onClose);
    fireEvent.click(screen.getByTestId("launchpad-draft-ai"));
    await waitFor(() => expect((screen.getByTestId("launchpad-launch") as HTMLButtonElement).disabled).toBe(true));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
  });

  it("keeps the next rule being typed while the previous one saves", async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    mockApi({ memoryPostGate: gate });
    window.sessionStorage.setItem(progressItem(), JSON.stringify({ step: "rules" }));
    render(<PatrolLaunchpad onClose={() => {}} />);
    const input = (await screen.findByTestId("launchpad-memory-input")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "Rule one" } });
    fireEvent.click(screen.getByTestId("launchpad-memory-add"));
    // While the save is in flight, the person starts the next rule.
    fireEvent.change(input, { target: { value: "Rule two" } });
    release();
    await waitFor(() => expect(screen.getByTestId("launchpad-memory-add")).toBeTruthy());
    await new Promise((r) => setTimeout(r, 0));
    expect(input.value).toBe("Rule two");
  });

  it("sends a viewer with saved progress back to the preamble", async () => {
    window.sessionStorage.setItem(progressItem(), JSON.stringify({ step: "launch" }));
    mockApi({ role: "viewer" });
    render(<PatrolLaunchpad onClose={() => {}} />);
    expect(await screen.findByTestId("launchpad-step-why")).toBeTruthy();
    expect(await screen.findByTestId("launchpad-readonly")).toBeTruthy();
  });

  it("counts an always-on connector that appears after the person ticked boxes", async () => {
    const calls = mockApi({ alwaysOnLater: true });
    await walkToLaunch(calls);
    // Back from the other tab: the catalog refreshes on focus.
    fireEvent.focus(window);
    await waitFor(() => expect(screen.getByTestId("launchpad-summary").textContent).toContain("Workspace files"));
  });

  it("re-reads the catalog at launch when a refresh failed, so a ticked tool is not dropped", async () => {
    const calls = mockApi({ catalogLater: ["fail"] });
    await walkToLaunch(calls);
    fireEvent.focus(window); // the refresh fails: the catalog is unknown again
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mcp-servers").length).toBe(2));
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-done");
    const tools = calls.find((c) => c.url.endsWith("/mcp-servers") && c.method === "POST")?.body as {
      enabled_optional: string[];
    };
    expect(tools.enabled_optional).toContain("dealdesk");
  });

  it("stops the launch before creating anything when the tools still cannot be confirmed", async () => {
    const calls = mockApi({ catalogLater: ["fail", "fail"] });
    await walkToLaunch(calls);
    fireEvent.focus(window);
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mcp-servers").length).toBe(2));
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("Couldn’t confirm the tools you picked");
    expect(calls.some((c) => c.url === "/api/conversations" && c.method === "POST")).toBe(false);
  });

  it("keeps a saved personal persona when the personal roster could not be read", async () => {
    window.sessionStorage.setItem(progressItem(), JSON.stringify({ step: "rules", persona: "user:my-twin" }));
    mockApi({ userPersonasNull: true });
    render(<PatrolLaunchpad onClose={() => {}} />);
    await screen.findByTestId("launchpad-step-rules");
    // Wait until the roster has loaded; the saved ref must survive "unknown".
    await screen.findByRole("option", { name: "Trader" });
    const saved = JSON.parse(window.sessionStorage.getItem(progressItem()) ?? "{}") as { persona?: string };
    expect(saved.persona).toBe("user:my-twin");
  });

  it("rolls back a half-finished launch when the dialog is closed on the error", async () => {
    const onClose = vi.fn();
    const calls = mockApi({ tools: "fail" });
    await walkToLaunch(calls, onClose);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("tools down");
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(calls.some((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toBe(true);
  });

  it("keeps the dialog open, and retries, when the rollback itself fails", async () => {
    const onClose = vi.fn();
    const calls = mockApi({ tools: "fail", deleteFailures: 1 });
    await walkToLaunch(calls, onClose);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("tools down");
    // First close: the DELETE fails, so the dialog stays and says what is left.
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Couldn’t remove the half-created patrol chat"));
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Open the chat" })).toBeTruthy();
    // Second close retries, succeeds, and closes.
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(calls.filter((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toHaveLength(2);
  });

  it("does not suggest Run now when the first run's answer was lost", async () => {
    const calls = mockApi({ runNow: "drop" });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    const note = await screen.findByTestId("launchpad-first-run-unknown");
    expect(note.textContent).toContain("couldn’t confirm its first check started");
    expect(screen.queryByTestId("launchpad-first-run-refused")).toBeNull();
  });

  it("retries a create whose reply was lost with the same key, and gets the one chat it made", async () => {
    const calls = mockApi({ createRepliesLost: 1 });
    await walkToLaunch(calls);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-done");
    const creates = calls.filter((c) => c.url === "/api/conversations" && c.method === "POST");
    expect(creates).toHaveLength(2);
    const keys = creates.map((c) => (c.body as { client_request_id?: string }).client_request_id);
    expect(keys[0]).toMatch(/^launchpad-/);
    expect(keys[1]).toBe(keys[0]); // the retry is the same request
  });

  it("keeps the dialog open while a create cannot be confirmed, then settles and rolls it back", async () => {
    const onClose = vi.fn();
    // The first two creates (the attempt and its immediate retry) lose their reply.
    const calls = mockApi({ createRepliesLost: 2 });
    await walkToLaunch(calls, onClose);
    fireEvent.click(screen.getByTestId("launchpad-launch"));
    expect((await screen.findByRole("alert")).textContent).toContain("Couldn’t confirm whether the patrol chat");
    // Closing settles it first (same key: the server returns the chat it made),
    // then rolls that chat back, then closes.
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    const creates = calls.filter((c) => c.url === "/api/conversations" && c.method === "POST");
    expect(new Set(creates.map((c) => (c.body as { client_request_id?: string }).client_request_id)).size).toBe(1);
    expect(calls.some((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toBe(true);
  });

  it("treats a memory save whose reply was lost as saved once it shows up, and never saves the same rule twice", async () => {
    const calls = mockApi({ memoryReplyLost: true });
    window.sessionStorage.setItem(progressItem(), JSON.stringify({ step: "rules" }));
    render(<PatrolLaunchpad onClose={() => {}} />);
    const input = (await screen.findByTestId("launchpad-memory-input")) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "Never pause without asking" } });
    fireEvent.click(screen.getByTestId("launchpad-memory-add"));
    // The save committed although its reply was lost: it is found and pinned.
    await waitFor(() => expect(calls.some((c) => c.url === "/api/memories/m1" && c.method === "PATCH")).toBe(true));
    // The same rule again is refused locally, with no second save.
    fireEvent.change(input, { target: { value: "never pause without asking " } });
    fireEvent.click(screen.getByTestId("launchpad-memory-add"));
    expect(await screen.findByText("That one is already saved.")).toBeTruthy();
    expect(calls.filter((c) => c.url === "/api/memories" && c.method === "POST")).toHaveLength(1);
  });

  it("after a reload, an unfinished launch comes back on its error screen and is cleaned up on close", async () => {
    window.sessionStorage.setItem(recoveryItem(), JSON.stringify({ conversationId: "conv-9", scheduleId: "s-1" }));
    const onClose = vi.fn();
    const calls = mockApi();
    render(<PatrolLaunchpad onClose={onClose} />);
    expect((await screen.findByRole("alert")).textContent).toContain("Your last launch didn’t finish");
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(calls.some((c) => c.url.endsWith("/schedules/s-1") && c.method === "PATCH")).toBe(true);
    expect(calls.some((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toBe(true);
    expect(window.sessionStorage.getItem(recoveryItem())).toBeNull();
  });

  it("after a reload, an unconfirmed create is settled with its own key and rolled back", async () => {
    window.sessionStorage.setItem(
      recoveryItem(),
      JSON.stringify({ conversationId: "", scheduleId: "", pendingCreate: { body: { title: "P", client_request_id: "launchpad-k" } } }),
    );
    const onClose = vi.fn();
    const calls = mockApi();
    render(<PatrolLaunchpad onClose={onClose} />);
    await screen.findByRole("alert");
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    const create = calls.find((c) => c.url === "/api/conversations" && c.method === "POST");
    expect((create?.body as { client_request_id?: string } | undefined)?.client_request_id).toBe("launchpad-k");
    expect(calls.some((c) => c.url === "/api/conversations/conv-9" && c.method === "DELETE")).toBe(true);
  });
});
