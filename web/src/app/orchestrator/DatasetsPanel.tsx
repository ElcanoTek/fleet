"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  orchestratorApi,
  type Dataset,
  type DatasetColumn,
  type DatasetRow,
} from "@/app/shared/lib/orchestratorApi";
import { useToast } from "@/app/shared/ui/Toast";

// Dataset / table agent (#514): define a typed table + per-row goal, import
// rows (CSV), run the agent over pending rows, review PROPOSED write-backs
// (bulk approve), retry failures, export CSV. Rows only ever land as
// proposals — the approve action here is the human review gate.

const KIND_OPTIONS = ["text", "number", "boolean"] as const;

function emptyColumn(output: boolean): DatasetColumn {
  return { name: "", type: "text", output };
}

export function DatasetsPanel() {
  const { showToast } = useToast();
  const [datasets, setDatasets] = useState<Dataset[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [rows, setRows] = useState<DatasetRow[]>([]);
  const [rowCounts, setRowCounts] = useState<Record<string, number>>({});
  const [statusFilter, setStatusFilter] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);

  const selected = datasets.find((d) => d.id === selectedId) ?? null;

  const reloadList = useCallback(async () => {
    try {
      const res = await orchestratorApi.datasets();
      setDatasets(res.datasets ?? []);
    } catch (err) {
      showToast(`Failed to load datasets: ${(err as Error).message}`, "error");
    }
  }, [showToast]);

  const reloadRows = useCallback(async () => {
    if (!selectedId) return;
    try {
      const qs = statusFilter ? `?status=${encodeURIComponent(statusFilter)}` : "";
      const res = await orchestratorApi.datasetRows(selectedId, qs);
      setRows(res.rows ?? []);
      setRowCounts(res.row_counts ?? {});
    } catch (err) {
      showToast(`Failed to load rows: ${(err as Error).message}`, "error");
    }
  }, [selectedId, statusFilter, showToast]);

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
      showToast(`Imported ${res.imported} row(s)`, "success");
    });
  };

  const exportHref = selectedId
    ? `/api/orchestrator/datasets/${encodeURIComponent(selectedId)}/export`
    : undefined;

  return (
    <div className="section" role="region" aria-label="Datasets">
      <div className="section-header" style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h2>Datasets</h2>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          New dataset
        </button>
      </div>

      {datasets.length === 0 ? (
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
              onClick={() => setSelectedId(d.id)}
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
                showToast(`Approved ${res.approved} row(s)`, "success");
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
            {exportHref ? (
              <a className="btn" href={exportHref} download>
                Export CSV
              </a>
            ) : null}
            <button
              type="button"
              className="btn btn-danger"
              disabled={busy || selected.status === "running"}
              onClick={() => {
                if (!window.confirm(`Delete dataset "${selected.name}" and all its rows?`)) return;
                void act("Delete", async () => {
                  await orchestratorApi.deleteDataset(selected.id);
                  setSelectedId(null);
                });
              }}
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
            {rows.length === 0 ? <p className="empty-state">No rows{statusFilter ? ` with status "${statusFilter}"` : ""} — import a CSV to get started.</p> : null}
          </div>
        </>
      ) : null}

      {createOpen ? (
        <DatasetCreateModal
          onClose={() => setCreateOpen(false)}
          onCreated={(d) => {
            setCreateOpen(false);
            setSelectedId(d.id);
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
  const [columns, setColumns] = useState<DatasetColumn[]>([emptyColumn(false), emptyColumn(true)]);
  const [saving, setSaving] = useState(false);

  const setCol = (i: number, patch: Partial<DatasetColumn>) => {
    setColumns((prev) => prev.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  };

  const submit = async () => {
    if (saving) return;
    setSaving(true);
    try {
      const d = await orchestratorApi.createDataset({
        name: name.trim(),
        goal: goal.trim(),
        model: model.trim(),
        columns: columns.filter((c) => c.name.trim() !== ""),
      });
      onCreated(d);
    } catch (err) {
      showToast(`Create failed: ${(err as Error).message}`, "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="modal-overlay" role="dialog" aria-modal="true" aria-label="New dataset">
      <div className="modal">
        <div className="modal-header">
          <h3>New dataset</h3>
          <button type="button" className="btn" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>
        <div className="modal-body" style={{ display: "grid", gap: "0.75rem" }}>
          <label>
            Name
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="prospect-leads" />
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
              <div key={i} style={{ display: "flex", gap: "0.5rem", marginBottom: "0.4rem", alignItems: "center" }}>
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
                <button type="button" className="btn btn-small" onClick={() => setColumns((prev) => prev.filter((_, j) => j !== i))}>
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
