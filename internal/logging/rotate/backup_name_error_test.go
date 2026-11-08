package rotate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// When the backup directory cannot be read (EACCES on the existence check),
// rotation fails at once instead of searching for a free name forever under
// the rotator's and the process log's locks, and logging carries on in the
// current file: the record is written, and the error is returned so the
// caller's sink-failure path reports it (#2074 review).
func TestUnreadableBackupDirFailsRotationAndKeepsLogging(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	prev := osLstat
	t.Cleanup(func() { osLstat = prev })
	dir := t.TempDir()
	name := filepath.Join(dir, "foobar.log")
	l := &Logger{Filename: name, MaxSize: 8} // 8 bytes
	defer l.Close()

	if _, err := l.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	osLstat = func(string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "lstat", Path: dir, Err: fs.ErrPermission}
	}

	done := make(chan error, 1)
	go func() { done <- l.Rotate() }()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Rotate = %v, want the permission error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rotate did not return: backupName loops on an unreadable directory")
	}

	// Over MaxSize: the write triggers a rotation, which fails, and the record
	// still lands in the current file.
	n, err := l.Write([]byte("again\n"))
	if err == nil || n != len("again\n") {
		t.Fatalf("Write = %d, %v; want the record written and the rotation error returned", n, err)
	}
	data, rerr := os.ReadFile(name)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "again") {
		t.Fatalf("log file = %q, want both records", data)
	}
}

// The search for a free backup name is bounded: with every name taken it
// gives up with an error rather than looping.
func TestBackupNameSearchIsBounded(t *testing.T) {
	prev := osLstat
	t.Cleanup(func() { osLstat = prev })
	osLstat = func(string) (os.FileInfo, error) { return nil, nil } // everything exists
	if _, err := backupName(filepath.Join(t.TempDir(), "foobar.log")); err == nil {
		t.Fatal("backupName found a name although every one is taken")
	}
}
