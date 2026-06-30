"use client";

import { useCallback } from "react";
import type { McpServer, MCPChoice } from "@/app/shared/lib/orchestratorApi";

// ONE McpServerPicker, reused in BOTH:
//   - the chat conversation toolbar (mode="conversation")
//   - the orchestrator task form    (mode="task")
//
// In BOTH modes it renders an enable/disable toggle per Optional server PLUS a
// per-server credential-account dropdown. The structure is identical across
// modes by design (the P7 gate asserts identical rendering): the only thing the
// mode changes is which copy of the selection is being edited and a couple of
// aria labels — never the set of controls shown. This is exactly the migration
// plan's "ONE shared component" rule: chat's per-conversation opt-in and the
// scheduled task's per-task selection reduce to the SAME { server, account }[]
// shape, so the SAME picker drives both.
//
// Selection shape: MCPChoice[] = [{ server, account? }]. A server is "enabled"
// when it appears in the list; its account is the chosen credential seat
// (account === "" / undefined means the default/shared seat).

export type McpServerPickerMode = "conversation" | "task";

export type McpServerPickerProps = {
  mode: McpServerPickerMode;
  servers: McpServer[];
  selection: MCPChoice[];
  onChange: (next: MCPChoice[]) => void;
  disabled?: boolean;
};

function findChoice(selection: MCPChoice[], server: string): MCPChoice | undefined {
  return selection.find((c) => c.server === server);
}

export function McpServerPicker({ mode, servers, selection, onChange, disabled }: McpServerPickerProps) {
  const toggleServer = useCallback(
    (server: string, enabled: boolean) => {
      if (enabled) {
        if (findChoice(selection, server)) return;
        onChange([...selection, { server }]);
      } else {
        onChange(selection.filter((c) => c.server !== server));
      }
    },
    [selection, onChange],
  );

  const setAccount = useCallback(
    (server: string, account: string) => {
      const next = selection.map((c) =>
        c.server === server ? { server, ...(account ? { account } : {}) } : c,
      );
      // If the server wasn't yet enabled, enabling it implicitly when an
      // account is chosen keeps the controls consistent.
      if (!findChoice(selection, server)) {
        next.push({ server, ...(account ? { account } : {}) });
      }
      onChange(next);
    },
    [selection, onChange],
  );

  return (
    <div
      className="mcp-server-picker"
      data-mode={mode}
      role="group"
      aria-label={mode === "task" ? "MCP servers for this task" : "MCP servers for this conversation"}
    >
      {servers.length === 0 ? (
        <p className="mcp-server-picker__empty">No optional MCP servers available.</p>
      ) : (
        <ul className="mcp-server-picker__list">
          {servers.map((server) => {
            // Per-user remote (hosted) MCP servers (#443/#466) are auto-applied to
            // ALL of the owner's scheduled runs by the run overlay — the per-task
            // selection doesn't gate them — so we render them as a read-only,
            // already-on "connected" row (no toggle to flip, no credential seat to
            // pick) rather than a control that would falsely imply per-task choice.
            if (server.remote) {
              return (
                <li
                  key={server.name}
                  className="mcp-server-picker__row mcp-server-picker__row--remote"
                  data-server={server.name}
                  data-remote="true"
                >
                  <label className="mcp-server-picker__toggle">
                    <input
                      type="checkbox"
                      checked
                      disabled
                      aria-label={`${server.name} (connected, auto-available)`}
                      data-testid={`mcp-remote-${server.name}`}
                    />
                    <span className="mcp-server-picker__name">{server.display_name || server.name}</span>
                    <span className="mcp-server-picker__count">connected · auto-available</span>
                  </label>
                  {server.description ? (
                    <p className="mcp-server-picker__desc">{server.description}</p>
                  ) : null}
                </li>
              );
            }
            const choice = findChoice(selection, server.name);
            const enabled = !!choice;
            const accounts = server.accounts ?? [];
            const accountInputId = `mcp-${mode}-${server.name}-account`;
            return (
              <li key={server.name} className="mcp-server-picker__row" data-server={server.name}>
                <label className="mcp-server-picker__toggle">
                  <input
                    type="checkbox"
                    checked={enabled}
                    disabled={disabled}
                    aria-label={`Enable ${server.name}`}
                    data-testid={`mcp-toggle-${server.name}`}
                    onChange={(e) => toggleServer(server.name, e.target.checked)}
                  />
                  <span className="mcp-server-picker__name">{server.name}</span>
                  {typeof server.tool_count === "number" ? (
                    <span className="mcp-server-picker__count">{server.tool_count} tools</span>
                  ) : null}
                </label>
                {server.description ? (
                  <p className="mcp-server-picker__desc">{server.description}</p>
                ) : null}
                <div className="mcp-server-picker__account">
                  <label htmlFor={accountInputId} className="mcp-server-picker__account-label">
                    Account
                  </label>
                  <select
                    id={accountInputId}
                    className="mcp-server-picker__account-select"
                    value={choice?.account ?? ""}
                    disabled={disabled || !enabled}
                    aria-label={`Credential account for ${server.name}`}
                    data-testid={`mcp-account-${server.name}`}
                    onChange={(e) => setAccount(server.name, e.target.value)}
                  >
                    <option value="">Default seat</option>
                    {accounts.map((acct) => (
                      <option key={acct} value={acct}>
                        {acct}
                      </option>
                    ))}
                  </select>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

export default McpServerPicker;
