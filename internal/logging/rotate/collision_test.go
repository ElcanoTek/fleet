package rotate

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two rotations within the same instant (the clock stands still between
// them) keep both backups: the second takes a counter suffix instead of
// renaming over the first, and compression keeps each one (#2074 review:
// millisecond names collided, a rename replaced a backup and one backup's
// .gz could truncate another's).
func TestRotationsInTheSameInstantNeverReplaceABackup(t *testing.T) {
	currentTime = fakeTime
	dir := t.TempDir()
	l := &Logger{Filename: filepath.Join(dir, "foobar.log"), MaxSize: 100, Compress: true}
	defer l.Close()

	for _, line := range []string{"first\n", "second\n", "third\n"} {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if err := l.Rotate(); err != nil {
			t.Fatal(err)
		}
	}
	waitMill(l)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var contents strings.Builder
	backups := 0
	for _, e := range entries {
		if e.Name() == "foobar.log" {
			continue
		}
		backups++
		if !strings.HasSuffix(e.Name(), compressSuffix) {
			t.Fatalf("backup %s was not compressed", e.Name())
		}
		if _, err := l.timeFromName(e.Name(), "foobar-", ".log"+compressSuffix); err != nil {
			t.Fatalf("backup %s is not recognised as one: %v", e.Name(), err)
		}
		contents.WriteString(gunzip(t, filepath.Join(dir, e.Name())))
	}
	if backups != 3 {
		t.Fatalf("%d backups, want three distinct ones", backups)
	}
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(contents.String(), want) {
			t.Fatalf("a backup was replaced: %q has no %q", contents.String(), want)
		}
	}
}

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
