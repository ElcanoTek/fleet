package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdChat dispatches `fleet-admin chat user ...` (interactive chat users — email
// + bcrypt, store-backed). Mirrors chat-admin's semantics.
func cmdChat(argv []string) int {
	if len(argv) < 2 || argv[0] != "user" {
		return errf(1, "usage: fleet-admin chat user add|update|del|list")
	}
	sub := argv[1]
	rest := argv[2:]
	switch sub {
	case "add":
		return chatUserUpsert(rest, true)
	case "update", "passwd", "password":
		return chatUserUpsert(rest, false)
	case "del", "delete", "rm":
		return chatUserDel(rest)
	case "list", "ls":
		return chatUserList(rest)
	default:
		return errf(1, "unknown chat user subcommand %q", sub)
	}
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
	if password == "" {
		return errf(1, "password required (use --password -)")
	}

	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := store.Open(dsn)
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
	st, err := store.Open(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	if err := st.DeleteUser(context.Background(), email); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("deleted chat user %s\n", email)
	return 0
}

func chatUserList(argv []string) int {
	fs := flag.NewFlagSet("chat user list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "chat Postgres DSN")
	_, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	dsn, err := chatDSN(*dbURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := store.Open(dsn)
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return errf(5, "%v", err)
	}
	if len(users) == 0 {
		fmt.Fprintln(os.Stderr, "no chat users yet — add one with: fleet-admin chat user add <email> --password -")
		return 0
	}
	for _, u := range users {
		fmt.Println(u.Email)
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
