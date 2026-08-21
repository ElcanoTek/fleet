"use client";

import { useEffect, useId, useState } from "react";
import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { Icon } from "./Icon";
import { formatBytes } from "./formatters";

// Project home (#509 follow-up): the page a project's rail row opens — title,
// this member's chats in the project, a Sources panel (workspace files from
// those chats: uploads, generated CSVs/plots, …), and an instructions editor
// on the right, in the Claude-desktop / ChatGPT-projects arrangement. Also
// hosts the PER-PROJECT settings dialog (name / sharing / pin / delete) —
// the all-projects modal stays only for creation.
//
// Privacy: the chat list and Sources show the CALLER'S OWN conversations
// only. A team-shared project shares its definition (instructions, memory),
// never the members' private chats — the backend enforces the same rule.

type ProjectChatEntry = {
  id: string;
  title: string;
  updated_at: number;
  // Last text message's snippet ("You: …" when the user spoke last) — the
  // 1–2 line history under each chat title.
  preview?: string;
};

type ProjectFileEntry = {
  conversation_id: string;
  conversation_title: string;
  path: string;
  name: string;
  size: number;
  modified_at: number;
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

export function ProjectHome({
  project,
  chats,
  isOwner,
  initialSettingsOpen,
  onBack,
  onOpenChat,
  onNewChat,
  onSaveInstructions,
  onUpdateSettings,
  myTeam,
  onDelete,
}: {
  project: Project;
  chats: ConversationSummary[];
  isOwner: boolean;
  // Open straight into the settings dialog (the rail kebab's
  // "Project settings…" path).
  initialSettingsOpen?: boolean;
  onBack: () => void;
  onOpenChat: (conversationId: string) => void;
  onNewChat: () => void;
  // Both mutations resolve true on success — the dialogs/cards keep their
  // draft (and show the parent's rail-error toast) on failure.
  onSaveInstructions: (instructions: string) => Promise<boolean>;
  onUpdateSettings: (patch: {
    name?: string;
    team_shared?: boolean;
  }) => Promise<boolean>;
  // The viewer's own team (#1157): "" = not in a team, so team sharing cannot
  // work yet and the dialog says where to fix that instead of letting the
  // toggle 400. undefined = not read yet — the copy stays neutral.
  myTeam?: string;
  onDelete: () => void;
}) {
  const [files, setFiles] = useState<ProjectFileEntry[] | null>(null);
  // Server-side chat list with previews; the prop list (already in client
  // state) renders instantly while this loads, then the previews fill in.
  const [fetchedChats, setFetchedChats] = useState<ProjectChatEntry[] | null>(null);
  const [filesTruncated, setFilesTruncated] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(
    Boolean(initialSettingsOpen),
  );

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
  }, [project.id]);

  // Sources — fetched per open; best-effort (a failure shows the empty state).
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
            if (!cancelled) setFiles([]);
            return;
          }
          const data = (await res.json()) as {
            files?: ProjectFileEntry[];
            truncated?: boolean;
          };
          if (!cancelled) {
            setFiles(data.files ?? []);
            setFilesTruncated(Boolean(data.truncated));
          }
        } catch {
          if (!cancelled) setFiles([]);
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

  const saveSettings = async () => {
    if (savingSettings) return;
    const patch: { name?: string; team_shared?: boolean } = {};
    if (nameDraft.trim() && nameDraft.trim() !== project.name)
      patch.name = nameDraft.trim();
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

  const cardClass =
    "rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4";

  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4 sm:px-8"
      data-testid="project-home"
    >
      <div className="mx-auto max-w-5xl">
        {/* Header: back · title (+pin) · settings */}
        <div className="mb-6 flex items-center gap-3">
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
            <span className="shrink-0 rounded-full border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)]">
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
          {/* Main column: new chat + this member's chats. */}
          <div className="min-w-0">
            <button
              type="button"
              className="mb-4 flex w-full items-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-4 py-3 text-left text-[0.9rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={onNewChat}
            >
              <Icon name="plus" className="size-4 shrink-0" />
              New chat in {project.name}
            </button>

            <p className="px-1 pb-1 text-[0.65rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
              Chats
            </p>
            {chatList.length === 0 ? (
              <p className="px-1 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
                No chats yet — start one above or drag a chat onto the project
                in the sidebar.
              </p>
            ) : (
              <div className="flex flex-col gap-0.5">
                {chatList.map((c) => (
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
                      <span className="block truncate text-[0.9rem] text-[var(--color-text-primary)]">
                        {c.title || "Untitled"}
                      </span>
                      {c.preview ? (
                        <span className="mt-0.5 line-clamp-2 block text-[0.78rem] leading-snug text-[var(--color-text-muted)]">
                          {c.preview}
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
          </div>

          {/* Right column: instructions (Claude-desktop style) + sources. */}
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
              <p className="mt-1.5 text-[0.7rem] text-[var(--color-text-muted)]">
                Injected into every chat in this project, before personal
                memories.
              </p>
            </div>

            <div className={cardClass}>
              <h2 className="mb-2 flex items-center gap-1.5 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
                <Icon name="paperclip" className="size-3.5 shrink-0" />
                Sources
              </h2>
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
                    <a
                      key={`${f.conversation_id}/${f.path}`}
                      href={`/api/conversations/${encodeURIComponent(f.conversation_id)}/workspace/${encodeURIComponent(f.path)}`}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="group flex items-center gap-2 rounded-md px-2 py-1.5 transition hover:bg-[var(--color-overlay-soft)]"
                      title={`${f.name} — from “${f.conversation_title || "Untitled"}”`}
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
                    </a>
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
                : "Members can chat in the project and read/write its shared memory; only you can edit or delete it."}
            </p>
            <div className="flex items-center justify-between">
              <button
                type="button"
                className="rounded-md px-2.5 py-1.5 text-[0.8rem] font-medium text-[var(--color-danger)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
                onClick={onDelete}
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
    </div>
  );
}
