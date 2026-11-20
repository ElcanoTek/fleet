import { t as tr } from "@/app/lib/i18n";
import { getLanguage } from "@/app/lib/i18n";
import { responseErrorText } from "@/app/lib/responseError";

// Wire types and small pure helpers for Settings → Admin → Chats. The types
// mirror internal/store's AdminChat, AdminChatSchedule, AdminNote,
// AdminPendingCard and AdminChatOverview as the /api/admin/chat* routes
// serialize them.

/** Why a chat needs attention (store.AdminAttention*). */
export type AttentionReason = "cards_waiting" | "turn_failed";

export type AdminChat = {
  id: string;
  title: string;
  owner_email: string;
  model: string;
  approval_mode: string;
  persona: string;
  lockdown: boolean;
  archived: boolean;
  multiplayer: boolean;
  participants?: string[];
  team_shared: boolean;
  created_at: number;
  updated_at: number;
  active_patrols: number;
  pending_approvals: number;
  queued_inputs: number;
  turn_running: boolean;
  /** When the oldest approval card still waiting was staged (0 / absent = none). */
  waiting_since?: number;
  /** running, completed, cancelled or error; absent for a chat with no turn. */
  last_turn_status?: string;
  /** A note is pinned to the chat; note_seen: someone in it pressed "Got it". */
  has_note?: boolean;
  note_seen?: boolean;
  attention?: AttentionReason[];
  /** Set on a chat in the recycle bin: when it was deleted, and purged. */
  deleted_at?: number;
  purges_at?: number;
};

export type AdminPatrol = {
  id: string;
  title: string;
  prompt: string;
  prompt_sha256: string;
  cron?: string;
  timezone: string;
  status: string;
  next_run_at: number;
  user_email: string;
  fire_in_flight: boolean;
  /** Bumped by every edit of the patrol's definition; a schedule edit is
   *  compare-and-swap on it. */
  def_version: number;
};

export type AdminAction = {
  id: number;
  created_at: number;
  actor: string;
  via: string;
  action: string;
  reason?: string;
  detail?: string;
};

export type TranscriptEntry = {
  id: number;
  created_at: number;
  role: string;
  type: string;
  kind?: string;
  sender?: string;
  tool?: string;
  text: string;
  is_error?: boolean;
  truncated?: boolean;
};

export type AdminRequest = {
  id: string;
  kind: string;
  state: string;
  result?: string;
  input_state?: string;
};

/** The note an administrator pinned to a chat (ADR-0117). */
export type AdminNote = {
  text: string;
  author: string;
  via: string;
  created_at: number;
  updated_at: number;
  seen_at?: number;
  seen_by?: string;
};

export type PendingCard = {
  id: string;
  tool: string;
  mcp_server?: string;
  for_email: string;
  created_at: number;
  expires_at?: number;
};

export type ChatDetailData = {
  chat: AdminChat;
  patrols: AdminPatrol[];
  actions: AdminAction[];
  pending_cards?: PendingCard[];
  note?: AdminNote | null;
};

export type ChatCounts = {
  chats: number;
  attention: number;
  running: number;
  patrols: number;
  noted: number;
  cards_waiting: number;
};

export type ChatPerson = ChatCounts & { email: string; last_active: number };

export type ChatOverview = ChatCounts & {
  archived: number;
  deleted: number;
  people: ChatPerson[];
};

// sendJSON is the page's one mutation path: a JSON body, the server's reason
// as the error on failure, the JSON answer (or null for a 204) on success.
export async function sendJSON<T>(url: string, method: string, body?: unknown): Promise<T | null> {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new Error(await responseErrorText(res, tr("The change failed")));
  if (res.status === 204) return null;
  return (await res.json()) as T;
}

export function chatUrl(chatId: string, sub = ""): string {
  return `/api/admin/chats/${encodeURIComponent(chatId)}${sub}`;
}

/** lineDiff is a small unified-style line diff for the patrol editor's preview. */
export function lineDiff(a: string, b: string): { kind: " " | "-" | "+"; text: string }[] {
  const al = a.split("\n");
  const bl = b.split("\n");
  if ((al.length + 1) * (bl.length + 1) > 4_000_000) return [];
  const n = al.length;
  const m = bl.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = al[i] === bl[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out: { kind: " " | "-" | "+"; text: string }[] = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && al[i] === bl[j]) {
      out.push({ kind: " ", text: al[i] });
      i++;
      j++;
    } else if (i < n && (j === m || lcs[i + 1][j] >= lcs[i][j + 1])) {
      out.push({ kind: "-", text: al[i] });
      i++;
    } else {
      out.push({ kind: "+", text: bl[j] });
      j++;
    }
  }
  return out;
}

/** formatRunTime renders a patrol run (Unix seconds) in the patrol's own timezone. */
export function formatRunTime(unix: number, timeZone: string): string {
  const opts: Intl.DateTimeFormatOptions = {
    weekday: "short",
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  };
  try {
    return new Date(unix * 1000).toLocaleString(getLanguage(), { ...opts, timeZone });
  } catch {
    return new Date(unix * 1000).toLocaleString(getLanguage(), opts);
  }
}

/** formatSpan renders a length of time in its largest whole unit: "3h", "2d". */
export function formatSpan(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return tr("{0}s", [s]);
  if (s < 3600) return tr("{0}m", [Math.floor(s / 60)]);
  if (s < 86400) return tr("{0}h", [Math.floor(s / 3600)]);
  return tr("{0}d", [Math.floor(s / 86400)]);
}

/** initials is the one or two letters an avatar shows for an email. */
export function initials(email: string): string {
  const name = email.split("@")[0] ?? "";
  const parts = name.split(/[._\-+]+/).filter(Boolean);
  const letters = parts.length >= 2 ? parts[0][0] + parts[1][0] : name.slice(0, 2);
  return (letters || "?").toUpperCase();
}

/** approvalModeLabel names a chat's approval mode the way the chat does. */
export function approvalModeLabel(mode: string): string {
  if (mode === "autonomous") return tr("Autonomous");
  if (mode === "auto") return tr("Auto-approve");
  if (mode === "notify") return tr("Notify");
  return tr("Manual");
}

/** attentionLabel says, in a few words, why a chat needs attention. */
export function attentionLabel(chat: AdminChat, nowSec: number): string[] {
  const out: string[] = [];
  for (const reason of chat.attention ?? []) {
    if (reason === "cards_waiting") {
      const waited = chat.waiting_since ? formatSpan(nowSec - chat.waiting_since) : "";
      out.push(
        chat.pending_approvals === 1
          ? waited
            ? tr("1 card waiting {0}", [waited])
            : tr("1 card waiting")
          : waited
            ? tr("{0} cards waiting {1}", [chat.pending_approvals, waited])
            : tr("{0} cards waiting", [chat.pending_approvals]),
      );
    } else if (reason === "turn_failed") {
      out.push(tr("last turn failed"));
    }
  }
  return out;
}

/** actionLabel names a recorded admin change (chat.admin_actions.action). */
export function actionLabel(action: string): string {
  switch (action) {
    case "note_set":
      return tr("Pinned a note");
    case "note_replace":
      return tr("Replaced the pinned note");
    case "note_clear":
      return tr("Removed the pinned note");
    case "notice":
      return tr("Added a notice");
    case "update":
      return tr("Changed settings");
    case "request_message":
      return tr("Sent a message");
    case "request_stop":
      return tr("Stopped the chat’s work");
    case "request_transfer":
      return tr("Transferred the chat");
    case "request_archive":
      return tr("Archived or restored the chat");
    case "request_delete":
      return tr("Moved the chat to the recycle bin");
    case "request_restore":
      return tr("Restored the chat from the recycle bin");
    case "request_purge":
      return tr("Deleted the chat permanently");
    case "patrol_prompt":
      return tr("Edited a patrol’s instructions");
    case "patrol_schedule":
      return tr("Changed a patrol’s schedule");
    case "patrol_pause":
      return tr("Paused a patrol");
    case "patrol_resume":
      return tr("Resumed a patrol");
    default:
      return action;
  }
}
