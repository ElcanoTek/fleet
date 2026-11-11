import { afterEach, expect, it, vi } from "vitest";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
});

it.each([
  { language: "en", starting: "starting…", writing: "writing…", thinking: "thinking…", agent: "Sub-agent", tools: "2 tools", house: "Fleet Tools", blocked: "blocked" },
  { language: "es", starting: "iniciando…", writing: "escribiendo…", thinking: "pensando…", agent: "Subagente", tools: "2 herramientas", house: "Herramientas de Fleet", blocked: "bloqueado" },
  { language: "pt", starting: "a iniciar…", writing: "a escrever…", thinking: "a pensar…", agent: "Subagente", tools: "2 ferramentas", house: "Ferramentas do Fleet", blocked: "bloqueado" },
])("localizes $language activity and connector labels while preserving tool data", async (copy) => {
  window.history.replaceState({}, "", `/?lang=${copy.language}`);
  vi.resetModules();
  const { applySubagentProgress, liveSubagentLabel } = await import("@/app/chat/ui/history");
  const { customTransportBadge } = await import("@/app/settings/connections/catalog");
  const { orderedCategories } = await import("@/app/chat/ui/toolsPicker");
  const { RunStatusChip, runStateLabel } = await import("@/app/shared/ui/StatusChip");
  const started = applySubagentProgress(undefined, { phase: "started" });
  expect(started.current).toBe(copy.starting);
  expect(liveSubagentLabel({ id: "call-1", name: "spawn_subagent", input: "{}", state: "pending", subagent: started }))
    .toBe(`${copy.agent} · ${copy.starting}`);
  expect(applySubagentProgress(started, { phase: "text" }).current).toBe(copy.writing);
  expect(applySubagentProgress(started, { phase: "thinking" }).current).toBe(copy.thinking);
  const detail = "My report {0} <tag>";
  const tool = applySubagentProgress(started, { phase: "tool", tool: "mcp_example_read", detail });
  expect(tool.current).toBe(`mcp_example_read · ${detail}`);
  expect(tool.toolsUsed).toEqual(["mcp_example_read"]);
  expect(customTransportBadge("http_tools", 2)).toContain(copy.tools);
  expect(orderedCategories([{ id: "house", name: "Fleet Tools" }])[0])
    .toEqual({ id: "house", name: copy.house });
  for (const children of [undefined, "blocked"]) {
    const badge = RunStatusChip({ state: "blocked", children });
    expect(badge.props.children).toBe(copy.blocked);
    expect(badge.props.className).toContain("status-blocked");
  }
  expect(runStateLabel("Save")).toBe("Save");
  expect(RunStatusChip({ state: "blocked", children: "Customer status" }).props.children)
    .toBe("Customer status");
});
