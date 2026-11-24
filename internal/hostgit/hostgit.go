// Package hostgit is the one way fleet runs git on the host.
//
// fleet runs git host-side only in checkouts the operator owns: the fleet
// checkout (`fleet update --check`, which also fetches) and the client-bundle
// checkout (its freshness check and the residue note). Nothing fleet runs can
// write them. hostgit keeps the operator's credential helpers, ssh command and
// transports there, and turns off what a read-only check has no use for:
// hooks (scripts and hooks defined in configuration), core.fsmonitor, pagers,
// editors, submodule recursion and background maintenance.
//
// fleet runs no git at all in the repository at the workspace root, which
// worktree_config tasks branch from: that repository is written from
// sandboxes, and git takes programs from a repository (hooks, filters,
// transports) and reads whatever its files lead to. Registering, listing,
// removing and reclaiming per-run worktrees, and deleting a run's branch, are
// done without git by internal/worktree; resolving the base, creating the
// branch and the checkout happen in the run's sandbox (ADR-0118).
//
// hostgit does not check where the directory it is given lies: the fleet and
// client-bundle checkouts are directories the operator configured
// (FLEET_ROOT or the checkout beside the binary, FLEET_CLIENT_CONFIG_DIR),
// and keeping them out of the workspace root is the operator's to do.
//
// The guard test in this package fails when code elsewhere runs git without
// going through it.
//
// Command-line configuration (-c) is inherited by every git child process a
// command starts and wins over every configuration file, so the switches
// hold in children too. The environment is fleet's own (operator-controlled),
// minus the variables that would point git at a different repository than
// the dir argument.
package hostgit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitBinary is the program every command runs, resolved through PATH.
const gitBinary = "git"

// MaxDuration bounds every command Output runs, whatever
// deadline the caller's context carries.
const MaxDuration = 2 * time.Minute

// waitDelay bounds how long a command's output pipes are drained after its
// context ends, so a child that keeps them open cannot outlive the context.
const waitDelay = 5 * time.Second

// hookEvents is every hook event githooks(5) defines, as of git 2.55; update
// it when a newer git adds one (configured hooks for an event missing here
// are not switched off).
var hookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit",
	"pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push", "pre-receive",
	"update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite",
	"sendemail-validate", "fsmonitor-watchman", "p4-changelist",
	"p4-prepare-changelist", "p4-post-changelist", "p4-pre-submit",
	"post-index-change",
}

// commonConfig is the -c set every command carries: hooks, fsmonitor,
// submodule recursion and background maintenance off.
var commonConfig = func() []string {
	out := make([]string, 0, 6+len(hookEvents))
	out = append(out,
		"core.hooksPath=/dev/null",
		"core.fsmonitor=false",
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"gc.auto=0",
		"maintenance.auto=false",
	)
	for _, ev := range hookEvents {
		out = append(out, "hook."+ev+".enabled=false")
	}
	return out
}()

// subcommands are the git subcommands fleet runs in an operator checkout.
var subcommands = map[string]bool{
	"rev-parse": true, "rev-list": true, "symbolic-ref": true, "status": true, "fetch": true,
}

// clearedEnv are environment variables removed before git runs: each would
// make git act on a different repository, index, object store or
// configuration file than the dir argument names.
var clearedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE", "GIT_PREFIX", "GIT_CONFIG",
}

// ErrRefused is returned (wrapped) for an operation hostgit does not run.
var ErrRefused = errors.New("not an operation fleet runs here")

// Available reports whether a git binary is on PATH.
func Available() bool {
	_, err := exec.LookPath(gitBinary)
	return err == nil
}

// command returns a git command that runs args in the operator checkout at
// dir (see the package doc), for Output to run.
// An error means nothing was run: dir is empty, or the subcommand is not one
// fleet runs (ErrRefused).
func command(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("hostgit: empty repository directory")
	}
	if len(args) == 0 || !subcommands[args[0]] {
		return nil, fmt.Errorf("hostgit: git %q: %w", strings.Join(args, " "), ErrRefused)
	}
	full := make([]string, 0, 3+2*len(commonConfig)+len(args))
	full = append(full, "--no-pager", "-C", dir)
	for _, kv := range commonConfig {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)
	//nolint:gosec // G204: fixed git binary; dir and args are fleet-chosen (operator-configured paths, literal subcommands), passed as separate argv with no shell.
	cmd := exec.CommandContext(ctx, gitBinary, full...)
	cmd.Env = environ([]string{"GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:"})
	cmd.WaitDelay = waitDelay
	return cmd, nil
}

// Output runs command, bounded by MaxDuration, and returns its stdout. On a
// non-zero exit the *exec.ExitError carries stderr, as exec.Cmd.Output does.
func Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, MaxDuration)
	defer cancel()
	cmd, err := command(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

// environ is fleet's environment with clearedEnv removed and set applied.
func environ(set []string) []string {
	drop := make(map[string]bool, len(clearedEnv)+len(set))
	for _, k := range clearedEnv {
		drop[k] = true
	}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	parent := os.Environ()
	out := make([]string, 0, len(parent)+len(set))
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}
