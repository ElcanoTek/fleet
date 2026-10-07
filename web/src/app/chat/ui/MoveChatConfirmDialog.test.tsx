import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import {
  decideMoveConfirm,
  MoveChatConfirmDialog,
  SharedChatLossConfirmDialog,
} from "./MoveChatConfirmDialog";

// The rail's re-filing confirmations (finding #13), and since the sharing
// decisions #25–#30 every way a SHARED chat loses its audience: move to a
// project its team can't see (B35), remove from project (B32), delete (B33),
// archive (B34). Each names the team and quotes the shared-file count from
// GET /conversations/{id}/outputs → shared_count. A private chat keeps
// today's confirm (or none).

const teamShared = { id: "p2", name: "test 2", teamShared: true, team: "Testing" };
const personal = { id: "p1", name: "test 1 - personal", teamShared: false };

function stubOutputs(shared_count: number | "fail") {
  const fetchMock = vi.fn(async () =>
    shared_count === "fail"
      ? new Response("nope", { status: 500 })
      : new Response(
          JSON.stringify({ outputs: [], total: shared_count, shared_count, team_visible: true }),
          { status: 200 },
        ),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("decideMoveConfirm", () => {
  const chat = { id: "c1", title: "Q3 CPA", team_visible: true, project_id: "p2" };

  it("confirms unsharing when a team-shared chat moves into a personal project (B35)", () => {
    expect(
      decideMoveConfirm({
        conversation: chat,
        projectID: "p1",
        target: personal,
        sourceTeam: "Testing",
        team: "Testing",
      }),
    ).toEqual({
      kind: "unshare-move",
      targetProjectName: "test 1 - personal",
      team: "Testing",
      conversationId: "c1",
      chatTitle: "Q3 CPA",
    });
  });

  it("confirms unsharing when a team-shared chat leaves its project altogether (B32)", () => {
    expect(
      decideMoveConfirm({ conversation: chat, projectID: "", target: null, team: "Testing" }),
    ).toEqual({
      kind: "unshare-unfile",
      team: "Testing",
      conversationId: "c1",
      chatTitle: "Q3 CPA",
    });
  });

  it("does not confirm a move between two projects shared with the same team", () => {
    expect(
      decideMoveConfirm({
        conversation: { ...chat, project_id: "p3" },
        projectID: "p2",
        target: teamShared,
        sourceTeam: "Testing",
      }),
    ).toBeNull();
  });

  it("confirms a move into a project shared with a DIFFERENT team — it unshares", () => {
    expect(
      decideMoveConfirm({
        conversation: { ...chat, project_id: "p3" },
        projectID: "p2",
        target: teamShared,
        sourceTeam: "Other",
        team: "Other",
      })?.kind,
    ).toBe("unshare-move");
  });

  it("confirms unfiling a chat that is not team-shared, with today's confirm", () => {
    expect(
      decideMoveConfirm({ conversation: { project_id: "p1" }, projectID: "", target: null }),
    ).toEqual({ kind: "unfile" });
  });

  it("does not confirm filing an unfiled chat, or a move into its own project", () => {
    expect(
      decideMoveConfirm({ conversation: {}, projectID: "p1", target: personal }),
    ).toBeNull();
    expect(
      decideMoveConfirm({ conversation: chat, projectID: "p2", target: teamShared }),
    ).toBeNull();
  });

  it("treats a destination the local list hasn't loaded as not team-shared", () => {
    expect(
      decideMoveConfirm({ conversation: { team_visible: true }, projectID: "p9", target: null }),
    ).toEqual({ kind: "unshare-move", targetProjectName: "another project", team: undefined });
  });
});

describe("MoveChatConfirmDialog", () => {
  const noop = () => {};

  it("B35: names the project, the team and the shared-file count", async () => {
    const fetchMock = stubOutputs(3);
    const onConfirm = vi.fn();
    render(
      <MoveChatConfirmDialog
        confirm={{
          kind: "unshare-move",
          targetProjectName: "Bento",
          team: "Elcano",
          conversationId: "c1",
          chatTitle: "Q3 CPA by domain",
        }}
        onCancel={noop}
        onConfirm={onConfirm}
        onPinAndConfirm={noop}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Move to Bento?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "Bento isn’t shared with Elcano, so “Q3 CPA by domain” will stop being shared, along with 3 shared files. Teammates lose access, but their branches keep their copies.",
      ),
    );
    expect(fetchMock).toHaveBeenCalledWith("/api/conversations/c1/outputs", expect.anything());
    // Nothing promises expiry here, so there is nothing to pin.
    expect(screen.queryByRole("button", { name: /pin it/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Move and stop sharing" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("says “1 shared file” in the singular and drops the clause for 0", async () => {
    stubOutputs(1);
    const { unmount } = render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-move", targetProjectName: "Bento", team: "Elcano", conversationId: "c1", chatTitle: "Q3" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    await waitFor(() =>
      expect(screen.getByRole("dialog")).toHaveTextContent("along with 1 shared file."),
    );
    unmount();
    stubOutputs(0);
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-move", targetProjectName: "Bento", team: "Elcano", conversationId: "c2", chatTitle: "Q3" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    await waitFor(() =>
      expect(screen.getByRole("dialog")).toHaveTextContent(
        "“Q3” will stop being shared. Teammates lose access",
      ),
    );
  });

  it("never quotes a count it couldn't read", async () => {
    stubOutputs("fail");
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-move", targetProjectName: "Bento", team: "Elcano", conversationId: "c1", chatTitle: "Q3" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Move and stop sharing" })).toBeEnabled(),
    );
    expect(screen.getByRole("dialog")).toHaveTextContent("along with shared files.");
    expect(screen.getByRole("dialog")).not.toHaveTextContent(/\d shared file/);
  });

  it("B32: today's remove copy plus the team and the count, with Remove and Pin it and remove", async () => {
    stubOutputs(3);
    const onConfirm = vi.fn();
    const onPinAndConfirm = vi.fn();
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-unfile", team: "Elcano", conversationId: "c1", chatTitle: "Q3 CPA by domain" }}
        onCancel={noop}
        onConfirm={onConfirm}
        onPinAndConfirm={onPinAndConfirm}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Remove from project?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "“Q3 CPA by domain” will become temporary and expire unless pinned. It also stops being shared with Elcano, along with 3 shared files.",
      ),
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Pin it and remove" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Pin it and remove" }));
    expect(onPinAndConfirm).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("B32: holds BOTH Remove and Pin it and remove until the count settles", async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        await gate;
        return new Response("nope", { status: 500 });
      }),
    );
    const onConfirm = vi.fn();
    const onPinAndConfirm = vi.fn();
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-unfile", team: "Elcano", conversationId: "c1" }}
        onCancel={noop}
        onConfirm={onConfirm}
        onPinAndConfirm={onPinAndConfirm}
      />,
    );
    const remove = screen.getByRole("button", { name: "Remove" });
    const pin = screen.getByRole("button", { name: "Pin it and remove" });
    expect(remove).toBeDisabled();
    expect(pin).toBeDisabled();
    fireEvent.click(remove);
    fireEvent.click(pin);
    expect(onConfirm).not.toHaveBeenCalled();
    expect(onPinAndConfirm).not.toHaveBeenCalled();
    // A failed count settles too: unnumbered copy, both actions available.
    release?.();
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove" })).toBeEnabled());
    expect(screen.getByRole("dialog")).toHaveTextContent("along with shared files.");
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("names the audience 'your team' when no team name is known", () => {
    stubOutputs(0);
    render(
      <MoveChatConfirmDialog
        confirm={{ kind: "unshare-unfile", conversationId: "c1" }}
        onCancel={noop}
        onConfirm={noop}
        onPinAndConfirm={noop}
      />,
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(/stops being shared with your team/);
  });

  it("keeps the private remove-from-project copy and reads no counts", () => {
    const fetchMock = stubOutputs(3);
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
    expect(fetchMock).not.toHaveBeenCalled();
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
      screen.getByRole("button", { name: "Cancel removing this chat from the project" }),
    );
    expect(onCancel).toHaveBeenCalledTimes(2);
  });
});

describe("SharedChatLossConfirmDialog", () => {
  it("B33: delete names the team and its shared files", async () => {
    stubOutputs(3);
    const onConfirm = vi.fn();
    render(
      <SharedChatLossConfirmDialog
        kind="delete"
        conversationId="c1"
        chatTitle="Q3 CPA by domain"
        team="Elcano"
        onCancel={() => {}}
        onConfirm={onConfirm}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Delete “Q3 CPA by domain”?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "The chat and its files are deleted. Elcano will lose access to this chat and its 3 shared files. Teammates who branched it keep their copies.",
      ),
    );
    await waitFor(() => expect(screen.getByRole("button", { name: "Delete chat" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Delete chat" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("B34: archive says it stops sharing and comes back as Only you", async () => {
    stubOutputs(1);
    render(
      <SharedChatLossConfirmDialog
        kind="archive"
        conversationId="c1"
        chatTitle="Q3"
        team="Elcano"
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Archive “Q3”?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "Archiving stops sharing this chat with Elcano, along with its 1 shared file. Teammates who branched it keep their copies. If you unarchive it, it comes back as Only you.",
      ),
    );
    expect(screen.getByRole("button", { name: "Archive chat" })).toBeInTheDocument();
  });

  it("drops the files clause when nothing is shared", async () => {
    stubOutputs(0);
    render(
      <SharedChatLossConfirmDialog
        kind="delete"
        conversationId="c1"
        chatTitle="Q3"
        team="Elcano"
        onCancel={() => {}}
        onConfirm={() => {}}
      />,
    );
    await waitFor(() =>
      expect(screen.getByRole("dialog")).toHaveTextContent(
        "Elcano will lose access to this chat. Teammates who branched it",
      ),
    );
  });
});
