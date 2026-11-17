import { afterEach, expect, it, vi } from "vitest";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
});

it.each([
  { language: "en", tools: "2 tools", house: "Fleet Tools", blocked: "blocked" },
  { language: "es", tools: "2 herramientas", house: "Herramientas de Fleet", blocked: "bloqueado" },
  { language: "pt", tools: "2 ferramentas", house: "Ferramentas do Fleet", blocked: "bloqueado" },
])("localizes $language connector and status labels", async (copy) => {
  window.history.replaceState({}, "", `/?lang=${copy.language}`);
  vi.resetModules();
  const { customTransportBadge } = await import("@/app/settings/connections/catalog");
  const { orderedCategories } = await import("@/app/chat/ui/toolsPicker");
  const { RunStatusChip, runStateLabel } = await import("@/app/shared/ui/StatusChip");
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
