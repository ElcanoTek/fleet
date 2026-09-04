"use client";

// Settings → Shared files — the cross-chat shared file library. Admins stage
// files once; every conversation's agent can then read them (mounted read-only
// into each sandbox at shared/<name>), and every member can browse/download
// them here.
//
// Unlike the /settings/admin/* pages this one is member-visible by design:
// members get the read-only library view (list + downloads), admins
// additionally get upload / rename / move / describe / delete. useIsAdmin is
// VISIBILITY only — every mutation is independently authorized upstream, so a
// member who conjures the controls still can't change anything. While the
// admin probe is undecided the read-only view renders (never a blank page).

import { useRef, useState } from "react";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { formatBytes, screenFilesForUpload } from "@/app/lib/uploadLimits";
import {
  btnClass,
  CodeChip,
  InlineConfirmButton,
  SETTINGS_INPUT,
} from "../ui/atoms";
import {
  ConnEmpty,
  ConnField,
  ConnForm,
  ConnGroup,
  ConnGroupHead,
  ConnPanel,
  ConnPanelHead,
  ConnPanelSub,
  ConnRow,
  ConnRows,
  SetSection,
} from "../ui/panels";
import { useIsAdmin } from "../useIsAdmin";

type SharedFile = {
  id: string;
  name: string;
  folder: string;
  description: string;
  size_bytes: number;
  content_type?: string;
  sha256?: string;
  uploaded_by?: string;
  // Unix SECONDS (matches the Go rows).
  created_at: number;
  updated_at: number;
};

type Library = {
  files: SharedFile[];
  total_bytes: number;
  // 0 = unlimited (the shared_files_max_total_mb admin setting).
  max_total_bytes: number;
};

async function fetchLibrary(): Promise<Library | null> {
  const response = await fetch("/api/shared-files", { cache: "no-store" });
  if (response.status === 401) {
    // Same navigation-as-a-call shape the features page uses.
    window.location.assign("/login");
    return null;
  }
  if (!response.ok) {
    const text = (await response.text()).trim();
    throw new Error(text || `Shared files request failed: ${response.status}`);
  }
  const data = (await response.json()) as Partial<Library>;
  return {
    files: data.files ?? [],
    total_bytes: data.total_bytes ?? 0,
    max_total_bytes: data.max_total_bytes ?? 0,
  };
}

// '' (library root) sorts first; the rest alphabetically, files by name.
function groupByFolder(files: SharedFile[]): { folder: string; files: SharedFile[] }[] {
  const byFolder = new Map<string, SharedFile[]>();
  for (const f of files) {
    const list = byFolder.get(f.folder) ?? [];
    list.push(f);
    byFolder.set(f.folder, list);
  }
  return [...byFolder.entries()]
    .sort(([a], [b]) => (a === "" ? -1 : b === "" ? 1 : a.localeCompare(b)))
    .map(([folder, group]) => ({
      folder,
      files: [...group].sort((a, b) => a.name.localeCompare(b.name)),
    }));
}

function formatDate(unixSeconds: number): string {
  return new Date(unixSeconds * 1000).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

async function readError(res: Response, fallback: string): Promise<string> {
  // The Go handlers reply with short plain-text reasons (bad folder,
  // duplicate name, over the cap) — show those words, not a bare status.
  const text = (await res.text()).trim();
  return text.length > 0 && text.length <= 300 ? text : fallback;
}

export default function SharedFilesPage() {
  const admin = useIsAdmin();
  const { data: library, loading, error, reload } = useCancellableFetch<Library | null>(
    fetchLibrary,
    [],
  );
  // One mutation at a time; upload/delete failures surface in a page banner,
  // edit failures inline in the row's editor.
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [editing, setEditing] = useState<{
    id: string;
    name: string;
    folder: string;
    description: string;
  } | null>(null);
  const [editError, setEditError] = useState<string | null>(null);

  const isAdmin = admin === "admin";
  const groups = library ? groupByFolder(library.files) : [];
  const folders = [...new Set((library?.files ?? []).map((f) => f.folder).filter(Boolean))].sort();

  // A mutation succeeded: refetch rather than patch state — totals, folder
  // grouping, and server-normalized fields all come back authoritative.
  const refresh = async () => {
    await reload();
  };

  const remove = async (id: string) => {
    setBusy(true);
    setActionError(null);
    try {
      const res = await fetch(`/api/shared-files/${encodeURIComponent(id)}`, {
        method: "DELETE",
      });
      if (res.status === 401) {
        window.location.assign("/login");
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Delete failed: ${res.status}`));
      await refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Delete failed.");
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async (file: SharedFile) => {
    if (!editing) return;
    // Send only what changed — the PATCH contract treats presence as intent
    // (empty folder = move to root, empty description = clear).
    const patch: Record<string, string> = {};
    const name = editing.name.trim();
    const folder = editing.folder.trim();
    if (name !== file.name) patch.name = name;
    if (folder !== file.folder) patch.folder = folder;
    if (editing.description !== file.description) patch.description = editing.description;
    if (Object.keys(patch).length === 0) {
      setEditing(null);
      return;
    }
    setBusy(true);
    setEditError(null);
    try {
      const res = await fetch(`/api/shared-files/${encodeURIComponent(file.id)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      if (res.status === 401) {
        window.location.assign("/login");
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Save failed: ${res.status}`));
      setEditing(null);
      await refresh();
    } catch (err) {
      setEditError(err instanceof Error ? err.message : "Save failed.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <SetSection
      title="Shared files"
      intro={
        <>
          A file library every conversation shares: the agent can read each file at{" "}
          <CodeChip>shared/&lt;name&gt;</CodeChip> (read-only) in every conversation&apos;s
          sandbox, and any member can download it here.
          {isAdmin ? " Only admins change the library." : " Admins manage the library."}
        </>
      }
    >
      <div data-testid="shared-files-panel">
        {actionError ? (
          <NoticeBanner tone="danger" className="mb-[0.85rem]" role="alert">
            {actionError}
          </NoticeBanner>
        ) : null}

        {error ? (
          <div className="flex items-center gap-3">
            <NoticeBanner tone="danger" className="flex-1">
              {error}
            </NoticeBanner>
            <button
              type="button"
              onClick={() => void reload()}
              className={btnClass({ sm: true, reveal: true })}
            >
              Retry
            </button>
          </div>
        ) : loading || library === null ? (
          <p className="text-[0.8rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : (
          <>
            <UsageMeter library={library} isAdmin={isAdmin} />
            {isAdmin ? (
              <UploadPanel
                library={library}
                busy={busy}
                setBusy={setBusy}
                onUploaded={() => void refresh()}
              />
            ) : null}

            {library.files.length === 0 ? (
              <ConnEmpty>
                No shared files yet.{" "}
                {isAdmin
                  ? "Upload some above to make them readable in every conversation."
                  : "An admin can add some from this page."}
              </ConnEmpty>
            ) : (
              groups.map((group) => (
                <ConnGroup key={group.folder || "\u0000root"}>
                  <ConnGroupHead
                    title={group.folder === "" ? "Library root" : group.folder}
                  />
                  <ConnPanel>
                    <ConnRows>
                      {group.files.map((file) => (
                        <ConnRow
                          key={file.id}
                          name={file.name}
                          sub={
                            <>
                              {formatBytes(file.size_bytes)}
                              {file.description ? <> · {file.description}</> : null}
                              {file.uploaded_by ? <> · uploaded by {file.uploaded_by}</> : null}
                              {" · updated "}
                              {formatDate(file.updated_at)}
                            </>
                          }
                          actions={
                            <>
                              <a
                                href={`/api/shared-files/${encodeURIComponent(file.id)}/download`}
                                aria-label={`Download ${file.name}`}
                                className={btnClass({ sm: true, reveal: true })}
                              >
                                Download
                              </a>
                              {isAdmin ? (
                                <>
                                  <button
                                    type="button"
                                    aria-label={`Edit ${file.name}`}
                                    aria-expanded={editing?.id === file.id}
                                    disabled={busy}
                                    data-testid={`edit-${file.id}`}
                                    onClick={() => {
                                      setEditError(null);
                                      setEditing((prev) =>
                                        prev?.id === file.id
                                          ? null
                                          : {
                                              id: file.id,
                                              name: file.name,
                                              folder: file.folder,
                                              description: file.description,
                                            },
                                      );
                                    }}
                                    className={btnClass({ sm: true })}
                                  >
                                    Edit
                                  </button>
                                  <InlineConfirmButton
                                    label="Delete"
                                    onConfirm={() => void remove(file.id)}
                                    disabled={busy}
                                    testId={`delete-${file.id}`}
                                  />
                                </>
                              ) : null}
                            </>
                          }
                          detail={
                            isAdmin && editing?.id === file.id ? (
                              <div className="mt-[0.55rem]">
                                <ConnForm className="mb-0!">
                                  <ConnField label="Name" grow>
                                    <input
                                      type="text"
                                      value={editing.name}
                                      disabled={busy}
                                      data-testid={`edit-name-${file.id}`}
                                      onChange={(e) =>
                                        setEditing((prev) =>
                                          prev ? { ...prev, name: e.target.value } : prev,
                                        )
                                      }
                                      className={SETTINGS_INPUT}
                                    />
                                  </ConnField>
                                  <ConnField label="Folder (empty = library root)">
                                    <input
                                      type="text"
                                      value={editing.folder}
                                      disabled={busy}
                                      list="shared-file-folders"
                                      data-testid={`edit-folder-${file.id}`}
                                      onChange={(e) =>
                                        setEditing((prev) =>
                                          prev ? { ...prev, folder: e.target.value } : prev,
                                        )
                                      }
                                      className={SETTINGS_INPUT}
                                    />
                                  </ConnField>
                                  <ConnField label="Description (empty = clear)" grow>
                                    <input
                                      type="text"
                                      value={editing.description}
                                      disabled={busy}
                                      data-testid={`edit-description-${file.id}`}
                                      onChange={(e) =>
                                        setEditing((prev) =>
                                          prev ? { ...prev, description: e.target.value } : prev,
                                        )
                                      }
                                      className={SETTINGS_INPUT}
                                    />
                                  </ConnField>
                                  <button
                                    type="button"
                                    disabled={busy || editing.name.trim() === ""}
                                    data-testid={`save-edit-${file.id}`}
                                    onClick={() => void saveEdit(file)}
                                    className={btnClass({ variant: "primary", sm: true })}
                                  >
                                    Save
                                  </button>
                                  <button
                                    type="button"
                                    disabled={busy}
                                    onClick={() => setEditing(null)}
                                    className={btnClass({ sm: true, reveal: true })}
                                  >
                                    Cancel
                                  </button>
                                </ConnForm>
                                {editError ? (
                                  <p
                                    className="m-0 mt-[0.4rem] text-[0.73rem] text-[var(--color-danger)]"
                                    role="alert"
                                  >
                                    {editError}
                                  </p>
                                ) : null}
                              </div>
                            ) : undefined
                          }
                        />
                      ))}
                    </ConnRows>
                  </ConnPanel>
                </ConnGroup>
              ))
            )}
            {/* One shared folder-suggestion list for the move/upload inputs. */}
            <datalist id="shared-file-folders">
              {folders.map((f) => (
                <option key={f} value={f}>
                  {f}
                </option>
              ))}
            </datalist>
          </>
        )}
      </div>
    </SetSection>
  );
}

function UsageMeter({ library, isAdmin }: { library: Library; isAdmin: boolean }) {
  const capped = library.max_total_bytes > 0;
  return (
    <ConnPanel>
      <ConnPanelHead title="Library usage">
        <span
          className="text-[0.78rem] text-[var(--color-text-secondary)] [font-variant-numeric:tabular-nums]"
          data-testid="usage-meter"
        >
          {capped
            ? `${formatBytes(library.total_bytes)} of ${formatBytes(library.max_total_bytes)} used`
            : `${formatBytes(library.total_bytes)} used — no limit`}
        </span>
      </ConnPanelHead>
      {isAdmin ? (
        <ConnPanelSub>
          The cap is the <CodeChip>shared_files_max_total_mb</CodeChip> setting in Settings →
          Admin → Features (0 = unlimited).
        </ConnPanelSub>
      ) : null}
    </ConnPanel>
  );
}

// UploadPanel — the admin multi-file upload. Picked files are screened
// client-side against the remaining library capacity when a cap is known
// (max_total_bytes > 0): a file that can't possibly fit is rejected at pick
// time with a readable message instead of after a full round-trip. The server
// still enforces its own per-file and total caps (413) regardless.
function UploadPanel({
  library,
  busy,
  setBusy,
  onUploaded,
}: {
  library: Library;
  busy: boolean;
  setBusy: (b: boolean) => void;
  onUploaded: () => void;
}) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [picked, setPicked] = useState<File[]>([]);
  const [pickError, setPickError] = useState<string | null>(null);
  const [folder, setFolder] = useState("");
  const [description, setDescription] = useState("");
  const [uploadError, setUploadError] = useState<string | null>(null);

  const onPick = (files: File[]) => {
    setUploadError(null);
    if (library.max_total_bytes > 0) {
      const remaining = Math.max(0, library.max_total_bytes - library.total_bytes);
      const screened = screenFilesForUpload(files, remaining);
      setPicked(screened.accepted);
      setPickError(screened.error);
    } else {
      setPicked(files);
      setPickError(null);
    }
  };

  const upload = async () => {
    setBusy(true);
    setUploadError(null);
    try {
      const form = new FormData();
      for (const f of picked) form.append("files", f);
      const fld = folder.trim();
      if (fld) form.append("folder", fld);
      const desc = description.trim();
      if (desc) form.append("description", desc);
      const res = await fetch("/api/shared-files", { method: "POST", body: form });
      if (res.status === 401) {
        window.location.assign("/login");
        return;
      }
      if (!res.ok) throw new Error(await readError(res, `Upload failed: ${res.status}`));
      setPicked([]);
      setPickError(null);
      setFolder("");
      setDescription("");
      if (inputRef.current) inputRef.current.value = "";
      onUploaded();
    } catch (err) {
      setUploadError(err instanceof Error ? err.message : "Upload failed.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConnPanel>
      <ConnPanelHead title="Upload files" />
      <ConnPanelSub>
        Each file lands in the chosen folder (one name segment, no slashes); the description,
        if given, applies to every file in the batch.
      </ConnPanelSub>
      <ConnForm>
        <ConnField label="Files" grow>
          <input
            ref={inputRef}
            type="file"
            multiple
            disabled={busy}
            // ConnField already wraps this in a <label>, but the linter can't
            // see through the component boundary.
            aria-label="Files to upload"
            data-testid="upload-files-input"
            onChange={(e) => onPick(Array.from(e.target.files ?? []))}
            className="text-[0.82rem] text-[var(--color-text-secondary)] file:mr-3 file:cursor-pointer file:rounded-[var(--radius-md)] file:border file:border-[var(--color-border)] file:bg-[var(--color-overlay-soft)] file:px-[0.7rem] file:py-[0.35rem] file:text-[0.78rem] file:font-medium file:text-[var(--color-text-primary)]"
          />
        </ConnField>
        <ConnField label="Folder (optional)">
          <input
            type="text"
            value={folder}
            disabled={busy}
            placeholder="reference"
            list="shared-file-folders"
            data-testid="upload-folder-input"
            onChange={(e) => setFolder(e.target.value)}
            className={SETTINGS_INPUT}
          />
        </ConnField>
        <ConnField label="Description (optional)" grow>
          <input
            type="text"
            value={description}
            disabled={busy}
            data-testid="upload-description-input"
            onChange={(e) => setDescription(e.target.value)}
            className={SETTINGS_INPUT}
          />
        </ConnField>
        <button
          type="button"
          disabled={busy || picked.length === 0}
          data-testid="upload-submit"
          onClick={() => void upload()}
          className={btnClass({ variant: "primary", sm: true })}
        >
          {busy ? "Uploading…" : picked.length > 1 ? `Upload ${picked.length} files` : "Upload"}
        </button>
      </ConnForm>
      {pickError ? (
        <p className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]" role="alert">
          {pickError}
        </p>
      ) : null}
      {uploadError ? (
        <p className="m-0 text-[0.73rem] text-[var(--color-danger)]" role="alert">
          {uploadError}
        </p>
      ) : null}
    </ConnPanel>
  );
}
