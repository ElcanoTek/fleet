package admincli

// LEGACY-ENV-ALIAS: remove after migrate-env — this whole file, and its
// calls in cmdConfig (migrate-env) and cmdDoctor (the alias warnings).

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// configMigrateEnv is `fleet config migrate-env [--write] [--env-file <path>]`
// (ADR-0099): rewrite the env file's deprecated CHAT_/CUTLASS_ keys to their
// canonical FLEET_ names. Without --write it prints the plan and changes
// nothing. With --write it copies the file to <file>.bak-<UTC timestamp>
// first, then rewrites it atomically (0600, owner kept). It refuses when two
// spellings of one setting disagree. It never prints a value.
func configMigrateEnv(argv []string) int {
	fs := flag.NewFlagSet("config migrate-env", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "env file (default: the server env file — FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	write := fs.Bool("write", false, "apply the rewrite (default: dry run — print the plan only)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	if fs.NArg() > 0 {
		return errf(1, "usage: fleet config migrate-env [--write] [--env-file <path>]")
	}
	path := serverEnvFile(*envFile)
	m, err := config.PlanEnvMigration(path)
	if err != nil {
		return errf(5, "%v", err)
	}
	printMigrationPlan(os.Stdout, m)
	if len(m.Conflicts) > 0 {
		return errf(3, "%v — nothing was written", config.ErrMigrationConflicts)
	}
	if !m.Pending() {
		fmt.Println("nothing to rewrite")
		return 0
	}
	if !*write {
		fmt.Println("\ndry run: nothing was written. Apply with: fleet config migrate-env --write")
		return 0
	}
	backup, err := m.Write(time.Now())
	if err != nil {
		return errf(5, "%v", err)
	}
	fmt.Printf("\nrewrote %s (backup: %s)\n", path, backup)
	printEnvApplyHint()
	return 0
}

// printMigrationPlan prints line numbers and key names only — never values.
func printMigrationPlan(w io.Writer, m *config.EnvMigration) {
	fmt.Fprintf(w, "env file: %s\n", m.Path)
	if len(m.Changes) == 0 && len(m.Conflicts) == 0 {
		return
	}
	fmt.Fprintln(w, "")
	for _, c := range m.Changes {
		switch c.Action {
		case config.ActionRename:
			fmt.Fprintf(w, "  line %d: - %s=…\n", c.Line, c.From)
			fmt.Fprintf(w, "  line %d: + %s=…   (value unchanged)\n", c.Line, c.Canonical)
		case config.ActionDrop:
			fmt.Fprintf(w, "  line %d: - %s=…   (%s already holds the same value)\n", c.Line, c.From, c.Canonical)
		case config.ActionLeave:
			fmt.Fprintf(w, "  line %d:   %s left as is: the env file never admitted this spelling, so the server has been ignoring it; renaming it to %s would start applying it — decide by hand\n", c.Line, c.From, c.Canonical)
		}
	}
	for _, c := range m.Conflicts {
		fmt.Fprintf(w, "  CONFLICT: %s\n", c)
	}
}

// warnDeprecatedEnvAliases prints one warning per deprecated alias spelling
// set in the process environment or the server env file (ADR-0099). It is
// read-only and never prints a value; `fleet doctor` calls it first.
func warnDeprecatedEnvAliases(w io.Writer) {
	path := serverEnvFile("")
	findings, err := config.DeprecatedAliasFindings(path)
	if err != nil {
		fmt.Fprintf(w, "fleet doctor: could not check %s for deprecated env aliases: %v\n", path, err)
		return
	}
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "WARN deprecated CHAT_/CUTLASS_ env aliases (ADR-0099); rewrite the env file with `fleet config migrate-env --write`:\n")
	for _, f := range findings {
		fmt.Fprintf(w, "  - %s\n", f)
	}
}
