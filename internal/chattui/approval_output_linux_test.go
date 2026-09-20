//go:build linux

package chattui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCLIApprovalEscapesActualTerminalOutput(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	slaveClosed := false
	defer func() {
		if !slaveClosed {
			_ = slave.Close()
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"resolved_approvals":[{"approval_id":"a","tool":"bash","status":"approved"}]}`)
			return
		}
		fmt.Fprint(w, `{"status":"approved","result_text":"done\u001b]52;c;evil"}`)
	}))
	defer srv.Close()
	var errOut bytes.Buffer
	if code := runResolveApproval(NewClient(Config{ServerURL: srv.URL}), "c", "a", true, slave, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	// runResolveApproval has written everything by return, so close the last
	// slave fd: the master then drains the pty buffer and errors (EIO on
	// Linux, sometimes io.EOF) — the deterministic end-of-drain a pty offers.
	// Do NOT reach for SetReadDeadline instead: it is not honored on this fd
	// (os.OpenFile without O_NONBLOCK is not pollable; the call fails with
	// os.ErrNoDeadline), so the read would block forever — that dead end is
	// why this test once hung. No timer decides correctness here.
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	slaveClosed = true

	// Drain everything the writer produced BEFORE asserting: it emits the
	// verb line and the result text in separate writes, so a single Read
	// returned only the first and the escaped "\u001b" was still in flight —
	// the ~5/10 flake this drain replaces. We accumulate ALL bytes and only
	// then check them, so a raw ESC emitted BEFORE the escaped form still
	// fails; never read-until-you-see-the-escape.
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := master.Read(buf)
		out.Write(buf[:n])
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) {
			break
		}
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "\x1b") || !strings.Contains(text, `\u001b`) {
		t.Fatalf("unsafe terminal output: %q", text)
	}
}
