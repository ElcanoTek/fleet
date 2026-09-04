"use client";

// The Share dialog: one container, two audiences (#226 + ADR-0013, joined by
// ADR-0057).
//
// Sharing used to mean exactly one thing here — mint a public link — while a
// second, quite different sharing scope existed at the API and nowhere in the
// UI, and Settings → Team's copy already promised users they could "share it
// with your team from its own menu". This dialog makes that true and keeps the
// two scopes visibly apart:
//
//   • Share with team — your team, read-only, revocable, no URL exists.
//   • Share by link   — anyone who has the URL, including people outside the
//                       deployment. Read-only, revocable.
//
// Team sharing is deliberately NARROWER here than the backend allows: it is
// offered only for a chat inside a TEAM-SHARED PROJECT. That is not a
// technical limit, it is the product one — the project home's Team section is
// the single place a teammate goes looking, so a team-shared chat with no
// project would be visible to people with no surface that lists it. When the
// toggle is unavailable the helper text says which of the two situations the
// user is in and what to do about it, rather than greying out silently.
//
// Two things that section learned from QA (C-2 / B-4):
//
//  1. ONE disabled treatment, applied to every unavailable state. A bare
//     `disabled` on a native checkbox is very nearly invisible — the box still
//     looks live, the pointer stays an arrow, and nothing but the prose says
//     otherwise — so it read as a broken control rather than an unavailable
//     one. `CONTROL_UNAVAILABLE` + `aria-disabled` is that treatment, and it
//     is derived from the same `canTeamShare` the `disabled` attribute is, so
//     no state can drift out of it.
//  2. The section OFFERS the fix, it does not only describe it. Each
//     unavailable state carries the action that resolves it: move the chat
//     into one of your team-shared projects (a select, right here — the move
//     is optimistic upstream, so the toggle goes live in this same dialog),
//     share the project with your team (owner), ask the owner (member), share
//     *a* project with your team first, or — with no team at all — the same
//     role-branched pointer the Projects modal gives.
//
// Every one of those affordances is behind an OPTIONAL prop. Absent the prop
// the section degrades to the descriptive copy it had before rather than
// claiming something it cannot know: "you have no team-shared projects" is a
// statement, and it must not be made from an unread list.

import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { useId } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { CloseButton } from "@/app/shared/ui/CloseButton";

// The single unavailable treatment: dimmed, and a pointer that says the
// control will not respond. Paired everywhere with the native `disabled` (so
// it is genuinely inert, not a handler that no-ops) and `aria-disabled` (so a
// screen reader is told, not left to infer it from the sentence beside it).
const CONTROL_UNAVAILABLE = "cursor-not-allowed opacity-50";

// Why the team toggle is unavailable — one case per fix, because the fixes
// differ. `null` is not in the union: it means the toggle is live.
type UnavailableReason =
  // The caller is in no team at all — there is no audience to name, and the
  // server refuses the share for exactly that reason.
  | "no-team"
  // The chat's project is shared with a team the caller is not in.
  | "other-team"
  // Personal (un-shared) project, and the caller owns it: they can fix it.
  | "personal-owner"
  // Personal project owned by someone else: only that someone can fix it.
  | "personal-member"
  // Personal project, and we do not know who is asking (no `userEmail`) — so
  // offer neither the owner's action nor the member's, and say nothing.
  | "personal-unknown"
  // The chat is in no project.
  | "no-project";

export function ShareDialog({
  conversation,
  project,
  myTeam,
  userEmail,
  isAdmin,
  teamSharedProjects,
  busy,
  copied,
  error,
  buildShareUrl,
  onCreateLink,
  onCopyLink,
  onStopLink,
  onSetTeamShared,
  onMoveToProject,
  onOpenProjectSettings,
  onOpenProjects,
  onClose,
}: {
  // null when the conversation vanished under the dialog (deleted in another
  // tab): the dialog then says so instead of rendering controls that would
  // act on nothing.
  conversation: ConversationSummary | null;
  // The chat's project, when it is in one — decides whether team sharing is
  // available and phrases the helper text when it is not.
  project: Project | null;
  myTeam?: string;
  // The signed-in user, used only to tell a project's OWNER from a member: the
  // personal-project fix is "share it" for one and "ask them" for the other.
  // undefined = unknown, and then neither is claimed.
  userEmail?: string;
  // Whether the caller is a fleet admin. Only consulted in the no-team state,
  // where the pointer differs by role (most fleet users are admins; telling an
  // admin to go ask an admin is worse than useless). undefined = unknown → a
  // neutral pointer that names both surfaces.
  isAdmin?: boolean;
  // The team-shared projects the caller can file this chat into. undefined =
  // the list was never passed, which is NOT the same as empty — the dialog
  // stays silent rather than asserting there are none.
  teamSharedProjects?: Project[];
  busy: boolean;
  copied: boolean;
  // The last failure or refusal from a share action, shown inside the dialog.
  // The server's `409` names a precondition the reader can act on (no team, no
  // team-shared home — ADR-0057); a toast behind this modal is not where that
  // sentence belongs.
  error?: string | null;
  buildShareUrl: (token: string) => string;
  onCreateLink: (conversation: ConversationSummary) => void;
  onCopyLink: (url: string) => void;
  onStopLink: (conversation: ConversationSummary) => void;
  onSetTeamShared: (
    conversation: ConversationSummary,
    visible: boolean,
  ) => void;
  // Re-file this chat into a project — the same (conversationId, projectID)
  // move the rail's kebab performs. Optimistic upstream, which is what lets
  // the toggle go live without closing this dialog.
  onMoveToProject?: (conversationId: string, projectID: string) => void;
  onOpenProjectSettings: (projectID: string) => void;
  // Open the Projects surface — offered when the caller has a team but no
  // team-shared project to put this chat in.
  onOpenProjects?: () => void;
  onClose: () => void;
}) {
  const token = conversation?.share_token ?? "";
  const teamShared = Boolean(conversation?.team_visible);
  const projectIsTeamShared = Boolean(project?.team_id);
  const teamName = project?.team_id || myTeam || "your team";
  // Only a chat inside a project shared WITH YOUR TEAM can be team-shared, and
  // the server enforces exactly that (ADR-0057) — so the gate here has to
  // match it or the dialog offers a control the API refuses. `myTeam ===
  // undefined` means the team hasn't been read yet; don't disable on unknown.
  const inMyTeamsProject =
    projectIsTeamShared && (myTeam === undefined || project?.team_id === myTeam);
  // Un-sharing is always available. A chat can be in a state the share rules
  // no longer allow (its owner left the team, the project was re-shared
  // elsewhere), and that is precisely when its owner most needs the checkbox
  // to work — the server never refuses a revoke either.
  const canTeamShare = inMyTeamsProject || teamShared;
  // "" is a READ empty team (the caller has none); undefined is an unread one.
  const callerHasNoTeam = myTeam === "";
  const iOwnProject =
    userEmail && project?.owner_email
      ? project.owner_email === userEmail
      : undefined;

  const unavailable: UnavailableReason | null = canTeamShare
    ? null
    : callerHasNoTeam
      ? "no-team"
      : projectIsTeamShared
        ? "other-team"
        : project
          ? iOwnProject === undefined
            ? "personal-unknown"
            : iOwnProject
              ? "personal-owner"
              : "personal-member"
          : "no-project";

  // Move targets: team-shared projects in the caller's OWN team (the server
  // pairs the chat's project against the caller's team, so another team's
  // project is not a fix), minus the one the chat is already in.
  const moveTargets =
    teamSharedProjects === undefined
      ? undefined
      : teamSharedProjects.filter(
          (p) =>
            Boolean(p.team_id) &&
            (myTeam ? p.team_id === myTeam : true) &&
            p.id !== project?.id,
        );

  const teamCheckboxId = useId();

  const toggleUnavailable = busy || Boolean(unavailable);

  return (
    <DialogShell
      label="Share this chat"
      scrimLabel="Close share dialog"
      onDismiss={onClose}
      className="max-w-[28rem] p-5"
    >
      <div className="mb-4 flex items-start justify-between gap-3">
        <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">
          Share {conversation ? `“${conversation.title}”` : "this chat"}
        </h2>
        <CloseButton label="Close share dialog" onClick={onClose} />
      </div>

      {!conversation ? (
        <p className="text-[0.875rem] text-[var(--color-text-secondary)]">
          This chat is no longer available.
        </p>
      ) : (
        <>
          {/* The server's own sentence when it refuses (409) — the reason is
              actionable, so it belongs in front of the control that was
              refused, not in a toast behind the dialog. */}
          {error ? (
            <p
              role="alert"
              className="mb-3 rounded-[0.6rem] border border-[var(--color-danger-border)] px-2.5 py-1.5 text-[0.78rem] leading-[1.55] text-[var(--color-danger)]"
            >
              {error}
            </p>
          ) : null}

          {/* ── Scope 1: the team ───────────────────────────────────── */}
          <section className="mb-4 rounded-[0.9rem] border border-[var(--color-border)] p-3">
            {/* The helper text sits OUTSIDE the <label>: it holds a button
                ("project settings"), and an interactive element inside a
                label activates that label's control when clicked. */}
            <div className="flex items-start gap-2.5">
              {/* A disabled input swallows pointer events in most browsers,
                  so the not-allowed cursor has to live on a wrapper. */}
              <span
                className={`mt-0.5 inline-flex ${toggleUnavailable ? "cursor-not-allowed" : ""}`}
              >
                <input
                  id={teamCheckboxId}
                  type="checkbox"
                  className={toggleUnavailable ? CONTROL_UNAVAILABLE : ""}
                  checked={teamShared}
                  disabled={toggleUnavailable}
                  aria-disabled={toggleUnavailable || undefined}
                  aria-label={`Share with ${teamName}`}
                  onChange={(e) => {
                    // `disabled` is the real treatment — a browser will not
                    // dispatch this at all. The guard is the belt to that
                    // braces: a synthetic click (jsdom, an extension, a
                    // future styled control) must not reach the server with
                    // a request ADR-0057 says it will refuse.
                    if (toggleUnavailable) return;
                    onSetTeamShared(conversation, e.target.checked);
                  }}
                />
              </span>
              <div className="min-w-0 flex-1">
                <label
                  htmlFor={teamCheckboxId}
                  className={`flex items-center gap-1.5 text-[0.875rem] font-medium text-[var(--color-text-primary)] ${toggleUnavailable ? CONTROL_UNAVAILABLE : ""}`}
                >
                  <TeamGlyph className="size-3.5 shrink-0" />
                  Share with team{project?.team_id ? ` (${project.team_id})` : ""}
                </label>
                {/* The copy stays at full contrast in every state: the
                    control is what is unavailable, the explanation of why is
                    the one thing the reader needs to be able to read. */}
                <p className="mt-1 block text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
                  {inMyTeamsProject ? (
                    teamShared ? (
                      <>
                        Your team can read this chat from{" "}
                        <strong className="font-medium">{project?.name}</strong>
                        ’s home page and branch it to build on it. They
                        can&rsquo;t change it, and files in the chat&rsquo;s
                        workspace are not shared.
                      </>
                    ) : (
                      <>
                        Teammates get a read-only view on{" "}
                        <strong className="font-medium">{project?.name}</strong>
                        ’s home page. Revocable any time.
                      </>
                    )
                  ) : teamShared && project ? (
                    // Shared, but the pairing has since broken. Say so, and
                    // leave the checkbox live so it can be taken back.
                    <>
                      This chat is still shared with{" "}
                      <strong className="font-medium">{teamName}</strong>, but{" "}
                      <strong className="font-medium">{project.name}</strong>{" "}
                      is no longer shared with your team. Un-tick to stop
                      sharing it.
                    </>
                  ) : projectIsTeamShared && project ? (
                    <>
                      <strong className="font-medium">{project.name}</strong>{" "}
                      is shared with{" "}
                      <strong className="font-medium">{project.team_id}</strong>
                      , which you aren&rsquo;t in — so you can&rsquo;t share a
                      chat into it.
                    </>
                  ) : project ? (
                    <>
                      <strong className="font-medium">{project.name}</strong>{" "}
                      isn&rsquo;t shared with your team. Share the project
                      first
                      {/* The inline link is the OWNER's fix. Withhold it from
                          someone we know is not the owner — project settings
                          are owner-only, so it would point a member at a
                          door that is locked for them; they get "ask the
                          owner" below instead. */}
                      {project.owner_email && iOwnProject !== false ? (
                        <>
                          {" "}
                          —{" "}
                          <button
                            type="button"
                            className="underline hover:text-[var(--color-text-primary)]"
                            onClick={() => onOpenProjectSettings(project.id)}
                          >
                            project settings
                          </button>
                        </>
                      ) : null}
                      .
                    </>
                  ) : (
                    "Move this chat into a team-shared project to share it with your team."
                  )}
                </p>
                {unavailable ? (
                  <TeamShareFix
                    reason={unavailable}
                    conversation={conversation}
                    project={project}
                    moveTargets={moveTargets}
                    isAdmin={isAdmin}
                    busy={busy}
                    onMoveToProject={onMoveToProject}
                    onOpenProjectSettings={onOpenProjectSettings}
                    onOpenProjects={onOpenProjects}
                  />
                ) : null}
              </div>
            </div>
          </section>

          {/* ── Scope 2: a public link ──────────────────────────────── */}
          <section className="mb-4 rounded-[0.9rem] border border-[var(--color-border)] p-3">
            <p className="m-0 flex items-center gap-1.5 text-[0.875rem] font-medium text-[var(--color-text-primary)]">
              <ShareGlyph className="size-3.5 shrink-0" />
              Share by link
            </p>
            {token ? (
              <>
                <p className="mb-2 mt-1 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
                  <strong className="font-medium">
                    Anyone with this link
                  </strong>{" "}
                  can view a read-only copy — including people outside your
                  team.
                </p>
                <div className="flex items-center gap-2">
                  <input
                    readOnly
                    aria-label="Share link URL"
                    value={buildShareUrl(token)}
                    onFocus={(e) => e.currentTarget.select()}
                    className="min-w-0 flex-1 rounded-lg border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 font-mono text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                  />
                  <button
                    type="button"
                    onClick={() => onCopyLink(buildShareUrl(token))}
                    className="shrink-0 rounded-full border border-[var(--color-accent)] px-3.5 py-1.5 text-[0.8125rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-accent)] hover:text-[var(--color-surface-1)]"
                  >
                    {copied ? "Copied ✓" : "Copy link"}
                  </button>
                </div>
                <button
                  type="button"
                  onClick={() => onStopLink(conversation)}
                  className="mt-2 rounded-full border border-[var(--color-danger-border)] px-3 py-1 text-[0.78rem] font-medium text-[var(--color-danger)] transition hover:bg-[var(--color-overlay-soft)]"
                >
                  Stop sharing the link
                </button>
              </>
            ) : (
              <>
                <p className="mb-2 mt-1 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
                  Creates a URL anyone can open — read-only, and not limited
                  to your team. Revocable any time.
                </p>
                <button
                  type="button"
                  onClick={() => onCreateLink(conversation)}
                  className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                >
                  Create link
                </button>
              </>
            )}
          </section>

          <div className="flex justify-end">
            <button
              type="button"
              onClick={onClose}
              className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            >
              Done
            </button>
          </div>
        </>
      )}
    </DialogShell>
  );
}

// TeamShareFix is the "offer the fix, don't just describe it" half of the team
// section: for each unavailable state, the one action that resolves it.
//
// It renders NOTHING when it cannot honestly offer anything — an unread
// project list, or a personal project whose owner we cannot identify. Silence
// there leaves the adaptive sentence above as the whole answer, which is what
// the dialog did before and is still true.
function TeamShareFix({
  reason,
  conversation,
  project,
  moveTargets,
  isAdmin,
  busy,
  onMoveToProject,
  onOpenProjectSettings,
  onOpenProjects,
}: {
  reason: UnavailableReason;
  conversation: ConversationSummary;
  project: Project | null;
  moveTargets: Project[] | undefined;
  isAdmin?: boolean;
  busy: boolean;
  onMoveToProject?: (conversationId: string, projectID: string) => void;
  onOpenProjectSettings: (projectID: string) => void;
  onOpenProjects?: () => void;
}) {
  const line = "mt-2 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]";
  const linkish =
    "underline hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-[var(--color-accent)]";

  if (reason === "no-team") {
    // Verbatim the Projects modal's pointer, for the same reason it branches:
    // most fleet users are admins, and sending one of them to ask someone else
    // would be worse than useless.
    return (
      <p className={line}>
        {isAdmin === undefined
          ? "You’re not on a team yet. Teams are managed in Settings → Team, or by an admin in Settings → Admin → Users."
          : isAdmin
            ? "You’re not on a team yet. Add yourself to one in Settings → Admin → Users, or create one in Settings → Team."
            : "You’re not on a team yet. Ask an admin to add you in Settings → Admin → Users."}
      </p>
    );
  }

  if (reason === "personal-unknown") return null;

  if (reason === "personal-owner" && project) {
    return (
      <button
        type="button"
        onClick={() => onOpenProjectSettings(project.id)}
        className="mt-2 rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.78rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
      >
        Share this project with your team
      </button>
    );
  }

  if (reason === "personal-member" && project) {
    return (
      <p className={line}>
        Ask{" "}
        <strong className="font-medium">
          {project.owner_email || "the project’s owner"}
        </strong>{" "}
        to share this project with your team.
      </p>
    );
  }

  // "no-project" and "other-team": the fix is the same — put this chat in one
  // of the caller's OWN team-shared projects.
  if (moveTargets === undefined) return null;

  if (moveTargets.length === 0) {
    return (
      <p className={line}>
        Share a project with your team first
        {onOpenProjects ? (
          <>
            {" "}
            —{" "}
            <button type="button" className={linkish} onClick={onOpenProjects}>
              Projects
            </button>
          </>
        ) : null}
        .
      </p>
    );
  }

  if (!onMoveToProject) return null;

  return (
    <label className="mt-2 flex flex-wrap items-center gap-2 text-[0.78rem] text-[var(--color-text-secondary)]">
      <span>Move to project</span>
      {/* Uncontrolled-looking on purpose: the value snaps back to the
          placeholder because the move re-renders this section as the ENABLED
          toggle — the select has done its job and is gone. */}
      <select
        aria-label="Move to project"
        className="min-w-0 max-w-[14rem] flex-1 truncate rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
        value=""
        disabled={busy}
        onChange={(e) => {
          const id = e.target.value;
          if (id) onMoveToProject(conversation.id, id);
        }}
      >
        <option value="">Choose a team-shared project…</option>
        {moveTargets.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
            {p.team_id ? ` (${p.team_id})` : ""}
          </option>
        ))}
      </select>
    </label>
  );
}
