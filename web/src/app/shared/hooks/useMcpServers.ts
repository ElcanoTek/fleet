"use client";

import { useCallback } from "react";
import { orchestratorApi, type McpServer } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";

// useMcpServers loads the Optional-MCP catalog (server names, tool counts, and
// per-server credential-account names — never secret values). Feeds the shared
// <McpServerPicker> in the orchestrator task form. The same catalog shape is
// produced by chat's GET /api/mcp-servers for the conversation-toolbar instance
// of the picker, so the component is genuinely reused across both views.
//
// Built on the shared useCancellableFetch hook so the cancelled-ref guard and
// the lone setState-after-await live in one audited place — this hook no longer
// needs its own react-hooks/set-state-in-effect suppression.

export type UseMcpServers = {
  servers: McpServer[];
  loading: boolean;
  error: string | null;
  reload: () => Promise<void>;
};

export function useMcpServers(enabled = true): UseMcpServers {
  const { data, loading, error, reload } = useCancellableFetch(
    useCallback(async () => (await orchestratorApi.mcpServers()).servers ?? [], []),
    [],
    { enabled },
  );

  // Preserve the previous contract: servers is always an array (never null),
  // including before the first load and after a failed fetch.
  return { servers: data ?? [], loading, error, reload };
}
