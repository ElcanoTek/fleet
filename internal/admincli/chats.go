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
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdChats implements `fleet chats` — chat administration from the host
// (ADR-0115; the same powers as Settings -> Chats in the browser):
//
//	fleet chats list    [--user EMAIL] [--search TEXT] [--state S | --attention] [--active-days N] [--archived | --deleted] [--limit N] [--json]
//	fleet chats people  [--user EMAIL] [--search TEXT] [--active-days N] [--archived] [--json]
//	fleet chats show    <chat_id> [--json]
//	fleet chats history <chat_id> [--tail N] [--before ID] [--tools] [--json]
//	fleet chats note    <chat_id> [--text TEXT | --file PATH|- | --clear] [--json]
//	fleet chats send    <chat_id> (--message TEXT | --file PATH|-) --reason TEXT [--wait] [--timeout 10m]
//	fleet chats stop    <chat_id> --reason TEXT [--wait]
//	fleet chats transfer <chat_id> --to EMAIL --reason TEXT [--wait]
//	fleet chats delete  <chat_id> --reason TEXT [--permanent --yes] [--wait]
//	fleet chats restore <chat_id> --reason TEXT [--wait]
//	fleet chats notice  <chat_id> --text TEXT [--json]
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
// <unix user>@<host>) and announced in the chat — except a pinned note
// (ADR-0117), which is recorded and shown above the conversation instead of
// being written into it.
func cmdChats(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, chatsUsage)
		return 1
	}
	out, errW := os.Stdout, os.Stderr
	switch argv[0] {
	case "list", "ls":
		return chatsList(argv[1:], out)
	case "people", "who":
		return chatsPeople(argv[1:], out)
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
	case "delete", "rm":
		return chatsRequest(store.AdminRequestDelete, argv[1:], os.Stdin, out, errW)
	case "restore", "undelete":
		return chatsRequest(store.AdminRequestRestore, argv[1:], os.Stdin, out, errW)
	case "note":
		return chatsNote(argv[1:], os.Stdin, out)
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

  fleet chats list     [--user EMAIL] [--search TEXT] [--state attention|running|patrols|noted|archived]
                       [--attention] [--active-days N] [--archived | --deleted] [--limit N] [--json]
  fleet chats people   [--user EMAIL] [--search TEXT] [--active-days N] [--archived] [--json]
                                                          (who needs help: each owner's chats, counted)
  fleet chats show     <chat_id> [--json]                 (owner, model, approvals, attention, pinned note, waiting
                                                           cards, patrols, queue, recent admin changes)
  fleet chats history  <chat_id> [--tail 30] [--before ID] [--tools] [--json]
  fleet chats note     <chat_id> [--json]                 (show the note pinned to the chat)
  fleet chats note     <chat_id> (--text TEXT | --file PATH|-) [--json]   (pin or replace it)
  fleet chats note     <chat_id> --clear [--json]         (remove it)
  fleet chats send     <chat_id> (--message TEXT | --file PATH|-) --reason TEXT [--wait] [--timeout 10m]
  fleet chats stop     <chat_id> --reason TEXT [--wait]   (the owner's Stop: running turn + queued inputs)
  fleet chats transfer <chat_id> --to EMAIL --reason TEXT [--wait]
  fleet chats delete   <chat_id> --reason TEXT [--wait]   (to the recycle bin: restorable for 30 days)
  fleet chats delete   <chat_id> --permanent --yes --reason TEXT [--wait]   (emergency: deleted at once, files reclaimed)
  fleet chats restore  <chat_id> --reason TEXT [--wait]   (out of the recycle bin; its patrols stay paused)
  fleet chats notice   <chat_id> --text TEXT [--json]     (a UI-only line in the chat; the model never sees it)
  fleet chats set      <chat_id> [--title T] [--model SLUG] [--approval-mode manual] [--archive|--unarchive] --reason TEXT
  fleet chats actions  [<chat_id>] [--limit 50] [--json]  (the record of admin changes)
  fleet chats request  <request_id> [--json]              (an admin request's progress)
  fleet chats patrol   list|show|set-prompt|set-schedule|pause|resume  (same as fleet patrol)

Every change is recorded as --as (default <unix user>@<host>) and noted
in the chat. An admin message runs as the chat's owner, with the owner's connectors, labelled
as yours; it always runs in Manual, so anything that needs approval asks the owner. send, stop and
transfer are carried out by the running server on its next scheduler pass (30 s by default).

A chat needs attention (--attention, --state attention) when an approval card is waiting or its
latest turn ended in an error. Answering someone else's approval cards is not an administrator action.
--search matches the title, the owner's email or the id. A pinned note is shown to the chat's owner
and Multiplayer participants above the conversation; it is not part of the chat's history, so the
model never reads it. A chat carries one note: replacing it marks it unread.
`

// chatsStore is what `fleet chats` needs from the store.
type chatsStore interface {
	AdminListChats(ctx context.Context, f store.AdminChatFilter) ([]store.AdminChat, error)
	AdminChatOverview(ctx context.Context, f store.AdminChatFilter) (store.AdminChatOverview, error)
	AdminListPendingCards(ctx context.Context, convID string) ([]store.AdminPendingCard, error)
	GetAdminNote(ctx context.Context, convID string) (*store.AdminNote, error)
	AdminSetNote(ctx context.Context, a store.AdminActor, convID, text string) (store.AdminNote, error)
	AdminClearNote(ctx context.Context, a store.AdminActor, convID string) (bool, error)
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
	search := fs.String("search", "", "title, owner's email or id contains")
	state := fs.String("state", "", "only chats in one state: attention, running, patrols, noted or archived")
	attention := fs.Bool("attention", false, "only chats that need attention (same as --state attention)")
	days := fs.Int("active-days", 0, "only chats active in the last N days")
	archived := fs.Bool("archived", false, "include archived chats")
	deleted := fs.Bool("deleted", false, "list the recycle bin (deleted chats not yet purged)")
	limit := fs.Int("limit", 50, "maximum rows")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "unexpected argument %q", fs.Arg(0))
	}
	chatState := strings.ToLower(strings.TrimSpace(*state))
	if *attention {
		if chatState != "" && chatState != store.AdminChatStateAttention {
			return errf(1, "--attention means --state attention: it cannot be combined with --state %s", chatState)
		}
		chatState = store.AdminChatStateAttention
	}
	if !store.ValidAdminChatState(chatState) {
		return errf(1, "unknown --state %q: %v", *state, store.ErrAdminChatState)
	}
	st, closeFn, code := openChatsStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeFn()
	chats, err := st.AdminListChats(context.Background(), store.AdminChatFilter{
		OwnerEmail: *owner, Search: *search, State: chatState, ActiveWithinDays: *days, IncludeArchived: *archived, Deleted: *deleted, Limit: *limit,
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
		if len(c.Attention) > 0 {
			flags = append(flags, "attention:"+strings.Join(c.Attention, "+"))
		}
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
		switch {
		case c.NoteSeen:
			flags = append(flags, "note:seen")
		case c.HasNote:
			flags = append(flags, "note")
		}
		if c.Archived {
			flags = append(flags, "archived")
		}
		if c.Lockdown {
			flags = append(flags, "lockdown")
		}
		if c.PurgesAt != nil {
			flags = append(flags, "deleted; purged "+fmtTime(*c.PurgesAt))
		}
		rows = append(rows, []string{shortScheduleID(c.ID), truncateRunes(c.Title, 44), c.OwnerEmail, c.ApprovalMode, fmtTime(c.UpdatedAt), strings.Join(flags, " ")})
	}
	if err := renderTable(out, []string{"ID", "TITLE", "OWNER", "APPROVALS", "UPDATED (UTC)", "STATE"}, rows); err != nil {
		return errf(1, "%v", err)
	}
	return 0
}

// chatsPeople implements `fleet chats people`: the chats counted per owner,
// those with chats needing attention first — the answer to "who needs help?".
func chatsPeople(argv []string, out io.Writer) int {
	fs := flag.NewFlagSet("chats people", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	owner := fs.String("user", "", "only this owner")
	search := fs.String("search", "", "title, owner's email or id contains")
	days := fs.Int("active-days", 0, "only chats active in the last N days")
	archived := fs.Bool("archived", false, "count archived chats too")
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
	o, err := st.AdminChatOverview(context.Background(), store.AdminChatFilter{
		OwnerEmail: *owner, Search: *search, ActiveWithinDays: *days, IncludeArchived: *archived,
	})
	if err != nil {
		return errf(1, "people: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, o)
	}
	rows := make([][]string, 0, len(o.People))
	for _, p := range o.People {
		rows = append(rows, []string{
			p.Email, fmt.Sprint(p.Chats), fmt.Sprint(p.Attention), fmt.Sprint(p.CardsWaiting),
			fmt.Sprint(p.Running), fmt.Sprint(p.Patrols), fmt.Sprint(p.Noted), fmtTime(p.LastActive),
		})
	}
	if err := renderTable(out, []string{"PERSON", "CHATS", "ATTENTION", "CARDS", "RUNNING", "PATROLS", "NOTES", "LAST ACTIVE (UTC)"}, rows); err != nil {
		return errf(1, "%v", err)
	}
	fmt.Fprintf(out, "\n%d chat(s): %d need attention, %d approval card(s) waiting, %d running, %d with patrols, %d with a note; %d archived, %d in the recycle bin\n",
		o.Chats, o.Attention, o.CardsWaiting, o.Running, o.Patrols, o.Noted, o.Archived, o.Deleted)
	if o.PeopleTruncated {
		fmt.Fprintf(out, "only the first %d people are listed (the totals cover everyone); narrow with --search, --user or --active-days\n", len(o.People))
	}
	return 0
}

// attentionText says why a chat needs attention, in words (the
// store.AdminAttention* reasons).
func attentionText(c store.AdminChat) string {
	parts := make([]string, 0, len(c.Attention))
	for _, r := range c.Attention {
		switch r {
		case store.AdminAttentionCardsWaiting:
			parts = append(parts, "an approval card is waiting since "+fmtTime(c.WaitingSince)+" UTC")
		case store.AdminAttentionTurnFailed:
			parts = append(parts, "the latest turn ended in an error")
		default:
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "; ")
}

// noteSeenText says whether anyone in the chat has pressed "Got it" on the
// note's current text.
func noteSeenText(n store.AdminNote) string {
	if n.SeenAt == nil {
		return "not seen yet"
	}
	if n.SeenBy == "" {
		return "seen " + fmtTime(*n.SeenAt) + " UTC"
	}
	return "seen by " + n.SeenBy + " " + fmtTime(*n.SeenAt) + " UTC"
}

// printAdminNote prints a pinned note: who wrote it, whether it was seen, and
// its text indented under that line.
func printAdminNote(out io.Writer, n store.AdminNote) {
	fmt.Fprintf(out, "pinned note (by %s via %s, %s UTC; %s):\n", n.Author, n.Via, fmtTime(n.UpdatedAt), noteSeenText(n))
	for line := range strings.SplitSeq(n.Text, "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}
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
	note, err := st.GetAdminNote(ctx, c.ID)
	if err != nil {
		return errf(1, "note: %v", err)
	}
	cards, err := st.AdminListPendingCards(ctx, c.ID)
	if err != nil {
		return errf(1, "approval cards: %v", err)
	}
	if *asJSON {
		for i := range patrols {
			patrols[i].Prompt = ""
		}
		return writeJSONOut(out, map[string]any{"chat": c, "patrols": patrols, "queued_inputs": len(queue), "actions": actions, "note": note, "pending_cards": cards})
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
	if len(c.Attention) > 0 {
		fmt.Fprintf(out, "attention:   %s\n", attentionText(c))
	}
	if note != nil {
		fmt.Fprintln(out)
		printAdminNote(out, *note)
	}
	if len(cards) > 0 {
		// Seeing a card is all an administrator does with it: answering stays
		// with the chat's people (ADR-0115).
		fmt.Fprintln(out, "\nwaiting approval cards (only the chat's people can answer them):")
		for _, k := range cards {
			tool := k.Tool
			if k.MCPServer != "" {
				tool = k.MCPServer + "/" + k.Tool
			}
			expires := "does not expire"
			if k.ExpiresAt > 0 {
				expires = "expires " + fmtTime(k.ExpiresAt) + " UTC"
			}
			fmt.Fprintf(out, "  %-28s for %s, waiting since %s UTC, %s\n", truncateRunes(tool, 28), k.ForEmail, fmtTime(k.CreatedAt), expires)
		}
		if more := c.PendingApprovals - len(cards); more > 0 {
			// The listing is bounded; the count is not.
			fmt.Fprintf(out, "  these are the %d that have waited longest; %d more are waiting\n", len(cards), more)
		}
	}
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
	before := fs.Int64("before", 0, "page back: only entries older than this entry id")
	tools := fs.Bool("tools", false, "include tool calls and results")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	if *before < 0 {
		return errf(1, "--before must be an entry id (see the ids in fleet chats history --json)")
	}
	// The store's own bounds (default 30, at most 500), applied here too so a
	// full page can be told from the last one.
	if *tail <= 0 {
		*tail = 30
	}
	if *tail > 500 {
		*tail = 500
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
	entries, err := st.AdminChatTranscript(ctx, c.ID, store.AdminTranscriptOptions{Tail: *tail, IncludeTools: *tools, BeforeID: *before})
	if err != nil {
		return errf(1, "history: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, entries)
	}
	which := "last"
	if *before > 0 {
		which = fmt.Sprintf("before entry %d,", *before)
	}
	fmt.Fprintf(out, "%s — %s (owner %s), %s %d entries\n\n", c.Title, c.ID, c.OwnerEmail, which, len(entries))
	for _, e := range entries {
		text := strings.TrimSpace(e.Text)
		for _, img := range e.Images {
			text = strings.TrimSpace(text + "\n[image: " + img + "]")
		}
		fmt.Fprintf(out, "[%s] %s\n%s\n\n", fmtTime(e.CreatedAt), transcriptLabel(e, c.OwnerEmail), text)
	}
	if len(entries) > 0 && len(entries) == *tail {
		// A full page: there may be older entries (the hint is also printed
		// when the page happens to end exactly at the chat's first entry).
		// The hint continues the same reading: the same entry set and page size.
		hint := fmt.Sprintf("fleet chats history %s --before %d", c.ID, entries[0].ID)
		if *tools {
			hint += " --tools"
		}
		if *tail != 30 {
			hint += fmt.Sprintf(" --tail %d", *tail)
		}
		fmt.Fprintf(out, "older entries: %s\n", hint)
	}
	return 0
}

func transcriptLabel(e store.AdminTranscriptEntry, owner string) string {
	switch {
	case e.Type == "notice":
		return "notice (" + e.Kind + ")"
	case e.Type == agent.EntryTypeSummaryBoundary:
		return "earlier messages were summarized here"
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
	var permanent, yes *bool
	switch kind {
	case store.AdminRequestDelete:
		permanent = fs.Bool("permanent", false, "delete at once instead of to the recycle bin (an emergency, e.g. a full disk; cannot be undone)")
		yes = fs.Bool("yes", false, "confirm a --permanent delete")
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
	if kind == store.AdminRequestDelete && *permanent {
		if !*yes {
			return errf(1, "--permanent cannot be undone: add --yes to confirm (or drop --permanent to use the recycle bin)")
		}
		kind = store.AdminRequestPurge
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
	asJSON := fs.Bool("json", false, "JSON output")
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
	if *asJSON {
		return writeJSONOut(out, map[string]any{"ok": true, "chat_id": c.ID})
	}
	fmt.Fprintf(out, "notice written to %q (%s)\n", c.Title, c.ID)
	return 0
}

// chatsNote implements `fleet chats note`: show, pin (or replace) or clear
// the note an administrator pins to a chat (ADR-0117). The chat's people see
// it above the conversation; it is not written into the chat's history, so
// the model never reads it.
func chatsNote(argv []string, stdin io.Reader, out io.Writer) int {
	lead, argv := splitLeadingID(argv)
	fs := flag.NewFlagSet("chats note", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	as := fs.String("as", "", "record the change as this administrator (default <unix user>@<host>)")
	text := fs.String("text", "", "pin this note, replacing any note already there")
	file := fs.String("file", "", "read the note from a file (- for stdin)")
	clearNote := fs.Bool("clear", false, "remove the pinned note")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	id, err := patrolIDArg(fs, lead)
	if err != nil {
		return errf(1, "%v", err)
	}
	hasText, hasFile := flagWasSet(fs, "text"), flagWasSet(fs, "file")
	given := 0
	for _, on := range []bool{hasText, hasFile, *clearNote} {
		if on {
			given++
		}
	}
	if given > 1 {
		return errf(1, "give one of --text, --file or --clear")
	}
	body := *text
	if hasFile {
		b, err := readPromptFile(*file, stdin)
		if err != nil {
			return errf(1, "%v", err)
		}
		body = b
	}
	if hasText || hasFile {
		// The store checks these too; refusing here saves opening it.
		body = strings.TrimSpace(body)
		if body == "" {
			return errf(1, "%v", store.ErrAdminNoteEmpty)
		}
		if utf8.RuneCountInString(body) > store.MaxAdminNoteRunes {
			return errf(1, "%v", store.ErrAdminNoteTooLong)
		}
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
	switch {
	case *clearNote:
		cleared, err := st.AdminClearNote(ctx, cliAdminActor(*as), c.ID)
		if err != nil {
			return errf(1, "clear note: %v", err)
		}
		if *asJSON {
			return writeJSONOut(out, map[string]any{"cleared": cleared})
		}
		if !cleared {
			fmt.Fprintf(out, "no note was pinned to %q (%s)\n", c.Title, c.ID)
			return 0
		}
		fmt.Fprintf(out, "note removed from %q (%s)\n", c.Title, c.ID)
		return 0
	case hasText || hasFile:
		n, err := st.AdminSetNote(ctx, cliAdminActor(*as), c.ID, body)
		if err != nil {
			return errf(1, "note: %v", err)
		}
		if *asJSON {
			return writeJSONOut(out, map[string]any{"note": n})
		}
		fmt.Fprintf(out, "note pinned to %q (%s): the chat's people see it above the conversation; the assistant never reads it\n", c.Title, c.ID)
		return 0
	}
	n, err := st.GetAdminNote(ctx, c.ID)
	if err != nil {
		return errf(1, "note: %v", err)
	}
	if *asJSON {
		return writeJSONOut(out, map[string]any{"note": n})
	}
	if n == nil {
		fmt.Fprintf(out, "no note is pinned to %q (%s)\n", c.Title, c.ID)
		return 0
	}
	fmt.Fprintf(out, "%s — %s (owner %s)\n", c.Title, c.ID, c.OwnerEmail)
	printAdminNote(out, *n)
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
