package rotate

import (
	"os"
	"testing"
)

// Close stops the mill goroutine (upstream left one running per Logger for the
// life of the process) and a later Write reopens the file and starts a new one.
func TestCloseStopsTheMillAndWriteReopens(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	dir := makeTempDir("TestCloseStopsTheMill", t)
	defer func() { _ = os.RemoveAll(dir) }()

	l := &Logger{Filename: logFile(dir), MaxSize: 10, MaxBackups: 1}
	if _, err := l.Write([]byte("boo!")); err != nil {
		t.Fatal(err)
	}
	if l.millCh == nil {
		t.Fatal("the first write should start the mill")
	}
	done := l.millDone
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Close returned with the mill goroutine still running")
	}
	if l.millCh != nil || l.file != nil {
		t.Fatal("Close left state behind")
	}

	if _, err := l.Write([]byte("again")); err != nil {
		t.Fatalf("write after Close: %v", err)
	}
	defer func() { _ = l.Close() }()
	if l.millCh == nil {
		t.Fatal("a write after Close should restart the mill")
	}
	existsWithContent(logFile(dir), []byte("boo!again"), t)
}
