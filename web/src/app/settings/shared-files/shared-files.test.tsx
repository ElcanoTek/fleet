import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import SharedFilesPage from "./page";

// Settings → Shared files — the cross-chat shared file library. Load-bearing
// assertions: a member gets the read-only library (grouped list + download
// links, no manage controls); an admin gets upload/rename/delete on top;
// delete fires only after the inline confirm and DELETEs /api/shared-files/
// {id}; rename PATCHes only the changed field; the usage meter renders both
// the capped and the unlimited form; an empty library shows the empty state
// instead of a blank panel.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; each test picks the state. (The real hook
// probes an admin endpoint; authorization stays server-side regardless.)
let adminState: "unknown" | "admin" | "member" = "admin";
vi.mock("../useIsAdmin", () => ({
  useIsAdmin: () => adminState,
}));

type SharedFile = {
  id: string;
  name: string;
  folder: string;
  description: string;
  size_bytes: number;
  uploaded_by?: string;
  created_at: number;
  updated_at: number;
};

type Library = {
  files: SharedFile[];
  total_bytes: number;
  max_total_bytes: number;
};

const REPORT: SharedFile = {
  id: "f-report",
  name: "quarterly-report.pdf",
  folder: "",
  description: "Q2 numbers",
  size_bytes: 1024 * 1024,
  uploaded_by: "boss@x.com",
  created_at: 1_755_000_000,
  updated_at: 1_755_000_000,
};

const PLAYBOOK: SharedFile = {
  id: "f-playbook",
  name: "playbook.md",
  folder: "docs",
  description: "",
  size_bytes: 512 * 1024,
  uploaded_by: "boss@x.com",
  created_at: 1_755_000_000,
  updated_at: 1_755_000_000,
};

const LIBRARY: Library = {
  files: [REPORT, PLAYBOOK],
  total_bytes: 1536 * 1024, // 1.5 MB
  max_total_bytes: 100 * 1024 * 1024, // 100 MB
};

function mockFetch(
  library: Library,
  onWrite?: (url: string, init: RequestInit) => { status: number; body: unknown } | undefined,
) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    if (!init || init.method === undefined || init.method === "GET") {
      return {
        ok: true,
        status: 200,
        json: async () => library,
        text: async () => JSON.stringify(library),
      };
    }
    const custom = onWrite?.(url, init);
    if (custom) {
      return {
        ok: custom.status < 400,
        status: custom.status,
        json: async () => custom.body,
        text: async () =>
          typeof custom.body === "string" ? custom.body : JSON.stringify(custom.body),
      };
    }
    return { ok: true, status: 204, json: async () => ({}), text: async () => "" };
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  adminState = "admin";
});

describe("SharedFilesPage", () => {
  it("shows a member the read-only library without manage controls", async () => {
    adminState = "member";
    vi.stubGlobal("fetch", mockFetch(LIBRARY));
    render(<SharedFilesPage />);

    // Grouped by folder, library root first, with a download link per row.
    expect(await screen.findByText("quarterly-report.pdf")).toBeInTheDocument();
    // Role queries: the folder name also appears in the datalist suggestions,
    // so match the group heading specifically.
    expect(screen.getByRole("heading", { name: "Library root" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "docs" })).toBeInTheDocument();
    expect(screen.getByText("playbook.md")).toBeInTheDocument();
    // Both rows carry the uploader + updated-date attribution.
    expect(screen.getAllByText(/uploaded by boss@x.com/)).toHaveLength(2);
    expect(
      screen.getByRole("link", { name: "Download quarterly-report.pdf" }),
    ).toHaveAttribute("href", "/api/shared-files/f-report/download");

    // No manage controls for a member — the server rejects them anyway.
    expect(screen.queryByTestId("upload-files-input")).toBeNull();
    expect(screen.queryByTestId("edit-f-report")).toBeNull();
    expect(screen.queryByTestId("delete-f-report")).toBeNull();
  });

  it("shows an admin the upload, rename, and delete affordances", async () => {
    vi.stubGlobal("fetch", mockFetch(LIBRARY));
    render(<SharedFilesPage />);

    expect(await screen.findByText("quarterly-report.pdf")).toBeInTheDocument();
    expect(screen.getByTestId("upload-files-input")).toBeInTheDocument();
    expect(screen.getByTestId("edit-f-report")).toBeInTheDocument();
    expect(screen.getByTestId("delete-f-report")).toBeInTheDocument();
    // The cap points the admin at the setting that controls it.
    expect(screen.getByText("shared_files_max_total_mb")).toBeInTheDocument();
  });

  it("deletes only after the inline confirm, via DELETE /api/shared-files/{id}", async () => {
    const fetchMock = mockFetch(LIBRARY, (url, init) => {
      if (init.method === "DELETE" && url.includes("f-report")) {
        return { status: 204, body: "" };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SharedFilesPage />);

    const del = await screen.findByTestId("delete-f-report");
    // First click arms; no DELETE yet.
    fireEvent.click(del);
    expect(del).toHaveTextContent("Confirm delete");
    expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false);
    // Second click fires the DELETE.
    fireEvent.click(del);
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(true),
    );
    const call = fetchMock.mock.calls.find(([, i]) => i?.method === "DELETE");
    expect(call?.[0]).toBe("/api/shared-files/f-report");
  });

  it("renames via PATCH carrying only the changed name", async () => {
    const fetchMock = mockFetch(LIBRARY, (url, init) => {
      if (init.method === "PATCH" && url.includes("f-report")) {
        return { status: 200, body: { ...REPORT, name: "renamed.pdf" } };
      }
      return undefined;
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<SharedFilesPage />);

    fireEvent.click(await screen.findByTestId("edit-f-report"));
    const name = screen.getByTestId("edit-name-f-report");
    fireEvent.change(name, { target: { value: "renamed.pdf" } });
    fireEvent.click(screen.getByTestId("save-edit-f-report"));

    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === "PATCH")).toBe(true),
    );
    const patch = fetchMock.mock.calls.find(([, i]) => i?.method === "PATCH");
    expect(patch?.[0]).toBe("/api/shared-files/f-report");
    // Only the field that changed — folder/description absence means
    // "leave as-is" on the wire.
    expect(JSON.parse(String(patch?.[1]?.body))).toEqual({ name: "renamed.pdf" });
    // The editor closes once the save lands.
    await waitFor(() => expect(screen.queryByTestId("edit-name-f-report")).toBeNull());
  });

  it("renders the capped usage meter", async () => {
    vi.stubGlobal("fetch", mockFetch(LIBRARY));
    render(<SharedFilesPage />);
    expect(await screen.findByTestId("usage-meter")).toHaveTextContent(
      "1.5 MB of 100.0 MB used",
    );
  });

  it("renders the unlimited usage meter when max_total_bytes is 0", async () => {
    vi.stubGlobal("fetch", mockFetch({ ...LIBRARY, max_total_bytes: 0 }));
    render(<SharedFilesPage />);
    expect(await screen.findByTestId("usage-meter")).toHaveTextContent(
      "1.5 MB used — no limit",
    );
  });

  it("shows the empty state for an empty library", async () => {
    vi.stubGlobal("fetch", mockFetch({ files: [], total_bytes: 0, max_total_bytes: 0 }));
    render(<SharedFilesPage />);
    expect(await screen.findByText(/No shared files yet/)).toBeInTheDocument();
  });
});
