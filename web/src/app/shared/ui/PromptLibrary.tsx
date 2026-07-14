"use client";

import { useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Icon } from "./Icon";
import {
  orchestratorApi,
  type PromptLibraryItem,
  type PromptLibraryWrite,
} from "@/app/shared/lib/orchestratorApi";
import { useDialogA11y } from "./useDialogA11y";

type Props = {
  currentText: string;
  onInsert: (content: string) => void;
  compact?: boolean;
};

const FIELD = "w-full rounded-md border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]";

const emptyDraft = (content = ""): PromptLibraryWrite => ({
  name: "",
  description: "",
  content,
  visibility: "private",
});

export function PromptLibrary({ currentText, onInsert, compact = false }: Props) {
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<PromptLibraryItem[]>([]);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<PromptLibraryWrite>(emptyDraft());
  const [saving, setSaving] = useState(false);
  const modalRef = useRef<HTMLDivElement | null>(null);
  const searchRef = useRef<HTMLInputElement | null>(null);

  useDialogA11y(open, modalRef, () => setOpen(false), { initialFocusRef: searchRef });

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await orchestratorApi.prompts();
      setItems(next);
      setSelectedID((id) => (id && next.some((p) => p.id === id) ? id : next[0]?.id ?? null));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not load prompt library");
    } finally {
      setLoading(false);
    }
  };

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((p) => `${p.name}\n${p.description ?? ""}\n${p.path ?? ""}`.toLowerCase().includes(q));
  }, [items, query]);
  const selected = items.find((p) => p.id === selectedID) ?? null;

  const beginNew = () => {
    setSelectedID(null);
    setDraft(emptyDraft(currentText));
    setEditing(true);
    setError(null);
  };

  const beginEdit = () => {
    if (!selected || selected.read_only || !selected.owned_by_caller) return;
    setDraft({
      name: selected.name,
      description: selected.description ?? "",
      content: selected.content,
      visibility: selected.visibility,
    });
    setEditing(true);
    setError(null);
  };

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      if (selected && !selected.read_only) await orchestratorApi.updatePrompt(selected.id, draft);
      else await orchestratorApi.createPrompt(draft);
      setEditing(false);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save prompt");
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    if (!selected || !selected.owned_by_caller || selected.read_only) return;
    if (!window.confirm(`Delete “${selected.name}”?`)) return;
    try {
      await orchestratorApi.deletePrompt(selected.id);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not delete prompt");
    }
  };

  const exportLibrary = () => {
    const payload = {
      format: "fleet-prompt-library",
      version: 1,
      exported_at: new Date().toISOString(),
      prompts: items,
    };
    const url = URL.createObjectURL(new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `fleet-prompts-${new Date().toISOString().slice(0, 10)}.json`;
    link.click();
    URL.revokeObjectURL(url);
  };

  return (
    <>
      <button
        type="button"
        aria-label="Open prompt library"
        title="Prompt library"
        className={
          compact
            ? "relative inline-flex h-[1.95rem] w-[1.95rem] shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition hover:bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            : "btn btn-secondary btn-small"
        }
        onClick={() => {
          setOpen(true);
          void load();
        }}
      >
        <Icon name="book" className="size-4" />
        {!compact ? <span style={{ marginLeft: 6 }}>Prompt library</span> : null}
      </button>

      {open
        ? createPortal(
        // Portalled to <body>: the composer sits inside transform-animated
        // ancestors, and position:fixed resolves against the nearest
        // transformed box — which used to shove this dialog half off-screen.
        <div className="fixed inset-0 z-[1200] flex items-center justify-center bg-black/60 p-0 sm:p-4" role="dialog" aria-modal="true" aria-label="Prompt library">
          <div ref={modalRef} className="flex h-[100dvh] w-full flex-col overflow-hidden border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-2xl sm:h-[min(46rem,92vh)] sm:w-[min(68rem,96vw)] sm:rounded-xl">
            <header className="flex flex-wrap items-center gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-4 py-3">
              <span aria-hidden="true" className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]">
                <Icon name="book" className="size-4.5" />
              </span>
              <div className="min-w-0 flex-1">
                <h2 className="m-0 text-base font-semibold text-[var(--color-text-primary)]">Prompt library</h2>
                <p className="m-0 truncate text-xs text-[var(--color-text-muted)]">Git-backed team prompts and workspace prompts, together.</p>
              </div>
              {/* The close stays pinned to the title row (top-right) even when
                  the action buttons wrap to a second row on phones. */}
              <button
                type="button"
                className="order-3 flex h-10 w-10 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-transparent text-[var(--color-text-muted)] transition hover:border-[var(--color-border)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                aria-label="Close prompt library"
                onClick={() => setOpen(false)}
              >
                <Icon name="close" className="size-4.5" />
              </button>
              <div className="order-4 flex w-full items-center gap-2 sm:order-2 sm:w-auto">
                <button type="button" className="btn btn-secondary btn-small" disabled={items.length === 0} onClick={exportLibrary}>Back up JSON</button>
                <button type="button" className="btn btn-primary btn-small" onClick={beginNew}>New prompt</button>
              </div>
            </header>

            {error ? <div className="mx-4 mt-3 rounded-md bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)] px-3 py-2 text-sm text-[var(--color-danger)]">{error}</div> : null}

            <div className="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(10rem,38%)_minmax(0,1fr)] md:grid-cols-[19rem_minmax(0,1fr)] md:grid-rows-1">
              <aside className="flex min-h-0 flex-col border-b border-[var(--color-border)] p-3 md:border-b-0 md:border-r">
                <input
                  ref={searchRef}
                  type="search"
                  aria-label="Search prompts"
                  placeholder="Search prompts…"
                  className={`${FIELD} mb-2`}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
                <div className="min-h-0 flex-1 overflow-y-auto">
                  {loading ? <p className="p-2 text-sm text-[var(--color-text-muted)]">Loading…</p> : null}
                  {!loading && visible.length === 0 ? <p className="p-2 text-sm text-[var(--color-text-muted)]">No matching prompts.</p> : null}
                  {visible.map((p) => {
                    const kind = p.source === "git" ? "git" : p.visibility === "workspace" ? "workspace" : "private";
                    return (
                      <button
                        key={p.id}
                        type="button"
                        className={`mb-1 flex w-full items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition ${selectedID === p.id ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)]" : "border-transparent hover:bg-[var(--color-surface-2)]"}`}
                        onClick={() => { setSelectedID(p.id); setEditing(false); }}
                      >
                        <span aria-hidden="true" className={`mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-md ${kind === "git" ? "bg-[color-mix(in_srgb,#7c6bff_16%,transparent)] text-[#a99cff]" : kind === "workspace" ? "bg-[var(--color-status-assigned-bg)] text-[var(--color-status-assigned-fg)]" : "bg-[var(--color-overlay-soft)] text-[var(--color-text-muted)]"}`}>
                          <Icon name={kind === "git" ? "layers" : kind === "workspace" ? "briefcase" : "lock"} className="size-3.5" />
                        </span>
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-sm font-medium text-[var(--color-text-primary)]">{p.name}</span>
                          {p.description ? (
                            <span className="mt-0.5 block truncate text-xs text-[var(--color-text-secondary)]">{p.description}</span>
                          ) : null}
                          <span className="mt-0.5 flex items-center gap-1.5 text-[0.68rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                            <span>{kind === "git" ? "Git" : kind === "workspace" ? "Workspace" : "Private"}</span>
                            {p.path ? <span className="truncate normal-case tracking-normal">{p.path}</span> : null}
                          </span>
                        </span>
                      </button>
                    );
                  })}
                </div>
              </aside>

              <main className="min-h-0 overflow-y-auto p-4">
                {editing ? (
                  <div className="grid gap-3">
                    <label className="grid gap-1 text-sm">Name<input className={FIELD} maxLength={120} value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} /></label>
                    <label className="grid gap-1 text-sm">Description <span className="text-xs text-[var(--color-text-muted)]">Optional — helps teammates find it.</span><input className={FIELD} maxLength={1024} value={draft.description} onChange={(e) => setDraft({ ...draft, description: e.target.value })} /></label>
                    <label className="grid gap-1 text-sm">Prompt<textarea className={`${FIELD} min-h-64 font-mono text-xs`} value={draft.content} onChange={(e) => setDraft({ ...draft, content: e.target.value })} /></label>
                    <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={draft.visibility === "workspace"} onChange={(e) => setDraft({ ...draft, visibility: e.target.checked ? "workspace" : "private" })} /> Share with this workspace</label>
                    <div className="flex justify-end gap-2">
                      <button type="button" className="btn btn-secondary" onClick={() => setEditing(false)}>Cancel</button>
                      <button type="button" className="btn btn-primary" disabled={saving || !draft.name.trim() || !draft.content.trim()} onClick={() => void save()}>{saving ? "Saving…" : "Save prompt"}</button>
                    </div>
                  </div>
                ) : selected ? (
                  <div className="flex min-h-full flex-col">
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div>
                        <h3 className="m-0 text-lg font-semibold text-[var(--color-text-primary)]">{selected.name}</h3>
                        <p className="mt-1 text-sm text-[var(--color-text-muted)]">{selected.description || "No description"}</p>
                      </div>
                      <div className="flex gap-2">
                        {!selected.read_only && selected.owned_by_caller ? <button type="button" className="btn btn-secondary btn-small" onClick={beginEdit}>Edit</button> : null}
                        {!selected.read_only && selected.owned_by_caller ? <button type="button" className="btn btn-danger btn-small" onClick={() => void remove()}>Delete</button> : null}
                      </div>
                    </div>
                    <pre className="mt-3 min-h-0 flex-1 overflow-auto whitespace-pre-wrap rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3 text-xs text-[var(--color-text-secondary)]">{selected.content}</pre>
                    <div className="mt-3 flex items-center justify-between gap-3">
                      <span className="text-xs text-[var(--color-text-muted)]">{selected.read_only ? `Tracked in ${selected.path}` : selected.visibility === "workspace" ? `Shared by ${selected.owner_username}` : "Only you can see this prompt"}</span>
                      <button type="button" className="btn btn-primary" onClick={() => { onInsert(selected.content); setOpen(false); }}>
                        <Icon name="arrow-up" className="mr-1.5 size-3.5 rotate-90" />
                        Use prompt
                      </button>
                    </div>
                  </div>
                ) : (
                  <div className="flex h-full flex-col items-center justify-center gap-2 text-sm text-[var(--color-text-muted)]">
                    <Icon name="book" className="size-8 opacity-40" />
                    <span>Choose a prompt on the left, or create one.</span>
                    <button type="button" className="btn btn-secondary btn-small" onClick={beginNew}>New prompt</button>
                  </div>
                )}
              </main>
            </div>
          </div>
        </div>,
          document.body,
        )
        : null}
    </>
  );
}
