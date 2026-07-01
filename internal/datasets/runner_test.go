package datasets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeStore is an in-memory datasets.Store double so the runner's control
// flow (claiming, outcomes, drain/pause transitions, concurrency) is testable
// without Postgres. The DB accessors have their own db-backed tests.
type fakeStore struct {
	mu      sync.Mutex
	dataset *models.Dataset
	rows    []*models.DatasetRow
	status  string
}

func (f *fakeStore) GetDataset(context.Context, uuid.UUID) (*models.Dataset, error) {
	return f.dataset, nil
}

func (f *fakeStore) ClaimNextDatasetRow(_ context.Context, _ uuid.UUID) (*models.DatasetRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Status == models.DatasetRowPending {
			r.Status = models.DatasetRowRunning
			r.Attempts++
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) FinishDatasetRow(_ context.Context, rowID uuid.UUID, proposed json.RawMessage, note, errMsg string, cost float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == rowID {
			if r.Status != models.DatasetRowRunning {
				return errors.New("not running")
			}
			if len(proposed) > 0 {
				r.Status = models.DatasetRowProposed
				r.Proposed = proposed
			} else {
				r.Status = models.DatasetRowFailed
			}
			r.ResultNote = note
			r.Error = errMsg
			r.CostUSD += cost
			return nil
		}
	}
	return errors.New("row not found")
}

func (f *fakeStore) UpdateDatasetStatus(_ context.Context, _ uuid.UUID, from []string, to string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range from {
		if f.status == s {
			f.status = to
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) rowStatuses() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, r := range f.rows {
		out[r.Status]++
	}
	return out
}

// fakeTurns scripts RunTurn by row company value.
type fakeTurns struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	block    chan struct{} // when set, RunTurn parks until closed
	reply    func(prompt string) (string, error)
}

func (f *fakeTurns) RunTurn(ctx context.Context, in agent.TurnInput, _ agent.EventSink) (*agent.TurnResult, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxSeen {
		f.maxSeen = f.inFlight
	}
	block := f.block
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inFlight--; f.mu.Unlock() }()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	text, err := f.reply(in.UserMessage)
	if err != nil {
		return nil, err
	}
	return &agent.TurnResult{FinalText: text, CostUSD: 0.01}, nil
}

func newFixture(nRows int, reply func(string) (string, error)) (*fakeStore, *fakeTurns, *Runner) {
	cols := []models.DatasetColumn{
		{Name: "company", Type: models.DatasetColumnText},
		{Name: "summary", Type: models.DatasetColumnText, Output: true},
	}
	d := &models.Dataset{ID: uuid.New(), Name: "t", Goal: "summarize", Columns: cols, Model: "m", Concurrency: 2}
	store := &fakeStore{dataset: d, status: models.DatasetStatusIdle}
	for i := 0; i < nRows; i++ {
		cells, _ := json.Marshal(map[string]any{"company": fmt.Sprintf("co-%d", i)})
		store.rows = append(store.rows, &models.DatasetRow{
			ID: uuid.New(), DatasetID: d.ID, RowIndex: i, Cells: cells, Status: models.DatasetRowPending,
		})
	}
	turns := &fakeTurns{reply: reply}
	return store, turns, New(store, turns)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestRunner_DrainsToProposed(t *testing.T) {
	store, turns, r := newFixture(5, func(string) (string, error) {
		return `{"summary":"fine company"}`, nil
	})
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return store.rowStatuses()[models.DatasetRowProposed] == 5 })
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.status == models.DatasetStatusIdle
	})
	if turns.maxSeen > 2 {
		t.Fatalf("concurrency 2 exceeded: saw %d in flight", turns.maxSeen)
	}
	for _, row := range store.rows {
		if string(row.Proposed) == "" || !strings.Contains(string(row.Proposed), "fine company") {
			t.Fatalf("row %d proposal: %s", row.RowIndex, row.Proposed)
		}
		if row.CostUSD == 0 {
			t.Fatalf("row %d cost not recorded", row.RowIndex)
		}
	}
}

func TestRunner_NonconformingBecomesNoteNeverCells(t *testing.T) {
	store, _, r := newFixture(1, func(string) (string, error) {
		return "A lovely free-form essay about the company.", nil
	})
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return store.rowStatuses()[models.DatasetRowFailed] == 1 })
	row := store.rows[0]
	if len(row.Proposed) != 0 {
		t.Fatalf("free-form answer must NEVER become a proposed write-back: %s", row.Proposed)
	}
	if !strings.Contains(row.ResultNote, "lovely free-form essay") {
		t.Fatalf("free-form answer must be preserved as a note: %q", row.ResultNote)
	}
	if !strings.Contains(row.Error, "did not conform") {
		t.Fatalf("error: %q", row.Error)
	}
}

func TestRunner_RunErrorFailsRow(t *testing.T) {
	store, _, r := newFixture(1, func(string) (string, error) {
		return "", errors.New("model exploded")
	})
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return store.rowStatuses()[models.DatasetRowFailed] == 1 })
	if !strings.Contains(store.rows[0].Error, "model exploded") {
		t.Fatalf("error: %q", store.rows[0].Error)
	}
}

func TestRunner_PauseParksAsPaused(t *testing.T) {
	store, turns, r := newFixture(10, func(string) (string, error) {
		return `{"summary":"x"}`, nil
	})
	turns.block = make(chan struct{}) // park every RunTurn until released
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		turns.mu.Lock()
		defer turns.mu.Unlock()
		return turns.inFlight == 2
	})
	if !r.Pause(store.dataset.ID) {
		t.Fatal("Pause should find an active run")
	}
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.status == models.DatasetStatusPaused
	})
	// Second Start on a paused dataset resumes.
	turns.mu.Lock()
	turns.block = nil
	turns.mu.Unlock()
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		st := store.rowStatuses()
		return st[models.DatasetRowPending] == 0 && st[models.DatasetRowRunning] == 0
	})
}

func TestRunner_DoubleStartRejected(t *testing.T) {
	store, turns, r := newFixture(4, func(string) (string, error) {
		return `{"summary":"x"}`, nil
	})
	turns.block = make(chan struct{})
	defer close(turns.block)
	if err := r.Start(context.Background(), store.dataset.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), store.dataset.ID); err == nil {
		t.Fatal("second Start must be rejected while running")
	}
	r.Pause(store.dataset.ID)
}
