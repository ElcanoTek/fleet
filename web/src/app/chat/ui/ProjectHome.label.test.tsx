import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { ProjectHome } from "./ProjectHome";
import type { Project } from "./ProjectsModal";

// Project settings dialog a11y contract (cohort-3 a11y pass): the visible
// "Name" caption used to be a bare <label> with no htmlFor, so clicking it did
// nothing and the association existed only in the visual layout. It is now a
// real label/control pair; the input keeps its explicit aria-label so the
// accessible name stays "Project name" (what e2e/mocked/project-home.spec.ts
// queries by, and the unambiguous name inside a dialog full of other fields).

const PROJECT: Project = {
  id: "p1",
  owner_email: "owner@example.com",
  name: "Acme",
  instructions: "",
  mcp_servers: [],
  created_at: 1767225600,
  updated_at: 1767225600,
};

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({}), { status: 200 })),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function renderHome() {
  return render(
    <ProjectHome
      project={PROJECT}
      chats={[]}
      userEmail="owner@example.com"
      isOwner
      initialSettingsOpen
      onBack={() => {}}
      onOpenChat={() => {}}
      onOpenTeamChat={() => {}}
      onNewChat={() => {}}
      onSaveInstructions={async () => true}
      onUpdateSettings={async () => true}
      myTeam=""
      onDelete={async () => true}
    />,
  );
}

describe("ProjectHome settings dialog", () => {
  it("associates the visible Name label with the name input", () => {
    renderHome();
    const input = screen.getByLabelText("Project name");
    const label = screen.getByText("Name");

    expect(label.tagName).toBe("LABEL");
    expect(input.id).not.toBe("");
    expect(label).toHaveAttribute("for", input.id);
  });

  it("keeps 'Project name' as the accessible name", () => {
    renderHome();
    expect(screen.getByRole("textbox", { name: "Project name" })).toHaveValue("Acme");
  });
});
