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

import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { CloseButton } from "@/app/shared/ui/CloseButton";

export function ShareDialog({
  conversation,
  project,
  myTeam,
  busy,
  copied,
  buildShareUrl,
  onCreateLink,
  onCopyLink,
  onStopLink,
  onSetTeamShared,
  onOpenProjectSettings,
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
  busy: boolean;
  copied: boolean;
  buildShareUrl: (token: string) => string;
  onCreateLink: (conversation: ConversationSummary) => void;
  onCopyLink: (url: string) => void;
  onStopLink: (conversation: ConversationSummary) => void;
  onSetTeamShared: (
    conversation: ConversationSummary,
    visible: boolean,
  ) => void;
  onOpenProjectSettings: (projectID: string) => void;
  onClose: () => void;
}) {
  const token = conversation?.share_token ?? "";
  const teamShared = Boolean(conversation?.team_visible);
  const projectIsTeamShared = Boolean(project?.team_id);
  const teamName = project?.team_id || myTeam || "your team";
  // Only a chat inside a team-shared project can be team-shared. Everything
  // else gets the toggle disabled and a sentence that fits its case.
  const canTeamShare = projectIsTeamShared;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <button
        aria-label="Close share dialog"
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onClose}
      />
      <div
        role="dialog"
        aria-label="Share this chat"
        className="motion-safe:animate-pop-up-base relative z-10 w-full max-w-[28rem] rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-md)]"
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
            {/* ── Scope 1: the team ───────────────────────────────────── */}
            <section className="mb-4 rounded-[0.9rem] border border-[var(--color-border)] p-3">
              <label className="flex items-start gap-2.5">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={teamShared}
                  disabled={busy || !canTeamShare}
                  aria-label={`Share with ${teamName}`}
                  onChange={(e) =>
                    onSetTeamShared(conversation, e.target.checked)
                  }
                />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-1.5 text-[0.875rem] font-medium text-[var(--color-text-primary)]">
                    <TeamGlyph className="size-3.5 shrink-0" />
                    Share with team{project?.team_id ? ` (${project.team_id})` : ""}
                  </span>
                  <span className="mt-1 block text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
                    {canTeamShare ? (
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
                    ) : project ? (
                      <>
                        <strong className="font-medium">{project.name}</strong>{" "}
                        isn&rsquo;t shared with your team. Share the project
                        first
                        {project.owner_email ? (
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
                  </span>
                </span>
              </label>
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
      </div>
    </div>
  );
}
