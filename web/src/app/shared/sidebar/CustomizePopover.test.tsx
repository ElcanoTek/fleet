import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { setBootstrapForTest } from "@/app/lib/bootstrap";
import { CustomizeButton } from "./CustomizePopover";
import { BUILTIN_LAYOUT, addShortcut, isStartPage, isKeptAtTop, teamChatsMode, setTeamChatsMode, type SidebarLayout } from "./layout";
import { getSidebarState, loadSidebar, resetSidebarStoreForTests, setSidebarCustomizerOpen, useSidebarCustomizerOpen } from "./store";
import { SidebarTargetsContext, resetPatrolCacheForTests, type SidebarPins, type SidebarTargets } from "./targets";

// The gear in the sidebar's header (docs/SIDEBAR.md): the few choices people
// change most, one click from the sidebar itself.

const me = vi.hoisted(() => ({ role: "member" }));
vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: me.role, team_id: "", admin: false }),
}));

function stubSidebar(layout: SidebarLayout | null = null) {
  const puts: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/patrols") return Response.json({ patrols: [] });
      if (init?.method === "PUT") {
        puts.push(JSON.parse(String(init.body)));
        return Response.json({ layout: puts[puts.length - 1] });
      }
      return Response.json({ layout, workspace_default: null });
    }),
  );
  return puts;
}

const pins = (over: Partial<SidebarPins> = {}): SidebarPins => ({
  entries: [
    { id: "project:p1", kind: "project", ref: "p1", title: "TWC emails", canUnpin: true },
    { id: "c9", kind: "chat", ref: "c9", title: "FW26 Campaign", canUnpin: true },
  ],
  unpin: () => {},
  pin: () => true,
  ...over,
});

function withTargets(ui: ReactNode, p?: SidebarPins) {
  const targets: SidebarTargets = {
    activeConversationId: null,
    activeProjectId: null,
    pins: p,
    titleFor: (item) => (item.ref === "c1" ? "Campaign Watch (live name)" : undefined),
    availability: () => "yes",
    open: () => false,
  };
  return <SidebarTargetsContext.Provider value={targets}>{ui}</SidebarTargetsContext.Provider>;
}

async function openPanel(layout: SidebarLayout | null = null, p?: SidebarPins) {
  const puts = stubSidebar(layout);
  await loadSidebar();
  render(withTargets(<CustomizeButton collapsed={false} />, p));
  fireEvent.click(screen.getByTestId("customize-sidebar-button"));
  const panel = await screen.findByTestId("customize-popover");
  // The controls come on once the account is known not to be read-only.
  if (me.role !== "viewer") await waitFor(() => expect(within(panel).getByRole("button", { name: "Compact" })).toBeEnabled());
  return { panel, puts };
}

const option = (panel: HTMLElement, name: string | RegExp) => within(panel).getByRole("radio", { name });

beforeEach(() => {
  me.role = "member";
  setBootstrapForTest({ session: { email: "me@example.com" } } as never);
  resetSidebarStoreForTests();
  resetPatrolCacheForTests();
  window.localStorage.clear();
});
afterEach(() => {
  cleanup();
  setSidebarCustomizerOpen(false);
  setBootstrapForTest(null);
  vi.unstubAllGlobals();
});

describe("the gear's Customize panel", () => {
  it("opens from the gear and says where fleet opens: the last chat, until a start page is set", async () => {
    const { panel } = await openPanel();
    expect(within(panel).getByRole("heading", { name: "Customize sidebar" })).toBeInTheDocument();
    expect(option(panel, "The last chat you had open")).toBeChecked();
    expect(option(panel, "Background tasks")).not.toBeChecked();
    // Patrols is a list of chats, not somewhere to land.
    expect(within(panel).queryByRole("radio", { name: "Patrols" })).toBeNull();
    expect(panel).toHaveTextContent("Any chat, dashboard or project can be the start page: ⋮ › Set as start page.");
  });

  it("lists the top items and what is pinned, by their live names, and sets one as the start page", async () => {
    const layout = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Saved name", view: "dashboard" });
    const { panel } = await openPanel(layout, pins());
    expect(within(panel).getAllByTestId("customize-home-option").map((o) => o.textContent)).toEqual([
      "The last chat you had open",
      "Background tasks",
      "Campaign Watch (live name)Dashboard",
      "TWC emailsProject",
      "FW26 CampaignChat",
    ]);
    fireEvent.click(option(panel, /TWC emails/));
    const next = getSidebarState().layout;
    expect(isStartPage(next, "project", "p1")).toBe(true);
    // A pinned project becomes the start page where it is: not a top item.
    expect(isKeptAtTop(next, "project", "p1")).toBe(false);
    await waitFor(() => expect(option(panel, /TWC emails/)).toBeChecked());

    fireEvent.click(option(panel, "The last chat you had open"));
    expect(getSidebarState().layout.home).toBe("");
  });

  it("shows a start page set from a row's menu even when it is neither a top item nor pinned", async () => {
    const layout = { ...addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c7", title: "Board" }) };
    layout.items = layout.items.map((i) => (i.ref === "c7" ? { ...i, hidden: true } : i));
    layout.home = layout.items.find((i) => i.ref === "c7")!.id;
    const { panel } = await openPanel(layout);
    expect(option(panel, /Board/)).toBeChecked();
  });

  it("sets the density", async () => {
    const { panel, puts } = await openPanel();
    expect(within(panel).getByRole("button", { name: "Comfortable" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(within(panel).getByRole("button", { name: "Compact" }));
    expect(getSidebarState().layout.compact).toBe(true);
    await waitFor(() => expect(puts.length).toBeGreaterThan(0), { timeout: 3000 });
    fireEvent.click(within(panel).getByRole("button", { name: "Comfortable" }));
    expect(getSidebarState().layout.compact).toBeUndefined();
  });

  it("shows or hides Patrols, Background tasks, Projects and teammates' shared chats", async () => {
    const { panel } = await openPanel();
    const sw = (key: string) => within(panel).getByTestId(`customize-show-${key}`);
    for (const key of ["patrols", "tasks", "projects", "team"]) expect(sw(key)).toBeChecked();

    fireEvent.click(sw("patrols"));
    expect(getSidebarState().layout.items.find((i) => i.id === "patrols")?.hidden).toBe(true);
    fireEvent.click(sw("tasks"));
    expect(getSidebarState().layout.items.find((i) => i.id === "tasks")?.hidden).toBe(true);
    fireEvent.click(sw("projects"));
    expect(getSidebarState().layout.sections.find((s) => s.id === "projects")?.hidden).toBe(true);
    fireEvent.click(sw("team"));
    expect(teamChatsMode(getSidebarState().layout)).toBe("hidden");
    await waitFor(() => expect(sw("patrols")).not.toBeChecked());

    fireEvent.click(sw("patrols"));
    expect(getSidebarState().layout.items.find((i) => i.id === "patrols")?.hidden).toBeUndefined();
    fireEvent.click(sw("team"));
    expect(teamChatsMode(getSidebarState().layout)).toBe("chats");
  });

  it("showing teammates' chats again puts them back in the list of their own they had", async () => {
    const { panel } = await openPanel(setTeamChatsMode(setTeamChatsMode(BUILTIN_LAYOUT, "own"), "hidden"));
    // Hidden from a list of their own: the layout still remembers "own".
    expect(within(panel).getByTestId("customize-show-team")).not.toBeChecked();
    fireEvent.click(within(panel).getByTestId("customize-show-team"));
    expect(teamChatsMode(getSidebarState().layout)).toBe("own");
  });

  it('"More options" opens the full panel and closes this one', async () => {
    const { panel } = await openPanel();
    const fullPanel = renderHook(() => useSidebarCustomizerOpen());
    expect(fullPanel.result.current).toBe(false);
    fireEvent.click(within(panel).getByTestId("customize-more"));
    await waitFor(() => expect(screen.queryByTestId("customize-popover")).toBeNull());
    expect(fullPanel.result.current).toBe(true);
  });

  it("says a change is saved to the account", async () => {
    const { panel } = await openPanel();
    expect(within(panel).getByTestId("customize-save-state")).toHaveTextContent("Saved to your account, on every device.");
    fireEvent.click(within(panel).getByRole("button", { name: "Compact" }));
    expect(within(panel).getByTestId("customize-save-state")).toHaveTextContent("Saving…");
  });

  it("a read-only account can look but not change, and is told why", async () => {
    me.role = "viewer";
    const { panel } = await openPanel();
    await waitFor(() =>
      expect(within(panel).getByTestId("customize-save-state")).toHaveTextContent("Your account is read-only, so your sidebar can't be changed."),
    );
    expect(within(panel).getByRole("button", { name: "Compact" })).toBeDisabled();
    expect(within(panel).getByTestId("customize-show-patrols")).toBeDisabled();
    expect(option(panel, "Background tasks")).toBeDisabled();
  });
});
