1. **Change `RecoverStrandedTurns` to Batch Load Data:**
   We can significantly reduce the number of queries in the loop by batch loading `TurnJournalRow` and `TurnEvent` for all `turn_id`s in `todo` upfront.

2. **Details for `LoadTurnJournal` batching:**
   Instead of `LoadTurnJournal(ctx, turnID)` inside the loop, we can write a query that loads journals for all stranded turns using `WHERE turn_id = ANY($1)` (passing `turnIDs []string`). We then group these into a map `map[string][]TurnJournalRow`.

3. **Details for `LoadTurnEvents` batching:**
   Similarly, instead of `LoadTurnEvents(ctx, turnID, 0)`, we can batch load events for all stranded turns and group them into a map `map[string][]TurnEvent`.

4. **Refactor `recoverOneTurn` Signature:**
   Change `recoverOneTurn` to accept the pre-loaded `journal` and `events` as arguments:
   ```go
   func (s *Store) recoverOneTurn(ctx context.Context, turnID, convID string, journal []TurnJournalRow, events []TurnEvent) (RecoveredTurn, error)
   ```

5. **Run tests & pre-commit steps:**
   Run `go test -tags fleet_host_executor ./internal/store/...` to ensure everything works correctly. We will also run our benchmark again to verify the speed improvement.
   Complete pre-commit steps to ensure proper testing, verification, review, and reflection are done.
