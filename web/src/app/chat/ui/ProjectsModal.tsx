"use client";

import { useCallback, useEffect, useState } from "react";
import { Icon } from "@/app/shared/ui/Icon";

// Projects / Spaces modal (#509): create/edit shared team workspaces — the
// binding object for standing instructions, curated connectors, default
// persona/model, shared memory, and team membership. Conversations started
// from here inherit the project's context automatically. Self-contained: owns
// its own fetching against the /api/projects proxies.

export type Project = {
  id: string;
  owner_email: string;
  name: string;
  instructions?: string;
  team_id?: string;
  default_persona?: string;
  default_model?: string;
  mcp_servers: string[];
  created_at: number;
  updated_at: number;
};

type ProjectMemory = {
  id: string;
  content: string;
  kind?: string;
  user_email?: string;
  retired_at?: number;
};

export function ProjectsModal({
  userEmail,
  onClose,
  onStartChat,
}: {
  userEmail: string;
  onClose: () => void;
  onStartChat: (projectID: string) => void;
}) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [memories, setMemories] = useState<ProjectMemory[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Draft form state (create or edit).
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [instructions, setInstructions] = useState("");
  const [teamShared, setTeamShared] = useState(false);
  const [memoryDraft, setMemoryDraft] = useState("");

  const selected = projects.find((p) => p.id === selectedId) ?? null;
  const isOwner = selected ? selected.owner_email === userEmail : false;

  const load = useCallback(async () => {
    try {
      const res = await fetch("/api/projects", { cache: "no-store" });
      if (!res.ok) throw new Error(await res.text());
      const data = (await res.json()) as { projects?: Project[] };
      setProjects(data.projects ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to load projects.");
    }
  }, []);

  const loadMemories = useCallback(async (id: string) => {
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}/memories`, { cache: "no-store" });
      if (!res.ok) throw new Error(await res.text());
      const data = (await res.json()) as { memories?: ProjectMemory[] };
      setMemories(data.memories ?? []);
    } catch {
      setMemories([]);
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void load();
    });
    return () => {
      cancelled = true;
    };
  }, [load]);

  useEffect(() => {
    if (!selectedId) return;
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void loadMemories(selectedId);
    });
    return () => {
      cancelled = true;
    };
  }, [selectedId, loadMemories]);

  const openEditor = (p: Project | null) => {
    setEditing(true);
    setName(p?.name ?? "");
    setInstructions(p?.instructions ?? "");
    setTeamShared(!!p?.team_id);
    setSelectedId(p?.id ?? null);
  };

  const save = async () => {
    if (busy || !name.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const body = JSON.stringify({ name: name.trim(), instructions, team_shared: teamShared });
      const res = selectedId
        ? await fetch(`/api/projects/${encodeURIComponent(selectedId)}`, {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body,
          })
        : await fetch("/api/projects", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body,
          });
      if (!res.ok) throw new Error(await res.text());
      const p = (await res.json()) as Project;
      setEditing(false);
      setSelectedId(p.id);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to save project.");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    if (!window.confirm("Delete this project? Conversations are kept (detached); shared project memories are removed.")) return;
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!res.ok) throw new Error(await res.text());
      setSelectedId(null);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to delete project.");
    }
  };

  const addMemory = async () => {
    const content = memoryDraft.trim();
    if (!content || !selectedId) return;
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(selectedId)}/memories`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content }),
      });
      if (!res.ok) throw new Error(await res.text());
      setMemoryDraft("");
      await loadMemories(selectedId);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to add memory.");
    }
  };

  const deleteMemory = async (memID: string) => {
    if (!selectedId) return;
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(selectedId)}/memories/${encodeURIComponent(memID)}`, { method: "DELETE" });
      if (!res.ok) throw new Error(await res.text());
      await loadMemories(selectedId);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to remove memory.");
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <button aria-label="Close projects" className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]" type="button" onClick={onClose} />
      <div className="relative z-10 flex max-h-[88vh] w-full max-w-[40rem] flex-col gap-4 overflow-hidden rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">Projects</h2>
            <p className="mt-1 text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
              Shared workspaces: standing instructions, shared memory, and defaults every chat in the project inherits.
            </p>
          </div>
          <button type="button" aria-label="Close projects" className="inline-flex size-9 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]" onClick={onClose}>
            <Icon name="close" className="size-4" />
          </button>
        </div>

        {error ? (
          <div className="rounded-[0.75rem] border border-[var(--color-danger)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger)]">
            {error}
          </div>
        ) : null}

        {editing ? (
          <div className="grid gap-2">
            <input
              className="w-full rounded-[0.9rem] border border-[var(--color-border-strong)] bg-transparent px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              placeholder="Project name"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <textarea
              className="min-h-24 w-full resize-y rounded-[0.9rem] border border-[var(--color-border-strong)] bg-transparent px-3 py-2 text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
              placeholder="Standing instructions every chat in this project follows…"
              value={instructions}
              onChange={(e) => setInstructions(e.target.value)}
            />
            <label className="flex items-center gap-2 text-[0.8125rem] text-[var(--color-text-secondary)]">
              <input type="checkbox" checked={teamShared} onChange={(e) => setTeamShared(e.target.checked)} />
              Share with my team (members can chat in it and read/write its shared memory)
            </label>
            <div className="flex items-center justify-end gap-2">
              <button type="button" className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]" onClick={() => setEditing(false)}>
                Cancel
              </button>
              <button type="button" className="rounded-full bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40" disabled={busy || !name.trim()} onClick={() => void save()}>
                {busy ? "Saving…" : selectedId ? "Save changes" : "Create project"}
              </button>
            </div>
          </div>
        ) : (
          <button type="button" className="self-start rounded-full bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80" onClick={() => openEditor(null)}>
            New project
          </button>
        )}

        <div className="min-h-0 flex-1 overflow-y-auto pr-1">
          {projects.length === 0 && !editing ? (
            <p className="rounded-[0.9rem] border border-dashed border-[var(--color-border)] px-3 py-4 text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
              No projects yet. A project turns a folder into a shared team workspace.
            </p>
          ) : (
            <div className="grid gap-2">
              {projects.map((p) => (
                <div key={p.id} className={`rounded-[0.9rem] border p-3 ${p.id === selectedId ? "border-[var(--color-accent)]" : "border-[var(--color-border)]"} bg-[var(--color-overlay-soft)]`}>
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <button type="button" className="text-[0.9rem] font-medium text-[var(--color-text-primary)] hover:underline" onClick={() => setSelectedId(p.id === selectedId ? null : p.id)}>
                      {p.name}
                    </button>
                    <div className="flex items-center gap-3 text-[0.72rem] text-[var(--color-text-muted)]">
                      {p.team_id ? <span title={`Shared with team ${p.team_id}`}>team: {p.team_id}</span> : <span>personal</span>}
                      <button type="button" className="hover:text-[var(--color-text-primary)]" onClick={() => onStartChat(p.id)}>
                        Start chat
                      </button>
                      {p.owner_email === userEmail ? (
                        <>
                          <button type="button" className="hover:text-[var(--color-text-primary)]" onClick={() => openEditor(p)}>
                            Edit
                          </button>
                          <button type="button" className="hover:text-[var(--color-danger)]" onClick={() => void remove(p.id)}>
                            Delete
                          </button>
                        </>
                      ) : null}
                    </div>
                  </div>
                  {p.instructions ? (
                    <p className="mt-1 whitespace-pre-wrap text-[0.78rem] leading-[1.5] text-[var(--color-text-secondary)]">{p.instructions}</p>
                  ) : null}
                  {p.id === selectedId ? (
                    <div className="mt-3 border-t border-[var(--color-border)] pt-2">
                      <p className="mb-1 text-[0.72rem] font-medium text-[var(--color-text-muted)]">
                        Shared project memory {isOwner || selected?.team_id ? "(visible to all members)" : ""}
                      </p>
                      {memories.length === 0 ? (
                        <p className="text-[0.75rem] text-[var(--color-text-muted)]">No shared memories yet.</p>
                      ) : (
                        <ul className="grid gap-1">
                          {memories.map((m) => (
                            <li key={m.id} className="flex items-start justify-between gap-2 text-[0.78rem] text-[var(--color-text-secondary)]">
                              <span>
                                {m.content}
                                {m.user_email ? <span className="ml-1 text-[0.68rem] text-[var(--color-text-muted)]">— {m.user_email}</span> : null}
                              </span>
                              <button type="button" className="text-[0.7rem] text-[var(--color-text-muted)] hover:text-[var(--color-danger)]" onClick={() => void deleteMemory(m.id)}>
                                Remove
                              </button>
                            </li>
                          ))}
                        </ul>
                      )}
                      <div className="mt-2 flex items-center gap-2">
                        <input
                          className="min-w-0 flex-1 rounded-[0.7rem] border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                          placeholder="Add a shared fact the whole project should know…"
                          value={memoryDraft}
                          onChange={(e) => setMemoryDraft(e.target.value)}
                        />
                        <button type="button" className="rounded-full border border-[var(--color-border-strong)] px-2.5 py-1 text-[0.72rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-40" disabled={!memoryDraft.trim()} onClick={() => void addMemory()}>
                          Add
                        </button>
                      </div>
                    </div>
                  ) : null}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
