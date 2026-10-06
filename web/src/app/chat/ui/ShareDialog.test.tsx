import { afterEach, describe, expect, it, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { ChatShareControls, ShareDialog, teamShareState } from "./ShareDialog";
import { ChatToastProvider } from "./ChatToasts";
import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import type { ConversationOutputs, OutputFile } from "./teamSharing";

// The share dialog (docs/TEAM-SHARING.md; spec "Share dialog"): one team
// block, always first, naming the team; one main button per state that
// completes the whole fix; one collapsed public-link row. A chat is shared
// with a team only inside a project shared with that team (ADR-0057), so
// every state below is a route to that rule, never around it.

const conversation = (
  over: Partial<ConversationSummary> = {},
): ConversationSummary => ({
  id: "c1",
  title: "Draft client recap",
  persona: "victoria",
  model: "m",
  pinned: false,
  updated_at: 1,
  ...over,
});

const project = (over: Partial<Project> = {}): Project => ({
  id: "p1",
  owner_email: "sam@x.com",
  name: "Knowertech",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
  ...over,
});

const file = (over: Partial<OutputFile> = {}): OutputFile => ({
  path: "out/recap_draft_v2.pdf",
  name: "recap_draft_v2.pdf",
  size: 412 * 1024,
  modified_at: 1_759_650_000,
  shared: true,
  ...over,
});

const outputs = (files: OutputFile[]): ConversationOutputs => ({
  outputs: files,
  total: files.length,
  shared_count: files.filter((f) => f.shared).length,
  team_visible: false,
});

type DialogProps = Parameters<typeof ShareDialog>[0];

function dialogProps(over: Partial<DialogProps> = {}): DialogProps {
  return {
    conversation: conversation(),
    project: null,
    myTeam: "Elcano",
    busy: false,
    copied: false,
    buildShareUrl: (t: string) => `https://fleet.example/shared/${t}`,
    onCreateLink: vi.fn(),
    onCopyLink: vi.fn(),
    onStopLink: vi.fn(),
    onShareWithTeam: vi.fn(async () => ({
      team_visible: true,
      shared_files: 2,
      total_files: 2,
    })),
    onStopSharingWithTeam: vi.fn(async () => ({
      team_visible: false,
      shared_files: 2,
      total_files: 2,
    })),
    onMoveAndShare: vi.fn(async () => ({
      team_visible: true,
      shared_files: 0,
      total_files: 0,
    })),
    onCreateSharedProject: vi.fn(),
    onShareProjectFirst: vi.fn(),
    onManageInSources: vi.fn(),
    loadOutputs: vi.fn(async () => outputs([])),
    onClose: vi.fn(),
    ...over,
  };
}

function renderDialog(over: Partial<DialogProps> = {}) {
  const props = dialogProps(over);
  render(
    <ChatToastProvider>
      <ShareDialog {...props} />
    </ChatToastProvider>,
  );
  return props;
}

const teamBlock = () => screen.getByTestId("share-team-block");

const teamProject = project({ team_id: "Elcano" });
const inTeamProject = conversation({ project_id: "p1" });

afterEach(cleanup);

describe("teamShareState", () => {
  const base = { moveTargets: [] as Project[] };
  it("maps each situation to its frame", () => {
    expect(
      teamShareState({ ...base, conversation: conversation(), project: null, myTeam: "Elcano", moveTargets: [teamProject] }),
    ).toBe("A1");
    expect(
      teamShareState({ ...base, conversation: conversation(), project: null, myTeam: "Elcano" }),
    ).toBe("A1b");
    expect(
      teamShareState({ ...base, conversation: inTeamProject, project: project(), myTeam: "Elcano" }),
    ).toBe("A2");
    expect(
      teamShareState({ ...base, conversation: inTeamProject, project: project(), myTeam: "" }),
    ).toBe("A3a");
    expect(
      teamShareState({ ...base, conversation: inTeamProject, project: teamProject, myTeam: "Elcano" }),
    ).toBe("A4");
    expect(
      teamShareState({ ...base, conversation: conversation({ project_id: "p1", team_visible: true }), project: teamProject, myTeam: "Elcano" }),
    ).toBe("A5");
    expect(
      teamShareState({ ...base, conversation: conversation({ project_id: "p1", share_token: "tok" }), project: teamProject, myTeam: "Elcano" }),
    ).toBe("A6");
    expect(
      teamShareState({ ...base, conversation: inTeamProject, project: project({ team_id: "Reklaim" }), myTeam: "Elcano" }),
    ).toBe("other-team");
  });

  it("an unread team never blocks a team project", () => {
    expect(
      teamShareState({ ...base, conversation: inTeamProject, project: teamProject, myTeam: undefined }),
    ).toBe("A4");
  });

  it("a shared chat always offers stopping, even with no team now", () => {
    expect(
      teamShareState({ ...base, conversation: conversation({ team_visible: true }), project: null, myTeam: "" }),
    ).toBe("A5");
  });
});

describe("ShareDialog — layout", () => {
  it("titles the chat and says where it lives and who sees it", () => {
    renderDialog({ conversation: inTeamProject, project: teamProject });
    expect(
      screen.getByRole("heading", { name: "Share “Draft client recap”" }),
    ).toBeInTheDocument();
    const where = screen.getByTestId("share-dialog-where");
    expect(where).toHaveTextContent("In Knowertech");
    expect(where).toHaveTextContent("Only you");
  });

  it("says 'Not in a project' and 'Public link' when that is the truth", () => {
    renderDialog({ conversation: conversation({ share_token: "tok" }) });
    const where = screen.getByTestId("share-dialog-where");
    expect(where).toHaveTextContent("Not in a project");
    expect(where).toHaveTextContent("Public link");
  });

  it("the team block comes before the public-link row", () => {
    renderDialog({ conversation: inTeamProject, project: teamProject });
    const team = teamBlock();
    const outside = screen.getByRole("region", { name: "Share outside your team" });
    expect(
      team.compareDocumentPosition(outside) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("says so rather than acting on nothing when the chat is gone", () => {
    renderDialog({ conversation: null });
    expect(screen.getByText("This chat is no longer available.")).toBeInTheDocument();
    expect(screen.queryByTestId("share-team-block")).toBeNull();
  });

  it("shows the server's refusal as an alert", () => {
    renderDialog({ error: "This chat's project isn't shared with your team." });
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This chat's project isn't shared with your team.",
    );
  });
});

describe("ShareDialog — A1: no project, a team project exists", () => {
  it("explains the rule, offers the picker, and Move and share does both", async () => {
    const other = project({ id: "p2", name: "Bento", team_id: "Elcano" });
    const props = renderDialog({ teamSharedProjects: [teamProject, other] });
    expect(teamBlock()).toHaveAttribute("data-state", "A1");
    expect(
      within(teamBlock()).getByRole("heading", { name: "Share with Elcano" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Pick one and this chat will move into it\./),
    ).toBeInTheDocument();
    const picker = screen.getByLabelText("Project shared with Elcano");
    fireEvent.change(picker, { target: { value: "p2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Move and share" }));
    });
    expect(props.onMoveAndShare).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      "p2",
    );
    // Every successful share confirms with the file toast.
    expect(await screen.findByTestId("chat-toast")).toHaveTextContent(
      "“Draft client recap” is shared with Elcano. Files it creates will be shared too.",
    );
  });

  it("lists only the caller's own team's projects", () => {
    renderDialog({
      teamSharedProjects: [
        teamProject,
        project({ id: "p3", name: "Elsewhere", team_id: "Reklaim" }),
      ],
    });
    const picker = screen.getByLabelText("Project shared with Elcano");
    expect(within(picker).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Knowertech",
    ]);
  });
});

describe("ShareDialog — A1b: no team project yet", () => {
  it("offers Create shared project and hands over the chat", () => {
    const props = renderDialog({ teamSharedProjects: [] });
    expect(teamBlock()).toHaveAttribute("data-state", "A1b");
    expect(screen.getByText(/Create one to move this chat into\./)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Create shared project" }));
    expect(props.onCreateSharedProject).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
    );
  });
});

describe("ShareDialog — A2: personal project", () => {
  it("names the project, and Share project first goes to it", () => {
    const props = renderDialog({ conversation: inTeamProject, project: project() });
    expect(teamBlock()).toHaveAttribute("data-state", "A2");
    expect(teamBlock()).toHaveTextContent(
      "Knowertech isn’t shared with Elcano, so its chats are private to you. Share the project first, then this chat.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Share project first" }));
    expect(props.onShareProjectFirst).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      "p1",
    );
  });
});

describe("ShareDialog — A3a: not on a team", () => {
  it("guides admins and members, and marks only the control unavailable", () => {
    renderDialog({ conversation: inTeamProject, project: project(), myTeam: "" });
    expect(teamBlock()).toHaveAttribute("data-state", "A3a");
    expect(
      within(teamBlock()).getByRole("heading", { name: "Share with your team" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/You’re not on a team yet/)).toBeInTheDocument();
    expect(screen.getByText(/Admins:/)).toBeInTheDocument();
    expect(screen.getByText(/Everyone else:/)).toBeInTheDocument();
    const control = screen.getByRole("button", {
      name: "Share with a team (unavailable)",
    });
    expect(control).toBeDisabled();
    expect(control).toHaveAttribute("aria-disabled", "true");
    expect(control.className).toContain("border-dashed");
    expect(control.querySelector("svg")).not.toBeNull();
    // Never grey the whole block.
    expect(teamBlock().className).not.toContain("opacity");
  });

  it("shows a known non-admin only the ask-an-admin line", () => {
    renderDialog({ myTeam: "", isAdmin: false });
    expect(screen.queryByText(/Admins:/)).toBeNull();
    expect(screen.getByText(/Everyone else:/)).toBeInTheDocument();
  });
});

describe("ShareDialog — A4: team project, not shared", () => {
  it("with no outputs yet, says files it creates will be shared too", async () => {
    renderDialog({ conversation: inTeamProject, project: teamProject });
    expect(teamBlock()).toHaveAttribute("data-state", "A4");
    expect(
      screen.getByText(
        "Teammates find it on Knowertech’s home, read it, and branch it into their own chat.",
      ),
    ).toBeInTheDocument();
    expect(await screen.findByTestId("share-file-line")).toHaveTextContent(
      "Files it creates will be shared too.",
    );
  });

  it("shares with every file by default, without touching exclusions, and toasts the count", async () => {
    const props = renderDialog({
      conversation: inTeamProject,
      project: teamProject,
      loadOutputs: vi.fn(async () =>
        outputs([file(), file({ path: "pacing_notes.json", name: "pacing_notes.json" })]),
      ),
    });
    expect(await screen.findByText("Includes 2 files")).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share with Elcano" }));
    });
    expect(props.onShareWithTeam).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      undefined,
    );
    const toastEl = await screen.findByTestId("chat-toast");
    expect(toastEl).toHaveTextContent(
      "“Draft client recap” is shared with Elcano, with 2 files.",
    );
    fireEvent.click(within(toastEl).getByRole("button", { name: "Manage" }));
    expect(props.onManageInSources).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      "p1",
    );
  });

  it("Choose… lists files newest first with size and date; unchecked files are sent as exclusions", async () => {
    const props = renderDialog({
      conversation: inTeamProject,
      project: teamProject,
      loadOutputs: vi.fn(async () =>
        outputs([
          file(),
          file({ path: "pacing_notes.json", name: "pacing_notes.json", size: 38 * 1024 }),
        ]),
      ),
    });
    fireEvent.click(await screen.findByRole("button", { name: "Choose…" }));
    const group = screen.getByRole("group", { name: "Files to share" });
    const boxes = within(group).getAllByRole("checkbox");
    expect(boxes).toHaveLength(2);
    boxes.forEach((b) => expect(b).toBeChecked());
    expect(within(group).getByText(/412\.0 KB ·/)).toBeInTheDocument();
    fireEvent.click(within(group).getByRole("checkbox", { name: /pacing_notes\.json/ }));
    expect(screen.getByText("Includes 1 file")).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share with Elcano" }));
    });
    expect(props.onShareWithTeam).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      ["pacing_notes.json"],
    );
  });

  it("keeps an earlier exclusion unchecked when the checklist opens", async () => {
    renderDialog({
      conversation: inTeamProject,
      project: teamProject,
      loadOutputs: vi.fn(async () =>
        outputs([file(), file({ path: "old.csv", name: "old.csv", shared: false })]),
      ),
    });
    expect(await screen.findByText("Includes 1 file")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Choose…" }));
    expect(screen.getByRole("checkbox", { name: /old\.csv/ })).not.toBeChecked();
  });

  it("names the team on the button, never the project", () => {
    renderDialog({ conversation: inTeamProject, project: teamProject });
    expect(screen.queryByRole("button", { name: /Knowertech/ })).toBeNull();
  });
});

describe("ShareDialog — A5: shared", () => {
  const shared = conversation({ project_id: "p1", team_visible: true });
  const three = () =>
    outputs([
      file(),
      file({ path: "a.csv", name: "a.csv" }),
      file({ path: "b.csv", name: "b.csv" }),
      file({ path: "c.csv", name: "c.csv", shared: false }),
    ]);

  it("says what teammates can do, counts files, and links to Sources", async () => {
    const props = renderDialog({
      conversation: shared,
      project: teamProject,
      loadOutputs: vi.fn(async () => three()),
    });
    expect(
      within(teamBlock()).getByRole("heading", { name: "Shared with Elcano" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Teammates can read it and branch it, but can’t edit it. Only Elcano members can open the link.",
      ),
    ).toBeInTheDocument();
    expect(await screen.findByTestId("share-file-line")).toHaveTextContent(
      "3 of 4 files shared",
    );
    fireEvent.click(screen.getByRole("button", { name: "Manage in Sources" }));
    expect(props.onManageInSources).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      "p1",
    );
  });

  it("Copy link for <team> copies the team link and confirms who can open it", async () => {
    const writeText = vi.fn(async () => {});
    Object.assign(navigator, { clipboard: { writeText } });
    renderDialog({ conversation: shared, project: teamProject });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy link for Elcano" }));
    });
    expect(writeText).toHaveBeenCalledWith(
      `${window.location.origin}/chat?team=c1`,
    );
    expect(await screen.findByTestId("chat-toast")).toHaveTextContent(
      "Link copied. Only Elcano members can open it.",
    );
  });

  it("A5b: Stop sharing confirms inline with the shared-file count", async () => {
    const props = renderDialog({
      conversation: shared,
      project: teamProject,
      loadOutputs: vi.fn(async () => three()),
    });
    await screen.findByTestId("share-file-line");
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing" }));
    const confirm = screen.getByRole("alertdialog", { name: "Stop sharing" });
    expect(confirm).toHaveTextContent(
      "Stop sharing with Elcano? 3 shared files will stop being shared too.",
    );
    fireEvent.click(within(confirm).getByRole("button", { name: "Keep sharing" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(props.onStopSharingWithTeam).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Stop sharing" }));
    await act(async () => {
      fireEvent.click(
        within(screen.getByRole("alertdialog")).getByRole("button", {
          name: "Stop sharing",
        }),
      );
    });
    expect(props.onStopSharingWithTeam).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
    );
  });

  it("A5b with no shared files says teammates lose access", () => {
    renderDialog({ conversation: shared, project: teamProject });
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing" }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent(
      "Stop sharing with Elcano? Teammates lose access right away.",
    );
  });
});

describe("ShareDialog — A6: team project with a public link", () => {
  it("warns about links, still shares with the team, and leaves the link on", async () => {
    const props = renderDialog({
      conversation: conversation({ project_id: "p1", share_token: "tok" }),
      project: teamProject,
    });
    expect(teamBlock()).toHaveAttribute("data-state", "A6");
    expect(
      screen.getByText(
        "Teammates can’t branch from a link, and it won’t show on Knowertech’s home.",
      ),
    ).toBeInTheDocument();
    // The public link row opens on its own because a link exists.
    expect(screen.getByLabelText("Share link URL")).toHaveValue(
      "https://fleet.example/shared/tok",
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Share with Elcano" }));
    });
    expect(props.onShareWithTeam).toHaveBeenCalled();
    expect(props.onStopLink).not.toHaveBeenCalled();
  });
});

describe("ShareDialog — other team's project", () => {
  it("says so and offers the move into the caller's own team project", () => {
    renderDialog({
      conversation: inTeamProject,
      project: project({ team_id: "Reklaim Ops" }),
      teamSharedProjects: [project({ id: "p2", name: "Bento", team_id: "Elcano" })],
    });
    expect(teamBlock()).toHaveTextContent(
      "Knowertech is shared with Reklaim Ops, and you’re not in that team.",
    );
    expect(screen.getByRole("button", { name: "Move and share" })).toBeEnabled();
  });
});

describe("ShareDialog — the public link row", () => {
  it("is collapsed by default and creating a link is a deliberate click", () => {
    const props = renderDialog();
    const toggle = screen.getByRole("button", { name: /Share outside your team/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent("Public link. Transcript only, never files.");
    expect(screen.queryByRole("button", { name: "Create public link" })).toBeNull();
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: "Create public link" }));
    expect(props.onCreateLink).toHaveBeenCalled();
  });

  it("keeps the two-step revoke", () => {
    const props = renderDialog({ conversation: conversation({ share_token: "tok" }) });
    fireEvent.click(screen.getByRole("button", { name: "Stop the public link" }));
    expect(
      screen.getByText("Anyone holding the link loses access at once."),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Keep the link" }));
    expect(props.onStopLink).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Stop the public link" }));
    fireEvent.click(screen.getByRole("button", { name: "Stop the public link" }));
    expect(props.onStopLink).toHaveBeenCalled();
  });

  it("copies the public link", () => {
    const props = renderDialog({ conversation: conversation({ share_token: "tok" }) });
    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    expect(props.onCopyLink).toHaveBeenCalledWith("https://fleet.example/shared/tok");
  });

  it("disables Create while a request is in flight", () => {
    renderDialog({ busy: true });
    fireEvent.click(screen.getByRole("button", { name: /Share outside your team/ }));
    expect(screen.getByRole("button", { name: "Creating…" })).toBeDisabled();
  });
});

describe("ChatShareControls — the chat header entry point", () => {
  it("shows Only you, and both chip and Share open the dialog", () => {
    const onOpen = vi.fn();
    render(<ChatShareControls conversation={{}} team="Elcano" onOpen={onOpen} />);
    const chip = screen.getByRole("button", { name: "Only you — open sharing" });
    fireEvent.click(chip);
    fireEvent.click(screen.getByRole("button", { name: "Share" }));
    expect(onOpen).toHaveBeenCalledTimes(2);
  });

  it("names the team when shared, and Public link for a link", () => {
    const { rerender } = render(
      <ChatShareControls conversation={{ team_visible: true }} team="Elcano" onOpen={() => {}} />,
    );
    expect(
      screen.getByRole("button", { name: "Shared with Elcano — open sharing" }),
    ).toBeInTheDocument();
    rerender(
      <ChatShareControls conversation={{ share_token: "t" }} team="Elcano" onOpen={() => {}} />,
    );
    expect(
      screen.getByRole("button", { name: "Public link — open sharing" }),
    ).toBeInTheDocument();
  });

  it("falls back to 'your team' rather than guessing a name", async () => {
    render(<ChatShareControls conversation={{ team_visible: true }} onOpen={() => {}} />);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Shared with your team — open sharing" }),
      ).toBeInTheDocument(),
    );
  });
});
