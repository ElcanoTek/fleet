import { memoryKindLabel } from "@/app/lib/i18n/ownedLabels";
import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
// MemoryManagerDialog — chat's "Memories" modal (personal list and
// the open project's team learnings) and the project picker for "Move to
// team learnings", built on the standard .picker-modal sheet/dialog shell.

import { useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { Icon } from "@/app/shared/ui/Icon";
import { Segmented } from "@/app/shared/ui/Segmented";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { MEMORY_KINDS } from "@/app/shared/lib/memoriesApi";
import { memoryModalSubtitle } from "./memoryModalCopy";
import { TeamLearningsPanel } from "./ProjectHome";
import { openedByTouch } from "./toolsPicker";
import type { ChatMemories, MemoryView } from "./useChatMemories";

/** A team-shared project a personal memory can move into. */
export type PromoteTarget = { id: string; name: string; team_id?: string };

/** The open chat's project, in the shape the memory views need (Item D1). */
export type MemoryProject = { id: string; name: string; teamShared: boolean };

type MemoryFilter = "all" | "pinned" | "active" | "retired";

const MEMORY_FILTERS: { id: MemoryFilter; label: string }[] = [
  { id: "all", label: tr("All") },
  { id: "pinned", label: tr("Pinned") },
  { id: "active", label: tr("Active") },
  { id: "retired", label: tr("Retired") },
];

export function MemoryManagerDialog({
  mem,
  activeMemoryView,
  activeProjectForMemory,
  activeProjectOwner,
  teamSharedProjects,
  userEmail,
}: {
  mem: ChatMemories;
  /** memoryView, falling back to "list" outside a project (see chat-experience). */
  activeMemoryView: MemoryView;
  activeProjectForMemory: MemoryProject | null;
  /** The open project's owner, for the team learnings panel. */
  activeProjectOwner: string;
  teamSharedProjects: PromoteTarget[];
  userEmail: string;
}) {
  const {
    memories,
    memoryView,
    setMemoryView,
    setMemoryManagerOpen,
    setPromoteMemoryId,
    memoryDraft,
    setMemoryDraft,
    memoryKindDraft,
    setMemoryKindDraft,
    editingMemoryId,
    memoryError,
    isLoadingMemories,
    isSavingMemory,
    saveMemory,
    promoteMemoryToProject,
    deleteMemory,
    patchMemory,
    editMemory,
    cancelEdit,
  } = mem;

  const [searchQuery, setSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<MemoryFilter>("all");
  const panelRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  useDialogA11y(true, panelRef, () => setMemoryManagerOpen(false), {
    initialFocusRef: openedByTouch() ? panelRef : textareaRef,
  });

  const activeCount = useMemo(
    () => memories.filter((m) => m.retired_at == null).length,
    [memories],
  );
  const pinnedCount = useMemo(
    () => memories.filter((m) => m.pinned && m.retired_at == null).length,
    [memories],
  );

  const filteredMemories = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return memories.filter((m) => {
      const retired = m.retired_at != null;
      if (statusFilter === "pinned" && (!m.pinned || retired)) return false;
      if (statusFilter === "active" && retired) return false;
      if (statusFilter === "retired" && !retired) return false;
      if (q) {
        const hay = `${m.content} ${m.kind ?? ""} ${m.source ?? ""}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
  }, [memories, searchQuery, statusFilter]);

  const isTeamView = memoryView === "team" && Boolean(activeProjectForMemory);

  const modalContent = (
    <div className="scrim fixed inset-0 z-[1200] flex items-stretch justify-center p-0 sm:items-center sm:p-4">
      <button
        type="button"
        tabIndex={-1}
        aria-hidden="true"
        onClick={() => setMemoryManagerOpen(false)}
        className="absolute inset-0 cursor-default"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={tr("Memories")}
        data-testid="memory-manager-dialog"
        className="picker-modal relative z-10 outline-none sm:max-w-[40rem]"
      >
        <header className="picker-modal__head">
          <span aria-hidden="true" className="picker-icon-tile shrink-0">
            <Icon name="brain" className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-x-2.5 gap-y-0.5">
              <h2 className="picker-modal__title">{tr("Memories")}</h2>
              {!isTeamView ? (
                <span className="text-[0.78rem] tabular-nums text-[var(--color-text-subtle)]">
        {rich("{0} active{1}", activeCount, pinnedCount > 0 ? tr(" · {0} pinned", [pinnedCount]) : "")}
      </span>
              ) : null}
            </div>
            <p className="m-0 text-[0.75rem] leading-[1.35] text-[var(--color-text-subtle)]">
              {memoryModalSubtitle(activeMemoryView, {
                userEmail,
                projectName: activeProjectForMemory?.name,
              })}
            </p>
          </div>
          <CloseButton
            label={tr("Close memories")}
            onClick={() => setMemoryManagerOpen(false)}
            className="any-pointer-coarse:size-11"
          />
        </header>

        <div className="picker-modal__rail">
          {activeProjectForMemory ? (
            <div className="flex items-center">
              <Segmented
                label={tr("Memory view")}
                value={activeMemoryView}
                options={[
                  { value: "list", label: tr("My memory") },
                  { value: "team" as const, label: tr("Team learnings") },
                ]}
                onChange={setMemoryView}
              />
            </div>
          ) : null}
          {!isTeamView ? (
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="relative min-w-[12rem] flex-1">
                <Icon
                  name="search"
                  className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-[var(--color-text-subtle)]"
                />
                <input
                  type="search"
                  aria-label={tr("Search memories")}
                  placeholder={tr("Search memories…")}
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  className="h-8 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] pl-8 pr-2.5 text-[0.78rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
                />
              </div>
              <div
                className="flex flex-wrap items-center gap-1"
                role="group"
                aria-label={tr("Filter memories")}
              >
                {MEMORY_FILTERS.map((f) => (
                  <button
                    key={f.id}
                    type="button"
                    aria-pressed={statusFilter === f.id}
                    onClick={() => setStatusFilter(f.id)}
                    className="pill-btn hit-area-touch h-6 px-2.5 text-[0.72rem]"
                  >
                    {f.label}
                  </button>
                ))}
              </div>
            </div>
          ) : null}
        </div>

        {isTeamView && activeProjectForMemory ? (
          <div className="picker-modal__body">
            <TeamLearningsPanel
              projectId={activeProjectForMemory.id}
              projectOwner={activeProjectOwner}
              userEmail={userEmail}
              teamShared={activeProjectForMemory.teamShared}
            />
          </div>
        ) : (
          <div className="picker-modal__body grid content-start gap-3">
            <div className="grid gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3">
              <textarea
                ref={textareaRef}
                rows={3}
                className="min-h-[4.5rem] w-full resize-y rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.84rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
                placeholder={tr("Remember that deal names may contain intentional typos.")}
                value={memoryDraft}
                onChange={(event) => setMemoryDraft(event.target.value)}
              />
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <label
                    className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.08em] text-[var(--color-text-subtle)]"
                    htmlFor="memory-kind"
                  >
                    {tr("Kind")}
                  </label>
                  <select
                    id="memory-kind"
                    className="h-7 rounded-[var(--radius-sm)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-2 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                    value={memoryKindDraft}
                    onChange={(event) =>
                      setMemoryKindDraft(event.target.value)
                    }
                  >
                    {MEMORY_KINDS.map((k) => (
                      <option key={k} value={k}>
                        {memoryKindLabel(k)}
                      </option>
                    ))}
                  </select>
                </div>
                <div className="flex items-center gap-2">
                  {editingMemoryId ? (
                    <button
                      type="button"
                      className="btn btn-ghost btn-sm"
                      onClick={cancelEdit}
                    >
                      {tr("Cancel edit")}
                    </button>
                  ) : null}
                  <button
                    type="button"
                    className="btn btn-primary btn-sm"
                    disabled={!memoryDraft.trim() || isSavingMemory}
                    onClick={() => void saveMemory()}
                  >
                    {isSavingMemory
                      ? tr("Saving...")
                      : editingMemoryId
                        ? tr("Save changes")
                        : tr("Add memory")}
                  </button>
                </div>
              </div>
            </div>

            {memoryError ? (
              <div
                role="alert"
                className="rounded-[var(--radius-md)] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger,#dc2626)]"
              >
                {memoryError}
              </div>
            ) : null}

            {isLoadingMemories ? (
              <p className="py-4 text-center text-[0.8125rem] text-[var(--color-text-muted)]">
                {tr("Loading memories...")}
              </p>
            ) : memories.length === 0 ? (
              <p className="rounded-[var(--radius-md)] border border-dashed border-[var(--color-border)] px-3.5 py-5 text-center text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
                {tr("No memories yet. Add one manually or tell chat “remember this: ...”.")}
              </p>
            ) : filteredMemories.length === 0 ? (
              <p className="rounded-[var(--radius-md)] border border-dashed border-[var(--color-border)] px-3.5 py-5 text-center text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
                {tr("No memories match your filter.")}
              </p>
            ) : (
              <div className="grid gap-2">
                {filteredMemories.map((memory) => {
                  const retired = memory.retired_at != null;
                  const sourceLabel =
                    memory.source === "chat"
                      ? memory.origin === "auto"
                        ? tr("Auto-extracted from chat")
                        : tr("Saved from chat")
                      : memory.source === "proposed"
                        ? tr("Proposed")
                        : tr("Manual");
                  const learned = memory.learned_at
                    ? new Date(
                        memory.learned_at * 1000,
                      ).toLocaleDateString(getLanguage())
                    : null;
                  return (
                    <div
                      key={memory.id}
                      className={`rounded-[var(--radius-md)] border px-3.5 py-3 transition ${
                        memory.pinned && !retired
                          ? "border-[var(--color-selected-border)] bg-[var(--color-selected-bg)]"
                          : "border-[var(--color-border)] bg-[var(--color-surface-2)]"
                      } ${retired ? "opacity-60" : ""}`}
                    >
                      <p className="whitespace-pre-wrap text-[0.85rem] leading-[1.5] text-[var(--color-text-primary)]">
                        {memory.pinned ? (
                          <span title={tr("Pinned")}>📌 </span>
                        ) : null}
                        {memory.content}
                      </p>
                      <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
                        <span className="flex flex-wrap items-center gap-1.5">
                          {memory.kind && memory.kind !== "fact" ? (
                            <StatusChip tone="neutral">
                              {memoryKindLabel(memory.kind)}
                            </StatusChip>
                          ) : null}
                          <span>
                            {sourceLabel}
                            {learned ? tr(" · learned {0}", [learned]) : ""}
                            {retired ? tr(" · retired") : ""}
                          </span>
                        </span>
                        <div className="flex flex-wrap items-center gap-1.5">
                          {retired ? (
                            <button
                              type="button"
                              className="pill-btn hit-area-touch"
                              onClick={() =>
                                void patchMemory(memory.id, {
                                  retired: false,
                                })
                              }
                            >
                              {tr("Restore")}
                            </button>
                          ) : (
                            <>
                              <button
                                type="button"
                                className="pill-btn hit-area-touch"
                                onClick={() =>
                                  void patchMemory(memory.id, {
                                    pinned: !memory.pinned,
                                  })
                                }
                              >
                                {memory.pinned ? tr("Unpin") : tr("Pin")}
                              </button>
                              <button
                                type="button"
                                className="pill-btn hit-area-touch"
                                onClick={() => editMemory(memory)}
                              >
                                {tr("Edit")}
                              </button>
                              <button
                                type="button"
                                className="pill-btn hit-area-touch"
                                title={tr("Keep for audit, stop injecting into chats")}
                                onClick={() =>
                                  void patchMemory(memory.id, {
                                    retired: true,
                                  })
                                }
                              >
                                {tr("Retire")}
                              </button>
                              {memory.source !== "proposed" &&
                              teamSharedProjects.length > 0 ? (
                                <button
                                  type="button"
                                  className="pill-btn hit-area-touch"
                                  title={tr("Move this memory into a project's team learnings")}
                                  onClick={() => {
                                    if (teamSharedProjects.length === 1)
                                      void promoteMemoryToProject(
                                        memory.id,
                                        teamSharedProjects[0].id,
                                      );
                                    else setPromoteMemoryId(memory.id);
                                  }}
                                >
                                  {tr("Move to team learnings")}
                                </button>
                              ) : null}
                            </>
                          )}
                          <button
                            type="button"
                            className="pill-btn hit-area-touch hover:text-[var(--color-danger)]!"
                            onClick={() => void deleteMemory(memory.id)}
                          >
                            {tr("Delete")}
                          </button>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}

        <footer className="picker-modal__foot">
          <span className="min-w-0 truncate text-[0.75rem] text-[var(--color-text-subtle)]">
            {tr(memories.length === 1 ? "{0} memory saved" : "{0} memories saved", [memories.length])}
          </span>
          <a href="/settings/memory">
            {tr("Manage in Settings")}
            <Icon name="external-link" className="size-3" />
          </a>
        </footer>
      </div>
    </div>
  );

  if (typeof document === "undefined") {
    return modalContent;
  }
  return createPortal(modalContent, document.body);
}

export function MemoryPromoteDialog({
  id,
  teamSharedProjects,
  promoteMemoryToProject,
  onCancel,
}: {
  /** The memory being moved. */
  id: string;
  teamSharedProjects: PromoteTarget[];
  promoteMemoryToProject: (id: string, projectID: string) => Promise<void>;
  onCancel: () => void;
}) {
  const panelRef = useRef<HTMLDivElement | null>(null);
  useDialogA11y(true, panelRef, onCancel);

  const modalContent = (
    <div className="scrim fixed inset-0 z-[1210] flex items-stretch justify-center p-0 sm:items-center sm:p-4">
      <button
        type="button"
        tabIndex={-1}
        aria-hidden="true"
        onClick={onCancel}
        className="absolute inset-0 cursor-default"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={tr("Move to team learnings")}
        className="picker-modal relative z-10 outline-none sm:max-w-[26rem]"
      >
        <header className="picker-modal__head">
          <span aria-hidden="true" className="picker-icon-tile shrink-0">
            <Icon name="folder" className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="picker-modal__title">{tr("Move to team learnings")}</h2>
            <p className="m-0 text-[0.75rem] leading-[1.35] text-[var(--color-text-subtle)]">
              {tr("Pick the project this belongs to. It leaves your personal memory and every member of that project sees it.")}
            </p>
          </div>
          <CloseButton
            label={tr("Cancel moving the memory")}
            onClick={onCancel}
            className="any-pointer-coarse:size-11"
          />
        </header>
        <div className="picker-modal__body grid content-start gap-1.5">
          {teamSharedProjects.map((p) => (
            <button
              key={p.id}
              type="button"
              className="flex items-center justify-between gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3.5 py-2.5 text-left text-[0.85rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-selected-border)] hover:bg-[var(--color-selected-bg)]"
              onClick={() => void promoteMemoryToProject(id, p.id)}
            >
              <span className="truncate">{p.name}</span>
              {p.team_id ? (
                <span className="shrink-0 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                  {p.team_id}
                </span>
              ) : null}
            </button>
          ))}
        </div>
        <footer className="picker-modal__foot justify-end">
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={onCancel}
          >
            {tr("Cancel")}</button>
        </footer>
      </div>
    </div>
  );

  if (typeof document === "undefined") {
    return modalContent;
  }
  return createPortal(modalContent, document.body);
}
