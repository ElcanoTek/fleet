"use client";

// Locked file names (B19, B20): a file a reader can see MENTIONED but cannot
// open renders as its name with a lock glyph and "(not shared)", never as a
// link or an image that would only ever 404.
//
// Two places produce one:
//
//   - The teammate's read-only view. linkSharedFiles (workspaceHref.ts)
//     rewrites every unshared reference to `[name (not shared)](#fleet-file-
//     not-shared)`; the assistant renderer sees that sentinel and draws a
//     LockedFileLabel instead of an anchor.
//   - A teammate's BRANCH. Only the files shared at branch time were copied
//     into it, so the transcript still names outputs the branch does not have
//     (branch_origin.withheld_files). The branch chat renders through the
//     ordinary transcript, so those are recognised by path, through
//     WithheldFilesContext, rather than by rewriting its markdown.
//
// No provider (every other chat) means nothing is locked: the renderer behaves
// exactly as before.

import { createContext, useContext, type ReactNode } from "react";
import { LockGlyph } from "./ShareGlyphs";
import { workspacePathFromHref } from "./OutputShareMarkers";
import { LOCKED_SUFFIX } from "./workspaceHref";

export type WithheldFiles = {
  conversationId: string;
  /** Workspace-relative paths the branch transcript names but does not have. */
  withheld: ReadonlySet<string>;
};

export const WithheldFilesContext = createContext<WithheldFiles | null>(null);

/** The lock glyph + muted name a locked reference renders as. */
export function LockedFileLabel({ children }: { children: ReactNode }) {
  return (
    <span
      data-testid="locked-file"
      className="inline-flex items-baseline gap-1 text-[var(--color-text-muted)]"
    >
      <LockGlyph className="size-3 shrink-0 translate-y-[1px] self-center" />
      <span>{children}</span>
    </span>
  );
}

/**
 * WithheldFileGate renders `children` (the live link or image) unless the
 * resolved workspace href names a file the branch did not receive — then the
 * locked name. Reads the context itself so the memoised markdown components
 * keep their identity.
 */
export function WithheldFileGate({
  href,
  name,
  children,
}: {
  href: string;
  name: string;
  children: ReactNode;
}) {
  const ctx = useContext(WithheldFilesContext);
  if (!ctx || ctx.withheld.size === 0) return <>{children}</>;
  const path = workspacePathFromHref(href, ctx.conversationId);
  if (path === null || !ctx.withheld.has(path)) return <>{children}</>;
  return (
    <LockedFileLabel>
      {(path.split("/").pop() || name) + LOCKED_SUFFIX}
    </LockedFileLabel>
  );
}
