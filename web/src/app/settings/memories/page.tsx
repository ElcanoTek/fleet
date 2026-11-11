import { memoryKindLabel } from "@/app/lib/i18n/ownedLabels";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
import { useCallback, useEffect, useState } from "react";
import {
  btnClass,
  ConnBadge,
  InlineConfirmButton,
  Segmented,
  SETTINGS_INPUT,
} from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRows,
  SetSection,
} from "../ui/panels";
import {
  deleteMemory,
  listMemories,
  MEMORY_KINDS,
  MemoriesApiError,
  patchMemory,
  promoteMemory,
  saveMemory,
  type UserMemory,
} from "@/app/shared/lib/memoriesApi";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { fetchMe } from "@/app/shared/lib/account";
import { redirectToLogin } from "@/app/settings/lib/getGated";

type ProjectSummary = {
  id: string;
  name: string;
  team_id?: string;
};

const ROW =
  "grid gap-[0.45rem] border-b border-[var(--color-border-subtle)] px-[0.1rem] py-3 last:border-b-0";
const ROW_HEAD = "flex flex-wrap items-center justify-between gap-2";
const LOADING =
  "m-0 py-3 text-center text-[0.79rem] text-[var(--color-text-muted)]";

export default function MemoriesPage() {
  const [memories, setMemories] = useState<UserMemory[] | null>(null);
  const [teamProjects, setTeamProjects] = useState<ProjectSummary[]>([]);
  const [readOnlyViewer, setReadOnlyViewer] = useState(false);
  const [filter, setFilter] = useState<"active" | "all" | "retired">("active");
  const [search, setSearch] = useState("");
  const [draft, setDraft] = useState("");
  const [kindDraft, setKindDraft] = useState<string>("fact");
  const [editingId, setEditingId] = useState<string | null>(null);
  const [promoteId, setPromoteId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const loadMemories = useCallback(async () => {
    try {
      setMemories(await listMemories(tr("Unable to load memories")));
    } catch (err) {
      if (err instanceof MemoriesApiError && err.status === 401) {
        redirectToLogin();
        return;
      }
      setError(errorMessage(err, tr("Unable to load memories.")));
      setMemories((cur) => cur ?? []);
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void loadMemories();
    });
    // Optional viewer-role check (shared/lib/account).
    void fetchMe().then((me) => {
      if (!cancelled && me?.role === "viewer") setReadOnlyViewer(true);
    });
    void (async () => {
      try {
        const res = await fetch("/api/projects", { cache: "no-store" });
        if (!res.ok || cancelled) return;
        const data = (await res.json()) as { projects?: ProjectSummary[] };
        if (!cancelled) {
          setTeamProjects((data.projects ?? []).filter((p) => Boolean(p.team_id)));
        }
      } catch {
        // Optional team project list for promoting memories.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [loadMemories]);

  const handleSave = async () => {
    const content = draft.trim();
    if (!content || busy) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await saveMemory({ id: editingId, content, kind: kindDraft }, tr("Unable to save memory"));
      const wasEditing = editingId !== null;
      setDraft("");
      setKindDraft("fact");
      setEditingId(null);
      setNotice(wasEditing ? tr("Memory updated.") : tr("Memory added."));
      await loadMemories();
    } catch (err) {
      setError(errorMessage(err, tr("Unable to save memory.")));
    } finally {
      setBusy(false);
    }
  };

  const handlePatch = async (
    id: string,
    patch: { pinned?: boolean; retired?: boolean },
  ) => {
    setError(null);
    setNotice(null);
    try {
      await patchMemory(id, patch, tr("Unable to update memory"));
      await loadMemories();
    } catch (err) {
      setError(errorMessage(err, tr("Unable to update memory.")));
    }
  };

  const handleDelete = async (id: string) => {
    setError(null);
    setNotice(null);
    try {
      await deleteMemory(id, tr("Unable to delete memory"));
      setMemories((cur) => (cur ? cur.filter((m) => m.id !== id) : cur));
      if (editingId === id) {
        setEditingId(null);
        setDraft("");
      }
      setNotice(tr("Memory deleted."));
    } catch (err) {
      setError(errorMessage(err, tr("Unable to delete memory.")));
    }
  };

  const handlePromote = async (id: string, projectId: string) => {
    setError(null);
    setNotice(null);
    try {
      await promoteMemory(id, projectId, tr("Unable to move memory to team learnings"));
      setPromoteId(null);
      setNotice(tr("Moved to project team learnings."));
      await loadMemories();
    } catch (err) {
      setError(
        errorMessage(err, tr("Unable to move memory to team learnings.")),
      );
    }
  };

  const q = search.trim().toLowerCase();
  const allMemories = memories ?? [];
  const activeCount = allMemories.filter((m) => m.retired_at == null).length;
  const retiredCount = allMemories.filter((m) => m.retired_at != null).length;
  const visibleMemories = allMemories.filter((m) => {
    const isRetired = m.retired_at != null;
    if (filter === "active" && isRetired) return false;
    if (filter === "retired" && !isRetired) return false;
    if (q && !m.content.toLowerCase().includes(q) && !(m.kind ?? "").toLowerCase().includes(q)) {
      return false;
    }
    return true;
  });

  return (
    <SetSection
      title={tr("Memories")}
      intro={tr("Facts and preferences the assistant remembers in every chat you start.")}
    >
      {error ? (
        <p
          role="alert"
          className="mb-3 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-danger)_45%,transparent)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-danger)]"
        >
          {error}
        </p>
      ) : null}
      {notice ? (
        <p
          role="status"
          className="mb-3 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-accent)_40%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-text-primary)]"
        >
          {notice}
        </p>
      ) : null}

      <div className="mb-3 flex flex-wrap items-center justify-end gap-2">
        <Segmented
          label={tr("Memory status filter")}
          value={filter}
          options={[
            { value: "active", label: tr("Active ({0})", [activeCount]) },
            { value: "retired", label: tr("Retired ({0})", [retiredCount]) },
            { value: "all", label: tr("All ({0})", [allMemories.length]) },
          ]}
          onChange={setFilter}
        />
      </div>

      {!readOnlyViewer ? (
        <ConnPanel>
          <ConnPanelHead
            title={editingId ? tr("Edit memory") : tr("Add a memory")}
          />
              <ConnPanelSub>
                {tr("Good memories are standing rules and preferences (for example, “Deal names may contain intentional typos; never correct them”), not temporary numbers.")}
              </ConnPanelSub>
              <div className="grid gap-3">
                <ConnField label={tr("Memory")}>
                  <textarea
                    aria-label={tr("Memory text")}
                    rows={3}
                    placeholder={tr("Remember that deal names may contain intentional typos; never correct them.")}
                    value={draft}
                    onChange={(e) => setDraft(e.target.value)}
                    className={SETTINGS_INPUT}
                  />
                </ConnField>
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <label
                      htmlFor="settings-memory-kind"
                      className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
                    >
                      {tr("Kind")}
                    </label>
                    <select
                      id="settings-memory-kind"
                      value={kindDraft}
                      onChange={(e) => setKindDraft(e.target.value)}
                      className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.8rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                    >
                      {MEMORY_KINDS.map((k) => (
                        <option key={k} value={k}>
                          {memoryKindLabel(k)}
                        </option>
                      ))}
                    </select>
                  </div>
                  <div className="flex items-center gap-2">
                    {editingId ? (
                      <button
                        type="button"
                        className={btnClass({ sm: true, reveal: true })}
                        onClick={() => {
                          setEditingId(null);
                          setDraft("");
                          setKindDraft("fact");
                        }}
                      >
                        {tr("Cancel edit")}
                      </button>
                    ) : null}
                    <button
                      type="button"
                      disabled={!draft.trim() || busy}
                      onClick={() => void handleSave()}
                      className={btnClass({ variant: "primary", sm: true })}
                    >
                      {busy
                        ? tr("Saving…")
                        : editingId
                          ? tr("Save changes")
                          : tr("Add memory")}
                    </button>
                  </div>
                </div>
              </div>
            </ConnPanel>
          ) : null}

          <ConnPanel>
            <ConnPanelHead title={tr("Saved memories")}>
              {allMemories.length > 0 ? (
                <input
                  type="search"
                  aria-label={tr("Search memories")}
                  placeholder={tr("Filter memories…")}
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  className="w-48 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
                />
              ) : null}
            </ConnPanelHead>
            <ConnPanelSub>
              {tr("Pinned memories come first. Retire a memory to stop using it in new chats while keeping a record of it.")}
            </ConnPanelSub>

            {memories === null ? (
              <p className={LOADING}>{tr("Loading memories…")}</p>
            ) : visibleMemories.length === 0 ? (
              <ConnEmpty>
                {allMemories.length === 0
                  ? tr("No memories saved yet. Add one above or tell the assistant in chat “remember this: …”.")
                  : tr("No memories match this filter.")}
              </ConnEmpty>
            ) : (
              <ConnRows>
                {visibleMemories.map((memory) => {
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
                    ? new Date(memory.learned_at * 1000).toLocaleDateString(getLanguage())
                    : null;

                  return (
                    <div
                      key={memory.id}
                      data-testid="settings-memory-row"
                      className={`${ROW} ${retired ? "opacity-65" : ""}`}
                    >
                      <p className="m-0 whitespace-pre-wrap text-[0.85rem] leading-[1.55] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                        {memory.pinned ? (
                          <span title={tr("Pinned")}>📌 </span>
                        ) : null}
                        {memory.content}
                      </p>
                      <div className={ROW_HEAD}>
                        <div className="flex flex-wrap items-center gap-1.5 text-[0.72rem] text-[var(--color-text-muted)]">
                          {memory.pinned ? (
                            <ConnBadge variant="overridden">{tr("Pinned")}</ConnBadge>
                          ) : null}
                          {memory.kind ? (
                            <ConnBadge variant="neutral">{memoryKindLabel(memory.kind)}</ConnBadge>
                          ) : null}
                          {retired ? (
                            <ConnBadge variant="warn">{tr("Retired")}</ConnBadge>
                          ) : null}
                          <span>
                            {sourceLabel}
                            {learned ? tr(" · learned {0}", [learned]) : ""}
                          </span>
                        </div>
                        {!readOnlyViewer ? (
                          <div className="flex flex-wrap items-center gap-1.5">
                            {retired ? (
                              <button
                                type="button"
                                className={btnClass({ sm: true, reveal: true })}
                                onClick={() =>
                                  void handlePatch(memory.id, { retired: false })
                                }
                              >
                                {tr("Restore")}
                              </button>
                            ) : (
                              <>
                                <button
                                  type="button"
                                  className={btnClass({ sm: true })}
                                  onClick={() =>
                                    void handlePatch(memory.id, {
                                      pinned: !memory.pinned,
                                    })
                                  }
                                >
                                  {memory.pinned ? tr("Unpin") : tr("Pin")}
                                </button>
                                <button
                                  type="button"
                                  className={btnClass({ sm: true })}
                                  onClick={() => {
                                    setEditingId(memory.id);
                                    setDraft(memory.content);
                                    setKindDraft(memory.kind ?? "fact");
                                  }}
                                >
                                  {tr("Edit")}
                                </button>
                                <button
                                  type="button"
                                  className={btnClass({ sm: true })}
                                  title={tr("Keep for the record, stop giving to the assistant")}
                                  onClick={() =>
                                    void handlePatch(memory.id, { retired: true })
                                  }
                                >
                                  {tr("Retire")}
                                </button>
                                {memory.source !== "proposed" &&
                                teamProjects.length > 0 ? (
                                  <button
                                    type="button"
                                    className={btnClass({ sm: true })}
                                    onClick={() => {
                                      if (teamProjects.length === 1) {
                                        void handlePromote(
                                          memory.id,
                                          teamProjects[0].id,
                                        );
                                      } else {
                                        setPromoteId(
                                          promoteId === memory.id
                                            ? null
                                            : memory.id,
                                        );
                                      }
                                    }}
                                  >
                                    {tr("Move to team learnings")}
                                  </button>
                                ) : null}
                              </>
                            )}
                            <InlineConfirmButton
                              label={tr("Delete")}
                              onConfirm={() => void handleDelete(memory.id)}
                            />
                          </div>
                        ) : null}
                      </div>
                      {promoteId === memory.id && teamProjects.length > 1 ? (
                        <div className="mt-1 flex flex-wrap items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.75rem]">
                          <span className="text-[var(--color-text-secondary)]">
                            {tr("Move into project:")}
                          </span>
                          {teamProjects.map((p) => (
                            <button
                              key={p.id}
                              type="button"
                              className={btnClass({ sm: true, reveal: true })}
                              onClick={() => void handlePromote(memory.id, p.id)}
                            >
                              {p.name}
                            </button>
                          ))}
                          <button
                            type="button"
                            className={btnClass({ sm: true })}
                            onClick={() => setPromoteId(null)}
                          >
                            {tr("Cancel")}</button>
                        </div>
                      ) : null}
                    </div>
                  );
                })}
              </ConnRows>
            )}
          </ConnPanel>
    </SetSection>
  );
}
