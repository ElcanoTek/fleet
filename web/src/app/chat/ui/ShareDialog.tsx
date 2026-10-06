"use client";

// The Share dialog (ADR-0057; docs/TEAM-SHARING.md "Share dialog").
//
// One team block, always first, and one public-link row below it. The team
// block names the team, stays at full contrast in every state, and carries
// exactly ONE main button that completes the whole fix for the state the chat
// is in — the reader never has to leave, find a second surface, and come back:
//
//   A1   no project, a project shared with the team exists → pick one, "Move and share"
//   A1b  no project, none shared with the team            → "Create shared project"
//   A2   personal project                                  → "Share project first"
//   A3a  caller on no team                                 → guidance + an unavailable control
//   A4   team project, chat not shared                     → file line + "Share with <team>"
//   A5   chat shared                                       → "Copy link for <team>" + Stop sharing
//   A5b  stop sharing, confirming                          → names the shared-file count
//   A6   team project, chat has a public link              → warning + "Share with <team>"
//
// plus the case the canvas calls s7: the chat's project is shared with a team
// the caller is not in. Its fix is the A1 one — move the chat into one of the
// caller's own team projects — so it reuses that picker with its own sentence.
//
// The rule underneath is unchanged (ADR-0057): a chat is shared with a team
// only when it sits in a project shared with that team AND its owner shares
// it. Every state above is a route to satisfying that rule, never a way
// round it — the server refuses anything else with a 409, and that sentence
// is shown here, in front of the control it refused.
//
// Sharing a chat shares its OUTPUTS (files the agent presented in a reply),
// minus any the owner unchecked. The dialog reads them from
// GET /conversations/{id}/outputs; uploads are never outputs, so they are
// never listed or counted here.
//
// Buttons name the team, never the project (#11): the project is where the
// chat lives, the team is who will see it.

import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { LockGlyph, ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { Icon } from "./Icon";
import { formatBytes } from "./formatters";
import { useChatToast } from "./ChatToasts";
import {
  fetchConversationOutputs,
  filesLabel,
  formatDay,
  plural,
  sharedToast,
  teamLinkUrl,
  type ConversationOutputs,
  type ShareWithTeamResult,
} from "./teamSharing";
import { useEffect, useId, useState } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { CloseButton } from "@/app/shared/ui/CloseButton";

// ── shared button looks (live Fleet tokens only) ─────────────────────────
const FOCUS =
  "focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";
const PRIMARY_BTN = `inline-flex min-h-9 items-center gap-2 rounded-full border border-[var(--color-primary)] bg-[var(--color-primary)] px-4 py-2 text-[0.8125rem] font-semibold leading-tight text-[var(--color-on-primary)] transition hover:bg-[var(--color-primary-hover)] disabled:cursor-not-allowed disabled:opacity-60 ${FOCUS}`;
const SECONDARY_BTN = `inline-flex min-h-[2.125rem] items-center gap-1.5 rounded-full border border-[var(--color-border-strong)] bg-transparent px-3.5 py-1.5 text-[0.8125rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] disabled:cursor-not-allowed disabled:opacity-60 ${FOCUS}`;
const DANGER_OUTLINE_BTN = `inline-flex min-h-[2.125rem] items-center rounded-full border border-[var(--color-danger-border)] bg-transparent px-3.5 py-1.5 text-[0.8125rem] font-medium text-[var(--color-danger)] transition hover:bg-[var(--color-overlay-soft)] disabled:cursor-not-allowed disabled:opacity-60 ${FOCUS}`;
const LINK_BTN = `p-0 text-[0.78rem] font-medium text-[var(--color-accent)] underline underline-offset-2 ${FOCUS}`;
const BODY = "m-0 text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]";
const STRONG = "font-semibold text-[var(--color-text-primary)]";

/** Which of the dialog's team-block states the chat is in. Pure, exported for tests. */
export type TeamShareState =
  | "A1" // no project; a team project to move into exists
  | "A1b" // no project; no team project yet
  | "A2" // personal project
  | "A3a" // caller on no team
  | "A4" // team project, not shared
  | "A5" // shared
  | "A6" // team project, public link, not shared
  | "other-team"; // project shared with a team the caller is not in

export function teamShareState({
  conversation,
  project,
  myTeam,
  moveTargets,
}: {
  conversation: ConversationSummary;
  project: Project | null;
  myTeam?: string;
  moveTargets: Project[];
}): TeamShareState {
  // Shared wins over everything: whatever else has changed since (the owner
  // left the team, the project was re-shared), stopping is always possible
  // and is what the owner most needs here. The server never refuses a revoke.
  if (conversation.team_visible) return "A5";
  // "" is a READ empty team; undefined is unread and claims nothing.
  if (myTeam === "") return "A3a";
  if (project?.team_id) {
    if (myTeam !== undefined && project.team_id !== myTeam) return "other-team";
    return conversation.share_token ? "A6" : "A4";
  }
  if (project) return "A2";
  return moveTargets.length > 0 ? "A1" : "A1b";
}

/** "In Knowertech · Shared with Elcano" — the subline under the title. */
function visibilityLabel(
  conversation: Pick<ConversationSummary, "team_visible" | "share_token">,
  team: string,
): string {
  if (conversation.team_visible) {
    return conversation.share_token
      ? `Shared with ${team} and by link`
      : `Shared with ${team}`;
  }
  return conversation.share_token ? "Public link" : "Only you";
}

export type ShareDialogProps = {
  // null when the conversation vanished under the dialog (deleted in another
  // tab): the dialog then says so instead of rendering controls that would
  // act on nothing.
  conversation: ConversationSummary | null;
  /** The chat's project, when it is in one. */
  project: Project | null;
  /** The caller's team: "" = on no team (read), undefined = not read yet. */
  myTeam?: string;
  /** Fleet admin? Only phrases the no-team guidance (A3a). */
  isAdmin?: boolean;
  /**
   * Team-shared projects the caller can see. The dialog narrows them to the
   * caller's OWN team (the server pairs a chat's project against the caller's
   * team, so another team's project is not a fix) for the A1 picker.
   */
  teamSharedProjects?: Project[];
  busy: boolean;
  /** The public link was just copied. */
  copied: boolean;
  /** The last failure or refusal (the server's 409 sentence), shown inline. */
  error?: string | null;
  buildShareUrl: (token: string) => string;
  onCreateLink: (conversation: ConversationSummary) => void;
  onCopyLink: (url: string) => void;
  onStopLink: (conversation: ConversationSummary) => void;
  /**
   * Share with the team (A4/A6). `unsharedPaths`, when given, REPLACES the
   * chat's excluded-file set (the checklist); omitted, earlier choices stand.
   * Resolves null on failure (the caller sets `error`).
   */
  onShareWithTeam: (
    conversation: ConversationSummary,
    unsharedPaths?: string[],
  ) => Promise<ShareWithTeamResult | null>;
  /** Stop sharing with the team (A5b). Resolves null on failure. */
  onStopSharingWithTeam: (
    conversation: ConversationSummary,
  ) => Promise<ShareWithTeamResult | null>;
  /** A1: move into `projectId`, then share. Resolves null on failure. */
  onMoveAndShare: (
    conversation: ConversationSummary,
    projectId: string,
  ) => Promise<ShareWithTeamResult | null>;
  /** A1b: New project with the team preselected; on create the chat moves in and is shared. */
  onCreateSharedProject: (conversation: ConversationSummary) => void;
  /** A2: open the project home with "Share <project> with <team>?" already open. */
  onShareProjectFirst: (
    conversation: ConversationSummary,
    projectId: string,
  ) => void;
  /** "Manage in Sources" / the toast's "Manage": the project home's Sources at this chat. */
  onManageInSources: (
    conversation: ConversationSummary,
    projectId: string,
  ) => void;
  /** Test seam; defaults to GET /conversations/{id}/outputs. */
  loadOutputs?: (conversationId: string) => Promise<ConversationOutputs>;
  onClose: () => void;
};

export function ShareDialog({
  conversation,
  project,
  myTeam,
  isAdmin,
  teamSharedProjects,
  busy,
  copied,
  error,
  buildShareUrl,
  onCreateLink,
  onCopyLink,
  onStopLink,
  onShareWithTeam,
  onStopSharingWithTeam,
  onMoveAndShare,
  onCreateSharedProject,
  onShareProjectFirst,
  onManageInSources,
  loadOutputs = fetchConversationOutputs,
  onClose,
}: ShareDialogProps) {
  const toast = useChatToast();
  const token = conversation?.share_token ?? "";
  // The audience: the team the chat's project is shared with, else the
  // caller's own. "your team" is the honest fallback, never a guessed name.
  const team = project?.team_id || myTeam || "your team";

  const moveTargets = (teamSharedProjects ?? []).filter(
    (p) =>
      Boolean(p.team_id) &&
      (myTeam ? p.team_id === myTeam : true) &&
      p.id !== project?.id,
  );

  const state: TeamShareState | null = conversation
    ? teamShareState({ conversation, project, myTeam, moveTargets })
    : null;
  // The A1 target team is the caller's team (the picker only lists those).
  const audience =
    state === "A1" || state === "A1b" || state === "other-team"
      ? myTeam || "your team"
      : state === "A3a"
        ? "your team"
        : team;

  // Outputs: read whenever the file line can show (A4/A5/A6). Keyed by chat
  // id AND read generation, so a stale read for another chat (or an earlier
  // read of this one) never paints this one, and a re-read after a share is
  // "loading" again rather than the old counts. Three honest states:
  // loading (nothing that promises a count is enabled yet), ready, and
  // failed (said plainly — never rendered as zero files).
  const wantsOutputs =
    state === "A4" || state === "A5" || state === "A6";
  const conversationId = conversation?.id ?? "";
  const [outputsTick, setOutputsTick] = useState(0);
  const outputsKey = `${conversationId}:${outputsTick}`;
  const [outputs, setOutputs] = useState<
    | { key: string; status: "ready"; data: ConversationOutputs }
    | { key: string; status: "failed" }
    | null
  >(null);
  useEffect(() => {
    if (!wantsOutputs || !conversationId) return;
    let cancelled = false;
    const key = `${conversationId}:${outputsTick}`;
    loadOutputs(conversationId).then(
      (data) => {
        if (!cancelled) setOutputs({ key, status: "ready", data });
      },
      () => {
        if (!cancelled) setOutputs({ key, status: "failed" });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [wantsOutputs, conversationId, loadOutputs, outputsTick]);
  const outputsLoad = outputs?.key === outputsKey ? outputs : null;
  const outputsStatus: "loading" | "ready" | "failed" = outputsLoad
    ? outputsLoad.status
    : "loading";
  const files = outputsLoad?.status === "ready" ? outputsLoad.data : null;

  // Checklist (A4): paths the owner UNchecked. Seeded from the server's
  // exclusions so sharing again restores earlier choices (#29).
  const [chooseOpen, setChooseOpen] = useState(false);
  const [unchecked, setUnchecked] = useState<string[] | null>(null);
  const effectiveUnchecked =
    unchecked ??
    (files ? files.outputs.filter((f) => !f.shared).map((f) => f.path) : []);

  // A1 picker.
  const [picked, setPicked] = useState<string>("");
  const pickedId = picked || moveTargets[0]?.id || "";

  // A5b.
  const [confirmStop, setConfirmStop] = useState(false);
  // Public-link row: collapsed unless a link already exists (#03).
  const [linkOpen, setLinkOpen] = useState(Boolean(token));
  const [confirmStopLink, setConfirmStopLink] = useState(false);
  const [pending, setPending] = useState(false);
  const working = busy || pending;

  const linkRowId = useId();
  const pickerId = useId();

  const confirmShared = (
    c: ConversationSummary,
    result: ShareWithTeamResult,
    projectId: string,
    teamName: string,
  ) => {
    toast.notify(
      sharedToast(
        c.title,
        teamName,
        result.shared_files,
        projectId ? () => onManageInSources(c, projectId) : undefined,
      ),
    );
  };

  const run = async (fn: () => Promise<void>) => {
    setPending(true);
    try {
      await fn();
    } finally {
      setPending(false);
    }
  };

  const shareNow = (c: ConversationSummary) =>
    run(async () => {
      // Only send the checklist when the owner opened it: otherwise the
      // server's existing exclusions stand untouched.
      const paths = chooseOpen || unchecked ? effectiveUnchecked : undefined;
      const result = await onShareWithTeam(c, paths);
      if (!result) return;
      setChooseOpen(false);
      setUnchecked(null);
      setOutputsTick((t) => t + 1);
      confirmShared(c, result, project?.id ?? "", team);
    });

  const moveAndShare = (c: ConversationSummary) =>
    run(async () => {
      if (!pickedId) return;
      const target = moveTargets.find((p) => p.id === pickedId);
      const result = await onMoveAndShare(c, pickedId);
      if (!result) return;
      setOutputsTick((t) => t + 1);
      confirmShared(c, result, pickedId, target?.team_id || audience);
    });

  const stopSharing = (c: ConversationSummary) =>
    run(async () => {
      const result = await onStopSharingWithTeam(c);
      setConfirmStop(false);
      if (result) setOutputsTick((t) => t + 1);
    });

  const copyTeamLink = (c: ConversationSummary) => {
    const url = teamLinkUrl(c.id);
    const done = () =>
      toast.notify({
        message: `Link copied. Only ${team} members can open it.`,
      });
    // A blocked (or missing) clipboard is not a copied link — but the
    // owner clicked, so say so and hand them the link to copy themselves
    // rather than letting the click do nothing.
    const failed = () =>
      toast.notify({
        message: `Couldn’t copy the link. Copy it from here: ${url}`,
      });
    const clip =
      typeof navigator !== "undefined" ? navigator.clipboard : undefined;
    if (!clip) {
      failed();
      return;
    }
    void clip.writeText(url).then(done, failed);
  };

  // The A4/A6 file line and its Choose… checklist: the same control in both
  // states, so a chat with a public link in a team project shares with the
  // owner's file choices too. The main Share button waits while the list is
  // loading; a failed list says so and still lets the owner share.
  const shareFileChooser =
    outputsStatus === "failed" ? (
      <p
        data-testid="share-file-line"
        className="m-0 flex flex-wrap items-center gap-1.5 text-[0.78rem] text-[var(--color-text-secondary)]"
      >
        <Icon name="file-text" className="size-3.5 shrink-0" />
        <span>
          Couldn’t list this chat’s files. Sharing shares the files it
          presented; you can adjust them in Sources afterwards.
        </span>
      </p>
    ) : outputsStatus === "loading" ? (
      <p
        data-testid="share-file-line"
        className="m-0 flex flex-wrap items-center gap-1.5 text-[0.78rem] text-[var(--color-text-muted)]"
      >
        <Icon name="file-text" className="size-3.5 shrink-0" />
        <span>Listing this chat’s files…</span>
      </p>
    ) : files ? (
      <div className="flex flex-col gap-2">
        <p
          data-testid="share-file-line"
          className="m-0 flex flex-wrap items-center gap-1.5 text-[0.78rem] text-[var(--color-text-secondary)]"
        >
          <Icon name="file-text" className="size-3.5 shrink-0" />
          {files.total > 0 ? (
            <>
              <span>
                Includes{" "}
                {filesLabel(
                  files.outputs.length - effectiveUnchecked.length,
                )}
              </span>
              <span
                aria-hidden="true"
                className="text-[var(--color-text-muted)]"
              >
                ·
              </span>
              <button
                type="button"
                aria-expanded={chooseOpen}
                className={LINK_BTN}
                onClick={() => {
                  if (!chooseOpen && unchecked === null)
                    setUnchecked(effectiveUnchecked);
                  setChooseOpen(!chooseOpen);
                }}
              >
                {chooseOpen ? "Done" : "Choose…"}
              </button>
            </>
          ) : (
            <span>Files it creates will be shared too.</span>
          )}
        </p>
        {chooseOpen && files.outputs.length > 0 ? (
          <div
            role="group"
            aria-label="Files to share"
            className="flex max-h-52 flex-col overflow-auto rounded-[var(--radius-md)] border border-[var(--color-border)] p-1"
          >
            {files.outputs.map((f) => {
              const checked = !effectiveUnchecked.includes(f.path);
              return (
                <label
                  key={f.path}
                  aria-label={`${f.name}, ${formatBytes(f.size)}, ${formatDay(f.modified_at)}`}
                  className="flex cursor-pointer items-center gap-2.5 rounded-[var(--radius-md)] px-2 py-1.5 hover:bg-[var(--color-overlay-soft)]"
                >
                  <input
                    type="checkbox"
                    checked={checked}
                    onChange={() =>
                      setUnchecked(
                        checked
                          ? [...effectiveUnchecked, f.path]
                          : effectiveUnchecked.filter(
                              (p) => p !== f.path,
                            ),
                      )
                    }
                    className="m-0 size-[0.95rem] shrink-0 accent-[var(--color-primary)]"
                  />
                  <span className="flex min-w-0 flex-1 flex-col gap-px">
                    <span className="text-[0.78rem] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                      {f.name}
                    </span>
                    <span className="font-[family-name:var(--font-code)] text-[0.6875rem] tabular-nums text-[var(--color-text-muted)]">
                      {formatBytes(f.size)} · {formatDay(f.modified_at)}
                    </span>
                  </span>
                </label>
              );
            })}
          </div>
        ) : null}
      </div>
    ) : null;

  return (
    <DialogShell
      label="Share this chat"
      scrimLabel="Close share dialog"
      onDismiss={onClose}
      className="max-w-[29rem] p-5"
    >
      <div className="mb-3.5 flex items-start justify-between gap-3">
        <div className="flex min-w-0 flex-1 flex-col gap-1.5">
          <h2 className="m-0 text-[1.0625rem] font-medium leading-[1.3] text-[var(--color-text-primary)] [text-wrap:pretty]">
            Share {conversation ? `“${conversation.title}”` : "this chat"}
          </h2>
          {conversation ? (
            <p
              data-testid="share-dialog-where"
              className="m-0 flex flex-wrap items-center gap-1.5 text-[0.78rem] text-[var(--color-text-secondary)]"
            >
              {project ? (
                <Icon name="folder" className="size-3.5 shrink-0" />
              ) : null}
              <span>{project ? `In ${project.name}` : "Not in a project"}</span>
              <span aria-hidden="true" className="text-[var(--color-text-muted)]">
                ·
              </span>
              <VisibilityPill conversation={conversation} team={team} />
            </p>
          ) : null}
        </div>
        <CloseButton label="Close share dialog" onClick={onClose} />
      </div>

      {!conversation || !state ? (
        <p className={BODY}>This chat is no longer available.</p>
      ) : (
        <div className="flex flex-col gap-3.5">
          {error ? (
            <p
              role="alert"
              className="m-0 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] px-2.5 py-1.5 text-[0.78rem] leading-[1.55] text-[var(--color-danger)]"
            >
              {error}
            </p>
          ) : null}

          {/* ── The team block: always first, always full contrast ── */}
          <section
            aria-label="Share with your team"
            data-testid="share-team-block"
            data-state={state}
            className="flex flex-col gap-2.5 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_9%,transparent)] p-3.5"
          >
            <h3 className="m-0 flex items-center gap-2 text-[0.9rem] font-semibold text-[var(--color-text-primary)]">
              <TeamGlyph className="size-4 shrink-0" />
              {state === "A5" ? `Shared with ${team}` : `Share with ${audience}`}
            </h3>

            {state === "A1" || state === "other-team" ? (
              <>
                <p className={BODY}>
                  {state === "other-team" && project ? (
                    <>
                      <strong className={STRONG}>{project.name}</strong> is
                      shared with <strong className={STRONG}>{project.team_id}</strong>,
                      and you’re not in that team. To share this chat with{" "}
                      {audience}, move it to a project shared with {audience}.
                    </>
                  ) : (
                    <>
                      To share a chat with {audience}, it needs to be in a
                      project shared with {audience}. Pick one and this chat
                      will move into it.
                    </>
                  )}
                </p>
                {moveTargets.length > 0 ? (
                  <>
                    <div className="flex flex-col gap-1.5">
                      <label
                        htmlFor={pickerId}
                        className="text-[0.75rem] text-[var(--color-text-muted)]"
                      >
                        Project shared with {audience}
                      </label>
                      <select
                        id={pickerId}
                        value={pickedId}
                        disabled={working}
                        onChange={(e) => setPicked(e.target.value)}
                        className="h-9 min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                      >
                        {moveTargets.map((p) => (
                          <option key={p.id} value={p.id}>
                            {p.name}
                          </option>
                        ))}
                      </select>
                    </div>
                    <div>
                      <button
                        type="button"
                        className={PRIMARY_BTN}
                        disabled={working || !pickedId}
                        onClick={() => void moveAndShare(conversation)}
                      >
                        Move and share
                      </button>
                    </div>
                  </>
                ) : (
                  <div>
                    <button
                      type="button"
                      className={PRIMARY_BTN}
                      disabled={working}
                      onClick={() => onCreateSharedProject(conversation)}
                    >
                      <Icon name="plus" className="size-3.5" />
                      Create shared project
                    </button>
                  </div>
                )}
              </>
            ) : null}

            {state === "A1b" ? (
              <>
                <p className={BODY}>
                  To share a chat with {audience}, it needs to be in a project
                  shared with {audience}. Create one to move this chat into.
                </p>
                <div>
                  <button
                    type="button"
                    className={PRIMARY_BTN}
                    disabled={working}
                    onClick={() => onCreateSharedProject(conversation)}
                  >
                    <Icon name="plus" className="size-3.5" />
                    Create shared project
                  </button>
                </div>
              </>
            ) : null}

            {state === "A2" && project ? (
              <>
                <p className={BODY}>
                  <strong className={STRONG}>{project.name}</strong> isn’t
                  shared with {audience}, so its chats are private to you.
                  Share the project first, then this chat.
                </p>
                <div>
                  <button
                    type="button"
                    className={SECONDARY_BTN}
                    disabled={working}
                    onClick={() => onShareProjectFirst(conversation, project.id)}
                  >
                    <Icon name="folder" className="size-3.5" />
                    Share project first
                  </button>
                </div>
              </>
            ) : null}

            {state === "A3a" ? (
              <>
                <p className={BODY}>
                  You’re not on a team yet, so there’s no one to share with.
                </p>
                <ul className="m-0 flex list-disc flex-col gap-1 pl-[1.125rem] text-[0.78rem] leading-[1.5] text-[var(--color-text-secondary)]">
                  {isAdmin !== false ? (
                    <li>
                      <strong className={STRONG}>Admins:</strong> add yourself
                      in Settings → Admin → Users, or create a team in Settings
                      → Team.
                    </li>
                  ) : null}
                  {isAdmin !== true ? (
                    <li>
                      <strong className={STRONG}>Everyone else:</strong> ask an
                      admin to add you to a team.
                    </li>
                  ) : null}
                </ul>
                <div>
                  {/* The one unavailable control: dashed outline, a lock, and
                      the word itself (#10). Only the control looks off — the
                      block around it stays at full contrast. */}
                  <button
                    type="button"
                    disabled
                    aria-disabled="true"
                    className="inline-flex h-9 cursor-not-allowed items-center gap-1.5 rounded-full border border-dashed border-[var(--color-border-strong)] bg-transparent px-4 text-[0.8125rem] font-semibold text-[var(--color-text-disabled)]"
                  >
                    <LockGlyph className="size-3.5" />
                    Share with a team (unavailable)
                  </button>
                </div>
              </>
            ) : null}

            {state === "A4" ? (
              <>
                <p className={BODY}>
                  Teammates find it on {project?.name}’s home, read it, and
                  branch it into their own chat.
                </p>
                {shareFileChooser}
                <div>
                  <button
                    type="button"
                    className={PRIMARY_BTN}
                    disabled={working || outputsStatus === "loading"}
                    onClick={() => void shareNow(conversation)}
                  >
                    Share with {team}
                  </button>
                </div>
              </>
            ) : null}

            {state === "A5" && !confirmStop ? (
              <>
                <p className={BODY}>
                  Teammates can read it and branch it, but can’t edit it. Only{" "}
                  {team} members can open the link.
                </p>
                {files ? (
                  <p
                    data-testid="share-file-line"
                    className="m-0 flex flex-wrap items-center gap-1.5 text-[0.78rem] text-[var(--color-text-secondary)]"
                  >
                    <Icon name="file-text" className="size-3.5 shrink-0" />
                    {files.total > 0 ? (
                      <>
                        <span>
                          {Math.min(files.shared_count, files.total)} of{" "}
                          {filesLabel(files.total)} shared
                        </span>
                        {project ? (
                          <>
                            <span
                              aria-hidden="true"
                              className="text-[var(--color-text-muted)]"
                            >
                              ·
                            </span>
                            <button
                              type="button"
                              className={LINK_BTN}
                              onClick={() =>
                                onManageInSources(conversation, project.id)
                              }
                            >
                              Manage in Sources
                            </button>
                          </>
                        ) : null}
                      </>
                    ) : (
                      <span>Files it creates will be shared too.</span>
                    )}
                  </p>
                ) : null}
                <div className="flex flex-wrap gap-2">
                  <button
                    type="button"
                    className={PRIMARY_BTN}
                    disabled={working}
                    onClick={() => copyTeamLink(conversation)}
                  >
                    <TeamGlyph className="size-3.5" />
                    Copy link for {team}
                  </button>
                  <button
                    type="button"
                    className={DANGER_OUTLINE_BTN}
                    disabled={working}
                    onClick={() => setConfirmStop(true)}
                  >
                    Stop sharing
                  </button>
                </div>
              </>
            ) : null}

            {state === "A5" && confirmStop ? (
              <div
                role="alertdialog"
                aria-label="Stop sharing"
                className="flex flex-col gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_8%,transparent)] p-3"
              >
                <p className="m-0 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)]">
                  Stop sharing with {team}?{" "}
                  {outputsStatus === "loading"
                    ? "Counting shared files…"
                    : outputsStatus === "failed" || !files
                      ? "Its shared files will stop being shared too."
                      : files.shared_count > 0
                        ? `${plural(files.shared_count, "shared file", "shared files")} will stop being shared too.`
                        : "Teammates lose access right away."}
                </p>
                <div className="flex flex-wrap gap-2">
                  <button
                    type="button"
                    // The sentence above names the count it stops; the
                    // confirm waits for it (a failed count still allows it,
                    // with the honest no-number sentence).
                    disabled={working || outputsStatus === "loading"}
                    onClick={() => void stopSharing(conversation)}
                    className={`inline-flex min-h-[1.875rem] items-center rounded-full border border-[var(--color-danger)] bg-[var(--color-danger)] px-3.5 text-[0.78rem] font-semibold text-[var(--color-surface-1)] transition hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-60 ${FOCUS}`}
                  >
                    Stop sharing
                  </button>
                  <button
                    type="button"
                    disabled={working}
                    onClick={() => setConfirmStop(false)}
                    className={`inline-flex min-h-[1.875rem] items-center rounded-full border border-[var(--color-border-strong)] bg-transparent px-3.5 text-[0.78rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-60 ${FOCUS}`}
                  >
                    Keep sharing
                  </button>
                </div>
              </div>
            ) : null}

            {state === "A6" ? (
              <>
                <div className="flex items-start gap-2.5 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-warning-strong)_50%,transparent)] bg-[color-mix(in_srgb,var(--color-warning-strong)_12%,transparent)] px-3 py-2.5">
                  <Icon
                    name="warning"
                    className="mt-0.5 size-3.5 shrink-0 text-[var(--color-warning-soft)]"
                  />
                  <p className="m-0 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)]">
                    Teammates can’t branch from a link, and it won’t show on{" "}
                    {project?.name}’s home.
                  </p>
                </div>
                {shareFileChooser}
                <div>
                  <button
                    type="button"
                    className={PRIMARY_BTN}
                    disabled={working || outputsStatus === "loading"}
                    onClick={() => void shareNow(conversation)}
                  >
                    Share with {team}
                  </button>
                </div>
              </>
            ) : null}
          </section>

          {/* ── Public link: one collapsed row, behavior unchanged ── */}
          <section
            aria-label="Share outside your team"
            className="flex flex-col rounded-[var(--radius-lg)] border border-[var(--color-border)]"
          >
            <button
              type="button"
              aria-expanded={linkOpen}
              aria-controls={linkRowId}
              onClick={() => setLinkOpen(!linkOpen)}
              className={`flex items-center gap-2.5 rounded-[var(--radius-lg)] bg-transparent px-3.5 py-3 text-left text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] ${FOCUS}`}
            >
              <ShareGlyph className="size-4 shrink-0 text-[var(--color-text-secondary)]" />
              <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                <span className="text-[0.84rem] font-medium">
                  Share outside your team
                </span>
                <span className="text-[0.75rem] text-[var(--color-text-muted)]">
                  {token
                    ? "Link is on. Anyone with it can read the transcript, never files."
                    : "Public link. Transcript only, never files."}
                </span>
              </span>
              <Icon
                name={linkOpen ? "chevron-down" : "chevron-right"}
                className="size-4 shrink-0 text-[var(--color-text-muted)]"
              />
            </button>
            {linkOpen ? (
              <div id={linkRowId} className="flex flex-col gap-2.5 px-3.5 pb-3.5">
                {token ? (
                  <>
                    <div className="flex items-center gap-2">
                      <input
                        readOnly
                        aria-label="Share link URL"
                        value={buildShareUrl(token)}
                        onFocus={(e) => e.currentTarget.select()}
                        className="h-8 min-w-0 flex-1 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 font-mono text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                      />
                      <button
                        type="button"
                        onClick={() => onCopyLink(buildShareUrl(token))}
                        className={`h-8 shrink-0 rounded-full border border-[var(--color-accent)] px-3.5 text-[0.78rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-accent)] hover:text-[var(--color-surface-1)] ${FOCUS}`}
                      >
                        {copied ? "Copied ✓" : "Copy link"}
                      </button>
                    </div>
                    {/* Revoking is destructive for whoever holds the URL, so
                        it keeps its two-step confirm (unchanged behavior). */}
                    {confirmStopLink ? (
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
                          Anyone holding the link loses access at once.
                        </span>
                        <button
                          type="button"
                          disabled={working}
                          onClick={() => {
                            setConfirmStopLink(false);
                            onStopLink(conversation);
                          }}
                          className={`rounded-full border border-[var(--color-danger-border)] bg-[var(--color-danger)] px-3 py-1 text-[0.78rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-50 ${FOCUS}`}
                        >
                          {busy ? "Stopping…" : "Stop the public link"}
                        </button>
                        <button
                          type="button"
                          disabled={working}
                          onClick={() => setConfirmStopLink(false)}
                          className={`rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.78rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50 ${FOCUS}`}
                        >
                          Keep the link
                        </button>
                      </div>
                    ) : (
                      <div>
                        <button
                          type="button"
                          disabled={working}
                          onClick={() => setConfirmStopLink(true)}
                          className={`rounded-full border border-[var(--color-danger-border)] px-3 py-1 text-[0.75rem] font-medium text-[var(--color-danger)] transition hover:bg-[var(--color-overlay-soft)] disabled:cursor-not-allowed disabled:opacity-50 ${FOCUS}`}
                        >
                          Stop the public link
                        </button>
                      </div>
                    )}
                  </>
                ) : (
                  <>
                    <p className="m-0 text-[0.78rem] leading-[1.5] text-[var(--color-text-secondary)]">
                      Anyone with the link can read the transcript, including
                      people outside your team. They can’t branch it, and files
                      are never included.
                    </p>
                    <div>
                      {/* `disabled` while a request is in flight: a second
                          click used to mint a second POST. */}
                      <button
                        type="button"
                        disabled={working}
                        onClick={() => onCreateLink(conversation)}
                        className={`rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.78rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] disabled:cursor-not-allowed disabled:opacity-50 ${FOCUS}`}
                      >
                        {busy ? "Creating…" : "Create public link"}
                      </button>
                    </div>
                  </>
                )}
              </div>
            ) : null}
          </section>

          <div className="flex justify-end">
            <button
              type="button"
              onClick={onClose}
              className={`rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] ${FOCUS}`}
            >
              Done
            </button>
          </div>
        </div>
      )}
    </DialogShell>
  );
}

// The visibility pill: Only you (lock), Shared with <team> (people), or
// Public link (chain). Used in the dialog's subline and the chat header.
function VisibilityPill({
  conversation,
  team,
}: {
  conversation: Pick<ConversationSummary, "team_visible" | "share_token">;
  team: string;
}) {
  const shared = Boolean(conversation.team_visible);
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full border px-2 py-px text-[0.72rem] ${
        shared
          ? "border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-text-primary)]"
          : "border-[var(--color-border)] text-[var(--color-text-secondary)]"
      }`}
    >
      {shared ? (
        <TeamGlyph className="size-3 shrink-0" />
      ) : conversation.share_token ? (
        <ShareGlyph className="size-3 shrink-0" />
      ) : (
        <LockGlyph className="size-3 shrink-0" />
      )}
      {visibilityLabel(conversation, team)}
    </span>
  );
}

// ChatShareControls: the chat header's sharing entry point (#12) — the
// visibility chip ("Only you" / "Shared with <team>" / "Public link") and a
// Share button, on every chat. Both open the share dialog: the chip states a
// fact, and the dialog is where that fact is changed. The chip's label
// collapses to its glyph on a phone; its accessible name carries the state at
// every width.
export function ChatShareControls({
  conversation,
  team,
  onOpen,
}: {
  conversation: Pick<ConversationSummary, "team_visible" | "share_token">;
  team?: string;
  onOpen: () => void;
}) {
  const label = visibilityLabel(conversation, team || "your team");
  const shared = Boolean(conversation.team_visible);
  return (
    <span className="ml-auto inline-flex shrink-0 items-center gap-2">
      <button
        type="button"
        data-testid="chat-header-visibility-chip"
        aria-label={`${label} — open sharing`}
        title={`${label}. Click to change sharing.`}
        onClick={onOpen}
        className={`inline-flex h-[1.625rem] items-center gap-1.5 rounded-full border px-2.5 text-[0.75rem] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] ${FOCUS} ${
          shared
            ? "border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-text-primary)]"
            : "border-[var(--color-border)] text-[var(--color-text-secondary)]"
        }`}
      >
        {shared ? (
          <TeamGlyph className="size-3 shrink-0" />
        ) : conversation.share_token ? (
          <ShareGlyph className="size-3 shrink-0" />
        ) : (
          <LockGlyph className="size-3 shrink-0" />
        )}
        <span className="hidden sm:inline">{label}</span>
      </button>
      <button
        type="button"
        data-testid="chat-header-share"
        onClick={onOpen}
        className={`inline-flex h-[1.875rem] items-center rounded-full border border-[var(--color-border-strong)] bg-transparent px-3.5 text-[0.8125rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] ${FOCUS}`}
      >
        Share
      </button>
    </span>
  );
}
