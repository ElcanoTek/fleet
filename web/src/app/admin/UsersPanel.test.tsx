import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { RoleBadge, UsersPanel } from "./UsersPanel";

const USERS = [
  { email: "alice@x.com", role: "admin", team_id: "blue", created_at: 1, updated_at: 1 },
  { email: "bob@x.com", role: "member", team_id: "", created_at: 1, updated_at: 1 },
];

function mockFetch(impl: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string | URL | Request, init?: RequestInit) => Promise.resolve(impl(String(url), init))),
  );
}

describe("RoleBadge", () => {
  afterEach(cleanup);
  it("renders the role label", () => {
    render(<RoleBadge role="viewer" />);
    expect(screen.getByText("viewer")).toBeInTheDocument();
  });
});

describe("UsersPanel", () => {
  beforeEach(() => {
    mockFetch((url) => {
      if (url === "/api/admin/users") {
        return new Response(JSON.stringify({ users: USERS }), { status: 200 });
      }
      throw new Error(`unexpected ${url}`);
    });
  });
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("lists provisioned users with their role and team", async () => {
    render(<UsersPanel />);
    expect(await screen.findByText("alice@x.com")).toBeInTheDocument();
    expect(screen.getByText("bob@x.com")).toBeInTheDocument();
    // Role <select> reflects the current role.
    expect((screen.getByLabelText("Role for alice@x.com") as HTMLSelectElement).value).toBe("admin");
    expect((screen.getByLabelText("Team for alice@x.com") as HTMLInputElement).value).toBe("blue");
  });

  it("disables Save until a row is edited, then PATCHes the change", async () => {
    const calls: { url: string; body: string }[] = [];
    mockFetch((url, init) => {
      if (url === "/api/admin/users") {
        return new Response(JSON.stringify({ users: USERS }), { status: 200 });
      }
      if (url === "/api/admin/users/bob%40x.com" && init?.method === "PATCH") {
        calls.push({ url, body: String(init?.body) });
        return new Response(
          JSON.stringify({ email: "bob@x.com", role: "viewer", team_id: "blue", created_at: 1, updated_at: 2 }),
          { status: 200 },
        );
      }
      throw new Error(`unexpected ${url}`);
    });

    render(<UsersPanel />);
    await screen.findByText("bob@x.com");

    // Save starts disabled (no edits).
    const rowSaveButtons = screen.getAllByRole("button", { name: /save/i });
    const bobSave = rowSaveButtons[1];
    expect(bobSave).toBeDisabled();

    // Edit role + team, then save.
    fireEvent.change(screen.getByLabelText("Role for bob@x.com"), { target: { value: "viewer" } });
    fireEvent.change(screen.getByLabelText("Team for bob@x.com"), { target: { value: "blue" } });
    expect(bobSave).toBeEnabled();
    fireEvent.click(bobSave);

    await waitFor(() => expect(calls.length).toBe(1));
    expect(JSON.parse(calls[0].body)).toEqual({ role: "viewer", team_id: "blue" });
    // The returned row is reflected (role badge updates to viewer).
    await waitFor(() => expect(screen.getByText("Saved")).toBeInTheDocument());
  });

  it("surfaces a 403 as an admin-only message", async () => {
    mockFetch(() => new Response("forbidden", { status: 403 }));
    render(<UsersPanel />);
    expect(await screen.findByText("You are not an admin.")).toBeInTheDocument();
  });

  it("shows the empty state when no users exist", async () => {
    mockFetch((url) => {
      if (url === "/api/admin/users") return new Response(JSON.stringify({ users: [] }), { status: 200 });
      throw new Error(`unexpected ${url}`);
    });
    render(<UsersPanel />);
    expect(await screen.findByText(/No users provisioned yet/)).toBeInTheDocument();
  });
});
