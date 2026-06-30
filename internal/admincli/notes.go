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
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
)

// cmdNotes dispatches `fleet-admin notes ...` — the admin notes wiki. The CLI
// runs in-process with the sched pool (no HTTP hop). Body content arrives on
// stdin (CI-friendly, matches the mcp-account / create-user convention).
//
// Exit codes: 0 ok · 1 usage · 2 not-found · 3 conflict · 5 operational.
func cmdNotes(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin notes set|get|list|rm | notes proposal publish|reject")
	}
	switch argv[0] {
	case "set":
		return notesSet(argv[1:])
	case "get":
		return notesGet(argv[1:])
	case "list", "ls":
		return notesList(argv[1:])
	case "rm", "del", "delete", "archive":
		return notesRm(argv[1:])
	case "proposal", "proposals":
		return notesProposal(argv[1:])
	default:
		return errf(1, "unknown notes subcommand %q", argv[0])
	}
}

// openNotesStore opens the sched DB and returns the notes store plus a closer
// the caller must defer (the underlying *db.Database owns the connection pool;
// sched.Store keeps only its conn handle, so it cannot close it itself).
func openNotesStore(dbURL string) (*sched.Store, func(), int) {
	dsn, err := schedDSN(dbURL)
	if err != nil {
		return nil, nil, errf(1, "%v", err)
	}
	database := scheddb.New()
	if err := database.Init(dsn, scheddb.DefaultPoolConfig()); err != nil {
		return nil, nil, errf(1, "open sched DB: %v", err)
	}
	return sched.NewStore(database), func() { _ = database.Close() }, 0
}

func notesSet(argv []string) int {
	fs := flag.NewFlagSet("notes set", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	title := fs.String("title", "", "note title")
	by := fs.String("by", "admin", "created_by / updated_by identity")
	slug, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(slug) == "" {
		return errf(1, "slug required")
	}
	body, err := readStdinValue()
	if err != nil {
		return errf(5, "%v", err)
	}
	if strings.TrimSpace(body) == "" {
		return errf(1, "note body required on stdin")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	ctx := context.Background()

	// Upsert: create if the slug is new, else update title/body (bumps version).
	existing, gerr := st.GetNoteBySlug(ctx, slug)
	switch {
	case errors.Is(gerr, sched.ErrNoteNotFound):
		t := *title
		if t == "" {
			t = slug
		}
		if _, err := st.CreateNote(ctx, slug, t, body, *by); err != nil {
			return notesErr(err)
		}
		fmt.Printf("created note %q\n", slug)
		return 0
	case gerr != nil:
		return errf(5, "%v", gerr)
	default:
		var tp *string
		if *title != "" {
			tp = title
		}
		if _, err := st.UpdateNote(ctx, existing.ID, tp, &body, *by); err != nil {
			return notesErr(err)
		}
		fmt.Printf("updated note %q\n", slug)
		return 0
	}
}

func notesGet(argv []string) int {
	fs := flag.NewFlagSet("notes get", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	slug, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(slug) == "" {
		return errf(1, "slug required")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	note, err := st.GetNoteBySlug(context.Background(), slug)
	if errors.Is(err, sched.ErrNoteNotFound) {
		return errf(2, "note %q not found", slug)
	}
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Print(note.Body)
	if !strings.HasSuffix(note.Body, "\n") {
		fmt.Println()
	}
	return 0
}

func notesList(argv []string) int {
	fs := flag.NewFlagSet("notes list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	all := fs.Bool("all", false, "include archived notes")
	_, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	notes, err := st.ListNotes(context.Background(), *all)
	if err != nil {
		return errf(5, "%v", err)
	}
	if len(notes) == 0 {
		fmt.Fprintln(os.Stderr, "(no notes)")
		return 0
	}
	for _, n := range notes {
		fmt.Printf("%s\tv%d\t%s\t%s\n", n.Slug, n.Version, n.Status, n.Title)
	}
	return 0
}

func notesRm(argv []string) int {
	fs := flag.NewFlagSet("notes rm", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	by := fs.String("by", "admin", "updated_by identity")
	slug, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(slug) == "" {
		return errf(1, "slug required")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	ctx := context.Background()
	note, err := st.GetNoteBySlug(ctx, slug)
	if errors.Is(err, sched.ErrNoteNotFound) {
		return errf(2, "note %q not found", slug)
	}
	if err != nil {
		return errf(5, "%v", err)
	}
	if err := st.ArchiveNote(ctx, note.ID, *by); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("archived note %q\n", slug)
	return 0
}

func notesProposal(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin notes proposal publish|reject <id>")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "publish":
		return notesProposalPublish(rest)
	case "reject":
		return notesProposalReject(rest)
	default:
		return errf(1, "unknown notes proposal subcommand %q", sub)
	}
}

func notesProposalPublish(argv []string) int {
	fs := flag.NewFlagSet("notes proposal publish", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	note := fs.String("note", "", "decision note")
	by := fs.String("by", "admin", "decided_by identity")
	idStr, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return errf(1, "invalid proposal id")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	n, err := st.PublishProposal(context.Background(), id, *by, *note)
	if errors.Is(err, sched.ErrNoteNotFound) {
		return errf(2, "proposal not found")
	}
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("published proposal %s -> note %q (v%d)\n", id, n.Slug, n.Version)
	return 0
}

func notesProposalReject(argv []string) int {
	fs := flag.NewFlagSet("notes proposal reject", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	reason := fs.String("reason", "", "rejection reason (required)")
	by := fs.String("by", "admin", "decided_by identity")
	idStr, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(*reason) == "" {
		return errf(1, "--reason is required")
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return errf(1, "invalid proposal id")
	}
	st, closeStore, code := openNotesStore(*dbURL)
	if st == nil {
		return code
	}
	defer closeStore()
	if err := st.RejectProposal(context.Background(), id, *by, *reason); err != nil {
		if errors.Is(err, sched.ErrNoteNotFound) {
			return errf(2, "pending proposal not found")
		}
		return errf(5, "%v", err)
	}
	fmt.Printf("rejected proposal %s\n", id)
	return 0
}

func notesErr(err error) int {
	switch {
	case errors.Is(err, sched.ErrSlugConflict):
		return errf(3, "%v", err)
	case errors.Is(err, sched.ErrInvalidSlug), errors.Is(err, sched.ErrInvalidBody):
		return errf(1, "%v", err)
	case errors.Is(err, sched.ErrNoteNotFound):
		return errf(2, "%v", err)
	default:
		return errf(5, "%v", err)
	}
}
