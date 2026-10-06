"use client";

// The two banners that tie a teammate's branch to the chat it came from
// (docs/TEAM-SHARING.md, frames B20 and B21).
//
//   BranchOriginBanner — in the BRANCH (the teammate's own chat): where it came
//   from, and that the shared files are now the teammate's own copies. It links
//   back to the owner's live chat only while that chat is still shared; after
//   that the link would open a "not shared anymore" page, so it is not offered.
//
//   ViewerBranchBanner — in the OWNER's live chat, seen by a teammate who has
//   branched it before: a way to their branch, and whether the owner has added
//   messages since. "Since" compares the chat's last update with the branch's
//   creation time; there is no per-person read state behind it.

import type { BranchOrigin, ViewerBranch } from "./teamSharing";
import { formatDay, ownerFirstName } from "./teamSharing";

function BranchGlyph({ className }: { className?: string }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="6" cy="5" r="2" />
      <circle cx="6" cy="19" r="2" />
      <circle cx="18" cy="8" r="2" />
      <path d="M6 7v10" />
      <path d="M18 10c0 4-6 3-11.2 7.4" />
    </svg>
  );
}

const linkButton =
  "rounded-[var(--radius-md)] p-0 font-medium whitespace-nowrap text-[var(--color-accent)] underline underline-offset-2 transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

export function BranchOriginBanner({
  origin,
  onOpenSource,
}: {
  origin: BranchOrigin;
  onOpenSource: (conversationId: string) => void;
}) {
  return (
    <div
      data-testid="branch-origin-banner"
      className="flex items-center gap-2 rounded-[0.625rem] border border-[var(--color-border)] px-3 py-2 text-[0.78rem] text-[var(--color-text-secondary)]"
    >
      <BranchGlyph className="size-3.5 shrink-0 text-[var(--color-accent)]" />
      <span className="min-w-0 flex-1">
        Branched from {origin.source_owner_email}’s chat on{" "}
        {formatDay(origin.branched_at)}. Shared files came with it as
        your own copies.
      </span>
      {origin.source_still_shared ? (
        <button
          type="button"
          className={linkButton}
          onClick={() => onOpenSource(origin.source_conversation_id)}
        >
          Open {ownerFirstName(origin.source_owner_email)}’s chat
        </button>
      ) : null}
    </div>
  );
}

export function ViewerBranchBanner({
  branch,
  ownerEmail,
  onOpenBranch,
}: {
  branch: ViewerBranch;
  ownerEmail: string;
  onOpenBranch: (conversationId: string) => void;
}) {
  return (
    <div
      role="status"
      data-testid="viewer-branch-banner"
      className="flex flex-wrap items-center gap-2 rounded-[0.625rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-text-secondary)]"
    >
      <BranchGlyph className="size-3.5 shrink-0 text-[var(--color-accent)]" />
      <span>You branched this on {formatDay(branch.branched_at)}</span>
      <span aria-hidden="true" className="text-[var(--color-text-muted)]">
        ·
      </span>
      <button
        type="button"
        className={linkButton}
        onClick={() => onOpenBranch(branch.conversation_id)}
      >
        Open your branch
      </button>
      {branch.changed_since ? (
        <span className="basis-full pl-[1.375rem] text-[var(--color-text-primary)]">
          {ownerFirstName(ownerEmail)} has added messages since you branched.
        </span>
      ) : null}
    </div>
  );
}
