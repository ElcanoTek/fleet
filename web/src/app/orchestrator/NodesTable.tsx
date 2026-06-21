"use client";

import type { Node } from "@/app/shared/lib/orchestratorApi";
import { formatDate } from "@/app/shared/lib/format";

// NodesTable — the Registered Agents table. React port of moc dashboard.js
// renderNodes(). Filtering to "active" (idle|busy) is driven by the parent.

export type NodesTableProps = {
  nodes: Node[];
  activeOnly: boolean;
};

export function NodesTable({ nodes, activeOnly }: NodesTableProps) {
  const filtered = activeOnly
    ? nodes.filter((n) => n.status === "idle" || n.status === "busy")
    : nodes;

  return (
    <div className="table-wrapper">
      <table id="nodesTable">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Hostname</th>
            <th scope="col">OS</th>
            <th scope="col">Status</th>
            <th scope="col">Last Heartbeat</th>
          </tr>
        </thead>
        <tbody>
          {filtered.length === 0 ? (
            <tr>
              <td colSpan={5} className="table-empty">
                {activeOnly ? "No active agents found" : "No agents registered yet"}
              </td>
            </tr>
          ) : (
            filtered.map((node) => (
              <tr key={node.id}>
                <td>{node.name || "-"}</td>
                <td>{node.hostname || "-"}</td>
                <td>{node.os_type || "-"}</td>
                <td>
                  <span className={`status-badge status-${node.status ?? "unknown"}`}>
                    {node.status ?? "-"}
                  </span>
                </td>
                <td>{formatDate(node.last_heartbeat)}</td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}

export default NodesTable;
