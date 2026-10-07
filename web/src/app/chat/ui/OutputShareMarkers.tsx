"use client";

// Owner's chat, B17 (#40): in a chat the owner has shared with the team, each
// output chip (a workspace file link in an assistant reply) carries a small
// "Shared" or "Not shared" marker. Clicking it opens the project home's
// Sources at this chat's group, where the share state is changed — there are
// no toggles on chips, so there is one place to change it.
//
// The markers come through a context, not a prop, because the transcript
// renderer (AssistantContent) is mounted several components deep and lazily.
// No provider, or a null value — a private chat, a teammate's read-only view,
// a public link — means no markers: those surfaces render exactly as before.
//
// Only paths the server lists as OUTPUTS get a marker (GET
// /conversations/{id}/outputs): uploads and files the agent never presented
// are not outputs, so a link to one stays a plain link.

import { createContext, useContext } from "react";
import { LockGlyph, TeamGlyph } from "./ShareGlyphs";
import { conversationWorkspaceUrl } from "@/app/lib/conversationApiUrl";

export type OutputShareMarkers = {
  conversationId: string;
  /** Output path → shared. */
  shared: ReadonlyMap<string, boolean>;
  team: string;
  onOpenSources: (path: string) => void;
};

export const OutputShareContext = createContext<OutputShareMarkers | null>(
  null,
);

export function useOutputShareMarkers(): OutputShareMarkers | null {
  return useContext(OutputShareContext);
}

/**
 * The workspace-relative path a resolved workspace href points at, or null
 * when it is not a workspace file of this conversation. The inverse of the
 * per-segment encoding resolveWorkspaceHref applies, so the result compares
 * equal to the server's OutputFile.path.
 */
export function workspacePathFromHref(
  resolvedHref: string,
  conversationId: string,
): string | null {
  const base = conversationWorkspaceUrl(conversationId);
  if (!base || !resolvedHref.startsWith(base)) return null;
  try {
    return resolvedHref
      .slice(base.length)
      .split("/")
      .filter((s) => s.length > 0)
      .map((s) => decodeURIComponent(s))
      .join("/");
  } catch {
    return null;
  }
}

export function OutputShareMarker({
  name,
  shared,
  team,
  onClick,
}: {
  name: string;
  shared: boolean;
  team: string;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      data-testid="output-share-marker"
      title="Manage in Sources"
      aria-label={
        shared
          ? `${name} is shared with ${team}. Manage in Sources`
          : `${name} is not shared. Manage in Sources`
      }
      onClick={onClick}
      className={`ml-1 inline-flex h-5 items-center gap-1 whitespace-nowrap rounded-full px-1.5 align-middle text-[0.6875rem] font-medium no-underline transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
        shared
          ? "text-[var(--color-accent)]"
          : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
      }`}
    >
      {shared ? (
        <TeamGlyph className="size-[0.7rem]" />
      ) : (
        <LockGlyph className="size-[0.65rem]" />
      )}
      <span>{shared ? "Shared" : "Not shared"}</span>
    </button>
  );
}
