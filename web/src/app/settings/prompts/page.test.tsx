import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import PromptsPage from "./page";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("Settings → Prompts page", () => {
  it("lists workspace and deployment prompts, creates a new prompt, and deletes owned prompts", async () => {
    const prompts = [
      {
        id: "p-1",
        name: "Daily Pacing Check",
        description: "Checks deal pacing against target",
        content: "Check deal pacing for all live campaigns.",
        visibility: "workspace",
        read_only: false,
        owned_by_caller: true,
      },
      {
        id: "git-1",
        name: "Weekly Revenue Audit",
        description: "Bundled deployment audit prompt",
        content: "Run the weekly revenue audit.",
        visibility: "workspace",
        read_only: true,
        owned_by_caller: false,
      },
    ];

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? "GET";
        if (url === "/api/orchestrator/prompts" && method === "GET") {
          return new Response(JSON.stringify(prompts), { status: 200 });
        }
        if (url === "/api/orchestrator/prompts" && method === "POST") {
          const body = JSON.parse(String(init?.body));
          const created = {
            id: "p-2",
            name: body.name,
            description: body.description,
            content: body.content,
            visibility: body.visibility,
            read_only: false,
            owned_by_caller: true,
          };
          prompts.unshift(created);
          return new Response(JSON.stringify(created), { status: 201 });
        }
        if (url === "/api/orchestrator/prompts/p-1" && method === "DELETE") {
          const idx = prompts.findIndex((p) => p.id === "p-1");
          if (idx >= 0) prompts.splice(idx, 1);
          return new Response(null, { status: 204 });
        }
        return new Response("not found", { status: 404 });
      }),
    );

    render(<PromptsPage />);

    expect(await screen.findByText("Daily Pacing Check")).toBeInTheDocument();
    expect(await screen.findByText("Weekly Revenue Audit")).toBeInTheDocument();

    // Open New prompt form and create one
    fireEvent.click(screen.getByTestId("new-prompt-toggle"));
    fireEvent.change(screen.getByLabelText("Prompt name"), {
      target: { value: "Margin Alert" },
    });
    fireEvent.change(screen.getByLabelText("Prompt text"), {
      target: { value: "Check margin below 20%." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create prompt" }));

    expect(await screen.findByText("Margin Alert")).toBeInTheDocument();

    // Delete Daily Pacing Check via two-click InlineConfirmButton
    const deleteButtons = screen.getAllByRole("button", { name: "Delete" });
    fireEvent.click(deleteButtons[1]);
    fireEvent.click(screen.getByRole("button", { name: "Confirm delete" }));
    await waitFor(() =>
      expect(screen.queryByText("Daily Pacing Check")).toBeNull(),
    );
  });
});
