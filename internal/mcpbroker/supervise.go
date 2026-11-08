package mcpbroker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ServeStdio runs a Server for backend over THIS process's stdin/stdout — the child
// side of SpawnClient. Protocol frames ride stdin/stdout; the broker must send all
// logs to stderr so it never corrupts the frame stream. It returns when the parent
// closes the pipe (EOF) or ctx is cancelled.
func ServeStdio(ctx context.Context, backend Backend) error {
	return NewServer(backend).Serve(ctx, &stdioConn{r: os.Stdin, w: os.Stdout})
}

// teardownGrace is how long a spawned broker has to exit after its stdin is closed
// and it is SIGTERMed, before it is SIGKILLed.
const teardownGrace = 3 * time.Second

// eofExitGrace is how long stop waits for the child to exit on its own after
// the pipe closes (EOF ends its Serve loop; it then closes its backends and
// flushes its spans) before it signals. A var so tests can shorten it.
var eofExitGrace = 2500 * time.Millisecond

// stdioConn adapts a child process's stdout (read side) and stdin (write side) into
// a single io.ReadWriteCloser — the connection the broker protocol rides over. The
// broker child speaks frames on stdin/stdout and logs to stderr.
type stdioConn struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *stdioConn) Close() error {
	werr := c.w.Close()
	if rerr := c.r.Close(); werr == nil {
		return rerr
	}
	return werr
}

// SpawnClient starts cmd as a broker child and returns a Client talking to it over
// the child's stdio, plus a stop func that tears the child down: close the pipe
// (EOF unblocks the child's Serve loop and fails outstanding calls), give the
// child a bounded grace (eofExitGrace) to exit on its own — its shutdown work,
// the span flush included, runs after Serve returns — then SIGTERM the child's
// process GROUP, SIGKILL after a further grace period, and reap. A hung child
// therefore delays shutdown by at most eofExitGrace + teardownGrace. The child is
// placed in its own process group so the signal also reaches any grandchildren —
// e.g. the MCP server subprocesses the broker itself spawns.
//
// SpawnClient wires cmd.Stdin/Stdout; the caller may set cmd.Stderr (log capture)
// and cmd.Env beforehand. stop is idempotent and safe to defer.
func SpawnClient(cmd *exec.Cmd) (*Client, func() error, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcpbroker: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("mcpbroker: stdout pipe: %w", err)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true // own process group so teardown reaches grandchildren

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("mcpbroker: start broker: %w", err)
	}

	client := NewClient(&stdioConn{r: stdout, w: stdin})

	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			defer func() {
				// Wait has joined exec's stderr copier, so nothing more can
				// arrive: a line-buffering stderr sink (logging.ChildStderr)
				// flushes the child's last unterminated line now.
				// An *os.File (os.Stderr itself) is not ours to close.
				if _, isFile := cmd.Stderr.(*os.File); !isFile {
					if c, ok := cmd.Stderr.(io.Closer); ok {
						_ = c.Close()
					}
				}
			}()
			// Close the connection first: the child's Serve loop sees EOF and
			// returns, and any outstanding calls fail rather than hang.
			_ = client.Close()
			pgid := cmd.Process.Pid
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			// The EOF-driven exit first: signalling at once would kill the
			// child before its deferred span flush ran.
			select {
			case stopErr = <-done:
				// Reap any grandchild (an MCP server subprocess) the child
				// left in its group; ESRCH when there is none.
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
				return
			case <-time.After(eofExitGrace):
			}
			// Signal the whole group (negative pid), escalating to SIGKILL if the
			// child does not exit within the grace window.
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case stopErr = <-done:
			case <-time.After(teardownGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				stopErr = <-done
			}
		})
		return stopErr
	}
	return client, stop, nil
}
