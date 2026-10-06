"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Icon } from "./Icon";
import { TeamGlyph } from "./ShareGlyphs";
import { formatBytes } from "./formatters";
import {
  fetchProjectSources,
  filesLabel,
  formatDay,
  ownerFileUrl,
  setOutputShared,
  teamFileUrl,
  type SourcesFile,
  type SourcesGroup,
} from "./teamSharing";

// Sources (Fleet Projects sharing, B16 / B18; decisions #37–#39, #47): the
// project's files, grouped by the chat that made them. Sharing a chat shares
// its OUTPUTS (the files the agent presented in a reply); this panel is where
// an owner adjusts that per file afterwards, and where a teammate downloads
// what was shared with them.
//
//   - "From your team" first (one group per teammate chat, shared files only,
//     downloaded through the team-files route that re-checks every gate),
//     then "Your chats" — the caller's own chats, including teammate-branch
//     groups whose files are labeled "Your copy".
//   - The share toggle exists only on outputs of the caller's own TEAM-SHARED
//     chats. A private chat's group is download-only, with one line saying
//     how its files get shared (share the chat). Non-outputs — files the agent
//     wrote but never presented — are download-only everywhere and never
//     counted.
//   - Uploads never appear: the backend excludes them from the listing.
//   - The most recently active group is open by default; each person's
//     open/closed choices are remembered per project (my-state.sources_open).
//     "Manage" / "Manage in Sources" focus one group: open, scroll, highlight.

const enc = encodeURIComponent;

// The listing before groups existed: a flat list of the caller's own files.
// Grouped here so the panel still works against a server that predates the
// grouped shape (share state unknown → no toggles, nothing claimed shared).
type LegacyFile = {
  conversation_id: string;
  conversation_title: string;
  path: string;
  name: string;
  size: number;
  modified_at: number;
};

function groupLegacy(files: LegacyFile[]): SourcesGroup[] {
  const byChat = new Map<string, SourcesGroup>();
  for (const f of files) {
    let g = byChat.get(f.conversation_id);
    if (!g) {
      g = {
        conversation_id: f.conversation_id,
        title: f.conversation_title,
        owner_email: "",
        mine: true,
        team_visible: false,
        is_branch: false,
        last_active_at: 0,
        file_count: 0,
        shared_count: 0,
        files: [],
      };
      byChat.set(f.conversation_id, g);
    }
    g.files.push({
      path: f.path,
      name: f.name,
      size: f.size,
      modified_at: f.modified_at,
      shared: false,
      output: false,
      your_copy: false,
    });
    g.last_active_at = Math.max(g.last_active_at, f.modified_at);
  }
  return [...byChat.values()];
}

export type SourcesFocus = { conversationId: string; nonce: number };

export function ProjectSources({
  projectId,
  teamName,
  reloadKey,
  focus,
  sourcesOpen,
  onSourcesOpenChange,
}: {
  projectId: string;
  // The team the project is shared with; "" = personal project.
  teamName: string;
  // Bumped by the home after any share change, so counts and toggles re-read.
  reloadKey: number;
  focus: SourcesFocus | null;
  // Remembered open/closed groups (my-state); undefined until read.
  sourcesOpen: Record<string, boolean> | undefined;
  onSourcesOpenChange: (next: Record<string, boolean>) => void;
}) {
  const [groups, setGroups] = useState<SourcesGroup[] | null>(null);
  const [truncated, setTruncated] = useState(false);
  // A FAILURE is reported, not rendered as an empty state: "this project has
  // no files" and "we could not ask" look identical to a reader.
  const [error, setError] = useState<string | null>(null);
  // Local open/closed overrides, layered on the remembered map so a click
  // takes effect at once even before (or without) the PUT landing.
  // A Map, not an object: the keys are conversation ids, and writing them as
  // object properties would let a key like "__proto__" touch a prototype.
  const [localOpen, setLocalOpen] = useState<ReadonlyMap<string, boolean>>(
    () => new Map(),
  );
  const [highlight, setHighlight] = useState<string | null>(null);
  const [busyPath, setBusyPath] = useState<string | null>(null);
  const groupRefs = useRef(new Map<string, HTMLDivElement>());

  // Counts the per-file toggles that have landed. A listing read while one
  // landed may predate it, and applying it would repaint the file as it was
  // before the click; such a listing is dropped and read again instead.
  const toggleWrites = useRef(0);
  const [relist, setRelist] = useState(0);
  // The focused chat is sent with the listing so the server includes its
  // group even past its per-half cap. loadedFor records which focus the
  // groups on screen were read for: only THAT listing can say "not there".
  const focusId = focus?.conversationId ?? null;
  const [loadedFor, setLoadedFor] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        const writesAtStart = toggleWrites.current;
        try {
          const data = (await fetchProjectSources(projectId, focusId)) as {
            groups?: SourcesGroup[];
            truncated?: boolean;
            files?: LegacyFile[];
          };
          if (cancelled) return;
          if (toggleWrites.current !== writesAtStart) {
            setRelist((n) => n + 1);
            return;
          }
          setGroups(
            Array.isArray(data.groups)
              ? data.groups
              : groupLegacy(data.files ?? []),
          );
          setTruncated(Boolean(data.truncated));
          setLoadedFor(focusId);
          setError(null);
        } catch {
          if (cancelled) return;
          setGroups([]);
          setLoadedFor(focusId);
          setError("Couldn’t load this project’s files.");
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [projectId, reloadKey, relist, focusId]);

  // Teammates' groups first, then the caller's own; most recently active
  // first within each.
  const ordered = useMemo(() => {
    const all = (groups ?? []).filter((g) => g.files.length > 0);
    const byRecent = (a: SourcesGroup, b: SourcesGroup) =>
      b.last_active_at - a.last_active_at;
    return {
      theirs: all.filter((g) => !g.mine).sort(byRecent),
      mine: all.filter((g) => g.mine).sort(byRecent),
    };
  }, [groups]);

  const mostRecentId = useMemo(() => {
    let best: SourcesGroup | null = null;
    for (const g of [...ordered.theirs, ...ordered.mine]) {
      if (!best || g.last_active_at > best.last_active_at) best = g;
    }
    return best?.conversation_id ?? null;
  }, [ordered]);

  const isOpen = (id: string): boolean => {
    const local = localOpen.get(id);
    if (local !== undefined) return local;
    if (sourcesOpen && Object.hasOwn(sourcesOpen, id)) {
      return Boolean(sourcesOpen[id]);
    }
    return id === mostRecentId;
  };

  // The remembered map, this visit's overrides, and one more change — the
  // local view. The PUT carries the one change only (persistOpen).
  const withOpen = (id: string, open: boolean): Map<string, boolean> => {
    const next = new Map<string, boolean>(Object.entries(sourcesOpen ?? {}));
    for (const [k, v] of localOpen) next.set(k, v);
    next.set(id, open);
    return next;
  };

  // Only the toggled key goes to the server, which merges it into the stored
  // map: sending this tab's whole cached map would overwrite another tab's
  // (or device's) choices with stale values. Built from entries, not by a
  // computed-property write on a plain object.
  const persistOpen = (id: string, open: boolean) => {
    setLocalOpen(withOpen(id, open));
    onSourcesOpenChange(Object.fromEntries([[id, open]]));
  };

  // Focus: open the group, scroll it into view, highlight it briefly. Runs
  // once per request (nonce), and again once the listing lands if the group
  // was not on screen yet.
  const [handledFocus, setHandledFocus] = useState<number | null>(null);
  const focusFound = (groups ?? []).some((g) => g.conversation_id === focusId);
  const focusReady =
    focus !== null &&
    groups !== null &&
    handledFocus !== focus.nonce &&
    focusFound;
  // The listing read FOR this focus does not have it (the chat has no files,
  // or the caller cannot see it): stop waiting — nothing to open, nothing
  // highlighted, and a later reload does not jump to it out of the blue.
  if (
    focus !== null &&
    groups !== null &&
    handledFocus !== focus.nonce &&
    !focusFound &&
    loadedFor === focusId
  ) {
    setHandledFocus(focus.nonce);
  }
  // Persisting the opened group goes to the parent from an effect: the
  // focus is noticed during render, where a parent update is not allowed.
  const [persistQueue, setPersistQueue] = useState<Record<string, boolean> | null>(null);
  if (focusReady && focus) {
    setHandledFocus(focus.nonce);
    if (!isOpen(focus.conversationId)) {
      setLocalOpen(withOpen(focus.conversationId, true));
      setPersistQueue(Object.fromEntries([[focus.conversationId, true]]));
    }
    setHighlight(focus.conversationId);
  }
  useEffect(() => {
    if (persistQueue) onSourcesOpenChange(persistQueue);
  }, [persistQueue, onSourcesOpenChange]);
  useEffect(() => {
    if (!highlight) return;
    const el = groupRefs.current.get(highlight);
    el?.scrollIntoView?.({ block: "center", behavior: "smooth" });
    const t = window.setTimeout(() => setHighlight(null), 2600);
    return () => window.clearTimeout(t);
  }, [highlight]);

  const toggleShared = async (g: SourcesGroup, f: SourcesFile) => {
    if (busyPath) return;
    setBusyPath(`${g.conversation_id}/${f.path}`);
    setError(null);
    try {
      const want = !f.shared;
      const res = await setOutputShared(g.conversation_id, f.path, want);
      toggleWrites.current += 1;
      const sharedBy = new Map(res.outputs.map((o) => [o.path, o.shared]));
      // The write succeeded, so the toggled path's exclusion now matches the
      // request even when the response omits it (the file vanished or fell
      // out of discovery mid-write): a stale row would misstate what a
      // recreated file would expose.
      if (!sharedBy.has(f.path)) sharedBy.set(f.path, want);
      setGroups((prev) =>
        (prev ?? []).map((x) =>
          x.conversation_id !== g.conversation_id
            ? x
            : {
                ...x,
                shared_count: res.shared_count,
                files: x.files.map((y) =>
                  sharedBy.has(y.path) ? { ...y, shared: Boolean(sharedBy.get(y.path)) } : y,
                ),
              },
        ),
      );
    } catch {
      setError(
        f.shared
          ? `Couldn’t stop sharing “${f.name}”.`
          : `Couldn’t share “${f.name}”.`,
      );
    } finally {
      setBusyPath(null);
    }
  };

  // Downloads are a plain `<a href download>` navigation, not a fetch into a
  // Blob: the browser streams the file straight to disk, so a large output
  // never has to fit in this tab's memory first. The trade-off is where a
  // failure shows — a file removed or unshared since this list loaded is
  // reported by the browser's own downloads UI, not by an in-app message.
  const downloadHref = (g: SourcesGroup, f: SourcesFile) =>
    g.mine ? ownerFileUrl(g.conversation_id, f.path) : teamFileUrl(g.conversation_id, f.path);

  const total =
    ordered.theirs.reduce((n, g) => n + g.files.length, 0) +
    ordered.mine.reduce((n, g) => n + g.files.length, 0);

  const renderGroup = (g: SourcesGroup) => {
    const open = isOpen(g.conversation_id);
    const files = [...g.files].sort((a, b) => b.modified_at - a.modified_at);
    // Visibility marks only mean something in a shared project.
    const showVis = g.mine && Boolean(teamName);
    const canToggle = g.mine && g.team_visible && Boolean(teamName);
    const count = g.mine
      ? `${filesLabel(g.file_count)}${g.team_visible && teamName ? ` · ${g.shared_count} shared` : ""}`
      : filesLabel(files.length);
    const note =
      g.mine && teamName && !g.team_visible
        ? "Share this chat and its files go with it."
        : "";
    const bodyId = `src-body-${enc(g.conversation_id)}`;
    return (
      <div
        key={g.conversation_id}
        ref={(el) => {
          if (el) groupRefs.current.set(g.conversation_id, el);
          else groupRefs.current.delete(g.conversation_id);
        }}
        data-testid="sources-group"
        data-conversation-id={g.conversation_id}
        className={[
          "rounded-[var(--radius-md)] transition-colors",
          highlight === g.conversation_id
            ? "bg-[color-mix(in_srgb,var(--color-primary)_16%,transparent)]"
            : "",
        ].join(" ")}
      >
        <button
          type="button"
          aria-expanded={open}
          aria-controls={open ? bodyId : undefined}
          className="flex w-full items-start gap-2 rounded-[var(--radius-md)] px-1.5 py-1.5 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={() => persistOpen(g.conversation_id, !open)}
        >
          <Icon
            name="folder"
            className={[
              "mt-0.5 size-4 shrink-0",
              open ? "text-[var(--color-accent)]" : "text-[var(--color-text-muted)]",
            ].join(" ")}
          />
          <span className="min-w-0 flex-1">
            <span className="block truncate text-[0.8rem] font-medium text-[var(--color-text-primary)]">
              {g.title || "Untitled"}
            </span>
            <span className="mt-0.5 flex flex-wrap items-center gap-1 text-[0.68rem] text-[var(--color-text-muted)]">
              {showVis ? (
                g.team_visible ? (
                  <span className="inline-flex items-center gap-1 text-[var(--color-text-secondary)]">
                    <TeamGlyph className="size-3" />
                    {teamName}
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1">
                    <Icon name="lock" className="size-2.5" />
                    Only you
                  </span>
                )
              ) : null}
              {!g.mine ? (
                <span className="inline-flex min-w-0 max-w-full items-center gap-1 text-[var(--color-text-secondary)]">
                  <TeamGlyph className="size-3 shrink-0" />
                  <span className="truncate">Shared by {g.owner_email}</span>
                </span>
              ) : null}
              {showVis || !g.mine ? <span aria-hidden="true">·</span> : null}
              <span>{count}</span>
            </span>
          </span>
        </button>
        {open ? (
          <div id={bodyId} className="pb-1 pl-6">
            {files.map((f) => {
              const key = `${g.conversation_id}/${f.path}`;
              const sharedMark = canToggle && f.output && f.shared;
              const meta = [
                f.your_copy ? "Your copy" : "",
                formatBytes(f.size),
                f.your_copy && g.branched_at
                  ? formatDay(g.branched_at)
                  : formatDay(f.modified_at),
              ]
                .filter(Boolean)
                .join(" · ");
              return (
                <div
                  key={key}
                  data-testid="sources-file"
                  className="group flex items-center gap-1.5 rounded-[var(--radius-md)] px-1.5 py-1 transition hover:bg-[var(--color-overlay-soft)]"
                >
                  <span className="min-w-0 flex-1">
                    <span
                      className="block break-all text-[0.78rem] leading-[1.35] text-[var(--color-text-primary)]"
                      title={f.name}
                    >
                      {f.name}
                    </span>
                    <span className="block font-[family-name:var(--font-code)] text-[0.65rem] text-[var(--color-text-muted)]">
                      {meta}
                      {sharedMark ? (
                        <span className="text-[var(--color-accent)]"> · Shared</span>
                      ) : null}
                    </span>
                  </span>
                  {canToggle && f.output ? (
                    <button
                      type="button"
                      aria-pressed={f.shared}
                      // One write at a time, and every toggle says so: a
                      // click elsewhere mid-write would otherwise be dropped
                      // silently (toggleShared is single-flight).
                      disabled={busyPath !== null}
                      aria-busy={busyPath === key || undefined}
                      title={
                        f.shared
                          ? `Shared with ${teamName}. Click to stop sharing`
                          : `Only you. Click to share with ${teamName}`
                      }
                      aria-label={
                        f.shared
                          ? `${f.name} is shared with ${teamName}. Stop sharing`
                          : `${f.name} is only you. Share with ${teamName}`
                      }
                      className={[
                        "inline-flex size-7 shrink-0 items-center justify-center rounded-[var(--radius-md)] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50",
                        f.shared
                          ? "bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)] hover:bg-[color-mix(in_srgb,var(--color-accent)_28%,transparent)]"
                          : "text-[var(--color-text-muted)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]",
                      ].join(" ")}
                      onClick={() => void toggleShared(g, f)}
                    >
                      {f.shared ? (
                        <TeamGlyph className="size-3.5" />
                      ) : (
                        <Icon name="lock" className="size-3.5" />
                      )}
                    </button>
                  ) : null}
                  <a
                    href={downloadHref(g, f)}
                    download={f.name}
                    rel="noreferrer noopener"
                    aria-label={`Download ${f.name}`}
                    title="Download"
                    className="inline-flex size-7 shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                  >
                    <Icon name="download" className="size-3.5" />
                  </a>
                </div>
              );
            })}
            {note ? (
              <p className="m-0 px-1.5 pt-1 text-[0.7rem] leading-[1.45] text-[var(--color-text-muted)]">
                {note}
              </p>
            ) : null}
          </div>
        ) : null}
      </div>
    );
  };

  const sectionLabel = (text: string) => (
    <p className="m-0 px-1.5 pb-0.5 pt-2 font-[family-name:var(--font-code)] text-[0.62rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
      {text}
    </p>
  );

  return (
    <section
      aria-label="Sources"
      data-testid="project-sources"
      className="rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4"
    >
      <h2 className="mb-1 flex items-center gap-1.5 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
        <Icon name="paperclip" className="size-3.5 shrink-0" />
        Sources
        {total > 0 ? (
          <span className="ml-auto font-[family-name:var(--font-code)] text-[0.68rem] font-normal text-[var(--color-text-muted)]">
            {filesLabel(total)}
          </span>
        ) : null}
      </h2>
      <p className="m-0 mb-2 text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
        {teamName
          ? "Your files and your team’s. Files in your shared chats are shared automatically. Adjust that here."
          : "Files from your chats in this project appear here."}
      </p>
      {error ? (
        <p
          role="alert"
          className="mb-2 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-2 py-1.5 text-[0.72rem] leading-[1.5] text-[var(--color-danger)]"
        >
          {error}
        </p>
      ) : null}
      {groups === null ? (
        <p className="m-0 text-[0.8rem] text-[var(--color-text-muted)]">Loading…</p>
      ) : (
        <div className="flex flex-col gap-0.5">
          {ordered.theirs.length > 0 ? sectionLabel("From your team") : null}
          {ordered.theirs.map(renderGroup)}
          {ordered.theirs.length > 0 && ordered.mine.length > 0
            ? sectionLabel("Your chats")
            : null}
          {ordered.mine.map(renderGroup)}
          {truncated ? (
            <p className="m-0 px-1.5 pt-1 text-[0.7rem] text-[var(--color-text-muted)]">
              Showing the newest files only.
            </p>
          ) : null}
        </div>
      )}
    </section>
  );
}
