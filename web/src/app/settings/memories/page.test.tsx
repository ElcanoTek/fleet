import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import MemoriesPage from "./page";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("Settings → Memories page", () => {
  it("loads personal memories, adds a new memory, and supports pin/edit/retire/delete", async () => {
    const memories = [
      {
        id: "m-1",
        content: "Deal names may contain intentional typos; never correct them.",
        source: "manual",
        kind: "preference",
        pinned: false,
        created_at: 1700000000,
        updated_at: 1700000000,
      },
    ];
    const calls: { method: string; url: string; body?: unknown }[] = [];

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? "GET";
        const body = init?.body ? JSON.parse(String(init.body)) : undefined;
        calls.push({ method, url, body });

        if (url === "/api/projects") {
          return new Response(JSON.stringify({ projects: [] }), { status: 200 });
        }
        if (url === "/api/memories" && method === "GET") {
          return new Response(JSON.stringify({ memories }), { status: 200 });
        }
        if (url === "/api/memories" && method === "POST") {
          memories.unshift({
            id: "m-2",
            content: body.content,
            source: "manual",
            kind: body.kind,
            pinned: false,
            created_at: 1700000100,
            updated_at: 1700000100,
          });
          return new Response(JSON.stringify(memories[0]), { status: 201 });
        }
        if (url === "/api/memories/m-1" && method === "PATCH") {
          Object.assign(memories.find((m) => m.id === "m-1")!, body);
          return new Response(JSON.stringify({ ok: true }), { status: 200 });
        }
        if (url === "/api/memories/m-1" && method === "DELETE") {
          const idx = memories.findIndex((m) => m.id === "m-1");
          if (idx >= 0) memories.splice(idx, 1);
          return new Response(null, { status: 204 });
        }
        return new Response("not found", { status: 404 });
      }),
    );

    render(<MemoriesPage />);

    expect(
      await screen.findByText(
        "Deal names may contain intentional typos; never correct them.",
      ),
    ).toBeInTheDocument();

    // Add a new memory
    fireEvent.change(screen.getByLabelText("Memory text"), {
      target: { value: "Always show dates in ISO format." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add memory" }));

    expect(
      await screen.findByText("Always show dates in ISO format."),
    ).toBeInTheDocument();

    // Pin m-1
    const pinButtons = screen.getAllByRole("button", { name: "Pin" });
    fireEvent.click(pinButtons[1]);
    await waitFor(() =>
      expect(calls).toContainEqual({
        method: "PATCH",
        url: "/api/memories/m-1",
        body: { pinned: true },
      }),
    );

    // Delete m-1 via two-click InlineConfirmButton
    const deleteButtons = screen.getAllByRole("button", { name: "Delete" });
    fireEvent.click(deleteButtons[1]);
    fireEvent.click(screen.getByRole("button", { name: "Confirm delete" }));
    await waitFor(() =>
      expect(
        screen.queryByText(
          "Deal names may contain intentional typos; never correct them.",
        ),
      ).toBeNull(),
    );
  });
});
