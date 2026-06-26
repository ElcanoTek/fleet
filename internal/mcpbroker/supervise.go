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

// stdioConn adapts a child process's stdout (read side) and stdin (write side) into
// a single io.ReadWriteCloser — the connection the broker protocol rides over. The
// broker child speaks frames on stdin/stdout and logs to stderr, the same split the
// ACP agent subprocess uses.
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
// (EOF unblocks the child's Serve loop and fails outstanding calls), SIGTERM the
// child's process GROUP, then SIGKILL after a grace period, and reap. The child is
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
			// Close the connection first: the child's Serve loop sees EOF and
			// returns, and any outstanding calls fail rather than hang.
			_ = client.Close()
			// Signal the whole group (negative pid), escalating to SIGKILL if the
			// child does not exit within the grace window.
			pgid := cmd.Process.Pid
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
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
