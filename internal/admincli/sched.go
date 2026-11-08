package admincli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// cmdSched dispatches `fleet sched user|apikey|task|trigger|dlq|budget ...`.
func cmdSched(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet sched user|apikey|task|trigger|dlq|budget ...")
	}
	switch argv[0] {
	case "user":
		return cmdSchedUser(argv[1:])
	case "apikey", "key":
		return cmdSchedAPIKey(argv[1:])
	case "task":
		return cmdSchedTask(argv[1:])
	case "trigger":
		return cmdSchedTrigger(argv[1:])
	case "dlq":
		return cmdSchedDLQ(argv[1:])
	case "budget":
		return cmdSchedBudget(argv[1:])
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
	if err := st.Initialize(dsn, storage.DefaultPoolConfig()); err != nil {
		return nil, errf(1, "open sched DB: %v", err)
	}
	return st, 0
}

// errIdentityNotReady is what an identity-dependent sched verb says when the
// one identity table does not exist yet: the chat store creates core.users
// (chat migration 099) and the scheduler's open merges its legacy accounts
// into it (#2049 M4). A sched-only verb on a fresh database, or mid-upgrade
// before the chat store migrated, gets here. The CLI does not migrate the
// chat store itself: a newer chat schema under a still-running older server
// is the operator's call, not a side effect of `fleet sched user`.
const errIdentityNotReady = "the identity table (core.users) does not exist yet: the chat store creates it. " +
	"Start `fleet serve` once against this database (or run a chat verb such as `fleet chat user list`), then retry"

// openSchedIdentityStorage is openSchedStorage for the verbs that read or
// write accounts (`fleet sched user …`, a bundle import with Operations
// Center users): it refuses up front, with what to do, when the identity
// table is not there yet, instead of failing later on a missing relation.
func openSchedIdentityStorage(dbURL string) (*storage.Storage, int) {
	st, code := openSchedStorage(dbURL)
	if st == nil {
		return nil, code
	}
	if !st.DB().IdentityMerge().Ready {
		_ = st.Close()
		return nil, errf(1, "%s", errIdentityNotReady)
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

// schedUserNotice is printed by every `fleet sched user` verb. Operations
// Center access is part of the one account (#2049 M4, ADR-0105): these verbs
// are aliases over the identity table, kept so scripts keep working.
const schedUserNotice = "note: `fleet sched user` is deprecated. Operations Center access is part of the one Fleet account: " +
	"manage it in Settings → Admin → Users, or with `fleet chat user` / `fleet admin` (docs/IDENTITY.md)."

func cmdSchedUser(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet sched user add|update|set-role|del|list")
	}
	sub := argv[0]
	rest := argv[1:]
	fmt.Fprintln(os.Stderr, schedUserNotice)
	switch sub {
	case "add":
		return schedUserAdd(rest)
	case "update", "passwd", "password":
		return schedUserUpdatePassword(rest)
	case "set-role":
		return schedUserSetRole(rest)
	case "rename":
		// The account key is the email, which about 30 chat tables carry as a
		// plain column: renaming it is not a one-row write any more.
		return errf(1, "sched user rename is no longer supported: an account is keyed by its email. Add the new address as an account instead")
	case "del", "delete", "rm":
		return schedUserDel(rest)
	case "list", "ls":
		return schedUserList(rest)
	default:
		return errf(1, "unknown sched user subcommand %q", sub)
	}
}

// schedUserAdd creates an account with Operations Center access: role viewer
// (read-only chat) plus the scope the legacy role maps to; --role admin makes
// a Fleet admin. The username is the account key (lowercased).
func schedUserAdd(argv []string) int {
	fs := flag.NewFlagSet("sched user add", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	role := fs.String("role", "client", "role: admin|client|readonly")
	pw := fs.String("password", "", `password ("-" reads from stdin)`)
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	username = identity.NormalizeEmail(username)
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

	st, code := openSchedIdentityStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	hash, err := identity.HashPassword(password, 0)
	if err != nil {
		return errf(5, "hash password: %v", err)
	}
	scope, admin, _ := identity.ParseOpsRole(*role)
	acct := identity.NewAccount{Email: username, PasswordHash: hash, Role: identity.RoleViewer, OpsScope: scope}
	if admin {
		acct.Role, acct.OpsScope = identity.RoleAdmin, identity.OpsNone
	}
	ctx := context.Background()
	if _, err := st.Identity().CreateAccount(ctx, acct); err != nil {
		if !errors.Is(err, identity.ErrExists) {
			return errf(5, "%v", err)
		}
		// The email is already an account (a chat user): before the one
		// identity table this added a separate Operations Center user, so
		// grant that account the access instead. Its password is not changed.
		u, err := st.Identity().AddOpsAccess(ctx, username, *role)
		if errors.Is(err, identity.ErrExists) {
			return errf(3, "username %q already has Operations Center access, or is disabled (change its access with: fleet sched user set-role %s --role <role>)", username, username)
		}
		if err != nil {
			return errf(5, "%v", err)
		}
		fmt.Printf("granted existing account %s Operations Center role %s (chat role %s; its password is unchanged)\n", username, *role, u.Role)
		return 0
	}
	fmt.Printf("created user %s (role=%s; chat role %s)\n", username, *role, acct.Role)
	return 0
}

// schedUserUpdatePassword sets the account's one Fleet password (chat and the
// Operations Center share it). The account's password sessions end.
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
	st, code := openSchedIdentityStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	hash, err := identity.HashPassword(password, 0)
	if err != nil {
		return errf(5, "hash password: %v", err)
	}
	if err := st.Identity().SetPassword(context.Background(), username, hash); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return errf(2, "user %q not found", username)
		}
		return errf(5, "%v", err)
	}
	fmt.Printf("updated the Fleet password of %s (chat and the Operations Center share it)\n", identity.NormalizeEmail(username))
	return 0
}

// schedUserSetRole sets the account's Operations Center access: admin makes
// it a Fleet admin; client/readonly set the scope of a non-admin.
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
	st, code := openSchedIdentityStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	if _, err := st.Identity().SetOpsAccess(context.Background(), username, *role); err != nil {
		switch {
		case errors.Is(err, identity.ErrNotFound):
			return errf(2, "user %q not found", username)
		case identity.IsInputError(err):
			return errf(1, "%v", err)
		}
		return errf(5, "%v", err)
	}
	fmt.Printf("set Operations Center role of %s to %s\n", identity.NormalizeEmail(username), *role)
	return 0
}

// schedUserDel ends the account's Operations Center access (scope none). The
// account itself — its chat login and data — stays: removing a person is
// `fleet chat user del` / `fleet admin rm`. An admin is refused: their access
// is the admin role.
func schedUserDel(argv []string) int {
	fs := flag.NewFlagSet("sched user del", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	username, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	st, code := openSchedIdentityStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.GetIdentity(ctx, username)
	if err != nil || identity.LegacyOpsRole(u.Role, u.OpsScope) == "" {
		return errf(2, "user %q not found", username)
	}
	if _, err := st.Identity().SetOpsAccess(ctx, username, identity.LegacyOpsNone); err != nil {
		if identity.IsInputError(err) {
			return errf(1, "%v (remove an admin with: fleet admin rm %s)", err, u.Email)
		}
		return errf(5, "%v", err)
	}
	fmt.Printf("removed Operations Center access of %s (the account itself stays; remove it with: fleet chat user del %s)\n", u.Email, u.Email)
	return 0
}

func schedUserList(argv []string) int {
	fs := flag.NewFlagSet("sched user list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	asJSON := fs.Bool("json", false, "machine-readable output")
	// No positional: a stray argument is an error, not silently discarded (the
	// same rule as `sched apikey list`).
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "sched user list takes no arguments (got %q)", fs.Args())
	}
	st, code := openSchedIdentityStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		return errf(5, "%v", err)
	}
	if *asJSON {
		type row struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		rows := make([]row, 0, len(users))
		for _, u := range users {
			rows = append(rows, row{Username: u.Username, Role: u.Role})
		}
		return printJSON(rows)
	}
	if len(users) == 0 {
		fmt.Fprintln(os.Stderr, "no Operations Center users yet — add one with: fleet admin add <email>")
		return 0
	}
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		rows = append(rows, []string{u.Username, u.Role})
	}
	if err := renderTable(os.Stdout, []string{"USERNAME", "ROLE"}, rows); err != nil {
		return errf(5, "render: %v", err)
	}
	return 0
}

// ── sched apikey ──

func cmdSchedAPIKey(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet sched apikey create|list|revoke|rotate|delete")
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
	case "rotate":
		return schedAPIKeyRotate(rest)
	case "delete", "del", "rm":
		return schedAPIKeyDelete(rest)
	default:
		return errf(1, "unknown sched apikey subcommand %q", sub)
	}
}

// keyFormatNote is the one line every key-minting surface prints: operators
// handed a key to look for need to know its shape, and the two families look
// nothing alike.
const keyFormatNote = "typed keys look like fleet_<type>_<base58> (fleet_task_…, fleet_readonly_…, fleet_webhook_…, fleet_admin_…); legacy --role keys are sk-<base64>. Send either as the X-API-Key header (the /v1 API and A2A accept nothing else)."

// openKeyManager opens the store `fleet sched apikey` operates on — the
// service's own store when it can be derived (see keyStore), announced on
// stderr so a mismatch is never silent. The returned keyStore lets mutating
// verbs hand the written files back to the store's owner (fixOwnership).
func openKeyManager() (*apikeys.Manager, keyStore, int) {
	ks := resolveKeyStore()
	ks.announce()
	mgr, err := apikeys.NewManager(ks.Path(), "")
	if err != nil {
		return nil, ks, errf(1, "apikeys manager (%s): %v", ks.Path(), err)
	}
	return mgr, ks, 0
}

// apiKeyCreateFlags is the parsed `sched apikey create` flag surface, built by
// newAPIKeyCreateFlagSet so the rate-limit alias handling is unit-testable.
type apiKeyCreateFlags struct {
	keyType, triggerSlugs, role string
	rateLimit                   int
	// legacyRateLimit is the deprecated --rate-limit spelling (#722: the
	// primary flag names its unit because v1's numbers were per HOUR).
	legacyRateLimit int
}

func newAPIKeyCreateFlagSet() (*flag.FlagSet, *apiKeyCreateFlags) {
	fs := flag.NewFlagSet("sched apikey create", flag.ContinueOnError)
	f := &apiKeyCreateFlags{}
	fs.StringVar(&f.keyType, "type", "", "typed key class: admin|task|webhook|readonly (#190). When set, supersedes --role")
	fs.StringVar(&f.triggerSlugs, "trigger-slugs", "", "comma-separated trigger slugs a webhook key may fire (required for --type webhook)")
	fs.StringVar(&f.role, "role", "admin", "legacy role granted to the key (used only when --type is empty)")
	// #722: the flag names its unit so an operator porting v1 numbers (which
	// were per-HOUR) can't silently set a 60× stricter cap.
	fs.IntVar(&f.rateLimit, "rate-limit-per-minute", 0, "per-minute request rate limit (0 = unlimited)")
	fs.IntVar(&f.legacyRateLimit, "rate-limit", 0, "DEPRECATED alias for --rate-limit-per-minute (same per-MINUTE unit; warns)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleet sched apikey create <name> [--type admin|task|webhook|readonly] [--rate-limit-per-minute N] [--trigger-slugs a,b] [--role admin]")
		fmt.Fprintln(fs.Output(), "  "+keyFormatNote)
		fmt.Fprintln(fs.Output(), "  The key is written to the store the fleet.service process reads (announced as \"key store:\"); FLEET_DATA_DIR overrides.")
		fs.PrintDefaults()
	}
	return fs, f
}

// effectiveRateLimit resolves the per-minute cap from the primary flag and the
// deprecated --rate-limit alias (docs/FEATURE-NOTES.md #722 promises the alias
// "remains and warns"). The alias only fills in when the primary was not given;
// passing both is a usage error rather than a silent pick. The warning goes to
// errW so a scripted create still gets clean stdout.
func effectiveRateLimit(fs *flag.FlagSet, f *apiKeyCreateFlags, errW io.Writer) (int, error) {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	switch {
	case set["rate-limit"] && set["rate-limit-per-minute"]:
		return 0, fmt.Errorf("pass --rate-limit-per-minute or its deprecated alias --rate-limit, not both")
	case set["rate-limit"]:
		fmt.Fprintf(errW, "warning: --rate-limit is deprecated — use --rate-limit-per-minute %d (the unit is per MINUTE; v1's flag was per hour)\n", f.legacyRateLimit)
		return f.legacyRateLimit, nil
	default:
		return f.rateLimit, nil
	}
}

func schedAPIKeyCreate(argv []string) int {
	fs, f := newAPIKeyCreateFlagSet()
	name, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(name) == "" {
		return errf(1, "key name required")
	}
	rateLimitPerMinute, err := effectiveRateLimit(fs, f, os.Stderr)
	if err != nil {
		return errf(1, "%v", err)
	}
	keyType, triggerSlugs, role := &f.keyType, &f.triggerSlugs, &f.role
	rateLimit := &rateLimitPerMinute
	mgr, ks, code := openKeyManager()
	if mgr == nil {
		return code
	}

	// Typed-key path (#190): the type determines the permission set.
	if t := strings.TrimSpace(*keyType); t != "" {
		kt := apikeys.KeyType(t)
		if !kt.Valid() {
			return errf(1, "unknown key type %q (want admin|task|webhook|readonly)", t)
		}
		slugs := parseCSVList(*triggerSlugs)
		if kt == apikeys.KeyTypeWebhook {
			if len(slugs) == 0 {
				return errf(1, "--type webhook requires --trigger-slugs")
			}
			// Validate slug shape (matching the HTTP path) so a webhook key can't
			// be minted scoped to a slug the trigger endpoint would never accept.
			for _, s := range slugs {
				if !slugPattern.MatchString(s) {
					return errf(1, "invalid trigger slug %q (want [a-z0-9][a-z0-9_-]{0,127})", s)
				}
			}
		}
		key, raw, err := mgr.CreateTypedKey(context.Background(), name, kt, slugs, *rateLimit, nil, "created via the fleet CLI")
		if err != nil {
			return errf(5, "%v", err)
		}
		ks.fixOwnership()
		fmt.Printf("created API key %s (id=%s type=%s)\n", key.Name, key.KeyID, kt)
		if kt == apikeys.KeyTypeWebhook {
			fmt.Printf("trigger slugs: %s\n", strings.Join(slugs, ", "))
		}
		fmt.Printf("secret (shown once): %s\n", raw)
		fmt.Printf("format: fleet_%s_<base58> — send it as the X-API-Key header; a free read-only check of the key is POST /v1/tasks/estimate (creates nothing).\n", kt)
		return 0
	}

	// Legacy role-based path: mints an untyped sk- key, unchanged.
	roleVal := *role
	key, raw, err := mgr.CreateKey(context.Background(), name, nil, &roleVal, *rateLimit, nil, "created via the fleet CLI")
	if err != nil {
		return errf(5, "%v", err)
	}
	ks.fixOwnership()
	fmt.Printf("created API key %s (id=%s role=%s)\n", key.Name, key.KeyID, roleVal)
	fmt.Printf("secret (shown once): %s\n", raw)
	fmt.Println("format: legacy sk-<base64> — send it as the X-API-Key header (prefer --type task|readonly|webhook|admin for a scoped fleet_<type>_… key).")
	return 0
}

func schedAPIKeyList(argv []string) int {
	fs := flag.NewFlagSet("sched apikey list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	// Parse argv in full (this verb takes no positional), so a typo'd flag or a
	// stray argument is an error rather than silently ignored — it used to
	// discard argv entirely, so `apikey list --josn` printed the list and 0.
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "sched apikey list takes no arguments (got %q)", fs.Args())
	}
	mgr, _, code := openKeyManager()
	if mgr == nil {
		return code
	}
	keys := mgr.ListKeys()
	if *asJSON {
		type row struct {
			KeyID   string `json:"key_id"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Enabled bool   `json:"enabled"`
		}
		rows := make([]row, 0, len(keys))
		for _, k := range keys {
			typeStr := string(k.Type)
			if typeStr == "" {
				typeStr = "legacy"
			}
			// Never the raw key — the same fields as the text listing.
			rows = append(rows, row{KeyID: k.KeyID, Name: k.Name, Type: typeStr, Enabled: k.Enabled})
		}
		return printJSON(rows)
	}
	if len(keys) == 0 {
		fmt.Fprintln(os.Stderr, "(no API keys)")
		return 0
	}
	for _, k := range keys {
		typeStr := string(k.Type)
		if typeStr == "" {
			typeStr = "legacy"
		}
		// Never print the raw key — only its stable ID, name, type and state.
		fmt.Printf("%s\t%s\ttype=%s\tenabled=%v\n", k.KeyID, k.Name, typeStr, k.Enabled)
	}
	return 0
}

func schedAPIKeyRevoke(argv []string) int {
	keyID, rest := splitPositional(argv)
	if keyID == "" {
		return errf(1, "key id required")
	}
	if len(rest) > 0 {
		return errf(1, "sched apikey revoke takes only <key-id> (unexpected: %q)", rest)
	}
	mgr, ks, code := openKeyManager()
	if mgr == nil {
		return code
	}
	if err := mgr.RevokeKey(context.Background(), keyID); err != nil {
		return errf(5, "%v", err)
	}
	ks.fixOwnership()
	fmt.Printf("revoked API key %s\n", keyID)
	return 0
}

func schedAPIKeyRotate(argv []string) int {
	fs := flag.NewFlagSet("sched apikey rotate", flag.ContinueOnError)
	graceHours := fs.Int("grace-hours", 24, "hours the previous key stays valid during rotation")
	keyID, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(keyID) == "" {
		return errf(1, "key id required")
	}
	mgr, ks, code := openKeyManager()
	if mgr == nil {
		return code
	}
	// RotateKey preserves the key's type (#190); a typed key rotates to a fresh
	// token of the same type, so its scope is unchanged.
	key, raw, err := mgr.RotateKey(context.Background(), keyID, *graceHours)
	if err != nil {
		return errf(5, "%v", err)
	}
	ks.fixOwnership()
	typeStr := string(key.Type)
	if typeStr == "" {
		typeStr = "legacy"
	}
	fmt.Printf("rotated API key %s (type=%s, previous key valid for %dh)\n", key.KeyID, typeStr, *graceHours)
	fmt.Printf("secret (shown once): %s\n", raw)
	return 0
}

func schedAPIKeyDelete(argv []string) int {
	keyID, rest := splitPositional(argv)
	if keyID == "" {
		return errf(1, "key id required")
	}
	if len(rest) > 0 {
		return errf(1, "sched apikey delete takes only <key-id> (unexpected: %q)", rest)
	}
	mgr, ks, code := openKeyManager()
	if mgr == nil {
		return code
	}
	if err := mgr.DeleteKey(context.Background(), keyID); err != nil {
		return errf(5, "%v", err)
	}
	ks.fixOwnership()
	fmt.Printf("deleted API key %s\n", keyID)
	return 0
}

// splitTwoPositional lifts the first TWO positional args out of argv.
func splitTwoPositional(argv []string) (positional []string, flagArgs []string) {
	for _, a := range argv {
		switch {
		case len(a) > 0 && a[0] == '-':
			flagArgs = append(flagArgs, a)
		case len(positional) < 2:
			positional = append(positional, a)
		default:
			flagArgs = append(flagArgs, a)
		}
	}
	return positional, flagArgs
}

// parseCSVList splits a comma-separated flag value into trimmed, non-empty
// items (nil when the flag is unset).
func parseCSVList(raw string) []string {
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

// printJSON writes v to stdout as indented JSON — the one encoder every
// `--json` list verb shares, so machine callers get the same shape rules
// everywhere (an empty list is `[]`, never null or a "(none)" line on stderr).
func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return errf(5, "encode json: %v", err)
	}
	return 0
}
