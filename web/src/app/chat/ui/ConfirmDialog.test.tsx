import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ConfirmDialog, NameChip } from "./ConfirmDialog";
import { DeleteProjectConfirmDialog } from "./DeleteProjectConfirmDialog";

// Finding #13: the shared in-app confirm every confirm path on the chat
// surface routes through, replacing window.confirm. The treatment comes from
// the shared DialogShell (a --color-surface-1 panel with --shadow-md over a
// --color-overlay-strong scrim), so these assert the contract rather than the
// pixels: it is a real modal dialog, the scrim cancels, and it can hold a
// second action.
//
// It carries BOTH confirm shapes since the B-2 pass folded ConfirmModal into
// it — untitled (the body copy is the accessible name) and titled (with an
// optional busy state) — so both are covered here.

describe("ConfirmDialog", () => {
  it("is a modal dialog named by its own body copy", () => {
    render(
      <ConfirmDialog
        bodyId="body-1"
        cancelAriaLabel="Cancel the thing"
        confirmLabel="Do it"
        onCancel={() => {}}
        onConfirm={() => {}}
      >
        Really do the thing?
      </ConfirmDialog>,
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAttribute("aria-labelledby", "body-1");
    expect(screen.getByText("Really do the thing?")).toHaveAttribute(
      "id",
      "body-1",
    );
  });

  it("cancels from the button and from the scrim, and confirms from the confirm button", () => {
    const onCancel = vi.fn();
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        bodyId="body-2"
        cancelAriaLabel="Cancel the thing"
        confirmLabel="Do it"
        onCancel={onCancel}
        onConfirm={onConfirm}
      >
        Really?
      </ConfirmDialog>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel the thing" }));
    expect(onCancel).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "Do it" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("cancels on Escape, the way the native confirm did", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        bodyId="body-esc"
        cancelAriaLabel="Cancel the thing"
        confirmLabel="Do it"
        onCancel={onCancel}
        onConfirm={() => {}}
      >
        Really?
      </ConfirmDialog>,
    );
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("holds a second action a native confirm could not", () => {
    const secondary = vi.fn();
    render(
      <ConfirmDialog
        bodyId="body-3"
        cancelAriaLabel="Cancel"
        confirmLabel="Do it"
        onCancel={() => {}}
        onConfirm={() => {}}
        secondary={{ label: "Do it differently", onClick: secondary }}
      >
        Really?
      </ConfirmDialog>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Do it differently" }));
    expect(secondary).toHaveBeenCalledTimes(1);
  });

  it("paints the destructive confirm in the danger token, over the app's own scrim", () => {
    render(
      <ConfirmDialog
        bodyId="body-4"
        cancelAriaLabel="Dismiss without deleting"
        confirmLabel="Delete"
        confirmTone="danger"
        testId="d"
        onCancel={() => {}}
        onConfirm={() => {}}
      >
        Really?
      </ConfirmDialog>,
    );
    expect(screen.getByRole("button", { name: "Delete" }).className).toContain(
      "var(--color-danger)",
    );
    const panel = screen.getByTestId("d");
    expect(panel.className).toContain("bg-[var(--color-surface-1)]");
    expect(panel.className).toContain("shadow-[var(--shadow-md)]");
    expect(
      screen.getByLabelText("Dismiss without deleting").className,
    ).toContain("bg-[var(--color-overlay-strong)]");
  });

  it("takes a title as its accessible name, with the body underneath", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Stop sharing this project?"
        confirmLabel="Stop sharing"
        confirmTone="danger"
        onCancel={onCancel}
        onConfirm={() => {}}
      >
        <p className="m-0">Every shared chat stops being shared.</p>
      </ConfirmDialog>,
    );
    const dialog = screen.getByRole("dialog", {
      name: "Stop sharing this project?",
    });
    expect(dialog).toHaveTextContent("Every shared chat stops being shared.");
    expect(
      screen.getByRole("heading", { name: "Stop sharing this project?" }),
    ).toBeInTheDocument();
    // The scrim names the action it performs even when the caller doesn't.
    fireEvent.click(
      screen.getByRole("button", { name: "Cancel: Stop sharing this project?" }),
    );
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("keeps rich titles and chip punctuation readable with a plain accessible name", () => {
    render(
      <ConfirmDialog
        title="Transfer Research to Alex?"
        titleContent={<>Transfer <NameChip>Research</NameChip> to <NameChip suffix="?">Alex</NameChip></>}
        confirmLabel="Transfer"
        onCancel={() => {}}
        onConfirm={() => {}}
      >
        They can edit and delete the project.
      </ConfirmDialog>,
    );
    expect(screen.getByRole("dialog", { name: "Transfer Research to Alex?" })).toBeInTheDocument();
    expect(screen.getByRole("heading")).toHaveTextContent("Transfer Research to Alex?");
    expect(screen.getByRole("button", { name: "Cancel: Transfer Research to Alex?" })).toBeInTheDocument();
  });

  it("holds the confirm while a titled confirm is busy", () => {
    render(
      <ConfirmDialog
        title="Transfer this project?"
        confirmLabel="Transfer"
        confirmTone="accent"
        busy
        onCancel={() => {}}
        onConfirm={() => {}}
      >
        <p className="m-0">They will be able to edit and delete it.</p>
      </ConfirmDialog>,
    );
    expect(screen.getByRole("button", { name: "Transfer" })).toBeDisabled();
  });

  it("offers no second action unless one is passed", () => {
    render(
      <ConfirmDialog
        bodyId="body-solo"
        cancelAriaLabel="Cancel the thing"
        confirmLabel="Do it"
        onCancel={() => {}}
        onConfirm={() => {}}
      >
        Really?
      </ConfirmDialog>,
    );
    // Cancel + confirm, and nothing between them.
    expect(screen.getAllByRole("button")).toHaveLength(3); // + the scrim
  });
});

describe("DeleteProjectConfirmDialog", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("uses B29's copy and real counts from /impact, with Export first", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({ memories: 4, chats: 3, members: 2, team_shared_chats: 1 }),
          { status: 200 },
        ),
      ),
    );
    const onConfirm = vi.fn();
    render(
      <DeleteProjectConfirmDialog
        projectId="p1"
        projectName="test 2"
        onCancel={() => {}}
        onConfirm={onConfirm}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Delete test 2?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "3 chats from 2 people leave the project and become temporary. They expire unless someone pins them.",
      ),
    );
    expect(dialog).toHaveTextContent("Team learnings (4) are lost.");
    expect(dialog).toHaveTextContent("Instructions and sharing are removed.");
    expect(screen.getByRole("link", { name: "Export first" })).toHaveAttribute(
      "href",
      "/api/projects/p1/export",
    );
    expect(onConfirm).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("states what is lost without inventing counts when it has no id", () => {
    render(
      <DeleteProjectConfirmDialog onCancel={() => {}} onConfirm={() => {}} />,
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent(/Delete this project\?/);
    expect(dialog).toHaveTextContent(
      "Every chat in it leaves the project and becomes temporary.",
    );
    expect(dialog).toHaveTextContent("Its team learnings are lost.");
    expect(screen.queryByRole("link", { name: "Export first" })).toBeNull();
  });
});
