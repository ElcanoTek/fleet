import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ProjectSettingsDialog } from "./ProjectSettingsDialog";
import type { Project } from "./ProjectsModal";

// Project settings (B26–B31): Name, "Who can see it", the owner line with
// Transfer…, Delete project, Cancel / Save. Counts are real, from
// /impact; the team's size from /members.

const SHARED: Project = {
  id: "kt",
  owner_email: "sam@x.com",
  name: "Knowertech",
  team_id: "Elcano",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
};
const PRIVATE: Project = { ...SHARED, team_id: undefined };

const IMPACT = {
  memories: 0,
  chats: 3,
  members: 2,
  team_shared_chats: 2,
  chats_from_teammates: 1,
  teammates_with_chats: 1,
};

function stub() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/impact")) return new Response(JSON.stringify(IMPACT), { status: 200 });
    if (url.endsWith("/members"))
      return new Response(
        JSON.stringify({ members: ["dana@x.com", "jules@x.com", "sam@x.com"] }),
        { status: 200 },
      );
    return new Response("{}", { status: 200 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderDialog(overrides: Partial<Parameters<typeof ProjectSettingsDialog>[0]> = {}) {
  const props = {
    project: SHARED,
    myTeam: "Elcano",
    isAdmin: false,
    onClose: vi.fn(),
    onSave: vi.fn(async () => true),
    onTransfer: vi.fn(async () => null),
    onDelete: vi.fn(),
    ...overrides,
  };
  render(<ProjectSettingsDialog {...props} />);
  return props;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProjectSettingsDialog", () => {
  it("shows the team option with the team's size and the owner line", async () => {
    stub();
    renderDialog();
    const team = screen.getByRole("radio", { name: /Elcano/ });
    expect(team).toHaveAttribute("aria-checked", "true");
    await waitFor(() => expect(team).toHaveTextContent("3 people"));
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "Elcano will see the instructions, Team learnings, and chats shared with them. Each chat stays Only you until its owner shares it.",
    );
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "Owner: sam@x.com. Only the owner can edit, share, or delete.",
    );
  });

  it("a save that lands after the dialog was dismissed closes nothing", async () => {
    stub();
    let finish: (ok: boolean) => void = () => {};
    const onClose = vi.fn();
    const { unmount } = render(
      <ProjectSettingsDialog
        project={SHARED}
        myTeam="Elcano"
        isAdmin={false}
        onClose={onClose}
        onSave={() =>
          new Promise<boolean>((resolve) => {
            finish = resolve;
          })
        }
        onTransfer={vi.fn(async () => null)}
        onDelete={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    // Dismissed while the save is in flight (another project's settings may
    // be open by the time it lands).
    unmount();
    await act(async () => {
      finish(true);
    });
    expect(onClose).not.toHaveBeenCalled();
  });

  it("B28: switching to Only you counts what changes before Save", async () => {
    stub();
    const props = renderDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    await waitFor(() =>
      expect(screen.getByRole("dialog")).toHaveTextContent(
        "2 shared chats will become Only you, and 1 chat from 1 teammate will move to their unfiled chats, where they can expire.",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(props.onSave).toHaveBeenCalledWith({ team_shared: false }));
    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
  });

  it("B28: Save waits for the counts before it can unshare", async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/impact")) {
          await gate;
          return new Response(JSON.stringify(IMPACT), { status: 200 });
        }
        return new Response(JSON.stringify({ members: [] }), { status: 200 });
      }),
    );
    const props = renderDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    const save = screen.getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    fireEvent.click(save);
    expect(props.onSave).not.toHaveBeenCalled();
    release?.();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  });

  it("B28: re-choosing Only you waits for a fresh count, not the earlier one", async () => {
    const gates: Array<() => void> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/impact")) {
          await new Promise<void>((resolve) => gates.push(resolve));
          return new Response(JSON.stringify(IMPACT), { status: 200 });
        }
        return new Response(JSON.stringify({ members: [] }), { status: 200 });
      }),
    );
    renderDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    await waitFor(() => expect(gates).toHaveLength(1));
    gates[0]();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
    // Back to the team, then Only you again: the second /impact is held.
    fireEvent.click(screen.getByRole("radio", { name: /Elcano/ }));
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    await waitFor(() => expect(gates).toHaveLength(2));
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.getByRole("dialog")).toHaveTextContent("Counting what changes…");
    gates[1]();
    await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeEnabled());
  });

  it("B29: Delete project waits for the counts before it can delete", async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).endsWith("/impact")) {
          await gate;
          return new Response(JSON.stringify(IMPACT), { status: 200 });
        }
        return new Response(JSON.stringify({ members: [] }), { status: 200 });
      }),
    );
    const props = renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    const panel = screen.getByRole("alertdialog", { name: "Delete Knowertech?" });
    const del = within(panel).getByRole("button", { name: "Delete project" });
    expect(del).toBeDisabled();
    fireEvent.click(del);
    expect(props.onDelete).not.toHaveBeenCalled();
    release?.();
    await waitFor(() =>
      expect(within(panel).getByRole("button", { name: "Delete project" })).toBeEnabled(),
    );
  });

  it("B29: a failed count still lets the owner delete, with honest wording", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).endsWith("/impact")
          ? new Response("boom", { status: 500 })
          : new Response(JSON.stringify({ members: [] }), { status: 200 }),
      ),
    );
    const props = renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    const panel = screen.getByRole("alertdialog", { name: "Delete Knowertech?" });
    await waitFor(() =>
      expect(panel).toHaveTextContent("Every chat in it leaves the project and becomes temporary."),
    );
    fireEvent.click(within(panel).getByRole("button", { name: "Delete project" }));
    expect(props.onDelete).toHaveBeenCalledTimes(1);
  });

  it("B26: no team — the team option is unavailable, with the A3a guidance", () => {
    stub();
    renderDialog({ project: PRIVATE, myTeam: "", isAdmin: true });
    const team = screen.getByRole("radio", { name: /unavailable/ });
    expect(team).toBeDisabled();
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "You’re not on a team, so projects stay Only you. Add yourself in Settings → Admin → Users, or create a team in Settings → Team.",
    );
  });

  it("B31: transfer is unavailable until the project is shared, with the reason", () => {
    stub();
    renderDialog({ project: PRIVATE });
    expect(screen.getByRole("button", { name: /Transfer… \(unavailable\)/ })).toBeDisabled();
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "Transfer needs a member, so share the project first.",
    );
  });

  it("B30: transfer picks a member, says what changes, and happens apart from Save", async () => {
    stub();
    const props = renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Transfer…" }));
    const jules = await screen.findByRole("radio", { name: /jules@x.com/ });
    // The owner is not offered to themselves.
    expect(screen.queryByRole("radio", { name: /sam@x.com/ })).toBeNull();
    fireEvent.click(jules);
    expect(screen.getByRole("group", { name: "Transfer ownership" })).toHaveTextContent(
      "jules@x.com becomes the owner. You stay a member: you keep your chats here and can still share them, but you can’t edit settings, share the project, or delete it. This happens right away, separately from Save.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership" }));
    await waitFor(() => expect(props.onTransfer).toHaveBeenCalledWith("jules@x.com"));
    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(props.onSave).not.toHaveBeenCalled();
  });

  it("an owner who has left the team is told they lose the project and its chats go temporary", async () => {
    stub();
    renderDialog({ myTeam: "other" });
    fireEvent.click(screen.getByRole("button", { name: "Transfer…" }));
    fireEvent.click(await screen.findByRole("radio", { name: /jules@x.com/ }));
    const group = screen.getByRole("group", { name: "Transfer ownership" });
    expect(group).toHaveTextContent(/jules@x\.com becomes the owner\. You’re no longer on /);
    expect(group).toHaveTextContent(/you lose access to .*, and your chats in it become temporary again\./);
    expect(group).not.toHaveTextContent("You stay a member");
  });

  it("B29: delete quotes real counts and offers Export first", async () => {
    stub();
    const props = renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    const panel = screen.getByRole("alertdialog", { name: "Delete Knowertech?" });
    // Opening the panel destroys nothing; only its own button does.
    expect(props.onDelete).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(panel).toHaveTextContent(
        "3 chats from 2 people leave the project and become temporary. They expire unless someone pins them.",
      ),
    );
    expect(panel).toHaveTextContent("Team learnings (0) are lost.");
    expect(panel).toHaveTextContent("Instructions and sharing are removed.");
    expect(screen.getByRole("link", { name: "Export first" })).toHaveAttribute(
      "href",
      "/api/projects/kt/export",
    );
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }));
    expect(props.onDelete).toHaveBeenCalledTimes(1);
  });

  it("pairs the visible Name label with the field, and keeps 'Project name' as its accessible name", () => {
    stub();
    renderDialog();
    const input = screen.getByRole("textbox", { name: "Project name" });
    expect(input).toHaveValue("Knowertech");
    const label = screen.getByText("Name");
    expect(label.tagName).toBe("LABEL");
    expect(input.id).not.toBe("");
    expect(label).toHaveAttribute("for", input.id);
  });

  it("B28: says it couldn't count teammates' chats rather than implying none move", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        // An older server: no chats_from_teammates field at all.
        if (url.endsWith("/impact"))
          return new Response(
            JSON.stringify({ memories: 1, chats: 2, members: 2, team_shared_chats: 1 }),
            { status: 200 },
          );
        return new Response(JSON.stringify({ members: ["sam@x.com"] }), { status: 200 });
      }),
    );
    renderDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    await waitFor(() =>
      expect(screen.getByRole("dialog")).toHaveTextContent(
        "1 shared chat will become Only you, and chats from teammates will move to their unfiled chats, where they can expire (we couldn’t count those).",
      ),
    );
    expect(screen.getByRole("dialog")).not.toHaveTextContent("0 chats");
  });

  it("Cancel after switching to Only you changes nothing", async () => {
    stub();
    const props = renderDialog();
    fireEvent.click(screen.getByRole("radio", { name: /Only you/ }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(props.onClose).toHaveBeenCalled();
    expect(props.onSave).not.toHaveBeenCalled();
  });

  it("B30: keeps the panel open and shows the server's reason when a transfer is refused", async () => {
    stub();
    const props = renderDialog({
      onTransfer: vi.fn(async () => "the new owner must be a member of the project's team"),
    });
    fireEvent.click(screen.getByRole("button", { name: "Transfer…" }));
    fireEvent.click(await screen.findByRole("radio", { name: /dana@x.com/ }));
    fireEvent.click(screen.getByRole("button", { name: "Transfer ownership" }));
    const group = screen.getByRole("group", { name: "Transfer ownership" });
    expect(
      await within(group).findByText("the new owner must be a member of the project's team"),
    ).toHaveAttribute("role", "alert");
    expect(props.onClose).not.toHaveBeenCalled();
  });

  it("B30: says why there is nobody to transfer to, rather than an empty picker", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ members: ["sam@x.com"] }), { status: 200 })),
    );
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Transfer…" }));
    expect(
      await screen.findByText("Only a member can become the owner, and nobody else is on Elcano yet."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Transfer ownership" })).toBeNull();
  });

  it("B30: a failed members lookup is reported, not shown as an empty team", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).endsWith("/members")
          ? new Response("boom", { status: 500 })
          : new Response("{}", { status: 200 }),
      ),
    );
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Transfer…" }));
    expect(await screen.findByText(/Couldn’t load this project’s members/)).toBeInTheDocument();
    expect(screen.queryByText(/nobody else is on/)).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });
});
