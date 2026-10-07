// Shared types and fetch helpers for team sharing with files (docs/TEAM-SHARING.md).
//
// One module so the share dialog, the project home, the sidebar confirms and
// the teammate viewer all speak the same wire shapes. The Go handlers are the
// source of truth; every helper here is a thin, typed wrapper over one route.

import type { ChatToastInput } from "./ChatToasts";

/** One output: a workspace file the agent presented in a reply (a file chip). */
export type OutputFile = {
  path: string;
  name: string;
  size: number;
  modified_at: number;
  /**
   * The file's revision (modification time in nanoseconds and size): changes
   * whenever its bytes are rewritten, even within the same second. Optional
   * so an older server still type-checks.
   */
  rev?: string;
  /** Not excluded by the owner. Independent of whether the chat itself is shared. */
  shared: boolean;
};

export type ConversationOutputs = {
  outputs: OutputFile[];
  total: number;
  shared_count: number;
  team_visible: boolean;
  /**
   * True when the chat references more distinct files than output discovery
   * considers (the server keeps the most recent 500): older references are
   * not in `outputs`. Optional so an older server still type-checks.
   */
  truncated?: boolean;
};

export type ShareWithTeamResult = {
  team_visible: boolean;
  /** Shared outputs after the change; for an unshare, how many just stopped being shared. */
  shared_files: number;
  total_files: number;
};

/** The viewer's own most recent branch of a teammate's chat. */
export type ViewerBranch = {
  conversation_id: string;
  branched_at: number;
  /** The original changed after the branch was made. */
  changed_since: boolean;
};

export type BranchOrigin = {
  source_conversation_id: string;
  source_owner_email: string;
  source_title: string;
  branched_at: number;
  copied_files: { path: string; name: string; size: number }[];
  withheld_files: string[];
  /**
   * The transcript referenced more files than withheld_files records (it is
   * bounded): a reference in neither list is then treated as withheld unless
   * the branch's own workspace has the file. Absent on older origins.
   */
  withheld_truncated?: boolean;
  source_still_shared: boolean;
};

export type TeamLinkStatus = {
  status: "owner" | "open" | "not_on_team" | "not_shared";
  team_id?: string;
  viewer_email: string;
  project?: { id: string; name: string };
};

export type SourcesFile = {
  path: string;
  name: string;
  size: number;
  modified_at: number;
  shared: boolean;
  /** Presented in a reply. Non-outputs are download-only and never counted. */
  output: boolean;
  /** Copied in when the caller branched a teammate's chat. */
  your_copy: boolean;
};

export type SourcesGroup = {
  conversation_id: string;
  title: string;
  owner_email: string;
  mine: boolean;
  team_visible: boolean;
  is_branch: boolean;
  branched_at?: number;
  last_active_at: number;
  file_count: number;
  shared_count: number;
  files: SourcesFile[];
};

export type ProjectSources = {
  groups: SourcesGroup[];
  truncated: boolean;
};

export type ProjectMyState = {
  kept_personal: boolean;
  has_shared_chat: boolean;
  sources_open: Record<string, boolean>;
};

const enc = encodeURIComponent;

/** /api/conversations/<id>/team-files/<path> — a teammate's download of a shared output. */
export function teamFileUrl(conversationId: string, path: string): string {
  const segments = path
    .split("/")
    .filter((s) => s.length > 0)
    .map((s) => enc(s))
    .join("/");
  return `/api/conversations/${enc(conversationId)}/team-files/${segments}`;
}

/** /api/conversations/<id>/workspace/<path> — the owner's own download. */
export function ownerFileUrl(conversationId: string, path: string): string {
  const segments = path
    .split("/")
    .filter((s) => s.length > 0)
    .map((s) => enc(s))
    .join("/");
  return `/api/conversations/${enc(conversationId)}/workspace/${segments}`;
}

/** The team link a teammate opens: lands on the read-only view. */
export function teamLinkUrl(conversationId: string, origin?: string): string {
  const base =
    origin ?? (typeof window !== "undefined" ? window.location.origin : "");
  return `${base}/chat?team=${enc(conversationId)}`;
}

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(text.trim() || `request failed (${res.status})`);
  }
  return (await res.json()) as T;
}

export async function fetchConversationOutputs(
  conversationId: string,
): Promise<ConversationOutputs> {
  return json(
    await fetch(`/api/conversations/${enc(conversationId)}/outputs`, {
      cache: "no-store",
    }),
  );
}

export async function setOutputShared(
  conversationId: string,
  path: string,
  shared: boolean,
): Promise<ConversationOutputs> {
  return json(
    await fetch(`/api/conversations/${enc(conversationId)}/outputs/share`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path, shared }),
    }),
  );
}

/**
 * The share dialog's checklist as the owner saw it: every path it LISTED and
 * the ones they unchecked. The server applies it to exactly the listed paths —
 * an exclusion for a file the checklist did not show (missing on disk right
 * now, past the listing bound) is left alone, so re-sharing with everything
 * checked can never re-expose it.
 */
export type OutputChecklist = {
  listedPaths: string[];
  unsharedPaths: string[];
};

/**
 * Share or stop sharing a chat with the team. `checklist`, when given with
 * visible=true, is applied to the paths it listed; omitted, the owner's
 * earlier choices are kept.
 */
export async function shareChatWithTeam(
  conversationId: string,
  visible: boolean,
  checklist?: OutputChecklist,
): Promise<ShareWithTeamResult> {
  const body: {
    visible: boolean;
    unshared_paths?: string[];
    listed_paths?: string[];
  } = { visible };
  if (visible && checklist) {
    body.unshared_paths = checklist.unsharedPaths;
    body.listed_paths = checklist.listedPaths;
  }
  return json(
    await fetch(`/api/conversations/${enc(conversationId)}/share-with-team`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),
  );
}

export async function fetchTeamLinkStatus(
  conversationId: string,
): Promise<TeamLinkStatus> {
  return json(
    await fetch(`/api/conversations/${enc(conversationId)}/team-link`, {
      cache: "no-store",
    }),
  );
}

// focus (optional) is the chat "Manage in Sources" is opening: the server
// lists its group even past the per-half group cap, when the caller may see it.
export async function fetchProjectSources(
  projectId: string,
  focus?: string | null,
): Promise<ProjectSources> {
  const query = focus ? `?focus=${enc(focus)}` : "";
  return json(
    await fetch(`/api/projects/${enc(projectId)}/files${query}`, {
      cache: "no-store",
    }),
  );
}

export async function fetchProjectMyState(
  projectId: string,
): Promise<ProjectMyState> {
  return json(
    await fetch(`/api/projects/${enc(projectId)}/my-state`, {
      cache: "no-store",
    }),
  );
}

export async function updateProjectMyState(
  projectId: string,
  patch: Partial<Pick<ProjectMyState, "kept_personal" | "sources_open">>,
): Promise<ProjectMyState> {
  return json(
    await fetch(`/api/projects/${enc(projectId)}/my-state`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }),
  );
}

/** "1 chat" / "2 chats" — the count-plus-noun every confirm and toast uses. */
export function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** "1 file" / "3 files". */
export function filesLabel(n: number): string {
  return plural(n, "file", "files");
}

/** "Oct 5" — a row's or a file's day (unix seconds); "" when unknown. */
export function formatDay(unixSeconds: number): string {
  if (!unixSeconds) return "";
  try {
    return new Date(unixSeconds * 1000).toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  } catch {
    return "";
  }
}

/**
 * The one confirmation sentence every file-sharing path ends with (B5):
 * "'Q3 recap' is shared with Elcano, with 2 files." — or, with no outputs
 * yet, that files it creates will be shared too.
 */
function sharedToastText(
  title: string,
  team: string,
  sharedFiles: number,
): string {
  const head = `“${title}” is shared with ${team}`;
  if (sharedFiles > 0) return `${head}, with ${filesLabel(sharedFiles)}.`;
  return `${head}. Files it creates will be shared too.`;
}

/**
 * The whole B5 toast, so every share path (dialog, row pill, card, move
 * toast) says the same thing: sharedToastText plus "Manage", which opens
 * Sources at this chat's group. Manage only appears when there are files to
 * manage — Sources hides a chat with none, so the button would land nowhere.
 */
export function sharedToast(
  title: string,
  team: string,
  sharedFiles: number,
  onManage?: () => void,
): ChatToastInput {
  return {
    message: sharedToastText(title, team, sharedFiles),
    action:
      onManage && sharedFiles > 0 ? { label: "Manage", onClick: onManage } : undefined,
  };
}

/**
 * The owner's first name for sentences ("Read-only. This is Sam’s chat."):
 * the email's local part up to the first `.`, `_`, `-` or `+`, capitalised.
 * Chips and banners that identify the owner show the full email instead.
 */
export function ownerFirstName(email: string): string {
  const at = email.indexOf("@");
  const local = at > 0 ? email.slice(0, at) : email;
  const first = local.split(/[._+-]/).find((s) => s.length > 0) ?? local;
  return first ? first[0].toUpperCase() + first.slice(1) : email;
}
