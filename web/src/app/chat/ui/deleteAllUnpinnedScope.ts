// The client's mirror of the server's definition of a TEMPORARY chat.
//
// One SQL constant on the server (store.temporaryConversationClause) decides
// what the retention sweep deletes for inactivity AND what "Delete temporary
// chats" (`DELETE /conversations` with no body, store.DeleteAllUnpinned)
// removes:
//
//   pinned = FALSE AND archived_at IS NULL AND project_id IS NULL
//     AND share_token IS NULL AND team_visible = FALSE AND multiplayer = FALSE
//     AND no armed or paused patrol or timer
//
// A chat is temporary unless something keeps it. The rail used to drop
// everything but the pinned rows from local state on confirm, so every
// project chat vanished from the rail until the next reload even though the
// server had (correctly) kept it — the deletion looked like it had taken the
// projects with it, at exactly the moment a user would panic (finding #20).
// And the button used to delete every unpinned, unfiled chat — a chat shared
// with the team, a Multiplayer room, a chat with a patrol — which the sweep
// itself never touches.
//
// The archived clause needs no expression here: archived conversations live in
// a separate `archivedConversations` list which this action never touches, so
// "not archived" is already true of every row the predicate is applied to.
//
// One exported predicate, used for the rail's hourglass mark, for the count
// in the confirm dialog and for the post-delete local update, so the three
// can never disagree with each other. The delete still refetches afterwards,
// which is what makes the server — not this file — the authority if the SQL
// ever changes.

export type DeletableConversation = {
  id?: string;
  pinned: boolean;
  project_id?: string;
  share_token?: string;
  team_visible?: boolean;
  multiplayer?: boolean;
  // From the server's listing: an armed or paused patrol or timer keeps it.
  has_schedule?: boolean;
};

// What this page knows of a chat's schedules, for the chats it has read them
// for (opened here, or changed here): fresher than the listing's has_schedule,
// so a patrol made or ended a minute ago counts at once.
export type ScheduleKinds = ReadonlyMap<string, "patrol" | "timer" | "none">;

// isTemporaryChat is true for exactly the chats the server would delete.
export function isTemporaryChat(c: DeletableConversation, kinds?: ScheduleKinds): boolean {
  const known = c.id ? kinds?.get(c.id) : undefined;
  const scheduled = known ? known !== "none" : Boolean(c.has_schedule);
  return !c.pinned && !c.project_id && !c.share_token && !c.team_visible && !c.multiplayer && !scheduled;
}

// deletedByDeleteAllUnpinned is true for exactly the rows the server removes.
export function deletedByDeleteAllUnpinned(c: DeletableConversation, kinds?: ScheduleKinds): boolean {
  return isTemporaryChat(c, kinds);
}

// survivesDeleteAllUnpinned is its complement: something keeps the chat.
export function survivesDeleteAllUnpinned(c: DeletableConversation, kinds?: ScheduleKinds): boolean {
  return !deletedByDeleteAllUnpinned(c, kinds);
}

// afterDeleteAllUnpinned is the local list update.
export function afterDeleteAllUnpinned<T extends DeletableConversation>(
  conversations: T[],
  kinds?: ScheduleKinds,
): T[] {
  return conversations.filter((c) => survivesDeleteAllUnpinned(c, kinds));
}

// countDeletedByDeleteAllUnpinned is the number the confirm dialog quotes.
export function countDeletedByDeleteAllUnpinned(
  conversations: DeletableConversation[],
  kinds?: ScheduleKinds,
): number {
  return conversations.filter((c) => deletedByDeleteAllUnpinned(c, kinds)).length;
}
