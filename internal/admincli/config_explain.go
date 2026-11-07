package admincli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// explainUsage states explain's contract: the configured value and its source,
// from the sources it can see — never "the server's in-memory config".
const explainUsage = `usage: fleet config explain [KEY ...] [--all] [--json] [--no-unit] [--env-file <path>]

  Reports each setting's CONFIGURED value and where it comes from, drawn from
  the sources this command can see:
    - the systemd unit's own environment (Environment=, EnvironmentFile=,
      minus UnsetEnvironment=), on a systemd box — what the service starts
      with; it outranks your shell (--no-unit leaves it out);
    - your shell's process environment;
    - the env file, as fleet's own loader reads it;
    - the setting's alias spellings (deprecated ones are flagged);
    - its default, including one inherited from another setting.
  It is not the running server's in-memory configuration: admin overrides
  made in the app, values not yet reloaded, and run-time decisions are not
  shown. Secrets are always redacted.

  No KEY: every setting that is set or carries a note. --all: every setting
  (docs/CONFIGURATION.md).
`

// configExplain is `fleet config explain` (#2053); see explainUsage.
func configExplain(argv []string) int {
	fs := flag.NewFlagSet("config explain", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "server env file (default FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	all := fs.Bool("all", false, "list every registered setting, set or not")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	noUnit := fs.Bool("no-unit", false, "leave the systemd unit's environment out of the sources")
	fs.Usage = func() {
		_, _ = io.WriteString(fs.Output(), explainUsage)
		fs.PrintDefaults()
	}
	keys, flagArgs := splitPositionals(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	keys = append(keys, fs.Args()...)
	path := serverEnvFile(*envFile)
	var unit *config.UnitEnv
	if !*noUnit {
		unit = loadUnitEnv()
	}
	rep, err := config.Explain(config.ExplainOptions{EnvFile: path, Keys: keys, All: *all, Unit: unit})
	if err != nil {
		return errf(1, "%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return errf(5, "%v", err)
		}
		return 0
	}
	writeExplanations(os.Stdout, rep)
	return 0
}

// systemdUnitDirs is the unit search path the file-parsing fallback uses
// (systemd's own order). A var so tests can point it at a fixture.
var systemdUnitDirs = config.SystemdUnitDirs

// systemctlShow returns `systemctl show -p LoadState -p Environment -p
// EnvironmentFiles -p UnsetEnvironment <unit>`: systemd's merged view of the
// unit (base unit + drop-ins, resets applied). ok is false when systemctl is
// unavailable or fails (no systemd, a container), and the caller falls back
// to parsing unit files. A var so tests can stub it.
var systemctlShow = func(unit string) (out string, ok bool) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed systemctl binary; the unit name is passed as one argv element.
	b, err := exec.CommandContext(ctx, bin, "show",
		"-p", "LoadState", "-p", "Environment", "-p", "EnvironmentFiles", "-p", "UnsetEnvironment", unit).Output()
	if err != nil {
		return "", false
	}
	return string(b), true
}

// loadUnitEnv returns the fleet unit's environment (nil when there is no such
// unit): systemd's own view when systemctl answers, else the unit files
// parsed in systemd's order. EnvironmentFile= contents the CLI cannot read
// are listed in UnitEnv.Unreadable.
func loadUnitEnv() *config.UnitEnv {
	unit := serviceName("")
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	if out, ok := systemctlShow(unit); ok {
		return config.UnitEnvFromSystemctl(unit, out, os.ReadFile)
	}
	return config.UnitEnvFromFiles(systemdUnitDirs, unit, os.ReadFile)
}

// writeExplanations renders the sources, the table (KEY, VALUE, SOURCE), then
// any notes.
func writeExplanations(w io.Writer, rep *config.ExplainReport) {
	src := rep.Sources
	fmt.Fprintln(w, "sources (highest first):")
	if u := src.SystemdUnit; u != nil {
		fmt.Fprintf(w, "  systemd unit %s", u.Unit)
		if len(u.Files) > 0 {
			fmt.Fprintf(w, " (EnvironmentFile=%s)", strings.Join(u.Files, " "))
		}
		fmt.Fprintln(w)
		for _, f := range u.Unreadable {
			fmt.Fprintf(w, "    could not read %s — run as root to include it\n", f)
		}
	}
	fmt.Fprintf(w, "  process env: %s\n", src.ProcessEnv)
	switch {
	case src.EnvFile == "":
	case src.EnvFileReadByLoader:
		fmt.Fprintf(w, "  env file: %s\n", src.EnvFile)
	default:
		fmt.Fprintf(w, "  env file: %s — not read by fleet's own loader here (the unit does not point FLEET_ENV_FILE at it); its lines count only as the unit's EnvironmentFile=\n", src.EnvFile)
	}
	fmt.Fprint(w, "  defaults (the registry's, or inherited from another setting)\n\n")
	if len(rep.Settings) == 0 {
		fmt.Fprintln(w, "no registered setting is set (use --all to list every setting and its default)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tCONFIGURED VALUE\tSOURCE")
	for _, x := range rep.Settings {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", x.Key, explainCell(explainValueCell(x)), explainSourceCell(x))
	}
	_ = tw.Flush()
	for _, x := range rep.Settings {
		for _, n := range x.Notes {
			fmt.Fprintf(w, "note: %s: %s\n", x.Key, n)
		}
	}
}

func explainValueCell(x config.Explanation) string {
	switch {
	case x.Value == "" && !x.Set:
		return "(unset)"
	case x.Value == "":
		return "(empty)"
	}
	return x.Value
}

func explainSourceCell(x config.Explanation) string {
	s := x.Source
	if x.SourceDetail != "" {
		s += " (" + x.SourceDetail + ")"
	}
	if x.Spelling != "" && x.Spelling != x.Key {
		s += " as " + x.Spelling
	}
	if x.Deprecated {
		s += " (DEPRECATED alias — run `fleet config migrate-env`)"
	}
	if len(x.InheritedFrom) > 0 {
		s += " (inherited from " + strings.Join(x.InheritedFrom, " ← ")
		if x.InheritedSource != "" {
			s += ", set in " + x.InheritedSource
		}
		s += ")"
	}
	return s
}

// explainCell keeps a multi-line value (a webhook body template) on one row.
func explainCell(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// splitPositionals separates bare arguments (KEY names) from flags, so keys
// and flags may come in any order. A flag that takes a value (--env-file
// <path>) keeps its value.
func splitPositionals(argv []string) (positional, flags []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if (a == "--env-file" || a == "-env-file") && i+1 < len(argv) {
			flags = append(flags, argv[i+1])
			i++
		}
	}
	return positional, flags
}
