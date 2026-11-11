import { t } from "./index";
import type { PillField } from "@/app/chat/ui/protocolPills";
import type { PromptLibraryItem, TaskTemplate } from "@/app/shared/lib/orchestratorApi";

export function localizeField(field: PillField): PillField {
  return {
    ...field,
    label: t(field.label),
    ...(field.placeholder ? { placeholder: t(field.placeholder) } : {}),
    ...(field.hint ? { hint: t(field.hint) } : {}),
    ...(field.options ? { optionLabels: Object.fromEntries(field.options.map((option) => [option, t(option)])) } : {}),
    // Text defaults are draft prose. Select values, dates, numbers and boolean
    // defaults retain their exact machine meaning.
    ...((field.type === "text" || field.type === "textarea") && typeof field.default === "string"
      ? { default: t(field.default) } : {}),
  };
}

export function localizeConnector<T extends { remote?: boolean; display_name?: string; description?: string; setup_hint?: string }>(entry: T): T {
  if (entry.remote) return entry; // User-created connection names and metadata.
  return {
    ...entry,
    ...(entry.display_name ? { display_name: t(entry.display_name) } : {}),
    ...(entry.description ? { description: t(entry.description) } : {}),
    ...(entry.setup_hint ? { setup_hint: t(entry.setup_hint) } : {}),
  };
}

export function localizeTaskTemplate(template: TaskTemplate): TaskTemplate {
  return {
    ...template,
    name: t(template.name),
    ...(template.description ? { description: t(template.description) } : {}),
    task: {
      ...template.task,
      ...(template.task.title ? { title: t(template.task.title) } : {}),
      ...(template.task.description ? { description: t(template.task.description) } : {}),
      ...(template.task.prompt ? { prompt: t(template.task.prompt) } : {}),
    },
  };
}

export function localizePrompt(prompt: PromptLibraryItem): PromptLibraryItem {
  if (prompt.source !== "git") return prompt;
  return {
    ...prompt,
    name: t(prompt.name),
    ...(prompt.description ? { description: t(prompt.description) } : {}),
    ...(prompt.fields ? { fields: prompt.fields.map(localizeField) } : {}),
    ...(prompt.prompt_template ? { prompt_template: t(prompt.prompt_template) } : {}),
    // Content is the versioned source file, used verbatim for export/backup.
  };
}

export function skillDescription(skill: { source?: string; description: string }): string {
  return ["bundle", "builtin", "plugin"].includes(skill.source ?? "") ? t(skill.description) : skill.description;
}
