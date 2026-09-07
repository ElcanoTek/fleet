import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import TeamSettingsPage from "./page";

// Settings → Team (#1157, copy corrected by ADR-0057): read your own team from
// GET /api/me/team, create one with PUT, leave one behind a confirm that
// states what leaving costs, and turn the upstream 409 into the one sentence
// that helps — the name is taken, an admin can add you (joining stays
// admin-granted, and the server cannot say whether a user or a team-shared
// project holds the name, so neither does the copy).

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

const me = (
  team: string,
  admin = false,
  impact: { shared_projects?: number; shared_chats?: number } = {},
) =>
  new Response(
    JSON.stringify({
      email: "ann@x.com",
      role: admin ? "admin" : "member",
      team_id: team,
      admin,
      ...impact,
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
    // Stateful: the GET reflects the last PUT, as the server's would — the
    // page re-reads after a write, so a fixed GET would undo the create.
    let team = "";
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        puts.push(String(init.body));
        team = (JSON.parse(String(init.body)) as { team_id: string }).team_id;
        return me(team);
      }
      return me(team);
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
    // The confirmation names the ONE path that actually adds teammates.
    expect(
      screen.getByText(/Teammates get added by an admin in Settings → Admin → Users/),
    ).toBeInTheDocument();
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

    expect(
      await screen.findByText(/That name is already in use\. An admin can add you/i),
    ).toBeVisible();
    // Still teamless: the failed write must not fake success.
    expect(screen.queryByTestId("team-current")).toBeNull();
  });

  it("confirms before leaving, stating what it costs", async () => {
    const puts: string[] = [];
    let team = "platform";
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        puts.push(String(init.body));
        team = "";
        return me("");
      }
      return team
        ? me("platform", false, { shared_projects: 3, shared_chats: 2 })
        : me("");
    });

    render(<TeamSettingsPage />);
    expect(await screen.findByTestId("team-current")).toHaveTextContent(
      "platform",
    );

    fireEvent.click(screen.getByRole("button", { name: "Leave team" }));

    // Nothing is written until the consequences have been shown and accepted.
    const dialog = await screen.findByRole("dialog", { name: "Leave platform?" });
    expect(puts).toEqual([]);
    expect(dialog).toHaveTextContent("3 team-shared projects");
    expect(dialog).toHaveTextContent("2 chats you shared with the team");
    expect(dialog).toHaveTextContent("Projects you own stay yours");

    fireEvent.click(
      within(dialog).getByRole("button", { name: "Leave team" }),
    );

    await waitFor(() => expect(puts.length).toBe(1));
    expect(JSON.parse(puts[0])).toEqual({ team_id: "" });
    expect(await screen.findByText(/You left your team/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId("team-current")).toBeNull());
  });

  it("re-reads the team after a write so the Leave confirm still has its counts", async () => {
    let team = "";
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        team = (JSON.parse(String(init.body)) as { team_id: string }).team_id;
        // The PUT echoes the account row WITHOUT the LeaveTeamImpact fields.
        return me(team);
      }
      // …the GET is what carries them.
      return team ? me(team, false, { shared_projects: 0, shared_chats: 0 }) : me("");
    });

    render(<TeamSettingsPage />);
    await screen.findByText(/not in a team yet/i);
    fireEvent.change(screen.getByLabelText("Team name"), { target: { value: "platform" } });
    fireEvent.click(screen.getByRole("button", { name: "Create team" }));
    await screen.findByTestId("team-current");

    fireEvent.click(screen.getByRole("button", { name: "Leave team" }));
    const dialog = await screen.findByRole("dialog", { name: "Leave platform?" });
    // Counts from the re-read, not "we couldn't work out the numbers" from
    // the PUT's echo.
    await waitFor(() =>
      expect(dialog).toHaveTextContent("any project shared with it (there are none right now)"),
    );
    expect(dialog).not.toHaveTextContent(/couldn’t work out the exact numbers/);
  });

  it("cancelling the leave confirm writes nothing", async () => {
    const puts: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/me/team" && init?.method === "PUT") {
        puts.push(String(init.body));
        return me("");
      }
      return me("platform", false, { shared_projects: 0, shared_chats: 0 });
    });

    render(<TeamSettingsPage />);
    await screen.findByTestId("team-current");
    fireEvent.click(screen.getByRole("button", { name: "Leave team" }));

    const dialog = await screen.findByRole("dialog", { name: "Leave platform?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Leave platform?" })).toBeNull(),
    );
    expect(puts).toEqual([]);
    expect(screen.getByTestId("team-current")).toHaveTextContent("platform");
  });

  it("tells an admin they can also join an existing team", async () => {
    mockFetch(() => me("", true));

    render(<TeamSettingsPage />);
    expect(
      await screen.findByText(/As an admin you can also put yourself/i),
    ).toBeInTheDocument();
    // The wrong "everyone joins the same name" promise is gone for good.
    expect(screen.queryByText(/joins the same name/i)).toBeNull();
    expect(screen.getByRole("button", { name: "Set team" })).toBeInTheDocument();
  });
});
