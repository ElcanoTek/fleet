// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/store"
)

func ttyAnswering(answer string) (confirmIO, *bytes.Buffer) {
	var out bytes.Buffer
	return confirmIO{in: strings.NewReader(answer), out: &out, tty: true}, &out
}

// On a terminal a y/N question goes on for y or yes only; a typed-back
// question goes on only for the exact text (case and surrounding space
// aside). Without a terminal it refuses, reading nothing, unless --no-confirm.
func TestConfirmDestructive(t *testing.T) {
	t.Parallel()
	yn := confirmPrompt{warning: "this overwrites the database.", refusal: "refused: pass --no-confirm"}
	typed := confirmPrompt{warning: "this deletes ann@x.com.", emailToConfirm: "ann@x.com", refusal: "refused: pass --no-confirm", aborted: "aborted: nothing was deleted"}
	sam := confirmPrompt{warning: "this deletes sam@x.com.", emailToConfirm: "sam@x.com", refusal: "refused"}
	kate := confirmPrompt{warning: "this deletes kate@x.com.", emailToConfirm: "kate@x.com", refusal: "refused"}
	for _, tc := range []struct {
		name      string
		p         confirmPrompt
		answer    string
		noConfirm bool
		tty       bool
		want      int
		wantOut   []string
	}{
		{"y/N: y", yn, "y\n", false, true, 0, []string{"WARNING: this overwrites the database. Continue? [y/N]: "}},
		{"y/N: YES", yn, " YES \n", false, true, 0, nil},
		{"y/N: empty", yn, "\n", false, true, 1, []string{"aborted"}},
		{"y/N: no newline, no answer", yn, "", false, true, 1, []string{"aborted"}},
		{"y/N: n", yn, "n\n", false, true, 1, []string{"aborted"}},
		{"typed: the email", typed, "ann@x.com\n", false, true, 0, []string{"WARNING: this deletes ann@x.com.\nType ann@x.com to confirm: "}},
		{"typed: other case, spaces", typed, "  Ann@X.com \n", false, true, 0, nil},
		{"typed: yes is not the email", typed, "yes\n", false, true, 1, []string{"aborted: nothing was deleted"}},
		{"typed: another email", typed, "bob@x.com\n", false, true, 1, []string{"aborted: nothing was deleted"}},
		// Case folding calls these the same letter; each can be an account
		// of its own, so they are other addresses (pgtext.SameEmail).
		{"typed: long s for s", sam, "\u017fam@x.com\n", false, true, 1, []string{"aborted"}},
		{"typed: Kelvin sign for K", kate, "\u212aate@x.com\n", false, true, 1, []string{"aborted"}},
		{"typed: capital S is the same address", sam, "Sam@x.com\n", false, true, 0, nil},
		{"no terminal, no flag", typed, "ann@x.com\n", false, false, 1, nil},
		{"no terminal, --no-confirm", typed, "", true, false, 0, nil},
		{"terminal, --no-confirm asks nothing", typed, "", true, true, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			c := confirmIO{in: strings.NewReader(tc.answer), out: &out, tty: tc.tty}
			if got := confirmDestructive(c, tc.noConfirm, tc.p); got != tc.want {
				t.Fatalf("exit %d, want %d (output %q)", got, tc.want, out.String())
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output %q does not contain %q", out.String(), w)
				}
			}
			if (tc.noConfirm || !tc.tty) && out.Len() != 0 {
				t.Errorf("asked although it must not: %q", out.String())
			}
		})
	}
}

// --no-confirm and its alias --yes both skip the question.
func TestNoConfirmFlagAlias(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--no-confirm"}, true},
		{[]string{"--yes"}, true},
		{[]string{"--yes=false"}, false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		got := noConfirmFlag(fs, "for tests")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got() != tc.want {
			t.Errorf("%v: %v, want %v", tc.args, got(), tc.want)
		}
	}
	// The flags are boolean for the positional splitter: neither swallows
	// the email after it. "--" ends the flags, and a second positional is
	// left for fs.Args() (the verbs refuse it).
	for _, tc := range []struct {
		argv      []string
		wantEmail string
		wantRest  string
		wantNArg  int
	}{
		{[]string{"--yes", "ann@x.com", "--database-url", "postgres://x"}, "ann@x.com", "--yes --database-url postgres://x", 0},
		{[]string{"--database-url", "postgres://x", "ann@x.com"}, "ann@x.com", "--database-url postgres://x", 0},
		{[]string{"--", "-ann@x.com"}, "-ann@x.com", "--", 0},
		{[]string{"--no-confirm", "--", "ann@x.com", "bob@x.com"}, "ann@x.com", "--no-confirm -- bob@x.com", 1},
		{[]string{"ann@x.com", "bob@x.com", "--yes"}, "ann@x.com", "bob@x.com --yes", 2},
	} {
		email, rest := splitPositionalMixed(tc.argv, noConfirmBoolFlags)
		if email != tc.wantEmail || strings.Join(rest, " ") != tc.wantRest {
			t.Errorf("%q: split = %q, %q; want %q, %q", tc.argv, email, rest, tc.wantEmail, tc.wantRest)
			continue
		}
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("database-url", "", "")
		noConfirmFlag(fs, "for tests")
		if err := fs.Parse(rest); err != nil || fs.NArg() != tc.wantNArg {
			t.Errorf("%q: parse err %v, NArg %d; want %d", tc.argv, err, fs.NArg(), tc.wantNArg)
		}
	}
}

type fakeDeletionPreflight struct {
	heartbeat bool
	preflight error
	summary   store.AccountDeletionSummary
	reads     int
}

func (f *fakeDeletionPreflight) ServerHeartbeatSeen(context.Context) (bool, error) {
	f.reads++
	return f.heartbeat, nil
}

func (f *fakeDeletionPreflight) DeleteUserPreflight(context.Context, string) error {
	f.reads++
	return f.preflight
}

func (f *fakeDeletionPreflight) AccountDeletionSummary(context.Context, string) (store.AccountDeletionSummary, error) {
	f.reads++
	if f.preflight != nil {
		return store.AccountDeletionSummary{}, f.preflight
	}
	return f.summary, nil
}

// An account deletion asks for the email typed back after showing what goes,
// counted; refuses before asking what the deletion would refuse anyway; and
// with --no-confirm reads nothing.
func TestConfirmAccountDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sum := store.AccountDeletionSummary{ID: "ann-id", Email: "ann@x.com", Role: "admin", Conversations: 12, OwnedProjects: 1, Memories: 34, RemoteMCPServers: 2}

	t.Run("shows the counts and wants the email", func(t *testing.T) {
		t.Parallel()
		f := &fakeDeletionPreflight{summary: sum}
		c, out := ttyAnswering("ann@x.com\n")
		id, code := confirmAccountDeletion(ctx, f, "remove", "Ann@x.com", false, c)
		if code != 0 || id != "ann-id" {
			t.Fatalf("id %q, exit %d; want the shown account's id, 0 (output %q)", id, code, out)
		}
		for _, w := range []string{"permanently deletes the account ann@x.com (role admin)", "conversations: 12", "projects it owns: 1", "memories: 34", "remote MCP connections: 2", "user API keys", "Type ann@x.com to confirm",
			"only its database rows", "api_keys.json", "docs/BACKUP_RESTORE.md"} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("prompt %q does not say %q", out, w)
			}
		}
	})
	t.Run("a wrong answer deletes nothing", func(t *testing.T) {
		t.Parallel()
		c, out := ttyAnswering("y\n")
		if _, code := confirmAccountDeletion(ctx, &fakeDeletionPreflight{summary: sum}, "remove", "ann@x.com", false, c); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if !strings.Contains(out.String(), "nothing was deleted") {
			t.Errorf("output %q does not say nothing was deleted", out)
		}
	})
	for _, tc := range []struct {
		name string
		f    *fakeDeletionPreflight
		want int
	}{
		{"no such account", &fakeDeletionPreflight{preflight: store.ErrUserNotFound}, 2},
		{"a server running", &fakeDeletionPreflight{heartbeat: true, summary: sum}, 1},
		{"a team-shared project owned", &fakeDeletionPreflight{preflight: &store.OwnsSharedProjectsError{Projects: []store.ProjectRef{{ID: "p", Name: "Shared"}}}}, 1},
		{"a database error", &fakeDeletionPreflight{preflight: errors.New("boom")}, 5},
	} {
		t.Run("refused before asking: "+tc.name, func(t *testing.T) {
			t.Parallel()
			c, out := ttyAnswering("ann@x.com\n")
			if _, code := confirmAccountDeletion(ctx, tc.f, "delete", "ann@x.com", false, c); code != tc.want {
				t.Fatalf("exit %d, want %d", code, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("asked before refusing: %q", out)
			}
		})
	}
	t.Run("no terminal refuses, reading nothing", func(t *testing.T) {
		t.Parallel()
		f := &fakeDeletionPreflight{summary: sum}
		if _, code := confirmAccountDeletion(ctx, f, "delete", "ann@x.com", false, confirmIO{in: strings.NewReader("ann@x.com\n"), out: &bytes.Buffer{}}); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if f.reads != 0 {
			t.Errorf("read the database %d time(s) before refusing", f.reads)
		}
	})
	t.Run("--no-confirm reads and asks nothing", func(t *testing.T) {
		t.Parallel()
		f := &fakeDeletionPreflight{heartbeat: true}
		c, out := ttyAnswering("")
		if id, code := confirmAccountDeletion(ctx, f, "delete", "ann@x.com", true, c); code != 0 || id != "" {
			t.Fatalf("id %q, exit %d; want no id, 0", id, code)
		}
		if f.reads != 0 || out.Len() != 0 {
			t.Errorf("reads %d, output %q; want none", f.reads, out)
		}
	})
}

// A patrol edit asks its question as a plain y/N, and refuses without a
// terminal (a reader that is not a file) unless --yes.
func TestConfirmPatrolEditUsesTheSharedQuestion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	c := confirmIO{in: strings.NewReader("y\n"), out: &out, tty: true}
	if code := confirmDestructive(c, false, confirmPrompt{question: "Change the prompt?"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != "Change the prompt? [y/N]: " {
		t.Errorf("asked %q", out.String())
	}
	if code := confirmPatrolEdit(strings.NewReader("y\n"), &bytes.Buffer{}, "Change the prompt?"); code != 1 {
		t.Errorf("no terminal: exit %d, want 1", code)
	}
}

// One mapping for every account-deletion error, from the preflight or the
// deletion: a refusal is exit 1 whichever step met it.
func TestAccountDeletionErrorCode(t *testing.T) {
	// Not parallel: captureStderr swaps os.Stderr.
	for _, tc := range []struct {
		err    error
		want   int
		detail string
	}{
		{store.ErrUserNotFound, 2, ""},
		// A wrapped sentinel keeps what wrapped it in the message.
		{fmt.Errorf("lookup: %w", store.ErrUserNotFound), 2, "lookup: "},
		{fmt.Errorf("commit: %w", store.ErrAccountChanged), 1, "commit: "},
		{&store.OwnsSharedProjectsError{Projects: []store.ProjectRef{{ID: "p", Name: "Shared"}}}, 1, ""},
		{fmt.Errorf("wrapped: %w", &store.OwnsSharedProjectsError{}), 1, ""},
		{accountdelete.ErrNotTheExpectedAccount, 1, ""},
		{store.ErrAccountChanged, 1, ""},
		{fmt.Errorf("%w (x): boom", errKeyStore), 1, ""},
		{serverRunningError(), 1, ""},
		{errors.New("boom"), 5, ""},
		// A refusal that also left the files to settle is printed whole.
		{&accountdelete.OutcomeUnconfirmedError{Err: store.ErrAccountChanged, ReadErr: errors.New("db down")}, 5, ""},
		{fmt.Errorf("%w; moving the account's files back failed: %w", store.ErrUserNotFound, errors.New("EACCES")), 5, ""},
	} {
		var got int
		stderr := captureStderr(t, func() { got = accountDeletionErrorCode("delete", "ann@x.com", tc.err) })
		if got != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.err, got, tc.want)
		}
		if tc.detail != "" && !strings.Contains(stderr, tc.detail) {
			t.Errorf("%v: stderr %q does not carry %q", tc.err, stderr, tc.detail)
		}
	}
}

// The success line claims only what went: with steps left it names the
// database rows as deleted and what may remain, never "everything".
func TestAccountDeletedLine(t *testing.T) {
	t.Parallel()
	files := accountdelete.Problem{Kind: accountdelete.UploadsFailed, Email: "ann@x.com", Err: "EACCES"}
	keys := accountdelete.Problem{Kind: accountdelete.KeysFailed, Email: "ann@x.com", Err: "busy"}
	for _, tc := range []struct {
		name    string
		res     accountdelete.Result
		want    []string
		notWant []string
	}{
		{"complete", accountdelete.Result{KeysDeleted: 2}, []string{"deleted the account ann@x.com, everything it owned", "files)", "2 user API keys"}, []string{"may remain"}},
		{"files left", accountdelete.Result{KeysDeleted: 1, Problems: []accountdelete.Problem{files}},
			[]string{"its database rows", "and 1 user API key;", "some of its uploads or private workspace files may remain", "warnings above"}, []string{"everything it owned", "files)"}},
		{"keys left", accountdelete.Result{Problems: []accountdelete.Problem{keys}},
			[]string{"its database rows", "its user API keys may remain"}, []string{"everything it owned", "0 user API keys"}},
		{"both left", accountdelete.Result{Problems: []accountdelete.Problem{keys, files}},
			[]string{"its user API keys and some of its uploads or private workspace files may remain"}, []string{"everything it owned"}},
	} {
		got := accountDeletedLine("ann@x.com", tc.res)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not say %q", tc.name, got, w)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q says %q", tc.name, got, w)
			}
		}
	}
}
