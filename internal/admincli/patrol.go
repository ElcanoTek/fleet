package admincli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdPatrol implements `fleet patrol` — operator access to chat patrols (the
// recurring schedules people set from a chat's Patrol bar):
//
//	fleet patrol list [--user EMAIL] [--conversation ID] [--timers] [--all] [--limit N] [--json]
//	fleet patrol show <schedule_id> [--json | --prompt-only]
//	fleet patrol set-prompt <schedule_id> --file <path|-> --expect-sha256 <hex> --reason <text>
//	                        [--title <text>] [--notify=false] [--backup-dir DIR] [--dry-run] [--yes] [--json]
//
// The Patrol bar only lets a patrol's owner edit it. An operator who has to
// change a client's patrol (a prompt that still names a retired tool, a log
// that moved) used to have only a hand-written UPDATE on sched.chat_schedules,
// which skipped the def_version bump the scheduler relies on and left no trace
// in the chat. set-prompt is the safe form: it shows the diff first, refuses
// an edit built from a prompt that has since changed (compare-and-swap on the
// prompt's sha256), keeps the old prompt in a backup file that set-prompt can
// restore, bumps def_version like the owner's own edit, and writes a notice in
// the chat saying the instructions were changed and why.
func cmdPatrol(argv []string) int {
	if len(argv) == 0 {
		return errf(1, "usage: fleet patrol list|show|set-prompt (fleet patrol <verb> --help)")
	}
	switch argv[0] {
	case "list", "ls":
		return patrolList(argv[1:], os.Stdout)
	case "show", "get":
		return patrolShow(argv[1:], os.Stdout)
	case "set-prompt":
		return patrolSetPrompt(argv[1:], os.Stdin, os.Stdout, os.Stderr)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, patrolUsage)
		return 0
	default:
		return errf(1, "unknown patrol subcommand %q\n\n%s", argv[0], patrolUsage)
	}
}

const patrolUsage = `fleet patrol — operator access to chat patrols

  fleet patrol list [--user EMAIL] [--conversation ID] [--timers] [--all] [--limit N] [--json]
  fleet patrol show <schedule_id> [--json | --prompt-only]
  fleet patrol set-prompt <schedule_id> --file <path|-> --expect-sha256 <hex> --reason <text>
                          [--title <text>] [--notify=false] [--backup-dir DIR] [--dry-run] [--yes] [--json]

A schedule id may be shortened to its first 8+ characters. set-prompt prints the
diff, refuses when the prompt changed since --expect-sha256 was read (from show),
saves the old prompt to --backup-dir, and writes a notice into the chat.
`

type patrolAdminStore interface {
	AdminListChatSchedules(ctx context.Context, f store.AdminChatScheduleFilter) ([]store.AdminChatSchedule, error)
	AdminGetChatSchedule(ctx context.Context, idOrPrefix string) (store.AdminChatSchedule, error)
	AdminSetChatSchedulePrompt(ctx context.Context, in store.AdminChatSchedulePromptEdit) (before, after store.AdminChatSchedule, err error)
}

// openPatrolStore is swapped by tests.
var openPatrolStore = func(dbURL string) (patrolAdminStore, func(), int) {
	st, code := openChatStore(dbURL)
	if st == nil {
		return nil, func() {}, code
	}
	return st, func() { _ = st.Close() }, 0
}

// splitLeadingID lets the schedule id come before the flags
// (`fleet patrol show 5ee926c2 --json`), which Go's flag package would
// otherwise treat as the end of the flags.
func splitLeadingID(argv []string) (string, []string) {
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		return argv[0], argv[1:]
	}
	return "", argv
}

func patrolIDArg(fs *flag.FlagSet, lead string) (string, error) {
	rest := fs.Args()
	if lead == "" && len(rest) > 0 {
		lead, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 {
		return "", fmt.Errorf("unexpected argument %q", rest[0])
	}
	if strings.TrimSpace(lead) == "" {
		return "", errors.New("a schedule id is required (see fleet patrol list)")
	}
	return lead, nil
}

func patrolList(argv []string, out io.Writer) int {
	fs := flag.NewFlagSet("patrol list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	user := fs.String("user", "", "only this owner's schedules")
	conv := fs.String("conversation", "", "only this chat's schedules (id or prefix)")
	timers := fs.Bool("timers", false, "include one-shot timers (default: recurring patrols only)")
	all := fs.Bool("all", false, "include completed and cancelled schedules")
	limit := fs.Int("limit", 200, "maximum rows")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected argument %q", fs.Arg(0))
	}
	st, closeFn, code := openPatrolStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	list, err := st.AdminListChatSchedules(context.Background(), store.AdminChatScheduleFilter{
		UserEmail: *user, ConversationID: *conv, IncludeTerminal: *all, PatrolsOnly: !*timers, Limit: *limit,
	})
	if err != nil {
		return errf(1, "list patrols: %v", err)
	}
	if *asJSON {
		if list == nil {
			list = []store.AdminChatSchedule{}
		}
		for i := range list {
			list[i].Prompt = "" // the list is an index; `show` prints a prompt
		}
		return writeJSONOut(out, list)
	}
	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{
			shortScheduleID(s.ID), truncateRunes(s.ConversationTitle, 40), s.UserEmail,
			patrolWhen(s), s.Status, patrolNextRun(s), fmt.Sprintf("%d", len(s.Prompt)), s.PromptSHA256[:12],
		})
	}
	if err := renderTable(out, []string{"ID", "CHAT", "OWNER", "SCHEDULE", "STATUS", "NEXT RUN (UTC)", "PROMPT BYTES", "SHA256"}, rows); err != nil {
		return errf(1, "%v", err)
	}
	return 0
}

func patrolShow(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("patrol show", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	asJSON := fs.Bool("json", false, "JSON output")
	promptOnly := fs.Bool("prompt-only", false, "print only the prompt, byte for byte (to save, edit and pass to set-prompt --file)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	if *asJSON && *promptOnly {
		return errf(1, "--json and --prompt-only cannot be combined")
	}
	st, closeFn, code := openPatrolStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	s, err := st.AdminGetChatSchedule(context.Background(), id)
	if err != nil {
		return errf(1, "patrol %s: %v", id, err)
	}
	switch {
	case *asJSON:
		return writeJSONOut(out, s)
	case *promptOnly:
		_, _ = io.WriteString(out, s.Prompt)
		return 0
	}
	writePatrolHeader(out, s)
	fmt.Fprintln(out, "--- prompt ---")
	fmt.Fprintln(out, s.Prompt)
	return 0
}

func writePatrolHeader(out io.Writer, s store.AdminChatSchedule) {
	fmt.Fprintf(out, "id:             %s\n", s.ID)
	fmt.Fprintf(out, "title:          %s\n", s.Title)
	fmt.Fprintf(out, "chat:           %s (%s)\n", s.ConversationTitle, s.ConversationID)
	fmt.Fprintf(out, "owner:          %s\n", s.UserEmail)
	fmt.Fprintf(out, "schedule:       %s\n", patrolWhen(s))
	fmt.Fprintf(out, "status:         %s\n", s.Status)
	fmt.Fprintf(out, "next run (UTC): %s\n", patrolNextRun(s))
	fmt.Fprintf(out, "runs:           %d\n", s.RunCount)
	fmt.Fprintf(out, "fire in flight: %t\n", s.FireInFlight)
	fmt.Fprintf(out, "def_version:    %d\n", s.DefVersion)
	fmt.Fprintf(out, "prompt:         %d bytes, sha256 %s\n", len(s.Prompt), s.PromptSHA256)
}

type patrolSetPromptResult struct {
	ID            string `json:"id"`
	ChatID        string `json:"conversation_id"`
	Changed       bool   `json:"changed"`
	DryRun        bool   `json:"dry_run"`
	OldSHA256     string `json:"old_prompt_sha256"`
	NewSHA256     string `json:"new_prompt_sha256"`
	OldDefVersion int64  `json:"old_def_version"`
	NewDefVersion int64  `json:"new_def_version,omitempty"`
	BackupPath    string `json:"backup_path,omitempty"`
	QueuedRuns    int    `json:"queued_runs_updated,omitempty"`
	RunningOld    bool   `json:"running_with_old_prompt,omitempty"`
	Notice        string `json:"notice,omitempty"`
	Diff          string `json:"diff"`
}

const defaultPatrolBackupDir = "/var/backups/fleet/patrol-prompts"

func patrolSetPrompt(argv []string, stdin io.Reader, out, errW io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("patrol set-prompt", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	file := fs.String("file", "", "the new prompt: a file path, or - for stdin (required)")
	expect := fs.String("expect-sha256", "", "sha256 of the prompt the edit was built from, from `fleet patrol show` (required)")
	reason := fs.String("reason", "", "why the instructions changed; shown in the chat's notice (required)")
	title := fs.String("title", "", "also rename the patrol")
	notify := fs.Bool("notify", true, "write a notice into the chat saying the patrol's instructions were updated")
	backupDir := fs.String("backup-dir", defaultPatrolBackupDir, "directory the old prompt is saved to before the change")
	dryRun := fs.Bool("dry-run", false, "print the diff and change nothing")
	yes := fs.Bool("yes", false, "apply without the confirmation prompt")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	if strings.TrimSpace(*file) == "" {
		return errf(1, "--file is required (a path, or - for stdin)")
	}
	if strings.TrimSpace(*expect) == "" {
		return errf(1, "--expect-sha256 is required: copy it from `fleet patrol show %s`", id)
	}
	reasonText := oneLine(*reason)
	if reasonText == "" {
		return errf(1, "--reason is required: say why the instructions changed (it is shown in the chat)")
	}
	newPrompt, err := readPromptFile(*file, stdin)
	if err != nil {
		return errf(1, "%v", err)
	}

	st, closeFn, code := openPatrolStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	cur, err := st.AdminGetChatSchedule(ctx, id)
	if err != nil {
		return errf(1, "patrol %s: %v", id, err)
	}
	want := strings.ToLower(strings.TrimSpace(*expect))
	if cur.PromptSHA256 != want {
		return errf(1, "%v", &store.ChatSchedulePromptChangedError{Expected: want, Current: cur.PromptSHA256})
	}
	var titlePtr *string
	newTitle := cur.Title
	if t := strings.TrimSpace(*title); t != "" {
		titlePtr, newTitle = &t, t
	}
	res := patrolSetPromptResult{
		ID: cur.ID, ChatID: cur.ConversationID, DryRun: *dryRun,
		OldSHA256: cur.PromptSHA256, NewSHA256: store.PromptSHA256(newPrompt),
		OldDefVersion: cur.DefVersion,
		Diff:          lineDiff(cur.Prompt, newPrompt, 3),
	}
	if newTitle != cur.Title {
		res.Diff = fmt.Sprintf("title: %q -> %q\n", cur.Title, newTitle) + res.Diff
	}
	res.Changed = newPrompt != cur.Prompt || newTitle != cur.Title
	if !*asJSON {
		writePatrolHeader(out, cur)
		fmt.Fprintln(out)
		if res.Changed {
			fmt.Fprint(out, res.Diff)
		} else {
			fmt.Fprintln(out, "no change: the new prompt is identical to the current one")
		}
	}
	if !res.Changed || *dryRun {
		if *asJSON {
			return writeJSONOut(out, res)
		}
		return 0
	}
	if !*yes {
		if code := confirmPatrolEdit(stdin, errW, cur); code != 0 {
			return code
		}
	}
	backup, err := writePatrolBackup(*backupDir, cur, time.Now())
	if err != nil {
		return errf(1, "save the old prompt before changing it: %v (use --backup-dir to pick a writable directory)", err)
	}
	res.BackupPath = backup
	edit := store.AdminChatSchedulePromptEdit{ID: cur.ID, ExpectSHA256: want, Prompt: newPrompt, Title: titlePtr}
	if *notify {
		edit.Notice = patrolUpdatedNotice(newTitle, reasonText)
		res.Notice = edit.Notice
	}
	_, after, err := st.AdminSetChatSchedulePrompt(ctx, edit)
	if err != nil {
		return errf(1, "update patrol %s: %v (nothing was changed; the old prompt is also in %s)", cur.ID, err, backup)
	}
	res.NewDefVersion = after.DefVersion
	res.NewSHA256 = after.PromptSHA256
	res.QueuedRuns = after.QueuedRunsUpdated
	res.RunningOld = after.RunningWithOldPrompt
	if *asJSON {
		return writeJSONOut(out, res)
	}
	fmt.Fprintf(out, "\nupdated patrol %s: def_version %d -> %d, prompt sha256 %s\n", after.ID, cur.DefVersion, after.DefVersion, after.PromptSHA256)
	fmt.Fprintf(out, "old prompt saved to %s (restore: fleet patrol set-prompt %s --file %s --expect-sha256 %s --reason ...)\n", backup, shortScheduleID(after.ID), backup, after.PromptSHA256)
	if res.Notice != "" {
		fmt.Fprintf(out, "notice written to the chat: %s\n", res.Notice)
	}
	if cur.FireInFlight {
		fmt.Fprintln(out, "a run was in flight: it is superseded and the next run uses the new prompt")
	}
	if after.RunningWithOldPrompt {
		fmt.Fprintln(out, "note: a run of this patrol is running right now; it finishes with the old prompt (stop it in the chat if it must not)")
	}
	if after.QueuedRunsUpdated > 0 {
		fmt.Fprintf(out, "%d run(s) already waiting in the chat's queue now carry the new prompt\n", after.QueuedRunsUpdated)
	}
	return 0
}

func confirmPatrolEdit(stdin io.Reader, errW io.Writer, cur store.AdminChatSchedule) int {
	f, ok := stdin.(*os.File)
	if !ok || !isTerminal(f) {
		return errf(1, "refusing to change a patrol without confirmation: pass --yes (or --dry-run to preview)")
	}
	fmt.Fprintf(errW, "Change the prompt of %q in %q (owner %s)? [y/N]: ", cur.Title, cur.ConversationTitle, cur.UserEmail)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		fmt.Fprintln(errW, "aborted")
		return 1
	}
	return 0
}

func readPromptFile(path string, stdin io.Reader) (string, error) {
	var r io.Reader
	if path == "-" {
		r = stdin
	} else {
		f, err := os.Open(path) // #nosec G304 -- operator-supplied path on their own box
		if err != nil {
			return "", fmt.Errorf("read --file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, store.MaxAdminChatSchedulePromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read --file: %w", err)
	}
	if len(b) > store.MaxAdminChatSchedulePromptBytes {
		return "", fmt.Errorf("--file is over %d bytes; a patrol prompt is instructions, not data", store.MaxAdminChatSchedulePromptBytes)
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return "", errors.New("--file is empty")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("--file contains NUL bytes; it does not look like a prompt")
	}
	return p, nil
}

// writePatrolBackup saves the current prompt, byte for byte, so it can be fed
// straight back to set-prompt --file.
func writePatrolBackup(dir string, cur store.AdminChatSchedule, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-v%d.txt", cur.ID, now.UTC().Format("20060102T150405Z"), cur.DefVersion)
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- operator-chosen dir, id from the DB
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(f, cur.Prompt); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

func patrolUpdatedNotice(title, reason string) string {
	return fmt.Sprintf("An administrator updated the instructions of the patrol %q. Reason: %s", truncateRunes(title, 80), truncateRunes(reason, 300))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func shortScheduleID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func patrolWhen(s store.AdminChatSchedule) string {
	if s.CronExpr != "" {
		return s.CronExpr + " " + s.Timezone
	}
	return fmt.Sprintf("timer %ds", s.DurationSeconds)
}

func patrolNextRun(s store.AdminChatSchedule) string {
	if s.Status != store.ChatScheduleStatusActive || s.NextRunAt == 0 {
		return "-"
	}
	return time.Unix(s.NextRunAt, 0).UTC().Format("2006-01-02 15:04")
}

func writeJSONOut(out io.Writer, v any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return errf(1, "%v", err)
	}
	return 0
}

// lineDiff renders a unified-style line diff of a → b with `context` lines of
// context ("-" removed, "+" added, " " kept, "@@" between hunks). Patrol
// prompts are a few hundred lines, so the plain LCS table is cheap; past
// maxDiffCells it prints a summary instead of a diff.
func lineDiff(a, b string, context int) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	const maxDiffCells = 4_000_000
	if (len(al)+1)*(len(bl)+1) > maxDiffCells {
		return fmt.Sprintf("(diff too large to show: %d lines -> %d lines)\n", len(al), len(bl))
	}
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			ops = append(ops, op{' ', al[i]})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			// Removals before additions, as unified diffs print them.
			ops = append(ops, op{'-', al[i]})
			i++
		default:
			ops = append(ops, op{'+', bl[j]})
			j++
		}
	}
	// Keep changed lines plus `context` lines around them.
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind == ' ' {
			continue
		}
		for c := max(0, k-context); c <= min(len(ops)-1, k+context); c++ {
			keep[c] = true
		}
	}
	var sb strings.Builder
	prev := -2
	for k, o := range ops {
		if !keep[k] {
			continue
		}
		if k != prev+1 {
			sb.WriteString("@@\n")
		}
		sb.WriteByte(o.kind)
		sb.WriteString(o.text)
		sb.WriteByte('\n')
		prev = k
	}
	if sb.Len() == 0 {
		return "(no line changes)\n"
	}
	return sb.String()
}
