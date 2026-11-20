package admincli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdChats implements `fleet chats` — chat administration from the host
// (ADR-0115; the same powers as Settings -> Chats in the browser):
//
//	fleet chats list    [--user EMAIL] [--search TEXT] [--active-days N] [--archived] [--limit N] [--json]
//	fleet chats show    <chat_id> [--json]
//	fleet chats history <chat_id> [--tail N] [--tools] [--json]
//	fleet chats send    <chat_id> (--message TEXT | --file PATH|-) --reason TEXT [--wait] [--timeout 10m]
//	fleet chats stop    <chat_id> --reason TEXT [--wait]
//	fleet chats transfer <chat_id> --to EMAIL --reason TEXT [--wait]
//	fleet chats notice  <chat_id> --text TEXT
//	fleet chats set     <chat_id> [--title T] [--model SLUG] [--approval-mode manual] [--archive|--unarchive] --reason TEXT
//	fleet chats actions [<chat_id>] [--limit N] [--json]
//	fleet chats request <request_id> [--json]
//
// Reads and settings changes go straight to the database. A message, a stop
// and a transfer must be carried out by the running server (a turn, a
// running turn's cancellation, the reconciliation a transfer needs), so the
// CLI records them as admin requests and the server executes them on its next
// scheduler pass (at most FLEET_SCHED_TICK_SECONDS, 30 s by default); --wait
// follows the request to its end. Every change is recorded with --as (default
// <unix user>@<host>) and announced in the chat.
func cmdChats(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, chatsUsage)
		return 1
	}
	out, errW := os.Stdout, os.Stderr
	switch argv[0] {
	case "list", "ls":
		return chatsList(argv[1:], out)
	case "show", "get":
		return chatsShow(argv[1:], out)
	case "history", "transcript":
		return chatsHistory(argv[1:], out)
	case "send":
		return chatsRequest(store.AdminRequestMessage, argv[1:], os.Stdin, out, errW)
	case "stop":
		return chatsRequest(store.AdminRequestStop, argv[1:], os.Stdin, out, errW)
	case "transfer":
		return chatsRequest(store.AdminRequestTransfer, argv[1:], os.Stdin, out, errW)
	case "notice":
		return chatsNotice(argv[1:], out)
	case "set":
		return chatsSet(argv[1:], out)
	case "actions", "audit":
		return chatsActions(argv[1:], out)
	case "request":
		return chatsRequestStatus(argv[1:], out)
	case "patrol", "patrols":
		return cmdPatrol(argv[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, chatsUsage)
		return 0
	default:
		return errf(1, "unknown chats subcommand %q\n\n%s", argv[0], chatsUsage)
	}
}

const chatsUsage = `fleet chats — administer any chat on this box (ADR-0115; also Settings -> Chats)

  fleet chats list     [--user EMAIL] [--search TEXT] [--active-days N] [--archived] [--limit N] [--json]
  fleet chats show     <chat_id> [--json]                 (owner, model, approvals, patrols, queue, recent admin changes)
  fleet chats history  <chat_id> [--tail 30] [--tools] [--json]
  fleet chats send     <chat_id> (--message TEXT | --file PATH|-) --reason TEXT [--wait] [--timeout 10m]
  fleet chats stop     <chat_id> --reason TEXT [--wait]   (the owner's Stop: running turn + queued inputs)
  fleet chats transfer <chat_id> --to EMAIL --reason TEXT [--wait]
  fleet chats notice   <chat_id> --text TEXT              (a note in the chat; the model never sees it)
  fleet chats set      <chat_id> [--title T] [--model SLUG] [--approval-mode manual] [--archive|--unarchive] --reason TEXT
  fleet chats actions  [<chat_id>] [--limit 50] [--json]  (the record of admin changes)
  fleet chats request  <request_id> [--json]              (an admin request's progress)
  fleet chats patrol   list|show|set-prompt|pause|resume  (same as fleet patrol)

Every change is recorded as --as (default <unix user>@<host>) and noted
in the chat. An admin message runs as the chat's owner, with the owner's connectors, labelled
as yours; it always runs in Manual, so anything that needs approval asks the owner. send, stop and
transfer are carried out by the running server on its next scheduler pass (30 s by default).
`

// chatsStore is what `fleet chats` needs from the store.
type chatsStore interface {
	AdminListChats(ctx context.Context, f store.AdminChatFilter) ([]store.AdminChat, error)
	AdminGetChat(ctx context.Context, idOrPrefix string) (store.AdminChat, error)
	AdminChatTranscript(ctx context.Context, convID string, o store.AdminTranscriptOptions) ([]store.AdminTranscriptEntry, error)
	AdminAppendNotice(ctx context.Context, a store.AdminActor, convID, text string) error
	AdminUpdateChat(ctx context.Context, a store.AdminActor, convID string, c store.AdminChatChange, reason string) (store.AdminChat, error)
	ListAdminActions(ctx context.Context, convID string, limit int) ([]store.AdminAction, error)
	CreateAdminRequest(ctx context.Context, in store.AdminRequestInput) (store.AdminRequest, error)
	GetAdminRequest(ctx context.Context, id string) (store.AdminRequest, error)
	AdminListChatSchedules(ctx context.Context, f store.AdminChatScheduleFilter) ([]store.AdminChatSchedule, error)
	ListQueuedInputs(ctx context.Context, userEmail, convID string) ([]store.InputQueueRow, error)
}

// openChatsStore is swapped by tests.
var openChatsStore = func(dbURL string) (chatsStore, func(), int) {
	st, code := openChatStore(dbURL)
	if st == nil {
		return nil, func() {}, code
	}
	return st, func() { _ = st.Close() }, 0
}

// cliAdminActor is who a CLI change is recorded as: --as, else
// <unix user>@<host>.
func cliAdminActor(as string) store.AdminActor {
	a := strings.TrimSpace(as)
	if a == "" {
		name := "root"
		if u, err := user.Current(); err == nil && u.Username != "" {
			name = u.Username
		}
		host, _ := os.Hostname()
		if host == "" {
			host = "localhost"
		}
		a = name + "@" + host
	}
	return store.AdminActor{Email: strings.ToLower(a), Via: store.AdminViaCLI}
}

func fmtTime(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04")
}

func chatsList(argv []string, out io.Writer) int {
	fs := flag.NewFlagSet("chats list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	owner := fs.String("user", "", "only this owner's chats")
	search := fs.String("search", "", "title or id contains")
	days := fs.Int("active-days", 0, "only chats active in the last N days")
	archived := fs.Bool("archived", false, "include archived chats")
	limit := fs.Int("limit", 50, "maximum rows")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected argument %q", fs.Arg(0))
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	chats, err := st.AdminListChats(context.Background(), store.AdminChatFilter{
		OwnerEmail: *owner, Search: *search, ActiveWithinDays: *days, IncludeArchived: *archived, Limit: *limit,
	})
	if err != nil {
		return errf(1, "list chats: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, chats)
	}
	rows := make([][]string, 0, len(chats))
	for _, c := range chats {
		flags := []string{}
		if c.TurnRunning {
			flags = append(flags, "running")
		}
		if c.ActivePatrols > 0 {
			flags = append(flags, fmt.Sprintf("patrols:%d", c.ActivePatrols))
		}
		if c.PendingApprovals > 0 {
			flags = append(flags, fmt.Sprintf("cards:%d", c.PendingApprovals))
		}
		if c.QueuedInputs > 0 {
			flags = append(flags, fmt.Sprintf("queued:%d", c.QueuedInputs))
		}
		if c.Archived {
			flags = append(flags, "archived")
		}
		if c.Lockdown {
			flags = append(flags, "lockdown")
		}
		rows = append(rows, []string{shortScheduleID(c.ID), truncateRunes(c.Title, 44), c.OwnerEmail, c.ApprovalMode, fmtTime(c.UpdatedAt), strings.Join(flags, " ")})
	}
	if err := renderTable(out, []string{"ID", "TITLE", "OWNER", "APPROVALS", "UPDATED (UTC)", "STATE"}, rows); err != nil {
		return errf(1, "%v", err)
	}
	return 0
}

func chatsShow(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats show", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	c, err := st.AdminGetChat(ctx, id)
	if err != nil {
		return errf(1, "chat %s: %v", id, err)
	}
	patrols, err := st.AdminListChatSchedules(ctx, store.AdminChatScheduleFilter{ConversationID: c.ID})
	if err != nil {
		return errf(1, "patrols: %v", err)
	}
	queue, err := st.ListQueuedInputs(ctx, "", c.ID)
	if err != nil {
		return errf(1, "queue: %v", err)
	}
	actions, err := st.ListAdminActions(ctx, c.ID, 10)
	if err != nil {
		return errf(1, "admin actions: %v", err)
	}
	if *asJSON {
		for i := range patrols {
			patrols[i].Prompt = ""
		}
		return writeJSONOut(out, map[string]any{"chat": c, "patrols": patrols, "queued_inputs": len(queue), "actions": actions})
	}
	fmt.Fprintf(out, "id:          %s\n", c.ID)
	fmt.Fprintf(out, "title:       %s\n", c.Title)
	fmt.Fprintf(out, "owner:       %s\n", c.OwnerEmail)
	if len(c.Participants) > 0 {
		fmt.Fprintf(out, "participants: %s\n", strings.Join(c.Participants, ", "))
	}
	fmt.Fprintf(out, "model:       %s\n", c.Model)
	fmt.Fprintf(out, "approvals:   %s\n", c.ApprovalMode)
	fmt.Fprintf(out, "persona:     %s\n", c.Persona)
	fmt.Fprintf(out, "updated:     %s UTC (created %s)\n", fmtTime(c.UpdatedAt), fmtTime(c.CreatedAt))
	fmt.Fprintf(out, "state:       running=%t archived=%t lockdown=%t multiplayer=%t team_shared=%t\n", c.TurnRunning, c.Archived, c.Lockdown, c.Multiplayer, c.TeamShared)
	fmt.Fprintf(out, "waiting:     %d approval card(s), %d queued input(s)\n", c.PendingApprovals, len(queue))
	if len(patrols) > 0 {
		fmt.Fprintln(out, "\npatrols:")
		for _, p := range patrols {
			fmt.Fprintf(out, "  %s  %-8s %-28s %s  next %s  (fleet patrol show %s)\n", shortScheduleID(p.ID), p.Status, truncateRunes(p.Title, 28), patrolWhen(p), patrolNextRun(p), shortScheduleID(p.ID))
		}
	}
	if len(actions) > 0 {
		fmt.Fprintln(out, "\nrecent admin changes:")
		for _, a := range actions {
			fmt.Fprintf(out, "  %s  %-16s %s (%s)  %s\n", fmtTime(a.CreatedAt), a.Action, a.Actor, a.Via, truncateRunes(oneLine(a.Reason+" "+a.Detail), 80))
		}
	}
	return 0
}

func chatsHistory(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats history", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	tail := fs.Int("tail", 30, "how many of the latest entries")
	tools := fs.Bool("tools", false, "include tool calls and results")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	c, err := st.AdminGetChat(ctx, id)
	if err != nil {
		return errf(1, "chat %s: %v", id, err)
	}
	entries, err := st.AdminChatTranscript(ctx, c.ID, store.AdminTranscriptOptions{Tail: *tail, IncludeTools: *tools})
	if err != nil {
		return errf(1, "history: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, entries)
	}
	fmt.Fprintf(out, "%s — %s (owner %s), last %d entries\n\n", c.Title, c.ID, c.OwnerEmail, len(entries))
	for _, e := range entries {
		fmt.Fprintf(out, "[%s] %s\n%s\n\n", fmtTime(e.CreatedAt), transcriptLabel(e, c.OwnerEmail), strings.TrimSpace(e.Text))
	}
	return 0
}

func transcriptLabel(e store.AdminTranscriptEntry, owner string) string {
	switch {
	case e.Type == "notice":
		return "notice (" + e.Kind + ")"
	case e.Type == "tool_call":
		return "tool call: " + e.Tool
	case e.Type == "tool_result" && e.IsErr:
		return "tool result (error): " + e.Tool
	case e.Type == "tool_result":
		return "tool result: " + e.Tool
	case e.Role == "user" && e.Kind == "admin_message":
		return "admin message from " + e.Sender
	case e.Role == "user" && e.Kind == "chat_patrol":
		return "patrol run"
	case e.Role == "user" && e.Kind != "":
		return "fleet (" + e.Kind + ")"
	case e.Role == "user" && e.Sender != "":
		return "user " + e.Sender
	case e.Role == "user":
		return "user " + owner
	default:
		return e.Role
	}
}

func chatsRequest(kind string, argv []string, stdin io.Reader, out, errW io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats "+kind, flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	as := fs.String("as", "", "record the change as this administrator (default <unix user>@<host>)")
	reason := fs.String("reason", "", "why; recorded and shown in the chat (required)")
	wait := fs.Bool("wait", false, "wait until the server has carried the request out (and, for a message, the turn has ended)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long --wait waits")
	asJSON := fs.Bool("json", false, "JSON output")
	var message, file, to *string
	switch kind {
	case store.AdminRequestMessage:
		message = fs.String("message", "", "the message")
		file = fs.String("file", "", "read the message from a file (- for stdin)")
	case store.AdminRequestTransfer:
		to = fs.String("to", "", "the new owner's email (required)")
	}
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	payload := ""
	switch kind {
	case store.AdminRequestMessage:
		switch {
		case *message != "" && *file != "":
			return errf(1, "give --message or --file, not both")
		case *file != "":
			b, err := readPromptFile(*file, stdin)
			if err != nil {
				return errf(1, "%v", err)
			}
			payload = b
		default:
			payload = *message
		}
		if strings.TrimSpace(payload) == "" {
			return errf(1, "--message (or --file) is required")
		}
	case store.AdminRequestTransfer:
		payload = *to
		if strings.TrimSpace(payload) == "" {
			return errf(1, "--to is required")
		}
	}
	if strings.TrimSpace(*reason) == "" {
		return errf(1, "--reason is required: it is recorded and shown in the chat")
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	req, err := st.CreateAdminRequest(ctx, store.AdminRequestInput{
		Actor: cliAdminActor(*as), Kind: kind, ConversationID: id, Payload: payload, Reason: *reason,
	})
	if err != nil {
		return errf(1, "%s: %v", kind, err)
	}
	if !*asJSON {
		fmt.Fprintf(out, "admin request %s recorded (%s in chat %s); the server carries it out on its next pass\n", req.ID, kind, shortScheduleID(req.ConversationID))
	}
	if !*wait {
		if *asJSON {
			return writeJSONOut(out, req)
		}
		fmt.Fprintf(out, "follow it with: fleet chats request %s\n", req.ID)
		return 0
	}
	final, err := waitAdminRequest(ctx, st, req.ID, *timeout, errW)
	if err != nil {
		return errf(1, "%v (follow it with: fleet chats request %s)", err, req.ID)
	}
	if *asJSON {
		return writeJSONOut(out, final)
	}
	printAdminRequest(out, final)
	if final.State == store.AdminRequestFailed {
		return 1
	}
	if kind == store.AdminRequestMessage {
		if reply := latestReply(ctx, st, final); reply != "" {
			fmt.Fprintf(out, "\nreply:\n%s\n", reply)
		}
	}
	return 0
}

// waitPollInterval is how often --wait polls; a var so tests can shrink it.
var waitPollInterval = 2 * time.Second

// waitAdminRequest polls a request until it is settled and, for a message,
// until its turn has ended.
func waitAdminRequest(ctx context.Context, st chatsStore, id string, timeout time.Duration, errW io.Writer) (store.AdminRequest, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		r, err := st.GetAdminRequest(ctx, id)
		if err != nil {
			return store.AdminRequest{}, err
		}
		phase := r.State + "/" + r.InputState
		if phase != last {
			fmt.Fprintf(errW, "  %s\n", describeRequestPhase(r))
			last = phase
		}
		if requestFinished(r) {
			return r, nil
		}
		if time.Now().After(deadline) {
			return r, errors.New("timed out waiting for the server")
		}
		time.Sleep(waitPollInterval)
	}
}

func requestFinished(r store.AdminRequest) bool {
	switch r.State {
	case store.AdminRequestFailed:
		return true
	case store.AdminRequestDone:
		if r.Kind != store.AdminRequestMessage {
			return true
		}
		return r.InputState == store.InputStateCompleted || r.InputState == store.InputStateCancelled
	}
	return false
}

func describeRequestPhase(r store.AdminRequest) string {
	switch {
	case r.State == store.AdminRequestPending:
		return "waiting for the server to pick the request up…"
	case r.State == store.AdminRequestRunning:
		return "the server is carrying it out…"
	case r.State == store.AdminRequestFailed:
		return "failed: " + r.Result
	case r.Kind == store.AdminRequestMessage && r.InputState == store.InputStateQueued:
		return "queued in the chat (waits for any running turn)…"
	case r.Kind == store.AdminRequestMessage && r.InputState == store.InputStateRunning:
		return "the turn is running…"
	case r.Kind == store.AdminRequestMessage && r.InputState != "":
		return "the turn ended (" + r.InputState + ")"
	default:
		return "done: " + r.Result
	}
}

func printAdminRequest(out io.Writer, r store.AdminRequest) {
	fmt.Fprintf(out, "request:  %s\n", r.ID)
	fmt.Fprintf(out, "kind:     %s in chat %s\n", r.Kind, r.ConversationID)
	fmt.Fprintf(out, "by:       %s (%s) at %s UTC\n", r.Actor, r.Via, fmtTime(r.CreatedAt))
	fmt.Fprintf(out, "reason:   %s\n", r.Reason)
	fmt.Fprintf(out, "state:    %s", r.State)
	if r.Result != "" {
		fmt.Fprintf(out, " — %s", r.Result)
	}
	fmt.Fprintln(out)
	if r.InputID != "" {
		fmt.Fprintf(out, "turn:     input %s, %s", r.InputID, valueOr(r.InputState, "gone (purged)"))
		if r.TurnID != "" {
			fmt.Fprintf(out, ", turn %s", r.TurnID)
		}
		fmt.Fprintln(out)
	}
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// latestReply returns the assistant's last text after the admin message.
func latestReply(ctx context.Context, st chatsStore, r store.AdminRequest) string {
	entries, err := st.AdminChatTranscript(ctx, r.ConversationID, store.AdminTranscriptOptions{Tail: 40, MaxTextRunes: 6000})
	if err != nil {
		return ""
	}
	reply := ""
	seen := false
	for _, e := range entries {
		if e.Role == "user" && e.Kind == "admin_message" && e.CreatedAt >= r.CreatedAt {
			seen, reply = true, ""
			continue
		}
		if seen && e.Role == "assistant" && e.Type == "text" && strings.TrimSpace(e.Text) != "" {
			reply = strings.TrimSpace(e.Text)
		}
	}
	return reply
}

func chatsRequestStatus(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats request", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	r, err := st.GetAdminRequest(context.Background(), id)
	if err != nil {
		return errf(1, "%v", err)
	}
	if *asJSON {
		return writeJSONOut(out, r)
	}
	printAdminRequest(out, r)
	return 0
}

func chatsNotice(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats notice", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	as := fs.String("as", "", "record the change as this administrator")
	text := fs.String("text", "", "the notice (required)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	if strings.TrimSpace(*text) == "" {
		return errf(1, "--text is required")
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	c, err := st.AdminGetChat(ctx, id)
	if err != nil {
		return errf(1, "chat %s: %v", id, err)
	}
	if err := st.AdminAppendNotice(ctx, cliAdminActor(*as), c.ID, *text); err != nil {
		return errf(1, "notice: %v", err)
	}
	fmt.Fprintf(out, "notice written to %q (%s)\n", c.Title, c.ID)
	return 0
}

func chatsSet(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats set", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	as := fs.String("as", "", "record the change as this administrator")
	title := fs.String("title", "", "rename the chat")
	model := fs.String("model", "", "switch the chat's model")
	mode := fs.String("approval-mode", "", "manual (an administrator can turn approvals down, never up)")
	archive := fs.Bool("archive", false, "archive the chat")
	unarchive := fs.Bool("unarchive", false, "restore the chat from the archive")
	reason := fs.String("reason", "", "why; recorded and shown in the chat (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	if *archive && *unarchive {
		return errf(1, "--archive and --unarchive cannot be combined")
	}
	var change store.AdminChatChange
	if flagWasSet(fs, "title") {
		change.Title = title
	}
	if flagWasSet(fs, "model") {
		change.Model = model
	}
	if flagWasSet(fs, "approval-mode") {
		change.ApprovalMode = mode
	}
	settings := change.Title != nil || change.Model != nil || change.ApprovalMode != nil
	if !settings && !*archive && !*unarchive {
		return errf(1, "nothing to change: pass --title, --model, --approval-mode, --archive or --unarchive")
	}
	if strings.TrimSpace(*reason) == "" {
		return errf(1, "--reason is required: it is recorded and shown in the chat")
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	actor := cliAdminActor(*as)
	result := map[string]any{}
	if settings {
		after, err := st.AdminUpdateChat(ctx, actor, id, change, *reason)
		if err != nil {
			return errf(1, "set: %v", err)
		}
		result["chat"] = after
		if !*asJSON {
			fmt.Fprintf(out, "updated %q (%s): model %s, approvals %s\n", after.Title, after.ID, after.Model, after.ApprovalMode)
		}
	}
	if *archive || *unarchive {
		// Archiving also revokes a shared chat's participants, which only the
		// running server can do: it is an admin request, carried out on the
		// server's next pass.
		payload := "unarchive"
		if *archive {
			payload = "archive"
		}
		req, err := st.CreateAdminRequest(ctx, store.AdminRequestInput{Actor: actor, Kind: store.AdminRequestArchive, ConversationID: id, Payload: payload, Reason: *reason})
		if err != nil {
			return errf(1, "%s: %v", payload, err)
		}
		result["request"] = req
		if !*asJSON {
			fmt.Fprintf(out, "admin request %s recorded (%s); the server carries it out on its next pass — follow it with: fleet chats request %s\n", req.ID, payload, req.ID)
		}
	}
	if *asJSON {
		return writeJSONOut(out, result)
	}
	return 0
}

func chatsActions(argv []string, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats actions", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	limit := fs.Int("limit", 50, "maximum rows")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if lead == "" && fs.NArg() > 0 {
		lead = fs.Arg(0)
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	ctx := context.Background()
	convID := ""
	if lead != "" {
		c, err := st.AdminGetChat(ctx, lead)
		if err != nil {
			return errf(1, "chat %s: %v", lead, err)
		}
		convID = c.ID
	}
	actions, err := st.ListAdminActions(ctx, convID, *limit)
	if err != nil {
		return errf(1, "actions: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, actions)
	}
	rows := make([][]string, 0, len(actions))
	for _, a := range actions {
		rows = append(rows, []string{fmtTime(a.CreatedAt), a.Actor, a.Via, a.Action, shortScheduleID(a.ConversationID), truncateRunes(oneLine(a.Reason), 50), truncateRunes(oneLine(a.Detail), 50)})
	}
	if err := renderTable(out, []string{"WHEN (UTC)", "ACTOR", "VIA", "ACTION", "CHAT", "REASON", "DETAIL"}, rows); err != nil {
		return errf(1, "%v", err)
	}
	return 0
}
