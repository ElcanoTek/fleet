// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/pgtext"
)

// The operator verbs below destroy or overwrite data no command can bring
// back, and ask before they act, through confirmDestructive:
//
//   - on a terminal it prints what will happen and waits for an answer —
//     y/yes for a bulk change, or the email of the account that goes, where
//     a slip of the finger would erase the wrong person;
//   - without a terminal (a pipe, cron, CI) no one can answer, so it refuses
//     unless the operator passed --no-confirm (alias --yes) — a script
//     says so explicitly instead of hanging on a prompt or proceeding
//     unasked.
//
// The verbs: fleet restore, fleet migrate down / force, fleet chat
// set-model, fleet sched task set-model, fleet model set / rollout / reset
// (which also take -y), and fleet admin rm / fleet chat user del. The
// operator patrol edits (fleet patrol set-prompt / set-schedule) ask the same
// way although they lose nothing: the old prompt is backed up and the change
// noted in the chat.

// confirmIO is where a confirmation reads its answer and writes its prompt,
// and whether a person is there to answer.
type confirmIO struct {
	in  io.Reader
	out io.Writer
	tty bool
}

// stdConfirmIO is the process's stdin and stderr; tests replace it.
var stdConfirmIO = func() confirmIO {
	return confirmIO{in: os.Stdin, out: os.Stderr, tty: isTerminal(os.Stdin)}
}

// confirmPrompt is one destructive verb's question.
type confirmPrompt struct {
	// warning says what will happen; printed after "WARNING: ".
	warning string
	// question, when set instead of warning, is asked as "<question> [y/N]"
	// with no WARNING: a change that loses nothing (a patrol edit is backed
	// up first).
	question string
	// emailToConfirm, when set, is the account email the operator must type
	// back. It is compared as identity emails are (pgtext.SameEmail): other
	// capitals and surrounding space match, a different address never does,
	// even one Unicode case folding calls the same. Empty asks y/N.
	emailToConfirm string
	// refusal is the error printed without a terminal and without
	// --no-confirm; it names the flag.
	refusal string
	// aborted is printed when the answer is anything else ("aborted" when
	// empty).
	aborted string
}

// confirmDestructive asks p on c unless noConfirm, and returns 0 to go on
// or the exit code to return (1: refused or aborted, nothing done).
func confirmDestructive(c confirmIO, noConfirm bool, p confirmPrompt) int {
	if noConfirm {
		return 0
	}
	if !c.tty {
		return errf(1, "%s", p.refusal)
	}
	switch {
	case p.emailToConfirm != "":
		fmt.Fprintf(c.out, "WARNING: %s\nType %s to confirm: ", p.warning, p.emailToConfirm)
	case p.question != "":
		fmt.Fprintf(c.out, "%s [y/N]: ", p.question)
	default:
		fmt.Fprintf(c.out, "WARNING: %s Continue? [y/N]: ", p.warning)
	}
	line, _ := bufio.NewReader(c.in).ReadString('\n')
	ans := strings.TrimSpace(line)
	ok := false
	if p.emailToConfirm != "" {
		ok = pgtext.SameEmail(ans, p.emailToConfirm)
	} else {
		ans = strings.ToLower(ans)
		ok = ans == "y" || ans == "yes"
	}
	if !ok {
		aborted := p.aborted
		if aborted == "" {
			aborted = "aborted"
		}
		fmt.Fprintln(c.out, aborted)
		return 1
	}
	return 0
}

// confirmBulkMutation asks before a fleet-wide change (migrations, bulk
// model updates), answered y/yes.
func confirmBulkMutation(noConfirm bool, warning string) int {
	return confirmDestructive(stdConfirmIO(), noConfirm, confirmPrompt{
		warning: warning,
		refusal: "refusing fleet-wide mutation without confirmation: pass --no-confirm for scripted updates (or --dry-run to preview)",
	})
}

// noConfirmFlag registers --no-confirm and its alias --yes on fs, and
// returns whether either was passed. what completes the help line ("for
// scripted restores").
func noConfirmFlag(fs *flag.FlagSet, what string) func() bool {
	noConfirm := fs.Bool("no-confirm", false, "skip the interactive confirmation ("+what+"); required without a terminal")
	yes := fs.Bool("yes", false, "alias for --no-confirm")
	return func() bool { return *noConfirm || *yes }
}

// noConfirmBoolFlags names noConfirmFlag's flags for splitPositionalMixed.
var noConfirmBoolFlags = map[string]bool{"no-confirm": true, "yes": true}
