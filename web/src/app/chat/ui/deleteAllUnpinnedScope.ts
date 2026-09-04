// The client's mirror of the server's "delete all unpinned" predicate.
//
// `DELETE /conversations` with no body runs store.DeleteAllUnpinned, whose
// WHERE clause is:
//
//   user_email = $1 AND pinned = FALSE AND archived_at IS NULL
//     AND project_id IS NULL
//
// Three keep states, not one. The rail used to drop everything but the pinned
// rows from local state on confirm, so every project chat vanished from the
// rail until the next reload even though the server had (correctly) kept it —
// the deletion looked like it had taken the projects with it, at exactly the
// moment a user would panic (finding #20).
//
// The archived clause needs no expression here: archived conversations live in
// a separate `archivedConversations` list which this action never touches, so
// "not archived" is already true of every row the predicate is applied to.
//
// One exported predicate, used BOTH for the count in the confirm dialog and
// for the post-delete local update, so the two can never disagree with each
// other. The delete still refetches afterwards, which is what makes the
// server — not this file — the authority if the SQL ever changes.

export type DeletableConversation = {
  pinned: boolean;
  project_id?: string;
};

// deletedByDeleteAllUnpinned is true for exactly the rows the server removes.
export function deletedByDeleteAllUnpinned(c: DeletableConversation): boolean {
  return !c.pinned && !c.project_id;
}

// survivesDeleteAllUnpinned is its complement: pinned OR filed in a project.
export function survivesDeleteAllUnpinned(c: DeletableConversation): boolean {
  return !deletedByDeleteAllUnpinned(c);
}

// afterDeleteAllUnpinned is the local list update.
export function afterDeleteAllUnpinned<T extends DeletableConversation>(
  conversations: T[],
): T[] {
  return conversations.filter(survivesDeleteAllUnpinned);
}

// countDeletedByDeleteAllUnpinned is the number the confirm dialog quotes.
export function countDeletedByDeleteAllUnpinned(
  conversations: DeletableConversation[],
): number {
  return conversations.filter(deletedByDeleteAllUnpinned).length;
}
