"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { validateFile } from "@/app/shared/lib/validation";
import { formatFileSize, truncateFilename } from "@/app/shared/lib/format";

// FileUpload — drag-and-drop multi-file picker for the task form. React port of
// moc's file-upload.js. Owns local file-entry state; the parent calls
// uploadAll() (passed an uploader) at submit time to get back the server-side
// filenames. Validation reuses shared/lib/validation.

const MAX_FILE_SIZE = 250 * 1024 * 1024;
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
  const [dragOver, setDragOver] = useState(false);
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
      const currentValid = current.filter((e) => e.status !== "invalid").length;
      if (currentValid + incoming.length > MAX_FILES) {
        return;
      }
      const added: FileEntry[] = [];
      for (const file of incoming) {
        const dup = current.some(
          (e) =>
            e.file.name === file.name &&
            e.file.size === file.size &&
            e.file.lastModified === file.lastModified,
        );
        if (dup) continue;
        const v = validateFile(file, { maxSize: MAX_FILE_SIZE });
        added.push({
          id: genId(),
          file,
          status: v.valid ? "pending" : "invalid",
          progress: 0,
          error: v.valid ? "" : v.message,
        });
      }
      update([...current, ...added]);
    },
    [update],
  );

  const removeFile = useCallback(
    (id: string) => {
      update(entriesRef.current.filter((e) => e.id !== id));
    },
    [update],
  );

  // Register the imperative handle once, in an effect (not during render).
  const updateRef = useRef(update);
  useEffect(() => {
    updateRef.current = update;
  }, [update]);
  useEffect(() => {
    if (!registerHandle) return;
    registerHandle({
      hasFiles: () =>
        entriesRef.current.some((e) => e.status === "pending" || e.status === "uploaded"),
      reset: () => updateRef.current([]),
      uploadAll: async (uploader) => {
        const current = entriesRef.current;
        const pending = current.filter((e) => e.status === "pending");
        if (pending.length === 0) {
          return current
            .filter((e) => e.status === "uploaded" && e.filename)
            .map((e) => e.filename!);
        }
        for (const entry of pending) {
          entry.status = "uploading";
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
    <div className="file-upload-area" role="region" aria-label="File upload area">
      <div
        className={`file-upload-dropzone${dragOver ? " drag-over" : ""}`}
        tabIndex={0}
        onDragEnter={(e) => {
          e.preventDefault();
          setDragOver(true);
        }}
        onDragOver={(e) => {
          e.preventDefault();
          setDragOver(true);
        }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragOver(false);
          if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            inputRef.current?.click();
          }
        }}
      >
        <div className="dropzone-content">
          <p className="dropzone-text">Drag &amp; drop files here</p>
          <p className="dropzone-subtext">or</p>
          <button
            type="button"
            className="btn btn-secondary dropzone-browse-btn"
            onClick={() => inputRef.current?.click()}
          >
            Browse Files
          </button>
          <p className="dropzone-limit">Max 250 MB per file · Up to 10 files</p>
        </div>
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
      <div className="file-list" aria-live="polite">
        {entries.map((entry) => (
          <div key={entry.id} className={`file-item ${entry.status}`} data-entry-id={entry.id}>
            <div className="file-item-info">
              <span className="file-item-name" title={entry.file.name}>
                {truncateFilename(entry.file.name)}
              </span>
              <span className="file-item-size">{formatFileSize(entry.file.size)}</span>
              {entry.error ? <span className="file-item-error-text">{entry.error}</span> : null}
            </div>
            <div className="file-item-actions">
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
          </div>
        ))}
      </div>
    </div>
  );
}

export default FileUpload;
