import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import {
  decideMoveConfirm,
  MoveChatConfirmDialog,
} from "./MoveChatConfirmDialog";

// Finding #13: the two re-filing confirmations were window.confirm() — the
// only native confirms on the chat surface. Unstyled, titled with the browser
// origin, blocking the whole tab, and — the functional part — able to render
// only a string and hold only OK/Cancel, so they could not show the project or
// the team as a chip and could not offer the action their own copy promises
// ("expire unless pinned", with nothing to click).
//
// The copy is unchanged. These assert that, and the two things the native
// dialog could not do.

const teamShared = { id: "p2", name: "test 2", teamShared: true };
const personal = { id: "p1", name: "test 1 - personal", teamShared: false };

describe("decideMoveConfirm", () => {
  it("confirms unsharing when a team-shared chat moves into a personal project", () => {
    expect(
      decideMoveConfirm({
        conversation: { team_visible: true, project_id: "p2" },
        projectID: "p1",
        target: personal,
        team: "Testing",
      }),
    ).toEqual({
      kind: "unshare-move",
      targetProjectName: "test 1 - personal",
      team: "Testing",
    });
  });

  it("confirms unsharing when a team-shared chat leaves its project altogether", () => {
    expect(
      decideMoveConfirm({
        conversation: { team_visible: true, project_id: "p2" },
        projectID: "",
        target: null,
        team: "Testing",
      }),
    ).toEqual({ kind: "unshare-unfile", team: "Testing" });
  });

  it("does not confirm a move between two team-shared projects", () => {
    expect(
      decideMoveConfirm({
        conversation: { team_visible: true, project_id: "p3" },
        projectID: "p2",
        target: teamShared,
      }),
    ).toBeNull();
  });

  it("confirms unfiling a chat that is not team-shared", () => {
    expect(
      decideMoveConfirm({
        conversation: { project_id: "p1" },
        projectID: "",
        target: null,
      }),
    ).toEqual({ kind: "unfile" });
  });

  it("does not confirm filing an unfiled chat", () => {
    expect(
      decideMoveConfirm({ conversation: {}, projectID: "p1", target: personal }),
    ).toBeNull();
  });

  it("treats a destination the local list hasn't loaded as not team-shared", () => {
    // Same behaviour the window.confirm branch had: an unknown project can't
    // be assumed to be team-shared, so the unshare is confirmed and the copy
    // says "another project" rather than naming one it doesn't know.
    expect(
      decideMoveConfirm({
        conversation: { team_visible: true },
        projectID: "p9",
        target: null,
      }),
    ).toEqual({
      kind: "unshare-move",
      targetProjectName: "another project",
      team: undefined,
    });
  });
});

describe("MoveChatConfirmDialog", () => {
  const noop = () => {};

  it("keeps the unshare-on-move copy and renders the project and the team as chips", () => {
    render(
      <MoveChatConfirmDialog
        confirm={{
          kind: "unshare-move",
          targetProjectName: "test 1 - personal",
          team: "Testing",
        }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    const dialog = screen.getByRole("dialog");
    // The sentence, verbatim from the window.confirm it replaces — with the
    // project and the audience as chips instead of quoted characters.
    expect(dialog).toHaveTextContent(
      /This chat is shared with Testing\. Moving it to test 1 - personal, which isn't shared with your team, will unshare it\. Continue\?/,
    );
    // The body is the accessible name: no invented heading, no browser origin.
    expect(dialog).toHaveAttribute("aria-labelledby", "move-chat-confirm-body");
    expect(screen.getByText("Testing")).toBeInTheDocument();
    expect(screen.getByText("test 1 - personal")).toBeInTheDocument();
    // Unsharing does not promise anything about expiry, so there is nothing
    // to pin here.
    expect(screen.queryByRole("button", { name: /pin it/i })).toBeNull();
  });

  it("keeps the unshare-on-remove copy", () => {
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-unfile", team: "Testing" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(
      /This chat is shared with Testing\. Removing it from the project will unshare it\. Continue\?/,
    );
  });

  it("names the audience 'your team' when no team name is known", () => {
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-unfile" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(
      /This chat is shared with your team\./,
    );
  });

  it("keeps the remove-from-project copy and offers Pin it alongside Remove", () => {
    const onConfirm = vi.fn();
    const onPinAndConfirm = vi.fn();
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unfile" }}
        onCancel={noop}
        onConfirm={onConfirm}
        onPinAndConfirm={onPinAndConfirm}
      />,
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(
      /This chat will become temporary and expire unless pinned\. Remove it from the project\?/,
    );
    // "expire unless pinned" now has a button.
    fireEvent.click(screen.getByRole("button", { name: "Pin it and remove" }));
    expect(onPinAndConfirm).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Remove from project" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("cancels from the button and from the scrim", () => {
    const onCancel = vi.fn();
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unfile" }}
        onCancel={onCancel}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(
      screen.getByRole("button", {
        name: "Cancel removing this chat from the project",
      }),
    );
    expect(onCancel).toHaveBeenCalledTimes(2);
  });
});
