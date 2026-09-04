"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { validateFile } from "@/app/shared/lib/validation";
import { DEFAULT_UPLOAD_MAX_BYTES } from "@/app/lib/uploadLimits";
import { formatFileSize, truncateFilename } from "@/app/shared/lib/format";

// FileUpload — compact multi-file attach row for the task form. Owns local
// file-entry state; the parent calls uploadAll() (passed an uploader) at submit
// time to get back the server-side filenames. Validation reuses
// shared/lib/validation.
//
// The picker itself is a single "Attach files" row (button + drag hint) that
// grows a file list — the parent dialog is the drag target, forwarding drops
// through handle.addFiles() so files can be dropped anywhere on the modal, not
// just on this row. Limit violations are VISIBLE: an over-the-cap batch adds
// nothing beyond the cap and says so inline (the old dropzone rejected the
// whole batch silently).

const MAX_FILE_SIZE = DEFAULT_UPLOAD_MAX_BYTES;
const MAX_FILES = 10;

export type FileEntry = {
  id: string;
  file: File;
  status: "pending" | "uploading" | "uploaded" | "error" | "invalid";
  progress: number;
  error: string;
  filename?: string;
};

export type FileUploadHandle = {
  hasFiles: () => boolean;
  // addFiles lets the parent forward files from its own drag-and-drop surface
  // (the New Task modal accepts drops anywhere on the dialog).
  addFiles: (files: FileList | File[]) => void;
  uploadAll: (uploader: (file: File) => Promise<{ filename: string }>) => Promise<string[]>;
  reset: () => void;
};

export type FileUploadProps = {
  onEntriesChange?: (entries: FileEntry[]) => void;
  // Imperative handle for the parent (task form) to drive upload at submit.
  registerHandle?: (handle: FileUploadHandle) => void;
};

let idCounter = 0;
function genId(): string {
  return `file-${++idCounter}-${Date.now()}`;
}

export function FileUpload({ onEntriesChange, registerHandle }: FileUploadProps) {
  const [entries, setEntries] = useState<FileEntry[]>([]);
  // A batch-level problem (too many files) — adjacent to the row, not a toast.
  const [limitError, setLimitError] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  // Mirror entries into a ref via effect (not during render) so the imperative
  // handle's closures read the latest list without a stale-closure bug.
  const entriesRef = useRef<FileEntry[]>([]);
  useEffect(() => {
    entriesRef.current = entries;
  }, [entries]);

  const update = useCallback(
    (next: FileEntry[]) => {
      entriesRef.current = next;
      setEntries(next);
      onEntriesChange?.(next);
    },
    [onEntriesChange],
  );

  const addFiles = useCallback(
    (files: FileList | File[]) => {
      const incoming = Array.from(files);
      const current = entriesRef.current;
      let validCount = current.filter((e) => e.status !== "invalid").length;
      let overflow = 0;
      const added: FileEntry[] = [];
      for (const file of incoming) {
        const dup = [...current, ...added].some(
          (e) =>
            e.file.name === file.name &&
            e.file.size === file.size &&
            e.file.lastModified === file.lastModified,
        );
        if (dup) continue;
        const v = validateFile(file, { maxSize: MAX_FILE_SIZE });
        // Duplicates and invalid files must not consume room in the batch:
        // a valid file later in the same drop should still fit.
        if (v.valid && validCount >= MAX_FILES) {
          overflow++;
          continue;
        }
        if (v.valid) validCount++;
        added.push({
          id: genId(),
          file,
          status: v.valid ? "pending" : "invalid",
          progress: 0,
          error: v.valid ? "" : v.message,
        });
      }
      setLimitError(
        overflow > 0
          ? `Up to ${MAX_FILES} files per task — ${overflow} ${overflow === 1 ? "file was" : "files were"} not added.`
          : "",
      );
      if (added.length > 0) update([...current, ...added]);
    },
    [update],
  );

  const removeFile = useCallback(
    (id: string) => {
      setLimitError("");
      update(entriesRef.current.filter((e) => e.id !== id));
    },
    [update],
  );

  // Register the imperative handle once, in an effect (not during render).
  const updateRef = useRef(update);
  useEffect(() => {
    updateRef.current = update;
  }, [update]);
  const addFilesRef = useRef(addFiles);
  useEffect(() => {
    addFilesRef.current = addFiles;
  }, [addFiles]);
  useEffect(() => {
    if (!registerHandle) return;
    registerHandle({
      hasFiles: () =>
        entriesRef.current.some((e) => e.status !== "invalid"),
      addFiles: (files) => addFilesRef.current(files),
      reset: () => {
        setLimitError("");
        updateRef.current([]);
      },
      uploadAll: async (uploader) => {
        const current = entriesRef.current;
        const pending = current.filter((e) => e.status === "pending" || e.status === "error");
        if (pending.length === 0) {
          return current
            .filter((e) => e.status === "uploaded" && e.filename)
            .map((e) => e.filename!);
        }
        for (const entry of pending) {
          entry.status = "uploading";
          entry.error = "";
          updateRef.current([...entriesRef.current]);
          try {
            const result = await uploader(entry.file);
            entry.status = "uploaded";
            entry.progress = 100;
            entry.filename = result.filename;
          } catch (err) {
            entry.status = "error";
            entry.error = (err as Error).message;
            updateRef.current([...entriesRef.current]);
            throw err;
          }
          updateRef.current([...entriesRef.current]);
        }
        return entriesRef.current
          .filter((e) => e.status === "uploaded" && e.filename)
          .map((e) => e.filename!);
      },
    });
  }, [registerHandle]);

  return (
    <div className="file-upload-area" role="region" aria-label="File attachments">
      <div className="file-attach-row">
        <button
          type="button"
          className="file-attach-btn"
          onClick={() => inputRef.current?.click()}
        >
          <svg
            width="12"
            height="12"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            aria-hidden="true"
          >
            <path d="M21.4 11.1l-8.5 8.5a6 6 0 0 1-8.5-8.5l9.2-9.2a4 4 0 1 1 5.7 5.7l-9.2 9.1a2 2 0 1 1-2.8-2.8l8.5-8.5" />
          </svg>
          Attach files
        </button>
        <span className="file-attach-hint">
          or drag anywhere onto this dialog — 1 GB per file, up to 10 files
        </span>
        <input
          ref={inputRef}
          type="file"
          multiple
          className="file-input-hidden"
          aria-hidden="true"
          tabIndex={-1}
          onChange={(e) => {
            if (e.target.files?.length) addFiles(e.target.files);
            e.target.value = "";
          }}
        />
      </div>
      {limitError ? (
        <p className="file-upload-error" role="alert">
          {limitError}
        </p>
      ) : null}
      <div className="file-list" aria-live="polite">
        {entries.map((entry) => (
          <div key={entry.id} className={`file-item ${entry.status}`} data-entry-id={entry.id}>
            <span className="file-item-name" title={entry.file.name}>
              {truncateFilename(entry.file.name)}
            </span>
            <span className="file-item-size">{formatFileSize(entry.file.size)}</span>
            {entry.error ? <span className="file-item-error-text">{entry.error}</span> : null}
            <span className="file-item-spacer" />
            {entry.status === "uploading" ? (
              <span className="file-item-status-icon" aria-label="Uploading">
                …
              </span>
            ) : null}
            {entry.status === "uploaded" ? (
              <span className="file-item-status-icon uploaded-icon" aria-label="Uploaded">
                ✓
              </span>
            ) : null}
            {entry.status !== "uploading" ? (
              <button
                type="button"
                className="file-item-remove"
                aria-label={`Remove ${entry.file.name}`}
                onClick={() => removeFile(entry.id)}
              >
                ×
              </button>
            ) : null}
          </div>
        ))}
      </div>
    </div>
  );
}

export default FileUpload;
