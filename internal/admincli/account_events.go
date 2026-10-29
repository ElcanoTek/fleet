package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"os"

	"github.com/ElcanoTek/fleet/internal/store"
)

// `fleet account-events export` prints every Chat account's membership as
// JSON Lines. It is what an identity provider imports Fleet's existing teams
// and roles from (central Auth's `auth app import-teams`, which
// production-support's register-auth-apps.sh feeds from this command), so it
// keeps its name although Fleet no longer publishes an account-events feed
// (ADR-0096).

// exportedAccount is one export line. The field names are the import's
// contract; do not rename them.
type exportedAccount struct {
	Email    string `json:"email"`
	Enabled  bool   `json:"enabled"`
	ChatRole string `json:"chat_role"`
	OpsRole  string `json:"ops_role"`
	// Team is the account's team ("" = none).
	Team string `json:"team"`
}

// cmdAccountEvents: fleet account-events export.
func cmdAccountEvents(argv []string) int {
	if len(argv) < 1 || argv[0] != "export" {
		return errf(1, "usage: fleet account-events export")
	}
	return accountEventsExport(argv[1:])
}

// accountEventsExport prints each Chat account with its Chat role and team
// and its effective Operations Center role ("none" without an enabled Ops
// identity). Read-only. It needs both databases, so an unreachable one fails
// the command: half the state would report a wrong role.
func accountEventsExport(argv []string) int {
	fs := flag.NewFlagSet("account-events export", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	chat, code := openChatStore(*chatURL)
	if chat == nil {
		return code
	}
	defer chat.Close()
	sched, code := openSchedStorage(*schedURL)
	if sched == nil {
		return code
	}
	defer sched.Close()

	ctx := context.Background()
	accounts, err := chat.ListAccountAccess(ctx)
	if err != nil {
		return errf(5, "export: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, a := range accounts {
		opsRole, err := sched.OpsRole(ctx, a.Email)
		if err != nil {
			return errf(5, "export: read the ops role of %s: %v", a.Email, err)
		}
		if err := enc.Encode(exportedAccount{Email: a.Email, Enabled: a.Enabled, ChatRole: a.Role, OpsRole: opsRole, Team: a.Team}); err != nil {
			return errf(5, "export: %v", err)
		}
	}
	return 0
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
