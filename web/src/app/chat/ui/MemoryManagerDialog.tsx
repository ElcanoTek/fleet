// MemoryManagerDialog — chat's "Memories" modal (personal list and
// the open project's team learnings) and the project picker for "Move to
// team learnings", moved out of chat-experience.tsx unchanged (#2008). State
// and actions come from useChatMemories.

import { CloseButton } from "@/app/shared/ui/CloseButton";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { Segmented } from "@/app/shared/ui/Segmented";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { MEMORY_KINDS } from "@/app/shared/lib/memoriesApi";
import { memoryModalSubtitle } from "./memoryModalCopy";
import { TeamLearningsPanel } from "./ProjectHome";
import type { ChatMemories, MemoryView } from "./useChatMemories";

/** A team-shared project a personal memory can move into. */
export type PromoteTarget = { id: string; name: string; team_id?: string };

/** The open chat's project, in the shape the memory views need (Item D1). */
export type MemoryProject = { id: string; name: string; teamShared: boolean };

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
  return (
    <DialogShell
      label="Memories"
      scrimLabel="Close memories"
      onDismiss={() => setMemoryManagerOpen(false)}
      className="flex max-h-[88vh] max-w-[34rem] flex-col gap-4 overflow-hidden p-5"
    >
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">
            Memories
          </h2>
          {/* The subtitle describes the VISIBLE tab. It used to
              describe personal memories on all three, so on Team
              learnings neither half of it was true — those are scoped
              to the project and its team, and injected only into that
              project's chats (finding #18). */}
          <p className="mt-1 text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
            {memoryModalSubtitle(activeMemoryView, {
              userEmail,
              projectName: activeProjectForMemory?.name,
            })}
          </p>
        </div>
        <CloseButton
          label="Close memories"
          onClick={() => setMemoryManagerOpen(false)}
        />
      </div>

      {/* The Team learnings tab appears only inside a project — it is that
          project's team learnings, the same list (and the same permissions) as
          the project home's panel, reachable without leaving the
          conversation (Item D4). */}
      {activeProjectForMemory ? (
        <div className="flex items-center">
          <Segmented
            label="Memory view"
            value={activeMemoryView}
            options={[
              { value: "list", label: "My memory" },
              { value: "team" as const, label: "Team learnings" },
            ]}
            onChange={setMemoryView}
          />
        </div>
      ) : null}

      {memoryView === "team" && activeProjectForMemory ? (
        <div className="min-h-0 flex-1 overflow-y-auto pr-1">
          <TeamLearningsPanel
            projectId={activeProjectForMemory.id}
            projectOwner={activeProjectOwner}
            userEmail={userEmail}
            teamShared={activeProjectForMemory.teamShared}
          />
        </div>
      ) : (
        <>
          <div className="grid gap-2">
            <textarea
              className="min-h-24 w-full resize-y rounded-[0.9rem] border border-[var(--color-border-strong)] bg-transparent px-3 py-2 text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
              placeholder="Remember that deal names may contain intentional typos."
              value={memoryDraft}
              onChange={(event) => setMemoryDraft(event.target.value)}
            />
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <label
                  className="text-[0.72rem] text-[var(--color-text-muted)]"
                  htmlFor="memory-kind"
                >
                  Kind
                </label>
                <select
                  id="memory-kind"
                  className="rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                  value={memoryKindDraft}
                  onChange={(event) =>
                    setMemoryKindDraft(event.target.value)
                  }
                >
                  {MEMORY_KINDS.map((k) => (
                    <option key={k} value={k}>
                      {k}
                    </option>
                  ))}
                </select>
              </div>
              <div className="flex items-center gap-2">
                {editingMemoryId ? (
                  <button
                    type="button"
                    className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                    onClick={cancelEdit}
                  >
                    Cancel edit
                  </button>
                ) : null}
                <button
                  type="button"
                  className="rounded-[var(--radius-md)] bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40"
                  disabled={!memoryDraft.trim() || isSavingMemory}
                  onClick={() => void saveMemory()}
                >
                  {isSavingMemory
                    ? "Saving..."
                    : editingMemoryId
                      ? "Save changes"
                      : "Add memory"}
                </button>
              </div>
            </div>
          </div>

          {memoryError ? (
            <div className="rounded-[0.75rem] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger,#dc2626)]">
              {memoryError}
            </div>
          ) : null}

          <div className="min-h-0 flex-1 overflow-y-auto pr-1">
            {isLoadingMemories ? (
              <p className="py-4 text-[0.8125rem] text-[var(--color-text-muted)]">
                Loading memories...
              </p>
            ) : memories.length === 0 ? (
              <p className="rounded-[0.9rem] border border-dashed border-[var(--color-border)] px-3 py-4 text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
                No memories yet. Add one manually or tell chat “remember
                this: ...”.
              </p>
            ) : (
              <div className="grid gap-2">
                {memories.map((memory) => {
                  const retired = memory.retired_at != null;
                  const sourceLabel =
                    memory.source === "chat"
                      ? memory.origin === "auto"
                        ? "Auto-extracted from chat"
                        : "Saved from chat"
                      : memory.source === "proposed"
                        ? "Proposed"
                        : "Manual";
                  const learned = memory.learned_at
                    ? new Date(
                        memory.learned_at * 1000,
                      ).toLocaleDateString()
                    : null;
                  return (
                    <div
                      key={memory.id}
                      className={`rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3 ${retired ? "opacity-60" : ""}`}
                    >
                      <p className="whitespace-pre-wrap text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)]">
                        {memory.pinned ? (
                          <span title="Pinned">📌 </span>
                        ) : null}
                        {memory.content}
                      </p>
                      <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
                        <span>
                          {memory.kind && memory.kind !== "fact" ? (
                            <StatusChip tone="neutral" className="mr-1.5">
                              {memory.kind}
                            </StatusChip>
                          ) : null}
                          {sourceLabel}
                          {learned ? ` · learned ${learned}` : ""}
                          {retired ? " · retired" : ""}
                        </span>
                        <div className="flex items-center gap-3">
                          {retired ? (
                            <button
                              type="button"
                              className="hover:text-[var(--color-text-primary)]"
                              onClick={() =>
                                void patchMemory(memory.id, {
                                  retired: false,
                                })
                              }
                            >
                              Restore
                            </button>
                          ) : (
                            <>
                              <button
                                type="button"
                                className="hover:text-[var(--color-text-primary)]"
                                onClick={() =>
                                  void patchMemory(memory.id, {
                                    pinned: !memory.pinned,
                                  })
                                }
                              >
                                {memory.pinned ? "Unpin" : "Pin"}
                              </button>
                              <button
                                type="button"
                                className="hover:text-[var(--color-text-primary)]"
                                onClick={() => editMemory(memory)}
                              >
                                Edit
                              </button>
                              <button
                                type="button"
                                className="hover:text-[var(--color-text-primary)]"
                                title="Keep for audit, stop injecting into chats"
                                onClick={() =>
                                  void patchMemory(memory.id, {
                                    retired: true,
                                  })
                                }
                              >
                                Retire
                              </button>
                              {/* Promote a personal memory the whole
                                  team should have (Item D5). Only for
                                  real, saved memories — a proposal has
                                  not been accepted yet — and only when
                                  there is a team-shared project to
                                  promote it into. */}
                              {memory.source !== "proposed" &&
                              teamSharedProjects.length > 0 ? (
                                <button
                                  type="button"
                                  className="hover:text-[var(--color-text-primary)]"
                                  title="Move this memory into a project's team learnings"
                                  onClick={() => {
                                    if (teamSharedProjects.length === 1)
                                      void promoteMemoryToProject(
                                        memory.id,
                                        teamSharedProjects[0].id,
                                      );
                                    else setPromoteMemoryId(memory.id);
                                  }}
                                >
                                  Move to team learnings
                                </button>
                              ) : null}
                            </>
                          )}
                          <button
                            type="button"
                            className="hover:text-[var(--color-danger)]"
                            onClick={() => void deleteMemory(memory.id)}
                          >
                            Delete
                          </button>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </>
      )}
    </DialogShell>
  );
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
  return (
    <DialogShell
      label="Move to team learnings"
      scrimLabel="Cancel moving the memory"
      onDismiss={onCancel}
      layer="stacked"
      className="max-w-[24rem] p-5"
    >
      <h2 className="mb-1 text-[0.95rem] font-semibold text-[var(--color-text-primary)]">
        Move to team learnings
      </h2>
      <p className="mb-3 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]">
        Pick the project this belongs to. It leaves your personal memory
        and every member of that project sees it.
      </p>
      <div className="grid gap-1">
        {teamSharedProjects.map((p) => (
          <button
            key={p.id}
            type="button"
            className="rounded-md border border-[var(--color-border)] px-3 py-2 text-left text-[0.85rem] text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]"
            onClick={() =>
              void promoteMemoryToProject(id, p.id)
            }
          >
            {p.name}
            <span className="ml-2 text-[0.7rem] text-[var(--color-text-muted)]">
              {p.team_id}
            </span>
          </button>
        ))}
      </div>
      <div className="mt-4 flex justify-end">
        <button
          type="button"
          className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.78rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </DialogShell>
  );
}
