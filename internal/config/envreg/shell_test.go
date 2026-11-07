package envreg

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// shellInternal are UPPER_CASE names shell scripts read that are not fleet
// settings: set by the shell, the OS, sudo, systemd, CI, or a tool fleet
// calls. Each group is commented; add a name here only when it is not
// configuration an operator sets for fleet.
var shellInternal = map[string]bool{
	// bash itself
	"BASH_SOURCE": true, "BASH_VERSION": true, "BASH_REMATCH": true, "BASH_LINENO": true, "BASHPID": true,
	"FUNCNAME": true, "LINENO": true, "PIPESTATUS": true, "RANDOM": true, "SECONDS": true, "IFS": true,
	"OPTARG": true, "OPTIND": true, "REPLY": true, "PPID": true, "EUID": true, "UID": true, "HOSTNAME": true,
	"SHLVL": true, "PWD": true, "OLDPWD": true, "COLUMNS": true, "LINES": true, "EPOCHSECONDS": true,
	// login / OS environment
	"HOME": true, "PATH": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "LANG": true,
	"LC_ALL": true, "TMPDIR": true, "TZ": true, "XDG_RUNTIME_DIR": true, "XDG_CONFIG_HOME": true,
	"XDG_CACHE_HOME": true, "DBUS_SESSION_BUS_ADDRESS": true, "NO_COLOR": true, "EDITOR": true, "VISUAL": true,
	// sudo / su
	"SUDO_USER": true, "SUDO_UID": true, "SUDO_GID": true, "SUDO_COMMAND": true,
	// CI (GitHub Actions)
	"CI": true, "GITHUB_ACTIONS": true, "GITHUB_OUTPUT": true, "GITHUB_ENV": true, "GITHUB_STEP_SUMMARY": true,
	"GITHUB_WORKSPACE": true, "GITHUB_SHA": true, "GITHUB_REF": true, "RUNNER_TEMP": true,
	// tools fleet's scripts drive, read under their own names
	"GOPATH": true, "GOBIN": true, "GOFLAGS": true, "GOTOOLCHAIN": true, "GOCACHE": true, "GOMODCACHE": true,
	"PGPASSWORD": true, "PGHOST": true, "PGUSER": true, "PGDATABASE": true, "PGPORT": true,
	"DEBIAN_FRONTEND": true, "SSH_AUTH_SOCK": true, "SSH_CONNECTION": true, "SSH_TTY": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "MAINPID": true,
}

var (
	// shellAssign finds a name a script assigns itself.
	shellAssign = regexp.MustCompile(`(?:^|[;&|\s(])(?:export\s+|local\s+|readonly\s+|declare\s+(?:-[A-Za-z]+\s+)*)?([A-Z_][A-Z0-9_]*)(?:\[[^\]]*\])?\+?=`)
	// shellLoopOrRead finds `for NAME in`, `read [-opts] NAME…`, getopts NAME.
	shellLoopOrRead = regexp.MustCompile(`\b(?:for\s+([A-Z_][A-Z0-9_]*)\s+in|read\s+(?:-[a-zA-Z]+\s+(?:\S+\s+)?)*([A-Z_][A-Z0-9_ ]*)|getopts\s+\S+\s+([A-Z_][A-Z0-9_]*))`)
)

// shellFiles returns the shell scripts under scripts/ and deploy/: *.sh, and
// any file whose first line is a sh/bash shebang.
func shellFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, top := range []string{"scripts", "deploy"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".sh") {
				out = append(out, path)
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			if sc.Scan() {
				first := sc.Text()
				if strings.HasPrefix(first, "#!") && (strings.Contains(first, "bash") || strings.HasSuffix(first, "/sh")) {
					out = append(out, path)
				}
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// stripShellComment drops a line's # comment (outside quotes, roughly: a #
// at the line start or after whitespace).
func stripShellComment(ln string) string {
	t := strings.TrimSpace(ln)
	if strings.HasPrefix(t, "#") {
		return ""
	}
	if i := strings.Index(ln, " #"); i >= 0 && !strings.Contains(ln[:i], `"`) && !strings.Contains(ln[:i], `'`) {
		return ln[:i]
	}
	return ln
}

// shellRead finds ${NAME…} and $NAME reads of UPPER_CASE names (the name must
// end the identifier: ${Conffiles} is not a read of C), and Perl's
// $ENV{NAME} in embedded perl.
var (
	shellReadWord = regexp.MustCompile(`\$\{?([A-Z_][A-Z0-9_]*)([^A-Za-z0-9_]|$)`)
	perlEnvRead   = regexp.MustCompile(`\$ENV\{([A-Z_][A-Z0-9_]*)\}`)
)

// scriptLocals are names a scanned script reads that look like environment
// inputs to the scan but are not configuration, each with the reason.
var scriptLocals = map[string]string{
	"ENV_FILE": "scripts/update.sh: the default of its env_get helper (copied from doctor.sh); every call passes the file explicitly",
}

// embeddedLanguageTokens are $NAME tokens inside awk/perl programs embedded in
// the scripts — not shell variables at all.
var embeddedLanguageTokens = map[string]bool{
	"NF": true, "NR": true, // awk fields
	"Q":   true, // perl: my $Q = "\x27" (scripts/stack-check.sh, vendored)
	"ENV": true, // perl's %ENV itself; the key is checked via perlEnvRead
	"_":   true, // perl's $_
}

type shellReadSite struct{ name, where string }

// defaultExpansion is a read with a default/required expansion.
var defaultExpansion = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*):?[-=?]`)

var upperWord = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// fileAssignments returns, per name, the first line (1-based) the file
// assigns it on (a plain or prefixed assignment, a for/read/getopts target,
// a mapfile/readarray array).
func fileAssignments(body string) map[string]int {
	first := map[string]int{}
	mark := func(n string, line int) {
		if _, ok := first[n]; !ok {
			first[n] = line
		}
	}
	for i, ln := range strings.Split(body, "\n") {
		ln = stripShellComment(ln)
		for _, m := range shellAssign.FindAllStringSubmatch(ln, -1) {
			mark(m[1], i+1)
		}
		for _, m := range shellLoopOrRead.FindAllStringSubmatch(ln, -1) {
			for _, g := range m[1:] {
				for _, n := range strings.Fields(g) {
					mark(n, i+1)
				}
			}
		}
		if strings.Contains(ln, "mapfile") || strings.Contains(ln, "readarray") {
			for _, w := range strings.Fields(ln) {
				if upperWord.MatchString(w) {
					mark(w, i+1)
				}
			}
		}
	}
	return first
}

// scanShellFile returns a file's environment reads. A name is read from the
// environment when either
//   - nothing assigns it: not this file, and not a sourced lib (libAssigned) —
//     for a lib itself, not any script that sources it (allAssigned); or
//   - it is read with a default expansion (${X:-d}, ${X:=d}, ${X:?}) on or
//     before the line the file first assigns X — the env-input idiom
//     X="${X:-d}" and X="${X_ARG:-${X:-d}}". A default expansion after the
//     file's first assignment only guards `set -u` for its own variable.
func scanShellFile(rel, body string, libAssigned, allAssigned map[string]bool) []shellReadSite {
	isLib := strings.Contains(rel, "/lib/")
	assigned := fileAssignments(body)
	var out []shellReadSite
	for i, ln := range strings.Split(body, "\n") {
		ln = stripShellComment(ln)
		line := i + 1
		where := rel + ":" + itoa(line)
		seen := map[string]bool{}
		for _, m := range perlEnvRead.FindAllStringSubmatch(ln, -1) {
			// The script usually hands embedded perl its own variables
			// (TIER="$tier" perl …): only a name it never assigns is an input.
			if _, here := assigned[m[1]]; !here {
				out = append(out, shellReadSite{m[1], where})
			}
		}
		for _, m := range defaultExpansion.FindAllStringSubmatch(ln, -1) {
			n := m[1]
			first, here := assigned[n]
			if seen[n] || (isLib && allAssigned[n]) || (!here && libAssigned[n]) || (here && first < line) {
				continue
			}
			seen[n] = true
			out = append(out, shellReadSite{n, where})
		}
		for _, m := range shellReadWord.FindAllStringSubmatch(ln, -1) {
			n := m[1]
			_, here := assigned[n]
			if seen[n] || embeddedLanguageTokens[n] || here || libAssigned[n] || (isLib && allAssigned[n]) {
				continue
			}
			seen[n] = true
			out = append(out, shellReadSite{n, where})
		}
	}
	return out
}

// TestShellScriptReadsAreRegistered — Codex round 3 (#2057): the "no
// unregistered reads" rule covers the shell scripts under scripts/ and
// deploy/ too. A name a script reads from the environment (see
// scanShellFile) must be a registered setting or a commented shellInternal
// name.
func TestShellScriptReadsAreRegistered(t *testing.T) {
	root := moduleRoot(t)
	files := shellFiles(t, root)
	if len(files) < 10 {
		t.Fatalf("found only %d shell scripts; the walk is broken", len(files))
	}
	bodies := map[string]string{}
	libAssigned, allAssigned := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		bodies[rel] = string(b)
		for n := range fileAssignments(string(b)) {
			allAssigned[n] = true
			if strings.Contains(rel, "/lib/") {
				libAssigned[n] = true
			}
		}
	}
	missing := map[string]string{}
	reads := 0
	for rel, body := range bodies {
		for _, r := range scanShellFile(rel, body, libAssigned, allAssigned) {
			reads++
			if _, local := scriptLocals[r.name]; local || IsRegistered(r.name) || shellInternal[r.name] {
				continue
			}
			if prev, seen := missing[r.name]; !seen || r.where < prev {
				missing[r.name] = r.where
			}
		}
	}
	if reads < 100 {
		t.Fatalf("found only %d environment reads in shell scripts; the scan is broken", reads)
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Errorf("%s is read from the environment by %s but is not registered (internal/config/envreg/table.go) nor a commented shellInternal name", n, missing[n])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
