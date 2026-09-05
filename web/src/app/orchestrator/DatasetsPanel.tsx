"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  orchestratorApi,
  type Dataset,
  type DatasetColumn,
  type DatasetRow,
} from "@/app/shared/lib/orchestratorApi";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { ConfirmDialog } from "@/app/shared/ui/ConfirmDialog";
import { useToast } from "@/app/shared/ui/Toast";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { downloadFile } from "./downloadFile";
import { plural } from "./plural";

// Dataset / table agent (#514): define a typed table + per-row goal, import
// rows (CSV), run the agent over pending rows, review PROPOSED write-backs
// (bulk approve), retry failures, export CSV. Rows only ever land as
// proposals — the approve action here is the human review gate.

const KIND_OPTIONS = ["text", "number", "boolean"] as const;

// Rows per page. The server's own default for GET /rows is 200 (max 1000);
// asking for it explicitly, with an offset, is what lets the table page past
// the first 200 rows instead of silently ending there.
export const ROWS_PAGE_SIZE = 200;

// A column draft in the create modal carries a client-side id so a row's key
// survives removing the row above it — an index key re-associates every
// following input with the wrong draft the moment one is deleted.
type ColumnDraft = DatasetColumn & { draftId: number };
let nextDraftId = 1;

function emptyColumn(output: boolean): ColumnDraft {
  return { draftId: nextDraftId++, name: "", type: "text", output };
}

export function DatasetsPanel() {
  const { showToast } = useToast();
  const [datasets, setDatasets] = useState<Dataset[]>([]);
  // null until the first list response: the empty state must not flash "No
  // datasets yet" while the request is still in flight (or on a failed load).
  const [listLoaded, setListLoaded] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [rows, setRows] = useState<DatasetRow[]>([]);
  const [rowsLoading, setRowsLoading] = useState(false);
  const [rowCounts, setRowCounts] = useState<Record<string, number>>({});
  const [statusFilter, setStatusFilterState] = useState("");
  // 1-based page into the selected dataset's (filtered) rows.
  const [rowsPage, setRowsPage] = useState(1);
  // The page count implied by the most recent rows response. A ref, not state:
  // it is read while BUILDING the next request, and deriving it during render
  // instead would need a setState-in-effect to feed it back to the fetch.
  const rowsPagesRef = useRef(1);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);
  // Monotonic id per rows request (the useDashboardData pattern): a slower
  // response for a dataset the user has already clicked away from must not
  // land under the new selection's heading.
  const rowsRunRef = useRef(0);

  const selected = datasets.find((d) => d.id === selectedId) ?? null;

  const reloadList = useCallback(async () => {
    try {
      const res = await orchestratorApi.datasets();
      setDatasets(res.datasets ?? []);
    } catch (err) {
      showToast(`Failed to load datasets: ${(err as Error).message}`, "error");
    } finally {
      setListLoaded(true);
    }
  }, [showToast]);

  const reloadRows = useCallback(async () => {
    if (!selectedId) return;
    const runId = ++rowsRunRef.current;
    setRowsLoading(true);
    try {
      const params = new URLSearchParams();
      params.set("limit", String(ROWS_PAGE_SIZE));
      // Clamp against the page count the LAST response implied. Approving or
      // rerunning the final rows of a page drops the total below this page's
      // offset while the dataset and filter stay put, so neither reset path
      // fires: without this the request asks for an offset past the end, the
      // table renders empty, and the pager hides itself (it needs more than
      // one page), leaving no control to get back.
      const page = Math.min(rowsPage, rowsPagesRef.current);
      params.set("offset", String((page - 1) * ROWS_PAGE_SIZE));
      if (statusFilter) params.set("status", statusFilter);
      const res = await orchestratorApi.datasetRows(selectedId, `?${params.toString()}`);
      if (runId !== rowsRunRef.current) return; // superseded by a newer selection
      setRows(res.rows ?? []);
      const counts = res.row_counts ?? {};
      setRowCounts(counts);
      const total = statusFilter
        ? (counts[statusFilter] ?? 0)
        : Object.values(counts).reduce((sum, n) => sum + n, 0);
      rowsPagesRef.current = Math.max(1, Math.ceil(total / ROWS_PAGE_SIZE));
      // Settle the control state on what was actually fetched, so Prev/Next
      // and the "Page N of M" label cannot disagree with the rows on screen.
      if (page > rowsPagesRef.current) setRowsPage(rowsPagesRef.current);
    } catch (err) {
      if (runId !== rowsRunRef.current) return;
      showToast(`Failed to load rows: ${(err as Error).message}`, "error");
    } finally {
      if (runId === rowsRunRef.current) setRowsLoading(false);
    }
  }, [selectedId, statusFilter, rowsPage, showToast]);

  // selectDataset swaps the selection and drops the OLD dataset's rows at once,
  // so they are never shown under the new heading while its fetch runs.
  const selectDataset = (id: string | null) => {
    rowsRunRef.current++; // retire any in-flight request for the old selection
    setSelectedId(id);
    setRows([]);
    setRowCounts({});
    setRowsPage(1);
    setRowsLoading(id !== null);
  };

  // A filter change re-buckets the rows, so the page number under the old
  // filter is meaningless (the useDashboardData rule).
  const setStatusFilter = (status: string) => {
    setStatusFilterState(status);
    setRowsPage(1);
  };

  // Deferred kick-offs (the useDashboardData pattern): queueMicrotask keeps
  // the state writes out of the synchronous effect body.
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void reloadList();
    });
    return () => {
      cancelled = true;
    };
  }, [reloadList]);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void reloadRows();
    });
    return () => {
      cancelled = true;
    };
  }, [reloadRows]);

  // Poll while the selected dataset is running so statuses tick over.
  useEffect(() => {
    if (selected?.status !== "running") return;
    const id = setInterval(() => {
      void reloadList();
      void reloadRows();
    }, 5000);
    return () => clearInterval(id);
  }, [selected?.status, reloadList, reloadRows]);

  const act = async (label: string, fn: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    try {
      await fn();
      await reloadList();
      await reloadRows();
    } catch (err) {
      showToast(`${label} failed: ${(err as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const uploadCSV = async (file: File) => {
    if (!selectedId) return;
    const text = await file.text();
    await act("Import", async () => {
      const res = await orchestratorApi.importDatasetRowsCSV(selectedId, text);
      showToast(`Imported ${plural(res.imported, "row")}`, "success");
    });
  };

  // Fetch-then-save (downloadFile) instead of an <a download> navigation, like
  // the Usage/Adoption CSV buttons: a 401/403/500 on a plain link replaces
  // the dashboard with the server's error body.
  const exportCSV = () => {
    if (!selected) return;
    void downloadFile(
      `/api/orchestrator/datasets/${encodeURIComponent(selected.id)}/export`,
      `${selected.name || "dataset"}.csv`,
    ).catch((err: unknown) =>
      showToast(`Export failed: ${err instanceof Error ? err.message : "download failed"}`, "error"),
    );
  };

  // How many rows the current filter covers, from the server's per-status
  // counts — the rows endpoint returns no total of its own.
  const rowsTotal = statusFilter
    ? (rowCounts[statusFilter] ?? 0)
    : Object.values(rowCounts).reduce((sum, n) => sum + n, 0);
  const rowsPages = Math.max(1, Math.ceil(rowsTotal / ROWS_PAGE_SIZE));
  const rowsStart = rowsTotal > 0 ? Math.min((rowsPage - 1) * ROWS_PAGE_SIZE + 1, rowsTotal) : 0;
  const rowsEnd = Math.min((rowsPage - 1) * ROWS_PAGE_SIZE + rows.length, rowsTotal);

  return (
    <div className="section" role="region" aria-label="Datasets">
      <div className="section-header" style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h2>Datasets</h2>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          New dataset
        </button>
      </div>

      {!listLoaded ? (
        <p className="refresh-note" data-testid="datasets-loading">
          Loading datasets…
        </p>
      ) : datasets.length === 0 ? (
        <p className="empty-state">
          No datasets yet. A dataset is a typed table the agent works row by row toward a goal —
          results come back as proposals you review before they land.
        </p>
      ) : (
        <div className="tasks-filter-bar" role="tablist" aria-label="Datasets">
          {datasets.map((d) => (
            <button
              key={d.id}
              type="button"
              role="tab"
              aria-selected={d.id === selectedId}
              className={`tab-btn${d.id === selectedId ? " tab-btn-active" : ""}`}
              onClick={() => selectDataset(d.id)}
            >
              {d.name}
              <span className={`status-badge status-${d.status}`} style={{ marginLeft: 6 }}>
                {d.status}
              </span>
            </button>
          ))}
        </div>
      )}

      {selected ? (
        <>
          <p style={{ margin: "0.5rem 0", color: "var(--color-text-secondary)" }}>{selected.goal}</p>
          <div className="tasks-filter-bar" role="toolbar" aria-label="Dataset actions">
            {selected.status === "running" ? (
              <button type="button" className="btn" disabled={busy} onClick={() => void act("Pause", () => orchestratorApi.pauseDataset(selected.id))}>
                Pause
              </button>
            ) : (
              <button
                type="button"
                className="btn btn-primary"
                disabled={busy || (rowCounts["pending"] ?? 0) === 0}
                onClick={() => void act("Run", () => orchestratorApi.runDataset(selected.id))}
              >
                Run {rowCounts["pending"] ?? 0} pending
              </button>
            )}
            <button
              type="button"
              className="btn"
              disabled={busy || (rowCounts["proposed"] ?? 0) === 0}
              onClick={() => void act("Approve", async () => {
                const res = await orchestratorApi.approveDatasetRows(selected.id);
                showToast(`Approved ${plural(res.approved, "row")}`, "success");
              })}
            >
              Approve all proposed ({rowCounts["proposed"] ?? 0})
            </button>
            <button
              type="button"
              className="btn"
              disabled={busy || (rowCounts["failed"] ?? 0) === 0}
              onClick={() => void act("Retry", () => orchestratorApi.rerunDatasetRows(selected.id))}
            >
              Retry failed ({rowCounts["failed"] ?? 0})
            </button>
            <button type="button" className="btn" disabled={busy} onClick={() => fileRef.current?.click()}>
              Import CSV
            </button>
            <input
              ref={fileRef}
              type="file"
              accept=".csv,text/csv"
              style={{ display: "none" }}
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) void uploadCSV(f);
                e.target.value = "";
              }}
            />
            <button type="button" className="btn" data-testid="dataset-export-csv" onClick={exportCSV}>
              Export CSV
            </button>
            <button
              type="button"
              className="btn btn-danger"
              disabled={busy || selected.status === "running"}
              onClick={() => setDeleteOpen(true)}
            >
              Delete
            </button>
            <select
              aria-label="Filter rows by status"
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
            >
              <option value="">All rows</option>
              <option value="pending">Pending</option>
              <option value="running">Running</option>
              <option value="proposed">Proposed</option>
              <option value="approved">Approved</option>
              <option value="failed">Failed</option>
            </select>
          </div>

          <div className="table-wrapper">
            <table>
              <thead>
                <tr>
                  <th>#</th>
                  {selected.columns.map((c) => (
                    <th key={c.name}>
                      {c.name}
                      {c.output ? " ✎" : ""}
                    </th>
                  ))}
                  <th>Status</th>
                  <th>Note / error</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr key={row.id}>
                    <td>{row.row_index + 1}</td>
                    {selected.columns.map((c) => {
                      const current = row.cells?.[c.name];
                      const proposed = row.proposed?.[c.name];
                      return (
                        <td key={c.name}>
                          {proposed !== undefined ? (
                            <span title="Proposed — approve to apply" style={{ color: "var(--color-accent)" }}>
                              {String(proposed)}
                            </span>
                          ) : current !== undefined && current !== null ? (
                            String(current)
                          ) : (
                            ""
                          )}
                        </td>
                      );
                    })}
                    <td>
                      <span className={`status-badge status-${row.status}`}>{row.status}</span>
                    </td>
                    <td style={{ maxWidth: "18rem", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={row.error || row.result_note}>
                      {row.error || row.result_note || ""}
                      {row.status === "proposed" || row.status === "failed" ? (
                        <button
                          type="button"
                          className="btn btn-small"
                          style={{ marginLeft: 6 }}
                          disabled={busy}
                          onClick={() =>
                            void act(
                              row.status === "proposed" ? "Approve" : "Retry",
                              () =>
                                row.status === "proposed"
                                  ? orchestratorApi.approveDatasetRows(selected.id, [row.id])
                                  : orchestratorApi.rerunDatasetRows(selected.id, [row.id]),
                            )
                          }
                        >
                          {row.status === "proposed" ? "Approve" : "Retry"}
                        </button>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {rowsLoading ? (
              <p className="refresh-note" data-testid="dataset-rows-loading">
                Loading rows…
              </p>
            ) : rows.length === 0 ? (
              <p className="empty-state">No rows{statusFilter ? ` with status "${statusFilter}"` : ""} — import a CSV to get started.</p>
            ) : null}
          </div>
          {rowsTotal > 0 ? (
            <div className="tasks-pagination" role="navigation" aria-label="Dataset rows pagination">
              <div className="pagination-info">
                <span data-testid="dataset-rows-showing">
                  {rowsLoading && rows.length === 0
                    ? `${rowsTotal} ${statusFilter ? `${statusFilter} ` : ""}${rowsTotal === 1 ? "row" : "rows"}`
                    : `Showing ${rowsStart}-${rowsEnd} of ${rowsTotal} ${statusFilter ? `${statusFilter} ` : ""}${rowsTotal === 1 ? "row" : "rows"}`}
                </span>
              </div>
              {rowsPages > 1 ? (
                <div className="pagination-controls">
                  <button
                    type="button"
                    className="btn btn-secondary"
                    aria-label="Previous rows page"
                    disabled={rowsPage <= 1 || rowsLoading}
                    onClick={() => setRowsPage((p) => Math.max(1, p - 1))}
                  >
                    Prev
                  </button>
                  <span className="page-info">
                    Page {rowsPage} of {rowsPages}
                  </span>
                  <button
                    type="button"
                    className="btn btn-secondary"
                    aria-label="Next rows page"
                    disabled={rowsPage >= rowsPages || rowsLoading}
                    onClick={() => setRowsPage((p) => p + 1)}
                  >
                    Next
                  </button>
                </div>
              ) : null}
            </div>
          ) : null}
          {/* The app's confirm dialog, not window.confirm: same keyboard
              contract and styling as every other destructive action here. */}
          <ConfirmDialog
            open={deleteOpen}
            title="Delete dataset"
            message={`Delete dataset "${selected.name}" and all its rows? This cannot be undone.`}
            confirmLabel={busy ? "Deleting…" : "Delete"}
            busy={busy}
            onConfirm={() => {
              void act("Delete", async () => {
                await orchestratorApi.deleteDataset(selected.id);
                selectDataset(null);
              }).then(() => setDeleteOpen(false));
            }}
            onCancel={() => setDeleteOpen(false)}
          />
        </>
      ) : null}

      {createOpen ? (
        <DatasetCreateModal
          onClose={() => setCreateOpen(false)}
          onCreated={(d) => {
            setCreateOpen(false);
            selectDataset(d.id);
            void reloadList();
          }}
        />
      ) : null}
    </div>
  );
}

function DatasetCreateModal({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (d: Dataset) => void;
}) {
  const { showToast } = useToast();
  const [name, setName] = useState("");
  const [goal, setGoal] = useState("");
  const [model, setModel] = useState("");
  const [columns, setColumns] = useState<ColumnDraft[]>([emptyColumn(false), emptyColumn(true)]);
  const [saving, setSaving] = useState(false);
  // Keyboard contract (Escape closes, Tab is trapped, focus starts in Name and
  // returns to the opener): the overlay was aria-modal in name only before.
  const overlayRef = useRef<HTMLDivElement | null>(null);
  const nameRef = useRef<HTMLInputElement | null>(null);
  useDialogA11y(true, overlayRef, onClose, { initialFocusRef: nameRef });

  const setCol = (i: number, patch: Partial<DatasetColumn>) => {
    setColumns((prev) => prev.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  };
  // The draft ids are client-side bookkeeping — the API gets plain columns.
  const columnsForApi = (): DatasetColumn[] =>
    columns
      .filter((c) => c.name.trim() !== "")
      .map(({ draftId: _draftId, ...c }) => c);

  const submit = async () => {
    if (saving) return;
    setSaving(true);
    try {
      const d = await orchestratorApi.createDataset({
        name: name.trim(),
        goal: goal.trim(),
        model: model.trim(),
        columns: columnsForApi(),
      });
      onCreated(d);
    } catch (err) {
      showToast(`Create failed: ${(err as Error).message}`, "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div ref={overlayRef} className="modal-overlay is-open" role="dialog" aria-modal="true" aria-label="New dataset">
      <div className="modal dataset-modal">
        <div className="modal-header">
          <h3>New dataset</h3>
          <CloseButton label="Close" onClick={onClose} />
        </div>
        <div className="modal-body" style={{ display: "grid", gap: "0.75rem" }}>
          <label>
            Name
            <input ref={nameRef} value={name} onChange={(e) => setName(e.target.value)} placeholder="prospect-leads" />
          </label>
          <label>
            Goal (what the agent does for EACH row)
            <textarea
              value={goal}
              onChange={(e) => setGoal(e.target.value)}
              rows={3}
              placeholder="Research the company and produce a one-line summary plus employee estimate."
            />
          </label>
          <label>
            Model (each row runs at this pinned model)
            <input value={model} onChange={(e) => setModel(e.target.value)} placeholder="anthropic/claude-sonnet-4-6" />
          </label>
          <fieldset>
            <legend>Columns — input columns carry your data; output columns (✎) are what the agent fills</legend>
            {columns.map((c, i) => (
              <div key={c.draftId} className="dataset-column-row">
                <input
                  aria-label={`Column ${i + 1} name`}
                  value={c.name}
                  onChange={(e) => setCol(i, { name: e.target.value })}
                  placeholder="column name"
                />
                <select
                  aria-label={`Column ${i + 1} type`}
                  value={c.type}
                  onChange={(e) => setCol(i, { type: e.target.value as DatasetColumn["type"] })}
                >
                  {KIND_OPTIONS.map((k) => (
                    <option key={k} value={k}>
                      {k}
                    </option>
                  ))}
                </select>
                <label style={{ display: "flex", gap: "0.25rem", alignItems: "center" }}>
                  <input
                    type="checkbox"
                    checked={!!c.output}
                    onChange={(e) => setCol(i, { output: e.target.checked })}
                  />
                  output
                </label>
                <input
                  aria-label={`Column ${i + 1} description`}
                  value={c.description ?? ""}
                  onChange={(e) => setCol(i, { description: e.target.value })}
                  placeholder="description (guides the agent)"
                />
                <button type="button" className="btn btn-small btn-danger-hover" aria-label="Remove column" data-tip-top="Remove column" onClick={() => setColumns((prev) => prev.filter((_, j) => j !== i))}>
                  ✕
                </button>
              </div>
            ))}
            <button type="button" className="btn btn-small" onClick={() => setColumns((prev) => [...prev, emptyColumn(false)])}>
              + column
            </button>
          </fieldset>
        </div>
        <div className="modal-footer" style={{ display: "flex", gap: "0.5rem", justifyContent: "flex-end" }}>
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={saving || !name.trim() || !goal.trim() || !model.trim()}
            onClick={() => void submit()}
          >
            {saving ? "Creating…" : "Create dataset"}
          </button>
        </div>
      </div>
    </div>
  );
}
