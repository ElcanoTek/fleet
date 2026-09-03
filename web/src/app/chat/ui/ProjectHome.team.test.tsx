import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { ProjectHome } from "./ProjectHome";
import type { Project } from "./ProjectsModal";
import type { ConversationSummary } from "./chat-experience";

// The project home is where the three P1 items land (C3 Team section, D2 Team
// learnings, E1 search) plus the A6 delete confirm — so this exercises them on
// one page, the way a member meets them.

const PROJECT: Project = {
  id: "p1",
  owner_email: "alice@x.com",
  name: "Quant",
  instructions: "",
  team_id: "quant",
  mcp_servers: [],
  created_at: 1767225600,
  updated_at: 1767225600,
};

const OWN_CHATS: ConversationSummary[] = [
  {
    id: "c1",
    title: "Spread study",
    persona: "victoria",
    model: "m",
    pinned: false,
    updated_at: 1767225600,
    project_id: "p1",
  },
  {
    id: "c2",
    title: "Vol surface",
    persona: "victoria",
    model: "m",
    pinned: false,
    updated_at: 1767225600,
    project_id: "p1",
  },
];

type Routes = Record<string, unknown>;

function mockRoutes(routes: Routes, onWrite?: (url: string, init: RequestInit) => void) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method && init.method !== "GET") {
        onWrite?.(url, init);
        return new Response(JSON.stringify({}), { status: 200 });
      }
      for (const [suffix, body] of Object.entries(routes)) {
        if (url.endsWith(suffix)) {
          return new Response(JSON.stringify(body), { status: 200 });
        }
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
}

function renderHome(
  over: Partial<Parameters<typeof ProjectHome>[0]> = {},
) {
  const props = {
    project: PROJECT,
    chats: OWN_CHATS,
    userEmail: "alice@x.com",
    isOwner: true,
    onBack: vi.fn(),
    onOpenChat: vi.fn(),
    onOpenTeamChat: vi.fn(),
    onNewChat: vi.fn(),
    onSaveInstructions: vi.fn(async () => true),
    onUpdateSettings: vi.fn(async () => true),
    myTeam: "quant",
    onDelete: vi.fn(),
    ...over,
  };
  render(<ProjectHome {...props} />);
  return props;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProjectHome — the Team section (C3)", () => {
  it("lists teammates' shared chats separately from your own, with the owner and a read-only note", async () => {
    mockRoutes({
      "/team-conversations": {
        conversations: [
          {
            id: "t1",
            title: "Bob's basis trade",
            user_email: "bob@x.com",
            updated_at: 1767225600,
          },
        ],
      },
    });
    const props = renderHome();

    const shared = await screen.findByText("Bob's basis trade");
    expect(screen.getByText(/bob · read-only/)).toBeInTheDocument();
    // Opening a teammate's chat goes to the read-only viewer, not the editor.
    fireEvent.click(shared);
    expect(props.onOpenTeamChat).toHaveBeenCalledWith("t1");
    expect(props.onOpenChat).not.toHaveBeenCalled();
  });

  it("teaches how to share when the section is empty", async () => {
    mockRoutes({ "/team-conversations": { conversations: [] } });
    renderHome();
    expect(
      await screen.findByText(/No shared chats yet\. Share one with your team from its ⋮ menu\./),
    ).toBeInTheDocument();
  });

  it("has no Team section at all in a personal project", async () => {
    mockRoutes({});
    renderHome({ project: { ...PROJECT, team_id: undefined } });
    await screen.findByText("Spread study");
    expect(screen.queryByText("Team")).toBeNull();
  });
});

describe("ProjectHome — search (E1)", () => {
  it("filters your chats and the team's from one field", async () => {
    mockRoutes({
      "/team-conversations": {
        conversations: [
          { id: "t1", title: "Bob's basis trade", user_email: "bob@x.com", updated_at: 1 },
        ],
      },
    });
    renderHome();
    await screen.findByText("Bob's basis trade");

    fireEvent.change(screen.getByLabelText("Search chats in this project"), {
      target: { value: "spread" },
    });

    expect(screen.getByText("Spread study")).toBeInTheDocument();
    expect(screen.queryByText("Vol surface")).toBeNull();
    expect(screen.queryByText("Bob's basis trade")).toBeNull();
    expect(screen.getByText(/No shared chats match/)).toBeInTheDocument();
  });
});

describe("ProjectHome — empty state (E2)", () => {
  it("names both filing paths and the payoff", async () => {
    mockRoutes({});
    renderHome({ chats: [] });
    const empty = await screen.findByText(/No chats yet\./);
    expect(empty).toHaveTextContent("drag a chat onto the project");
    expect(empty).toHaveTextContent("Move to project");
    expect(empty).toHaveTextContent("Chats in a project don’t expire");
  });
});

describe("ProjectHome — Team learnings (D2)", () => {
  const learnings = {
    "/memories": {
      memories: [
        {
          id: "m1",
          content: "quote spreads in bps",
          user_email: "bob@x.com",
          created_at: 1767225600,
        },
        {
          id: "m2",
          content: "alice's own note",
          user_email: "alice@x.com",
          created_at: 1767225600,
        },
      ],
    },
  };

  it("shows every entry with its author, and Retire as the default remove", async () => {
    const writes: { url: string; body: string }[] = [];
    mockRoutes(learnings, (url, init) =>
      writes.push({ url, body: String(init.body) }),
    );
    renderHome();

    const panel = await screen.findByTestId("team-learnings");
    expect(within(panel).getByText("quote spreads in bps")).toBeInTheDocument();
    expect(within(panel).getByText(/bob · /)).toBeInTheDocument();

    // The project OWNER may manage anyone's entry.
    fireEvent.click(within(panel).getAllByRole("button", { name: "Retire" })[0]);
    await waitFor(() => expect(writes.length).toBe(1));
    expect(writes[0].url).toContain("/api/projects/p1/memories/m1");
    expect(JSON.parse(writes[0].body)).toEqual({ retired: true });
  });

  it("a plain member manages only their own entries", async () => {
    mockRoutes(learnings);
    renderHome({ userEmail: "bob@x.com", isOwner: false });

    const panel = await screen.findByTestId("team-learnings");
    // Bob wrote m1 and can act on it; alice's m2 is hers.
    expect(within(panel).getAllByRole("button", { name: "Retire" })).toHaveLength(1);
    const alicesRow = within(panel).getByText("alice's own note").closest("li");
    expect(within(alicesRow as HTMLElement).queryByRole("button", { name: "Retire" })).toBeNull();
  });

  it("says where a learning comes from when there are none", async () => {
    mockRoutes({ "/memories": { memories: [] } });
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    expect(
      within(panel).getByText(/No team learnings yet\. Save one from any chat in this project/),
    ).toBeInTheDocument();
  });
});

describe("ProjectHome — delete confirm (A6)", () => {
  it("states what members lose, with counts, and offers the export", async () => {
    mockRoutes({
      "/impact": { memories: 4, chats: 9, members: 3, team_shared_chats: 2 },
    });
    const props = renderHome({ initialSettingsOpen: true });

    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete Quant?" });

    await waitFor(() =>
      expect(dialog).toHaveTextContent("4 team learnings will be lost"),
    );
    expect(dialog).toHaveTextContent("9 chats from 3 members will leave the project");
    expect(dialog).toHaveTextContent("2 chats shared with the team will stop being shared");
    expect(within(dialog).getByRole("link", { name: "Export first" })).toHaveAttribute(
      "href",
      "/api/projects/p1/export",
    );

    // Nothing is destroyed until the confirm is answered.
    expect(props.onDelete).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete project" }));
    expect(props.onDelete).toHaveBeenCalled();
  });
});

describe("ProjectHome — the three context layers (D3)", () => {
  it("names all three, in the order the prompt builder assembles them", async () => {
    mockRoutes({});
    renderHome();
    const helper = await screen.findByText(/Every chat here is fed by three layers/);
    expect(helper).toHaveTextContent("Instructions");
    expect(helper).toHaveTextContent("Team learnings");
    expect(helper).toHaveTextContent("My memory");
  });
});
