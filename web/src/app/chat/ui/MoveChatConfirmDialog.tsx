"use client";

import { ConfirmDialog, NameChip } from "./ConfirmDialog";
import { TeamGlyph } from "./ShareGlyphs";

// The two re-filing confirmations the chat rail needs, as one in-app dialog
// (finding #13). Both used to be `window.confirm`, so neither could render the
// project or the team as anything but characters in a string, and the second
// could not offer the action its own sentence promises.
//
// The copy is UNCHANGED from those confirms. What changed is that the names in
// it are chips, and that "expire unless pinned" now has a button.
export type MoveConfirmKind =
  // A team-shared chat is being moved into a project that is not team-shared.
  | "unshare-move"
  // A team-shared chat is being taken out of its project altogether.
  | "unshare-unfile"
  // A chat is being taken out of its project, back into Temporary.
  | "unfile";

export type MoveConfirm = {
  kind: MoveConfirmKind;
  // The project being moved INTO, for "unshare-move" only.
  targetProjectName?: string;
  // The audience the chat is currently shared with, named rather than
  // inferred (ADR-0057). Undefined = fall back to "your team".
  team?: string;
};

// decideMoveConfirm answers "does this re-filing need a confirmation, and
// which one" — the branch that used to sit inline in front of the two
// window.confirm calls. Pure, so the decision is testable without a dialog.
//
// `target` is the destination project as the caller knows it; undefined/null
// with a non-empty projectID means "a project the local list hasn't loaded",
// which is exactly the case the old code called "another project".
export function decideMoveConfirm({
  conversation,
  projectID,
  target,
  team,
}: {
  conversation?: { team_visible?: boolean; project_id?: string } | null;
  // "" = unfile.
  projectID: string;
  target?: { id: string; name: string; teamShared: boolean } | null;
  team?: string;
}): MoveConfirm | null {
  // A team-shared chat cannot exist outside a team-shared project — the
  // server clears the flag on the way out — so say so before the move rather
  // than letting a teammate's bookmark quietly 404.
  const leavingTeamShare =
    Boolean(conversation?.team_visible) && !target?.teamShared;
  if (leavingTeamShare) {
    return projectID
      ? {
          kind: "unshare-move",
          targetProjectName: target?.name ?? "another project",
          team,
        }
      : { kind: "unshare-unfile", team };
  }
  // Unfiling drops a chat back into Temporary, where retention can reach it —
  // the corollary of "chats in a project don't expire".
  if (!projectID && conversation?.project_id) return { kind: "unfile" };
  return null;
}

export function MoveChatConfirmDialog({
  confirm,
  onCancel,
  onConfirm,
  onPinAndConfirm,
}: {
  confirm: MoveConfirm;
  onCancel: () => void;
  onConfirm: () => void;
  // Pin the chat, then do the removal — the "expire unless pinned" escape
  // hatch. Only offered on the confirm whose copy makes that promise.
  onPinAndConfirm: () => void;
}) {
  const team = (
    <NameChip icon={<TeamGlyph className="size-3 shrink-0" />} suffix=".">
      {confirm.team || "your team"}
    </NameChip>
  );

  if (confirm.kind === "unfile") {
    return (
      <ConfirmDialog
        bodyId="move-chat-confirm-body"
        cancelAriaLabel="Cancel removing this chat from the project"
        confirmLabel="Remove from project"
        onCancel={onCancel}
        onConfirm={onConfirm}
        secondary={{ label: "Pin it and remove", onClick: onPinAndConfirm }}
        testId="move-chat-confirm"
      >
        This chat will become temporary and expire unless pinned. Remove it from
        the project?
      </ConfirmDialog>
    );
  }

  return (
    <ConfirmDialog
      bodyId="move-chat-confirm-body"
      cancelAriaLabel="Cancel moving this chat"
      confirmLabel="Continue"
      onCancel={onCancel}
      onConfirm={onConfirm}
      testId="move-chat-confirm"
    >
      {confirm.kind === "unshare-move" ? (
        <>
          This chat is shared with {team} Moving it to{" "}
          <NameChip suffix=",">{confirm.targetProjectName}</NameChip> which isn&apos;t
          shared with your team, will unshare it. Continue?
        </>
      ) : (
        <>
          This chat is shared with {team} Removing it from the project will
          unshare it. Continue?
        </>
      )}
    </ConfirmDialog>
  );
}
