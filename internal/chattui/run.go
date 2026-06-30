package chattui

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Run is the `fleet chat` entry point (#457). It resolves the connection config
// from flags/env, then either launches the interactive Bubble Tea TUI or — with
// --message / --no-tui — runs a single non-interactive turn to stdout (scripts,
// pipes, CI). Returns the process exit code.
func Run(argv []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	var f Flags
	fs.StringVar(&f.Server, "server", "", "fleet server URL (default $FLEET_CHAT_URL, else http://$FLEET_SERVER_ADDR, else http://127.0.0.1:8080)")
	fs.StringVar(&f.Email, "email", "", "your user email — X-User-Email (default $FLEET_USER_EMAIL)")
	fs.StringVar(&f.TokenFile, "token-file", "", "path to a file holding the shared server token (mode 0600); else $FLEET_SERVER_TOKEN / $CHAT_SERVER_TOKEN")
	fs.StringVar(&f.EnvFile, "env-file", "", "server env file to auto-read the token/addr from when not set otherwise (default $FLEET_ENV_FILE, else .env.local, else /etc/fleet/fleet.env)")
	fs.StringVar(&f.Model, "model", "", "model slug for the turn(s) (default: the conversation/server default)")
	fs.StringVar(&f.Persona, "persona", "", "persona for a NEW conversation")
	conv := fs.String("conversation", "", "resume an existing conversation by id")
	msg := fs.String("message", "", "non-interactive: send this one message, stream the reply to stdout, exit")
	noTUI := fs.Bool("no-tui", false, "force non-interactive mode (read the message from --message or stdin)")
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fleet chat [--message <text>|--no-tui] [--conversation <id>] [--model <slug>] [--email …] [--server …]")
		fmt.Fprintln(os.Stderr, "\nInteractive TUI by default; --message/--no-tui runs one turn to stdout (scriptable).")
		fmt.Fprintln(os.Stderr, "On the box running fleet, the shared token is read from the server env file automatically;")
		fmt.Fprintln(os.Stderr, "you usually only need: fleet chat --email you@org")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}

	cfg, err := Resolve(f, osEnv, osReadFile, osReadEnvValues)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet chat: "+err.Error())
		return 1
	}

	if strings.TrimSpace(*msg) != "" || *noTUI {
		return runOneShot(cfg, *conv, *msg, os.Stdin, os.Stdout, os.Stderr)
	}
	return runInteractive(cfg, *conv)
}

// runInteractive launches the TUI. It pings the server FIRST (before entering the
// alt-screen) so an unreachable/misconfigured server reports a clean one-line
// error instead of a blank UI that only fails on the first turn.
func runInteractive(cfg Config, convID string) int {
	m := newModel(cfg)
	m.convID = strings.TrimSpace(convID)
	if err := m.client.Ping(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "fleet chat: "+err.Error())
		return 1
	}
	p := tea.NewProgram(m)
	m.prog = p // the SSE goroutine pushes frames via p.Send
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleet chat: "+err.Error())
		return 1
	}
	return 0
}

// runOneShot sends a single turn and streams the reply as plain text to out. The
// message is --message or, if empty, all of stdin. Tool-call/▸ progress goes to
// errOut so a pipe capturing stdout gets only the agent's prose. This is the
// scriptable/CI path — it exercises auth + SSE parsing without a terminal.
func runOneShot(cfg Config, convID, message string, in io.Reader, out, errOut io.Writer) int {
	message = strings.TrimSpace(message)
	if message == "" {
		b, _ := io.ReadAll(in)
		message = strings.TrimSpace(string(b))
	}
	if message == "" {
		fmt.Fprintln(errOut, "fleet chat: no message (pass --message or pipe text on stdin)")
		return 1
	}
	client := NewClient(cfg)
	ctx := context.Background()
	var sawText bool
	_, err := client.Stream(ctx, message, strings.TrimSpace(convID), func(ev Event) {
		switch ev.Name {
		case "text.delta":
			if t := ev.Str("text"); t != "" {
				fmt.Fprint(out, t)
				sawText = true
			}
		case "tool.call":
			if n := ev.Str("name"); n != "" {
				fmt.Fprintln(errOut, "▸ "+n)
			}
		}
	})
	if sawText {
		fmt.Fprintln(out) // trailing newline so piped output ends cleanly
	}
	if err != nil {
		fmt.Fprintln(errOut, "fleet chat: "+err.Error())
		return 1
	}
	return 0
}
