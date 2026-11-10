import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useEffect } from "react";
import { MemoryManagerDialog, MemoryPromoteDialog, type PromoteTarget } from "./MemoryManagerDialog";
import { useChatMemories } from "./useChatMemories";
import type { UserMemory } from "@/app/shared/lib/memoriesApi";

// The memory manager moved out of chat-experience.tsx (#2008): the dialog
// over the real useChatMemories hook, against a mocked /api/memories.

const memory = (over: Partial<UserMemory>): UserMemory => ({
  id: "m1",
  content: "Deal names may contain typos.",
  source: "manual",
  kind: "fact",
  created_at: 1,
  updated_at: 1,
  ...over,
});

type Call = { url: string; method: string; body: unknown };

let stored: UserMemory[];
let calls: Call[];

function mockBackend(opts: { failWrites?: boolean } = {}) {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ url, method, body });
      if (method !== "GET" && opts.failWrites) return new Response("db locked", { status: 500 });
      if (url === "/api/memories" && method === "GET") return Response.json({ memories: stored });
      if (url === "/api/memories" && method === "POST") {
        stored = [...stored, memory({ id: "m-new", content: body.content, kind: body.kind })];
        return Response.json({});
      }
      const m = /^\/api\/memories\/([^/]+)$/.exec(url);
      if (m && method === "PATCH") {
        stored = stored.map((x) => (x.id === m[1] ? { ...x, ...body } : x));
        return Response.json({});
      }
      if (m && method === "DELETE") {
        stored = stored.filter((x) => x.id !== m[1]);
        return new Response(null, { status: 204 });
      }
      if (/^\/api\/projects\/[^/]+\/memories$/.test(url) && method === "POST") {
        stored = stored.filter((x) => x.id !== body.from_memory_id);
        return Response.json({});
      }
      return Response.json({}, { status: 404 });
    }),
  );
}

function Harness({ projects = [] as PromoteTarget[] }) {
  const mem = useChatMemories();
  const { openMemoryManager } = mem;
  useEffect(() => {
    openMemoryManager();
    // Mount-once: open the manager as the composer's Memories item does.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return (
    <>
      {mem.memoryManagerOpen ? (
        <MemoryManagerDialog
          mem={mem}
          activeMemoryView={mem.memoryView}
          activeProjectForMemory={null}
          activeProjectOwner=""
          teamSharedProjects={projects}
          userEmail="me@example.com"
        />
      ) : null}
      {mem.promoteMemoryId ? (
        <MemoryPromoteDialog
          id={mem.promoteMemoryId}
          teamSharedProjects={projects}
          promoteMemoryToProject={mem.promoteMemoryToProject}
          onCancel={() => mem.setPromoteMemoryId(null)}
        />
      ) : null}
    </>
  );
}

const dialog = () => screen.getByRole("dialog", { name: "Memories" });

beforeEach(() => {
  stored = [memory({})];
  mockBackend();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("MemoryManagerDialog", () => {
  it("lists the memories and adds one", async () => {
    render(<Harness />);
    expect(await screen.findByText("Deal names may contain typos.")).toBeInTheDocument();

    fireEvent.change(within(dialog()).getByRole("textbox"), { target: { value: "  Prefers tables.  " } });
    fireEvent.change(within(dialog()).getByLabelText("Kind"), { target: { value: "preference" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Add memory" }));

    expect(await screen.findByText("Prefers tables.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")).toEqual({
      url: "/api/memories",
      method: "POST",
      body: { content: "Prefers tables.", kind: "preference" },
    });
    expect(within(dialog()).getByRole("textbox")).toHaveValue("");
  });

  it("edits a memory in place", async () => {
    render(<Harness />);
    await screen.findByText("Deal names may contain typos.");

    fireEvent.click(within(dialog()).getByRole("button", { name: "Edit" }));
    const box = within(dialog()).getByRole("textbox");
    expect(box).toHaveValue("Deal names may contain typos.");
    fireEvent.change(box, { target: { value: "Deal names are spelled loosely." } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Save changes" }));

    expect(await screen.findByText("Deal names are spelled loosely.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")).toEqual({
      url: "/api/memories/m1",
      method: "PATCH",
      body: { content: "Deal names are spelled loosely.", kind: "fact" },
    });
  });

  it("cancels an edit", async () => {
    render(<Harness />);
    await screen.findByText("Deal names may contain typos.");
    fireEvent.click(within(dialog()).getByRole("button", { name: "Edit" }));
    fireEvent.click(within(dialog()).getByRole("button", { name: "Cancel edit" }));
    expect(within(dialog()).getByRole("textbox")).toHaveValue("");
    expect(within(dialog()).getByRole("button", { name: "Add memory" })).toBeInTheDocument();
  });

  it("pins and deletes a memory", async () => {
    render(<Harness />);
    await screen.findByText("Deal names may contain typos.");

    fireEvent.click(within(dialog()).getByRole("button", { name: "Pin" }));
    expect(await within(dialog()).findByRole("button", { name: "Unpin" })).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ pinned: true });

    fireEvent.click(within(dialog()).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.queryByText("Deal names may contain typos.")).not.toBeInTheDocument());
    expect(calls.some((c) => c.url === "/api/memories/m1" && c.method === "DELETE")).toBe(true);
  });

  it("moves a memory straight to the one team project", async () => {
    render(<Harness projects={[{ id: "p1", name: "Quant", team_id: "quant" }]} />);
    await screen.findByText("Deal names may contain typos.");

    fireEvent.click(within(dialog()).getByRole("button", { name: "Move to team learnings" }));
    await waitFor(() => expect(screen.queryByText("Deal names may contain typos.")).not.toBeInTheDocument());
    expect(calls.find((c) => c.url === "/api/projects/p1/memories")?.body).toEqual({ from_memory_id: "m1" });
  });

  it("asks which project when there are several", async () => {
    render(
      <Harness
        projects={[
          { id: "p1", name: "Quant", team_id: "quant" },
          { id: "p2", name: "Ops", team_id: "ops" },
        ]}
      />,
    );
    await screen.findByText("Deal names may contain typos.");

    fireEvent.click(within(dialog()).getByRole("button", { name: "Move to team learnings" }));
    const picker = await screen.findByRole("dialog", { name: "Move to team learnings" });
    fireEvent.click(within(picker).getByRole("button", { name: /Ops/ }));

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Move to team learnings" })).not.toBeInTheDocument(),
    );
    expect(calls.find((c) => c.url === "/api/projects/p2/memories")?.body).toEqual({ from_memory_id: "m1" });
  });

  it("shows the server's reason when a write fails", async () => {
    render(<Harness />);
    await screen.findByText("Deal names may contain typos.");
    mockBackend({ failWrites: true });

    fireEvent.click(within(dialog()).getByRole("button", { name: "Delete" }));
    expect(await within(dialog()).findByText("db locked")).toBeInTheDocument();
    expect(screen.getByText("Deal names may contain typos.")).toBeInTheDocument();
  });

  it("renders standard picker-modal shell and filters memories by search and status", async () => {
    stored = [
      memory({ id: "m1", content: "Deal names may contain typos.", pinned: true }),
      memory({ id: "m2", content: "Always format dates as ISO-8601.", pinned: false }),
      memory({ id: "m3", content: "Legacy archived note.", retired_at: 100 }),
    ];
    mockBackend();
    render(<Harness />);

    const dlg = await screen.findByTestId("memory-manager-dialog");
    expect(dlg.classList.contains("picker-modal")).toBe(true);
    expect(dlg.querySelector(".picker-modal__head")).toBeTruthy();
    expect(dlg.querySelector(".picker-modal__rail")).toBeTruthy();
    expect(dlg.querySelector(".picker-modal__body")).toBeTruthy();
    expect(dlg.querySelector(".picker-modal__foot")).toBeTruthy();
    expect(await within(dlg).findByText("2 active · 1 pinned")).toBeInTheDocument();
    expect(within(dlg).getByText("3 memories saved")).toBeInTheDocument();

    const searchBox = within(dlg).getByRole("searchbox", { name: "Search memories" });
    fireEvent.change(searchBox, { target: { value: "ISO-8601" } });
    expect(within(dlg).getByText("Always format dates as ISO-8601.")).toBeInTheDocument();
    expect(within(dlg).queryByText("Deal names may contain typos.")).toBeNull();

    fireEvent.change(searchBox, { target: { value: "" } });
    fireEvent.click(within(dlg).getByRole("button", { name: "Pinned" }));
    expect(within(dlg).getByText("Deal names may contain typos.")).toBeInTheDocument();
    expect(within(dlg).queryByText("Always format dates as ISO-8601.")).toBeNull();

    fireEvent.click(within(dlg).getByRole("button", { name: "Retired" }));
    expect(within(dlg).getByText("Legacy archived note.")).toBeInTheDocument();
    expect(within(dlg).queryByText("Deal names may contain typos.")).toBeNull();
  });
});
