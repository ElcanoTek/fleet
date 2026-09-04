import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  render,
  screen,
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
