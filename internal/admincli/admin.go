package admincli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// cmdAdmin is the one-stop admin-user command. Fleet has TWO independent user
// planes: the chat login DB (email+password web login + chat `/admin` RBAC) and
// the sched / Operations-Center DB (membership resolved by email via the session
// cookie bridge, #458). Making a human a full admin used to mean three steps
// across both planes — `chat user add`, `chat user role admin`, and a sched-side
// grant (or the FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS env). `fleet admin add
// <email>` does all of it in one idempotent command so nobody has to reason about
// the two planes. add/list/rm all operate across BOTH.
func cmdAdmin(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet admin add|list|rm <email>")
	}
	switch argv[0] {
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

// adminAdd provisions (or updates) an email as a full admin across both planes:
// a chat login with role=admin, and an Operations Center admin row keyed by the
// same email so the cookie bridge admits them. Idempotent: re-running updates the
// password and re-asserts admin on both sides. The password is prompted
// interactively (hidden, double-entered) unless --password - reads it from stdin.
func adminAdd(argv []string) int {
	fs := flag.NewFlagSet("admin add", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL)")
	pw := fs.String("password", "", `password ("-" reads from stdin; omit to prompt)`)
	email, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return errf(1, "email required — usage: fleet admin add <email>")
	}

	// Resolve BOTH DSNs before prompting, so a missing env var fails fast rather
	// than after the operator types a password.
	chatDsn, err := chatDSN(*chatURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	if _, err := schedDSN(*schedURL); err != nil {
		return errf(1, "%v", err)
	}

	password, err := resolveSecret(*pw, fmt.Sprintf("password for %s: ", email), true)
	if err != nil {
		return errf(1, "%v", err)
	}
	if len(password) < 8 {
		return errf(1, "password must be at least 8 characters")
	}

	// ── chat plane: web login + chat-admin role ──
	st, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	created := false
	if _, err := st.CreateUser(ctx, email, password); err != nil {
		// CreateUser reports "user <email> already exists" on a duplicate; treat
		// that as an update (reset password) rather than an error, so add is
		// idempotent.
		if strings.Contains(err.Error(), "already exists") {
			if err := st.UpdatePassword(ctx, email, password); err != nil {
				return errf(5, "update chat password: %v", err)
			}
		} else {
			return errf(5, "create chat user: %v", err)
		}
	} else {
		created = true
	}
	adminRole := "admin"
	if _, err := st.SetUserRoleTeam(ctx, email, &adminRole, nil); err != nil {
		return errf(5, "set chat admin role: %v", err)
	}

	// ── sched plane: Operations Center admin (cookie-bridged by email) ──
	sched, code := openSchedStorage(*schedURL)
	if sched == nil {
		return code
	}
	defer sched.Close()
	if err := sched.EnsureAdminUser(ctx, email); err != nil {
		return errf(5, "ensure Operations Center admin: %v", err)
	}

	verb := "updated"
	if created {
		verb = "created"
	}
	fmt.Printf("%s admin %s\n", verb, email)
	fmt.Println("  ✓ chat login (role=admin)")
	fmt.Println("  ✓ Operations Center admin")
	return 0
}

// adminList prints every chat login with its chat role, annotated with whether
// the same email is an Operations Center admin — the two-plane view in one place.
func adminList(argv []string) int {
	fs := flag.NewFlagSet("admin list", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	chatDsn, err := chatDSN(*chatURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	st, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	users, err := st.ListUsers(ctx)
	if err != nil {
		return errf(5, "list chat users: %v", err)
	}

	// Best-effort sched-admin set (by lowercased email/username). A sched DB that
	// is briefly unreachable shouldn't blank the chat listing.
	opsAdmins := map[string]bool{}
	if sched, _ := openSchedStorage(*schedURL); sched != nil {
		defer sched.Close()
		if su, err := sched.ListUsers(ctx); err == nil {
			for _, u := range su {
				if u.Role == "admin" {
					opsAdmins[strings.ToLower(u.Username)] = true
				}
			}
		}
	}

	if *asJSON {
		type row struct {
			Email          string `json:"email"`
			Role           string `json:"role"`
			OpsCenterAdmin bool   `json:"ops_center_admin"`
		}
		rows := make([]row, 0, len(users))
		for _, u := range users {
			rows = append(rows, row{Email: u.Email, Role: string(u.Role), OpsCenterAdmin: opsAdmins[strings.ToLower(u.Email)]})
		}
		return printJSON(rows)
	}
	if len(users) == 0 {
		fmt.Println("no chat users yet — add an admin with: fleet admin add <email>")
		return 0
	}
	// email  chat-role  ops-center — tab-separated so it stays column-able.
	for _, u := range users {
		ops := "-"
		if opsAdmins[strings.ToLower(u.Email)] {
			ops = "ops-center-admin"
		}
		fmt.Printf("%s\t%s\t%s\n", u.Email, u.Role, ops)
	}
	return 0
}

// adminRm removes an email from BOTH planes: its chat login and its Operations
// Center row. Each side is best-effort so a user present in only one plane is
// still cleaned up rather than aborting on the first "not found".
func adminRm(argv []string) int {
	fs := flag.NewFlagSet("admin rm", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL)")
	email, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return errf(1, "email required — usage: fleet admin rm <email>")
	}
	chatDsn, err := chatDSN(*chatURL)
	if err != nil {
		return errf(1, "%v", err)
	}
	if _, err := schedDSN(*schedURL); err != nil {
		return errf(1, "%v", err)
	}
	ctx := context.Background()

	removed := 0
	st, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		return errf(1, "open chat DB: %v", err)
	}
	defer st.Close()
	if err := st.DeleteUser(ctx, email); err != nil {
		fmt.Printf("  chat login: %v\n", err)
	} else {
		fmt.Println("  ✓ removed chat login")
		removed++
	}

	if sched, code := openSchedStorage(*schedURL); sched != nil {
		defer sched.Close()
		if u, err := sched.GetUserByUsername(strings.ToLower(email)); err == nil && u != nil {
			if err := sched.DeleteUser(u.ID); err != nil {
				fmt.Printf("  Operations Center: %v\n", err)
			} else {
				fmt.Println("  ✓ removed Operations Center admin")
				removed++
			}
		} else {
			fmt.Println("  Operations Center: no such member")
		}
	} else {
		return code
	}

	if removed == 0 {
		return errf(2, "%s was not present in either plane", email)
	}
	fmt.Printf("removed admin %s\n", email)
	return 0
}
