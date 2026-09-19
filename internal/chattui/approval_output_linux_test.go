//go:build linux

package chattui

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	defer slave.Close()
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
	buf := make([]byte, 4096)
	n, err := master.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	text := string(buf[:n])
	if strings.Contains(text, "\x1b") || !strings.Contains(text, `\u001b`) {
		t.Fatalf("unsafe terminal output: %q", text)
	}
}
