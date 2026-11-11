import { t } from ".";

const MEMORY_KIND: Readonly<Record<string, string>> = {
  fact: t("fact"), preference: t("preference"), identity: t("identity"),
  constraint: t("constraint"), context: t("context"),
};
const BUDGET_SCOPE: Readonly<Record<string, string>> = {
  user: t("user"), key: t("key"), project: t("project"),
};
const BUDGET_WINDOW: Readonly<Record<string, string>> = {
  day: t("day"), week: t("week"), month: t("month"),
};
const WARNING_SEVERITY: Readonly<Record<string, string>> = {
  warning: t("warning"), error: t("error"), info: t("info"), critical: t("critical"),
};

const MESSAGE_ROLE: Readonly<Record<string, string>> = {
  user: t("user"), assistant: t("assistant"), tool: t("tool"), system: t("system"), developer: t("developer"),
};
const USAGE_SOURCE: Readonly<Record<string, string>> = { tasks: t("tasks"), chat: t("chat") };

function label(labels: Readonly<Record<string, string>>, value: string): string {
  return Object.hasOwn(labels, value) ? labels[value] : value;
}

// Display-only vocabularies: unknown/custom values and API identifiers survive.
export const memoryKindLabel = (value: string): string => label(MEMORY_KIND, value);
export const budgetScopeLabel = (value: string): string => label(BUDGET_SCOPE, value);
export const budgetWindowLabel = (value: string): string => label(BUDGET_WINDOW, value);
export const warningSeverityLabel = (value: string): string => label(WARNING_SEVERITY, value);

export const messageRoleLabel = (value: string): string => label(MESSAGE_ROLE, value);
export const usageSourceLabel = (value: string): string => label(USAGE_SOURCE, value);
