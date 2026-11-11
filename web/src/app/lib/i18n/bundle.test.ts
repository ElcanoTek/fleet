import { afterEach, expect, it } from "vitest";
import { setBundleLocalizations } from "./index";
import { localizeConnector, localizeField, localizePrompt, localizeTaskTemplate, skillDescription } from "./bundle";

afterEach(() => setBundleLocalizations({ en: {} }));

it("translates bundle presentation while preserving form values and connector identifiers", () => {
  setBundleLocalizations({ en: { Channel: "Canal", "Choose channel": "Elige un canal", "Demo tool": "Herramienta de ejemplo" } });
  const field = { key: "channel", label: "Choose channel", type: "select" as const, options: ["Channel"], default: "Channel" };
  const localized = localizeField(field);
  expect(localized.label).toBe("Elige un canal");
  expect(localized.optionLabels).toEqual({ Channel: "Canal" });
  expect(localized.options).toEqual(["Channel"]);
  expect(localized.default).toBe("Channel");
  expect(field.label).toBe("Choose channel");
  const connector = { name: "demo_tool", display_name: "Demo tool", tools: ["read_data"], category: "data-storage", enabled: false };
  expect(localizeConnector(connector)).toEqual({ ...connector, display_name: "Herramienta de ejemplo" });
  const custom = { ...connector, remote: true };
  expect(localizeConnector(custom)).toBe(custom);
});

it("keeps user content and raw Git prompt files unchanged, including colliding source text", () => {
  setBundleLocalizations({ en: { "Example prompt": "Instrucción de ejemplo", "Inspect {topic}": "Examina {topic}" } });
  const prompt = { id: "git:example.yaml", name: "Example prompt", content: "Example prompt", source: "git" as const, read_only: true, owned_by_caller: false, visibility: "workspace" as const, prompt_template: "Inspect {topic}" };
  expect(localizePrompt(prompt)).toMatchObject({ name: "Instrucción de ejemplo", content: prompt.content, prompt_template: "Examina {topic}", id: prompt.id });
  const authored = { ...prompt, source: "workspace" as const };
  expect(localizePrompt(authored)).toBe(authored);
  expect(skillDescription({ source: "user", description: "Example prompt" })).toBe("Example prompt");
  expect(skillDescription({ source: "bundle", description: "Example prompt" })).toBe("Instrucción de ejemplo");
  const task = { name: "Example prompt", variables: ["topic"], task: { prompt: "Inspect {topic}", persona: "analyst", recurrence: "0 9 * * 1" } };
  expect(localizeTaskTemplate(task)).toEqual({ ...task, name: "Instrucción de ejemplo", task: { ...task.task, prompt: "Examina {topic}" } });
});
