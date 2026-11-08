package admincli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdAdmin is the one-stop admin-user command. A person is ONE row of the
// identity table (#2049 M4, ADR-0105): `fleet admin add <email>` makes that row
// a Fleet admin — chat admin and full Operations Center access, one role —
// with a password, in one idempotent command. add/list/rm act on that one row.
// --sched-database-url is still accepted (and ignored) so existing scripts
// keep working.
func cmdAdmin(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet admin add|list|rm <email> | fleet admin merge-report [--json]")
	}
	switch argv[0] {
	case "merge-report":
		return adminMergeReport(argv[1:], os.Stdout)
	case "add":
		return adminAdd(argv[1:])
	case "list", "ls":
		return adminList(argv[1:])
	case "rm", "del", "delete", "remove":
		return adminRm(argv[1:])
	default:
		return errf(1, "unknown admin subcommand %q", argv[0])
	}
}

// adminAdd provisions (or updates) an email as a Fleet admin: a login with
// role=admin, which is also full Operations Center access. Idempotent:
// re-running updates the password and re-asserts admin. The password is
// prompted interactively (hidden, double-entered) unless --password - reads it
// from stdin.
func adminAdd(argv []string) int {
	fs := flag.NewFlagSet("admin add", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default DATABASE_URL, schema chat)")
	_ = fs.String("sched-database-url", "", "ignored: there is one identity table (#2049 M4)")
	pw := fs.String("password", "", `password ("-" reads from stdin; omit to prompt)`)
	email, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return errf(1, "email required — usage: fleet admin add <email>")
	}

	// Resolve the DSN before prompting, so a missing env var fails fast rather
	// than after the operator types a password.
	if _, err := chatDSN(*chatURL); err != nil {
		return errf(1, "%v", err)
	}

	password, err := resolveSecret(*pw, fmt.Sprintf("password for %s: ", email), true)
	if err != nil {
		return errf(1, "%v", err)
	}
	if len(password) < 8 {
		return errf(1, "password must be at least 8 characters")
	}

	st, code := openChatStore(*chatURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ctx := context.Background()

	created := false
	if _, err := st.CreateUser(ctx, email, password); err != nil {
		// A duplicate is an update (reset password) rather than an error, so
		// add is idempotent.
		if errors.Is(err, store.ErrUserExists) {
			if err := st.UpdatePassword(ctx, email, password); err != nil {
				return errf(5, "update password: %v", err)
			}
		} else {
			return errf(5, "create user: %v", err)
		}
	} else {
		created = true
	}
	adminRole := "admin"
	if _, err := st.SetUserRoleTeam(ctx, email, &adminRole, nil); err != nil {
		return errf(5, "set admin role: %v", err)
	}

	verb := "updated"
	if created {
		verb = "created"
	}
	fmt.Printf("%s admin %s\n", verb, email)
	fmt.Println("  ✓ role=admin (chat admin and full Operations Center access)")
	return 0
}

// adminList prints every account with its Fleet role and Operations Center
// access, from the one identity table.
func adminList(argv []string) int {
	fs := flag.NewFlagSet("admin list", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default DATABASE_URL, schema chat)")
	_ = fs.String("sched-database-url", "", "ignored: there is one identity table (#2049 M4)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	// No positional: a stray argument is an error, not silently discarded (the
	// same rule as `sched apikey list`).
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "admin list takes no arguments (got %q)", fs.Args())
	}
	st, code := openChatStore(*chatURL)
	if st == nil {
		return code
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return errf(5, "list users: %v", err)
	}

	if *asJSON {
		type row struct {
			Email          string `json:"email"`
			Role           string `json:"role"`
			OpsScope       string `json:"ops_scope"`
			OpsCenterRole  string `json:"ops_center_role"`
			OpsCenterAdmin bool   `json:"ops_center_admin"`
		}
		rows := make([]row, 0, len(users))
		for _, u := range users {
			rows = append(rows, row{Email: u.Email, Role: u.Role, OpsScope: u.OpsScope, OpsCenterRole: u.LegacyOpsRole(), OpsCenterAdmin: u.Role == store.RoleAdmin})
		}
		return printJSON(rows)
	}
	if len(users) == 0 {
		fmt.Println("no users yet — add an admin with: fleet admin add <email>")
		return 0
	}
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		ops := u.OpsScope
		if u.Role == store.RoleAdmin {
			ops = "admin"
		}
		rows = append(rows, []string{u.Email, u.Role, ops})
	}
	if err := renderTable(os.Stdout, []string{"EMAIL", "ROLE", "OPS_CENTER"}, rows); err != nil {
		return errf(5, "render: %v", err)
	}
	return 0
}

// adminRm removes an account: its login, its Operations Center access and its
// owned data (the store's delete cascade). One row, one delete.
func adminRm(argv []string) int {
	fs := flag.NewFlagSet("admin rm", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default DATABASE_URL, schema chat)")
	_ = fs.String("sched-database-url", "", "ignored: there is one identity table (#2049 M4)")
	email, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return errf(1, "email required — usage: fleet admin rm <email>")
	}
	st, code := openChatStore(*chatURL)
	if st == nil {
		return code
	}
	defer st.Close()
	if err := st.DeleteUser(context.Background(), email); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return errf(2, "%s is not an account", email)
		}
		return errf(5, "remove %s: %v", email, err)
	}
	fmt.Printf("removed admin %s (login, Operations Center access and owned data)\n", email)
	return 0
}

// adminMergeReport prints what the one-time identity merge (#2049 M4,
// identity.MergeLegacySchedUsers) recorded in core.identity_merge: the
// Operations Center admins it demoted (split admin states fail closed), the
// sched rows with an unknown role, the case-duplicate usernames, and the
// rest. Read-only: it opens the database without migrating anything.
// `fleet doctor` reads --json and advises on what needs an operator.
func adminMergeReport(argv []string, w io.Writer) int {
	fs := flag.NewFlagSet("admin merge-report", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default DATABASE_URL, schema chat)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "admin merge-report takes no arguments (got %q)", fs.Args())
	}
	dsn, err := chatDSN(*chatURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errf(1, "open database: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := identity.LastMergeReport(ctx, db)
	if err != nil {
		return errf(5, "read the identity merge report: %v", err)
	}
	if *asJSON {
		out := struct {
			Ran    bool                  `json:"ran"`
			Report *identity.MergeReport `json:"report,omitempty"`
		}{Ran: rep != nil, Report: rep}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return errf(5, "%v", err)
		}
		return 0
	}
	if rep == nil {
		fmt.Fprintln(w, "identity merge: not run on this database yet (the scheduler merges its accounts on its first start after the chat store migrated)")
		return 0
	}
	fmt.Fprintf(w, "identity merge ran %s\n", rep.At.UTC().Format(time.RFC3339))
	for _, line := range rep.Lines() {
		fmt.Fprintln(w, line)
	}
	return 0
}
