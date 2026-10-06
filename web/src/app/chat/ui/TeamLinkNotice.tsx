"use client";

// The dead ends a team link can land on (docs/TEAM-SHARING.md, B22 and B23).
//
// A team link (`/chat?team=<id>`) opens the read-only view for a member of the
// chat's team. Everyone else gets one of these, and what they show is as much
// a security property as a design: NOTHING about the chat — no title, no
// transcript, no files — and, for someone outside the team, not even the
// project's name. The server decides which page applies (GET
// /conversations/{id}/team-link) and only sends what the page may show.
//
//   not_on_team (B22): the chat IS shared, with a team the viewer is not in.
//     Names the team (the link's audience, which its sender already knew) and
//     who the viewer is signed in as, so a person on the wrong account can
//     tell.
//   not_shared (B23): unshared, removed, archived, deleted — or never existed.
//     One sentence, plus "Open <project>" only when the viewer can still see
//     that project.

import { LockGlyph } from "./ShareGlyphs";

export type TeamLinkNoticeState =
  | { kind: "not_on_team"; team: string; viewerEmail: string }
  | { kind: "not_shared"; project?: { id: string; name: string } };

export function TeamLinkNotice({
  notice,
  onOpenProject,
}: {
  notice: TeamLinkNoticeState;
  onOpenProject: (projectId: string) => void;
}) {
  return (
    <div
      data-testid="team-link-notice"
      className="grid min-h-0 flex-1 place-items-center overflow-y-auto px-4 py-10"
    >
      <div className="flex w-full max-w-[25rem] flex-col items-center gap-3.5 text-center">
        <span className="grid size-12 place-items-center rounded-[var(--radius-lg)] bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]">
          <LockGlyph className="size-5" />
        </span>
        {notice.kind === "not_on_team" ? (
          <>
            <h1 className="text-[1.25rem] font-medium text-[var(--color-text-primary)]">
              This chat is shared with {notice.team}
            </h1>
            <p className="text-[0.875rem] leading-[1.5] text-[var(--color-text-secondary)]">
              Only {notice.team} members can open it. If you think you should
              have access, ask the person who sent you the link.
            </p>
            {notice.viewerEmail ? (
              <span className="text-[0.78rem] text-[var(--color-text-muted)]">
                Signed in as {notice.viewerEmail}
              </span>
            ) : null}
          </>
        ) : (
          <>
            <h1 className="text-[1.25rem] font-medium text-[var(--color-text-primary)]">
              This chat isn’t shared anymore
            </h1>
            <p className="text-[0.875rem] leading-[1.5] text-[var(--color-text-secondary)]">
              Ask the person who sent the link if you still need it.
            </p>
            {notice.project ? (
              <button
                type="button"
                onClick={() => onOpenProject(notice.project!.id)}
                className="inline-flex h-9 items-center rounded-full border border-[var(--color-border-strong)] px-4 text-[0.8125rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              >
                Open {notice.project.name}
              </button>
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}
