// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/pgtext"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/store"
)

// errKeyStore is deleteAccount's refusal when the API-key store cannot be
// opened: nothing has been deleted.
var errKeyStore = errors.New("cannot open the API-key store")

// errServerRunning is deleteAccount's refusal while a fleet server runs
// against the database (store.ErrServerRunning): nothing has been deleted.
// The server's own deletion stops the account's requests and turns, forgets
// its in-memory decisions and moves its files aside under its request gate —
// none of which a CLI process can do for a server it is not.
var errServerRunning = errors.New("the fleet server is running against this database, so nothing was deleted")

// deleteAccount is what `fleet admin rm` and `fleet chat user del` run once
// the operator has confirmed (confirmAccountDeletion): the same
// accountdelete.Delete as Settings → Admin → Users, limited to the account
// expectedID when the operator was shown one, pointed at the key
// store and attachment directory the fleet.service process uses. The key
// store is opened before anything is deleted, so an unreadable one stops the
// command with nothing changed (errKeyStore). It refuses while a fleet
// server runs against the database (errServerRunning), and holds a lock
// meanwhile that keeps a server from starting until it is done
// (store.ExcludeServers). What could not be removed after the database
// deletion is printed as a warning. Any other error is the database's, and
// nothing was deleted.
func deleteAccount(ctx context.Context, st *store.Store, email, expectedID string) (accountdelete.Result, error) {
	release, err := st.ExcludeServers(ctx)
	if errors.Is(err, store.ErrServerRunning) {
		return accountdelete.Result{}, serverRunningError()
	}
	if err != nil {
		return accountdelete.Result{}, err
	}
	defer release()
	ks := resolveKeyStore()
	ks.announce()
	mgr, err := apikeys.NewManager(ks.Path(), "")
	if err != nil {
		return accountdelete.Result{}, fmt.Errorf("%w (%s): %w", errKeyStore, ks.Path(), err)
	}
	dir, source := resolveAttachmentDir()
	fmt.Fprintf(os.Stderr, "attachment directory: %s (%s)\n", dir, source)
	res, err := accountdelete.Delete(ctx, st, email, accountdelete.Options{Keys: mgr, AttachmentDir: dir, ExpectedID: expectedID})
	if err != nil {
		return res, err
	}
	// Always: opening the store may have created its lock file, even when no
	// key was deleted.
	ks.fixOwnership()
	for _, p := range res.Problems {
		fmt.Fprintf(os.Stderr, "warning: %s\n", p.String())
	}
	return res, nil
}

// serverRunningError is errServerRunning with what to do instead.
func serverRunningError() error {
	return fmt.Errorf("%w: delete the account from Settings → Admin → Users while the server runs, or stop the server (systemctl stop %s) and run this again", errServerRunning, serviceName(""))
}

// accountDeletionPreflight is what confirmAccountDeletion reads before it
// asks: *store.Store.
type accountDeletionPreflight interface {
	ServerHeartbeatSeen(ctx context.Context) (bool, error)
	DeleteUserPreflight(ctx context.Context, email string) error
	AccountDeletionSummary(ctx context.Context, email string) (store.AccountDeletionSummary, error)
}

// runAccountDeletion is `fleet admin rm` and `fleet chat user del` once
// their flags are parsed and the store is open (action: "remove" /
// "delete"): ask (confirmAccountDeletion), delete (deleteAccount) the
// account the operator confirmed, and report.
func runAccountDeletion(ctx context.Context, st *store.Store, action, email string, noConfirm bool, c confirmIO) int {
	expectedID, code := confirmAccountDeletion(ctx, st, action, email, noConfirm, c)
	if code != 0 {
		return code
	}
	res, err := deleteAccount(ctx, st, email, expectedID)
	if err != nil {
		return accountDeletionErrorCode(action, email, err)
	}
	fmt.Println(accountDeletedLine(pgtext.EmailKey(email), res))
	return deletedExitCode(res)
}

// accountDeletionErrorCode prints a refused or failed account deletion —
// from the confirmation's preflight or from deleteAccount — and returns its
// exit code: 2 for no such account, 1 for a refusal (nothing was deleted),
// 5 for anything else. A deletion that did not commit and also left the
// account's files to settle (held until a maintenance pass, or not moved
// back or removed: accountdelete wraps the refusal with that) is printed
// whole with 5, so what is left is never hidden behind the refusal's short
// message.
func accountDeletionErrorCode(action, email string, err error) int {
	var shared *store.OwnsSharedProjectsError
	var unconfirmed *accountdelete.OutcomeUnconfirmedError
	var multi interface{ Unwrap() []error }
	switch {
	case errors.Is(err, errKeyStore):
		return errf(1, "%s %s: %v — nothing was deleted", action, email, err)
	case errors.Is(err, errServerRunning):
		return errf(1, "%s %s: %v", action, email, err)
	case errors.As(err, &unconfirmed), errors.As(err, &multi):
		return errf(5, "%s %s: %v", action, email, err)
	case errors.Is(err, store.ErrUserNotFound):
		return errf(2, "%s is not an account%s", email, wrappedDetail(err, store.ErrUserNotFound))
	case errors.As(err, &shared):
		return errf(1, "%s %s: %v — transfer or unshare them first; nothing was deleted", action, email, err)
	case errors.Is(err, accountdelete.ErrNotTheExpectedAccount), errors.Is(err, store.ErrAccountChanged):
		return errf(1, "%s %s: the address holds another account than the one this command read (it was deleted and created again meanwhile); nothing was deleted — run this again%s",
			action, email, wrappedDetail(err, accountdelete.ErrNotTheExpectedAccount, store.ErrAccountChanged))
	}
	return errf(5, "%s %s: %v", action, email, err)
}

// wrappedDetail is ": <err>" when err is not one of the bare sentinels a
// message already words, so what wrapped it is still printed; "" otherwise.
func wrappedDetail(err error, sentinels ...error) string {
	for _, s := range sentinels {
		if err == s { //nolint:errorlint // a bare sentinel, compared on purpose: a wrapped one carries more to print
			return ""
		}
	}
	return ": " + err.Error()
}

// confirmAccountDeletion asks before `fleet admin rm` / `fleet chat user del`
// delete email. It returns the id of the account the operator confirmed
// ("" when nobody was asked) and 0 to go on, or the exit code to return, with
// nothing deleted. --no-confirm (noConfirm) skips it, reading nothing.
// Without a terminal it refuses. On a terminal it first refuses what the
// deletion would refuse anyway — no such account, a server running (by its
// heartbeat; deleteAccount's lock is the check that holds), a team-shared
// project still owned — so the operator is not asked for nothing, then shows
// what goes, counted, and waits for the email typed back.
func confirmAccountDeletion(ctx context.Context, st accountDeletionPreflight, action, email string, noConfirm bool, c confirmIO) (string, int) {
	if noConfirm {
		return "", 0
	}
	if !c.tty {
		return "", errf(1, "refusing to %s the account %s without confirmation: it permanently deletes the account and everything it owns "+
			"(conversations, projects, memories, connections, user API keys, files); pass --no-confirm to %s it from a script", action, email, action)
	}
	if seen, err := st.ServerHeartbeatSeen(ctx); err != nil {
		return "", errf(5, "%s %s: server presence check: %v", action, email, err)
	} else if seen {
		return "", accountDeletionErrorCode(action, email, serverRunningError())
	}
	if err := st.DeleteUserPreflight(ctx, email); err != nil {
		return "", accountDeletionErrorCode(action, email, err)
	}
	sum, err := st.AccountDeletionSummary(ctx, email)
	if err != nil {
		return "", accountDeletionErrorCode(action, email, err)
	}
	if code := confirmDestructive(c, false, confirmPrompt{
		warning:        accountDeletionWarning(sum),
		emailToConfirm: sum.Email,
		aborted:        "aborted: the answer was not " + sum.Email + "; nothing was deleted",
	}); code != 0 {
		return "", code
	}
	return sum.ID, 0
}

// accountDeletionWarning is what an account deletion removes, counted from
// sum, as confirmAccountDeletion shows it.
func accountDeletionWarning(sum store.AccountDeletionSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "this permanently deletes the account %s (role %s) and everything it owns:\n", sum.Email, sum.Role)
	fmt.Fprintf(&b, "  - conversations: %d (with their messages and files; those in the recycle bin too)\n", sum.Conversations)
	fmt.Fprintf(&b, "  - projects it owns: %d (other members' chats in them are kept, outside any project)\n", sum.OwnedProjects)
	fmt.Fprintf(&b, "  - memories: %d (its own and the shared memories of its projects)\n", sum.Memories)
	fmt.Fprintf(&b, "  - remote MCP connections: %d (with their stored sign-ins)\n", sum.RemoteMCPServers)
	b.WriteString("  - its skills, personas, schedules, settings, user API keys, uploads and Operations Center access\n")
	b.WriteString("No fleet command brings it back. A fleet backup dump holds only its database rows; its uploads, private workspace\n" +
		"files and user API keys (api_keys.json) come back only from the file-level backups docs/BACKUP_RESTORE.md describes.")
	return b.String()
}

// exitDeletedStepsLeft is the exit code of `fleet admin rm` and `fleet chat
// user del` when the account was deleted but a later step (its API keys, its
// files) did not complete: the warnings on stderr say what is left. Distinct
// from every refusal, which deletes nothing.
const exitDeletedStepsLeft = 6

// deletedExitCode is 0 for a complete deletion, exitDeletedStepsLeft when
// steps were left.
func deletedExitCode(res accountdelete.Result) int {
	if len(res.Problems) > 0 {
		fmt.Fprintf(os.Stderr, "the account is deleted, with %d step(s) left (warnings above)\n", len(res.Problems))
		return exitDeletedStepsLeft
	}
	return 0
}

// accountDeletedLine is the success line of `fleet admin rm` and `fleet chat
// user del`, admin or not. With steps left (res.Problems: the database
// deletion committed, but keys or files were not removed) it claims only the
// database rows and names what is left; the warnings printed before it say
// how each is removed.
func accountDeletedLine(email string, res accountdelete.Result) string {
	if len(res.Problems) == 0 {
		return fmt.Sprintf("deleted the account %s, everything it owned (login, Operations Center access, conversations, projects, memories, connections, files) and %s",
			email, userKeysPhrase(res.KeysDeleted))
	}
	var keysLeft, filesLeft bool
	for _, p := range res.Problems {
		switch p.Kind {
		case accountdelete.KeysUnavailable, accountdelete.KeysFailed:
			keysLeft = true
		default:
			filesLeft = true
		}
	}
	var left []string
	keys := " and " + userKeysPhrase(res.KeysDeleted)
	if keysLeft {
		left = append(left, "its user API keys")
		keys = ""
	}
	if filesLeft {
		left = append(left, "some of its uploads or private workspace files")
	}
	return fmt.Sprintf("deleted the account %s and its database rows (login, Operations Center access, conversations, projects, memories, connections)%s; "+
		"%s may remain: see the warnings above for what is left and what removes it", email, keys, strings.Join(left, " and "))
}

// userKeysPhrase says how many user API keys a deletion removed.
func userKeysPhrase(n int) string {
	if n == 1 {
		return "1 user API key"
	}
	return fmt.Sprintf("%d user API keys", n)
}

// attachmentDirInputs are the facts chooseAttachmentDir decides from, split
// out so the decision is testable without systemd.
type attachmentDirInputs struct {
	explicit    string // EMAIL_ATTACHMENT_DIR in the CLI's own environment
	envFileDir  string // EMAIL_ATTACHMENT_DIR in the service's env file ("" = unset)
	unitWorkDir string // the unit's WorkingDirectory ("" = unit unknown)
	cwd         string
}

// defaultAttachmentDir is config.Load's EMAIL_ATTACHMENT_DIR default, relative
// to the service's working directory.
const defaultAttachmentDir = "./data/attachments"

// chooseAttachmentDir resolves EMAIL_ATTACHMENT_DIR the way chooseKeyStore
// resolves the data directory: an explicit value in the CLI's environment
// wins; otherwise the service's (its env file's value, or the default,
// relative to the unit's WorkingDirectory); otherwise the default relative to
// the current directory.
func chooseAttachmentDir(in attachmentDirInputs) (dir, source string) {
	abs := func(base, p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Clean(filepath.Join(base, p))
	}
	if e := strings.TrimSpace(in.explicit); e != "" {
		return abs(in.cwd, e), "EMAIL_ATTACHMENT_DIR in your environment"
	}
	if in.unitWorkDir != "" {
		d := strings.TrimSpace(in.envFileDir)
		if d == "" {
			d = defaultAttachmentDir
		}
		return abs(in.unitWorkDir, d), "the fleet.service directory (unit WorkingDirectory + its env file)"
	}
	return abs(in.cwd, defaultAttachmentDir), defaultAttachmentDir + " relative to the current directory (no fleet.service found)"
}

// resolveAttachmentDir gathers chooseAttachmentDir's inputs from the box.
func resolveAttachmentDir() (string, string) {
	in := attachmentDirInputs{explicit: creds.StripEnvQuotes(envreg.Get("EMAIL_ATTACHMENT_DIR")), cwd: "."}
	if cwd, err := os.Getwd(); err == nil {
		in.cwd = cwd
	}
	in.unitWorkDir = unitProperty(serviceName(""), "WorkingDirectory")
	if in.unitWorkDir != "" {
		if vals, err := creds.ReadEnvValues(serverEnvFile(""), "EMAIL_ATTACHMENT_DIR"); err == nil {
			in.envFileDir = vals["EMAIL_ATTACHMENT_DIR"]
		}
	}
	return chooseAttachmentDir(in)
}
