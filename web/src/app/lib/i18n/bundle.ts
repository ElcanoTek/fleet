import { bundleText } from "./index";
import type { PillField } from "@/app/chat/ui/protocolPills";
import type { PromptLibraryItem, TaskTemplate } from "@/app/shared/lib/orchestratorApi";

export function localizeField(field: PillField): PillField {
  return {
    ...field,
    label: bundleText(field.label),
    ...(field.placeholder ? { placeholder: bundleText(field.placeholder) } : {}),
    ...(field.hint ? { hint: bundleText(field.hint) } : {}),
    ...(field.options ? { optionLabels: Object.fromEntries(field.options.map((option) => [option, bundleText(option)])) } : {}),
    // Text defaults are draft prose. Select values, dates, numbers and boolean
    // defaults retain their exact machine meaning.
    ...((field.type === "text" || field.type === "textarea") && typeof field.default === "string"
      ? { default: bundleText(field.default) } : {}),
  };
}

export function localizeConnector<T extends { remote?: boolean; catalog_copy?: boolean; display_name?: string; description?: string; setup_hint?: string }>(entry: T): T {
  // A hosted connection's name and metadata are the user's own, except when
  // the server took its label and sentence from the bundle's directory entry
  // (catalog_copy): those are bundle copy and translate like any connector's.
  if (entry.remote && !entry.catalog_copy) return entry;
  return {
    ...entry,
    ...(entry.display_name ? { display_name: bundleText(entry.display_name) } : {}),
    ...(entry.description ? { description: bundleText(entry.description) } : {}),
    ...(entry.setup_hint ? { setup_hint: bundleText(entry.setup_hint) } : {}),
  };
}

export function localizeTaskTemplate(template: TaskTemplate): TaskTemplate {
  return {
    ...template,
    name: bundleText(template.name),
    ...(template.description ? { description: bundleText(template.description) } : {}),
    task: {
      ...template.task,
      ...(template.task.title ? { title: bundleText(template.task.title) } : {}),
      ...(template.task.description ? { description: bundleText(template.task.description) } : {}),
      ...(template.task.prompt ? { prompt: bundleText(template.task.prompt) } : {}),
    },
  };
}

export function localizePrompt(prompt: PromptLibraryItem): PromptLibraryItem {
  if (prompt.source !== "git") return prompt;
  return {
    ...prompt,
    name: bundleText(prompt.name),
    ...(prompt.description ? { description: bundleText(prompt.description) } : {}),
    ...(prompt.fields ? { fields: prompt.fields.map(localizeField) } : {}),
    ...(prompt.prompt_template ? { prompt_template: bundleText(prompt.prompt_template) } : {}),
    // Content is the versioned source file, used verbatim for export/backup.
  };
}

export function skillDescription(skill: { source?: string; description: string }): string {
  return ["bundle", "builtin", "plugin"].includes(skill.source ?? "") ? bundleText(skill.description) : skill.description;
}
