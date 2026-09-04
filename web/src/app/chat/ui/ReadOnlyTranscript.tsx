"use client";

// The ONE read-only transcript renderer, shared by the two doors onto a
// conversation the reader does not own: the public share link
// (`/shared/[token]`, #226) and a teammate's team-shared chat
// (TeamChatViewer, ADR-0057).
//
// Both docs pages say the two use "the same renderer", and for a while that
// was only true of the markdown pipeline underneath: `toBubbles` and the
// bubble markup were copied between them, which is a guarantee that lasts
// exactly until someone edits one. They are the same code now.
//
// What both show is the TRANSCRIPT — user and assistant text only. Tool calls,
// tool results and reasoning are filtered out server-side before either
// snapshot is built (their content can carry command output and API responses
// that were never part of what was shared); this filter is the client-side
// backstop for the same rule.
//
// The transcript-only rule has a second half the server cannot enforce, and
// this is where it lands: attachments and generated files stay behind the
// owner-scoped workspace route, so every link and image the transcript points
// at one is a 404 for BOTH readers — a teammate has no access to another
// member's workspace, and a share-link reader has no session at all. Rendering
// those as live links promised a download that could never arrive, so they
// render as plain text naming the file instead (redactUnsharedFiles). Not a
// disabled link: a disabled link is still a dead promise.

import type { ReactNode } from "react";
import { redactUnsharedFiles } from "./workspaceHref";

export type RawEntry = {
  // Present on the team-shared snapshot, which keeps persisted ids so a reader
  // can name a branch point. The public snapshot zeroes them.
  id?: number;
  role: string;
  type: string;
  content: unknown;
};

export type Bubble = {
  role: "user" | "assistant";
  text: string;
  // The last merged entry's persisted id — the branch point a reader forks
  // from. Undefined when the snapshot carries no ids.
  lastId?: number;
};

// toBubbles flattens stored history entries into a clean user/assistant text
// thread, merging consecutive same-role text (an assistant reply can land as
// several text entries within one turn).
export function toBubbles(entries: RawEntry[]): Bubble[] {
  const out: Bubble[] = [];
  for (const e of entries ?? []) {
    if (e.type !== "text" || (e.role !== "user" && e.role !== "assistant")) continue;
    const text = String((e.content as { text?: string } | null)?.text ?? "");
    if (!text) continue;
    const last = out[out.length - 1];
    if (last && last.role === e.role) {
      last.text += text;
      if (e.id) last.lastId = e.id;
    } else {
      out.push({ role: e.role, text, lastId: e.id });
    }
  }
  return out;
}

// Who is reading. The withholding is identical for both doors — neither can
// fetch the owner's files — but the copy names the reader it actually has,
// because "team views" is not what someone holding a share link is looking at.
// Required, not defaulted: a new door has to say who it lets in.
export type ReadOnlyAudience = "team" | "link";

const IMAGE_NOT_SHARED: Record<ReadOnlyAudience, string> = {
  team: "Image not shared with team views.",
  link: "Image not shared with view-only links.",
};

// ReadOnlyTranscript renders the bubbles.
//
// `renderAssistant` is a parameter rather than a fixed import because the two
// callers load the markdown pipeline differently ON PURPOSE: the team viewer
// lazy-loads it (it lives inside the chat bundle, where ~43 KiB of
// react-markdown + micromark is worth deferring), while the standalone public
// share page renders it directly. That is the one real difference between
// them; everything else — the flattening, the bubble markup — is shared here
// so it cannot drift.
//
// `actions` lets a caller hang per-message controls under an assistant reply
// (the team viewer's Copy button); the public view passes nothing, because a
// page anyone with a URL can open should offer no affordances of its own.
export function ReadOnlyTranscript({
  bubbles,
  audience,
  renderAssistant,
  actions,
}: {
  bubbles: Bubble[];
  audience: ReadOnlyAudience;
  renderAssistant: (text: string) => ReactNode;
  actions?: (bubble: Bubble) => ReactNode;
}) {
  const imagePlaceholder = IMAGE_NOT_SHARED[audience];
  return (
    <div className="flex flex-col gap-5">
      {bubbles.map((b, i) =>
        b.role === "user" ? (
          <div key={i} className="flex justify-end">
            <div className="max-w-[85%] whitespace-pre-wrap rounded-[1rem] bg-[var(--color-overlay-strong)] px-4 py-2.5 text-[0.9375rem] leading-[1.55]">
              {b.text}
            </div>
          </div>
        ) : (
          <div
            key={i}
            className="assistant-markdown max-w-full text-[0.9375rem] leading-[1.6]"
          >
            {/* Only what is RENDERED is redacted: `actions` still gets the
                bubble the snapshot carried, so the team viewer's Copy hands
                over the owner's text as written rather than a paraphrase of
                it. The file is unreachable either way. */}
            {renderAssistant(redactUnsharedFiles(b.text, imagePlaceholder))}
            {actions ? (
              <div className="mt-2 flex items-center gap-3 text-[0.7rem]">
                {actions(b)}
              </div>
            ) : null}
          </div>
        ),
      )}
    </div>
  );
}
