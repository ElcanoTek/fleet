package acp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
	"github.com/ElcanoTek/fleet/internal/version"
)

// DefaultTimeout bounds one ACP turn unless --timeout says otherwise. It is a
// hang guard for the ACP client, not a governance ceiling: fleet's own
// cost/token/iteration ceilings still apply to every turn server-side.
const DefaultTimeout = 30 * time.Minute

// Run is the `fleet acp` entry point: an ACP agent on stdin/stdout. stdout
// carries protocol frames only; every diagnostic goes to stderr. Returns the
// process exit code once the client disconnects.
func Run(argv []string) int {
	return run(argv, os.Stdin, os.Stdout, os.Stderr)
}

func run(argv []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	var f chattui.Flags
	fs.StringVar(&f.Server, "server", "", "fleet server URL (default $FLEET_CHAT_URL, else http://$FLEET_SERVER_ADDR, else http://127.0.0.1:8080)")
	fs.StringVar(&f.Email, "email", "", "the fleet user ACP turns run as — X-User-Email (default $FLEET_USER_EMAIL)")
	fs.StringVar(&f.TokenFile, "token-file", "", "path to a file holding the shared server token (mode 0600); else $FLEET_SERVER_TOKEN / $CHAT_SERVER_TOKEN")
	fs.StringVar(&f.EnvFile, "env-file", "", "server env file to auto-read the token/addr from (default $FLEET_ENV_FILE, else .env.local, else /etc/fleet/fleet.env)")
	fs.StringVar(&f.Model, "model", "", "model slug for new sessions (default: the workspace default)")
	fs.StringVar(&f.Persona, "persona", "", "persona for new sessions")
	fs.StringVar(&f.PublicURL, "public-url", "", "web UI base URL for approval and conversation links (default $FLEET_PUBLIC_BASE_URL / $FLEET_PUBLIC_URL, ignored when --server is set)")
	timeout := fs.Duration("timeout", DefaultTimeout, "stop a turn that runs longer than this (0 = no bound)")
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: fleet acp [--email you@org] [--server URL] [--model slug] [--timeout 30m]")
		fmt.Fprintln(errOut, "\nRun fleet as an Agent Client Protocol agent on stdin/stdout, for ACP clients")
		fmt.Fprintln(errOut, "(buzz-acp, Zed, JetBrains, …) to launch. Each prompt is one governed turn on the")
		fmt.Fprintln(errOut, "running fleet server, exactly like `fleet chat`. See docs/ACP.md.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "fleet acp: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, cfgErr := chattui.ResolveFromEnvironment(f)
	if cfgErr != nil {
		// Keep serving: the client launched us and is about to initialize, so
		// the error reaches the user as an auth_required reply on session/new,
		// not as an unexplained exit.
		fmt.Fprintln(errOut, "fleet acp: "+cfgErr.Error())
	}
	cfg.ClientName = "fleet-acp"
	cfg.ModelNewConversationsOnly = true // --model is for new sessions
	cfg.InputIDsUserUnique = true        // keys are random or session-scoped
	client := chattui.NewClient(cfg)
	if cfgErr == nil {
		if err := client.Ping(context.Background()); err != nil {
			fmt.Fprintln(errOut, "fleet acp: "+err.Error())
		}
		if cfg.Model == "" {
			if slug, err := client.DefaultModel(context.Background()); err == nil && slug != "" {
				client.AdoptDefaultModel(slug)
			}
		}
	}

	agent := NewAgent(client, cfgErr, cfg.PublicURL, *timeout, version.Version())
	// The SDK starts reading as soon as the connection is built, so stdin is
	// held shut until the logger and the agent's connection are wired — a
	// client that writes initialize immediately must not reach a half-built
	// agent.
	ready := make(chan struct{})
	conn := acpsdk.NewAgentSideConnection(agent, out, gatedReader{r: in, ready: ready})
	conn.SetLogger(slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelWarn})))
	agent.SetConnection(conn)
	close(ready)
	<-conn.Done()
	return 0
}

// gatedReader blocks every Read until ready is closed.
type gatedReader struct {
	r     io.Reader
	ready <-chan struct{}
}

func (g gatedReader) Read(p []byte) (int, error) {
	<-g.ready
	return g.r.Read(p)
}
