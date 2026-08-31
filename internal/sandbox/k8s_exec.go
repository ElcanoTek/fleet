// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// k8s_exec.go streams pod exec sessions for the kubernetes sandbox backend
// through client-go's remotecommand WebSocket executor — the same transport
// kubectl uses.
//
// It used to hand-roll the v4.channel.k8s.io protocol over gorilla/websocket.
// That client was unit-tested against a fake apiserver and never against a
// real one, and the first real cluster (the bundle's own kind rehearsal,
// issue #1264) showed it losing exec stdin NONDETERMINISTICALLY for payloads
// beyond a few KB: the 28KB bridge upload wedged until its deadline on ~4 of
// 5 attempts, every warm pod churned on a two-minute cycle, and no tool call
// could run. The identical uploads through client-go's executor succeeded 5
// of 5 on the same cluster, pod and payloads — as did kubectl at 7MB — so the
// defect was in our client, not the cluster.
//
// ADR-0049 chose the hand-rolled client to keep the dependency tree small and
// recorded a revisit trigger. Demonstrated unreliability of exec streaming —
// the one operation every bash/python/file tool call rides on — is that
// trigger; the ADR is amended in the same change. Scope of the adoption is
// deliberately narrow: client-go is a TRANSPORT for exec only. Pod CRUD and
// the preflight stay on the hand-rolled REST client (five plain verbs that
// demonstrably work), and the kubeconfig posture is unchanged — fleet's own
// strict parser still refuses exec plugins and insecure-skip-tls-verify, and
// the rest.Config handed to client-go is built from that already-validated
// material, never by clientcmd.
//
// The executor speaks v5.channel.k8s.io ONLY (client-go's default: v4 has no
// stdin half-close, and v5 is served by every Kubernetes version fleet
// supports). Stdin EOF is therefore signalled for real on the wire; the
// `head -c <len>` bounding in k8s_backend.go stays as belt-and-braces so the
// commands do not depend on the close frame arriving.
//
// The in-package session API (execPod / runOneShotExec / writeStdin / wait /
// close) is preserved so the backend's lifecycle choreography — in particular
// the teardown ordering that closes the bridge stdout pipe before joining the
// session (#1257) — is untouched.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// k8sExecStartGrace is how long execPod watches a fresh stream for an
// immediate failure (upgrade rejection, RBAC denial) before handing the
// session to the caller. client-go has no separate dial phase — the whole
// exec is one call — so an early exit of the stream is the failure signal.
const k8sExecStartGrace = 200 * time.Millisecond

// k8sExecCloseGrace is how long close waits for a session to end by itself
// after stdin is half-closed, before falling back to cancellation. A clean
// exit (the bridge's `for line in sys.stdin` loop returning on EOF) takes
// milliseconds; the grace only elapses in full for a process that ignores
// stdin EOF, and the cancel backstop below still bounds those.
const k8sExecCloseGrace = 2 * time.Second

// k8sExecSession is one live exec. writeStdin feeds the process's stdin
// through a pipe the executor drains; stdout/stderr are demultiplexed by
// client-go into the sinks given at start. done closes when the exec ends;
// result() then reports the outcome.
type k8sExecSession struct {
	stdinW *io.PipeWriter // nil when the exec was started without stdin
	cancel context.CancelFunc
	// ctx is the session context cancel() releases. Held so the teardown
	// contract below is observable: once done is closed, this context is
	// cancelled, whoever ended the stream.
	ctx context.Context

	done chan struct{}

	mu       sync.Mutex
	exitCode int
	execErr  error
}

// restConfigForExec derives a client-go rest.Config from the client's
// already-validated credentials. Deliberately NOT clientcmd: fleet's
// kubeconfig parser is what refuses exec plugins and
// insecure-skip-tls-verify, and loading the file again here would silently
// re-admit both.
func (c *k8sClient) restConfigForExec() *rest.Config {
	cfg := &rest.Config{
		Host: c.baseURL.String(),
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   c.caPEM,
			CertData: c.certPEM,
			KeyData:  c.keyPEM,
		},
	}
	switch {
	case c.tokenFile != "":
		// client-go re-reads the file per request, so rotated bound tokens
		// keep working exactly as they did with the hand-rolled reader.
		cfg.BearerTokenFile = c.tokenFile
	case c.staticToken != "":
		cfg.BearerToken = c.staticToken
	}
	// Never proxy: left nil, client-go defaults to honoring
	// HTTPS_PROXY/NO_PROXY, while the hand-rolled REST transport (pod CRUD,
	// preflight) ignores proxy env entirely — so on a box with egress-proxy
	// env set, exec streams would silently reroute through the proxy while
	// every other apiserver call goes direct. Pinning "no proxy" keeps both
	// paths symmetric and matches this backend's strict posture (it already
	// refuses kubeconfig proxy-url).
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	return cfg
}

// execPod starts an exec in the pod. stdout/stderr sinks must be
// goroutine-safe or owned solely by the stream. withStdin controls whether
// the server keeps a stdin channel open (a stdin-less exec gives the process
// an immediately-EOF stdin, matching the podman backend's unset cmd.Stdin).
//
// ctx bounds only the START of the exec; the session then runs on its own
// cancellable context, because the bridge session deliberately outlives any
// single request context and is torn down via close(). Callers that want the
// whole exec bounded do so through wait(ctx), as before.
func (c *k8sClient) execPod(ctx context.Context, namespace, pod, container string, command []string, withStdin bool, stdout, stderr io.Writer) (*k8sExecSession, error) {
	q := url.Values{}
	q.Set("container", container)
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("tty", "false")
	q.Set("stdin", strconv.FormatBool(withStdin))
	for _, arg := range command {
		q.Add("command", arg)
	}
	u := *c.baseURL
	u.Path = path.Join(u.Path, "/api/v1/namespaces/"+namespace+"/pods/"+pod+"/exec")
	u.RawQuery = q.Encode()

	executor, err := remotecommand.NewWebSocketExecutor(c.restConfigForExec(), "GET", u.String())
	if err != nil {
		return nil, fmt.Errorf("pod exec executor: %w", err)
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	s := &k8sExecSession{cancel: cancel, ctx: sessCtx, done: make(chan struct{}), exitCode: -1}

	var stdin io.Reader
	if withStdin {
		pr, pw := io.Pipe()
		s.stdinW = pw
		stdin = pr
	}

	go func() {
		defer close(s.done)
		// Release the context whoever ends the stream, not just the callers
		// that reach close(). client-go's websocket executor runs a keepalive
		// that pings every few seconds and lives until this context is
		// cancelled — so a stream that dies on its own (the pod evicted or
		// deleted mid-exec) left the ping loop writing to a dead socket
		// forever. Observed on the validation cluster: one abandoned session
		// logged 3,285 "Websocket Ping failed" lines over 15 hours, leaking a
		// goroutine and a socket apiece and burying every other log line.
		//
		// Safe unconditionally: by the time this runs StreamWithContext has
		// returned, so the context has no remaining purpose. close() still
		// cancels for the paths that tear a LIVE stream down.
		defer cancel()
		streamErr := executor.StreamWithContext(sessCtx, remotecommand.StreamOptions{
			Stdin:  stdin,
			Stdout: stdout,
			Stderr: stderr,
		})
		code, execErr := execOutcome(streamErr)
		s.mu.Lock()
		s.exitCode, s.execErr = code, execErr
		s.mu.Unlock()
		if s.stdinW != nil {
			// Unblock any writeStdin parked on the pipe once the exec is over.
			_ = s.stdinW.CloseWithError(io.ErrClosedPipe)
		}
	}()

	// Watch a fresh stream briefly so an upgrade/RBAC failure surfaces as the
	// dial error callers expect, instead of a session whose first wait fails.
	grace := time.NewTimer(k8sExecStartGrace)
	defer grace.Stop()
	select {
	case <-s.done:
		// The stream ended already. An instant failure is a dial error; an
		// exec that legitimately completed this fast is a valid session whose
		// result is ready.
		if _, resErr := s.result(); resErr != nil {
			cancel()
			return nil, fmt.Errorf("pod exec dial: %w", resErr)
		}
		return s, nil
	case <-ctx.Done():
		cancel()
		<-s.done
		return nil, fmt.Errorf("pod exec dial: %w", ctx.Err())
	case <-grace.C:
		return s, nil
	}
}

// execOutcome maps client-go's stream error to (exitCode, execErr): nil is
// exit 0; a CodeExitError is the process's own non-zero exit (not a transport
// failure); anything else means the outcome is unknown and is surfaced as an
// error with cluster-derived text sanitized (it ends up in logs).
func execOutcome(streamErr error) (int, error) {
	if streamErr == nil {
		return 0, nil
	}
	var codeErr utilexec.CodeExitError
	if errors.As(streamErr, &codeErr) {
		return codeErr.Code, nil
	}
	if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
		return -1, streamErr
	}
	return -1, fmt.Errorf("pod exec: %s", sanitizeClusterText(streamErr.Error()))
}

// k8sStdinWriteTimeout bounds how long writeStdin waits for the executor to
// consume a payload. Normally the stream is up and the write completes in
// microseconds; the bound exists for the dial that never finishes — an
// apiserver that accepts TCP but stalls the WebSocket upgrade leaves the pipe
// with no reader, and the old hand-rolled client's 15s handshake timeout is
// gone (client-go sets none). An unbounded Write here parks the caller — which
// may hold the sandbox mutex — forever, wedging close() and Pool.Close behind
// it. A var so tests can shrink it.
var k8sStdinWriteTimeout = 60 * time.Second

// writeStdin sends bytes to the process's stdin. Goroutine-safe by way of the
// pipe; blocks until the executor has consumed the bytes, the stream ends, or
// the write times out (see k8sStdinWriteTimeout). On timeout the session is
// cancelled: a stream that cannot consume stdin within the bound is wedged,
// and cancelling makes StreamWithContext return, which closes the pipe and
// reaps the write goroutine.
func (s *k8sExecSession) writeStdin(p []byte) error {
	if s.stdinW == nil {
		return errors.New("write pod exec stdin: session started without stdin")
	}
	written := make(chan error, 1)
	go func() {
		_, err := s.stdinW.Write(p)
		written <- err
	}()
	timeout := time.NewTimer(k8sStdinWriteTimeout)
	defer timeout.Stop()
	select {
	case err := <-written:
		if err != nil {
			return fmt.Errorf("write pod exec stdin: %w", err)
		}
		return nil
	case <-s.done:
		// Stream over; the payload can never be consumed. The stream
		// goroutine's CloseWithError has already reaped (or will reap) the
		// write goroutine.
		return errors.New("write pod exec stdin: exec stream ended before consuming the payload")
	case <-timeout.C:
		s.cancel()
		return fmt.Errorf("write pod exec stdin: stream did not consume the payload within %v (dial or upgrade stalled)", k8sStdinWriteTimeout)
	}
}

// closeStdin signals stdin EOF — under v5 client-go half-closes the stdin
// stream on the wire, so a stdin-to-EOF reader terminates even without the
// `head -c` bounding the callers keep as belt-and-braces.
func (s *k8sExecSession) closeStdin() {
	if s.stdinW != nil {
		_ = s.stdinW.Close()
	}
}

// result reports the exec outcome; authoritative once done is closed.
func (s *k8sExecSession) result() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.execErr
}

// wait blocks until the exec ends or ctx is done. On ctx expiry the stream is
// cancelled (which unblocks the executor) and ctx's error is returned — the
// PROCESS inside the pod may still be running; the caller owns the #796
// containment (delete the pod, poison the sandbox).
func (s *k8sExecSession) wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		s.cancel()
		<-s.done
		return -1, ctx.Err()
	case <-s.done:
		return s.result()
	}
}

// close tears the session down, preferring a clean end over a cancellation.
// Stdin is half-closed first (a real EOF on the wire under v5), so a process
// blocked reading stdin — the bridge's `for line in sys.stdin` loop — exits by
// itself and the stream finishes without error. Cancelling first worked too,
// but it tore the websocket down under client-go's copy goroutines, which
// logged spurious "use of closed network connection" errors on every sandbox
// retirement. The cancel stays as the backstop for a process that ignores
// EOF, and the whole close stays bounded for the same reason the podman
// backend bounds its own bridge reap: the wait runs under the sandbox mutex
// on paths that must not stall (Pool.Close drains parked sandboxes serially),
// so a wedged stream should cost a leaked goroutine, never a hung shutdown.
func (s *k8sExecSession) close() {
	if s.stdinW != nil {
		_ = s.stdinW.Close()
		select {
		case <-s.done:
			s.cancel() // release the session context; the stream already ended
			return
		case <-time.After(k8sExecCloseGrace):
		}
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(bridgeReapTimeout):
		log.Printf("sandbox: exec stream did not exit within %s of cancel — abandoning the join to keep teardown bounded", bridgeReapTimeout)
	}
}

// runOneShotExec execs command in the pod, optionally feeding stdin, and
// waits for it to finish. ctx bounds the whole call. Commands that consume
// stdin to EOF stay bounded with `head -c <len>` as belt-and-braces; the
// closeStdin below half-closes on the wire under v5.
func (c *k8sClient) runOneShotExec(ctx context.Context, namespace, pod, container string, command []string, stdin []byte, stdout, stderr io.Writer) (int, error) {
	session, err := c.execPod(ctx, namespace, pod, container, command, len(stdin) > 0, stdout, stderr)
	if err != nil {
		return -1, err
	}
	defer session.close()
	if len(stdin) > 0 {
		if err := session.writeStdin(stdin); err != nil {
			// The write can fail because the process already exited (its outcome
			// frame may still be in flight) — fall through to wait, which reports
			// the authoritative result; surface the write error only if the exec
			// outcome is itself unusable.
			if code, werr := session.wait(ctx); werr == nil {
				return code, nil
			}
			return -1, err
		}
		session.closeStdin()
	}
	return session.wait(ctx)
}
