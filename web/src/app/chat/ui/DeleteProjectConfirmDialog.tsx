"use client";

import { ConfirmDialog, NameChip } from "./ConfirmDialog";

// The RAIL kebab's "Delete project" confirmation (finding #13).
//
// The project home owns the richer version of this dialog — it can fetch the
// real counts (how many team learnings die with the project, how many chats
// from how many members leave it) and offer the export inline. The rail has no
// page to load those on, so it keeps the shorter copy that points at the
// project home, verbatim from the window.confirm it replaces. What it gains is
// being the app's own dialog: styled, scrim-dismissable, and able to render
// the project as a chip rather than bare characters in a string.
export function DeleteProjectConfirmDialog({
  projectName,
  onCancel,
  onConfirm,
}: {
  projectName?: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <ConfirmDialog
      bodyId="rail-delete-project-body"
      cancelAriaLabel="Cancel deleting the project"
      confirmLabel="Delete project"
      confirmTone="danger"
      testId="rail-delete-project-confirm"
      onCancel={onCancel}
      onConfirm={onConfirm}
    >
      Delete <NameChip>{projectName || "this project"}</NameChip>? Its team
      learnings are lost, and every member&apos;s chats leave the project and
      become temporary. Open the project to see the counts and export first.
    </ConfirmDialog>
  );
}
