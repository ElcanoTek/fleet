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
	msg := fs.String("message", "", "non-interactive: send one message, print the buffered final reply to stdout when the turn ends, exit")
	noTUI := fs.Bool("no-tui", false, "force non-interactive mode (read the message from --message or stdin)")
	approve := fs.String("approve", "", "non-interactive: approve this staged approval id (needs --conversation), print the outcome, exit")
	deny := fs.String("deny", "", "non-interactive: deny this staged approval id (needs --conversation), exit")
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fleet chat [--message <text>|--no-tui] [--conversation <id>] [--model <slug>] [--email …] [--server …]")
		fmt.Fprintln(os.Stderr, "       fleet chat --conversation <id> --approve|--deny <approval-id>")
		fmt.Fprintln(os.Stderr, "\nInteractive TUI by default; --message/--no-tui runs one turn to stdout (scriptable).")
		fmt.Fprintln(os.Stderr, "On the box running fleet, the shared token is read from the server env file automatically;")
		fmt.Fprintln(os.Stderr, "you usually only need: fleet chat --email you@org")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if (*approve != "" && *deny != "") || ((*approve != "" || *deny != "") && (*msg != "" || *noTUI)) {
		fmt.Fprintln(os.Stderr, "fleet chat: choose one of --approve, --deny, or a chat message")
		return 2
	}

	cfg, err := Resolve(f, osEnv, osReadFile, osReadEnvValues)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet chat: "+err.Error())
		return 1
	}

	// With no explicit --model, adopt the workspace default the server
	// advertises on /client-config (the same slug a new web chat starts on).
	// Best-effort: an unreachable endpoint leaves the model empty and the
	// server's own "choose a model" error stands. Resumed conversations keep
	// their stored model — turnModel only sends the default on a NEW thread.
	client := NewClient(cfg)
	if cfg.Model == "" {
		if slug, derr := client.DefaultModel(context.Background()); derr == nil && slug != "" {
			client.AdoptDefaultModel(slug)
		}
	}

	if id := strings.TrimSpace(*approve); id != "" || strings.TrimSpace(*deny) != "" {
		if strings.TrimSpace(*deny) != "" {
			id = strings.TrimSpace(*deny)
		}
		return runResolveApproval(client, *conv, id, strings.TrimSpace(*deny) == "", os.Stdout, os.Stderr)
	}
	if strings.TrimSpace(*msg) != "" || *noTUI {
		return runOneShot(client, *conv, *msg, os.Stdin, os.Stdout, os.Stderr)
	}
	return runInteractive(client, cfg, *conv)
}

// runResolveApproval settles one staged approval card from the command line —
// the scriptable counterpart of the TUI's /approve //deny (and the web card's
// Send/Cancel). Approving RUNS the staged tool server-side, so the outcome
// text (task id, send confirmation) goes to stdout for automation to read.
func runResolveApproval(client *Client, convID, approvalID string, approve bool, out, errOut io.Writer) int {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		fmt.Fprintln(errOut, "fleet chat: --approve/--deny needs --conversation <id> (the approval's conversation)")
		return 2
	}
	if approve {
		pending, err := client.loadApprovals(context.Background(), convID, approvalID)
		if err != nil {
			fmt.Fprintln(errOut, "fleet chat: cannot review approval: "+err.Error())
			return 1
		}
		var found *pendingApproval
		for i := range pending {
			if pending[i].id == approvalID {
				found = &pending[i]
				break
			}
		}
		if found == nil {
			fmt.Fprintln(errOut, "fleet chat: refusing to approve: the card is not in the pending snapshot")
			return 1
		}
		if !found.settled && !found.executing {
			fmt.Fprintln(errOut, frozenApprovalReview(*found))
			if reason := found.refuseApprove(); reason != "" {
				fmt.Fprintln(errOut, "fleet chat: "+reason)
				return 1
			}
		}
	}
	status, resultText, err := client.ResolveApproval(context.Background(), convID, approvalID, approve)
	if err != nil {
		fmt.Fprintln(errOut, "fleet chat: "+sanitizeTerminalText(err.Error()))
		return 1
	}
	verb := "denied"
	if approve {
		verb = "approved"
	}
	fmt.Fprintf(out, "%s (%s)\n", verb, status)
	if t := strings.TrimSpace(resultText); t != "" {
		fmt.Fprintln(out, sanitizeTerminalText(t))
	}
	return 0
}

// runInteractive launches the TUI. It pings the server FIRST (before entering the
// alt-screen) so an unreachable/misconfigured server reports a clean one-line
// error instead of a blank UI that only fails on the first turn.
func runInteractive(client *Client, cfg Config, convID string) int {
	m := newModel(cfg)
	m.client = client // the shared client, carrying any adopted workspace default
	if cfg.Model == "" && client.defaultModel != "" && strings.TrimSpace(convID) == "" {
		m.history = append(m.history, styleDim.Render("— workspace default model: "+client.defaultModel+" (/model <slug> to change) —"))
	}
	m.convID = strings.TrimSpace(convID)
	if err := client.Ping(context.Background()); err != nil {
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

// runOneShot buffers one turn's authoritative reply and writes plain text to out
// when the turn ends, so text.replace can retract superseded drafts. The
// message is --message or, if empty, all of stdin. Tool-call/▸ progress, staged
// approval notices, and the conversation id go to errOut so a pipe capturing
// stdout gets only the agent's prose. This is the scriptable/CI path — it
// exercises auth + SSE parsing without a terminal.
func runOneShot(client *Client, convID, message string, in io.Reader, out, errOut io.Writer) int {
	message = strings.TrimSpace(message)
	if message == "" {
		b, _ := io.ReadAll(in)
		message = strings.TrimSpace(string(b))
	}
	if message == "" {
		fmt.Fprintln(errOut, "fleet chat: no message (pass --message or pipe text on stdin)")
		return 1
	}
	ctx := context.Background()
	var visible strings.Builder
	var staged []pendingApproval
	newConvID, err := client.Stream(ctx, message, strings.TrimSpace(convID), func(ev Event) {
		switch ev.Name {
		case "tool.approval_superseded":
			kept := staged[:0]
			for _, a := range staged {
				if a.tool != ev.Str("tool") {
					kept = append(kept, a)
				}
			}
			staged = kept
		case "text.delta":
			visible.WriteString(ev.Str("text"))
		case "text.replace":
			visible.Reset()
			visible.WriteString(ev.Str("text"))
		case "tool.call":
			if n := ev.Str("name"); n != "" {
				fmt.Fprintln(errOut, "▸ "+n)
			}
		case "tool.approval_required":
			// Critical tool staged for human review. One-shot has no card to
			// click, so say exactly how to settle it from the shell.
			id := ev.Str("approval_id")
			if id != "" {
				staged = append(staged, pendingApproval{id: id, tool: ev.Str("tool")})
			}
			fmt.Fprintln(errOut, "⚠ approval required: "+orDefault(ev.Str("tool"), "tool")+
				" — "+orDefault(approvalSummaryLine(ev.Str("tool"), ev.Data["summary"]), "(no summary)")+
				" (approval "+id+")")
			card := pendingApproval{id: id, tool: ev.Str("tool")}
			card.frozenArgs, card.frozenComplete, card.frozenPresent = parseFrozenArgs(ev.Data["frozen_args"])
			fmt.Fprintln(errOut, frozenApprovalReview(card))
		}
	})
	if visible.Len() > 0 {
		fmt.Fprint(out, visible.String())
		fmt.Fprintln(out) // trailing newline so piped output ends cleanly
	}
	// The conversation id is the ONLY way to resume or settle approvals from a
	// script — without it every one-shot turn starts an orphan thread.
	if newConvID != "" {
		fmt.Fprintln(errOut, "conversation: "+newConvID)
	}
	for _, a := range staged {
		fmt.Fprintln(errOut, "  settle it: fleet chat --conversation "+newConvID+" --approve "+a.id+"   (or --deny)")
	}
	if err != nil {
		fmt.Fprintln(errOut, "fleet chat: "+err.Error())
		return 1
	}
	return 0
}
