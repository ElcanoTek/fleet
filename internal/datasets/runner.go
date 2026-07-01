package datasets

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TurnRunner is the slice of *agent.Manager one row run needs — the SAME
// governed interactive entrypoint chat turns and eval replays use.
type TurnRunner interface {
	RunTurn(ctx context.Context, in agent.TurnInput, sink agent.EventSink) (*agent.TurnResult, error)
}

// Store is the persistence surface the runner needs (satisfied by
// *storage.Storage). Claiming is atomic (pending→running under
// FOR UPDATE SKIP LOCKED) so concurrent workers never double-claim.
type Store interface {
	GetDataset(ctx context.Context, id uuid.UUID) (*models.Dataset, error)
	ClaimNextDatasetRow(ctx context.Context, datasetID uuid.UUID) (*models.DatasetRow, error)
	FinishDatasetRow(ctx context.Context, rowID uuid.UUID, proposed json.RawMessage, note, errMsg string, costUSD float64) error
	UpdateDatasetStatus(ctx context.Context, id uuid.UUID, from []string, to string) (bool, error)
}

// Runner drives dataset runs inside the fleet serve process: Start claims the
// dataset (idle|paused → running) and fans out Concurrency worker goroutines
// that each loop "claim one pending row → run it → write the outcome" until
// no pending rows remain (→ idle) or Pause cancels the run (→ paused). One
// active run per dataset; state transitions are DB-guarded so a second Start
// can't double-run.
type Runner struct {
	store Store
	turns TurnRunner

	mu      sync.Mutex
	cancels map[uuid.UUID]context.CancelFunc
}

// New builds the runner. It holds no background loops of its own — work only
// happens between Start and the run draining/pausing.
func New(store Store, turns TurnRunner) *Runner {
	return &Runner{store: store, turns: turns, cancels: map[uuid.UUID]context.CancelFunc{}}
}

// maxRowAttempts bounds re-claims of a crashing row (a row is re-claimable
// only after an explicit reset, but attempts also guards a pathological
// reset-loop from an automation).
const maxRowAttempts = 10

// Start claims the dataset and launches the run. Idempotent-ish: a dataset
// already running returns an error the handler maps to 409.
func (r *Runner) Start(ctx context.Context, id uuid.UUID) error {
	d, err := r.store.GetDataset(ctx, id)
	if err != nil {
		return err
	}
	ok, err := r.store.UpdateDatasetStatus(ctx, id, []string{models.DatasetStatusIdle, models.DatasetStatusPaused}, models.DatasetStatusRunning)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("dataset %s is already running", id)
	}

	// The run outlives the HTTP request: detach from the request context but
	// keep a cancel handle for Pause/shutdown. The goroutine's own defer
	// releases the context on natural drain (Pause/Shutdown are idempotent).
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()

	go func() {
		defer cancel()
		r.run(runCtx, d)
	}()
	return nil
}

// Pause cancels an active run. The workers observe the cancel between rows
// (an in-flight row run is cancelled too — its context derives from the
// run's) and the run loop parks the dataset as paused on exit.
func (r *Runner) Pause(id uuid.UUID) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Shutdown cancels every active run (process drain).
func (r *Runner) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, cancel := range r.cancels {
		cancel()
		delete(r.cancels, id)
	}
}

func (r *Runner) run(ctx context.Context, d *models.Dataset) {
	defer func() {
		// Park the terminal status: paused when Pause/Shutdown cancelled the
		// run, idle when it drained naturally. Read ctx.Err() HERE — before the
		// Start goroutine's own defer cancel() fires — so a drain never parks
		// as paused. Detached context: the write must survive the cancel.
		to := models.DatasetStatusIdle
		if ctx.Err() != nil {
			to = models.DatasetStatusPaused
		}
		r.mu.Lock()
		delete(r.cancels, d.ID)
		r.mu.Unlock()
		if _, err := r.store.UpdateDatasetStatus(context.WithoutCancel(ctx), d.ID, []string{models.DatasetStatusRunning}, to); err != nil {
			log.Printf("dataset %s: park status %s: %v", d.ID, to, err)
		}
	}()

	schema, err := OutputSchema(d.Columns)
	if err != nil {
		log.Printf("dataset %s: output schema: %v", d.ID, err)
		return
	}

	concurrency := d.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				row, err := r.store.ClaimNextDatasetRow(ctx, d.ID)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("dataset %s: claim: %v", d.ID, err)
					}
					return
				}
				if row == nil {
					return // drained
				}
				if row.Attempts > maxRowAttempts {
					_ = r.store.FinishDatasetRow(context.WithoutCancel(ctx), row.ID, nil, "", fmt.Sprintf("row exceeded %d attempts", maxRowAttempts), 0)
					continue
				}
				r.runRow(ctx, d, row, schema)
			}
		}()
	}
	wg.Wait()
}

// noopSink discards run events — dataset rows have no SSE stream (their
// outcome is the row status + note; per-row transcripts are a follow-on).
type noopSink struct{}

func (noopSink) Emit(string, any) {}

// runRow executes one governed row run and records the outcome. The write
// uses a cancel-detached context so a Pause mid-write still persists the
// result of work already paid for.
func (r *Runner) runRow(ctx context.Context, d *models.Dataset, row *models.DatasetRow, schema json.RawMessage) {
	prompt, err := RowPrompt(d, row)
	if err != nil {
		_ = r.store.FinishDatasetRow(context.WithoutCancel(ctx), row.ID, nil, "", err.Error(), 0)
		return
	}
	prompt += structuredoutput.PromptAugmentation(schema)

	turn, err := r.turns.RunTurn(ctx, agent.TurnInput{
		UserMessage:    prompt,
		Model:          d.Model,
		Persona:        d.Persona,
		ConversationID: fmt.Sprintf("dataset-%s-row-%d", d.ID, row.RowIndex),
	}, noopSink{})
	wctx := context.WithoutCancel(ctx)
	if err != nil {
		if ferr := r.store.FinishDatasetRow(wctx, row.ID, nil, "", clamp(err.Error(), maxNoteChars), 0); ferr != nil {
			log.Printf("dataset %s row %d: record failure: %v", d.ID, row.RowIndex, ferr)
		}
		return
	}

	proposed, verr := structuredoutput.ValidateOutput(turn.FinalText, schema)
	if verr != nil {
		// The member-triaged contract: a free-form answer is a NOTE, never a
		// cell mutation. The row fails with the answer preserved for review.
		note := clamp(strings.TrimSpace(turn.FinalText), maxNoteChars)
		if ferr := r.store.FinishDatasetRow(wctx, row.ID, nil, note, "output did not conform to the dataset schema: "+clamp(verr.Error(), 500), turn.CostUSD); ferr != nil {
			log.Printf("dataset %s row %d: record nonconforming: %v", d.ID, row.RowIndex, ferr)
		}
		return
	}
	if ferr := r.store.FinishDatasetRow(wctx, row.ID, proposed, "", "", turn.CostUSD); ferr != nil {
		log.Printf("dataset %s row %d: record proposal: %v", d.ID, row.RowIndex, ferr)
	}
}

func clamp(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…[truncated]"
}
