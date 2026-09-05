package handlers

// Dataset / table agent HTTP surface (#514): CRUD + CSV/JSON row import, the
// run/pause controls, the review queue actions (approve / re-run), and a CSV
// export. Row execution itself lives in internal/datasets (injected via
// SetDatasetRunner — handlers stay decoupled from the agent graph, the same
// seam discipline as WithTaskScheduler / SetTaskStreamProvider).

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/datasets"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// DatasetRunController is the slice of the dataset runner handlers need.
type DatasetRunController interface {
	Start(ctx context.Context, id uuid.UUID) error
	Pause(id uuid.UUID) bool
}

// SetDatasetRunner injects the run controller (nil = run/pause return 503,
// e.g. a CLI-embedded handler set with no agent engine).
func (h *Handlers) SetDatasetRunner(rc DatasetRunController) { h.datasetRunner = rc }

const (
	maxDatasetNameLen = 128
	maxDatasetGoalLen = 8000
	maxImportRows     = 5000
	maxImportBytes    = 16 << 20 // 16 MiB body cap on imports
)

type datasetCreateRequest struct {
	Name        string                 `json:"name"`
	Goal        string                 `json:"goal"`
	Columns     []models.DatasetColumn `json:"columns"`
	Model       string                 `json:"model"`
	Persona     string                 `json:"persona,omitempty"`
	Concurrency int                    `json:"concurrency,omitempty"`
}

// requireDatasetPermission is the permission gate every /datasets handler
// opens with. The routes sit in the AdminOrUserAuthMiddleware group, which
// only authenticates — it does no role check — so before this existed a
// readonly user or key could create, delete, import into and RUN datasets
// (each row runs the agent at the dataset's pinned model, i.e. unmetered
// spend). Mirrors the task surface: reads need view_tasks, mutations need
// create_task. Returns false after writing the 403.
func (h *Handlers) requireDatasetPermission(w http.ResponseWriter, r *http.Request, perm models.Permission) bool {
	if !h.principalFromRequest(r).hasPermission(perm) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return false
	}
	return true
}

// CreateDataset handles POST /datasets.
func (h *Handlers) CreateDataset(w http.ResponseWriter, r *http.Request) {
	if !h.requireDatasetPermission(w, r, models.PermissionCreateTask) {
		return
	}
	var req datasetCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Goal = strings.TrimSpace(req.Goal)
	switch {
	case req.Name == "" || len(req.Name) > maxDatasetNameLen:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name is required (≤%d chars)", maxDatasetNameLen))
		return
	case req.Goal == "" || len(req.Goal) > maxDatasetGoalLen:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("goal is required (≤%d chars)", maxDatasetGoalLen))
		return
	case strings.TrimSpace(req.Model) == "":
		writeError(w, http.StatusBadRequest, "model is required (each row runs at this pinned model)")
		return
	}
	if err := datasets.ValidateColumns(req.Columns); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Concurrency < 0 || req.Concurrency > 8 {
		writeError(w, http.StatusBadRequest, "concurrency must be 0..8 (0 = default 2)")
		return
	}
	if req.Concurrency == 0 {
		req.Concurrency = 2
	}

	d := &models.Dataset{
		ID:          uuid.New(),
		Name:        req.Name,
		Goal:        req.Goal,
		Columns:     req.Columns,
		Model:       strings.TrimSpace(req.Model),
		Persona:     strings.TrimSpace(req.Persona),
		Status:      models.DatasetStatusIdle,
		Concurrency: req.Concurrency,
	}
	if err := h.storage.CreateDataset(r.Context(), d); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create dataset")
		return
	}
	d.RowCounts = map[string]int{}
	writeJSON(w, http.StatusOK, d)
}

// ListDatasets handles GET /datasets.
func (h *Handlers) ListDatasets(w http.ResponseWriter, r *http.Request) {
	if !h.requireDatasetPermission(w, r, models.PermissionViewTasks) {
		return
	}
	out, err := h.storage.ListDatasets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list datasets")
		return
	}
	if out == nil {
		out = []*models.Dataset{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": out})
}

// datasetFromPath loads the {datasetID} path dataset after checking perm,
// writing the 403/400/404/500 itself. Only a missing row is a 404: the DB
// layer returns sql.ErrNoRows unwrapped for that case, and anything else (a
// pool exhausted, a column decode failure) is a 500, so an outage does not
// masquerade as "the dataset is gone" to a client that would then recreate it.
func (h *Handlers) datasetFromPath(w http.ResponseWriter, r *http.Request, perm models.Permission) (*models.Dataset, bool) {
	if !h.requireDatasetPermission(w, r, perm) {
		return nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "datasetID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid dataset id")
		return nil, false
	}
	d, err := h.storage.GetDataset(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Dataset not found")
			return nil, false
		}
		//nolint:gosec // G706 false positive: id is a uuid.UUID already parsed
		// from the path (its String() is hex + dashes), and it is passed
		// through logSafe as well — it cannot carry CR/LF to forge a log line.
		log.Printf("dataset %s: load: %v", logSafe(id.String()), err)
		writeError(w, http.StatusInternalServerError, "Failed to load dataset")
		return nil, false
	}
	return d, true
}

// GetDataset handles GET /datasets/{datasetID}.
func (h *Handlers) GetDataset(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionViewTasks)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// DeleteDataset handles DELETE /datasets/{datasetID}.
func (h *Handlers) DeleteDataset(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	if d.Status == models.DatasetStatusRunning {
		writeError(w, http.StatusConflict, "Pause the dataset before deleting it")
		return
	}
	if err := h.storage.DeleteDataset(r.Context(), d.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete dataset")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListDatasetRows handles GET /datasets/{datasetID}/rows?status=&limit=&offset=.
func (h *Handlers) ListDatasetRows(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionViewTasks)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	limit, ok := parseLimit(w, r, datasetRowsDefaultLimit, datasetRowsLimitMax)
	if !ok {
		return
	}
	offset, ok := parseOffset(w, r)
	if !ok {
		return
	}
	rows, err := h.storage.ListDatasetRows(r.Context(), d.ID, status, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list rows")
		return
	}
	if rows == nil {
		rows = []*models.DatasetRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "row_counts": d.RowCounts})
}

// ImportDatasetRows handles POST /datasets/{datasetID}/rows — JSON
// ({"rows":[{col:val}]}) or CSV (Content-Type text/csv, header row = input
// column names). Values land in INPUT columns only; unknown names are
// rejected loudly rather than silently dropped.
func (h *Handlers) ImportDatasetRows(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	// This is the operative body cap: BodySizeLimitMiddleware exempts the
	// dataset import path (like /upload) from its 1 MiB JSON limit precisely so
	// this 16 MiB CSV/JSON cap governs — otherwise a 2 MiB CSV failed under the
	// global limit with a misleading error and this line was dead code.
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)

	inputCols := map[string]models.DatasetColumn{}
	for _, c := range d.Columns {
		if !c.Output {
			inputCols[c.Name] = c
		}
	}

	var rows []map[string]any
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") {
		rows, err = parseCSVRows(r.Body, inputCols)
	} else {
		var req struct {
			Rows []map[string]any `json:"rows"`
		}
		if derr := readJSON(r, &req); derr != nil {
			writeError(w, http.StatusBadRequest, "Invalid JSON: "+derr.Error())
			return
		}
		rows = req.Rows
		err = validateImportRows(rows, inputCols)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusBadRequest, "no rows to import")
		return
	}
	if len(rows) > maxImportRows {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("import capped at %d rows per request", maxImportRows))
		return
	}

	cells := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		raw, merr := json.Marshal(row)
		if merr != nil {
			writeError(w, http.StatusBadRequest, "row encode: "+merr.Error())
			return
		}
		cells = append(cells, raw)
	}
	n, err := h.storage.AddDatasetRows(r.Context(), d.ID, cells)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to import rows")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": n})
}

// parseCSVRows maps a CSV (header = input column names) onto typed cells.
func parseCSVRows(body io.Reader, inputCols map[string]models.DatasetColumn) ([]map[string]any, error) {
	reader := csv.NewReader(body)
	reader.TrimLeadingSpace = true
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("csv: read header: %w", err)
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
		if _, ok := inputCols[header[i]]; !ok {
			return nil, fmt.Errorf("csv: header %q is not an input column of this dataset", header[i])
		}
	}
	var rows []map[string]any
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: row %d: %w", len(rows)+2, err)
		}
		row := map[string]any{}
		for i, v := range record {
			if i >= len(header) {
				break
			}
			col := inputCols[header[i]]
			row[header[i]] = coerceCell(v, col.Type)
		}
		rows = append(rows, row)
		if len(rows) > maxImportRows {
			return nil, fmt.Errorf("import capped at %d rows per request", maxImportRows)
		}
	}
	return rows, nil
}

// coerceCell best-effort types a CSV string by the column type; a value that
// doesn't parse stays a string (visible in review rather than dropped).
func coerceCell(v, colType string) any {
	v = strings.TrimSpace(v)
	switch colType {
	case models.DatasetColumnNumber:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	case models.DatasetColumnBoolean:
		if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
			return b
		}
	}
	return v
}

// validateImportRows rejects JSON-imported rows naming non-input columns.
func validateImportRows(rows []map[string]any, inputCols map[string]models.DatasetColumn) error {
	for i, row := range rows {
		for k := range row {
			if _, ok := inputCols[k]; !ok {
				return fmt.Errorf("row %d: %q is not an input column of this dataset", i+1, k)
			}
		}
	}
	return nil
}

// RunDataset handles POST /datasets/{datasetID}/run.
func (h *Handlers) RunDataset(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	if h.datasetRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "dataset runner not available on this server")
		return
	}
	if err := h.datasetRunner.Start(r.Context(), d.ID); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": models.DatasetStatusRunning})
}

// PauseDataset handles POST /datasets/{datasetID}/pause.
func (h *Handlers) PauseDataset(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	if h.datasetRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "dataset runner not available on this server")
		return
	}
	if !h.datasetRunner.Pause(d.ID) {
		writeError(w, http.StatusConflict, "dataset is not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": models.DatasetStatusPaused})
}

type datasetRowActionRequest struct {
	RowIDs []uuid.UUID `json:"row_ids,omitempty"`
}

// ApproveDatasetRows handles POST /datasets/{datasetID}/approve — the review
// queue's bulk approve: merge proposed values into cells for the given rows
// (all proposed rows when row_ids is empty).
func (h *Handlers) ApproveDatasetRows(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	var req datasetRowActionRequest
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	n, err := h.storage.ApproveDatasetRows(r.Context(), d.ID, req.RowIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to approve rows")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": n})
}

// RerunDatasetRows handles POST /datasets/{datasetID}/rerun — reset the given
// rows (or every failed row when row_ids is empty) to pending.
func (h *Handlers) RerunDatasetRows(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionCreateTask)
	if !ok {
		return
	}
	var req datasetRowActionRequest
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	n, err := h.storage.ResetDatasetRows(r.Context(), d.ID, req.RowIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset rows")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": n})
}

// ExportDataset handles GET /datasets/{datasetID}/export — a CSV of every
// row: all columns (approved values merged into cells) + status/note/error.
func (h *Handlers) ExportDataset(w http.ResponseWriter, r *http.Request) {
	d, ok := h.datasetFromPath(w, r, models.PermissionViewTasks)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeDatasetFilename(d.Name)+".csv"))
	writer := csv.NewWriter(w)

	header := make([]string, 0, len(d.Columns)+3)
	for _, c := range d.Columns {
		header = append(header, c.Name)
	}
	header = append(header, "_status", "_note", "_error")
	_ = writer.Write(header)

	// Page through every row (export is unbounded by the list default). The 200
	// and the header are already on the wire by the first page, so a mid-stream
	// failure (a paging query erroring, the client going away) cannot be turned
	// into an error status: abort the connection instead, so the client sees a
	// truncated transfer rather than a clean 200 over a silently short file.
	const page = 500
	for offset := 0; ; offset += page {
		rows, err := h.storage.ListDatasetRows(r.Context(), d.ID, "", page, offset)
		if err != nil {
			log.Printf("dataset export %s: %v", logSafe(d.ID.String()), err)
			panic(http.ErrAbortHandler)
		}
		for _, row := range rows {
			var cells map[string]any
			_ = json.Unmarshal(row.Cells, &cells)
			record := make([]string, 0, len(header))
			for _, c := range d.Columns {
				record = append(record, cellString(cells[c.Name]))
			}
			record = append(record, row.Status, row.ResultNote, row.Error)
			_ = writer.Write(record)
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			log.Printf("dataset export %s: write: %v", logSafe(d.ID.String()), err)
			panic(http.ErrAbortHandler)
		}
		if len(rows) < page {
			break
		}
	}
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		raw, _ := json.Marshal(t)
		return string(raw)
	}
}

// sanitizeDatasetFilename keeps the export filename header-safe.
func sanitizeDatasetFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "dataset"
	}
	return b.String()
}
