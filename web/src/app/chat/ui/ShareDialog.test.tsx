import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ShareDialog } from "./ShareDialog";
import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// One dialog, two audiences (ADR-0057). Team sharing is offered only for a
// chat inside a TEAM-SHARED project — the project home's Team section is the
// only place a teammate looks, so a team-shared chat outside one would be
// visible to people with no surface listing it. When the toggle is
// unavailable the helper text has to say WHICH situation the reader is in,
// because the two have different fixes.

const conversation = (over: Partial<ConversationSummary> = {}): ConversationSummary => ({
  id: "c1",
  title: "Spread study",
  persona: "victoria",
  model: "m",
  pinned: false,
  updated_at: 1,
  ...over,
});

const project = (over: Partial<Project> = {}): Project => ({
  id: "p1",
  owner_email: "alice@x.com",
  name: "Quant",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
  ...over,
});

type DialogProps = Parameters<typeof ShareDialog>[0];

function dialogProps(over: Partial<DialogProps> = {}): DialogProps {
  return {
    conversation: conversation(),
    project: null,
    myTeam: "quant",
    busy: false,
    copied: false,
    buildShareUrl: (t: string) => `https://fleet.example/shared/${t}`,
    onCreateLink: vi.fn(),
    onCopyLink: vi.fn(),
    onStopLink: vi.fn(),
    onSetTeamShared: vi.fn(),
    onOpenProjectSettings: vi.fn(),
    onClose: vi.fn(),
    ...over,
  };
}

function renderDialog(over: Partial<DialogProps> = {}) {
  const props = dialogProps(over);
  render(<ShareDialog {...props} />);
  return props;
}

afterEach(cleanup);

describe("ShareDialog", () => {
  it("disables team sharing for a chat in no project, and says how to fix it", () => {
    renderDialog();
    expect(screen.getByLabelText(/Share with/)).toBeDisabled();
    expect(
      screen.getByText(/Move this chat into a team-shared project/),
    ).toBeInTheDocument();
  });

  it("disables team sharing inside a PERSONAL project and points at the project", () => {
    const props = renderDialog({
      conversation: conversation({ project_id: "p1" }),
      project: project(),
    });
    expect(screen.getByLabelText(/Share with/)).toBeDisabled();
    expect(screen.getByText(/isn’t shared with your team/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "project settings" }));
    expect(props.onOpenProjectSettings).toHaveBeenCalledWith("p1");
  });

  it("enables the toggle inside a team-shared project and names the team", () => {
    const props = renderDialog({
      conversation: conversation({ project_id: "p1" }),
      project: project({ team_id: "quant" }),
    });
    const toggle = screen.getByLabelText("Share with quant");
    expect(toggle).toBeEnabled();
    expect(toggle).not.toBeChecked();

    fireEvent.click(toggle);
    expect(props.onSetTeamShared).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      true,
    );
  });

  it("reflects an already team-shared chat and can revoke it", () => {
    const props = renderDialog({
      conversation: conversation({ project_id: "p1", team_visible: true }),
      project: project({ team_id: "quant" }),
    });
    const toggle = screen.getByLabelText("Share with quant");
    expect(toggle).toBeChecked();
    fireEvent.click(toggle);
    expect(props.onSetTeamShared).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      false,
    );
  });

  it("keeps the link scope separate: no link exists until one is created", () => {
    const props = renderDialog();
    // Opening the dialog must not mint a public link as a side effect.
    expect(screen.queryByLabelText("Share link URL")).toBeNull();
    expect(props.onCreateLink).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Create link" }));
    expect(props.onCreateLink).toHaveBeenCalled();
  });

  it("shows the URL and the wider audience once a link exists", () => {
    const props = renderDialog({
      conversation: conversation({ share_token: "tok" }),
    });
    expect(screen.getByLabelText("Share link URL")).toHaveValue(
      "https://fleet.example/shared/tok",
    );
    expect(screen.getByText(/Anyone with this link/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    expect(props.onCopyLink).toHaveBeenCalledWith(
      "https://fleet.example/shared/tok",
    );
    // Revoking asks first — the URL dies for everyone holding it.
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing the link" }));
    expect(props.onStopLink).not.toHaveBeenCalled();
    expect(screen.getByText(/Anyone holding the link loses access/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing" }));
    expect(props.onStopLink).toHaveBeenCalled();
  });

  it("lets the reader keep the link after arming the revoke", () => {
    const props = renderDialog({
      conversation: conversation({ share_token: "tok" }),
    });
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing the link" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep the link" }));
    expect(props.onStopLink).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Stop sharing the link" })).toBeInTheDocument();
  });

  it("disables Create and Stop while a share request is in flight", () => {
    const create = renderDialog({ busy: true });
    const createBtn = screen.getByRole("button", { name: "Creating…" });
    expect(createBtn).toBeDisabled();
    fireEvent.click(createBtn);
    expect(create.onCreateLink).not.toHaveBeenCalled();
    cleanup();

    const stop = renderDialog({
      busy: true,
      conversation: conversation({ share_token: "tok" }),
    });
    const stopBtn = screen.getByRole("button", { name: "Stop sharing the link" });
    expect(stopBtn).toBeDisabled();
    fireEvent.click(stopBtn);
    expect(stop.onStopLink).not.toHaveBeenCalled();
  });

  it("says so rather than acting on nothing when the chat is gone", () => {
    renderDialog({ conversation: null });
    expect(screen.getByText(/no longer available/)).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });
});

// The dialog's gate must match the server's, which refuses a share with no
// audience or no home (ADR-0057) — otherwise it offers a control the API
// rejects — and must never block a REVOKE, which the server never refuses.
describe("ShareDialog — the team toggle tracks what the server will accept", () => {
  it("refuses to offer sharing into a project belonging to another team", () => {
    renderDialog({
      project: project({ team_id: "ops" }),
      myTeam: "quant",
    });
    const box = screen.getByRole("checkbox", { name: /Share with/ });
    expect(box).toBeDisabled();
    expect(screen.getByText(/which you aren’t in/)).toBeInTheDocument();
  });

  it("lets the owner un-share even after the pairing has broken", () => {
    // Shared with quant, but the project is no longer shared with the owner's
    // team — the exact state in which the owner most needs the control, and
    // the one a `project.team_id`-only gate disabled.
    const onSetTeamShared = vi.fn();
    renderDialog({
      conversation: conversation({ team_visible: true }),
      project: project({ team_id: undefined }),
      myTeam: "quant",
      onSetTeamShared,
    });
    const box = screen.getByRole("checkbox", { name: /Share with/ });
    expect(box).not.toBeDisabled();
    expect(box).toBeChecked();
    fireEvent.click(box);
    expect(onSetTeamShared).toHaveBeenCalledWith(expect.anything(), false);
  });

  it("does not nest the project-settings button inside the checkbox label", () => {
    // A <button> inside a <label> activates that label's control when clicked,
    // so "project settings" would also toggle team sharing.
    renderDialog({ project: project({ team_id: undefined }), myTeam: "quant" });
    const link = screen.getByRole("button", { name: "project settings" });
    expect(link.closest("label")).toBeNull();
  });
});

it("Escape closes the dialog", () => {
  const props = renderDialog();
  fireEvent.keyDown(document, { key: "Escape" });
  expect(props.onClose).toHaveBeenCalled();
});

// ─────────────────────────────────────────────────────────────────────────────
// QA C-2 / B-4: ONE disabled treatment, applied to every unavailable state.
//
// `disabled` alone was in the DOM already and read as a broken control: the
// box still looked live and the pointer stayed an arrow, so a user clicked it
// and nothing happened. The treatment is now dimmed + not-allowed +
// aria-disabled, and it has to hold in EVERY state the server would refuse —
// that is what these tests enumerate.

// The single treatment, asserted in one place so a new unavailable state
// cannot quietly ship half of it.
function expectUnavailableTreatment() {
  const box = screen.getByRole("checkbox", { name: /Share with/ });
  expect(box).toBeDisabled();
  expect(box).toHaveAttribute("aria-disabled", "true");
  expect(box.className).toContain("cursor-not-allowed");
  expect(box.className).toContain("opacity-50");
  // The pointer target the user actually aims at is the label; it carries the
  // same treatment, and the wrapper carries the cursor a disabled input eats.
  const label = screen.getByText(/Share with team/);
  expect(label.tagName).toBe("LABEL");
  expect(label.className).toContain("cursor-not-allowed");
  expect(label.className).toContain("opacity-50");
  expect(box.parentElement?.className).toContain("cursor-not-allowed");
  return box;
}

const teamProject = (over: Partial<Project> = {}) =>
  project({ id: "p2", name: "Index desk", team_id: "quant", ...over });

describe("ShareDialog — one disabled treatment in every unavailable state", () => {
  const unavailableStates: Array<[string, Partial<DialogProps>]> = [
    // The caller's relationship to the project (owner / teammate / outsider)
    // crossed with the project's own state (none / personal / team-shared).
    ["owner, no project", { userEmail: "alice@x.com", project: null }],
    [
      "owner, personal project",
      {
        userEmail: "alice@x.com",
        conversation: conversation({ project_id: "p1" }),
        project: project({ owner_email: "alice@x.com" }),
      },
    ],
    [
      "teammate (member), personal project owned by someone else",
      {
        userEmail: "bob@x.com",
        conversation: conversation({ project_id: "p1" }),
        project: project({ owner_email: "alice@x.com" }),
      },
    ],
    [
      "outsider: the project is shared with a team the caller isn't in",
      {
        userEmail: "bob@x.com",
        conversation: conversation({ project_id: "p1" }),
        project: project({ team_id: "ops" }),
        myTeam: "quant",
      },
    ],
    [
      "caller has no team, no project",
      { userEmail: "bob@x.com", myTeam: "", project: null },
    ],
    [
      "caller has no team, personal project",
      { userEmail: "bob@x.com", myTeam: "", project: project() },
    ],
    [
      "caller has no team, team-shared project",
      {
        userEmail: "bob@x.com",
        myTeam: "",
        conversation: conversation({ project_id: "p2" }),
        project: teamProject(),
      },
    ],
  ];

  for (const [name, over] of unavailableStates) {
    it(`${name}: dimmed, not-allowed, aria-disabled, and inert`, () => {
      const props = renderDialog(over);
      const box = expectUnavailableTreatment();
      // Genuinely non-interactive, not a handler that no-ops.
      fireEvent.click(box);
      expect(props.onSetTeamShared).not.toHaveBeenCalled();
    });
  }

  const availableStates: Array<[string, Partial<DialogProps>]> = [
    [
      "owner of a team-shared project",
      {
        userEmail: "alice@x.com",
        conversation: conversation({ project_id: "p2" }),
        project: teamProject({ owner_email: "alice@x.com" }),
      },
    ],
    [
      "teammate in someone else's team-shared project",
      {
        userEmail: "bob@x.com",
        conversation: conversation({ project_id: "p2" }),
        project: teamProject({ owner_email: "alice@x.com" }),
      },
    ],
    [
      "already shared, pairing since broken — the revoke must stay live",
      {
        userEmail: "alice@x.com",
        conversation: conversation({ project_id: "p1", team_visible: true }),
        project: project(),
      },
    ],
  ];

  for (const [name, over] of availableStates) {
    it(`${name}: the toggle is live and carries no disabled treatment`, () => {
      const props = renderDialog(over);
      const box = screen.getByRole("checkbox", { name: /Share with/ });
      expect(box).toBeEnabled();
      expect(box).not.toHaveAttribute("aria-disabled");
      expect(box.className).not.toContain("cursor-not-allowed");
      fireEvent.click(box);
      expect(props.onSetTeamShared).toHaveBeenCalled();
    });
  }

  it("uses the same treatment while a share request is in flight", () => {
    renderDialog({
      busy: true,
      userEmail: "alice@x.com",
      conversation: conversation({ project_id: "p2" }),
      project: teamProject(),
    });
    expectUnavailableTreatment();
  });
});

// The Recommended half of the finding: each unavailable state OFFERS its fix
// rather than only naming it.
describe("ShareDialog — every unavailable state offers its own fix", () => {
  it("no project + team-shared projects to choose from → Move to project", () => {
    const props = renderDialog({
      userEmail: "alice@x.com",
      project: null,
      teamSharedProjects: [teamProject(), teamProject({ id: "p3", name: "Macro" })],
      onMoveToProject: vi.fn(),
    });
    expectUnavailableTreatment();
    const select = screen.getByLabelText("Move to project");
    // Only the caller's own team-shared projects are offered.
    expect(
      Array.from(select.querySelectorAll("option")).map((o) => o.textContent),
    ).toEqual(["Choose a team-shared project…", "Index desk (quant)", "Macro (quant)"]);

    fireEvent.change(select, { target: { value: "p3" } });
    expect(props.onMoveToProject).toHaveBeenCalledWith("c1", "p3");
  });

  it("omits another team's projects, and the one the chat is already in", () => {
    renderDialog({
      userEmail: "bob@x.com",
      myTeam: "quant",
      conversation: conversation({ project_id: "p2" }),
      // The chat is in p2 (quant) but the project is shared with ops, so the
      // fix is a different quant project — never p2 itself, never an ops one.
      project: project({ id: "p2", name: "Index desk", team_id: "ops" }),
      teamSharedProjects: [
        teamProject({ id: "p2", name: "Index desk", team_id: "quant" }),
        teamProject({ id: "p4", name: "Rates", team_id: "quant" }),
        teamProject({ id: "p5", name: "Ops desk", team_id: "ops" }),
      ],
      onMoveToProject: vi.fn(),
    });
    const select = screen.getByLabelText("Move to project");
    expect(
      Array.from(select.querySelectorAll("option")).map((o) => o.getAttribute("value")),
    ).toEqual(["", "p4"]);
  });

  it("choosing a project moves the chat and the toggle goes live in the same dialog", () => {
    // The parent's move is optimistic, so the dialog is re-rendered with the
    // chat's new project — no reload, no re-opening the dialog.
    const onMoveToProject = vi.fn();
    const props = dialogProps({
      userEmail: "alice@x.com",
      project: null,
      teamSharedProjects: [teamProject()],
      onMoveToProject,
    });
    const { rerender } = render(<ShareDialog {...props} />);
    fireEvent.change(screen.getByLabelText("Move to project"), {
      target: { value: "p2" },
    });
    expect(onMoveToProject).toHaveBeenCalledWith("c1", "p2");

    rerender(
      <ShareDialog
        {...props}
        conversation={conversation({ project_id: "p2" })}
        project={teamProject()}
      />,
    );
    const box = screen.getByRole("checkbox", { name: /Share with/ });
    expect(box).toBeEnabled();
    expect(box).not.toHaveAttribute("aria-disabled");
    // The select has done its job and is gone.
    expect(screen.queryByLabelText("Move to project")).toBeNull();
    fireEvent.click(box);
    expect(props.onSetTeamShared).toHaveBeenCalledWith(
      expect.objectContaining({ id: "c1" }),
      true,
    );
  });

  it("a team but no team-shared project → share a project first, with a way there", () => {
    const onOpenProjects = vi.fn();
    renderDialog({
      userEmail: "alice@x.com",
      project: null,
      teamSharedProjects: [],
      onOpenProjects,
    });
    expectUnavailableTreatment();
    expect(screen.queryByLabelText("Move to project")).toBeNull();
    expect(
      screen.getByText(/Share a project with your team first/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Projects" }));
    expect(onOpenProjects).toHaveBeenCalled();
  });

  it("personal project, caller is the owner → share THIS project with the team", () => {
    const props = renderDialog({
      userEmail: "alice@x.com",
      conversation: conversation({ project_id: "p1" }),
      project: project({ owner_email: "alice@x.com" }),
    });
    expectUnavailableTreatment();
    fireEvent.click(
      screen.getByRole("button", { name: "Share this project with your team" }),
    );
    expect(props.onOpenProjectSettings).toHaveBeenCalledWith("p1");
  });

  it("personal project, caller is a member → ask the owner, and no locked door", () => {
    renderDialog({
      userEmail: "bob@x.com",
      conversation: conversation({ project_id: "p1" }),
      project: project({ owner_email: "alice@x.com" }),
    });
    expectUnavailableTreatment();
    expect(
      screen.getByText(/to share this project with your team/),
    ).toBeInTheDocument();
    expect(screen.getByText("alice@x.com")).toBeInTheDocument();
    // Project settings are owner-only: a member must not be pointed at them.
    expect(screen.queryByRole("button", { name: "project settings" })).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Share this project with your team" }),
    ).toBeNull();
  });

  it("no team → the Projects modal's role-branched pointer, for an admin", () => {
    renderDialog({ userEmail: "bob@x.com", myTeam: "", isAdmin: true });
    expectUnavailableTreatment();
    expect(
      screen.getByText(/Add yourself to one in Settings → Admin → Users/),
    ).toBeInTheDocument();
    // No team means no audience: neither move nor "share a project" is a fix.
    expect(screen.queryByLabelText("Move to project")).toBeNull();
    expect(screen.queryByText(/Share a project with your team first/)).toBeNull();
  });

  it("no team → and for a non-admin, the ask-an-admin pointer instead", () => {
    renderDialog({
      userEmail: "bob@x.com",
      myTeam: "",
      isAdmin: false,
      teamSharedProjects: [teamProject()],
      onMoveToProject: vi.fn(),
    });
    expectUnavailableTreatment();
    expect(
      screen.getByText(/Ask an admin to add you in Settings → Admin → Users/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Move to project")).toBeNull();
  });

  it("claims nothing it cannot know: unread lists and an unknown role stay quiet", () => {
    // No `teamSharedProjects` (never passed) is NOT "you have none", and no
    // `userEmail` is NOT "you are the owner" — the dialog degrades to the
    // adaptive sentence it always had.
    renderDialog({ project: null });
    expectUnavailableTreatment();
    expect(screen.queryByLabelText("Move to project")).toBeNull();
    expect(screen.queryByText(/Share a project with your team first/)).toBeNull();
    expect(screen.queryByText(/not on a team yet/)).toBeNull();
    expect(
      screen.getByText(/Move this chat into a team-shared project/),
    ).toBeInTheDocument();

    cleanup();
    renderDialog({ conversation: conversation({ project_id: "p1" }), project: project() });
    // Owner unknown → keep the existing link, offer neither role's action.
    expect(screen.getByRole("button", { name: "project settings" })).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Share this project with your team" }),
    ).toBeNull();
    expect(screen.queryByText(/to share this project with your team/)).toBeNull();
  });

  it("an unread team never disables the toggle on a team-shared project", () => {
    // myTeam === undefined means the read hasn't landed. Guessing "no team"
    // there would flash a dimmed control at a user who can in fact share.
    const props = renderDialog({
      myTeam: undefined,
      conversation: conversation({ project_id: "p2" }),
      project: teamProject(),
    });
    const box = screen.getByRole("checkbox", { name: /Share with/ });
    expect(box).toBeEnabled();
    fireEvent.click(box);
    expect(props.onSetTeamShared).toHaveBeenCalled();
  });
});

// The server is the authority: a `409` names a precondition the reader can act
// on, and that sentence has to land in front of the control that was refused —
// not in a toast behind the modal.
describe("ShareDialog — the server's refusal is shown, not swallowed", () => {
  it("renders the reason as an alert", () => {
    renderDialog({
      error:
        "a chat can only be shared with your team from inside a project that is shared with that team",
      project: null,
    });
    expect(screen.getByRole("alert")).toHaveTextContent(
      /only be shared with your team from inside a project/,
    );
  });

  it("shows nothing when there is nothing to report", () => {
    renderDialog({ error: null });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
