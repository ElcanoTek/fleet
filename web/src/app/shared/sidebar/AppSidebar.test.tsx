import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ConversationSummary } from "@/app/chat/ui/chat-experience";
import { setBootstrapForTest } from "@/app/lib/bootstrap";
import { AppSidebar } from "./AppSidebar";
import { takeChatHandoff } from "./chatHandoff";
import { resetSidebarStoreForTests } from "./store";
import { resetPatrolCacheForTests } from "./targets";
import { resetSidebarLibraryForTests } from "./useSidebarLibrary";

// The same sidebar on every page (docs/SIDEBAR.md): on Background tasks,
// Patrols, Settings, Guides and Analytics a person sees their pinned chats,
// projects and chats, and one click takes them back to any of them.

const navigate = vi.hoisted(() => vi.fn());
vi.mock("@/app/lib/navigation", async (original) => ({
  ...(await original<typeof import("@/app/lib/navigation")>()),
  navigate,
}));
const account = vi.hoisted(() => ({ role: "member" }));
vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: account.role, team_id: "", admin: false }),
}));
vi.mock("@/app/lib/useClientConfig", async (original) => {
  const real = await original<typeof import("@/app/lib/useClientConfig")>();
  return {
    ...real,
    useClientConfig: () => ({
      branding: { ...real.DEFAULT_BRANDING, app_name: "Acme", logo_url: "/api/brand/logo" },
      pills: [],
      models: null,
      teamChatsSharedByDefault: false,
      loading: false,
    }),
  };
});
// The patrol wizard is a lazy page of its own; only the call matters here.
const openPatrolWizard = vi.hoisted(() => vi.fn());
vi.mock("@/app/launchpad/patrolWizard", async (original) => ({
  ...(await original<typeof import("@/app/launchpad/patrolWizard")>()),
  openPatrolWizard,
}));

const conv = (over: Partial<ConversationSummary>): ConversationSummary => ({
  id: "c",
  title: "Untitled",
  persona: "default",
  model: "",
  pinned: false,
  updated_at: Math.floor(Date.now() / 1000) - 60,
  ...over,
});

function stubApi(fail: string[] = []) {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (fail.includes(`${method} ${url}`)) return json({}, 500);
      if (method !== "GET") return json({});
      if (url === "/api/conversations")
        return json({ conversations: [conv({ id: "own-1", title: "Budget pacing" }), conv({ id: "own-2", title: "Kept wrap", pinned: true })] });
      if (url === "/api/conversations?scope=shared")
        return json({ conversations: [conv({ id: "team-1", title: "Their plan", user_email: "dana@example.com", team_visible: true })] });
      if (url === "/api/conversations?archived=true") return json({ conversations: [] });
      if (url === "/api/projects")
        return json({ projects: [{ id: "p-1", owner_email: "me@example.com", name: "Playbook", mcp_servers: [], created_at: 1, updated_at: 1 }] });
      if (url === "/api/conversations/order") return json({ orders: {} });
      if (url === "/api/server-config") return json({});
      if (url === "/api/sidebar") return json({ layout: null, workspace_default: null });
      if (url === "/api/patrols") return json({ patrols: [] });
      return json({}, 404);
    }),
  );
}

const collapse = { collapsed: false, isNarrow: false, railReady: true, setCollapsed: () => {} };

function renderSidebar(props: Partial<Parameters<typeof AppSidebar>[0]> = {}) {
  return render(<AppSidebar activeView="orchestrator" sidebarOpen setSidebarOpen={() => {}} collapse={collapse} {...props} />);
}

const row = (id: string) => document.querySelector<HTMLElement>(`[data-conversation-id="${id}"] [data-rail-row]`)!;

beforeEach(() => {
  setBootstrapForTest({ session: { email: "me@example.com" } } as never);
  resetSidebarLibraryForTests();
  resetSidebarStoreForTests();
  resetPatrolCacheForTests();
  navigate.mockClear();
  openPatrolWizard.mockClear();
  takeChatHandoff();
  account.role = "member";
  stubApi();
});
afterEach(() => {
  cleanup();
  setBootstrapForTest(null);
  vi.unstubAllGlobals();
});

describe("AppSidebar — the sidebar on a page that is not Chat", () => {
  it("lists the person's pinned chats, projects, chats and teammates' chats", async () => {
    renderSidebar();
    await waitFor(() => expect(row("own-1")).toBeTruthy());
    expect(within(screen.getByTestId("pinned-section")).getByText("Kept wrap")).toBeInTheDocument();
    expect(within(screen.getByTestId("projects-section")).getByText("Playbook")).toBeInTheDocument();
    expect(within(screen.getByTestId("chats-section")).getByText("Budget pacing")).toBeInTheDocument();
    expect(within(screen.getByTestId("chats-section")).getByText("Their plan")).toBeInTheDocument();
    // Patrols and Background tasks lead; this page is marked as where we are.
    expect(screen.getByTestId("nav-to-patrols")).toHaveAttribute("href", "/patrols");
  });

  it("keeps the deployment's own name and logo, exactly as Chat draws them", async () => {
    renderSidebar();
    const logo = screen.getByAltText("Acme");
    expect(logo).toHaveAttribute("src", "/api/brand/logo");
    expect(logo).toHaveAttribute("width", "28");
    expect(screen.getByText("Internal")).toBeInTheDocument();
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  it("one click on a row is the way back to that chat", async () => {
    renderSidebar();
    await waitFor(() => expect(row("own-1")).toBeTruthy());
    fireEvent.click(row("own-1"));
    expect(navigate).toHaveBeenLastCalledWith("/chat?c=own-1");
    // A teammate's team-shared chat opens through the team link.
    fireEvent.click(row("team-1"));
    expect(navigate).toHaveBeenLastCalledWith("/chat?team=team-1");
  });

  it("hands what needs one of Chat's dialogs over to Chat", async () => {
    renderSidebar();
    await waitFor(() => expect(row("own-1")).toBeTruthy());
    fireEvent.click(within(row("own-1").closest("[data-conversation-id]") as HTMLElement).getByRole("button", { name: /Conversation options/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Share/ }));
    expect(navigate).toHaveBeenLastCalledWith("/chat?c=own-1");
    expect(takeChatHandoff()).toMatchObject({ kind: "chat", action: "share", conversation: { id: "own-1" } });
  });

  it("the + menu starts a chat in Chat, a patrol in the wizard, and a task on Background tasks", async () => {
    renderSidebar();
    fireEvent.click(screen.getByTestId("new-menu-button"));
    fireEvent.click(await screen.findByTestId("new-menu-task"));
    expect(navigate).toHaveBeenLastCalledWith("/orchestrator?new=1");

    fireEvent.click(screen.getByTestId("new-menu-button"));
    fireEvent.click(await screen.findByTestId("new-menu-patrol"));
    expect(openPatrolWizard).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByTestId("new-chat-button"));
    expect(navigate).toHaveBeenLastCalledWith("/chat?draft=");
    expect(takeChatHandoff()).toEqual({ kind: "new-chat", sealed: undefined });
  });

  it("Background tasks opens its own form from the + menu, and a read-only account is not offered it", async () => {
    const onNewTask = vi.fn();
    const view = renderSidebar({ onNewTask });
    fireEvent.click(screen.getByTestId("new-menu-button"));
    fireEvent.click(await screen.findByTestId("new-menu-task"));
    expect(onNewTask).toHaveBeenCalledTimes(1);
    expect(navigate).not.toHaveBeenCalled();
    view.unmount();

    renderSidebar({ onNewTask: null });
    fireEvent.click(screen.getByTestId("new-menu-button"));
    await screen.findByTestId("new-menu-chat");
    expect(screen.queryByTestId("new-menu-task")).toBeNull();
  });

  it("offers a read-only Viewer no change the API would refuse", async () => {
    account.role = "viewer";
    renderSidebar();
    await waitFor(() => expect(row("own-1")).toBeTruthy());
    // Nothing to create: no pen, no + menu.
    await waitFor(() => expect(screen.queryByTestId("new-menu-button")).toBeNull());
    expect(screen.queryByTestId("new-chat-button")).toBeNull();
    // An own chat's menu keeps only what changes nothing on the server.
    fireEvent.click(within(row("own-1").closest("[data-conversation-id]") as HTMLElement).getByRole("button", { name: /Conversation options/ }));
    await screen.findByRole("menuitem", { name: /Download chat/ });
    for (const name of [/^Pin/, /^Rename/, /^Labels/, /^Share/, /^Transfer/, /^Archive/, /^Delete/, /background task/, /workflow/, /patrol/i]) {
      expect(screen.queryByRole("menuitem", { name })).toBeNull();
    }
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    // A project the Viewer owns: no New chat, pin, rename, share, settings or delete.
    fireEvent.click(screen.getByRole("button", { name: "Project options for Playbook" }));
    await screen.findByRole("menuitem", { name: "Open project" });
    for (const name of [/^New chat/, /^Pin/, /^Rename/, /^Share/, /Project settings/, /Delete project/]) {
      expect(screen.queryByRole("menuitem", { name })).toBeNull();
    }
    expect(screen.queryByRole("button", { name: "New chat in Playbook" })).toBeNull();
    // Nothing is draggable into a project.
    expect(row("own-1").closest("[draggable=true]")).toBeNull();
  });

  it("says so in the sidebar when a change did not save", async () => {
    stubApi(["POST /api/conversations/own-1/pin"]);
    renderSidebar();
    await waitFor(() => expect(row("own-1")).toBeTruthy());
    fireEvent.click(within(row("own-1").closest("[data-conversation-id]") as HTMLElement).getByRole("button", { name: /Conversation options/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Pin/ }));
    expect(await screen.findByTestId("sidebar-error")).toHaveTextContent("Couldn’t pin “Budget pacing”. Try again.");
  });

  it("Ctrl+K puts the cursor in the search field", async () => {
    renderSidebar();
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    await waitFor(() => expect(screen.getByTestId("search-input")).toHaveFocus());
  });
});
