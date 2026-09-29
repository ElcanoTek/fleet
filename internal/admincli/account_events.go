package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Account events from the operator CLI (docs/ACCOUNT-EVENTS.md). A CLI write
// to a Chat account's membership or roles queues the same event the admin UI
// does (source "cli"); the running `fleet serve` delivers it. The CLI is a
// separate process that usually holds only one of the two databases, so the
// other one is opened from the deployment's env (FLEET_CHAT_DATABASE_URL /
// FLEET_SCHED_DATABASE_URL). When it cannot be, the write still happens and
// the operator is told to run `fleet account-events resync`: an event built
// from half the state would report a wrong role.

// feedState is whether this box publishes account events, as far as this
// process can tell.
type feedState int

const (
	feedOff feedState = iota
	feedOn
	// feedUnknown: the server env file exists but cannot be read (root-owned
	// 0600 on a provisioned box, the CLI run as someone else with
	// --database-url) and the shell names no URL, so the feed may well be on.
	feedUnknown
)

// accountEventsFeed reads FLEET_ACCOUNT_EVENTS_URL from the shell or the
// server env file.
func accountEventsFeed() feedState {
	if strings.TrimSpace(envOrFile("FLEET_ACCOUNT_EVENTS_URL")) != "" {
		return feedOn
	}
	if envFileReadErr() != nil {
		return feedUnknown
	}
	return feedOff
}

// accountEventsConfigured reports whether a write should publish. An unknown
// feed does not — nothing can be queued without knowing the feed is meant to
// deliver it — but it warns, because "off" would then be a silent miss: the
// write happens, no event is queued, and nothing says so.
func accountEventsConfigured() bool {
	switch accountEventsFeed() {
	case feedOn:
		return true
	case feedUnknown:
		warnEnvFileUnreadable.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: cannot read the server env file (%v), so this command cannot tell whether the account-events feed is on; if it is, the change is not published — run `fleet account-events resync` as a user who can read it\n", envFileReadErr())
		})
	case feedOff:
	}
	return false
}

var warnEnvFileUnreadable sync.Once

// openAccountEvents returns a recorder over chat and sched, opening whichever
// the command did not already hold (from otherDSN when the command has a flag
// for it, else the deployment env). It is nil (every call a no-op) when the
// feed is off or a database is unreachable; the returned func closes what this
// helper opened.
func openAccountEvents(chat *store.Store, sched *storage.Storage, otherDSN string) (*accountevents.Recorder, func()) {
	if !accountEventsConfigured() {
		return nil, func() {}
	}
	var closers []func()
	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	if chat == nil {
		dsn, err := chatDSN(otherDSN)
		if err == nil {
			chat, err = store.Open(dsn, store.DefaultPoolConfig())
		}
		if err != nil {
			warnAccountEvent(fmt.Errorf("%w: open chat DB: %w", accountevents.ErrNotQueued, err))
			return nil, closeAll
		}
		closers = append(closers, func() { _ = chat.Close() })
	}
	if sched == nil {
		dsn, err := schedDSN(otherDSN)
		if err == nil {
			sched = storage.New()
			err = sched.Initialize(dsn, storage.DefaultPoolConfig())
		}
		if err != nil {
			warnAccountEvent(fmt.Errorf("%w: open sched DB: %w", accountevents.ErrNotQueued, err))
			return nil, closeAll
		}
		closers = append(closers, func() { _ = sched.Close() })
	}
	return accountevents.NewRecorder(chat, sched, chat), closeAll
}

// commitCLIAccountChange publishes a CLI change and warns (never fails the
// command) when it could not. Deferred right after Begin, so a command that
// fails halfway still reports what the two planes hold.
func commitCLIAccountChange(ctx context.Context, change *accountevents.Change) {
	warnAccountEvent(change.Commit(ctx, store.AccountEventSourceCLI, ""))
}

func warnAccountEvent(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; run `fleet account-events resync` to republish\n", err)
	}
}

// cmdAccountEvents: fleet account-events status|export|resync.
func cmdAccountEvents(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet account-events status|export|resync")
	}
	switch argv[0] {
	case "status":
		return accountEventsStatus(argv[1:])
	case "export":
		return accountEventsExport(argv[1:])
	case "resync":
		return accountEventsResync(argv[1:])
	default:
		return errf(1, "unknown account-events subcommand %q", argv[0])
	}
}

// accountEventsStatus prints the outbox counts. It names no URL (a webhook URL
// can carry a token in its path or query) and never the secret.
func accountEventsStatus(argv []string) int {
	fs := flag.NewFlagSet("account-events status", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	st, code := openChatStore(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	stats, err := st.AccountEventStats(context.Background())
	if err != nil {
		return errf(5, "read account events: %v", err)
	}
	switch accountEventsFeed() {
	case feedOn:
		fmt.Println("feed:      on (FLEET_ACCOUNT_EVENTS_URL is set)")
	case feedUnknown:
		fmt.Printf("feed:      unknown (cannot read the server env file: %v; run as a user who can read it)\n", envFileReadErr())
	default:
		fmt.Println("feed:      off (FLEET_ACCOUNT_EVENTS_URL is unset; nothing is queued)")
	}
	fmt.Printf("pending:   %d\n", stats.Pending)
	if stats.OldestPendingAt > 0 {
		fmt.Printf("oldest:    %s ago\n", time.Since(time.Unix(stats.OldestPendingAt, 0)).Round(time.Second))
	}
	fmt.Printf("failed:    %d (rejected by the receiver, or gave up after 7 days)\n", stats.Failed)
	fmt.Printf("delivered: %d (kept 7 days)\n", stats.Delivered)
	if stats.LastError != "" {
		fmt.Printf("last error: %s\n", stats.LastError)
	}
	return 0
}

// accountEventsExport prints every Chat account's current state as JSON Lines
// (the event body's "user" object). Read-only; works with the feed off, so an
// operator can compare Fleet with a receiver before switching it on.
func accountEventsExport(argv []string) int {
	chat, rec, closeAll, code := snapshotRecorder(argv, "account-events export")
	if rec == nil {
		return code
	}
	defer closeAll()
	enc := json.NewEncoder(os.Stdout)
	err := forEachAccount(chat, func(ctx context.Context, email string) error {
		s, err := rec.Snapshot(ctx, email)
		if err != nil || !s.Exists {
			return err
		}
		return enc.Encode(accountevents.PayloadUser{Email: email, Enabled: s.Enabled, ChatRole: s.ChatRole, OpsRole: s.OpsRole})
	})
	if err != nil {
		return errf(5, "export: %v", err)
	}
	return 0
}

// accountEventsResync queues one "resync" event per Chat account carrying its
// current state, for the running server to deliver — and one user.deleted per
// email the feed knows once had an account that is gone now
// (store.DeletedAccountEmails), so a lost deletion is repaired too: without it
// a receiver that missed the deletion keeps the grant, and an identity
// provider's next push would re-create the account.
func accountEventsResync(argv []string) int {
	switch accountEventsFeed() {
	case feedUnknown:
		// Queued rows are never pruned while pending, so a resync into a feed
		// that is really off would sit in the table for good.
		return errf(1, "cannot tell whether the account-events feed is on (cannot read the server env file: %v); run resync as a user who can read it", envFileReadErr())
	case feedOff:
		return errf(1, "the account-events feed is off: set FLEET_ACCOUNT_EVENTS_URL and FLEET_ACCOUNT_EVENTS_SECRET first")
	case feedOn:
	}
	chat, rec, closeAll, code := snapshotRecorder(argv, "account-events resync")
	if rec == nil {
		return code
	}
	defer closeAll()
	queued := 0
	err := forEachAccount(chat, func(ctx context.Context, email string) error {
		ok, err := rec.Publish(ctx, email, store.AccountEventSourceResync, "")
		if ok && err == nil {
			queued++
		}
		return err
	})
	if err != nil {
		return errf(5, "resync (%d queued before the failure): %v", queued, err)
	}
	ctx := context.Background()
	gone, err := chat.DeletedAccountEmails(ctx)
	if err != nil {
		return errf(5, "resync (%d queued before the failure): list deleted accounts: %v", queued, err)
	}
	deleted := 0
	for _, email := range gone {
		ok, err := rec.PublishDeleted(ctx, email, store.AccountEventSourceResync, "")
		if err != nil {
			return errf(5, "resync (%d queued before the failure): %v", queued+deleted, err)
		}
		if ok {
			deleted++
		}
	}
	fmt.Printf("queued %d account event(s) (%d deletion(s)); the running fleet serve delivers them\n", queued+deleted, deleted)
	return 0
}

// snapshotRecorder opens both databases for export/resync. Unlike the write
// commands these fail when a database is unreachable: their whole output is
// the two-plane state.
func snapshotRecorder(argv []string, name string) (*store.Store, *accountevents.Recorder, func(), int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL)")
	if err := fs.Parse(argv); err != nil {
		return nil, nil, nil, 1
	}
	chat, code := openChatStore(*chatURL)
	if chat == nil {
		return nil, nil, nil, code
	}
	sched, code := openSchedStorage(*schedURL)
	if sched == nil {
		_ = chat.Close()
		return nil, nil, nil, code
	}
	closeAll := func() { _ = sched.Close(); _ = chat.Close() }
	return chat, accountevents.NewRecorder(chat, sched, chat), closeAll, 0
}

func forEachAccount(chat *store.Store, fn func(ctx context.Context, email string) error) error {
	ctx := context.Background()
	accounts, err := chat.ListAccountAccess(ctx)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if err := fn(ctx, a.Email); err != nil {
			return err
		}
	}
	return nil
}

func openChatStore(dbURL string) (*store.Store, int) {
	dsn, err := chatDSN(dbURL)
	if err != nil {
		return nil, errf(1, "%v", err)
	}
	st, err := store.Open(dsn, store.DefaultPoolConfig())
	if err != nil {
		return nil, errf(1, "open chat DB: %v", err)
	}
	return st, 0
}
