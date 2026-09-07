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
    onTransfer: vi.fn(async () => null),
    myTeam: "quant",
    onDelete: vi.fn(),
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
    // The old copy ("No shared chats yet. Share one with your team from its ⋮
    // menu.") was false from the owner's vantage — their own shared chats are
    // badged directly above — and instructed them to do what they had done.
    expect(
      await screen.findByText(/Nothing shared by your teammates yet\./),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Chats you share stay in your list above, marked with the team badge\./),
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

describe("ProjectHome — ownership transfer", () => {
  it("hands the project to a teammate, and never to the current owner", async () => {
    mockRoutes({ "/members": { members: ["alice@x.com", "bob@x.com"] } });
    const props = renderHome({ initialSettingsOpen: true });

    // Collapsed by default: a once-in-a-project action should not read as a
    // routine control in a dialog people open to rename things.
    expect(screen.queryByLabelText(/Transfer ownership of/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership…" }));

    const picker = await screen.findByLabelText("Transfer ownership of Quant");
    const options = within(picker as HTMLSelectElement)
      .getAllByRole("option")
      .map((o) => (o as HTMLOptionElement).value);
    // The current owner is not offered — handing it to yourself is a no-op.
    expect(options).toEqual(["", "bob@x.com"]);

    fireEvent.change(picker, { target: { value: "bob@x.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Transfer" }));

    // In-app confirm, not window.confirm(): it can name the project and the
    // new owner, and it looks like the rest of the app.
    const confirm = await screen.findByRole("dialog", {
      name: "Transfer Quant to bob@x.com?",
    });
    expect(props.onTransfer).not.toHaveBeenCalled();
    fireEvent.click(within(confirm).getByRole("button", { name: "Transfer" }));

    await waitFor(() => expect(props.onTransfer).toHaveBeenCalledWith("bob@x.com"));
  });

  it("says why there is nobody to transfer to, rather than an empty picker", async () => {
    mockRoutes({ "/members": { members: ["alice@x.com"] } });
    renderHome({ initialSettingsOpen: true });
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership…" }));
    expect(
      await screen.findByText(/Nobody else is on this project’s team yet/),
    ).toBeInTheDocument();
  });

  it("surfaces the server's reason when a transfer is refused", async () => {
    mockRoutes({ "/members": { members: ["alice@x.com", "bob@x.com"] } });
    renderHome({
      initialSettingsOpen: true,
      onTransfer: vi.fn(async () => "the new owner must be a member of the project's team"),
    });
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership…" }));
    const picker = await screen.findByLabelText("Transfer ownership of Quant");
    fireEvent.change(picker, { target: { value: "bob@x.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Transfer" }));
    const confirm = await screen.findByRole("dialog", {
      name: /^Transfer Quant to/,
    });
    fireEvent.click(within(confirm).getByRole("button", { name: "Transfer" }));

    expect(
      await screen.findByText(/must be a member of the project’s team|must be a member of the project's team/),
    ).toBeInTheDocument();
  });
});

// The panel's own failure modes. Each of these rendered a confident, wrong
// statement about the project — the worst thing a surface like this can do.
describe("ProjectHome — failure states don't lie", () => {
  it("does not report an empty team when the members lookup failed", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).endsWith("/members")) {
          return new Response("boom", { status: 500 });
        }
        return new Response(JSON.stringify({}), { status: 200 });
      }),
    );
    renderHome({ initialSettingsOpen: true });
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership…" }));

    expect(
      await screen.findByText(/Couldn’t load this project’s members/),
    ).toBeInTheDocument();
    // The "nobody else is on this project's team yet" copy sends the owner off
    // to fix a problem that may not exist.
    expect(screen.queryByText(/Nobody else is on this project/)).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

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
  it("says whose files it lists, rather than promising the project's", async () => {
    mockRoutes({ "/files": { files: [] } });
    renderHome();
    // The old copy promised "files from this project's chats", which for a
    // teammate described files that exist and are withheld by design.
    const empty = await screen.findByText(/chats in this project appear here/);
    expect(empty).toHaveTextContent("Files from your chats in this project appear here.");
    expect(screen.queryByText(/uploads, generated/)).toBeNull();
  });
});

describe("ProjectHome — Team section, the owner's vantage (C3)", () => {
  it("counts the viewer's own shares in the empty state", async () => {
    mockRoutes({ "/team-conversations": { conversations: [] } });
    renderHome({
      chats: [
        { ...OWN_CHATS[0], team_visible: true },
        { ...OWN_CHATS[1], team_visible: true },
      ],
    });
    expect(
      await screen.findByText(/You’ve shared 2 chats with the team\./),
    ).toBeInTheDocument();
  });

  it("does not mention shares the viewer has not made", async () => {
    mockRoutes({ "/team-conversations": { conversations: [] } });
    renderHome();
    await screen.findByText(/Nothing shared by your teammates yet\./);
    expect(screen.queryByText(/You’ve shared/)).toBeNull();
  });
});

describe("ProjectHome — un-ticking Share with my team (14)", () => {
  it("quotes how many of the teammates' chats it moves", async () => {
    mockRoutes({
      "/impact": {
        memories: 1,
        chats: 9,
        members: 3,
        team_shared_chats: 2,
        chats_from_teammates: 4,
        teammates_with_chats: 2,
      },
    });
    renderHome({ initialSettingsOpen: true });

    fireEvent.click(screen.getByRole("checkbox"));
    const dialog = await screen.findByRole("dialog", {
      name: "Stop sharing Quant with quant?",
    });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "4 chats from teammates will move to their unfiled chats.",
      ),
    );
    // Un-ticking is only staged once the confirm is answered.
    expect(screen.getByRole("checkbox")).toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop sharing" }));
    expect(screen.getByRole("checkbox")).not.toBeChecked();
  });

  it("keeps the tick when the confirm is cancelled", async () => {
    mockRoutes({ "/impact": { memories: 0, chats: 0, members: 0, team_shared_chats: 0, chats_from_teammates: 0 } });
    renderHome({ initialSettingsOpen: true });

    fireEvent.click(screen.getByRole("checkbox"));
    const dialog = await screen.findByRole("dialog", { name: /^Stop sharing/ });
    expect(dialog).toHaveTextContent("No chats from teammates are filed here");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("checkbox")).toBeChecked();
  });

  it("says it does not know rather than claiming nothing moves", async () => {
    // An older server, or a failed read: the count is simply absent. Rendering
    // that as 0 is the LeaveTeamImpact bug (internal/store/team_sharing.go) —
    // "we could not work out what this costs you" is not "nothing".
    mockRoutes({ "/impact": { memories: 1, chats: 2, members: 2, team_shared_chats: 1 } });
    renderHome({ initialSettingsOpen: true });

    fireEvent.click(screen.getByRole("checkbox"));
    const dialog = await screen.findByRole("dialog", { name: /^Stop sharing/ });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "Chats from teammates will move to their unfiled chats — we couldn’t work out how many.",
      ),
    );
    expect(dialog).not.toHaveTextContent("0 chats");
    expect(dialog).not.toHaveTextContent("No chats from teammates");
  });
});
