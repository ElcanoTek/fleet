"use client";

import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { useDialogDismiss } from "@/app/shared/ui/useDialogDismiss";
import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { Icon } from "./Icon";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { formatBytes, stripMarkdown } from "./formatters";

// Project home (#509 follow-up): the page a project's rail row opens — title,
// this member's chats in the project, the TEAM's shared chats beside them, a
// Sources panel (workspace files from those chats), and the two team-level
// context layers — Instructions and Team learnings — as a pair on the right.
// Also hosts the PER-PROJECT settings dialog (name / sharing / delete); the
// all-projects modal stays only for creation.
//
// The three context layers a project chat is fed by, named the way the UI now
// names them and in the order the prompt builder actually assembles them
// (internal/agent/prompt.go → buildSystemPrompt):
//
//   1. Instructions   — one field, owner-only, injected first.
//   2. Team learnings — the project's shared memory. Any member writes,
//                       approval-gated, every entry stamped with its author.
//   3. My memory      — the reader's own personal memories, everywhere.
//
// Layers 2 and 3 arrive in the same "User Memories" block, the project's
// entries tagged `[project]`. The helper copy under Instructions says exactly
// that, rather than naming two of the three.
//
// Privacy: "Chats" and Sources show the CALLER'S OWN conversations only — a
// team-shared project shares its definition, never a member's private chats.
// The Team section is the one exception and is doubly gated: a shared team
// AND the owner's explicit per-chat opt-in (ADR-0013 / ADR-0057).

type ProjectChatEntry = {
  id: string;
  title: string;
  updated_at: number;
  // Last text message's snippet ("You: …" when the user spoke last) — the
  // 1–2 line history under each chat title.
  preview?: string;
  // The owner's two share states, so a row can carry the right badge(s).
  share_token?: string;
  team_visible?: boolean;
};

// A teammate's chat in this project. user_email is the owner — the section is
// the one place a member sees whose work a chat is.
type TeamChatEntry = {
  id: string;
  title: string;
  user_email: string;
  updated_at: number;
};

type ProjectFileEntry = {
  conversation_id: string;
  conversation_title: string;
  path: string;
  name: string;
  size: number;
  modified_at: number;
};

// One team learning. user_email is the writer (provenance, recorded at write
// time); retired_at set = kept for the record but no longer injected.
type TeamLearning = {
  id: string;
  content: string;
  kind?: string;
  user_email?: string;
  pinned?: boolean;
  retired_at?: number | null;
  created_at?: number;
  updated_at?: number;
};

// What deleting the project would cost, from GET /api/projects/{id}/impact.
type DeleteImpact = {
  memories: number;
  chats: number;
  members: number;
  team_shared_chats: number;
};

function formatDay(unixSeconds: number): string {
  if (!unixSeconds) return "";
  try {
    return new Date(unixSeconds * 1000).toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  } catch {
    return "";
  }
}

// The local part of an email — enough to say whose chat this is without
// turning every row into an address.
function shortName(email: string): string {
  const at = email.indexOf("@");
  return at > 0 ? email.slice(0, at) : email;
}

const cardClass =
  "rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4";

export function ProjectHome({
  project,
  chats,
  userEmail,
  isOwner,
  initialSettingsOpen,
  onBack,
  onOpenChat,
  onOpenTeamChat,
  onNewChat,
  onSaveInstructions,
  onUpdateSettings,
  onTransfer,
  myTeam,
  onSettingsClosed,
  onDelete,
}: {
  project: Project;
  chats: ConversationSummary[];
  // The signed-in user — decides who may edit or retire a team learning
  // (its author, or the project owner) without a second round trip.
  userEmail: string;
  isOwner: boolean;
  // Open straight into the settings dialog (the rail kebab's
  // "Project settings…" path).
  initialSettingsOpen?: boolean;
  onBack: () => void;
  onOpenChat: (conversationId: string) => void;
  // Open a TEAMMATE's shared chat in the read-only viewer.
  onOpenTeamChat: (conversationId: string) => void;
  onNewChat: () => void;
  // Both mutations resolve true on success — the dialogs/cards keep their
  // draft (and show the parent's rail-error toast) on failure.
  onSaveInstructions: (instructions: string) => Promise<boolean>;
  onUpdateSettings: (patch: {
    name?: string;
    team_shared?: boolean;
  }) => Promise<boolean>;
  // Hand the project to another member (ADR-0057). Resolves true on success;
  // the dialog keeps its draft and shows the reason on failure.
  onTransfer: (toEmail: string) => Promise<string | null>;
  // The viewer's own team (#1157): "" = not in a team, so team sharing cannot
  // work yet and the dialog says where to fix that instead of letting the
  // toggle 400. undefined = not read yet — the copy stays neutral.
  myTeam?: string;
  // Called whenever the settings dialog opens or closes from inside, so the
  // parent's `settings` flag tracks it and a later request to open is a state
  // change rather than a no-op. Optional: the panel works standalone.
  onSettingsClosed?: (open: boolean) => void;
  onDelete: () => void;
}) {
  const [files, setFiles] = useState<ProjectFileEntry[] | null>(null);
  // A download that fails (a file the agent deleted since the listing, a
  // permission change) used to open a tab of raw server text. The Sources
  // list fetches instead, and reports failure here, in the app.
  const [fileError, setFileError] = useState<string | null>(null);
  // Server-side chat list with previews; the prop list (already in client
  // state) renders instantly while this loads, then the previews fill in.
  const [fetchedChats, setFetchedChats] = useState<ProjectChatEntry[] | null>(null);
  const [teamChats, setTeamChats] = useState<TeamChatEntry[] | null>(null);
  const [filesTruncated, setFilesTruncated] = useState(false);
  // Read at mount AND on every later transition to true. As mount-only state
  // this dialog opened at most once per project: the parent keeps ProjectHome
  // mounted (keyed on the project id) and only flips its `settings` flag, so
  // "Project settings…" from the rail kebab was dead after the first Cancel,
  // and dead outright whenever the home was already open. The parent is told
  // when it closes so its flag can fall back to false and the next open is a
  // real transition again.
  const [settingsOpen, setSettingsOpenState] = useState(
    Boolean(initialSettingsOpen),
  );
  const setSettingsOpen = useCallback(
    (open: boolean) => {
      setSettingsOpenState(open);
      onSettingsClosed?.(open);
    },
    [onSettingsClosed],
  );
  // Render-time reset (React's "adjust state when a prop changes" pattern, as
  // used for the instruction and name drafts above) rather than an effect: an
  // effect would render once with the stale value first.
  const [seenSettingsRequest, setSeenSettingsRequest] = useState(
    Boolean(initialSettingsOpen),
  );
  if (Boolean(initialSettingsOpen) !== seenSettingsRequest) {
    setSeenSettingsRequest(Boolean(initialSettingsOpen));
    if (initialSettingsOpen) setSettingsOpenState(true);
  }
  const [confirmDelete, setConfirmDelete] = useState(false);
  // Escape closes the settings dialog — but only while the delete confirm
  // ISN'T stacked on top of it, so one press never dismisses both.
  const closeSettings = useCallback(() => setSettingsOpen(false), [setSettingsOpen]);
  useDialogDismiss(settingsOpen && !confirmDelete, closeSettings);
  // Search over both chat lists (Item E1). Client-side over lists already in
  // memory: a project's chats are bounded by what one member filed there, and
  // the point is finding a chat you know is here, fast.
  const [query, setQuery] = useState("");
  const searchInputId = useId();

  // Instructions draft — resets when the saved value changes (React's
  // "reset state when a prop changes" render-time pattern, same as the rail's
  // rename-nonce; project switches remount via the parent's key=).
  const [draft, setDraft] = useState(project.instructions ?? "");
  const [savedInstructions, setSavedInstructions] = useState(
    project.instructions ?? "",
  );
  const [savingInstructions, setSavingInstructions] = useState(false);
  if ((project.instructions ?? "") !== savedInstructions) {
    setSavedInstructions(project.instructions ?? "");
    setDraft(project.instructions ?? "");
  }
  const dirty = draft !== savedInstructions;

  // Settings dialog draft — same render-time reset when the saved values
  // change (e.g. after a successful PATCH refreshes the projects list).
  const [nameDraft, setNameDraft] = useState(project.name);
  // Stable, collision-free id so the visible "Name" caption is a real
  // <label htmlFor> for the name field (clicking it focuses the input)
  // instead of unassociated text sitting above it.
  const projectNameInputId = useId();
  const [sharedDraft, setSharedDraft] = useState(Boolean(project.team_id));
  const [savingSettings, setSavingSettings] = useState(false);
  const settingsKey = `${project.name}\u0000${project.team_id ?? ""}`;
  const [seenSettingsKey, setSeenSettingsKey] = useState(settingsKey);
  if (settingsKey !== seenSettingsKey) {
    setSeenSettingsKey(settingsKey);
    setNameDraft(project.name);
    setSharedDraft(Boolean(project.team_id));
  }

  // Chat list with previews — best-effort; failure keeps the prop list.
  //
  // Re-runs when the LIVE list for this project changes (a chat dragged in
  // from the rail, one moved out, a share toggled), not only on project.id.
  // Fetched once, this panel showed a snapshot for as long as it stayed open:
  // file a chat into the project you are looking at and it simply did not
  // appear until you left and came back.
  const liveKey = chats
    .map((c) => `${c.id}:${c.title}:${c.team_visible ? 1 : 0}:${c.share_token ? 1 : 0}`)
    .join("|");
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/conversations`,
            { cache: "no-store" },
          );
          if (!res.ok) return;
          const data = (await res.json()) as {
            conversations?: ProjectChatEntry[];
          };
          if (!cancelled) setFetchedChats(data.conversations ?? null);
        } catch {
          // keep the prop list
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id, liveKey]);

  // The Team section (Item C3). Only a team-shared project can have one — a
  // personal project's chats cannot be team-shared at all — so the fetch is
  // skipped rather than asking for a list that is structurally empty.
  const teamShared = Boolean(project.team_id);
  useEffect(() => {
    if (!teamShared) return;
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/team-conversations`,
            { cache: "no-store" },
          );
          if (!res.ok) {
            if (!cancelled) setTeamChats([]);
            return;
          }
          const data = (await res.json()) as { conversations?: TeamChatEntry[] };
          if (!cancelled) setTeamChats(data.conversations ?? []);
        } catch {
          if (!cancelled) setTeamChats([]);
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id, teamShared]);

  // Sources — fetched per open. A FAILURE is reported, not rendered as an
  // empty state: "this project has no files" and "we could not ask" look
  // identical to a reader and only one of them is true.
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/files`,
            {
              cache: "no-store",
            },
          );
          if (!res.ok) {
            if (!cancelled) {
              setFiles([]);
              setFileError(`Couldn’t load this project’s files (HTTP ${res.status}).`);
            }
            return;
          }
          const data = (await res.json()) as {
            files?: ProjectFileEntry[];
            truncated?: boolean;
          };
          if (!cancelled) {
            setFiles(data.files ?? []);
            setFileError(null);
            setFilesTruncated(Boolean(data.truncated));
          }
        } catch {
          if (!cancelled) {
            setFiles([]);
            setFileError("Couldn’t reach the server to list this project’s files.");
          }
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id]);

  const saveInstructions = async () => {
    if (savingInstructions || !dirty) return;
    setSavingInstructions(true);
    const ok = await onSaveInstructions(draft);
    setSavingInstructions(false);
    if (ok) setSavedInstructions(draft);
  };

  const [settingsError, setSettingsError] = useState<string | null>(null);
  const saveSettings = async () => {
    if (savingSettings) return;
    setSettingsError(null);
    // An empty name used to produce an empty patch, so the dialog closed with
    // the rename silently discarded.
    if (!nameDraft.trim()) {
      setSettingsError("A project needs a name.");
      return;
    }
    const patch: { name?: string; team_shared?: boolean } = {};
    if (nameDraft.trim() !== project.name) patch.name = nameDraft.trim();
    if (sharedDraft !== Boolean(project.team_id))
      patch.team_shared = sharedDraft;
    if (Object.keys(patch).length === 0) {
      setSettingsOpen(false);
      return;
    }
    setSavingSettings(true);
    const ok = await onUpdateSettings(patch);
    setSavingSettings(false);
    if (ok) setSettingsOpen(false);
  };

  // Fetched list (with previews) once it lands; the prop list until then.
  const chatList: ProjectChatEntry[] = fetchedChats ?? chats;

  const q = query.trim().toLowerCase();
  const visibleChats = useMemo(
    () =>
      q
        ? chatList.filter(
            (c) =>
              (c.title ?? "").toLowerCase().includes(q) ||
              (c.preview ?? "").toLowerCase().includes(q),
          )
        : chatList,
    [chatList, q],
  );
  const visibleTeamChats = useMemo(
    () =>
      q
        ? (teamChats ?? []).filter(
            (c) =>
              (c.title ?? "").toLowerCase().includes(q) ||
              c.user_email.toLowerCase().includes(q),
          )
        : (teamChats ?? []),
    [teamChats, q],
  );
  const searchable = chatList.length + (teamChats?.length ?? 0) > 0;

  // A file download goes through fetch so a failure lands as an in-app error
  // instead of a tab full of server text (Item B1's secondary fix). The blob
  // URL is revoked on the next tick — long enough for the click to be taken.
  const downloadFile = async (f: ProjectFileEntry) => {
    setFileError(null);
    const href = `/api/conversations/${encodeURIComponent(f.conversation_id)}/workspace/${f.path
      .split("/")
      .map(encodeURIComponent)
      .join("/")}`;
    try {
      const res = await fetch(href, { cache: "no-store" });
      if (!res.ok) {
        setFileError(`Couldn’t open “${f.name}” — it may have been removed since this list loaded.`);
        return;
      }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = f.name;
      a.rel = "noreferrer noopener";
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 0);
    } catch {
      setFileError(`Couldn’t open “${f.name}” — the download failed.`);
    }
  };

  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4 sm:px-8"
      data-testid="project-home"
    >
      <div className="mx-auto max-w-5xl">
        {/* Header: back · title (+pin) · settings */}
        <div className="mb-4 flex items-center gap-3">
          <button
            type="button"
            aria-label="Back to chat"
            className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={onBack}
          >
            <Icon name="arrow-right" className="size-4 rotate-180" />
          </button>
          <Icon
            name="briefcase"
            className="size-5 shrink-0 text-[var(--color-accent)]"
          />
          <h1 className="min-w-0 flex-1 truncate text-[1.35rem] font-semibold text-[var(--color-text-primary)]">
            {project.name}
          </h1>
          {project.pinned ? (
            <Icon
              name="pin"
              className="size-4 shrink-0 text-[var(--color-accent)]"
            />
          ) : null}
          {project.team_id ? (
            <span
              title={`Shared with ${project.team_id}`}
              className="inline-flex shrink-0 items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)]"
            >
              <TeamGlyph className="size-3" />
              Shared with team
            </span>
          ) : null}
          {isOwner ? (
            <button
              type="button"
              aria-label="Project settings"
              title="Project settings"
              className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={() => setSettingsOpen(true)}
            >
              <Icon name="settings" className="size-4" />
            </button>
          ) : null}
        </div>

        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
          {/* Main column: new chat + this member's chats + the team's. */}
          <div className="min-w-0">
            <button
              type="button"
              className="mb-4 flex w-full items-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-4 py-3 text-left text-[0.9rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={onNewChat}
            >
              <Icon name="plus" className="size-4 shrink-0" />
              New chat in {project.name}
            </button>

            {searchable ? (
              <div className="mb-3 flex items-center gap-2 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5">
                <Icon
                  name="search"
                  className="size-3.5 shrink-0 text-[var(--color-text-muted)]"
                />
                <input
                  id={searchInputId}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Search chats in this project…"
                  aria-label="Search chats in this project"
                  className="min-w-0 flex-1 bg-transparent text-[0.83rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)]"
                />
                {query ? (
                  <button
                    type="button"
                    aria-label="Clear search"
                    className="shrink-0 text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
                    onClick={() => setQuery("")}
                  >
                    <Icon name="close" className="size-3.5" />
                  </button>
                ) : null}
              </div>
            ) : null}

            <p className="px-1 pb-1 text-[0.65rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
              Chats
            </p>
            {chatList.length === 0 ? (
              // The empty state teaches BOTH filing paths and says why to
              // bother — the payoff is the reason anyone files anything
              // (Item E2).
              <p className="px-1 py-2 text-[0.85rem] leading-[1.6] text-[var(--color-text-muted)]">
                No chats yet. Start one above, drag a chat onto the project in
                the sidebar, or use <strong>Move to project</strong> from any
                chat&rsquo;s ⋮ menu. Chats in a project don&rsquo;t expire.
              </p>
            ) : visibleChats.length === 0 ? (
              <p className="px-1 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
                No chats of yours match “{query}”.
              </p>
            ) : (
              <div className="flex flex-col gap-0.5">
                {visibleChats.map((c) => (
                  <button
                    key={c.id}
                    type="button"
                    className="flex w-full items-start gap-3 rounded-md px-3 py-2.5 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                    onClick={() => onOpenChat(c.id)}
                  >
                    <Icon
                      name="message"
                      className="mt-0.5 size-4 shrink-0 text-[var(--color-text-muted)]"
                    />
                    <span className="min-w-0 flex-1">
                      <span className="flex min-w-0 items-center gap-1.5">
                        <span className="truncate text-[0.9rem] text-[var(--color-text-primary)]">
                          {c.title || "Untitled"}
                        </span>
                        {c.team_visible ? (
                          <span
                            title={`Shared with ${project.team_id || "your team"}`}
                            aria-label={`Shared with ${project.team_id || "your team"}`}
                            className="shrink-0 text-[var(--color-accent)]"
                          >
                            <TeamGlyph className="size-3" />
                          </span>
                        ) : null}
                        {c.share_token ? (
                          <span
                            title="Shared by link — anyone with the link"
                            aria-label="Shared by link — anyone with the link"
                            className="shrink-0 text-[var(--color-accent)]"
                          >
                            <ShareGlyph className="size-3" />
                          </span>
                        ) : null}
                      </span>
                      {c.preview ? (
                        <span className="mt-0.5 line-clamp-2 block text-[0.78rem] leading-snug text-[var(--color-text-muted)]">
                          {stripMarkdown(c.preview)}
                        </span>
                      ) : null}
                    </span>
                    <span className="shrink-0 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                      {formatDay(c.updated_at)}
                    </span>
                  </button>
                ))}
              </div>
            )}

            {teamShared ? (
              <div className="mt-6">
                <p className="flex items-center gap-1.5 px-1 pb-1 text-[0.65rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
                  <TeamGlyph className="size-3" />
                  Team
                </p>
                {teamChats === null ? (
                  <p className="px-1 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
                    Loading…
                  </p>
                ) : teamChats.length === 0 ? (
                  <p className="px-1 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
                    No shared chats yet. Share one with your team from its ⋮
                    menu.
                  </p>
                ) : visibleTeamChats.length === 0 ? (
                  <p className="px-1 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
                    No shared chats match “{query}”.
                  </p>
                ) : (
                  <div className="flex flex-col gap-0.5">
                    {visibleTeamChats.map((c) => (
                      <button
                        key={c.id}
                        type="button"
                        className="flex w-full items-start gap-3 rounded-md px-3 py-2.5 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                        onClick={() => onOpenTeamChat(c.id)}
                      >
                        <TeamGlyph className="mt-0.5 size-4 shrink-0 text-[var(--color-text-muted)]" />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-[0.9rem] text-[var(--color-text-primary)]">
                            {c.title || "Untitled"}
                          </span>
                          <span className="mt-0.5 block truncate text-[0.78rem] text-[var(--color-text-muted)]">
                            {shortName(c.user_email)} · read-only
                          </span>
                        </span>
                        <span className="shrink-0 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                          {formatDay(c.updated_at)}
                        </span>
                      </button>
                    ))}
                  </div>
                )}
              </div>
            ) : null}
          </div>

          {/* Right column: the two team-level context layers as a pair
              (Instructions + Team learnings), then sources. */}
          <div className="flex min-w-0 flex-col gap-4">
            <div className={cardClass}>
              <div className="mb-2 flex items-center justify-between">
                <h2 className="text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
                  Instructions
                </h2>
                {isOwner && dirty ? (
                  <button
                    type="button"
                    disabled={savingInstructions}
                    className="rounded-md bg-[var(--color-accent)] px-2.5 py-1 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-90 disabled:opacity-60"
                    onClick={() => void saveInstructions()}
                  >
                    {savingInstructions ? "Saving…" : "Save"}
                  </button>
                ) : null}
              </div>
              {isOwner ? (
                <textarea
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  rows={8}
                  maxLength={8000}
                  placeholder="Standing instructions for every chat in this project…"
                  aria-label="Project instructions"
                  className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-2.5 text-[0.83rem] leading-relaxed text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)]"
                />
              ) : (
                <p className="whitespace-pre-wrap text-[0.83rem] leading-relaxed text-[var(--color-text-secondary)]">
                  {project.instructions?.trim() || "No instructions set."}
                </p>
              )}
              {/* The three layers, in the order buildSystemPrompt assembles
                  them. Instructions really do come first; team learnings and
                  personal memories arrive together in the memories block, the
                  project's tagged [project] — so this says "then", not
                  "before personal memories", which named only two of three. */}
              <p className="mt-1.5 text-[0.7rem] leading-[1.5] text-[var(--color-text-muted)]">
                Every chat here is fed by three layers:{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  Instructions
                </strong>{" "}
                first (owner-only), then this project&rsquo;s{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  Team learnings
                </strong>{" "}
                and each member&rsquo;s own{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  My memory
                </strong>
                .
              </p>
            </div>

            <TeamLearningsPanel
              projectId={project.id}
              projectOwner={project.owner_email}
              userEmail={userEmail}
              teamShared={teamShared}
            />

            <div className={cardClass}>
              <h2 className="mb-2 flex items-center gap-1.5 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
                <Icon name="paperclip" className="size-3.5 shrink-0" />
                Sources
              </h2>
              {fileError ? (
                <p className="mb-2 rounded-md border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-2 py-1.5 text-[0.72rem] leading-[1.5] text-[var(--color-danger)]">
                  {fileError}
                </p>
              ) : null}
              {files === null ? (
                <p className="text-[0.8rem] text-[var(--color-text-muted)]">
                  Loading…
                </p>
              ) : files.length === 0 ? (
                <p className="text-[0.8rem] text-[var(--color-text-muted)]">
                  Files from this project&apos;s chats — uploads, generated
                  CSVs, plots — appear here.
                </p>
              ) : (
                <div className="flex flex-col gap-0.5">
                  {files.map((f) => (
                    <button
                      key={`${f.conversation_id}/${f.path}`}
                      type="button"
                      className="group flex items-center gap-2 rounded-md px-2 py-1.5 text-left transition hover:bg-[var(--color-overlay-soft)]"
                      title={`${f.name} — from “${f.conversation_title || "Untitled"}”`}
                      onClick={() => void downloadFile(f)}
                    >
                      <Icon
                        name="download"
                        className="size-3.5 shrink-0 text-[var(--color-text-muted)] group-hover:text-[var(--color-text-primary)]"
                      />
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-[0.8rem] text-[var(--color-text-primary)]">
                          {f.name}
                        </span>
                        <span className="block truncate text-[0.68rem] text-[var(--color-text-muted)]">
                          {f.conversation_title || "Untitled"}
                        </span>
                      </span>
                      <span className="shrink-0 font-[family-name:var(--font-code)] text-[0.68rem] text-[var(--color-text-muted)]">
                        {formatBytes(f.size)}
                      </span>
                    </button>
                  ))}
                  {filesTruncated ? (
                    <p className="px-2 pt-1 text-[0.7rem] text-[var(--color-text-muted)]">
                      Showing the newest 200 files.
                    </p>
                  ) : null}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>

      {/* Per-project settings dialog — name / sharing / delete. Pin lives in
          the rail kebab (it's a list-ordering control, not project state the
          home needs to duplicate). */}
      {settingsOpen ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
          <button
            aria-label="Close settings"
            className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
            type="button"
            onClick={() => setSettingsOpen(false)}
          />
          <div
            role="dialog"
            aria-modal="true"
            aria-label={`Settings for ${project.name}`}
            className="relative z-10 w-full max-w-sm rounded-[1rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-md)]"
          >
            <h2 className="mb-4 text-[1rem] font-semibold text-[var(--color-text-primary)]">
              Project settings
            </h2>
            <label
              htmlFor={projectNameInputId}
              className="mb-1 block text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
            >
              Name
            </label>
            <input
              id={projectNameInputId}
              value={nameDraft}
              onChange={(e) => setNameDraft(e.target.value)}
              maxLength={128}
              // aria-label is kept deliberately: inside a dialog already titled
              // "Settings for <project>", "Project name" is the unambiguous
              // accessible name (and the one the e2e specs query by), while the
              // htmlFor/id pair above supplies the missing click-to-focus
              // association the bare <label> never had.
              aria-label="Project name"
              className="mb-4 w-full rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)]"
            />
            <label className="mb-1 flex items-center gap-2 text-[0.85rem] text-[var(--color-text-primary)]">
              <input
                type="checkbox"
                checked={sharedDraft}
                disabled={myTeam === ""}
                onChange={(e) => setSharedDraft(e.target.checked)}
              />
              Share with my team{myTeam ? ` (${myTeam})` : ""}
            </label>
            <p className="mb-4 text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
              {myTeam === ""
                ? "You are not in a team yet — create one in Settings → Team, then share this project with it."
                : "Members can chat in the project, read and write its team learnings, and share individual chats into it. Only you can edit or delete the project."}
            </p>
            {project.team_id && !sharedDraft ? (
              // Turning sharing off is not just a visibility change: it
              // unshares every chat members shared into the project.
              <p className="mb-4 text-[0.75rem] leading-[1.5] text-[var(--color-danger)]">
                Turning sharing off also unshares every chat members shared into
                this project.
              </p>
            ) : null}
            <TransferOwnership
              projectId={project.id}
              projectName={project.name}
              currentOwner={project.owner_email}
              onTransfer={onTransfer}
            />
            {settingsError ? (
              <p
                role="alert"
                className="mb-3 text-[0.75rem] leading-[1.5] text-[var(--color-danger)]"
              >
                {settingsError}
              </p>
            ) : null}
            <div className="flex items-center justify-between">
              <button
                type="button"
                className="rounded-md px-2.5 py-1.5 text-[0.8rem] font-medium text-[var(--color-danger)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
                onClick={() => setConfirmDelete(true)}
              >
                Delete project
              </button>
              <div className="flex gap-2">
                <button
                  type="button"
                  className="rounded-md px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
                  onClick={() => setSettingsOpen(false)}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  disabled={savingSettings}
                  className="rounded-md bg-[var(--color-accent)] px-3 py-1.5 text-[0.8rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-90 disabled:opacity-60"
                  onClick={() => void saveSettings()}
                >
                  {savingSettings ? "Saving…" : "Save"}
                </button>
              </div>
            </div>
          </div>
        </div>
      ) : null}

      {confirmDelete ? (
        <DeleteProjectConfirm
          project={project}
          onCancel={() => setConfirmDelete(false)}
          onConfirm={() => {
            setConfirmDelete(false);
            setSettingsOpen(false);
            onDelete();
          }}
        />
      ) : null}
    </div>
  );
}

// ── Team learnings (Item D2) ─────────────────────────────────────────────────
//
// The project's shared memory, and the FIRST surface anywhere that shows it:
// before this the entries were written (by the agent, by the projects modal)
// and injected into every project chat, with no screen listing them. Each row
// carries its writer and date, because a learning nobody can attribute is a
// rumour.
//
// Permissions, in one line: members manage their own entries, the owner
// manages all, and Retire is the default remove — it stops the entry being
// injected while keeping the record of what was learned and by whom. Delete is
// there for a genuine mistake. The server re-checks both (a hidden button is
// honest UI, not enforcement).
export function TeamLearningsPanel({
  projectId,
  projectOwner,
  userEmail,
  teamShared,
}: {
  projectId: string;
  projectOwner: string;
  userEmail: string;
  teamShared: boolean;
}) {
  const [entries, setEntries] = useState<TeamLearning[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState("");

  const load = useCallback(async () => {
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories`,
        { cache: "no-store" },
      );
      if (!res.ok) throw new Error(await res.text());
      const data = (await res.json()) as { memories?: TeamLearning[] };
      setEntries(data.memories ?? []);
      setError(null);
    } catch {
      // entries stays NULL, not []: an empty array renders "No team learnings
      // yet. Save one from any chat in this project" underneath the error,
      // which is a claim about the project we just failed to read.
      setEntries(null);
      setError("Couldn’t load team learnings.");
    }
  }, [projectId]);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void load();
    });
    return () => {
      cancelled = true;
    };
  }, [load]);

  const [savingEdit, setSavingEdit] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);

  const canManage = (m: TeamLearning) =>
    (m.user_email ?? "").toLowerCase() === userEmail.toLowerCase() ||
    projectOwner.toLowerCase() === userEmail.toLowerCase();

  const add = async () => {
    const content = draft.trim();
    if (!content || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ content }),
        },
      );
      if (!res.ok) throw new Error(await res.text());
      setDraft("");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t save that learning.");
    } finally {
      setBusy(false);
    }
  };

  // Returns whether the write landed, so a caller holding unsaved text (the
  // inline editor) knows whether it may throw that text away.
  const patch = async (id: string, body: Record<string, unknown>): Promise<boolean> => {
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(id)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
      );
      if (!res.ok) throw new Error(await res.text());
      await load();
      return true;
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t update that learning.");
      return false;
    }
  };

  const remove = async (id: string) => {
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(id)}`,
        { method: "DELETE" },
      );
      if (!res.ok) throw new Error(await res.text());
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t delete that learning.");
    }
  };

  const actionClass =
    "rounded px-1 py-0.5 text-[0.68rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]";

  return (
    <div className={cardClass} data-testid="team-learnings">
      <h2 className="mb-2 flex items-center gap-1.5 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
        <Icon name="brain" className="size-3.5 shrink-0" />
        Team learnings
      </h2>
      {error ? (
        <p className="mb-2 text-[0.72rem] text-[var(--color-danger)]">{error}</p>
      ) : null}
      {entries === null ? (
        <p className="text-[0.8rem] text-[var(--color-text-muted)]">
          {error ? "" : "Loading…"}
        </p>
      ) : entries.length === 0 ? (
        <p className="text-[0.8rem] leading-[1.5] text-[var(--color-text-muted)]">
          No team learnings yet. Save one from any chat in this project — every
          chat here is told about them.
        </p>
      ) : (
        <ul className="m-0 grid list-none gap-2 p-0">
          {entries.map((m) => {
            const retired = Boolean(m.retired_at);
            return (
              <li
                key={m.id}
                className="border-b border-[var(--color-border)] pb-2 last:border-b-0 last:pb-0"
              >
                {editingId === m.id ? (
                  <div className="grid gap-1">
                    <textarea
                      value={editDraft}
                      onChange={(e) => setEditDraft(e.target.value)}
                      rows={3}
                      aria-label="Edit team learning"
                      className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-2 text-[0.8rem] leading-relaxed text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)]"
                    />
                    <div className="flex justify-end gap-2">
                      <button
                        type="button"
                        className={actionClass}
                        onClick={() => setEditingId(null)}
                      >
                        Cancel
                      </button>
                      <button
                        type="button"
                        className={actionClass}
                        disabled={!editDraft.trim() || savingEdit}
                        onClick={() => {
                          const content = editDraft.trim();
                          if (!content) return;
                          if (content === m.content) {
                            setEditingId(null);
                            return;
                          }
                          // The editor stays up until the write lands. Tearing
                          // it down first threw the typed text away on any
                          // rejection — a permission error, a 500 — leaving a
                          // one-line message and no way back to the rewrite.
                          setSavingEdit(true);
                          void patch(m.id, { content }).then((ok) => {
                            setSavingEdit(false);
                            if (ok) setEditingId(null);
                          });
                        }}
                      >
                        {savingEdit ? "Saving…" : "Save"}
                      </button>
                    </div>
                  </div>
                ) : (
                  <>
                    <p
                      className={[
                        "m-0 whitespace-pre-wrap text-[0.8rem] leading-[1.5]",
                        retired
                          ? "text-[var(--color-text-muted)] line-through"
                          : "text-[var(--color-text-secondary)]",
                      ].join(" ")}
                    >
                      {m.pinned ? (
                        <Icon
                          name="pin"
                          className="mr-1 inline-block size-3 align-[-0.1em] text-[var(--color-accent)]"
                        />
                      ) : null}
                      {m.content}
                    </p>
                    <div className="mt-1 flex flex-wrap items-center gap-2 text-[0.68rem] text-[var(--color-text-muted)]">
                      <span>
                        {m.user_email ? shortName(m.user_email) : "unknown"}
                        {m.created_at ? ` · ${formatDay(m.created_at)}` : ""}
                        {retired ? " · retired" : ""}
                      </span>
                      {canManage(m) ? (
                        <span className="ml-auto flex items-center gap-1">
                          <button
                            type="button"
                            className={actionClass}
                            onClick={() => void patch(m.id, { pinned: !m.pinned })}
                          >
                            {m.pinned ? "Unpin" : "Pin"}
                          </button>
                          <button
                            type="button"
                            className={actionClass}
                            onClick={() => {
                              setEditingId(m.id);
                              setEditDraft(m.content);
                            }}
                          >
                            Edit
                          </button>
                          <button
                            type="button"
                            className={actionClass}
                            title={
                              retired
                                ? "Use this learning again"
                                : "Stop using this learning; keep the record"
                            }
                            onClick={() => void patch(m.id, { retired: !retired })}
                          >
                            {retired ? "Restore" : "Retire"}
                          </button>
                          {confirmRemove === m.id ? (
                            <>
                              <button
                                type="button"
                                className={`${actionClass} text-[var(--color-danger)]`}
                                onClick={() => {
                                  setConfirmRemove(null);
                                  void remove(m.id);
                                }}
                              >
                                Delete for good
                              </button>
                              <button
                                type="button"
                                className={actionClass}
                                onClick={() => setConfirmRemove(null)}
                              >
                                Keep
                              </button>
                            </>
                          ) : (
                            // Asks first. Delete is irreversible and sat one
                            // word from Retire in a row of four identically
                            // styled 0.68rem buttons — the project owner could
                            // destroy any member's contribution by mis-clicking.
                            <button
                              type="button"
                              className={`${actionClass} hover:text-[var(--color-danger)]`}
                              onClick={() => setConfirmRemove(m.id)}
                            >
                              Delete
                            </button>
                          )}
                        </span>
                      ) : null}
                    </div>
                  </>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <div className="mt-3 flex items-center gap-2">
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void add();
          }}
          placeholder="Add a learning the whole project should know…"
          aria-label="New team learning"
          className="min-w-0 flex-1 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1.5 text-[0.78rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)]"
        />
        <button
          type="button"
          disabled={busy || !draft.trim()}
          className="shrink-0 rounded-md border border-[var(--color-border-strong)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-40"
          onClick={() => void add()}
        >
          Add
        </button>
      </div>
      {!teamShared ? (
        <p className="mt-2 text-[0.68rem] leading-[1.5] text-[var(--color-text-muted)]">
          This project isn&rsquo;t shared with a team yet, so these are yours
          alone — they still reach every chat in the project.
        </p>
      ) : null}
    </div>
  );
}

// ── Transfer ownership ───────────────────────────────────────────────────────
//
// A project is owner-only to edit and delete, and could not change hands — so
// "the owner left" was terminal: the definition froze (every mutation is
// owner-scoped) and deleting the departing account destroyed the project and
// every team learning in it. Handing it over is the missing move, and the
// admin path (a departed owner cannot click anything) is why the endpoint
// authorizes owner-OR-admin rather than owner alone.
//
// Collapsed until asked for: it is a once-in-a-project action sitting in a
// dialog people open to rename things, and it should not read as a routine
// control.
function TransferOwnership({
  projectId,
  projectName,
  currentOwner,
  onTransfer,
}: {
  projectId: string;
  projectName: string;
  currentOwner: string;
  onTransfer: (toEmail: string) => Promise<string | null>;
}) {
  const [open, setOpen] = useState(false);
  const [members, setMembers] = useState<string[] | null>(null);
  const [choice, setChoice] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [reloadNonce, setReloadNonce] = useState(0);
  const selectId = useId();

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(projectId)}/members`,
            { cache: "no-store" },
          );
          if (!res.ok) {
            // A FAILED lookup is not an empty team. Rendering it as one told
            // the owner "nobody else is on this project's team yet" and sent
            // them off to fix a problem that does not exist, with no retry.
            if (!cancelled) {
              setMembers([]);
              setLoadError(`Couldn’t load this project’s members (HTTP ${res.status}).`);
            }
            return;
          }
          const data = (await res.json()) as { members?: string[] };
          if (!cancelled) {
            setMembers(data.members ?? []);
            setLoadError(null);
          }
        } catch {
          if (!cancelled) {
            setMembers([]);
            setLoadError("Couldn’t reach the server to list this project’s members.");
          }
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [open, projectId, reloadNonce]);

  // Everyone but the current owner — handing it to themselves is a no-op the
  // server accepts silently, but offering it would just be confusing.
  const candidates = (members ?? []).filter(
    (m) => m.toLowerCase() !== currentOwner.toLowerCase(),
  );

  if (!open) {
    return (
      <button
        type="button"
        className="mb-4 justify-self-start text-[0.75rem] text-[var(--color-text-muted)] underline transition hover:text-[var(--color-text-primary)]"
        onClick={() => setOpen(true)}
      >
        Transfer ownership…
      </button>
    );
  }

  return (
    <div className="mb-4 rounded-md border border-[var(--color-border)] p-3">
      <label
        htmlFor={selectId}
        className="mb-1 block text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
      >
        Transfer ownership of {projectName}
      </label>
      <p className="mb-2 text-[0.72rem] leading-[1.5] text-[var(--color-text-muted)]">
        The new owner can edit and delete the project. Its team, team learnings
        and everyone&rsquo;s chats are unchanged — and you stay a member if
        you&rsquo;re on the team.
      </p>
      {members === null ? (
        <p className="text-[0.75rem] text-[var(--color-text-muted)]">Loading members…</p>
      ) : loadError ? (
        <p className="text-[0.75rem] leading-[1.5] text-[var(--color-danger)]">
          {loadError}{" "}
          <button
            type="button"
            className="underline"
            onClick={() => {
              setMembers(null);
              setLoadError(null);
              setReloadNonce((n) => n + 1);
            }}
          >
            Try again
          </button>
        </p>
      ) : candidates.length === 0 ? (
        <p className="text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
          Nobody else is on this project&rsquo;s team yet. Share the project with
          a team, or have an admin add someone to it, and they&rsquo;ll appear
          here.
        </p>
      ) : (
        <div className="flex flex-wrap items-center gap-2">
          <select
            id={selectId}
            value={choice}
            onChange={(e) => setChoice(e.target.value)}
            className="min-w-0 flex-1 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1.5 text-[0.8rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)]"
          >
            <option value="">Choose a member…</option>
            {candidates.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          <button
            type="button"
            disabled={busy || !choice}
            className="shrink-0 rounded-md border border-[var(--color-border-strong)] px-2.5 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-40"
            onClick={() => {
              if (
                !window.confirm(
                  `Transfer ${projectName} to ${choice}? They'll be able to edit and delete it; you won't.`,
                )
              )
                return;
              setBusy(true);
              setError(null);
              void onTransfer(choice).then((err) => {
                setBusy(false);
                if (err) setError(err);
                else setOpen(false);
              });
            }}
          >
            {busy ? "Transferring…" : "Transfer"}
          </button>
        </div>
      )}
      {error ? (
        <p className="mt-2 text-[0.72rem] text-[var(--color-danger)]">{error}</p>
      ) : null}
      <button
        type="button"
        className="mt-2 text-[0.72rem] text-[var(--color-text-muted)] underline hover:text-[var(--color-text-primary)]"
        onClick={() => setOpen(false)}
      >
        Cancel
      </button>
    </div>
  );
}

// ── Delete confirm (Item A6) ─────────────────────────────────────────────────
//
// Deleting a project is not just "remove a label". Its team learnings die with
// it (by design — they are project state) and every member's chats leave it,
// which drops them into Temporary where retention can reach them. The confirm
// states both, with real counts, and offers the export that already exists
// rather than leaving the owner to find it afterwards.
function DeleteProjectConfirm({
  project,
  onCancel,
  onConfirm,
}: {
  project: Project;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [impact, setImpact] = useState<DeleteImpact | null>(null);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/impact`,
            { cache: "no-store" },
          );
          if (!res.ok) return;
          const data = (await res.json()) as DeleteImpact;
          if (!cancelled) setImpact(data);
        } catch {
          // Counts are copy: without them the dialog still states WHAT is
          // lost, just not how much.
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id]);

  const plural = (n: number, one: string, many: string) =>
    `${n} ${n === 1 ? one : many}`;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <button
        aria-label="Cancel deleting the project"
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onCancel}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={`Delete ${project.name}?`}
        className="relative z-10 w-full max-w-[28rem] rounded-[1rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-md)]"
      >
        <h2 className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]">
          Delete {project.name}?
        </h2>
        <ul className="mb-4 grid list-disc gap-[0.35rem] pl-[1.1rem] text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
          <li>
            {impact
              ? `${plural(impact.memories, "team learning", "team learnings")} will be lost`
              : "This project’s team learnings will be lost"}{" "}
            — they belong to the project and are not kept anywhere else.
          </li>
          <li>
            {impact
              ? `${plural(impact.chats, "chat", "chats")} from ${plural(impact.members, "member", "members")} will leave the project`
              : "Members’ chats will leave the project"}
              , become temporary, and expire unless pinned. The chats themselves
              stay with their owners.
          </li>
          {impact && impact.team_shared_chats > 0 ? (
            <li>
              {plural(impact.team_shared_chats, "chat", "chats")} shared with the
              team will stop being shared.
            </li>
          ) : null}
        </ul>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <a
            href={`/api/projects/${encodeURIComponent(project.id)}/export`}
            className="rounded-md border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Export first
          </a>
          <div className="flex gap-2">
            <button
              type="button"
              className="rounded-md px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
              onClick={onCancel}
            >
              Cancel
            </button>
            <button
              type="button"
              className="rounded-md px-3 py-1.5 text-[0.8rem] font-medium text-[var(--color-danger)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
              onClick={onConfirm}
            >
              Delete project
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
