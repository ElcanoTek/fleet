import { navigate } from "@/app/lib/navigation";
import type { ConversationSummary } from "@/app/chat/ui/chat-experience";

// Handing an action over to Chat (docs/SIDEBAR.md).
//
// The same sidebar is on every page, and so is each row's menu. Some of what
// a menu offers is a dialog or a confirmation that Chat owns — Share,
// Download, Transfer, a chat's patrols, the delete confirm that names the
// teammates who lose access. Another page does not rebuild those: it notes
// what was asked, goes to Chat, and Chat does it there, with the same code a
// click in Chat's own sidebar runs.
//
// The note is one module variable, taken once. Moving between pages is a
// client-side navigation (lib/navigation.tsx), so it survives the trip; a
// full page load starts it empty, which is right: a reload should not delete
// a chat.

export type ChatRowAction =
  | "share"
  | "download"
  | "transfer"
  | "patrol"
  | "task"
  | "workflow"
  | "delete"
  | "archive"
  | "unarchive";

export type ChatHandoff =
  | { kind: "new-chat"; sealed?: boolean }
  | { kind: "new-project" }
  | { kind: "select" }
  | { kind: "delete-temporary" }
  | { kind: "chat"; action: ChatRowAction; conversation: ConversationSummary }
  | { kind: "move"; conversationId: string; projectId: string }
  | { kind: "project"; action: "new-chat" | "delete"; projectId: string };

let pending: ChatHandoff | null = null;

// Which actions are about a chat the person should be looking at when the
// dialog opens. A patrol or a background task opens its chat itself, as part
// of what it does; deleting or archiving needs no chat opened first.
const OPENS_ITS_CHAT: ReadonlySet<ChatRowAction> = new Set(["share", "download", "transfer", "workflow"]);

/** Where Chat should be opened for a hand-off. */
export function handoffHref(h: ChatHandoff): string {
  if (h.kind === "chat" && OPENS_ITS_CHAT.has(h.action)) {
    return `/chat?c=${encodeURIComponent(h.conversation.id)}`;
  }
  // Chat's own new-chat link: it starts empty instead of opening the most
  // recent chat first.
  if (h.kind === "new-chat") return "/chat?draft=";
  return "/chat";
}

/** Notes the action and goes to Chat, which carries it out. */
export function handOffToChat(h: ChatHandoff) {
  pending = h;
  navigate(handoffHref(h));
}

/** Chat takes the noted action (once). */
export function takeChatHandoff(): ChatHandoff | null {
  const h = pending;
  pending = null;
  return h;
}

/** Whether an action is waiting for Chat. */
export function hasChatHandoff(): boolean {
  return pending !== null;
}

/** Where a row opens from a page that is not Chat. */
export function chatHref(c: Pick<ConversationSummary, "id">): string {
  return `/chat?c=${encodeURIComponent(c.id)}`;
}

/**
 * Where a teammate's chat opens: the team link for one shared with the team
 * (it resolves to the read-only viewer or the chat itself, as the viewer's
 * access allows), the chat itself for one reached through Multiplayer.
 */
export function teamChatHref(c: Pick<ConversationSummary, "id" | "multiplayer" | "team_visible">): string {
  const id = encodeURIComponent(c.id);
  return c.multiplayer || !c.team_visible ? `/chat?c=${id}` : `/chat?team=${id}`;
}

export function projectHref(projectId: string): string {
  return `/chat?project=${encodeURIComponent(projectId)}`;
}
