// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// k8s_exec.go streams pod exec sessions for the kubernetes sandbox backend
// over the apiserver's WebSocket channel protocol (v4.channel.k8s.io): each
// binary frame's first byte names a channel — 0 stdin (client→server),
// 1 stdout, 2 stderr, 3 error/status (server→client) — and the server closes
// the connection when the exec'd process exits, after publishing a
// metav1.Status on channel 3 carrying the exit code.
//
// v4 is the oldest protocol every supported apiserver speaks; it cannot
// half-close stdin, so an exec whose process reads stdin TO EOF must bound
// the read itself — the backend wraps such commands in `head -c <n>` (see
// k8s_backend.go) rather than depending on the newer v5 close channel.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	k8sExecProtocolV4 = "v4.channel.k8s.io"

	k8sChannelStdin  = 0
	k8sChannelStdout = 1
	k8sChannelStderr = 2
	k8sChannelError  = 3

	// k8sExecHandshakeTimeout bounds the WebSocket dial+upgrade.
	k8sExecHandshakeTimeout = 15 * time.Second

	// k8sStdinChunk bounds a single stdin frame. Large writes (a bridge
	// request embedding a big code cell, a fileop write payload) are split so
	// no single frame approaches server message-size limits.
	k8sStdinChunk = 512 * 1024
)

// k8sExecSession is one live exec connection. Writes go to the process's
// stdin; the background read loop demultiplexes stdout/stderr/status frames
// into the sinks given at dial time. done is closed when the read loop ends;
// result() then reports the exec's outcome.
type k8sExecSession struct {
	conn *websocket.Conn

	writeMu sync.Mutex

	done chan struct{}

	mu       sync.Mutex
	exitCode int
	execErr  error
}

// execPod dials the exec subresource for the named pod and starts the
// background demux loop. stdout/stderr sinks must be goroutine-safe or owned
// solely by the loop. withStdin controls whether the server keeps a stdin
// channel open (a stdin-less exec gives the process an immediately-EOF stdin,
// matching the podman backend's unset cmd.Stdin).
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
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = path.Join(u.Path, "/api/v1/namespaces/"+namespace+"/pods/"+pod+"/exec")
	u.RawQuery = q.Encode()

	header := http.Header{}
	token, err := c.bearerToken()
	if err != nil {
		return nil, err
	}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialer := &websocket.Dialer{
		TLSClientConfig:  c.tlsConfig,
		Subprotocols:     []string{k8sExecProtocolV4},
		HandshakeTimeout: k8sExecHandshakeTimeout,
	}
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			// The upgrade response body carries the apiserver's status message
			// (RBAC denial, container not found) — surface it, bounded and
			// newline-sanitized (cluster-derived text ends up in logs).
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("pod exec dial: %w (HTTP %d: %.500s)", err, resp.StatusCode, sanitizeClusterText(string(body)))
		}
		return nil, fmt.Errorf("pod exec dial: %w", err)
	}

	s := &k8sExecSession{conn: conn, done: make(chan struct{}), exitCode: -1}
	go s.readLoop(stdout, stderr)
	return s, nil
}

// readLoop demultiplexes server frames until the connection ends, then
// records the outcome. A clean close after a Success status yields exit 0; a
// NonZeroExitCode status yields the process's code; a connection that ends
// without any status is an error (the process outcome is unknown).
func (s *k8sExecSession) readLoop(stdout, stderr io.Writer) {
	defer close(s.done)
	var status []byte
	sawStatus := false
	for {
		msgType, data, err := s.conn.ReadMessage()
		if err != nil {
			code, execErr := parseExecStatus(status, sawStatus, err)
			s.mu.Lock()
			s.exitCode, s.execErr = code, execErr
			s.mu.Unlock()
			return
		}
		if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
			continue
		}
		if len(data) == 0 {
			continue
		}
		payload := data[1:]
		switch data[0] {
		case k8sChannelStdout:
			if stdout != nil && len(payload) > 0 {
				_, _ = stdout.Write(payload)
			}
		case k8sChannelStderr:
			if stderr != nil && len(payload) > 0 {
				_, _ = stderr.Write(payload)
			}
		case k8sChannelError:
			sawStatus = true
			status = append(status, payload...)
		}
	}
}

// parseExecStatus turns the channel-3 metav1.Status (if any) plus the read
// error that ended the loop into (exitCode, err). Only a normal-closure /
// EOF-family end with a parsed status is a trustworthy outcome.
func parseExecStatus(status []byte, sawStatus bool, readErr error) (int, error) {
	if !sawStatus {
		if websocket.IsCloseError(readErr, websocket.CloseNormalClosure) {
			// Some proxies drop the status frame on a zero-exit process; treat a
			// clean close without status as success — the failure directions
			// (non-zero exit, kill, RBAC) all DO produce a status or an abnormal
			// close, so this cannot mask them.
			return 0, nil
		}
		// %s of the sanitized text, not %w: a close error's reason text is
		// server-supplied (remote), and nothing upstream matches on the
		// wrapped type — losing the chain costs nothing here.
		return -1, fmt.Errorf("pod exec ended without a status frame: %s", sanitizeClusterText(readErr.Error()))
	}
	var st struct {
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(status, &st); err != nil {
		return -1, fmt.Errorf("parse pod exec status: %w (raw: %.200s)", err, sanitizeClusterText(string(status)))
	}
	// Every message below is cluster-derived text that ends up in logged
	// errors — sanitized like everything else that leaves this package.
	switch {
	case st.Status == "Success":
		return 0, nil
	case st.Reason == "NonZeroExitCode":
		for _, cause := range st.Details.Causes {
			if cause.Reason == "ExitCode" {
				code, err := strconv.Atoi(cause.Message)
				if err != nil {
					return -1, fmt.Errorf("parse pod exec exit code %q: %w", sanitizeClusterText(cause.Message), err)
				}
				return code, nil
			}
		}
		return -1, fmt.Errorf("pod exec reported NonZeroExitCode without an ExitCode cause: %s", sanitizeClusterText(st.Message))
	default:
		// A failure that is not an exit code: the exec itself failed (command
		// not found in a way the shell couldn't report, container gone, …).
		return -1, fmt.Errorf("pod exec failed: %s", sanitizeClusterText(st.Message))
	}
}

// writeStdin sends bytes to the process's stdin, chunked. Goroutine-safe.
func (s *k8sExecSession) writeStdin(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for len(p) > 0 {
		n := len(p)
		if n > k8sStdinChunk {
			n = k8sStdinChunk
		}
		// No manual size arithmetic (`1+n` trips CodeQL's
		// allocation-size-overflow check, exactly like podmanArgs' old
		// `len(rest)+1`); append sizes the backing array itself.
		frame := append([]byte{k8sChannelStdin}, p[:n]...)
		if err := s.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return fmt.Errorf("write pod exec stdin: %w", err)
		}
		p = p[n:]
	}
	return nil
}

// wait blocks until the exec ends or ctx is done. On ctx expiry the
// connection is torn down (which unblocks the read loop) and ctx's error is
// returned — the PROCESS inside the pod may still be running; the caller owns
// the #796 containment (delete the pod, poison the sandbox).
func (s *k8sExecSession) wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		_ = s.conn.Close()
		<-s.done
		return -1, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.execErr
	}
}

// close tears the connection down and waits, boundedly, for the read loop to
// exit. Bounded for the same reason the podman backend bounds its own bridge
// reap: the wait runs under the sandbox mutex on paths that must not stall
// (Pool.Close drains parked sandboxes serially), so a wedged loop should cost
// a leaked goroutine, never a hung shutdown.
func (s *k8sExecSession) close() {
	_ = s.conn.Close()
	select {
	case <-s.done:
	case <-time.After(bridgeReapTimeout):
		log.Printf("sandbox: exec read loop did not exit within %s of connection close — abandoning the join to keep teardown bounded", bridgeReapTimeout)
	}
}

// runOneShotExec execs command in the pod, optionally feeding stdin, and
// waits for it to finish. ctx bounds the whole call. The command must
// consume a bounded stdin (v4 cannot signal stdin EOF): callers wrap
// stdin-to-EOF readers in `head -c <len>`.
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
	}
	return session.wait(ctx)
}
