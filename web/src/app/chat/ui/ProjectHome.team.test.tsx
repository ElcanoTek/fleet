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
// learnings, E1 search) — so this exercises them on one page, the way a member
// meets them. Settings (rename, sharing, transfer, delete) are
// ProjectSettingsDialog and are tested there.

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
    onOpenSettings: vi.fn(),
    myTeam: "quant",
    ...over,
  };
  render(<ProjectHome {...props} />);
  return props;
}

// The team-learnings row keeps its actions under one ⋮ menu, like every other
// row in fleet. The Menu surface is portaled to <body>, so its items are
// queried from `screen` rather than from inside the panel.
function openRowMenu(row: HTMLElement) {
  fireEvent.click(
    within(row).getByRole("button", { name: /^Actions for/ }),
  );
}

function rowFor(panel: HTMLElement, content: string): HTMLElement {
  return within(panel).getByText(content).closest("li") as HTMLElement;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProjectHome — the Team section (C3)", () => {
  it("lists teammates' shared chats separately from your own, with the owner and what you can do", async () => {
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
    expect(screen.getByText("bob@x.com")).toBeInTheDocument();
    expect(screen.getByText("Read, branch")).toBeInTheDocument();
    // Opening a teammate's chat goes to the read-only viewer, not the editor.
    fireEvent.click(shared);
    expect(props.onOpenTeamChat).toHaveBeenCalledWith("t1");
    expect(props.onOpenChat).not.toHaveBeenCalled();
  });

  it("says where teammates' chats will appear when the section is empty", async () => {
    mockRoutes({ "/team-conversations": { conversations: [] } });
    renderHome();
    expect(
      await screen.findByText("Nothing from teammates yet. Chats they share show up here."),
    ).toBeInTheDocument();
  });

  it("has no Team section at all in a personal project", async () => {
    mockRoutes({});
    renderHome({ project: { ...PROJECT, team_id: undefined } });
    await screen.findByText("Spread study");
    expect(screen.queryByText("Shared by your team")).toBeNull();
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
  // The empty-state paragraph shows only once the getting-started card is
  // gone (it replaces the paragraph while it is up).
  it("names both ways in, and who can see a new chat", async () => {
    mockRoutes({ "/my-state": { has_shared_chat: true, kept_personal: false, sources_open: {} } });
    renderHome({ chats: [] });
    const empty = await screen.findByText(/No chats yet\./);
    expect(empty).toHaveTextContent(
      "No chats yet. Start one with New chat, or move one here from the sidebar. New chats can only be seen by you until you share them.",
    );
    expect(screen.queryByTestId("getting-started")).toBeNull();
  });

  it("is replaced by the getting-started card until the person has shared a chat", async () => {
    mockRoutes({ "/my-state": { has_shared_chat: false, kept_personal: false, sources_open: {} } });
    renderHome({ chats: [] });
    expect(await screen.findByTestId("getting-started")).toBeInTheDocument();
    expect(screen.queryByText(/No chats yet\./)).toBeNull();
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
    openRowMenu(rowFor(panel, "quote spreads in bps"));
    fireEvent.click(screen.getByRole("menuitem", { name: /^Retire/ }));
    await waitFor(() => expect(writes.length).toBe(1));
    expect(writes[0].url).toContain("/api/projects/p1/memories/m1");
    expect(JSON.parse(writes[0].body)).toEqual({ retired: true });
  });

  it("a plain member manages only their own entries", async () => {
    mockRoutes(learnings);
    renderHome({ userEmail: "bob@x.com", isOwner: false });

    const panel = await screen.findByTestId("team-learnings");
    // Bob wrote m1 and can act on it; alice's m2 is hers.
    expect(
      within(panel).getAllByRole("button", { name: /^Actions for/ }),
    ).toHaveLength(1);
    const alicesRow = rowFor(panel, "alice's own note");
    expect(
      within(alicesRow).queryByRole("button", { name: /^Actions for/ }),
    ).toBeNull();
  });

  it("keeps every action under one ⋮ menu, reachable by keyboard", async () => {
    mockRoutes(learnings);
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    const row = rowFor(panel, "quote spreads in bps");

    // Five text links per entry was the finding; the row now shows one
    // control, and it is a real button so focus reaches it (the reveal is
    // opacity, driven by hover AND group-focus-within).
    const kebab = within(row).getByRole("button", { name: /^Actions for/ });
    kebab.focus();
    expect(kebab).toHaveFocus();

    fireEvent.click(kebab);
    const menu = screen.getByRole("menu");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((i) => (i.textContent ?? "").split("Stop using")[0].trim()),
    ).toEqual(["Pin", "Edit", "Retire", "Delete"]);
  });

  it("sorts pinned entries first and shows the pin as a glyph", async () => {
    mockRoutes({
      "/memories": {
        memories: [
          { id: "m1", content: "unpinned first from the server", user_email: "alice@x.com" },
          { id: "m2", content: "pinned second", user_email: "alice@x.com", pinned: true },
        ],
      },
    });
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    const rows = within(panel).getAllByRole("listitem");
    expect(rows[0]).toHaveTextContent("pinned second");
    expect(rows[1]).toHaveTextContent("unpinned first from the server");
    // A labelled glyph beside author · date, not a word in the sentence.
    expect(within(rows[0]).getByRole("img", { name: "Pinned" })).toBeInTheDocument();
    expect(rows[0].querySelector("svg use")).toHaveAttribute(
      "href",
      "/icons/core-icons.svg#pin",
    );
    expect(rows[0]).not.toHaveTextContent("Pinned");
    expect(within(rows[1]).queryByRole("img", { name: "Pinned" })).toBeNull();
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

// The panel's own failure modes. Each of these rendered a confident, wrong
// statement about the project — the worst thing a surface like this can do.
describe("ProjectHome — failure states don't lie", () => {
  it("does not report an empty Team section when the shared-chats lookup failed", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).endsWith("/team-conversations")) {
          return new Response("boom", { status: 500 });
        }
        return new Response(JSON.stringify({}), { status: 200 });
      }),
    );
    renderHome();
    expect(
      await screen.findByText(/Couldn’t load your team’s shared chats \(HTTP 500\)/),
    ).toBeInTheDocument();
    // "Nothing shared yet" is a statement about the team's work; a failed read
    // cannot make it.
    expect(screen.queryByText(/Nothing shared by your teammates yet/)).toBeNull();
  });

  it("does not report an empty project when the learnings lookup failed", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).endsWith("/memories")) {
          return new Response("boom", { status: 500 });
        }
        return new Response(JSON.stringify({}), { status: 200 });
      }),
    );
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    expect(within(panel).getByText(/Couldn’t load team learnings/)).toBeInTheDocument();
    expect(within(panel).queryByText(/No team learnings yet/)).toBeNull();
  });
});

describe("ProjectHome — team learnings, destructive and lossy actions", () => {
  const learnings = {
    "/memories": {
      memories: [
        {
          id: "m1",
          content: "quote spreads in bps",
          user_email: "alice@x.com",
          created_at: 1767225600,
        },
      ],
    },
  };

  it("asks in a dialog before deleting a learning for good", async () => {
    const writes: { url: string; method: string }[] = [];
    mockRoutes(learnings, (url, init) =>
      writes.push({ url, method: String(init.method) }),
    );
    renderHome();
    const panel = await screen.findByTestId("team-learnings");

    // The inline "Delete for good · Keep" swap was easy to miss: no dialog
    // anywhere, and the second click was permanent.
    openRowMenu(rowFor(panel, "quote spreads in bps"));
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog", {
      name: "Delete this team learning for good?",
    });
    expect(writes).toHaveLength(0);
    // It quotes the entry, and points at the reversible action instead.
    expect(dialog).toHaveTextContent("quote spreads in bps");
    expect(dialog).toHaveTextContent("retire it instead");

    fireEvent.click(within(dialog).getByRole("button", { name: "Delete for good" }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0].method).toBe("DELETE");
  });

  it("never leaves a delete pending across another action", async () => {
    mockRoutes(learnings);
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    const row = rowFor(panel, "quote spreads in bps");

    // Keep dismisses it…
    openRowMenu(row);
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep" }));
    expect(screen.queryByRole("dialog")).toBeNull();

    // …and the confirm never survives a different action. Before, retiring an
    // entry left the row reading "Restore · Delete for good · Keep" with no
    // delete pending at all — one click from destroying it, and "Keep" did
    // nothing.
    openRowMenu(rowFor(panel, "quote spreads in bps"));
    fireEvent.click(screen.getByRole("menuitem", { name: /^Retire/ }));
    await waitFor(() =>
      expect(screen.queryByRole("menuitem")).toBeNull(),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete for good" })).toBeNull();
  });

  it("keeps the editor and the typed text when the save is refused", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (init?.method === "PATCH") return new Response("nope", { status: 403 });
        if (String(input).endsWith("/memories")) {
          return new Response(JSON.stringify(learnings["/memories"]), { status: 200 });
        }
        return new Response(JSON.stringify({}), { status: 200 });
      }),
    );
    renderHome();
    const panel = await screen.findByTestId("team-learnings");
    openRowMenu(rowFor(panel, "quote spreads in bps"));
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit" }));

    const editor = within(panel).getByLabelText("Edit team learning");
    fireEvent.change(editor, { target: { value: "a long careful rewrite" } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save" }));

    // Tearing the editor down first threw the rewrite away on any rejection.
    await waitFor(() =>
      expect(within(panel).getByLabelText("Edit team learning")).toHaveValue(
        "a long careful rewrite",
      ),
    );
  });
});

// ── The copy findings from the project-home QA pass ──────────────────────────

describe("ProjectHome — the header chip (C9)", () => {
  it("names the team it is shared with", async () => {
    mockRoutes({});
    renderHome();
    // "Shared with team" was true and useless: the page looked normal to an
    // owner whose team had been changed under them.
    expect(await screen.findByText("Shared with quant")).toBeInTheDocument();
  });

  it("tells an owner who is no longer in that team what to do about it", async () => {
    mockRoutes({});
    renderHome({ myTeam: "research" });
    expect(
      await screen.findByText(
        "You’re no longer in quant. Share this project with research instead, or make it personal.",
      ),
    ).toBeInTheDocument();
  });

  it("does not claim a replacement team when the owner has none", async () => {
    mockRoutes({});
    renderHome({ myTeam: "" });
    expect(
      await screen.findByText(/You’re no longer in quant, and you aren’t in a team now\./),
    ).toBeInTheDocument();
  });

  it("stays quiet while the team read is still out", async () => {
    mockRoutes({});
    renderHome({ myTeam: undefined });
    await screen.findByText("Shared with quant");
    expect(screen.queryByText(/You’re no longer in/)).toBeNull();
  });

  it("stays quiet for a member, who cannot act on it", async () => {
    mockRoutes({});
    renderHome({ isOwner: false, myTeam: "research" });
    await screen.findByText("Shared with quant");
    expect(screen.queryByText(/You’re no longer in/)).toBeNull();
  });
});

describe("ProjectHome — Sources (C4 / B1)", () => {
  it("says whose files it lists, per project type", async () => {
    mockRoutes({ "/files": { groups: [], truncated: false, files: [] } });
    renderHome({ project: { ...PROJECT, team_id: undefined } });
    const empty = await screen.findByText(/chats in this project appear here/);
    expect(empty).toHaveTextContent("Files from your chats in this project appear here.");
    expect(screen.queryByText(/uploads, generated/)).toBeNull();
    cleanup();

    renderHome();
    expect(
      await screen.findByText(
        "Your files and your team’s. Files in your shared chats are shared automatically. Adjust that here.",
      ),
    ).toBeInTheDocument();
  });
});

describe("ProjectHome — Your chats, the owner's vantage (C3)", () => {
  it("counts how many of the viewer's chats are shared with the team", async () => {
    mockRoutes({ "/team-conversations": { conversations: [] } });
    renderHome({
      chats: [
        { ...OWN_CHATS[0], team_visible: true },
        { ...OWN_CHATS[1], team_visible: false },
      ],
    });
    expect(await screen.findByText("1 of 2 shared with quant")).toBeInTheDocument();
  });

  it("does not count shares in a personal project", async () => {
    mockRoutes({});
    renderHome({ project: { ...PROJECT, team_id: undefined } });
    await screen.findByText("Spread study");
    expect(screen.queryByText(/of 2 shared with/)).toBeNull();
  });
});

describe("ProjectHome — settings live in one dialog", () => {
  it("the owner's gear asks the parent for ProjectSettingsDialog instead of opening its own", async () => {
    mockRoutes({});
    const props = renderHome();
    fireEvent.click(await screen.findByRole("button", { name: "Project settings" }));
    expect(props.onOpenSettings).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: /^Settings for/ })).toBeNull();
  });

  it("a member has no gear", async () => {
    mockRoutes({});
    renderHome({ isOwner: false, userEmail: "bob@x.com" });
    await screen.findByTestId("project-home");
    expect(screen.queryByRole("button", { name: "Project settings" })).toBeNull();
  });
});
