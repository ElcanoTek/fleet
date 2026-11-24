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

// deleteAccount is what `fleet admin rm` and `fleet chat user del` run: the
// same accountdelete.Delete as Settings → Admin → Users, pointed at the key
// store and attachment directory the fleet.service process uses. The key
// store is opened before anything is deleted, so an unreadable one stops the
// command with nothing changed (errKeyStore). It refuses while a fleet
// server runs against the database (errServerRunning), and holds a lock
// meanwhile that keeps a server from starting until it is done
// (store.ExcludeServers). What could not be removed after the database
// deletion is printed as a warning. Any other error is the database's, and
// nothing was deleted.
func deleteAccount(ctx context.Context, st *store.Store, email string) (accountdelete.Result, error) {
	release, err := st.ExcludeServers(ctx)
	if errors.Is(err, store.ErrServerRunning) {
		return accountdelete.Result{}, fmt.Errorf("%w: delete the account from Settings → Admin → Users while the server runs, or stop the server (systemctl stop %s) and run this again", errServerRunning, serviceName(""))
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
	res, err := accountdelete.Delete(ctx, st, email, accountdelete.Options{Keys: mgr, AttachmentDir: dir})
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
