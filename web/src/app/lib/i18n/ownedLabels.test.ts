import { afterEach, expect, it, vi } from "vitest";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
});

for (const language of ["es", "pt"] as const) {
  it(`${language}: translates only owned enum labels and retains unknown values`, async () => {
    window.history.replaceState({}, "", `/?lang=${language}`);
    vi.resetModules();
    const { memoryKindLabel, budgetScopeLabel, budgetWindowLabel, warningSeverityLabel, messageRoleLabel, usageSourceLabel } = await import("./ownedLabels");
    const { runStateLabel } = await import("@/app/shared/ui/StatusChip");
    expect(memoryKindLabel("preference")).toBe(language === "es" ? "preferencia" : "preferência");
    expect(memoryKindLabel("fact")).toBe(language === "es" ? "hecho" : "facto");
    expect(budgetScopeLabel("user")).toBe(language === "es" ? "usuario" : "utilizador");
    expect(budgetWindowLabel("month")).toBe(language === "es" ? "mes" : "mês");
    expect(warningSeverityLabel("warning")).toBe(language === "es" ? "advertencia" : "aviso");
    expect(messageRoleLabel("assistant")).toBe(language === "es" ? "asistente" : "assistente");
    expect(usageSourceLabel("tasks")).toBe(language === "es" ? "tareas" : "tarefas");
    expect(runStateLabel("running")).not.toBe("running");
    for (const label of [memoryKindLabel, budgetScopeLabel, budgetWindowLabel, warningSeverityLabel, messageRoleLabel, usageSourceLabel, runStateLabel]) {
      expect(label("Customer custom value {0}")).toBe("Customer custom value {0}");
      expect(label("constructor")).toBe("constructor");
    }
    // A valid word in another enum must not be translated outside its field.
    expect(budgetWindowLabel("preference")).toBe("preference");
  });
}
