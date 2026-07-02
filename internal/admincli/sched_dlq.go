package admincli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// cmdSchedDLQ dispatches the dead-letter-queue operator verbs (#253):
//
//	fleet-admin sched dlq list [--tag <tag>] [--limit N] [--offset N] [--json]
//	fleet-admin sched dlq replay <task_id>
//
// These build on the existing fleet-admin sched plumbing (openSchedStorage) and
// the re-enqueue seam from #270 (TaskToCreate / EnqueueTask) is shared with the
// replay reset performed by storage.ReplayDeadLetteredTask. The DLQ surface is
// deliberately an admin CLI rather than an orchestrator HTTP route + openapi
// change: review/replay is an operator action on the box, matching the other
// `sched` verbs (user, apikey, task, trigger).
func cmdSchedDLQ(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet sched dlq list|replay ...")
	}
	switch argv[0] {
	case "list", "ls":
		return schedDLQList(argv[1:])
	case "replay":
		return schedDLQReplay(argv[1:])
	default:
		return errf(1, "unknown sched dlq subcommand %q (want list|replay)", argv[0])
	}
}

// schedDLQList prints dead-lettered tasks, newest-quarantined first. Each row is
// task_id, dead_lettered_at, attempts, tags, and a one-line reason. --tag filters
// to tasks carrying that tag; --json emits the full task objects for scripting.
func schedDLQList(argv []string) int {
	fs := flag.NewFlagSet("sched dlq list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	tag := fs.String("tag", "", "only show dead-lettered tasks carrying this tag")
	limit := fs.Int("limit", 50, "max rows to return (<=0 = all)")
	offset := fs.Int("offset", 0, "rows to skip (pagination)")
	asJSON := fs.Bool("json", false, "emit the tasks as a JSON array")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	tasks, err := st.GetDeadLetteredTasks(context.Background(), *limit, *offset)
	if err != nil {
		return errf(5, "list dead-letter queue: %v", err)
	}
	if t := strings.TrimSpace(*tag); t != "" {
		tasks = filterTasksByTag(tasks, t)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(tasks); err != nil {
			return errf(5, "encode tasks: %v", err)
		}
		return 0
	}

	if len(tasks) == 0 {
		fmt.Fprintln(os.Stderr, "dead-letter queue is empty")
		return 0
	}
	for _, t := range tasks {
		when := "?"
		if t.DeadLetteredAt != nil {
			when = t.DeadLetteredAt.UTC().Format(time.RFC3339)
		}
		reason := ""
		if t.DeadLetterReason != nil {
			reason = oneLine(*t.DeadLetterReason)
		}
		tags := "-"
		if len(t.Tags) > 0 {
			tags = strings.Join(t.Tags, ",")
		}
		fmt.Printf("%s\t%s\tattempts=%d\ttags=%s\t%s\n", t.ID, when, t.DeadLetterAttempts, tags, reason)
	}
	fmt.Fprintf(os.Stderr, "%d dead-lettered task(s)\n", len(tasks))
	return 0
}

// schedDLQReplay re-enqueues one dead-lettered task: it resets the row to a fresh
// pending slate (attempt_count=0, DLQ columns cleared) so the scheduler's normal
// claim path runs it again. Errors if the task is not currently dead-lettered.
func schedDLQReplay(argv []string) int {
	fs := flag.NewFlagSet("sched dlq replay", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errf(1, "usage: fleet sched dlq replay <task_id>")
	}
	taskID, err := uuid.Parse(strings.TrimSpace(rest[0]))
	if err != nil {
		return errf(1, "invalid task id %q: %v", rest[0], err)
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	updated, err := st.ReplayDeadLetteredTask(context.Background(), taskID)
	if err != nil {
		if errors.Is(err, storage.ErrTaskNotDeadLettered) {
			return errf(4, "task %s is not dead-lettered (only dead-lettered tasks can be replayed)", taskID)
		}
		return errf(5, "replay task: %v", err)
	}
	fmt.Fprintf(os.Stderr, "replayed task %s — re-enqueued as %s (attempt_count reset to 0)\n", updated.ID, updated.Status)
	return 0
}

// filterTasksByTag returns the subset of tasks carrying tag (exact match against
// the task's normalized Tags).
func filterTasksByTag(tasks []*models.Task, tag string) []*models.Task {
	out := make([]*models.Task, 0, len(tasks))
	for _, t := range tasks {
		for _, tg := range t.Tags {
			if tg == tag {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// oneLine collapses a multi-line reason into a single space-joined line so the
// tabular listing stays one row per task.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
