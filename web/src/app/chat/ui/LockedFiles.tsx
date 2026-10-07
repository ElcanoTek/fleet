"use client";

// Locked file names (B19, B20): a file a reader can see MENTIONED but cannot
// open renders as its name with a lock glyph and "(not shared)", never as a
// link or an image that would only ever 404.
//
// Two places produce one:
//
//   - The teammate's read-only view. ReadOnlyTranscript provides a
//     ReadOnlyFilesContext; the assistant renderer's `a`/`img` overrides
//     decide each RENDERED reference by its resolved workspace path
//     (decideReadOnlyFile) and draw a LockedFileLabel for an unshared one.
//   - A teammate's BRANCH. Only the files shared at branch time were copied
//     into it, so the transcript still names outputs the branch does not have
//     (branch_origin.withheld_files). The branch chat renders through the
//     ordinary transcript, so those are recognised by path, through
//     WithheldFilesContext, rather than by rewriting its markdown. The list is
//     bounded server-side; when it was truncated the context carries an
//     allow-list instead, and anything outside it is locked. Either way the
//     branch's OWN current outputs win: a withheld path the branch later
//     creates and presents is its file, and renders live.
//
// No provider (every other chat) means nothing is locked: the renderer behaves
// exactly as before.

import { createContext, useContext, type ReactNode } from "react";
import { LockGlyph } from "./ShareGlyphs";
import { workspacePathFromHref } from "./OutputShareMarkers";
import { LOCKED_SUFFIX, type ReadOnlyFilePolicy } from "./workspaceHref";

export type WithheldFiles = {
  conversationId: string;
  /** Workspace-relative paths the branch transcript names but does not have. */
  withheld: ReadonlySet<string>;
  /**
   * Set when the server's withheld list was truncated (branch_origin.
   * withheld_truncated): an ALLOW-list — the branch's copied files plus its
   * own current outputs. A workspace reference outside it renders locked,
   * because the bounded withheld list cannot vouch for it. null: the withheld
   * list is complete and everything not in it renders as before.
   */
  available?: ReadonlySet<string> | null;
  /**
   * The branch's own current outputs (GET /conversations/{branch}/outputs:
   * files its transcript presents that exist in ITS workspace). They override
   * `withheld`: a path withheld at branch time that the branch later created
   * and presented is the branch's own file, so it renders live. Undefined or
   * null until that read lands.
   */
  outputs?: ReadonlySet<string> | null;
};

/** Whether `path` names a file this branch cannot open. */
export function isWithheldPath(ctx: WithheldFiles, path: string): boolean {
  if (ctx.outputs?.has(path)) return false;
  if (ctx.withheld.has(path)) return true;
  return ctx.available ? !ctx.available.has(path) : false;
}

export const WithheldFilesContext = createContext<WithheldFiles | null>(null);

/**
 * The read-only transcript views' file policy (ReadOnlyTranscript provides
 * it): every workspace reference the markdown RENDERS as a link or image is
 * decided at render time by its resolved path — a shared output becomes a
 * team-files link, anything else a locked or withheld name. No provider (the
 * owner's own chats) means the renderer behaves exactly as before.
 */
export const ReadOnlyFilesContext = createContext<ReadOnlyFilePolicy | null>(null);

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
  if (!ctx || (ctx.withheld.size === 0 && !ctx.available)) return <>{children}</>;
  const path = workspacePathFromHref(href, ctx.conversationId);
  if (path === null || !isWithheldPath(ctx, path)) return <>{children}</>;
  return (
    <LockedFileLabel>
      {(path.split("/").pop() || name) + LOCKED_SUFFIX}
    </LockedFileLabel>
  );
}
