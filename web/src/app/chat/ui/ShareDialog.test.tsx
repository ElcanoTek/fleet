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

function renderDialog(
  over: Partial<Parameters<typeof ShareDialog>[0]> = {},
) {
  const props = {
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
    fireEvent.click(screen.getByRole("button", { name: "Stop sharing the link" }));
    expect(props.onStopLink).toHaveBeenCalled();
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
