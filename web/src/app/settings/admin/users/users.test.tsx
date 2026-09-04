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
import {
  parseOwnedSharedProjects,
  PROJECTS_SURFACE_HREF,
} from "./DeleteRefusal";

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

// The users table itself — the row-edit panel renders outside it (fixed
// position, after the table in the DOM), so scoping to the table is how "in
// the row" is told apart from "in the panel".
const usersTable = () => screen.getByRole("table");

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

  // #1157: the popover used to resend the row's unchanged role alongside the
  // team, which upstream reads as a self-demotion — an ADMIN_EMAILS bootstrap
  // admin (users.role = "member") could then never set their own team. The
  // PATCH now carries only the fields that actually changed.
  it("PATCHes only the changed field when editing a team", async () => {
    const calls: { url: string; body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        calls.push({ url, body: String(init?.body) });
        return new Response(
          JSON.stringify({
            email: "bob@x.com",
            role: "member",
            team_id: "platform",
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
    // A5: the field is a picker over the teams that exist, not free text —
    // "blue" is alice's team, chosen rather than retyped (and mistyped).
    fireEvent.change(screen.getByLabelText("Team for bob@x.com"), {
      target: { value: "blue" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({ team_id: "blue" });
  });

  // A5: a genuinely new team is still reachable, but only deliberately —
  // "New team…" opens a name field. Typing a near-miss of an existing name is
  // now a choice, not the default outcome of a typo.
  it("creates a new team only through the explicit New team… option", async () => {
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        calls.push({ body: String(init.body) });
        return new Response(
          JSON.stringify({
            email: "bob@x.com",
            role: "member",
            team_id: "platform",
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
    const select = screen.getByLabelText("Team for bob@x.com");
    // Existing teams are offered; the free-text field is not there until asked
    // for.
    expect(
      within(select as HTMLSelectElement)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["— No team", "blue", "New team…"]);
    expect(
      screen.queryByLabelText("Team for bob@x.com: new team name"),
    ).toBeNull();

    fireEvent.change(select, { target: { value: "__new__" } });
    fireEvent.change(
      screen.getByLabelText("Team for bob@x.com: new team name"),
      { target: { value: "platform" } },
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({ team_id: "platform" });
  });

  it("closes the popover on Escape without saving", async () => {
    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  // The popover used to keep itself open by swallowing every click with an
  // onClick on its <div> — a handler on an element with no role. It now
  // renders as a labelled dialog that takes focus, and the document-level
  // close decides "outside" by containment instead.
  it("opens the row-edit popover as a focused dialog and keeps it open on inside clicks", async () => {
    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    const pop = screen.getByRole("dialog", { name: "Edit bob@x.com" });
    expect(document.activeElement).toBe(pop);

    // A click on a control inside the popover must not close it.
    fireEvent.click(within(pop).getByLabelText("Team for bob@x.com"));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    // A click anywhere outside still closes it — and focus goes back to the
    // kebab that opened it, not to the top of the document.
    fireEvent.click(document.body);
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Edit bob@x.com" }),
    );
  });

  // Regression guard: the focus-return effect fired on EVERY close, so
  // dismissing the popover by clicking another control pulled focus back out of
  // that control. Dead-space dismissal (the case above) must still restore.
  it("leaves focus on the control that dismissed the popover", async () => {
    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");

    openKebab("bob@x.com");
    expect(screen.getByRole("dialog", { name: "Edit bob@x.com" })).toBeInTheDocument();

    const search = screen.getByPlaceholderText(/search/i);
    search.focus();
    fireEvent.click(search);

    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(document.activeElement).toBe(search);
  });

  it("moves focus into the team-rename field when the rename opens", async () => {
    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");

    // The rename control only appears once a single team is filtered to.
    fireEvent.change(screen.getByLabelText("Filter by team"), {
      target: { value: "blue" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Rename team" }));
    expect(document.activeElement).toBe(
      screen.getByLabelText("New name for team blue"),
    );
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

  it("shows one badge for unified admins and an ops badge for an ops-only admin", async () => {
    mockFetch(
      listImpl([
        { ...USERS[0], ops_center_admin: true, ops_center_role: "admin" },
        { ...USERS[1], ops_center_admin: true, ops_center_role: "admin" },
      ]),
    );
    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");
    const aliceRow = screen
      .getByText("alice@x.com")
      .closest("tr") as HTMLElement;
    expect(within(aliceRow).getAllByText("Admin")).toHaveLength(1);
    expect(within(aliceRow).queryByText("ops: admin")).toBeNull();

    const bobRow = screen.getByText("bob@x.com").closest("tr") as HTMLElement;
    expect(
      within(bobRow).getByTitle("Operations Center admin"),
    ).toBeInTheDocument();
    expect(within(bobRow).getByText("ops: admin")).toBeInTheDocument();
  });

  it("presents Admin once and grants both permission planes", async () => {
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        calls.push({ body: String(init.body) });
        return new Response(
          JSON.stringify({
            ...USERS[1],
            role: "admin",
            ops_center_admin: true,
            ops_center_role: "admin",
          }),
          { status: 200 },
        );
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");
    openKebab("bob@x.com");

    const chat = screen.getByRole("group", { name: "Chat permissions" });
    const ops = screen.getByRole("group", { name: "Ops Center permissions" });
    const admin = screen.getByRole("group", { name: "Admin permissions" });
    const permissionGroups = within(
      screen.getByRole("dialog", { name: "Edit bob@x.com" }),
    ).getAllByRole("group");
    expect(permissionGroups.map((group) => group.getAttribute("aria-label"))).toEqual([
      "Admin permissions",
      "Chat permissions",
      "Ops Center permissions",
    ]);
    expect(
      within(chat).getAllByRole("button").map((button) => button.textContent),
    ).toEqual(["Viewer", "Contributor"]);
    expect(
      within(ops).getAllByRole("button").map((button) => button.textContent),
    ).toEqual(["None", "Viewer", "Contributor"]);
    const expectTooltip = (button: HTMLElement, description: string) => {
      expect(button).not.toHaveAttribute("title");
      const tooltipId = button.getAttribute("aria-describedby");
      expect(tooltipId).toBeTruthy();
      expect(document.getElementById(tooltipId ?? "")).toHaveTextContent(
        description,
      );
    };
    expectTooltip(
      within(admin).getByRole("button", { name: "Admin" }),
      "Full permissions in both Chat and the Ops Center.",
    );
    expectTooltip(
      within(chat).getByRole("button", { name: "Viewer" }),
      "Read-only Chat access: can view but cannot create or change content.",
    );
    expectTooltip(
      within(chat).getByRole("button", { name: "Contributor" }),
      "Can actively use Chat, including creating and updating content.",
    );
    expectTooltip(
      within(ops).getByRole("button", { name: "None" }),
      "No access to the Ops Center.",
    );
    expectTooltip(
      within(ops).getByRole("button", { name: "Viewer" }),
      "Can view Ops Center tasks and logs but cannot change them.",
    );
    expectTooltip(
      within(ops).getByRole("button", { name: "Contributor" }),
      "Can view, create, and run Ops Center tasks.",
    );
    expect(within(chat).queryByRole("button", { name: "Admin" })).toBeNull();
    expect(within(ops).queryByRole("button", { name: "Admin" })).toBeNull();
    expect(within(admin).getAllByRole("button")).toHaveLength(1);

    fireEvent.click(within(admin).getByRole("button", { name: "Admin" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      role: "admin",
      ops_role: "admin",
    });
    const bobRow = screen.getByText("bob@x.com").closest("tr") as HTMLElement;
    await waitFor(() =>
      expect(within(bobRow).getAllByText("Admin")).toHaveLength(1),
    );
    expect(within(bobRow).queryByText("ops: admin")).toBeNull();
  });

  it("leaves unified Admin safely when a narrower Chat role is selected", async () => {
    const adminUser = {
      ...USERS[0],
      ops_center_admin: true,
      ops_center_role: "admin",
    };
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users/alice%40x.com" && init?.method === "PATCH") {
        calls.push({ body: String(init.body) });
        return new Response(
          JSON.stringify({
            ...adminUser,
            role: "viewer",
            ops_center_admin: false,
            ops_center_role: "",
          }),
          { status: 200 },
        );
      }
      return listImpl([adminUser, USERS[1]])(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");
    openKebab("alice@x.com");

    const admin = screen.getByRole("group", { name: "Admin permissions" });
    expect(within(admin).getByRole("button", { name: "Admin" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(
      within(screen.getByRole("group", { name: "Chat permissions" })).getByRole(
        "button",
        { name: "Viewer" },
      ),
    );
    expect(within(admin).getByRole("button", { name: "Admin" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(
      within(screen.getByRole("group", { name: "Ops Center permissions" })).getByRole(
        "button",
        { name: "None" },
      ),
    ).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      role: "viewer",
      ops_role: "none",
    });
  });

  it("creates a user with Chat, Ops Center, and team assignments", async () => {
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users" && init?.method === "POST") {
        calls.push({ body: String(init?.body) });
        return new Response(
          JSON.stringify({
            email: "carol@x.com",
            role: "viewer",
            team_id: "platform",
            created_at: 3,
            updated_at: 3,
            ops_center_role: "client",
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
    const admin = screen.getByRole("group", {
      name: "New user Admin permissions",
    });
    const chat = screen.getByRole("group", {
      name: "New user Chat permissions",
    });
    const ops = screen.getByRole("group", {
      name: "New user Ops Center permissions",
    });
    expect(within(admin).getAllByRole("button")).toHaveLength(1);
    expect(
      within(chat).getAllByRole("button").map((button) => button.textContent),
    ).toEqual(["Viewer", "Contributor"]);
    expect(
      within(ops).getAllByRole("button").map((button) => button.textContent),
    ).toEqual(["None", "Viewer", "Contributor"]);
    fireEvent.click(within(chat).getByRole("button", { name: "Viewer" }));
    fireEvent.click(within(ops).getByRole("button", { name: "Contributor" }));
    fireEvent.change(screen.getByLabelText("New user team"), {
      target: { value: "__new__" },
    });
    fireEvent.change(screen.getByLabelText("New user team: new team name"), {
      target: { value: "platform" },
    });
    expect(addButton).toBeEnabled();
    fireEvent.click(addButton);

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      email: "carol@x.com",
      password: "carol-pw-123",
      role: "viewer",
      ops_role: "client",
      team_id: "platform",
    });
    // New row appears (the email also shows in the "created" footer, so expect
    // both) and the password is shown once.
    expect(
      (await screen.findAllByText("carol@x.com")).length,
    ).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("carol-pw-123")).toBeInTheDocument();
  });

  it("creates a unified admin from the Add user permission fields", async () => {
    const calls: { body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users" && init?.method === "POST") {
        calls.push({ body: String(init.body) });
        return new Response(
          JSON.stringify({
            email: "dana@x.com",
            role: "admin",
            team_id: "",
            created_at: 3,
            updated_at: 3,
            ops_center_admin: true,
            ops_center_role: "admin",
          }),
          { status: 201 },
        );
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("alice@x.com");
    fireEvent.click(screen.getByRole("button", { name: /add user/i }));
    fireEvent.change(screen.getByLabelText("New user email"), {
      target: { value: "dana@x.com" },
    });
    fireEvent.change(screen.getByLabelText("New user password"), {
      target: { value: "dana-pw-123" },
    });
    fireEvent.click(
      within(
        screen.getByRole("group", { name: "New user Admin permissions" }),
      ).getByRole("button", { name: "Admin" }),
    );
    fireEvent.click(screen.getByRole("button", { name: /^add user$/i }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(JSON.parse(calls[0].body)).toEqual({
      email: "dana@x.com",
      password: "dana-pw-123",
      role: "admin",
      ops_role: "admin",
      team_id: "",
    });
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

  it("surfaces the backend self-delete guard message in the panel", async () => {
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
    const panel = await screen.findByRole("dialog", {
      name: "Edit alice@x.com",
    });
    expect(
      within(panel).getByText("refusing to delete your own account"),
    ).toBeInTheDocument();
    // Row is NOT removed. (Scoped to the table: the still-open panel names
    // the account too.)
    expect(within(usersTable()).getByText("alice@x.com")).toBeInTheDocument();
  });

  // ── QA #21: the fail-closed delete refusal ──
  //
  // DELETE /admin/users/{email} 409s when the account still owns team-shared
  // projects (docs/TEAM-SHARING.md "Ownership transfer"). The body is a
  // paragraph naming them — a missing step, not a failure — so it has to land
  // in the panel where Confirm delete was clicked, wrap, and offer the next
  // step. Rendered in the row it was one unwrapped line that widened the whole
  // table into a horizontal scroll.
  const OWNS_REFUSAL =
    "this account still owns team-shared projects (test 2 - shared) — " +
    "transfer them to another member first, then delete the account";

  const refuseDelete = (email: string, body: string) =>
    mockFetch((url, init) => {
      if (
        url === `/api/admin/users/${encodeURIComponent(email)}` &&
        init?.method === "DELETE"
      ) {
        return new Response(body, { status: 409 });
      }
      return listImpl()(url);
    });

  const confirmDelete = async (email: string) => {
    render(<AdminUsersPage />);
    await screen.findByText(email);
    openKebab(email);
    fireEvent.click(screen.getByRole("button", { name: "Delete user" }));
    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i }));
    return screen.findByRole("dialog", { name: `Edit ${email}` });
  };

  it("shows the delete refusal in the editor panel, never in the row", async () => {
    refuseDelete("bob@x.com", OWNS_REFUSAL);
    const panel = await confirmDelete("bob@x.com");

    // In the panel, under Confirm delete — the panel stays open on refusal.
    const notice = within(panel).getByRole("alert");
    expect(notice).toHaveTextContent(/still owns team-shared projects/);
    // …and NOT in the table (the row rendering is gone, so the table cannot
    // be widened by a server sentence).
    expect(
      within(usersTable()).queryByText(/still owns team-shared projects/),
    ).toBeNull();
    // The row keeps no leftover "Saving…" state either.
    const bobRow = within(usersTable())
      .getByText("bob@x.com")
      .closest("tr") as HTMLElement;
    expect(within(bobRow).queryByText("Saving…")).toBeNull();
  });

  it("wraps the refusal so a long sentence cannot run off the panel", async () => {
    refuseDelete("bob@x.com", OWNS_REFUSAL);
    const panel = await confirmDelete("bob@x.com");
    // jsdom does no layout, so the wrap contract is asserted as the class that
    // implements it: every line of the notice can break mid-token.
    const message = within(panel).getByText(OWNS_REFUSAL);
    expect(message.className).toContain("[overflow-wrap:anywhere]");
  });

  it("offers one Transfer link per named project", async () => {
    refuseDelete(
      "bob@x.com",
      "this account still owns team-shared projects (alpha, beta) — " +
        "transfer them to another member first, then delete the account",
    );
    const panel = await confirmDelete("bob@x.com");

    const alpha = within(panel).getByRole("link", { name: "Transfer alpha" });
    const beta = within(panel).getByRole("link", { name: "Transfer beta" });
    // The 409 carries project NAMES only (no id), and every transfer surface
    // is keyed by project id — so the honest destination is the Projects
    // surface in chat, where that project's settings dialog holds the
    // collapsed "Transfer ownership…" control. See DeleteRefusal.tsx.
    expect(alpha).toHaveAttribute("href", PROJECTS_SURFACE_HREF);
    expect(beta).toHaveAttribute("href", PROJECTS_SURFACE_HREF);
    expect(within(panel).getAllByRole("link").length).toBe(2);
  });

  it("keeps the refusal when many projects push it past the generic length cap", async () => {
    // The generic error-body rule stops at 200 chars (an anti-HTML-page
    // guard); the refusal grows with the project list, so an account owning
    // several would otherwise lose the whole sentence to "Delete failed
    // (409)." — the one thing the admin needed to read.
    const names = ["alpha", "beta", "gamma", "delta", "epsilon", "zeta"].map(
      (n) => `${n} — quarterly research workspace`,
    );
    const body =
      `this account still owns team-shared projects (${names.join(", ")}) — ` +
      "transfer them to another member first, then delete the account";
    expect(body.length).toBeGreaterThan(200);
    refuseDelete("bob@x.com", body);
    const panel = await confirmDelete("bob@x.com");
    expect(within(panel).getByRole("alert")).toHaveTextContent(
      /still owns team-shared projects/,
    );
    expect(within(panel).getAllByRole("link").length).toBe(names.length);
  });

  it("shows no transfer links for a refusal that names no project", async () => {
    refuseDelete("bob@x.com", "Delete failed (500).");
    const panel = await confirmDelete("bob@x.com");
    expect(within(panel).getByRole("alert")).toHaveTextContent(
      "Delete failed (500).",
    );
    expect(within(panel).queryAllByRole("link").length).toBe(0);
  });

  it("keeps row-level status text wrap-safe and width-capped", async () => {
    // Save/reset failures still report in the row (they finish after the panel
    // closes), so they carry the same wrap contract: a long server sentence
    // must break and must not widen the column.
    const long =
      "PATCH refused: " + "unbreakable-token-".repeat(7) + "end-of-sentence";
    mockFetch((url, init) => {
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        return new Response(long, { status: 400 });
      }
      return listImpl()(url);
    });

    render(<AdminUsersPage />);
    await screen.findByText("bob@x.com");
    openKebab("bob@x.com");
    fireEvent.change(screen.getByLabelText("Team for bob@x.com"), {
      target: { value: "blue" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    const status = await screen.findByText(long);
    expect(status.className).toContain("[overflow-wrap:anywhere]");
    expect(status.className).toContain("whitespace-normal");
    expect(status.className).toMatch(/max-w-\[/);
  });
});

describe("parseOwnedSharedProjects", () => {
  it("reads the names out of the 409 sentence", () => {
    expect(parseOwnedSharedProjects(
      "this account still owns team-shared projects (alpha, beta gamma) — " +
        "transfer them to another member first, then delete the account",
    )).toEqual(["alpha", "beta gamma"]);
  });

  it("returns nothing for any other error text", () => {
    expect(parseOwnedSharedProjects("Delete failed (500).")).toEqual([]);
    expect(parseOwnedSharedProjects("user not found")).toEqual([]);
  });
});

it("search narrows the table and the summary reflects it", async () => {
  mockFetch(listImpl(USERS));
  render(<AdminUsersPage />);
  await screen.findByText(USERS[0].email);
  expect(screen.getByText(USERS[1].email)).toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Search users"), {
    target: { value: USERS[1].email.slice(0, 3) },
  });
  expect(screen.queryByText(USERS[0].email)).not.toBeInTheDocument();
  expect(screen.getByText(USERS[1].email)).toBeInTheDocument();
  expect(screen.getByText(/1 of 2 accounts?/)).toBeInTheDocument();
});

it("role filter narrows to admins", async () => {
  mockFetch(listImpl(USERS));
  render(<AdminUsersPage />);
  await screen.findByText(USERS[0].email);
  fireEvent.change(screen.getByLabelText("Filter by chat role"), {
    target: { value: "admin" },
  });
  // USERS[0] is the admin fixture; the viewer row disappears
  expect(screen.getByText(USERS[0].email)).toBeInTheDocument();
  expect(screen.queryByText(USERS[1].email)).not.toBeInTheDocument();
});
