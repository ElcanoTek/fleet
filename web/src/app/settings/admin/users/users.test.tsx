import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import AdminUsersPage from "./page";

// Settings → Admin → Users: one table joining GET /api/admin/users
// (provisioned accounts) with GET /api/admin/stats (usage per email), with all
// account management (PATCH role/team, PUT password, DELETE, POST create)
// living in the kebab popover + the reveal add-form. Ported behaviors from the
// old UsersPanel tests, adapted to the popover UI.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; force "admin" so the page renders. (The real
// hook probes an admin endpoint; authorization stays server-side regardless.)
vi.mock("../../useIsAdmin", () => ({
  useIsAdmin: () => "admin",
}));

const USERS = [
  {
    email: "alice@x.com",
    role: "admin",
    team_id: "blue",
    created_at: 1,
    updated_at: 1,
  },
  {
    email: "bob@x.com",
    role: "member",
    team_id: "",
    created_at: 1,
    updated_at: 1,
  },
];

const NOW = Math.floor(Date.now() / 1000);
const STATS = [
  {
    email: "alice@x.com",
    conversation_count: 3,
    pinned_count: 2,
    last_activity: NOW - 7200,
    total_cost_usd: 4.96,
    total_turns: 3,
  },
];

function mockFetch(
  impl: (url: string, init?: RequestInit) => Response | Promise<Response>,
) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string | URL | Request, init?: RequestInit) =>
      Promise.resolve(impl(String(url), init)),
    ),
  );
}

// Default routing: the two list endpoints; per-test overrides layer mutations
// on top.
function listImpl(users: unknown[] = USERS, stats: unknown[] = STATS) {
  return (url: string): Response => {
    if (url === "/api/admin/users") {
      return new Response(JSON.stringify({ users }), { status: 200 });
    }
    if (url === "/api/admin/stats") {
      return new Response(JSON.stringify({ users: stats }), { status: 200 });
    }
    throw new Error(`unexpected ${url}`);
  };
}

const openKebab = (email: string) => {
  fireEvent.click(screen.getByRole("button", { name: `Edit ${email}` }));
};

describe("AdminUsersPage", () => {
  beforeEach(() => {
    mockFetch(listImpl());
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("joins provisioned users with usage stats; missing stats show em dashes", async () => {
    render(<AdminUsersPage />);
    expect(await screen.findByText("alice@x.com")).toBeInTheDocument();
    expect(screen.getByText("bob@x.com")).toBeInTheDocument();

    // Alice has stats: convs/pinned/turns/spend + last active.
    const aliceRow = screen
      .getByText("alice@x.com")
      .closest("tr") as HTMLElement;
    expect(within(aliceRow).getAllByText("3").length).toBe(2); // convs + turns
    expect(within(aliceRow).getByText("$4.96")).toBeInTheDocument();
    expect(
      within(aliceRow).getByText(/last active 2h ago/),
    ).toBeInTheDocument();
    expect(within(aliceRow).getByText(/team: blue/)).toBeInTheDocument();
    // Role badge: admin → accent "Admin".
    expect(within(aliceRow).getByText("Admin")).toBeInTheDocument();

    // Bob is provisioned but has no stats: numeric cells show "—".
    const bobRow = screen.getByText("bob@x.com").closest("tr") as HTMLElement;
    expect(within(bobRow).getAllByText("—").length).toBe(4);
    expect(within(bobRow).getByText(/last active —/)).toBeInTheDocument();
  });

  it("renders a stats-only email read-only (no kebab)", async () => {
    mockFetch(
      listImpl(USERS, [
        ...STATS,
        {
          email: "ghost@x.com",
          conversation_count: 7,
          pinned_count: 0,
          last_activity: NOW - 60,
          total_cost_usd: 0.26,
          total_turns: 3,
        },
      ]),
    );
    render(<AdminUsersPage />);
    expect(await screen.findByText("ghost@x.com")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Edit ghost@x.com" }),
    ).toBeNull();
    // Provisioned rows keep their kebab.
    expect(
      screen.getByRole("button", { name: "Edit alice@x.com" }),
    ).toBeInTheDocument();
  });

  it("disables Save until the popover is dirty, then PATCHes the change", async () => {
    const calls: { url: string; body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        calls.push({ url, body: String(init?.body) });
        return new Response(
          JSON.stringify({
            email: "bob@x.com",
            role: "viewer",
            team_id: "blue",
            created_at: 1,
            updated_at: 2,
          }),
          { status: 200 },
        );
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    const saveButton = screen.getByRole("button", { name: "Save" });
    expect(saveButton).toBeDisabled(); // no edits yet

    // Edit role (segmented) + team, then save.
    fireEvent.click(
      within(screen.getByRole("group", { name: "Chat permissions" })).getByRole(
        "button",
        { name: "Viewer" },
      ),
    );
    fireEvent.change(screen.getByLabelText("Team for bob@x.com"), {
      target: { value: "blue" },
    });
    expect(saveButton).toBeEnabled();
    fireEvent.click(saveButton);

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      role: "viewer",
      team_id: "blue",
    });
    // Popover closed; the returned row is reflected (Viewer badge) + feedback.
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    await waitFor(() => expect(screen.getByText("Saved")).toBeInTheDocument());
    expect(screen.getAllByText("Viewer").length).toBeGreaterThan(0);
  });

  it("closes the popover on Escape without saving", async () => {
    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("surfaces a 403 as an admin-only message", async () => {
    mockFetch(() => new Response("forbidden", { status: 403 }));
    render(<AdminUsersPage />);
    expect(
      await screen.findByText("You are not an admin."),
    ).toBeInTheDocument();
  });

  it("shows the empty state when no users exist", async () => {
    mockFetch(listImpl([], []));
    render(<AdminUsersPage />);
    expect(
      await screen.findByText(/No users provisioned yet/),
    ).toBeInTheDocument();
  });

  it("shows the ops badge for Operations Center admins", async () => {
    mockFetch(listImpl([{ ...USERS[0], ops_center_admin: true }, USERS[1]]));
    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");
    expect(screen.getByTitle("Operations Center admin")).toBeInTheDocument();
    expect(screen.getByText("ops: admin")).toBeInTheDocument();
  });

  it("creates a user via the add form and shows the password once", async () => {
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users" && init?.method === "POST") {
        calls.push({ body: String(init?.body) });
        return new Response(
          JSON.stringify({
            email: "carol@x.com",
            role: "member",
            team_id: "",
            created_at: 3,
            updated_at: 3,
          }),
          { status: 201 },
        );
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");

    // Reveal the form (the toggle reads "Add user" while closed).
    fireEvent.click(screen.getByRole("button", { name: /add user/i }));

    const addButton = screen.getByRole("button", { name: /^add user$/i });
    expect(addButton).toBeDisabled(); // empty form

    fireEvent.change(screen.getByLabelText("New user email"), {
      target: { value: "carol@x.com" },
    });
    fireEvent.change(screen.getByLabelText("New user password"), {
      target: { value: "carol-pw-123" },
    });
    expect(addButton).toBeEnabled();
    fireEvent.click(addButton);

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      email: "carol@x.com",
      password: "carol-pw-123",
      role: "member",
    });
    // New row appears (the email also shows in the "created" footer, so expect
    // both) and the password is shown once.
    expect(
      (await screen.findAllByText("carol@x.com")).length,
    ).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("carol-pw-123")).toBeInTheDocument();
  });

  it("requires a second click to delete, then removes the row", async () => {
    const deletes: string[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "DELETE") {
        deletes.push(url);
        return new Response(null, { status: 204 });
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    fireEvent.click(screen.getByRole("button", { name: "Delete user" })); // arms
    expect(deletes.length).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i })); // fires
    await waitFor(() => expect(deletes.length).toBe(1));
    await waitFor(() =>
      expect(screen.queryByText("bob@x.com")).not.toBeInTheDocument(),
    );
  });

  it("resets a password and shows the generated value once", async () => {
    const puts: { url: string; body: string }[] = [];
    mockFetch((url, init) => {
      if (
        url === "/api/admin/users/bob%40x.com/password" &&
        init?.method === "PUT"
      ) {
        puts.push({ url, body: String(init?.body) });
        return new Response(null, { status: 204 });
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    fireEvent.click(screen.getByRole("button", { name: "Reset password" }));
    // The popover closes and the mutation lands.
    expect(screen.queryByRole("button", { name: "Reset password" })).toBeNull();
    await waitFor(() => expect(puts.length).toBe(1));
    const sent = JSON.parse(puts[0].body) as { password: string };
    expect(sent.password.length).toBe(16);
    // The generated password is surfaced once under the row.
    expect(await screen.findByText(sent.password)).toBeInTheDocument();
  });

  it("surfaces the backend self-delete guard message", async () => {
    mockFetch((url, init) => {
      if (
        url === "/api/admin/users/alice%40x.com" &&
        init?.method === "DELETE"
      ) {
        return new Response("refusing to delete your own account", {
          status: 400,
        });
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");

    openKebab("alice@x.com");
    fireEvent.click(screen.getByRole("button", { name: "Delete user" }));
    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i }));
    expect(
      await screen.findByText("refusing to delete your own account"),
    ).toBeInTheDocument();
    // Row is NOT removed.
    expect(screen.getByText("alice@x.com")).toBeInTheDocument();
  });
});
