import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { McpServerPicker } from "./McpServerPicker";
import type { McpServer, MCPChoice } from "@/app/shared/lib/orchestratorApi";

// The P7 gate: ONE McpServerPicker reused in BOTH the chat conversation toolbar
// (mode="conversation") and the orchestrator task form (mode="task"), rendering
// IDENTICALLY across modes. These tests assert that the set of controls and
// their structure are the same regardless of mode, and that the
// enable/disable + per-MCP account selection behave correctly.

const SERVERS: McpServer[] = [
  { name: "xandr", description: "Xandr DSP", tool_count: 7, accounts: ["client_a", "client_b"] },
  { name: "magnite", description: "Magnite SSP", tool_count: 4, accounts: [] },
];

// Strip the data-mode attribute + mode-specific aria-labels so two renders can
// be compared structurally. Everything else must be byte-identical.
function normalize(html: string): string {
  return html
    .replace(/data-mode="(conversation|task)"/g, 'data-mode="MODE"')
    .replace(/MCP servers for this (task|conversation)/g, "MCP servers for this MODE")
    // The per-field DOM ids embed the mode only to keep them unique across the
    // two simultaneously-mounted instances; collapse the token so the
    // structural comparison ignores it.
    .replace(/mcp-(conversation|task)-/g, "mcp-MODE-");
}

describe("McpServerPicker — identical rendering across modes", () => {
  it("renders the SAME controls in mode=conversation and mode=task", () => {
    const selection: MCPChoice[] = [{ server: "xandr", account: "client_a" }];

    const conv = render(
      <McpServerPicker mode="conversation" servers={SERVERS} selection={selection} onChange={() => {}} />,
    );
    const convHtml = normalize(conv.container.innerHTML);
    cleanup();

    const task = render(
      <McpServerPicker mode="task" servers={SERVERS} selection={selection} onChange={() => {}} />,
    );
    const taskHtml = normalize(task.container.innerHTML);

    expect(taskHtml).toBe(convHtml);
  });

  it("exposes a toggle AND an account dropdown per server in BOTH modes", () => {
    for (const mode of ["conversation", "task"] as const) {
      render(<McpServerPicker mode={mode} servers={SERVERS} selection={[]} onChange={() => {}} />);
      expect(screen.getByTestId("mcp-toggle-xandr")).toBeInTheDocument();
      expect(screen.getByTestId("mcp-account-xandr")).toBeInTheDocument();
      expect(screen.getByTestId("mcp-toggle-magnite")).toBeInTheDocument();
      expect(screen.getByTestId("mcp-account-magnite")).toBeInTheDocument();
      cleanup();
    }
  });
});

describe("McpServerPicker — enable/disable + account selection", () => {
  it("enabling a server adds it to the selection", () => {
    const onChange = vi.fn();
    render(<McpServerPicker mode="task" servers={SERVERS} selection={[]} onChange={onChange} />);
    fireEvent.click(screen.getByTestId("mcp-toggle-xandr"));
    expect(onChange).toHaveBeenCalledWith([{ server: "xandr" }]);
  });

  it("disabling a server removes it from the selection", () => {
    const onChange = vi.fn();
    render(
      <McpServerPicker
        mode="task"
        servers={SERVERS}
        selection={[{ server: "xandr" }]}
        onChange={onChange}
      />,
    );
    fireEvent.click(screen.getByTestId("mcp-toggle-xandr"));
    expect(onChange).toHaveBeenCalledWith([]);
  });

  it("selecting an account sets it on the enabled server's choice", () => {
    const onChange = vi.fn();
    render(
      <McpServerPicker
        mode="task"
        servers={SERVERS}
        selection={[{ server: "xandr" }]}
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByTestId("mcp-account-xandr"), { target: { value: "client_b" } });
    expect(onChange).toHaveBeenCalledWith([{ server: "xandr", account: "client_b" }]);
  });

  it("account dropdown is disabled until the server is enabled", () => {
    render(<McpServerPicker mode="task" servers={SERVERS} selection={[]} onChange={() => {}} />);
    expect(screen.getByTestId("mcp-account-xandr")).toBeDisabled();
  });

  it("offers the configured account names plus a Default seat", () => {
    render(
      <McpServerPicker
        mode="task"
        servers={SERVERS}
        selection={[{ server: "xandr" }]}
        onChange={() => {}}
      />,
    );
    const select = screen.getByTestId("mcp-account-xandr") as HTMLSelectElement;
    const options = Array.from(select.options).map((o) => o.value);
    expect(options).toEqual(["", "client_a", "client_b"]);
  });
});
