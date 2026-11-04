import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const navigateMock = vi.fn();
vi.mock("@/app/lib/navigation", () => ({ navigate: (href: string) => navigateMock(href) }));

import { LAUNCHPAD_LAUNCHED_KEY, LAUNCHPAD_STATE_KEY, PatrolLaunchpad } from "./PatrolLaunchpad";

// The Patrol Launchpad end to end against a stubbed API
// (docs/PATROL-LAUNCHPAD.md): it walks the six steps, saves a non-negotiable
// as a pinned memory, launches through the same endpoints "Start patrol"
// uses — in order, with the picked connectors and autonomy — and opens the
// patrol chat. "Draft it with AI" replaces the template draft, and a failed
// draft keeps it.

type Call = { url: string; method: string; body?: unknown };

function mockApi(opts: { draft?: "ok" | "fail" } = {}) {
  const calls: Call[] = [];
  const fn = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const json = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => "" });
    if (url === "/api/mcp-servers") {
      return json({
        servers: [
          { name: "gmail", display_name: "Gmail", description: "Read and search mail", enabled: true },
          { name: "dealdesk", display_name: "Deal Desk", description: "PMP deal management", enabled: false },
          { name: "trade", display_name: "Trade Desk", description: "DSP", status: "needs_setup" },
        ],
      });
    }
    if (url === "/api/memories" && method === "GET") return json({ memories: [] });
    if (url === "/api/memories" && method === "POST") return json({ id: "m1", content: "x" });
    if (url === "/api/personas") return json({ personas: ["assistant", "trader"], default: "assistant", user_personas: [] });
    if (url === "/api/patrol-draft") {
      if (opts.draft === "fail") return { ok: false, status: 502, json: async () => ({}), text: async () => "busy" };
      return json({ title: "Deal & inbox sweep", prompt: "**Mission**\nAI-written.", summary: "Every morning I check." });
    }
    if (url === "/api/conversations" && method === "POST") return json({ id: "conv-9" });
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

async function walkToLaunch(calls: Call[]) {
  render(<PatrolLaunchpad onClose={() => {}} />);
  await screen.findByText("2 tools ready");
  next();

  // Tools: Gmail is pre-ticked (on by default), Deal Desk is ticked by hand,
  // and the connector that needs setup links to its setup page.
  const gmail = (await screen.findByTestId("launchpad-tool-gmail")) as HTMLInputElement;
  expect(gmail.checked).toBe(true);
  fireEvent.click(screen.getByTestId("launchpad-tool-dealdesk"));
  expect(screen.getByRole("link", { name: /Set up/ }).getAttribute("href")).toBe(
    "/settings/connections?connector=trade",
  );
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
  fireEvent.click(screen.getByTestId("launchpad-mode-act"));
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

    fireEvent.click(screen.getByTestId("launchpad-launch"));
    await screen.findByTestId("launchpad-done");

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
    expect(window.sessionStorage.getItem(LAUNCHPAD_STATE_KEY)).toBeNull();
    expect(window.localStorage.getItem(LAUNCHPAD_LAUNCHED_KEY)).toBe("1");

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
});
