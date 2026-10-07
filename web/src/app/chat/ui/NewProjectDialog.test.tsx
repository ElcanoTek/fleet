import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { NewProjectDialog } from "./NewProjectDialog";

// New project (B27, decision #52): name, "Who can see it" (Only you / the
// team), optional instructions, Cancel / Create project. From the share
// dialog's A1b state the team is preselected and a line names the chat that
// moves in. No team: the team option is unavailable, with the reason.

afterEach(() => cleanup());

describe("NewProjectDialog", () => {
  it("creates a private project by default", async () => {
    const onCreate = vi.fn(async () => null);
    render(<NewProjectDialog team="Elcano" onClose={() => {}} onCreate={onCreate} />);
    const dialog = screen.getByRole("dialog", { name: "New project" });
    expect(dialog).toHaveTextContent(
      "A home for related chats, with shared instructions and memory.",
    );
    const name = screen.getByPlaceholderText("e.g. Knowertech: Q4 planning");
    const create = screen.getByRole("button", { name: "Create project" });
    expect(create).toBeDisabled();
    fireEvent.change(name, { target: { value: "  Q4 planning " } });
    expect(screen.getByRole("radio", { name: /Only you/ })).toHaveAttribute("aria-checked", "true");
    expect(dialog).toHaveTextContent("Only you can see it. You can share it with Elcano any time.");
    fireEvent.click(create);
    await waitFor(() =>
      expect(onCreate).toHaveBeenCalledWith({ name: "Q4 planning", instructions: "", teamShared: false }),
    );
  });

  it("shares with the team and adds instructions when asked", async () => {
    const onCreate = vi.fn(async () => null);
    render(<NewProjectDialog team="Elcano" onClose={() => {}} onCreate={onCreate} />);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Q4" } });
    fireEvent.click(screen.getByRole("radio", { name: /Elcano/ }));
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "Elcano will see the instructions and Team learnings. Each chat stays Only you until its owner shares it.",
    );
    fireEvent.click(screen.getByRole("button", { name: "+ Add instructions (optional)" }));
    fireEvent.change(screen.getByLabelText("Instructions (optional)"), {
      target: { value: "Be brief." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    await waitFor(() =>
      expect(onCreate).toHaveBeenCalledWith({ name: "Q4", instructions: "Be brief.", teamShared: true }),
    );
  });

  it("from the share dialog: team preselected, and the chat that moves in is named", () => {
    render(
      <NewProjectDialog
        team="Elcano"
        teamPreselected
        moveChat={{ id: "c1", title: "Q3 CPA by domain" }}
        onClose={() => {}}
        onCreate={async () => null}
      />,
    );
    expect(screen.getByRole("radio", { name: /Elcano/ })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "“Q3 CPA by domain” moves into the new project and is shared with Elcano.",
    );
  });

  it("shows the team option as unavailable, with the reason, for someone on no team", () => {
    render(
      <NewProjectDialog team="" isAdmin={false} onClose={() => {}} onCreate={async () => null} />,
    );
    const teamOption = screen.getByRole("radio", { name: /unavailable/ });
    expect(teamOption).toBeDisabled();
    expect(teamOption).toHaveTextContent("You’re not on a team");
    expect(screen.getByRole("dialog")).toHaveTextContent(
      "You’re not on a team, so projects stay Only you. Ask an admin to add you in Settings → Admin → Users.",
    );
  });

  it("keeps the dialog open and shows why when create fails", async () => {
    render(
      <NewProjectDialog team="Elcano" onClose={() => {}} onCreate={async () => "name taken"} />,
    );
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Q4" } });
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("name taken");
  });
});
