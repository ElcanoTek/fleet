package admincli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/ElcanoTek/fleet/internal/config"
)

// configExplain is `fleet config explain [KEY…] [--all] [--json]` (#2053): for
// each setting, the effective value, where it came from (process env, env
// file, a deprecated alias spelling, the default) and whether that spelling
// is deprecated. Secrets are always redacted. With no KEY it lists every
// setting that is set; --all lists every registered setting.
func configExplain(argv []string) int {
	fs := flag.NewFlagSet("config explain", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "server env file (default FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	all := fs.Bool("all", false, "list every registered setting, set or not")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleet config explain [KEY ...] [--all] [--json] [--env-file <path>]")
		fmt.Fprintln(fs.Output(), "  Shows each setting's effective value, its source and whether the spelling is deprecated.")
		fmt.Fprintln(fs.Output(), "  Secrets are always redacted. No KEY: every setting that is set; --all: every setting (docs/CONFIGURATION.md).")
		fs.PrintDefaults()
	}
	keys, flagArgs := splitPositionals(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	keys = append(keys, fs.Args()...)
	path := serverEnvFile(*envFile)
	exported := envFileExportedBySystemd(path)
	xs, err := config.Explain(path, keys, *all, exported)
	if err != nil {
		return errf(1, "%v", err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			EnvFile         string               `json:"env_file"`
			SystemdExported bool                 `json:"env_file_exported_by_systemd"`
			Settings        []config.Explanation `json:"settings"`
		}{path, exported, xs}); err != nil {
			return errf(5, "%v", err)
		}
		return 0
	}
	writeExplanations(os.Stdout, path, exported, xs)
	return 0
}

// systemdUnitDirs are where the service unit and its drop-ins are looked for.
// A var so tests can point it at a fixture.
var systemdUnitDirs = []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

// envFileExportedBySystemd reports whether the box's fleet unit (or a
// drop-in) loads envFile with EnvironmentFile=. On the standard install it
// does, and then every line of the file reaches the server — including
// spellings fleet's own env-file loader does not admit.
func envFileExportedBySystemd(envFile string) bool {
	unit := serviceName("")
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	var files []string
	for _, d := range systemdUnitDirs {
		files = append(files, filepath.Join(d, unit))
		dropins, _ := filepath.Glob(filepath.Join(d, unit+".d", "*.conf"))
		files = append(files, dropins...)
	}
	return config.EnvFileExportedBySystemd(envFile, files)
}

// writeExplanations renders the table: KEY, VALUE, SOURCE, then any notes.
func writeExplanations(w io.Writer, envFile string, exported bool, xs []config.Explanation) {
	fmt.Fprintf(w, "env file: %s", envFile)
	if exported {
		fmt.Fprint(w, " (exported by the systemd unit's EnvironmentFile=: every line reaches the server)")
	}
	fmt.Fprint(w, "\n\n")
	if len(xs) == 0 {
		fmt.Fprintln(w, "no registered setting is set (use --all to list every setting and its default)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	for _, x := range xs {
		src := x.Source
		if x.Spelling != "" && x.Spelling != x.Key {
			src += " as " + x.Spelling
		}
		if x.Deprecated {
			src += " (DEPRECATED alias — run `fleet config migrate-env`)"
		}
		val := x.Value
		switch {
		case val == "" && !x.Set:
			val = "(unset)"
		case val == "":
			val = "(empty)"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\n", x.Key, explainCell(val), src)
	}
	_ = tw.Flush()
	for _, x := range xs {
		for _, n := range x.Notes {
			fmt.Fprintf(w, "note: %s: %s\n", x.Key, n)
		}
	}
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
