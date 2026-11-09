package admincli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/chattui"
	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdChat is overloaded by design (#457):
//   - `fleet chat`                  → the interactive TUI to chat with the agent
//   - `fleet chat --message "…"`    → a one-shot non-interactive turn
//   - `fleet chat user add|…`       → chat-USER administration (email + bcrypt)
//   - `fleet chat set-model …`      → bulk-update conversation models
//
// The TUI is the common case, so anything that isn't the `user` or `set-model`
// admin verb routes to chattui.Run. (They never collide: `user` and `set-model`
// are bare positionals, while the TUI takes only flags/none.)
func cmdChat(argv []string) int {
	if len(argv) == 0 {
		return chattui.Run(argv)
	}
	if argv[0] == "set-model" {
		return chatSetModel(argv[1:])
	}
	if argv[0] != "user" {
		return chattui.Run(argv)
	}
	if len(argv) < 2 {
		return errf(1, "usage: fleet chat user add|update|role|del|list | fleet chat set-model --model <slug>  (or `fleet chat` for the agent TUI)")
	}
	sub := argv[1]
	rest := argv[2:]
	switch sub {
	case "add":
		return chatUserUpsert(rest, true)
	case "update", "passwd", "password":
		return chatUserUpsert(rest, false)
	case "role", "set-role":
		return chatUserRole(rest)
	case "del", "delete", "rm":
		return chatUserDel(rest)
	case "list", "ls":
		return chatUserList(rest)
	default:
		return errf(1, "unknown chat user subcommand %q", sub)
	}
}

type chatModelStore interface {
	ListConversationsForModelUpdate(ctx context.Context, fromModel string) ([]store.ConversationModelEntry, error)
	BulkUpdateConversationModel(ctx context.Context, model, fromModel string) (int, error)
}

// chatSetModel implements `fleet chat set-model`:
//
//	fleet chat set-model --model <slug> [--from-model <slug>] [--dry-run] [--no-confirm]
func chatSetModel(argv []string) int {
	fs := flag.NewFlagSet("chat set-model", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	model := fs.String("model", "", "the model slug to set (required)")
	fromModel := fs.String("from-model", "", "only re-assign conversations currently pinned to this slug")
	dryRun := fs.Bool("dry-run", false, "print the conversations that would change without writing")
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive confirmation (for scripted bulk updates)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if strings.TrimSpace(*model) == "" {
		return errf(1, "--model is required")
	}
	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	return runChatSetModel(context.Background(), st, os.Stdout, os.Stderr, strings.TrimSpace(*model), strings.TrimSpace(*fromModel), *dryRun, *noConfirm)
}

func runChatSetModel(ctx context.Context, st chatModelStore, outW, errW io.Writer, model, fromModel string, dryRun, noConfirm bool) int {
	if dryRun {
		convs, err := st.ListConversationsForModelUpdate(ctx, fromModel)
		if err != nil {
			fmt.Fprintf(errW, "list conversations: %v\n", err)
			return 5
		}
		for _, c := range convs {
			cur := c.Model
			if cur == "" {
				cur = "<none>"
			}
			fmt.Fprintf(outW, "%s\t%s\tmodel %s → %s\n", c.ID, c.UserEmail, cur, model)
		}
		fmt.Fprintf(errW, "dry-run: %d conversation(s) would be re-assigned\n", len(convs))
		return 0
	}

	scope := "all non-deleted conversations"
	if fromModel != "" {
		scope = "conversations currently pinned to " + fromModel
	}
	prompt := fmt.Sprintf("Set model=%s on %s? [y/N]: ", model, scope)
	if code := confirmBulkMutation(noConfirm, prompt); code != 0 {
		return code
	}

	updated, err := st.BulkUpdateConversationModel(ctx, model, fromModel)
	if err != nil {
		fmt.Fprintf(errW, "re-assign conversation model: %v\n", err)
		return 5
	}
	fmt.Fprintf(errW, "re-assigned model on %d conversation(s)\n", updated)
	return 0
}

// chatUserRole assigns an RBAC role and/or team to an existing chat user (#237):
//
//	fleet chat user role <email> --role viewer
//	fleet chat user role <email> --team growth
//	fleet chat user role <email> --role admin --team growth
//	fleet chat user role <email> --team ""        # clear the team
//
// A flag left unset leaves that column untouched (partial PATCH), matching
// store.SetUserRoleTeam. At least one of --role/--team must be provided.
func chatUserRole(argv []string) int {
	fs := flag.NewFlagSet("chat user role", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	role := fs.String("role", "", "role: member|viewer|admin")
	team := fs.String("team", "", `team id (empty string clears the team)`)
	email, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if email == "" {
		return errf(1, "email required")
	}
	// Only forward flags the operator actually set, so an omitted flag is a
	// no-op rather than an empty write. fs.Visit reports only set flags.
	var rolePtr, teamPtr *string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "role":
			rolePtr = role
		case "team":
			teamPtr = team
		}
	})
	if rolePtr == nil && teamPtr == nil {
		return errf(1, "provide --role and/or --team")
	}

	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	u, err := st.SetUserRoleTeam(ctx, email, rolePtr, teamPtr)
	if err != nil {
		return errf(5, "%v", err)
	}
	teamLabel := u.TeamID
	if teamLabel == "" {
		teamLabel = "(none)"
	}
	fmt.Printf("updated %s: role=%s team=%s\n", u.Email, u.Role, teamLabel)
	return 0
}

// chatUserUpsert handles add (create) and update (password change). The password
// is read from stdin when --password is "-" (never on argv).
func chatUserUpsert(argv []string, create bool) int {
	fs := flag.NewFlagSet("chat user", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	pw := fs.String("password", "", `password ("-" reads from stdin)`)
	email, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if email == "" {
		return errf(1, "email required")
	}
	password := *pw
	if password == "-" {
		v, err := readStdinValue()
		if err != nil {
			return errf(5, "%v", err)
		}
		password = v
	}
	if len(password) < 8 {
		return errf(1, "password must be at least 8 characters (use --password -)")
	}

	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if create {
		if _, err := st.CreateUser(ctx, email, password); err != nil {
			return errf(5, "%v", err)
		}
		fmt.Printf("created chat user %s\n", email)
		return 0
	}
	if err := st.UpdatePassword(ctx, email, password); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("updated password for chat user %s\n", email)
	return 0
}

func chatUserDel(argv []string) int {
	fs := flag.NewFlagSet("chat user del", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	email, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if email == "" {
		return errf(1, "email required")
	}
	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.DeleteUser(ctx, email); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("deleted chat user %s\n", email)
	return 0
}

func chatUserList(argv []string) int {
	fs := flag.NewFlagSet("chat user list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	asJSON := fs.Bool("json", false, "machine-readable output")
	// No positional: a stray argument is an error, not silently discarded (the
	// same rule as `sched apikey list`).
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "chat user list takes no arguments (got %q)", fs.Args())
	}
	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := openChatStoreDSN(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return errf(5, "%v", err)
	}
	if *asJSON {
		type row struct {
			Email string `json:"email"`
			Role  string `json:"role"`
			Team  string `json:"team,omitempty"`
		}
		rows := make([]row, 0, len(users))
		for _, u := range users {
			rows = append(rows, row{Email: u.Email, Role: u.Role, Team: u.TeamID})
		}
		return printJSON(rows)
	}
	if len(users) == 0 {
		fmt.Fprintln(os.Stderr, "no chat users yet — add one with: fleet chat user add <email> --password -")
		return 0
	}
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		team := u.TeamID
		if team == "" {
			team = "-"
		}
		rows = append(rows, []string{u.Email, u.Role, team})
	}
	if err := renderTable(os.Stdout, []string{"EMAIL", "ROLE", "TEAM"}, rows); err != nil {
		return errf(5, "render: %v", err)
	}
	return 0
}

// splitPositional separates the FIRST positional argument from the flag tokens.
// Go's flag package stops at the first non-flag arg, so the email/slug
// positional must be lifted out before Parse. Multiple positionals are returned
// joined back into the flag list (callers that need >1 positional use a
// dedicated splitter).
func splitPositional(argv []string) (first string, flagArgs []string) {
	for i, a := range argv {
		if len(a) > 0 && a[0] != '-' {
			first = a
			flagArgs = append(flagArgs, argv[:i]...)
			flagArgs = append(flagArgs, argv[i+1:]...)
			return first, flagArgs
		}
	}
	return "", argv
}
