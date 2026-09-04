import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { ProjectsModal } from "./ProjectsModal";

// Projects modal, team-sharing path (#1157, narrowed by ADR-0057). "Share with
// my team" is a dead end for a caller with no team — the write 400s upstream —
// so the modal reads /api/me/team. It used to offer to CREATE a team inline,
// with copy telling teammates to "join the same name", which the server
// refuses (409, joining is admin-granted). The teamless state is now
// display-only and points at whichever surface actually applies to the caller:
// an admin can fix it themselves, a member has to ask.

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

const meBody = (team: string, admin = false) =>
  new Response(
    JSON.stringify({
      email: "ann@x.com",
      role: admin ? "admin" : "member",
      team_id: team,
      admin,
    }),
    { status: 200 },
  );
const noProjects = () => new Response(JSON.stringify({ projects: [] }), { status: 200 });

function renderModal(initialCreate = true) {
  render(
    <ProjectsModal
      userEmail="ann@x.com"
      onClose={() => {}}
      onStartChat={() => {}}
      initialCreate={initialCreate}
    />,
  );
}

const PROJECT = {
  id: "p1",
  owner_email: "ann@x.com",
  name: "Quant",
  instructions: "",
  team_id: "platform",
  mcp_servers: [],
  created_at: 1767225600,
  updated_at: 1767225600,
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProjectsModal team sharing", () => {
  it("never invites a teamless member to type a team name", async () => {
    const puts: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team") {
        if (init?.method === "PUT") puts.push(String(init.body));
        return meBody("");
      }
      return noProjects();
    });

    renderModal();

    // Nothing to type, nothing to get wrong: no team field, no checkbox that
    // could only 400, and no write from this surface at all.
    expect(
      await screen.findByText(/Ask an admin to add you in Settings → Admin → Users/),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("Team name")).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(puts).toEqual([]);
  });

  it("sends a teamless ADMIN to the surfaces they can actually use", async () => {
    mockFetch((url) => (url === "/api/me/team" ? meBody("", true) : noProjects()));

    renderModal();

    // Most fleet users are admins; telling them to ask someone else would be
    // worse than useless.
    expect(
      await screen.findByText(
        /Add yourself to one in Settings → Admin → Users, or create one in Settings → Team/,
      ),
    ).toBeInTheDocument();
  });

  it("shows the named share checkbox straight away for a user with a team", async () => {
    mockFetch((url) => (url === "/api/me/team" ? meBody("platform") : noProjects()));

    renderModal();

    expect(await screen.findByRole("checkbox")).not.toBeChecked();
    expect(screen.getByText(/Share with my team \(platform\)/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Team name")).toBeNull();
    // …and a way to manage the team, rather than a dead end.
    expect(screen.getByRole("link", { name: "Manage your team" })).toBeInTheDocument();
  });
});

// Item 13, projects-modal half: both of this surface's confirms were
// window.confirm() — unstyled, titled with the browser's origin, and unable to
// name the project. The copy is unchanged; only the dialog moved into the app.
describe("ProjectsModal confirms", () => {
  it("asks in the app before deleting a project, and only then deletes", async () => {
    const calls: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team") return meBody("platform");
      if (init?.method === "DELETE") {
        calls.push(url);
        return new Response("{}", { status: 200 });
      }
      return new Response(JSON.stringify({ projects: [PROJECT] }), { status: 200 });
    });

    renderModal(false);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    const dialog = await screen.findByRole("dialog", { name: "Delete Quant?" });
    expect(dialog).toHaveTextContent("Its team learnings are lost");
    expect(calls).toEqual([]);

    fireEvent.click(within(dialog).getByRole("button", { name: "Delete project" }));
    await waitFor(() => expect(calls).toEqual(["/api/projects/p1"]));
  });

  it("keeps the project when the delete confirm is cancelled", async () => {
    const calls: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team") return meBody("platform");
      if (init?.method === "DELETE") {
        calls.push(url);
        return new Response("{}", { status: 200 });
      }
      return new Response(JSON.stringify({ projects: [PROJECT] }), { status: 200 });
    });

    renderModal(false);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog", { name: "Delete Quant?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    // Named, not bare: the projects panel underneath is itself a role="dialog"
    // now that every modal sits on the shared DialogShell, so "no dialog at
    // all" would be asserting that the modal closed too.
    expect(screen.queryByRole("dialog", { name: "Delete Quant?" })).toBeNull();
    expect(calls).toEqual([]);
  });

  it("asks before a save that stops sharing the project with the team", async () => {
    const patches: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team") return meBody("platform");
      if (init?.method === "PATCH") {
        patches.push(String(init.body));
        return new Response(JSON.stringify(PROJECT), { status: 200 });
      }
      return new Response(JSON.stringify({ projects: [PROJECT] }), { status: 200 });
    });

    renderModal(false);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    // The editor opens with the project's current sharing state ticked.
    const share = await screen.findByRole("checkbox");
    expect(share).toBeChecked();
    fireEvent.click(share);
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const dialog = await screen.findByRole("dialog", {
      name: "Stop sharing this project with your team?",
    });
    expect(patches).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop sharing" }));
    await waitFor(() => expect(patches).toHaveLength(1));
    expect(JSON.parse(patches[0]).team_shared).toBe(false);
  });
});
