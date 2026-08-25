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
    fireEvent.change(screen.getByLabelText("Team for bob@x.com"), {
      target: { value: "platform" },
    });
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
