package acp

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
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
// slowest path to a Stop sent and answered: a turn submitted just before the
// hang-up waits up to conversationWait + turnGrace (6s) to learn which turn
// to stop, the Stop request itself is bounded at 10s (chattui's cancel
// timeout), and an accepted Stop then waits up to stopSettleWait (3s) for the
// turn's terminal frame — 19s, plus a margin. Two rarer branches read on
// longer, but only after their Stop was answered: an accepted Stop whose turn
// then reports ending on its own lets the stream finish for up to another
// stopSettleWait (22s in all), and a Stop that finds the turn already over
// waits up to conversationWait for its final frame (21s). The bound does not
// keep their Stop from going out, but the give-up note on stderr can then
// name a turn that had in fact stopped or ended, which is why it says a turn
// may still be running and where to check, not that one is. A turn already
// streaming needs only the Stop and its confirmation, and against a healthy
// server the whole wait is a loopback round trip. A var so tests can shorten
// it.
var hangUpWait = 20 * time.Second

// stderrWait bounds how long one line fleet acp writes to stderr may hold up
// what wrote it (boundedWriter): a session/new, or the client going away. A
// var so tests can shorten it.
var stderrWait = time.Second

// Run is the `fleet acp` entry point: an ACP agent on stdin/stdout. stdout
// carries protocol frames only; every diagnostic goes to stderr. Returns the
// process exit code once the client disconnects, or once a SIGTERM, SIGINT or
// SIGHUP — the way editors usually stop an agent subprocess — has been
// handled as a disconnect.
func Run(argv []string) int {
	// An editor that exits or crashes takes the read end of our stdout with
	// it. Go kills a program that writes to a broken stdout with SIGPIPE, so
	// the turn's next streamed chunk would end the process before the Stop
	// below could go out. Ignored, such a write just fails (EPIPE), which run
	// takes as the client going away (clientOut).
	signal.Ignore(syscall.SIGPIPE)
	return run(argv, os.Stdin, os.Stdout, os.Stderr, stopSignals)
}

// stopSignals starts relaying the first SIGTERM, SIGINT or SIGHUP, which run
// handles like the client hanging up: the in-flight turns are stopped
// (bounded by hangUpWait) before the process exits. The signals' default
// action is restored as soon as the first arrives, so a second one ends the
// process at once and shutdown can never hang. A SIGHUP or SIGINT the
// process was started with ignored (nohup, a background job) stays ignored:
// Go's runtime keeps that inherited ignore and reports it (signal.Ignored).
// It keeps no inherited ignore of SIGTERM — a Go program dies of a SIGTERM
// even when started with it ignored — so a SIGTERM is always handled here.
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

// run serves ACP on in/out until the client goes away: in reaches EOF, a
// write to out fails (the client stopped reading), or a signal arrives on
// the channel listen returns (Run passes stopSignals; nil = none). Each way
// the agent hangs up, which stops every prompt in flight as a session/cancel
// would, stops writing to out, and then waits (awaitStops) for those turns
// to be stopped server-side before returning. listen is called only once the
// connection is about to serve: during startup nothing can be in flight, so
// a signal then keeps its default action and ends the process at once.
func run(argv []string, in io.Reader, out, errOut io.Writer, listen func() <-chan os.Signal) int {
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
	// What fleet acp itself says on stderr (Agent.diag) goes through
	// boundedWriter: the MCP servers a session/new ignores, and what became
	// of the turns once the client has gone, which must never hold up the
	// exit. slog keeps plain stderr: it carries only the SDK's diagnostics
	// about the live connection.
	diag := boundedWriter{w: errOut}
	agent.diag = diag
	// A failed write to stdout is the client going away too (clientOut):
	// lose hangs up at once rather than wait for stdin's EOF, which a client
	// that stopped reading may never send.
	stdoutLost := make(chan struct{})
	var (
		loseOnce sync.Once
		lostErr  error
	)
	lose := func(err error) {
		loseOnce.Do(func() {
			lostErr = err
			agent.hangUp()
			close(stdoutLost)
		})
	}
	// The SDK starts reading as soon as the connection is built, so stdin is
	// held shut until the logger and the agent's connection are wired — a
	// client that writes initialize immediately must not reach a half-built
	// agent.
	ready := make(chan struct{})
	conn := acpsdk.NewAgentSideConnection(agent, clientOut{w: out, gone: agent.lifetime, lost: lose}, gatedReader{r: in, ready: ready})
	conn.SetLogger(slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelWarn})))
	agent.SetConnection(conn)
	var stop <-chan os.Signal // nil: never
	if listen != nil {
		stop = listen()
	}
	close(ready)
	var why string
	select {
	case <-conn.Done():
		// The client closed stdin. The hang-up's Stops (SetConnection's
		// watcher starts them too; hangUp is idempotent) are HTTP requests
		// this process must stay alive to send, so run cannot return yet.
		why = "the ACP client closed the connection"
	case <-stdoutLost:
		why = fmt.Sprintf("the ACP client stopped reading stdout (%v)", lostErr)
	case s := <-stop:
		why = "got " + signalName(s)
	}
	agent.hangUp()
	awaitStops(agent, why, diag)
	// 0 however the client went, a signal included: the client (or the editor
	// that sent the signal) ended the session, as it does by closing stdin,
	// and what became of each turn is on stderr, not in the exit code.
	return 0
}

// signalName is a stop signal as an operator knows it ("SIGTERM"), where
// os.Signal's String says "terminated".
func signalName(s os.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return s.String()
}

// awaitStops waits, up to hangUpWait, for every prompt in flight when the
// client went away to finish its stop: a running turn is stopped
// server-side (reportGone says how that went), and a prompt still waiting
// for its session is abandoned unsubmitted, at once. Nothing is said when no
// turn was running, the normal way a client ends a session. If the wait runs
// out, the process exits anyway, and stderr says a turn may still be running
// and where to check, rather than leave the operator believing everything
// stopped. The bound starts before anything is written, and errOut is a
// boundedWriter, so a stderr nobody drains delays the return by at most
// stderrWait past hangUpWait.
func awaitStops(agent *Agent, why string, errOut io.Writer) {
	wait := time.NewTimer(hangUpWait)
	defer wait.Stop()
	idle, turns := agent.inFlight()
	select {
	case <-idle:
		return
	default:
	}
	// Prompts that were only waiting for a session leave at once (Prompt):
	// what is said, and waited on in earnest, is the turns being stopped.
	if turns > 0 {
		noun, them := "fleet turn", "it"
		if turns != 1 {
			noun, them = "fleet turns", "them"
		}
		fmt.Fprintf(errOut, "fleet acp: %s with %d %s running; stopping %s before exiting (waiting up to %s)\n", why, turns, noun, them, hangUpWait)
	}
	select {
	case <-idle:
	case <-wait.C:
		fmt.Fprintf(errOut, "fleet acp: gave up after %s waiting for the in-flight fleet turns to finish stopping; exiting anyway. A Stop may not have reached fleet, or fleet had not confirmed it, so a turn may still be running: check %s\n",
			hangUpWait, strings.Join(agent.stoppingConversations(), "; "))
	}
}

// errStderrStuck is what a boundedWriter write returns when stderr did not
// take it within stderrWait.
var errStderrStuck = errors.New("fleet acp: stderr did not take the write in time")

// boundedWriter is stderr for what fleet acp itself says (Agent.diag): the
// MCP servers a session/new ignores and, once the client has gone, what
// became of its turns. A client may leave stderr a full pipe that nobody
// drains, and a write to it then blocks for good: after the client has gone,
// with stdin already at EOF, nothing else would end the process, so the bound
// on the shutdown (hangUpWait) would not hold. Each write therefore runs on
// its own goroutine and is waited for at most stderrWait. A line that misses
// that is given up on (it still goes out if stderr drains before the process
// exits), and the caller goes on. While the client is connected, a stuck
// stderr costs each session/new that carries MCP servers that wait, and
// leaves one goroutine blocked on its write.
type boundedWriter struct{ w io.Writer }

func (b boundedWriter) Write(p []byte) (int, error) {
	t := time.NewTimer(stderrWait)
	defer t.Stop()
	p = bytes.Clone(p) // the caller may reuse p once Write returns (fmt does)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := b.w.Write(p)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-t.C:
		return 0, errStderrStuck
	}
}

// errClientGone is what a write to stdout returns once the client has gone.
var errClientGone = errors.New("fleet acp: the ACP client has gone; nothing more is written to stdout")

// clientOut is fleet acp's stdout. A write that fails means the client has
// stopped reading: it exited or crashed and took the pipe with it (Run
// ignores SIGPIPE, so the write fails rather than killing the process), even
// if stdin has not reached EOF. lost reports that, and run takes it as the
// client going away. Once the client has gone (gone, the agent's lifetime,
// has ended) nothing more is written: the frames — the rest of a stopped
// turn's stream, its notes, its cancelled answer — would reach nobody, and
// stdout is for protocol frames only, so what happened goes to stderr
// instead (reportGone, awaitStops). The cut lands before any stop runs, since
// each stop derives from the same lifetime. A write already under way when
// the client goes is not interrupted.
type clientOut struct {
	w    io.Writer
	gone context.Context
	lost func(error)
}

func (c clientOut) Write(p []byte) (int, error) {
	if c.gone.Err() != nil {
		return 0, errClientGone
	}
	n, err := c.w.Write(p)
	if err != nil {
		c.lost(err)
	}
	return n, err
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
