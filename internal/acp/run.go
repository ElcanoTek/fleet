package acp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
	"github.com/ElcanoTek/fleet/internal/version"
)

// DefaultTimeout bounds one ACP turn unless --timeout says otherwise. It is a
// hang guard for the ACP client, not a governance ceiling: fleet's own
// cost/token/iteration ceilings still apply to every turn server-side.
const DefaultTimeout = 30 * time.Minute

// hangUpWait bounds how long `fleet acp`, once its client has gone, waits for
// the prompts still in flight to stop their fleet turns before it exits. A
// Stop is an HTTP request this process has to stay alive to send: a fleet
// turn is detached from its stream by design, so exiting at once would leave
// it running server-side, spending, with nobody watching. 20s covers the
// slowest path that still gets a Stop out and answered: a turn submitted just
// before the hang-up waits up to conversationWait + turnGrace (6s) to learn
// which turn to stop, the Stop request itself is bounded at 10s (chattui's
// cancel timeout), and an accepted Stop then waits up to stopSettleWait (3s)
// for the turn's terminal frame — 19s, plus a margin. A turn already
// streaming needs only the last two, and against a healthy server the whole
// wait is a loopback round trip. A var so tests can shorten it.
var hangUpWait = 20 * time.Second

// Run is the `fleet acp` entry point: an ACP agent on stdin/stdout. stdout
// carries protocol frames only; every diagnostic goes to stderr. Returns the
// process exit code once the client disconnects, or once a SIGTERM, SIGINT or
// SIGHUP — the way editors usually stop an agent subprocess — has been
// handled as a disconnect.
func Run(argv []string) int {
	// An editor that exits or crashes takes the read end of our stdout with
	// it. Go kills a program that writes to a broken stdout with SIGPIPE, so
	// the turn's next streamed chunk would end the process before the Stop
	// below could go out. Ignored, such a write just fails (EPIPE), and the
	// hang-up is handled when stdin reaches EOF.
	signal.Ignore(syscall.SIGPIPE)
	return run(argv, os.Stdin, os.Stdout, os.Stderr, stopSignals)
}

// stopSignals starts relaying the first SIGTERM, SIGINT or SIGHUP, which run
// handles like the client hanging up: the in-flight turns are stopped
// (bounded by hangUpWait) before the process exits. The signals' default
// action is restored as soon as the first arrives, so a second one ends the
// process at once and shutdown can never hang. A signal the process was
// started with ignored (nohup, a background job) stays ignored.
func stopSignals() <-chan os.Signal {
	var watch []os.Signal
	for _, s := range []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			watch = append(watch, s)
		}
	}
	first := make(chan os.Signal, 1)
	if len(watch) == 0 {
		return first // never fires
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, watch...)
	go func() {
		s := <-sigs
		signal.Stop(sigs) // the next one takes the default action: exit
		first <- s
	}()
	return first
}

// run serves ACP on in/out until the client goes away: in reaches EOF, or a
// signal arrives on the channel stopSignals returns (Run's stopSignals; nil
// = none). Either way the agent hangs up, which stops every prompt in flight
// as a session/cancel would, stops writing to out, and then waits
// (awaitStops) for those turns to be stopped server-side before returning.
// stopSignals is called only once the connection is about to serve: during
// startup nothing can be in flight, so a signal then keeps its default action
// and ends the process at once.
func run(argv []string, in io.Reader, out, errOut io.Writer, stopSignals func() <-chan os.Signal) int {
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
	if *timeout < 0 {
		// A signed duration parses, but only 0 means "no bound": a mistyped
		// negative value must not silently remove the hang guard.
		fmt.Fprintf(errOut, "fleet acp: --timeout %s is negative (use 0 for no bound)\n", *timeout)
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
	agent.diag = errOut
	// The SDK starts reading as soon as the connection is built, so stdin is
	// held shut until the logger and the agent's connection are wired — a
	// client that writes initialize immediately must not reach a half-built
	// agent.
	ready := make(chan struct{})
	conn := acpsdk.NewAgentSideConnection(agent, clientOut{w: out, gone: agent.lifetime}, gatedReader{r: in, ready: ready})
	conn.SetLogger(slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelWarn})))
	agent.SetConnection(conn)
	var stop <-chan os.Signal // nil: never
	if stopSignals != nil {
		stop = stopSignals()
	}
	close(ready)
	var why string
	select {
	case <-conn.Done():
		// The client closed stdin. The hang-up's Stops (SetConnection's
		// watcher starts them too; hangUp is idempotent) are HTTP requests
		// this process must stay alive to send, so run cannot return yet.
		why = "the ACP client closed the connection"
	case s := <-stop:
		why = fmt.Sprintf("signal %v", s)
	}
	agent.hangUp()
	awaitStops(agent, why, errOut)
	return 0
}

// awaitStops waits, up to hangUpWait, for every prompt in flight when the
// client went away to finish its stop: a running turn is stopped
// server-side (reportGone says how that went), and a prompt still waiting
// for its session is abandoned unsubmitted. Nothing is said when nothing
// was in flight, the normal way a client ends a session. If the wait runs
// out, the process exits anyway, and stderr says a turn may still be running
// rather than leave the operator believing everything stopped.
func awaitStops(agent *Agent, why string, errOut io.Writer) {
	n, idle := agent.inFlight()
	if n == 0 {
		return
	}
	prompts := "prompt"
	if n != 1 {
		prompts += "s"
	}
	fmt.Fprintf(errOut, "fleet acp: %s with %d %s in flight; stopping their fleet turns before exiting (waiting up to %s)\n", why, n, prompts, hangUpWait)
	wait := time.NewTimer(hangUpWait)
	defer wait.Stop()
	select {
	case <-idle:
	case <-wait.C:
		where := "the fleet web chat"
		if agent.publicURL != "" {
			where += " (" + agent.publicURL + "/chat)"
		}
		fmt.Fprintf(errOut, "fleet acp: gave up after %s waiting for the in-flight fleet turns to stop; exiting anyway. A turn may still be running: check %s\n", hangUpWait, where)
	}
}

// errClientGone is what a write to stdout returns once the client has gone.
var errClientGone = errors.New("fleet acp: the ACP client has gone; nothing more is written to stdout")

// clientOut is fleet acp's stdout. Once the client has gone (gone, the
// agent's lifetime, has ended) nothing more is written: the frames — the
// rest of a stopped turn's stream, its notes, its cancelled answer — would
// reach nobody, and stdout is for protocol frames only, so what happened
// goes to stderr instead (reportGone, awaitStops). The cut lands before any
// stop runs, since each stop derives from the same lifetime. A write already
// under way when the client goes is not interrupted.
type clientOut struct {
	w    io.Writer
	gone context.Context
}

func (c clientOut) Write(p []byte) (int, error) {
	if c.gone.Err() != nil {
		return 0, errClientGone
	}
	return c.w.Write(p)
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
