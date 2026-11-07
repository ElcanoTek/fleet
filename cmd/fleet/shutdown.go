package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/runner"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// ── graceful shutdown helpers (#278) ──

// awaitShutdown blocks until a terminal signal or a fatal listener error,
// returning whether shutdown should be graceful (SIGTERM — deployment restart)
// or immediate (SIGINT — dev Ctrl-C — or a listener error). SIGUSR1 logs a
// diagnostic in-flight snapshot and keeps waiting (no shutdown).
func awaitShutdown(sigCh <-chan os.Signal, errCh <-chan error, chatSrv *httpapi.Server, pool *runner.Pool, lim *admission.Limiter) bool {
	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGTERM:
				log.Printf("fleet: SIGTERM received; beginning graceful shutdown")
				return true
			case syscall.SIGINT:
				log.Printf("fleet: SIGINT received; shutting down immediately")
				return false
			case syscall.SIGUSR1:
				//nolint:gosec // G706: only integer counters are formatted (%d) — no string from request input is logged, so no CR/LF log-line forgery is possible.
				log.Printf("fleet: status — active_chat_turns=%d active_sched_tasks=%d admission_inflight=%d/%d",
					chatSrv.ActiveTurns(), pool.ActiveTasks(), lim.InFlight(), lim.Total())
			}
		case err := <-errCh:
			log.Printf("fleet: listener error: %v; shutting down", err)
			return false
		}
	}
}

// performShutdown runs the drain sequence: stop admitting, drain in-flight chat
// turns + scheduled tasks within the grace budget (force-cancelling stragglers),
// then close the listeners. graceful=false is the fast path (cancel everything
// now). cancel stops the claim loop / scheduler-fed pool.
func performShutdown(graceful bool, grace time.Duration, cancel context.CancelFunc, chatSrv *httpapi.Server, pool *runner.Pool, poolDone <-chan struct{}, chatServer, orchServer, webServer *http.Server) {
	// Tell systemd we are stopping so it waits out the drain up to TimeoutStopSec.
	sdNotify("STOPPING=1")
	// Stop admitting new chat turns + flip /healthz to 503 (load balancers stop
	// routing here) and notify attached SSE clients to reconnect elsewhere.
	chatSrv.BeginShutdown()

	if graceful {
		// Stop the claim loop + scheduler-fed pool; in-flight tasks keep their
		// decoupled context and drain within the pool's own grace budget (parallel).
		cancel()
		graceCtx, graceStop := context.WithTimeout(context.Background(), grace)
		if chatSrv.DrainTurns(graceCtx) {
			log.Printf("fleet: all in-flight chat turns drained within grace")
		} else {
			//nolint:gosec // G706: grace is a time.Duration (digits+unit) and n is an int count — neither can forge a log line; values are operator-config-derived, not request input.
			log.Printf("fleet: grace period (%s) expired; force-cancelled %d in-flight chat turn(s)", grace, chatSrv.CancelInflightTurns())
		}
		// An approved card's call can outlive its POST (a long deal write
		// answers "executing" and finishes detached), so the HTTP handler
		// drain no longer covers it. Wait for it within the same grace; one
		// still running when the process exits keeps its executing sentinel,
		// which the next boot records as outcome unknown.
		if !chatSrv.DrainApprovalRuns(graceCtx) {
			//nolint:gosec // G706: grace is a time.Duration (digits+unit), operator-config-derived, not request input.
			log.Printf("fleet: grace period (%s) expired with approved actions still executing; their outcome will be recorded as unknown at the next start", grace)
		}
		graceStop()
		// After the turns, not before: a turn's completion tail can schedule a
		// queue-drain re-kick, so background work has to settle second or the
		// last thing shut down would be the thing that re-armed itself.
		chatSrv.StopBackground()
		<-poolDone // pool ran its own grace-bounded task drain in parallel
	} else {
		// Immediate path (SIGINT / listener error): cancel everything now.
		cancel()
		pool.ForceCancel()
		if n := chatSrv.CancelInflightTurns(); n > 0 {
			//nolint:gosec // G706: only an int count is formatted (%d) — no request-input string is logged, so no log-line forgery.
			log.Printf("fleet: cancelled %d in-flight chat turn(s)", n)
		}
		// Cancels pending timers and waits only for background work already
		// running, so this stays an immediate path.
		chatSrv.StopBackground()
		<-poolDone
	}

	// Close the HTTP listeners last: handlers have returned and detached work has
	// drained or been cancelled, so Shutdown completes promptly. Each server gets
	// the full grace budget and they close in parallel (not one off the other's
	// remainder).
	servers := []*http.Server{chatServer, orchServer}
	if webServer != nil {
		servers = append(servers, webServer)
	}
	closeServers(grace, servers...)
	log.Printf("fleet: shutdown complete")
}

// poolDrainGrace maps the process shutdown grace onto the worker pool's
// DrainGrace contract (#278): a non-positive grace (operator set
// FLEET_SHUTDOWN_GRACE_SECONDS=0) becomes negative so the pool force-cancels
// immediately instead of substituting its default.
func poolDrainGrace(cfg *config.Config) time.Duration {
	if grace := shutdownGrace(cfg); grace > 0 {
		return grace
	}
	return -1
}

// shutdownGrace resolves the graceful-shutdown grace period from
// FLEET_SHUTDOWN_GRACE_SECONDS (config default 30). A non-positive value means
// "no wait" (force-cancel immediately) and is returned as 0.
func shutdownGrace(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.ShutdownGraceSeconds <= 0 {
		return 0
	}
	return time.Duration(cfg.ShutdownGraceSeconds) * time.Second
}

// closeServers shuts the HTTP listeners down in parallel, each with the full
// grace budget (a hung one can't eat the other's time), so both drain their
// already-finishing connections without one starving the other. A non-positive
// budget falls back to 30s so Shutdown still has a bounded deadline.
func closeServers(grace time.Duration, servers ...*http.Server) {
	budget := grace
	if budget <= 0 {
		budget = 30 * time.Second
	}
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			defer safe.Recover("cmd.closeServers", nil)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			_ = s.Shutdown(shutdownCtx)
		}(srv)
	}
	wg.Wait()
}

// sdNotify sends one state line to systemd's notify socket (NOTIFY_SOCKET),
// e.g. "READY=1" once listeners are bound and "STOPPING=1" when draining. It is
// a no-op when the socket is unset (non-systemd / dev / tests) and best-effort
// otherwise — no new dependency, errors are intentionally swallowed. Handles the
// abstract-namespace form ('@' → leading NUL) systemd uses.
func sdNotify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	name := sock
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}
