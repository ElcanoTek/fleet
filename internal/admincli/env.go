package admincli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/redact"
)

// `fleet env` — inspect + edit the deployment's env files without hand-typing
// paths or leaking secrets to the terminal (patterned after gig's `gig env`).
//
//	fleet env [show]   print the server + web env files, secret values masked
//	fleet env edit     open one of them in an editor ($EDITOR, or a picker)
//
// This is a host-side operator convenience over the SAME files the credential
// writers (`fleet config set-*`, `fleet mcp account set`) target: the server
// env file (FLEET_ENV_FILE / /etc/fleet/fleet.env / .env.local) and the
// web-tier env file (/etc/fleet/fleet-web.env / web/.env.local). It never
// prints a secret VALUE: show masks by the same name heuristic the diagnose
// scrubber seeds from (redact.IsSecretEnvName) and strips DSN userinfo, and
// edit hands the file to the operator's editor without echoing it.

// cmdEnv dispatches `fleet env [show|edit|help]`. Bare `fleet env` (or with
// only flags) is show, matching the read-before-write habit the verb exists
// to encourage.
func cmdEnv(argv []string) int {
	sub := "show"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub = argv[0]
		argv = argv[1:]
	}
	switch sub {
	case "show":
		return envShow(argv)
	case "edit":
		return envEdit(argv)
	case "help":
		envUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown env subcommand %q\n\n", sub)
		envUsage(os.Stderr)
		return 1
	}
}

func envUsage(w io.Writer) {
	fmt.Fprint(w, `fleet env — inspect + edit the deployment env files (secrets never echoed)

SUBCOMMANDS
  fleet env [show] [--env-file P] [--web-env-file P]
        print the server env file and the web-tier env file with every
        secret-looking value masked (keys matching KEY/TOKEN/SECRET/
        PASSWORD/CREDENTIAL; DSN passwords are stripped from URLs)
  fleet env edit [--web] [--env-file P] [--editor CMD]
        open the server env file (--web: the web-tier file) in an editor:
        --editor, else $VISUAL/$EDITOR, else an interactive pick of
        nano / vim / helix (offered via dnf install if missing; helix's
        binary is 'hx')

FILES
  server: --env-file, else FLEET_ENV_FILE, else /etc/fleet/fleet.env when
          /etc/fleet exists, else .env.local — the file fleet.service reads
          and config.Load parses (LLM keys, DSNs, MCP creds, tuning knobs)
  web:    /etc/fleet/fleet-web.env when /etc/fleet exists, else
          web/.env.local — the fleet-web.service file (AUTH_* SSO keys)

NOTES
  • the files are 0600; on a provisioned box editing needs root
    (sudo fleet env edit) and the mode is restored after the editor exits
  • server-file changes: preflight with fleet validate-config, apply with
    fleet restart (reloadable knobs also apply live via kill -USR2, #286)
  • web-file changes: apply with systemctl restart fleet-web
`)
}

// envShow prints both env files with secret values masked. Never prints a
// secret: values are masked by key name (redact.IsSecretEnvName — the same
// heuristic that seeds the diagnose scrubber) and surviving values get DSN
// userinfo stripped (redactDSN), so an embedded postgres://user:pass@ password
// can't slip through under a non-secret-looking key.
func envShow(argv []string) int {
	fs := flag.NewFlagSet("env show", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "server env file (default FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	webFile := fs.String("web-env-file", "", "web-tier env file (default /etc/fleet/fleet-web.env when present, else web/.env.local)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	code := 0
	if err := printRedactedEnvFile(os.Stdout, serverEnvFile(*envFile)); err != nil {
		code = errf(5, "%v", err)
	}
	fmt.Println()
	if err := printRedactedEnvFile(os.Stdout, webEnvFile(*webFile)); err != nil {
		code = errf(5, "%v", err)
	}
	return code
}

// printRedactedEnvFile renders one env file: a `# path` header, then sorted
// KEY=VALUE lines with secrets masked. A missing file is a note, not an error
// (the operator may simply not have created it yet); an unreadable one (0600
// under root, run without sudo) is an error naming the sudo fix.
func printRedactedEnvFile(w io.Writer, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(w, "# %s (not present — create it with `fleet env edit`)\n", path)
		return nil
	}
	vals, err := creds.ReadEnvValues(path)
	if err != nil {
		return fmt.Errorf("read %s: %w (0600 files need root: sudo fleet env show)", path, err)
	}
	fmt.Fprintf(w, "# %s\n", path)
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s=%s\n", k, redactEnvValue(k, vals[k]))
	}
	return nil
}

// redactEnvValue masks a value for display: whole-value [REDACTED] when the
// key NAME denotes a credential, else DSN-userinfo stripping so connection
// strings show host/db but never the password.
func redactEnvValue(key, value string) string {
	if redact.IsSecretEnvName(key) {
		return "[REDACTED]"
	}
	return redactDSN(value)
}

// envEdit opens the resolved env file in the operator's editor and restores
// the 0600 mode afterwards (an editor that writes via rename can drop it).
// The editor resolves from --editor, then $VISUAL/$EDITOR, then — only on a
// TTY — an interactive nano/vim/helix pick with a dnf install offer for a
// missing choice (fleet targets Fedora boxes; see scripts/bootstrap.sh).
func envEdit(argv []string) int {
	fs := flag.NewFlagSet("env edit", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "env file to edit (default: the server env file — FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	web := fs.Bool("web", false, "edit the web-tier env file instead (/etc/fleet/fleet-web.env when present, else web/.env.local)")
	editor := fs.String("editor", "", "editor command (default $VISUAL, then $EDITOR, else an interactive pick)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}
	path := serverEnvFile(*envFile)
	if *web {
		path = webEnvFile(*envFile)
	}

	if err := ensureEditableEnvFile(path); err != nil {
		return errf(5, "%v", err)
	}

	editorArgv := resolveEditorArgv(*editor, os.Getenv)
	if editorArgv == nil {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errf(1, "no editor configured and stdin is not a terminal — set $EDITOR or pass --editor")
		}
		picked, err := pickEditor(bufio.NewReader(os.Stdin), os.Stderr)
		if err != nil {
			return errf(1, "%v", err)
		}
		editorArgv = []string{picked}
	}

	before := envFileDigest(path)
	// Remember who owned the file: an editor that saves via rename (vim's
	// default) hands the new inode to the editing user — root under sudo — and
	// a file the service user owned is re-owned out from under it.
	beforeInfo, _ := os.Stat(path)
	// context.Background() on purpose: an interactive editor session has no
	// sane deadline — the operator closes it when they're done.
	//nolint:gosec // G204: the editor binary+args come from the operator's own --editor flag, $VISUAL/$EDITOR, or the interactive menu — this is a local operator CLI acting for the person at the terminal, not a server path.
	cmd := exec.CommandContext(context.Background(), editorArgv[0], append(editorArgv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errf(5, "editor %s: %v", editorArgv[0], err)
	}
	// The editor may have replaced the file via rename (vim's default) — put
	// the secrets-file mode AND the previous owner back regardless of what it
	// left behind (ownership is only touched when running as root and it
	// actually changed; see creds.PreserveOwner).
	if err := os.Chmod(path, 0o600); err != nil {
		return errf(5, "restore 0600 on %s: %v", path, err)
	}
	if err := creds.PreserveOwner(path, beforeInfo); err != nil {
		return errf(5, "restore owner on %s: %v", path, err)
	}

	if bytes.Equal(before, envFileDigest(path)) {
		fmt.Printf("no changes to %s\n", path)
		return 0
	}
	for _, warn := range lintEnvFile(path) {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warn)
	}
	fmt.Printf("saved %s\n", path)
	if *web {
		fmt.Println("apply with: systemctl restart fleet-web")
	} else {
		fmt.Println("preflight with: fleet validate-config")
		fmt.Println("apply with: fleet restart   (systemctl restart fleet; reloadable knobs also apply live via kill -USR2 — docs/CONFIG-RELOAD.md)")
	}
	return 0
}

// ensureEditableEnvFile creates a missing env file 0600 (parent dir 0700) and
// verifies the current user can actually write it, so a permission problem
// surfaces as one actionable error BEFORE an editor session the save would
// fail out of.
func ensureEditableEnvFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G302,G304: path is the operator's own env file; 0600 is exactly the secrets-file mode we must ensure.
	switch {
	case os.IsPermission(err):
		return fmt.Errorf("%s is not writable by you — run as root: sudo fleet env edit", path)
	case os.IsNotExist(err):
		// Parent directory missing (e.g. a fresh box before bootstrap). Create
		// it 0700 like the credential writers do, then retry once.
		if mkErr := os.MkdirAll(dirOf(path), 0o700); mkErr != nil {
			if os.IsPermission(mkErr) {
				return fmt.Errorf("cannot create %s — run as root: sudo fleet env edit", dirOf(path))
			}
			return mkErr
		}
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G302,G304: same operator-owned secrets file as above.
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}
	return f.Close()
}

// dirOf is filepath.Dir with the empty-path guard callers here need.
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

// resolveEditorArgv resolves the editor command from, in order: the --editor
// flag, $VISUAL, $EDITOR. Values are split on whitespace so "code -w" works.
// nil means none configured (the caller falls back to the interactive pick).
func resolveEditorArgv(flagVal string, getenv func(string) string) []string {
	for _, v := range []string{flagVal, getenv("VISUAL"), getenv("EDITOR")} {
		if fields := strings.Fields(v); len(fields) > 0 {
			return fields
		}
	}
	return nil
}

// editorMenu is the interactive pick: display name / binary on PATH / dnf
// package. Helix's binary is `hx` and isn't on a default Fedora box, so a
// missing selection gets a dnf install offer; the same path covers nano/vim
// on a stripped image.
var editorMenu = []struct{ name, bin, pkg string }{
	{"nano", "nano", "nano"},
	{"vim", "vim", "vim"},
	{"helix", "hx", "helix"},
}

// pickEditor renders the menu on out, reads one choice from in, installs the
// pick via dnf if its binary is absent (after a Y/n confirm), and returns the
// binary to run. in/out are injected so the parse/derive logic is testable;
// the dnf exec itself streams to the real stdout/stderr.
func pickEditor(in *bufio.Reader, out io.Writer) (string, error) {
	def := 0
	haveDefault := false
	fmt.Fprintln(out, "Pick an editor:")
	for i, e := range editorMenu {
		if _, err := exec.LookPath(e.bin); err == nil {
			fmt.Fprintf(out, "  %d) %s\n", i+1, e.name)
			if !haveDefault {
				def, haveDefault = i, true
			}
		} else {
			fmt.Fprintf(out, "  %d) %s (not installed — will dnf install on select)\n", i+1, e.name)
		}
	}
	fmt.Fprintf(out, "Choice [%d]: ", def+1)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read choice: %w", err)
	}
	idx, err := parseEditorChoice(line, def)
	if err != nil {
		return "", err
	}
	pick := editorMenu[idx]
	if _, err := exec.LookPath(pick.bin); err != nil {
		if err := dnfInstallEditor(pick.name, pick.pkg, in, out); err != nil {
			return "", err
		}
	}
	return pick.bin, nil
}

// parseEditorChoice maps the operator's menu answer (number or name; empty =
// the default) to an editorMenu index.
func parseEditorChoice(answer string, def int) (int, error) {
	s := strings.ToLower(strings.TrimSpace(answer))
	if s == "" {
		return def, nil
	}
	for i, e := range editorMenu {
		if s == fmt.Sprint(i+1) || s == e.name || s == e.bin {
			return i, nil
		}
	}
	return 0, fmt.Errorf("invalid choice %q (pick 1-%d, or a name)", s, len(editorMenu))
}

// dnfInstallEditor offers to `dnf install -y pkg` for a picked-but-missing
// editor. Needs root (like editing /etc/fleet itself); dnf's own permission
// error surfaces if not.
func dnfInstallEditor(name, pkg string, in *bufio.Reader, out io.Writer) error {
	if _, err := exec.LookPath("dnf"); err != nil {
		return fmt.Errorf("%s is not installed and dnf is not on PATH — install it manually or set $EDITOR", name)
	}
	fmt.Fprintf(out, "%s is not installed. Run `dnf install -y %s` now? (Y/n): ", name, pkg)
	answer, err := in.ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("read answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "n", "no":
		return fmt.Errorf("cancelled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	//nolint:gosec // G204: fixed "dnf install -y" with a package name from the hardcoded editorMenu table — nothing operator- or model-supplied reaches argv.
	cmd := exec.CommandContext(ctx, "dnf", "install", "-y", pkg)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dnf install %s: %w", pkg, err)
	}
	return nil
}

// envFileDigest hashes the file's current content (nil for unreadable/missing)
// so edit can tell "saved unchanged" from a real change without keeping the
// secret bytes around.
func envFileDigest(path string) []byte {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own env file, resolved by serverEnvFile/webEnvFile.
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

// lintEnvFile flags lines the server's parser would silently SKIP — a
// non-comment line without a `KEY=` shape — plus duplicate keys (last one
// wins, which usually means a forgotten stale line). Warnings only: the
// operator may be mid-way through something deliberate, and
// `fleet validate-config` is the real preflight.
func lintEnvFile(path string) []string {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's own env file.
	if err != nil {
		return nil
	}
	defer f.Close()
	var warnings []string
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, ok := envLineKey(t)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s:%d: not a KEY=VALUE line — the server will silently skip it", path, n))
			continue
		}
		if prev, dup := seen[key]; dup {
			warnings = append(warnings, fmt.Sprintf("%s:%d: duplicate key %s (also on line %d; the later line wins)", path, n, key, prev))
		}
		seen[key] = n
	}
	return warnings
}

// envLineKey extracts the key from a trimmed non-comment line, tolerating the
// same `export ` prefix the server's loader does.
func envLineKey(trimmed string) (string, bool) {
	trimmed = strings.TrimPrefix(trimmed, "export ")
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" {
		return "", false
	}
	return key, true
}
