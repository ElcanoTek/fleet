import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { ProjectsModal } from "./ProjectsModal";

// Projects modal, team-sharing path (#1157). "Share with my team" is a dead end
// for a caller with no team — the write 400s upstream — so the modal reads
// /api/me/team and offers to create one inline instead of showing a checkbox
// that cannot work.

function mockFetch(
  impl: (url: string, init?: RequestInit) => Response | Promise<Response>,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) =>
      Promise.resolve(impl(String(input), init)),
    ),
  );
}

const meBody = (team: string) =>
  new Response(
    JSON.stringify({ email: "ann@x.com", role: "member", team_id: team, admin: false }),
    { status: 200 },
  );
const noProjects = () => new Response(JSON.stringify({ projects: [] }), { status: 200 });

function renderModal() {
  render(
    <ProjectsModal
      userEmail="ann@x.com"
      onClose={() => {}}
      onStartChat={() => {}}
      initialCreate
    />,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProjectsModal team sharing", () => {
  it("offers to create a team when the user has none, then shows the share checkbox", async () => {
    const puts: string[] = [];
    let team = "";
    mockFetch((url, init) => {
      if (url === "/api/me/team") {
        if (init?.method === "PUT") {
          puts.push(String(init.body));
          team = "platform";
        }
        return meBody(team);
      }
      return noProjects();
    });

    renderModal();

    // No checkbox to mislead with — an inline team create instead.
    const input = await screen.findByLabelText("Team name");
    expect(screen.queryByRole("checkbox")).toBeNull();

    fireEvent.change(input, { target: { value: "platform" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));

    await waitFor(() => expect(puts.length).toBe(1));
    expect(JSON.parse(puts[0])).toEqual({ team_id: "platform" });

    // The share checkbox appears, pre-checked, naming the team.
    const share = await screen.findByRole("checkbox");
    expect(share).toBeChecked();
    expect(screen.getByText(/Share with my team \(platform\)/)).toBeInTheDocument();
  });

  it("shows the named share checkbox straight away for a user with a team", async () => {
    mockFetch((url) => (url === "/api/me/team" ? meBody("platform") : noProjects()));

    renderModal();

    expect(await screen.findByRole("checkbox")).not.toBeChecked();
    expect(screen.getByText(/Share with my team \(platform\)/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Team name")).toBeNull();
  });
});
