// chatSessionStore — a tiny module-scoped singleton that lets the chat
// experience survive a route unmount.
//
// /chat and /settings are separate Next.js App Router route segments under one
// root layout, so navigating between them FULLY unmounts ChatExperience. All of
// its in-memory conversation state (the messagesByConv cache, the conversation
// list, the active conversation id) would otherwise be discarded and rebuilt
// from a fan of network round-trips on every return — which is exactly the
// "returning to chat from settings is slow" bug.
//
// Hoisting that cross-navigation slice of state OUT of the component and into
// module scope keeps it alive across the unmount. On return, ChatExperience
// rehydrates from this snapshot and paints instantly, then revalidates in the
// background and reconciles (stale-while-revalidate). A genuine cold start (no
// snapshot yet, or a hard reload that spins up a fresh JS context) is unchanged:
// the snapshot is null, so the full blocking spinner + serial bootstrap runs.
//
// Deliberately NOT a heavy state library: exactly one ChatExperience is ever
// mounted at a time (the route unmounts the old one before mounting the new),
// so there is no cross-component reactivity to coordinate — persistence across
// unmount is the only requirement, and a single module variable delivers it.
// The snapshot holds references to the very arrays/objects already in React
// state, so keeping it around costs no extra memory beyond one pointer each.

import type { Message } from "./history";
import type { SkillInfo } from "./skillSlash";
import type { ConversationSummary, ServerConfig } from "./chat-experience";

export type ChatSessionSnapshot = {
  // Per-conversation message cache (keyed by conversation id / PENDING sentinel).
  messagesByConv: Record<string, Message[]>;
  // Sidebar conversation lists.
  conversations: ConversationSummary[];
  archivedConversations: ConversationSummary[];
  // The conversation the user was viewing (null = empty new-chat view).
  activeConversationId: string | null;
  // Nice-to-haves that are cheap to keep so the composer/header don't flicker
  // back to defaults on return while the background revalidation is in flight.
  personas: string[];
  selectedPersona: string;
  selectedModel: string;
  skills: SkillInfo[];
  serverConfig: ServerConfig;
  userEmail: string;
};

// Lives for the life of the page/tab. A hard reload starts it null again.
let snapshot: ChatSessionSnapshot | null = null;

// Read the current snapshot (null = cold start, no cached session).
export function readChatSession(): ChatSessionSnapshot | null {
  return snapshot;
}

// Overwrite the snapshot with the latest state. Called on every relevant state
// change once the initial bootstrap has completed, so the snapshot the next
// mount rehydrates from is always current.
export function writeChatSession(next: ChatSessionSnapshot): void {
  snapshot = next;
}

// Drop the cached session, forcing the next mount back onto the cold-start
// path. Not used by the happy path today, but exported so a future
// sign-out / hard-reset flow can invalidate the cache explicitly.
export function clearChatSession(): void {
  snapshot = null;
}
