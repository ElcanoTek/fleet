package admincli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched"
)

// parseTaskID parses a positional task UUID argument.
func parseTaskID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// taskMemories dispatches `fleet-admin task memories ...` — operator inspection
// and clearing of a scheduled task's persistent memory (#198). Memory is written
// automatically by a Captain's Log task's remember tool; these subcommands let an
// operator observe and reset it.
//
// Exit codes: 0 ok · 1 usage · 2 not-found · 5 operational.
func taskMemories(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin task memories list|clear|delete <task_id> [key]")
	}
	switch argv[0] {
	case "list", "ls":
		return taskMemoriesList(argv[1:])
	case "clear":
		return taskMemoriesClear(argv[1:])
	case "delete", "del", "rm":
		return taskMemoriesDelete(argv[1:])
	default:
		return errf(1, "unknown task memories subcommand %q (want list|clear|delete)", argv[0])
	}
}

func taskMemoriesList(argv []string) int {
	fs := flag.NewFlagSet("task memories list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	taskID, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	tid, ok := parseTaskID(taskID)
	if !ok {
		return errf(1, "a valid task id is required")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	mems, err := st.ListTaskMemories(context.Background(), tid)
	if err != nil {
		return errf(5, "%v", err)
	}
	if len(mems) == 0 {
		fmt.Fprintln(os.Stderr, "(no memories for this task)")
		return 0
	}
	for _, m := range mems {
		// Tab-separated key/value; value newlines collapsed so one fact = one line.
		v := strings.ReplaceAll(m.Value, "\n", "\\n")
		fmt.Printf("%s\t%s\n", m.Key, v)
	}
	return 0
}

func taskMemoriesClear(argv []string) int {
	fs := flag.NewFlagSet("task memories clear", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	taskID, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	tid, ok := parseTaskID(taskID)
	if !ok {
		return errf(1, "a valid task id is required")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	n, err := st.DeleteAllTaskMemories(context.Background(), tid)
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("cleared %d memory key(s) for task %s\n", n, taskID)
	return 0
}

func taskMemoriesDelete(argv []string) int {
	fs := flag.NewFlagSet("task memories delete", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	// Two positionals: <task_id> <key>. splitPositional only peels the first, so
	// parse the rest manually after flags.
	rest := argv
	var taskID, key string
	var flagArgs []string
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			continue
		}
		if taskID == "" {
			taskID = a
		} else if key == "" {
			key = a
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	tid, ok := parseTaskID(taskID)
	if !ok {
		return errf(1, "a valid task id is required")
	}
	if strings.TrimSpace(key) == "" {
		return errf(1, "a key is required: fleet-admin task memories delete <task_id> <key>")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	err := st.DeleteTaskMemory(context.Background(), tid, key)
	if errors.Is(err, sched.ErrTaskMemoryNotFound) {
		return errf(2, "no memory %q for task %s", key, taskID)
	}
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("deleted memory %q for task %s\n", key, taskID)
	return 0
}
