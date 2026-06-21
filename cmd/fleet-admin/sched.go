package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// cmdSched dispatches `fleet-admin sched user|apikey ...`.
func cmdSched(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched user|apikey ...")
	}
	switch argv[0] {
	case "user":
		return cmdSchedUser(argv[1:])
	case "apikey", "key":
		return cmdSchedAPIKey(argv[1:])
	default:
		return errf(1, "unknown sched subcommand %q", argv[0])
	}
}

func openSchedStorage(dbURL string) (*storage.Storage, int) {
	dsn, err := schedDSN(dbURL)
	if err != nil {
		return nil, errf(1, "%v", err)
	}
	st := storage.New()
	if err := st.Initialize(dsn); err != nil {
		return nil, errf(1, "open sched DB: %v", err)
	}
	return st, 0
}

const validRolesMsg = "role must be one of: admin, client, readonly"

func validRole(role string) bool {
	switch role {
	case "admin", "client", "readonly":
		return true
	}
	return false
}

func cmdSchedUser(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched user add|update|set-role|rename|del|list")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "add":
		return schedUserAdd(rest)
	case "update", "passwd", "password":
		return schedUserUpdatePassword(rest)
	case "set-role":
		return schedUserSetRole(rest)
	case "rename":
		return schedUserRename(rest)
	case "del", "delete", "rm":
		return schedUserDel(rest)
	case "list", "ls":
		return schedUserList(rest)
	default:
		return errf(1, "unknown sched user subcommand %q", sub)
	}
}

func schedUserAdd(argv []string) int {
	fs := flag.NewFlagSet("sched user add", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	role := fs.String("role", "client", "role: admin|client|readonly")
	scopes := fs.String("scopes", "", "comma-separated scopes (optional)")
	pw := fs.String("password", "", `password ("-" reads from stdin)`)
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return errf(1, "username must be 3-64 characters")
	}
	if !validRole(*role) {
		return errf(1, validRolesMsg)
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

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	if existing, _ := st.GetUserByUsername(username); existing != nil {
		return errf(3, "username %q already exists", username)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errf(5, "hash password: %v", err)
	}
	user := &models.User{
		ID:           uuid.New(),
		Username:     username,
		PasswordHash: string(hash),
		Role:         *role,
		Scopes:       parseScopes(*scopes),
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := st.AddUser(user); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("created sched user %s (role=%s)\n", username, *role)
	return 0
}

func schedUserUpdatePassword(argv []string) int {
	fs := flag.NewFlagSet("sched user update", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	pw := fs.String("password", "", `password ("-" reads from stdin)`)
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
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
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	user, err := st.GetUserByUsername(username)
	if err != nil || user == nil {
		return errf(2, "user %q not found", username)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return errf(5, "hash password: %v", err)
	}
	user.PasswordHash = string(hash)
	if _, err := st.AddUser(user); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("updated password for sched user %s\n", username)
	return 0
}

func schedUserSetRole(argv []string) int {
	fs := flag.NewFlagSet("sched user set-role", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	role := fs.String("role", "", "role: admin|client|readonly")
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if !validRole(*role) {
		return errf(1, validRolesMsg)
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	user, err := st.GetUserByUsername(username)
	if err != nil || user == nil {
		return errf(2, "user %q not found", username)
	}
	if err := st.UpdateUserRole(user.ID, *role); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("set role of sched user %s to %s\n", username, *role)
	return 0
}

func schedUserRename(argv []string) int {
	fs := flag.NewFlagSet("sched user rename", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	positional, flagArgs := splitTwoPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if len(positional) < 2 {
		return errf(1, "usage: sched user rename <username> <new-username>")
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	user, err := st.GetUserByUsername(positional[0])
	if err != nil || user == nil {
		return errf(2, "user %q not found", positional[0])
	}
	if err := st.RenameUser(user.ID, positional[1]); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("renamed sched user %s -> %s\n", positional[0], positional[1])
	return 0
}

func schedUserDel(argv []string) int {
	fs := flag.NewFlagSet("sched user del", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	user, err := st.GetUserByUsername(username)
	if err != nil || user == nil {
		return errf(2, "user %q not found", username)
	}
	if err := st.DeleteUser(user.ID); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("deleted sched user %s\n", username)
	return 0
}

func schedUserList(argv []string) int {
	fs := flag.NewFlagSet("sched user list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	_, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return errf(5, "%v", err)
	}
	if len(users) == 0 {
		fmt.Fprintln(os.Stderr, "no sched users yet — add one with: fleet-admin sched user add <username> --role admin --password -")
		return 0
	}
	for _, u := range users {
		fmt.Printf("%s\t%s\n", u.Username, u.Role)
	}
	return 0
}

// ── sched apikey ──

func cmdSchedAPIKey(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched apikey create|list|revoke|delete")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "create":
		return schedAPIKeyCreate(rest)
	case "list", "ls":
		return schedAPIKeyList(rest)
	case "revoke":
		return schedAPIKeyRevoke(rest)
	case "delete", "del", "rm":
		return schedAPIKeyDelete(rest)
	default:
		return errf(1, "unknown sched apikey subcommand %q", sub)
	}
}

func openKeyManager() (*apikeys.Manager, int) {
	dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("FLEET_DATA_DIR"))
	}
	if dataDir == "" {
		dataDir = "./data"
	}
	mgr, err := apikeys.NewManager(dataDir+"/api_keys.json", "")
	if err != nil {
		return nil, errf(1, "apikeys manager: %v", err)
	}
	return mgr, 0
}

func schedAPIKeyCreate(argv []string) int {
	fs := flag.NewFlagSet("sched apikey create", flag.ContinueOnError)
	role := fs.String("role", "admin", "role granted to the key")
	rateLimit := fs.Int("rate-limit", 0, "per-minute rate limit (0 = unlimited)")
	name, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(name) == "" {
		return errf(1, "key name required")
	}
	mgr, code := openKeyManager()
	if mgr == nil {
		return code
	}
	roleVal := *role
	key, raw, err := mgr.CreateKey(name, nil, nil, &roleVal, *rateLimit, nil, "created via fleet-admin")
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("created API key %s (id=%s role=%s)\n", key.Name, key.KeyID, roleVal)
	fmt.Printf("secret (shown once): %s\n", raw)
	return 0
}

func schedAPIKeyList(argv []string) int {
	mgr, code := openKeyManager()
	if mgr == nil {
		return code
	}
	keys := mgr.ListKeys()
	if len(keys) == 0 {
		fmt.Fprintln(os.Stderr, "(no API keys)")
		return 0
	}
	for _, k := range keys {
		fmt.Printf("%s\t%s\tenabled=%v\n", k.KeyID, k.Name, k.Enabled)
	}
	return 0
}

func schedAPIKeyRevoke(argv []string) int {
	keyID, _ := splitPositional(argv)
	if keyID == "" {
		return errf(1, "key id required")
	}
	mgr, code := openKeyManager()
	if mgr == nil {
		return code
	}
	if err := mgr.RevokeKey(keyID); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("revoked API key %s\n", keyID)
	return 0
}

func schedAPIKeyDelete(argv []string) int {
	keyID, _ := splitPositional(argv)
	if keyID == "" {
		return errf(1, "key id required")
	}
	mgr, code := openKeyManager()
	if mgr == nil {
		return code
	}
	if err := mgr.DeleteKey(keyID); err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("deleted API key %s\n", keyID)
	return 0
}

func parseScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitTwoPositional lifts the first TWO positional args out of argv.
func splitTwoPositional(argv []string) (positional []string, flagArgs []string) {
	for _, a := range argv {
		if len(a) > 0 && a[0] == '-' && len(positional) >= 2 {
			flagArgs = append(flagArgs, a)
		} else if len(a) > 0 && a[0] == '-' {
			flagArgs = append(flagArgs, a)
		} else if len(positional) < 2 {
			positional = append(positional, a)
		} else {
			flagArgs = append(flagArgs, a)
		}
	}
	return positional, flagArgs
}
