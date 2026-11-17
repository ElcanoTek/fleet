import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { taskRunLabel } from "@/app/orchestrator/taskDisplay";
import type { ShortcutTarget } from "./layout";

// What the Customize panel's "Add to sidebar" list offers: the person's chats
// (patrols and dashboards among them), teammates' shared and multiplayer
// chats, projects, recent background tasks and fleet's own pages. Each list is
// read when the picker opens; one that fails is simply left out.

export type PickerEntry = ShortcutTarget & {
  group: "patrols" | "chats" | "team" | "projects" | "tasks" | "pages";
  /** A second line: whose chat, which project, … */
  detail?: string;
};

type ConvRow = { id: string; title?: string; user_email?: string; multiplayer?: boolean; updated_at?: number };

async function json<T>(url: string): Promise<T | null> {
  try {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  }
}

export async function loadPickerEntries(): Promise<PickerEntry[]> {
  const [own, shared, projects, patrols, tasks] = await Promise.all([
    json<{ conversations?: ConvRow[] | null }>("/api/conversations"),
    json<{ conversations?: ConvRow[] | null }>("/api/conversations?scope=shared"),
    json<{ projects?: { id: string; name: string }[] | null }>("/api/projects"),
    json<{ patrols?: { conversation_id: string; title?: string; conversation_title?: string }[] | null }>("/api/patrols"),
    orchestratorApi.tasks("limit=50&offset=0").catch(() => null),
  ]);
  const out: PickerEntry[] = [];
  const patrolIds = new Set<string>();
  for (const p of patrols?.patrols ?? []) {
    if (!p.conversation_id || patrolIds.has(p.conversation_id)) continue;
    patrolIds.add(p.conversation_id);
    out.push({
      group: "patrols",
      kind: "chat",
      ref: p.conversation_id,
      title: p.conversation_title || p.title || "",
      detail: p.title && p.title !== p.conversation_title ? p.title : undefined,
      icon: "activity",
    });
  }
  for (const c of own?.conversations ?? []) {
    if (patrolIds.has(c.id)) continue;
    out.push({ group: "chats", kind: "chat", ref: c.id, title: c.title || "" });
  }
  for (const c of shared?.conversations ?? []) {
    if (patrolIds.has(c.id)) continue;
    out.push({
      group: "team",
      kind: "chat",
      ref: c.id,
      title: c.title || "",
      detail: c.user_email ? c.user_email.split("@")[0] : undefined,
    });
  }
  for (const p of projects?.projects ?? []) {
    out.push({ group: "projects", kind: "project", ref: p.id, title: p.name });
  }
  for (const t of tasks?.data ?? []) {
    out.push({ group: "tasks", kind: "task", ref: t.id, title: taskRunLabel(t) });
  }
  for (const ref of ["analytics", "help", "settings"]) {
    out.push({ group: "pages", kind: "page", ref });
  }
  return out;
}
