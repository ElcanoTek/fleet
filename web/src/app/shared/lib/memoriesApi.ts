// memoriesApi — the personal-memory record and its five requests, shared by
// chat's memory manager (chat/ui/useChatMemories.ts) and Settings → Memories
// (#2008). Each surface keeps its own wording: a failed request throws a
// MemoriesApiError whose message is responseErrorText(res, `fail`), the
// server's reason or "<fail> (NNN)", and whose status lets a caller treat a
// 401 its own way (Settings goes to sign-in).

import { responseErrorText } from "@/app/lib/responseError";

export type UserMemory = {
  id: string;
  content: string;
  source: "manual" | "chat" | string;
  kind?: string;
  origin?: string;
  conversation_id?: string;
  pinned?: boolean;
  valid_from?: number;
  valid_to?: number;
  learned_at?: number;
  retired_at?: number;
  retired_by?: string;
  created_at: number;
  updated_at: number;
};

export const MEMORY_KINDS = [
  "fact",
  "preference",
  "identity",
  "constraint",
  "context",
] as const;

export class MemoriesApiError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "MemoriesApiError";
    this.status = status;
  }
}

async function check(res: Response, fail: string): Promise<Response> {
  if (!res.ok) throw new MemoriesApiError(await responseErrorText(res, fail), res.status);
  return res;
}

const memoryUrl = (id: string) => `/api/memories/${encodeURIComponent(id)}`;

/** GET /api/memories — the caller's personal memories. */
export async function listMemories(fail: string): Promise<UserMemory[]> {
  const res = await check(await fetch("/api/memories", { cache: "no-store" }), fail);
  const data = (await res.json()) as { memories?: UserMemory[] };
  return data.memories ?? [];
}

/** POST /api/memories (new) or PATCH /api/memories/{id} (edit): content and kind. */
export async function saveMemory(
  input: { id?: string | null; content: string; kind: string },
  fail: string,
): Promise<void> {
  await check(
    await fetch(input.id ? memoryUrl(input.id) : "/api/memories", {
      method: input.id ? "PATCH" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: input.content, kind: input.kind }),
    }),
    fail,
  );
}

/**
 * PATCH /api/memories/{id} for the pin/unpin and retire/restore controls
 * (#515). The server treats absent fields as untouched, so each action sends
 * only its own flag.
 */
export async function patchMemory(
  id: string,
  patch: { pinned?: boolean; retired?: boolean },
  fail: string,
): Promise<void> {
  await check(
    await fetch(memoryUrl(id), {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }),
    fail,
  );
}

/** DELETE /api/memories/{id}. */
export async function deleteMemory(id: string, fail: string): Promise<void> {
  await check(await fetch(memoryUrl(id), { method: "DELETE" }), fail);
}

/**
 * POST /api/projects/{id}/memories {from_memory_id} — MOVES one personal
 * memory into a project's team learnings (Item D5). A move, not a copy: two
 * rows saying the same thing would be injected twice in every project chat.
 * The server enforces both halves (the row must be the caller's own personal
 * memory; the caller must be a member of the project).
 */
export async function promoteMemory(id: string, projectId: string, fail: string): Promise<void> {
  await check(
    await fetch(`/api/projects/${encodeURIComponent(projectId)}/memories`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ from_memory_id: id }),
    }),
    fail,
  );
}
