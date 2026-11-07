package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EvalRun is one eval & regression harness invocation (#502): the set-level
// aggregate persisted to eval_runs after `fleet eval run <set>` replays a
// set's goldens through the governed loop. Results holds the marshaled
// per-case detail ([]evals.CaseResult — scorer verdicts, judge reasoning,
// cost/duration per case); the typed columns are what history listings and
// regression comparisons query.
type EvalRun struct {
	ID          uuid.UUID `json:"id"`
	EvalSet     string    `json:"eval_set"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	// BundleSHA fingerprints the replayed bundle content (evals.BundleFingerprint):
	// runs are model-comparable only at an equal sha.
	BundleSHA string  `json:"bundle_sha,omitempty"`
	Total     int     `json:"total"`
	Passed    int     `json:"passed"`
	MeanScore float64 `json:"mean_score"`
	// Threshold is the set's pass-fraction gate at run time; Pass records
	// whether passed/total met it.
	Threshold float64         `json:"threshold"`
	Pass      bool            `json:"pass"`
	CostUSD   float64         `json:"cost_usd"`
	Results   json.RawMessage `json:"results,omitempty"`
}

// Dataset / table agent (#514): a typed table whose rows an agent processes in
// the background toward Goal, proposing structured cell write-backs a human
// reviews. Rows run through the one governed loop; the write-back must conform
// to the schema derived from the output columns (free-form answers become a
// row note, never a cell).

// DatasetColumnType is the closed set of dataset cell types.
const (
	DatasetColumnText    = "text"
	DatasetColumnNumber  = "number"
	DatasetColumnBoolean = "boolean"
)

// DatasetColumn defines one column. Output=true columns are what the agent
// fills (they define the structured-output schema); input columns carry the
// row's UNTRUSTED source data.
type DatasetColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // text | number | boolean
	Output      bool   `json:"output"`
	Description string `json:"description,omitempty"`
}

// Dataset statuses. Running is an in-process claim by the server's dataset
// runner; a stale 'running' (crash) is reset to 'paused' at boot.
const (
	DatasetStatusIdle    = "idle"
	DatasetStatusRunning = "running"
	DatasetStatusPaused  = "paused"
)

// Dataset is the table-level definition + run state.
type Dataset struct {
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name"`
	Goal        string          `json:"goal"`
	Columns     []DatasetColumn `json:"columns"`
	Model       string          `json:"model,omitempty"`
	Persona     string          `json:"persona,omitempty"`
	Status      string          `json:"status"`
	Concurrency int             `json:"concurrency"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// RowCounts is populated on reads that aggregate row status (list/get).
	RowCounts map[string]int `json:"row_counts,omitempty"`
}

// Dataset row statuses.
const (
	DatasetRowPending  = "pending"
	DatasetRowRunning  = "running"
	DatasetRowProposed = "proposed"
	DatasetRowApproved = "approved"
	DatasetRowFailed   = "failed"
)

// DatasetRow is one work item. Cells holds input values (and, after approval,
// the merged output values); Proposed holds the validated agent write-back
// awaiting review.
type DatasetRow struct {
	ID         uuid.UUID       `json:"id"`
	DatasetID  uuid.UUID       `json:"dataset_id"`
	RowIndex   int             `json:"row_index"`
	Cells      json.RawMessage `json:"cells"`
	Status     string          `json:"status"`
	Proposed   json.RawMessage `json:"proposed,omitempty"`
	ResultNote string          `json:"result_note,omitempty"`
	Error      string          `json:"error,omitempty"`
	Attempts   int             `json:"attempts"`
	CostUSD    float64         `json:"cost_usd"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Self-improving memory (#516): feedback signals distilled into versioned,
// revertible per-task learned instructions injected at run time. Layered on the
// #285 Captain's Log primitive; does NOT write the client bundle or skills.

// Task-feedback ratings.
const (
	FeedbackUp   = "up"
	FeedbackDown = "down"
)

// TaskFeedback is one raw signal on a task's output.
type TaskFeedback struct {
	ID        uuid.UUID `json:"id"`
	TaskID    uuid.UUID `json:"task_id"`
	Rating    string    `json:"rating"`
	Critique  string    `json:"critique,omitempty"`
	Consumed  bool      `json:"consumed"`
	CreatedAt int64     `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Learned-instruction statuses.
const (
	LearnedProposed = "proposed"
	LearnedActive   = "active"
	LearnedArchived = "archived"
)

// TaskLearnedInstruction is a distilled, versioned instruction for a task. At
// most one is 'active' per task (the run-time injection target).
type TaskLearnedInstruction struct {
	ID          uuid.UUID `json:"id"`
	TaskID      uuid.UUID `json:"task_id"`
	Version     int       `json:"version"`
	Content     string    `json:"content"`
	Status      string    `json:"status"`
	SignalCount int       `json:"signal_count"`
	CreatedAt   int64     `json:"created_at"`
	ActivatedAt *int64    `json:"activated_at,omitempty"`
	ActivatedBy string    `json:"activated_by,omitempty"`
}
