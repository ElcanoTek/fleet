// homeChat — the person's Home chat (docs/HOME-CHAT.md): the one chat pinned
// at the top of their chat list and the chat a fresh visit lands on. The server owns
// the choice (GET/PUT/DELETE /api/me/home, and the "home" field of the active
// GET /api/conversations); this module is its wire shape, the calls, and the
// boot rule that decides when Home wins over everything else /chat could open.

import { readPref, removePref, writePref } from "@/app/shared/lib/storage";

/** How the person reaches their Home: their own chat and a Multiplayer chat
 *  open as a normal chat; a chat shared with their team opens read-only. */
export type HomeAccess = "owner" | "multiplayer" | "team_view";

export type HomeConversation = {
  conversation_id: string;
  access: HomeAccess;
  title: string;
  owner_email: string;
};

const ACCESS: ReadonlySet<string> = new Set(["owner", "multiplayer", "team_view"]);

/** The server's "home" field, or null for anything that is not one. */
export function parseHome(raw: unknown): HomeConversation | null {
  if (!raw || typeof raw !== "object") return null;
  const h = raw as Record<string, unknown>;
  if (typeof h.conversation_id !== "string" || !h.conversation_id) return null;
  if (typeof h.access !== "string" || !ACCESS.has(h.access)) return null;
  return {
    conversation_id: h.conversation_id,
    access: h.access as HomeAccess,
    title: typeof h.title === "string" ? h.title : "",
    owner_email: typeof h.owner_email === "string" ? h.owner_email : "",
  };
}

// Set once a tab has landed on Home, so only the first visit in a tab does.
// A reload in the same tab keeps today's behavior (the most recent chat)
// rather than yanking the person off the chat they were in.
const LANDED_KEY = "home-landed";

/** Forgets that this tab has landed, so the next visit lands on Home again.
 *  Signing out calls it: the next person (or the same one) signing in to this
 *  tab starts fresh. */
export function forgetHomeLanding(): void {
  removePref(LANDED_KEY, "session");
}

/** True the first time a tab asks; false after. Storage that throws counts as
 *  a first visit every time, which is the feature's default rather than the
 *  absence of it. */
export function claimHomeLanding(): boolean {
  if (readPref(LANDED_KEY, "session") === "1") return false;
  writePref(LANDED_KEY, "1", "session");
  return true;
}

export type BootTarget =
  | { kind: "home"; home: HomeConversation }
  | { kind: "conversation"; id: string }
  | { kind: "none" };

/** What a cold /chat boot opens. A link always wins (the person was just sent
 *  somewhere), then Home on the tab's first visit, then the most recent chat
 *  as before. */
export function bootTarget(input: {
  deepLinkConvId: string | null;
  // A ?team= link of any outcome: an owner's opens like ?c=, the rest show
  // the viewer or a notice, and Home must not replace either.
  teamLink: { ownerConvId: string | null } | null;
  newChatLink: boolean;
  home: HomeConversation | null;
  // Asked only when nothing above it won, so a visit that opened a link does
  // not use up the tab's landing.
  landOnHome: () => boolean;
  mostRecentConvId: string | null;
}): BootTarget {
  if (input.deepLinkConvId) return { kind: "conversation", id: input.deepLinkConvId };
  if (input.teamLink?.ownerConvId) return { kind: "conversation", id: input.teamLink.ownerConvId };
  if (input.newChatLink) return { kind: "none" };
  if (!input.teamLink && input.home && input.landOnHome()) return { kind: "home", home: input.home };
  return input.mostRecentConvId ? { kind: "conversation", id: input.mostRecentConvId } : { kind: "none" };
}

async function homeRequest(init: RequestInit): Promise<HomeConversation | null> {
  const res = await fetch("/api/me/home", { cache: "no-store", ...init });
  if (!res.ok) throw new Error(`home: HTTP ${res.status}`);
  const data = (await res.json()) as { home?: unknown };
  return parseHome(data.home);
}

/** Makes conversationId Home; resolves to Home as the server now reports it. */
export function saveHome(conversationId: string): Promise<HomeConversation | null> {
  return homeRequest({
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ conversation_id: conversationId }),
  });
}

/** Removes Home; the chat itself is left as it is. */
export async function removeHome(): Promise<void> {
  await homeRequest({ method: "DELETE" });
}
