"use client";

import { useEffect, useState } from "react";
import { ConfirmDialog, NameChip } from "./ConfirmDialog";
import { TeamGlyph } from "./ShareGlyphs";
import { fetchConversationOutputs, plural } from "./teamSharing";

// The confirmations the chat rail needs before a chat changes where it lives
// or whether it exists (finding #13, then the sharing decisions #25–#30).
// They used to be `window.confirm`, so none could render the project or the
// team as anything but characters in a string, and the unfile one could not
// offer the action its own sentence promises.
//
// The rule the shared-chat variants enforce: every way a shared chat loses its
// audience says so first, names the audience, and quotes how many SHARED files
// go with it (outputs the owner hasn't unchecked — never uploads). The count
// is read from GET /conversations/{id}/outputs → shared_count, only for a chat
// that is actually shared; a private chat keeps today's copy (or no confirm).
export type MoveConfirmKind =
  // A team-shared chat is being moved into a project NOT shared with its team
  // (B35).
  | "unshare-move"
  // A team-shared chat is being taken out of its project altogether (B32).
  | "unshare-unfile"
  // A private chat is being taken out of its project, back into Temporary.
  | "unfile";

export type MoveConfirm = {
  kind: MoveConfirmKind;
  // The project being moved INTO, for "unshare-move" only.
  targetProjectName?: string;
  // The audience the chat is currently shared with, named rather than
  // inferred (ADR-0057). Undefined = fall back to "your team".
  team?: string;
  // The chat, for the shared variants' copy and their shared-file count.
  conversationId?: string;
  chatTitle?: string;
};

// decideMoveConfirm answers "does this re-filing need a confirmation, and
// which one". Pure, so the decision is testable without a dialog.
//
// The server's rule (ADR-0057, unchanged): a move between two projects shared
// with the SAME team keeps the chat shared; any other move of a shared chat
// unshares it. So the destination's team is compared with the chat's current
// project's team when both are known. `target` undefined/null with a
// non-empty projectID means "a project the local list hasn't loaded", which
// can't be assumed to be shared.
export function decideMoveConfirm({
  conversation,
  projectID,
  target,
  sourceTeam,
  team,
}: {
  conversation?: {
    id?: string;
    title?: string;
    team_visible?: boolean;
    project_id?: string;
  } | null;
  // "" = unfile.
  projectID: string;
  target?: {
    id: string;
    name: string;
    teamShared: boolean;
    // The team the destination is shared with, when known.
    team?: string;
  } | null;
  // The team the chat's CURRENT project is shared with, when known.
  sourceTeam?: string;
  team?: string;
}): MoveConfirm | null {
  if (conversation?.project_id && conversation.project_id === projectID) {
    return null;
  }
  const sameTeam =
    Boolean(target?.teamShared) &&
    (!sourceTeam || !target?.team || target.team === sourceTeam);
  const leavingTeamShare = Boolean(conversation?.team_visible) && !sameTeam;
  const chat =
    conversation?.id || conversation?.title
      ? { conversationId: conversation?.id, chatTitle: conversation?.title }
      : {};
  if (leavingTeamShare) {
    return projectID
      ? {
          kind: "unshare-move",
          targetProjectName: target?.name ?? "another project",
          team,
          ...chat,
        }
      : { kind: "unshare-unfile", team, ...chat };
  }
  // Unfiling drops a chat back into Temporary, where retention can reach it —
  // the corollary of "chats in a project don't expire".
  if (!projectID && conversation?.project_id) return { kind: "unfile" };
  return null;
}

// useSharedFileCount reads how many of a shared chat's outputs are shared.
// undefined = still counting; null = the read failed (the copy then names the
// files without a number — a count we don't have is never rendered as 0).
function useSharedFileCount(
  conversationId: string | undefined,
  enabled = true,
): number | null | undefined {
  const [state, setState] = useState<{
    id: string;
    count: number | null;
  } | null>(null);
  useEffect(() => {
    if (!enabled || !conversationId) return;
    let cancelled = false;
    fetchConversationOutputs(conversationId)
      .then((r) => {
        if (!cancelled)
          setState({
            id: conversationId,
            count: typeof r.shared_count === "number" ? r.shared_count : null,
          });
      })
      .catch(() => {
        if (!cancelled) setState({ id: conversationId, count: null });
      });
    return () => {
      cancelled = true;
    };
  }, [conversationId, enabled]);
  if (!enabled || !conversationId) return null;
  return state && state.id === conversationId ? state.count : undefined;
}

/** "1 shared file" / "3 shared files". */
function sharedFilesLabel(n: number): string {
  return plural(n, "shared file", "shared files");
}

// ", along with 3 shared files" — empty for 0, unnumbered when unknown.
function alongWith(count: number | null | undefined, its = false): string {
  if (count === 0) return "";
  const pre = its ? "its " : "";
  if (typeof count !== "number") return `, along with ${pre}shared files`;
  return `, along with ${pre}${sharedFilesLabel(count)}`;
}

function quoted(title?: string): string {
  return `“${title?.trim() || "This chat"}”`;
}

export function TeamChip({ team, suffix }: { team?: string; suffix?: string }) {
  return (
    <NameChip icon={<TeamGlyph className="size-3 shrink-0" />} suffix={suffix}>
      {team || "your team"}
    </NameChip>
  );
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
  // hatch. Only offered on the confirms whose copy makes that promise.
  onPinAndConfirm: () => void;
}) {
  const shared = confirm.kind !== "unfile";
  const count = useSharedFileCount(confirm.conversationId, shared);

  if (confirm.kind === "unfile") {
    // A private chat: today's confirmation, unchanged.
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

  if (confirm.kind === "unshare-unfile") {
    // B32: today's sentence, plus what it costs the team.
    return (
      <ConfirmDialog
        title="Remove from project?"
        cancelAriaLabel="Cancel removing this chat from the project"
        confirmLabel="Pin it and remove"
        confirmTone="accent"
        busy={count === undefined}
        onCancel={onCancel}
        onConfirm={onPinAndConfirm}
        secondary={{ label: "Remove", onClick: onConfirm }}
        testId="move-chat-confirm"
      >
        <p className="m-0" data-testid="move-chat-confirm-body">
          {quoted(confirm.chatTitle)} will become temporary and expire unless
          pinned. It also stops being shared with{" "}
          <TeamChip team={confirm.team} suffix={`${alongWith(count)}.`} />
        </p>
      </ConfirmDialog>
    );
  }

  // B35: a shared chat moving to a project its team can't see.
  const target = confirm.targetProjectName || "another project";
  return (
    <ConfirmDialog
      title={`Move to ${target}?`}
      cancelAriaLabel="Cancel moving this chat"
      confirmLabel="Move and stop sharing"
      confirmTone="accent"
      busy={count === undefined}
      onCancel={onCancel}
      onConfirm={onConfirm}
      testId="move-chat-confirm"
    >
      <p className="m-0" data-testid="move-chat-confirm-body">
        <NameChip>{target}</NameChip> isn&rsquo;t shared with{" "}
        <TeamChip team={confirm.team} suffix="," /> so{" "}
        {quoted(confirm.chatTitle)} will stop being shared{alongWith(count)}.
        Teammates lose access, but their branches keep their copies.
      </p>
    </ConfirmDialog>
  );
}

// The two other ways a SHARED chat loses its audience: deleting it (B33) and
// archiving it (B34). A private chat never reaches this dialog — delete keeps
// today's confirm and archive has none.
export function SharedChatLossConfirmDialog({
  kind,
  conversationId,
  chatTitle,
  team,
  busy,
  onCancel,
  onConfirm,
}: {
  kind: "delete" | "archive";
  conversationId: string;
  chatTitle: string;
  team?: string;
  busy?: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const count = useSharedFileCount(conversationId);
  const title = `${kind === "delete" ? "Delete" : "Archive"} ${quoted(chatTitle)}?`;
  const teamName = team || "your team";
  return (
    <ConfirmDialog
      title={title}
      confirmLabel={
        kind === "delete" ? (busy ? "Deleting…" : "Delete chat") : "Archive chat"
      }
      confirmTone={kind === "delete" ? "danger" : "accent"}
      busy={busy || count === undefined}
      onCancel={onCancel}
      onConfirm={onConfirm}
      testId={`shared-chat-${kind}-confirm`}
    >
      {kind === "delete" ? (
        <p className="m-0">
          The chat and its files are deleted. <TeamChip team={teamName} /> will
          lose access to this chat
          {count === 0
            ? ""
            : typeof count === "number"
              ? ` and its ${sharedFilesLabel(count)}`
              : " and its shared files"}
          . Teammates who branched it keep their copies.
        </p>
      ) : (
        <p className="m-0">
          Archiving stops sharing this chat with{" "}
          <TeamChip team={teamName} suffix={`${alongWith(count, true)}.`} />{" "}
          Teammates who branched it keep their copies. If you unarchive it, it
          comes back as Only you.
        </p>
      )}
    </ConfirmDialog>
  );
}
