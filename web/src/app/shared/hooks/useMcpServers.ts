"use client";

import { useCallback, useEffect, useState } from "react";
import { orchestratorApi, type McpServer } from "@/app/shared/lib/orchestratorApi";

// useMcpServers loads the Optional-MCP catalog (server names, tool counts, and
// per-server credential-account names — never secret values). Feeds the shared
// <McpServerPicker> in the orchestrator task form. The same catalog shape is
// produced by chat's GET /api/mcp-servers for the conversation-toolbar instance
// of the picker, so the component is genuinely reused across both views.

export type UseMcpServers = {
  servers: McpServer[];
  loading: boolean;
  error: string | null;
  reload: () => Promise<void>;
};

export function useMcpServers(enabled = true): UseMcpServers {
  const [servers, setServers] = useState<McpServer[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await orchestratorApi.mcpServers();
      setServers(res.servers ?? []);
    } catch (err) {
      setError((err as Error).message);
      setServers([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- reload sets a load flag then fetches
    if (enabled) void reload();
  }, [enabled, reload]);

  return { servers, loading, error, reload };
}
