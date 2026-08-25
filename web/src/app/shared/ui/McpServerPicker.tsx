"use client";

import { useCallback } from "react";
import type { McpServer, MCPChoice } from "@/app/shared/lib/orchestratorApi";

// ONE McpServerPicker, reused in BOTH:
//   - the chat conversation toolbar (mode="conversation")
//   - the orchestrator task form    (mode="task")
//
// In BOTH modes it renders an enable/disable switch per Optional server; an
// enabled server additionally shows its tool count and a credential-account
// dropdown (disabled rows show their one-line purpose instead — the deciding
// fact — per the New Task modal redesign). The structure is identical across
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
//
// Row order: connected remote servers first (read-only — except that a
// connection with several logins gets a seat picker, #988), then the
// toggleable roster in catalog order. Enabled rows tint in place — deliberately NOT
// sorted first, so a row never moves (or resizes) under the pointer when
// toggled.

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

  // Remote (hosted) connections are auto-available, so the ONLY per-task
  // choice they carry is which login (seat, #988) the run uses. Picking a
  // label pins it via { server, account }; picking "Default" REMOVES the
  // entry — `{ server, account: "" }` means the same thing server-side, and a
  // selection that only lists pinned seats keeps the auto-available
  // behaviour legible.
  const setRemoteAccount = useCallback(
    (server: string, account: string) => {
      const rest = selection.filter((c) => c.server !== server);
      onChange(account ? [...rest, { server, account }] : rest);
    },
    [selection, onChange],
  );

  // Stable regrouping (remote → rest); ties keep catalog order.
  const ordered = [...servers].sort((a, b) => Number(b.remote ?? false) - Number(a.remote ?? false));

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
          {ordered.map((server) => {
            // Per-user remote (hosted) MCP servers (#443/#466) are auto-applied to
            // ALL of the owner's scheduled runs by the run overlay — the per-task
            // selection doesn't gate them — so we render them as a read-only,
            // already-on "Connected" row (no switch to flip, no credential seat to
            // pick) rather than a control that would falsely imply per-task choice.
            if (server.remote) {
              const seats = server.accounts ?? [];
              const remoteChoice = findChoice(selection, server.name);
              const remoteAccountInputId = `mcp-${mode}-${server.name}-account`;
              return (
                <li
                  key={server.name}
                  className="mcp-server-picker__row mcp-server-picker__row--remote"
                  data-server={server.name}
                  data-remote="true"
                >
                  <span
                    className="mcp-server-picker__connected"
                    data-testid={`mcp-remote-${server.name}`}
                    aria-label={`${server.name} (connected, auto-available)`}
                  >
                    Connected
                  </span>
                  <span className="mcp-server-picker__name">{server.display_name || server.name}</span>
                  {server.description ? (
                    <span className="mcp-server-picker__desc">{server.description}</span>
                  ) : (
                    <span className="mcp-server-picker__desc">auto-available on every run</span>
                  )}
                  {seats.length > 0 ? (
                    <div className="mcp-server-picker__account">
                      <label htmlFor={remoteAccountInputId} className="mcp-server-picker__account-label">
                        Account
                      </label>
                      <select
                        id={remoteAccountInputId}
                        className="mcp-server-picker__account-select"
                        value={remoteChoice?.account ?? ""}
                        disabled={disabled}
                        aria-label={`Credential account for ${server.name}`}
                        data-testid={`mcp-account-${server.name}`}
                        onChange={(e) => setRemoteAccount(server.name, e.target.value)}
                      >
                        <option value="">
                          Default seat{server.default_account ? ` (${server.default_account})` : ""}
                        </option>
                        {seats.map((acct) => (
                          <option key={acct} value={acct}>
                            {acct}
                          </option>
                        ))}
                      </select>
                    </div>
                  ) : null}
                </li>
              );
            }
            const choice = findChoice(selection, server.name);
            const enabled = !!choice;
            const accounts = server.accounts ?? [];
            const accountInputId = `mcp-${mode}-${server.name}-account`;
            return (
              <li
                key={server.name}
                className={`mcp-server-picker__row${enabled ? " mcp-server-picker__row--enabled" : ""}`}
                data-server={server.name}
              >
                <label className="mcp-server-picker__toggle">
                  <input
                    type="checkbox"
                    className="ui-switch"
                    checked={enabled}
                    disabled={disabled}
                    aria-label={`Enable ${server.name}`}
                    data-testid={`mcp-toggle-${server.name}`}
                    onChange={(e) => toggleServer(server.name, e.target.checked)}
                  />
                  <span className="mcp-server-picker__name">{server.name}</span>
                  {enabled && typeof server.tool_count === "number" ? (
                    <span className="mcp-server-picker__count">
                      {server.tool_count} {server.tool_count === 1 ? "tool" : "tools"}
                    </span>
                  ) : null}
                  {!enabled && server.description ? (
                    <span className="mcp-server-picker__desc">{server.description}</span>
                  ) : null}
                </label>
                {enabled ? (
                  <div className="mcp-server-picker__account">
                    <label htmlFor={accountInputId} className="mcp-server-picker__account-label">
                      Account
                    </label>
                    <select
                      id={accountInputId}
                      className="mcp-server-picker__account-select"
                      value={choice?.account ?? ""}
                      disabled={disabled}
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
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

export default McpServerPicker;
