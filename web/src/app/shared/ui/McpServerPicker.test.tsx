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

  it("exposes a switch per server in BOTH modes; the account dropdown appears once enabled", () => {
    for (const mode of ["conversation", "task"] as const) {
      render(
        <McpServerPicker mode={mode} servers={SERVERS} selection={[{ server: "xandr" }]} onChange={() => {}} />,
      );
      expect(screen.getByTestId("mcp-toggle-xandr")).toBeInTheDocument();
      expect(screen.getByTestId("mcp-toggle-magnite")).toBeInTheDocument();
      // Enabled → account seat picker; disabled → no dropdown (the row shows
      // the server's purpose instead).
      expect(screen.getByTestId("mcp-account-xandr")).toBeInTheDocument();
      expect(screen.queryByTestId("mcp-account-magnite")).not.toBeInTheDocument();
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

  it("hides the account dropdown until the server is enabled", () => {
    render(<McpServerPicker mode="task" servers={SERVERS} selection={[]} onChange={() => {}} />);
    expect(screen.queryByTestId("mcp-account-xandr")).not.toBeInTheDocument();
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

  it("keeps catalog order regardless of enablement (a toggled row must not move)", () => {
    render(
      <McpServerPicker
        mode="task"
        servers={SERVERS}
        selection={[{ server: "magnite" }]}
        onChange={() => {}}
      />,
    );
    const rows = Array.from(document.querySelectorAll("[data-server]")).map((r) =>
      r.getAttribute("data-server"),
    );
    expect(rows).toEqual(["xandr", "magnite"]);
  });

  it("shows the tool count only on enabled rows and pluralizes it", () => {
    const ONE_TOOL: McpServer[] = [{ name: "solo", description: "One-tool server", tool_count: 1, accounts: [] }];
    const { rerender } = render(
      <McpServerPicker mode="task" servers={ONE_TOOL} selection={[]} onChange={() => {}} />,
    );
    // Disabled → purpose text, no count.
    expect(screen.queryByText(/1 tool/)).not.toBeInTheDocument();
    expect(screen.getByText("One-tool server")).toBeInTheDocument();
    rerender(
      <McpServerPicker mode="task" servers={ONE_TOOL} selection={[{ server: "solo" }]} onChange={() => {}} />,
    );
    expect(screen.getByText("1 tool")).toBeInTheDocument();
    expect(screen.queryByText("1 tools")).not.toBeInTheDocument();
  });
});

describe("McpServerPicker — per-user remote (hosted) servers (#466)", () => {
  const WITH_REMOTE: McpServer[] = [
    ...SERVERS,
    { name: "my-notion", description: "Remote MCP server you connected.", remote: true },
  ];

  it("renders a connected remote server as a read-only row (no switch, no account)", () => {
    render(<McpServerPicker mode="task" servers={WITH_REMOTE} selection={[]} onChange={() => {}} />);
    const remote = screen.getByTestId("mcp-remote-my-notion");
    expect(remote).toBeInTheDocument();
    expect(remote).toHaveTextContent("Connected");
    // It is NOT a per-task toggle and carries no credential-seat dropdown.
    expect(screen.queryByTestId("mcp-toggle-my-notion")).not.toBeInTheDocument();
    expect(screen.queryByTestId("mcp-account-my-notion")).not.toBeInTheDocument();
    // Bundle servers are still ordinary switches alongside it.
    expect(screen.getByTestId("mcp-toggle-xandr")).toBeInTheDocument();
  });

  it("never adds a remote server to the per-task selection (the row has no live control)", () => {
    const onChange = vi.fn();
    render(<McpServerPicker mode="task" servers={WITH_REMOTE} selection={[]} onChange={onChange} />);
    // The remote row is a static pill, so a click cannot mutate the selection.
    fireEvent.click(screen.getByTestId("mcp-remote-my-notion"));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("lists remote rows ahead of the toggleable roster", () => {
    render(<McpServerPicker mode="task" servers={WITH_REMOTE} selection={[]} onChange={() => {}} />);
    const rows = Array.from(document.querySelectorAll("[data-server]")).map((r) =>
      r.getAttribute("data-server"),
    );
    expect(rows).toEqual(["my-notion", "xandr", "magnite"]);
  });
});

// Multi-login remote connections (#988): a remote row stays the read-only
// "Connected" pill, but when the connection has labeled seats it gains the
// same Account select bundled rows have. Picking a label PINS that seat for
// the task ({ server, account }); picking "Default" REMOVES the entry — the
// run then mounts the user's default seat exactly as if nothing was chosen.
describe("McpServerPicker — remote connections with several logins (#988)", () => {
  const REMOTE_SEATS: McpServer[] = [
    ...SERVERS,
    {
      name: "gamma",
      description: "Gamma decks.",
      remote: true,
      accounts: ["personal", "work"],
      default_account: "work",
    },
  ];

  it("keeps the Connected pill and offers Default seat (naming the default) plus each label", () => {
    render(<McpServerPicker mode="task" servers={REMOTE_SEATS} selection={[]} onChange={() => {}} />);
    expect(screen.getByTestId("mcp-remote-gamma")).toHaveTextContent("Connected");
    expect(screen.queryByTestId("mcp-toggle-gamma")).not.toBeInTheDocument();
    const select = screen.getByTestId("mcp-account-gamma") as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "personal", "work"]);
    expect(select.options[0].textContent).toBe("Default seat (work)");
    expect(select.value).toBe("");
  });

  it("picking a label pins { server, account } for the task", () => {
    const onChange = vi.fn();
    render(
      <McpServerPicker
        mode="task"
        servers={REMOTE_SEATS}
        selection={[{ server: "xandr" }]}
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByTestId("mcp-account-gamma"), { target: { value: "personal" } });
    expect(onChange).toHaveBeenCalledWith([{ server: "xandr" }, { server: "gamma", account: "personal" }]);
  });

  it("re-picking a label replaces the pinned seat and reflects it in the select", () => {
    const onChange = vi.fn();
    render(
      <McpServerPicker
        mode="task"
        servers={REMOTE_SEATS}
        selection={[{ server: "gamma", account: "personal" }]}
        onChange={onChange}
      />,
    );
    expect((screen.getByTestId("mcp-account-gamma") as HTMLSelectElement).value).toBe("personal");
    fireEvent.change(screen.getByTestId("mcp-account-gamma"), { target: { value: "work" } });
    expect(onChange).toHaveBeenCalledWith([{ server: "gamma", account: "work" }]);
  });

  it("picking Default removes the entry rather than storing account: ''", () => {
    const onChange = vi.fn();
    render(
      <McpServerPicker
        mode="task"
        servers={REMOTE_SEATS}
        selection={[{ server: "gamma", account: "personal" }, { server: "xandr" }]}
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByTestId("mcp-account-gamma"), { target: { value: "" } });
    expect(onChange).toHaveBeenCalledWith([{ server: "xandr" }]);
  });

  it("a remote connection without labeled seats still has no account control", () => {
    render(
      <McpServerPicker
        mode="task"
        servers={[{ name: "solo-remote", remote: true, accounts: [], default_account: "" }]}
        selection={[]}
        onChange={() => {}}
      />,
    );
    expect(screen.getByTestId("mcp-remote-solo-remote")).toBeInTheDocument();
    expect(screen.queryByTestId("mcp-account-solo-remote")).not.toBeInTheDocument();
  });
});
