import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import TeamSettingsPage from "./page";

// Settings → Team (#1157): read your own team from GET /api/me/team, create one
// with PUT, leave one, and surface the upstream 409 verbatim when the name
// belongs to somebody else's trust group (joining stays admin-granted).

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

const me = (team: string, admin = false) =>
  new Response(
    JSON.stringify({
      email: "ann@x.com",
      role: admin ? "admin" : "member",
      team_id: team,
      admin,
    }),
    { status: 200 },
  );

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("Settings → Team", () => {
  it("creates a team from the empty state", async () => {
    const puts: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        puts.push(String(init.body));
        return me("platform");
      }
      return me("");
    });

    render(<TeamSettingsPage />);
    await screen.findByText(/not in a team yet/i);

    fireEvent.change(screen.getByLabelText("Team name"), {
      target: { value: " platform " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));

    await waitFor(() => expect(puts.length).toBe(1));
    expect(JSON.parse(puts[0])).toEqual({ team_id: "platform" }); // trimmed
    expect(await screen.findByTestId("team-current")).toHaveTextContent(
      "platform",
    );
    expect(screen.getByText(/You are now in team/)).toBeInTheDocument();
  });

  it("shows the upstream conflict when the team belongs to someone else", async () => {
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        return new Response(
          "that team already exists — ask an admin to add you to it",
          { status: 409 },
        );
      }
      return me("");
    });

    render(<TeamSettingsPage />);
    await screen.findByText(/not in a team yet/i);

    fireEvent.change(screen.getByLabelText("Team name"), {
      target: { value: "platform" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));

    expect(await screen.findByText(/ask an admin to add you/i)).toBeVisible();
    // Still teamless: the failed write must not fake success.
    expect(screen.queryByTestId("team-current")).toBeNull();
  });

  it("leaves the current team", async () => {
    const puts: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        puts.push(String(init.body));
        return me("");
      }
      return me("platform");
    });

    render(<TeamSettingsPage />);
    expect(await screen.findByTestId("team-current")).toHaveTextContent(
      "platform",
    );

    fireEvent.click(screen.getByRole("button", { name: "Leave team" }));

    await waitFor(() => expect(puts.length).toBe(1));
    expect(JSON.parse(puts[0])).toEqual({ team_id: "" });
    expect(await screen.findByText(/You left your team/)).toBeInTheDocument();
  });

  it("tells an admin they can also join an existing team", async () => {
    mockFetch(() => me("", true));

    render(<TeamSettingsPage />);
    expect(
      await screen.findByText(/As an admin you can also put yourself/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set team" })).toBeInTheDocument();
  });
});
